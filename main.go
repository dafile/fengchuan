package main

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/* static/* CHANGELOG.md
var content embed.FS

// SSE client management
var (
	clients   = make(map[chan string]bool)
	clientMux sync.Mutex
)

type Entry struct {
	ID       string
	Content  string
	Type     string
	Filename string
	Ts       int64
	From     string
	Sender   string
}

type apiAsset struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

type fileMeta struct {
	Ts     int64  `json:"ts"`
	From   string `json:"from"`
	Sender string `json:"sender"`
}

var (
	ffmpegOnce       sync.Once
	ffmpegExecutable string
	thumbnailMux     sync.Mutex
)

const videoPosterPlaceholder = `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360" viewBox="0 0 640 360">
<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop stop-color="#243447"/><stop offset="1" stop-color="#173f45"/></linearGradient></defs>
<rect width="640" height="360" fill="url(#g)"/><circle cx="320" cy="180" r="54" fill="#89b4fa"/><path d="M306 148l50 32-50 32z" fill="#173f45"/></svg>`

func findFFmpeg() string {
	ffmpegOnce.Do(func() {
		var candidates []string
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), "ffmpeg.exe"))
		}
		if workingDir, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(workingDir, "ffmpeg.exe"))
		}
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			candidates = append(candidates, resolved)
		}
		for _, candidate := range candidates {
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				ffmpegExecutable = candidate
				return
			}
		}
	})
	return ffmpegExecutable
}

func videoThumbnailPath(id string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(id)))
	return filepath.Join("data", "thumbs", hex.EncodeToString(sum[:])+".jpg")
}

func removeStoredFile(id string) error {
	fullPath, err := safeDataPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(fullPath); err != nil {
		return err
	}
	os.Remove(fullPath + ".meta")
	os.Remove(videoThumbnailPath(id))
	if expirationTracker != nil {
		expirationTracker.mu.Lock()
		delete(expirationTracker.Expirations, filepath.ToSlash(id))
		expirationTracker.saveToFile()
		expirationTracker.mu.Unlock()
	}
	return nil
}

func generateVideoThumbnail(videoPath, thumbnailPath string) error {
	ffmpeg := findFFmpeg()
	if ffmpeg == "" {
		return fmt.Errorf("ffmpeg not found")
	}
	if err := os.MkdirAll(filepath.Dir(thumbnailPath), 0755); err != nil {
		return err
	}
	tempPath := thumbnailPath + fmt.Sprintf(".%d.tmp.jpg", time.Now().UnixNano())
	defer os.Remove(tempPath)
	for _, offset := range []string{"0.5", "0"} {
		cmd := exec.Command(
			ffmpeg,
			"-hide_banner",
			"-loglevel", "error",
			"-ss", offset,
			"-i", videoPath,
			"-frames:v", "1",
			"-vf", "scale=960:-2:force_original_aspect_ratio=decrease",
			"-q:v", "3",
			"-y",
			tempPath,
		)
		if err := cmd.Run(); err != nil {
			continue
		}
		info, err := os.Stat(tempPath)
		if err != nil || info.Size() == 0 {
			continue
		}
		if err := os.Rename(tempPath, thumbnailPath); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("unable to extract video frame")
}

func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i != -1 {
		addr = addr[:i]
	}
	addr = strings.TrimPrefix(addr, "[")
	addr = strings.TrimSuffix(addr, "]")
	if addr == "" {
		return "unknown"
	}
	return addr
}

func writeMeta(dir, name string, m fileMeta) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := atomicWriteFile(filepath.Join("data", dir, name+".meta"), b, 0644); err != nil {
		log.Printf("Failed to write metadata for %s/%s: %v", dir, name, err)
	}
}

func readMeta(dir, name string) fileMeta {
	var m fileMeta
	b, err := os.ReadFile(filepath.Join("data", dir, name+".meta"))
	if err != nil {
		return m
	}
	json.Unmarshal(b, &m)
	return m
}

// ---------- access log ----------

var accessLogMu sync.Mutex

const accessLogPath = "data/access.log"

type accessRecord struct {
	Ts     int64  `json:"ts"`
	IP     string `json:"ip"`
	Event  string `json:"event"`
	Detail string `json:"detail"`
}

func logAccess(r *http.Request, event, detail string) {
	rec := accessRecord{Ts: time.Now().Unix(), IP: clientIP(r), Event: event, Detail: detail}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	accessLogMu.Lock()
	defer accessLogMu.Unlock()
	f, err := os.OpenFile(accessLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(b)
	f.Write([]byte("\n"))
	f.Close()
}

func readAccessLogFiltered(date, event, ip, q string, offset, limit int) (int, []accessRecord) {
	data, err := os.ReadFile(accessLogPath)
	all := []accessRecord{}
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec accessRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			all = append(all, rec)
		}
	}
	filtered := []accessRecord{}
	for _, rec := range all {
		if date != "" {
			d := time.Unix(rec.Ts, 0).Format("2006-01-02")
			if d != date {
				continue
			}
		}
		if event != "" && rec.Event != event {
			continue
		}
		if ip != "" && !strings.Contains(rec.IP, ip) {
			continue
		}
		if q != "" {
			hay := rec.Detail + " " + rec.Event + " " + rec.IP
			if !strings.Contains(strings.ToLower(hay), strings.ToLower(q)) {
				continue
			}
		}
		filtered = append(filtered, rec)
	}
	total := len(filtered)
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return total, filtered[offset:end]
}

type ExpirationTracker struct {
	Expirations map[string]time.Time `json:"expirations"`
	mu          sync.Mutex           // mutex for thread safety
}

var expirationTracker *ExpirationTracker
var expirationOptions = []string{"Never", "1 hour", "4 hours", "1 day", "Custom"}

// appVersion and changelogPath are shown in the header; keep in sync with CHANGELOG.md
const appVersion = "v1.15.1"
const changelogPath = "CHANGELOG.md"

func initExpirationTracker() *ExpirationTracker {
	tracker := &ExpirationTracker{
		Expirations: make(map[string]time.Time),
	}
	// Load existing expirations from file
	expirationFile := filepath.Join("data", "expirations.json")
	if _, err := os.Stat(expirationFile); err == nil {
		data, err := os.ReadFile(expirationFile)
		if err == nil {
			var storedTracker ExpirationTracker
			if err := json.Unmarshal(data, &storedTracker); err == nil {
				for fileID, expiry := range storedTracker.Expirations {
					tracker.Expirations[filepath.ToSlash(fileID)] = expiry
				}
			}
		}
	}
	return tracker
}

func parseCustomDuration(customExpiry string) time.Duration {
	customExpiry = strings.TrimSpace(customExpiry)
	// Regex to match the format like 1h, 30m, 2d, etc.
	re := regexp.MustCompile(`^(\d+)([hmMdwy])$`)
	matches := re.FindStringSubmatch(customExpiry)
	if len(matches) < 2 { // bad value
		return 5 * time.Minute
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 5 * time.Minute
	}
	unit := strings.ToLower(matches[2])
	switch unit {
	case "m": // minutes
		if value < 5 {
			return 5 * time.Minute
		}
		return time.Duration(value) * time.Minute
	case "h": // hours
		return time.Duration(value) * time.Hour
	case "d": // days
		return time.Duration(value) * 24 * time.Hour
	case "w": // weeks
		return time.Duration(value) * 7 * 24 * time.Hour
	case "M": // months
		return time.Duration(value) * 30 * 24 * time.Hour
	case "y": // years
		return time.Duration(value) * 365 * 24 * time.Hour
	default:
		return 5 * time.Minute
	}
}

func (t *ExpirationTracker) SetExpiration(fileID, expiryOption string) {
	fileID = filepath.ToSlash(filepath.Clean(fileID))
	t.mu.Lock()
	defer t.mu.Unlock()
	if expiryOption == "Never" {
		delete(t.Expirations, fileID)
	} else {
		var duration time.Duration
		switch expiryOption {
		case "1 hour":
			duration = 1 * time.Hour
		case "4 hours":
			duration = 4 * time.Hour
		case "1 day":
			duration = 24 * time.Hour
		case "Custom":
			// Should not happen anymore.
			return
		default:
			if len(expiryOption) > 0 {
				duration = parseCustomDuration(expiryOption)
			} else {
				delete(t.Expirations, fileID)
				return
			}
		}
		t.Expirations[fileID] = time.Now().Add(duration)
	}
	t.saveToFile()
}

func (t *ExpirationTracker) saveToFile() {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		log.Printf("Error marshaling expirations: %v", err)
		return
	}
	expirationFile := filepath.Join("data", "expirations.json")
	if err := atomicWriteFile(expirationFile, data, 0644); err != nil {
		log.Printf("Error saving expirations: %v", err)
	}
}

func (t *ExpirationTracker) CleanupExpired() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var expiredFiles []string
	// Find expired files
	for fileID, expiryTime := range t.Expirations {
		if now.After(expiryTime) {
			expiredFiles = append(expiredFiles, fileID)
		}
	}
	// Delete expired files
	for _, fileID := range expiredFiles {
		fullPath, pathErr := safeDataPath(fileID)
		if pathErr != nil {
			log.Printf("Skipping unsafe expired path %q: %v", fileID, pathErr)
			delete(t.Expirations, fileID)
			continue
		}
		err := os.Remove(fullPath)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Error removing expired file %s: %v", fileID, err)
		} else {
			log.Printf("Removed expired file: %s", fileID)
		}
		os.Remove(fullPath + ".meta")
		os.Remove(videoThumbnailPath(fileID))
		delete(t.Expirations, fileID)
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
		notifyContentChange()
	}
	return expiredFiles
}

// ---------- 访问鉴权与站点设置 ----------

var passwordFlag = flag.String("password", "", "access password for the web UI")
var apiKeyFlag = flag.String("api-key", "", "API key for /api/upload and /api/message")
var sessionTTLFlag = flag.String("session-ttl", "10m", "session lifetime, e.g. 10m or 1h")

var authPassword string
var authTTL time.Duration
var authMu sync.Mutex

func getAuthTTL() time.Duration {
	authMu.Lock()
	defer authMu.Unlock()
	return authTTL
}

func setAuthTTL(d time.Duration) {
	authMu.Lock()
	authTTL = d
	authMu.Unlock()
}

func parseTTL(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("empty")
	}
	if regexp.MustCompile(`^[0-9]+$`).MatchString(v) {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid")
		}
		return time.Duration(n) * time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid")
	}
	return d, nil
}

type serverSettings struct {
	DefaultTheme string `json:"defaultTheme"`
	SessionTTL   string `json:"sessionTTL"`
}

var settingsMu sync.Mutex

func loadSettings() serverSettings {
	s := serverSettings{DefaultTheme: "auto"}
	b, err := os.ReadFile(filepath.Join("data", "settings.json"))
	if err == nil {
		json.Unmarshal(b, &s)
	}
	if s.DefaultTheme != "light" && s.DefaultTheme != "dark" {
		s.DefaultTheme = "auto"
	}
	return s
}

func saveSettings(s serverSettings) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join("data", "settings.json"), b, 0644)
}

func tokenFor(exp int64) string {
	return makeSessionToken(exp)
}

func tokenExpiry(tok string) (int64, bool) {
	return parseSessionToken(tok)
}

func isAuthed(r *http.Request) bool {
	c, err := r.Cookie("lc_auth")
	if err != nil {
		return false
	}
	exp, ok := tokenExpiry(c.Value)
	if !ok || sessionTokenRevoked(c.Value) {
		return false
	}
	return time.Now().Unix() < exp
}

func isAPIKeyAuthed(r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get("X-FengChuan-Key"))
	if token == "" {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(authorization, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		}
	}
	if token == "" || authAPIKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(authAPIKey)) == 1
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		p := r.URL.Path
		rateKey := authRateKey(r)
		public := p == "/login" || p == "/api/login" || p == "/api/logout" ||
			p == "/api/health" ||
			(p == "/api/settings" && r.Method == http.MethodGet) ||
			strings.HasPrefix(p, "/static/") ||
			p == "/favicon.ico" || p == "/manifest.json" || p == "/sw.js" ||
			p == "/icon-192.png" || p == "/icon-512.png" || p == "/style.css"
		apiKeyRoute := p == "/api/upload" || p == "/api/message"
		if !public && !isAuthed(r) && !(apiKeyRoute && isAPIKeyAuthed(r)) {
			if apiKeyRoute {
				if !authLimiter.allow(rateKey) {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					json.NewEncoder(w).Encode(map[string]string{"error": "too many authentication attempts"})
					return
				}
				authLimiter.fail(rateKey)
			}
			if strings.HasPrefix(p, "/api/") || p == "/submit" {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if apiKeyRoute && isAPIKeyAuthed(r) {
			authLimiter.reset(rateKey)
		}
		if !public {
			if c, err := r.Cookie("lc_auth"); err == nil {
				ttl := getAuthTTL()
				if exp, ok := tokenExpiry(c.Value); ok && exp-time.Now().Unix() < int64(ttl.Seconds()/2) {
					setAuthCookie(w, tokenFor(time.Now().Add(ttl).Unix()), int(ttl.Seconds()))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

var listenAddress = flag.String("listen", ":8080", "host:port in which the server will listen")

// Placeholder content for notepad files
const mdPlaceholder = `# Welcome to Markdown Notepad

Start typing your markdown here...

## Features

- **Bold** and *italic* text
- [Links](https://example.com)
- Lists (ordered and unordered)
- Code blocks
- And more!

` + "```" + `
function example() {
  console.log("Hello, Markdown!");
}
` + "```"

func handleContentUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	messageChan := make(chan string)
	clientMux.Lock()
	clients[messageChan] = true
	clientMux.Unlock()

	defer func() {
		clientMux.Lock()
		delete(clients, messageChan)
		clientMux.Unlock()
		close(messageChan)
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	// Send an initial message
	fmt.Fprintf(w, "data: %s\n\n", "connected")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-messageChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-ticker.C: // send keep-alive msg
			fmt.Fprintf(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func notifyContentChange() {
	clientMux.Lock()
	defer clientMux.Unlock()
	for client := range clients {
		select {
		case client <- "content_updated":
		default:
		}
	}
}

func collectEntries() []Entry {
	entries := []Entry{}
	textFiles, _ := os.ReadDir(filepath.Join("data", "text"))
	for _, file := range textFiles {
		if file.IsDir() || strings.HasSuffix(file.Name(), ".meta") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("data", "text", file.Name()))
		if err != nil {
			continue
		}
		info, _ := file.Info()
		ts := time.Now().Unix()
		if info != nil {
			ts = info.ModTime().Unix()
		}
		meta := readMeta("text", file.Name())
		if meta.Ts != 0 {
			ts = meta.Ts
		}
		entries = append(entries, Entry{
			ID:       "text/" + file.Name(),
			Type:     "text",
			Content:  string(data),
			Filename: file.Name(),
			Ts:       ts,
			From:     meta.From,
			Sender:   meta.Sender,
		})
	}
	files, _ := os.ReadDir(filepath.Join("data", "files"))
	for _, file := range files {
		if file.IsDir() || strings.HasSuffix(file.Name(), ".meta") {
			continue
		}
		info, _ := file.Info()
		ts := time.Now().Unix()
		if info != nil {
			ts = info.ModTime().Unix()
		}
		meta := readMeta("files", file.Name())
		if meta.Ts != 0 {
			ts = meta.Ts
		}
		entries = append(entries, Entry{
			ID:       "files/" + file.Name(),
			Type:     "file",
			Filename: file.Name(),
			Ts:       ts,
			From:     meta.From,
			Sender:   meta.Sender,
		})
	}
	linkTs := time.Now().Unix()
	if st, err := os.Stat(filepath.Join("data", "links.file")); err == nil {
		linkTs = st.ModTime().Unix()
	}
	data, err := os.ReadFile(filepath.Join("data", "links.file"))
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			entries = append(entries, Entry{
				ID:       "link/" + url.PathEscape(line),
				Type:     "link",
				Content:  line,
				Filename: line,
				Ts:       linkTs,
			})
		}
	}
	sort.SliceStable(entries, func(a, b int) bool {
		return entries[a].Ts < entries[b].Ts
	})
	return entries
}

func main() {
	flag.Parse()
	authPassword = strings.TrimSpace(*passwordFlag)
	if authPassword == "" {
		authPassword = strings.TrimSpace(os.Getenv("FENGCHUAN_PASSWORD"))
	}
	if authPassword == "" || authPassword == "change-me" {
		log.Fatal("Access password must be set with -password or FENGCHUAN_PASSWORD; the default change-me value is disabled")
	}
	{
		d, err := time.ParseDuration(*sessionTTLFlag)
		if err != nil || d <= 0 {
			log.Printf("Invalid session-ttl %q, falling back to 10m\n", *sessionTTLFlag)
			d = 10 * time.Minute
		}
		authTTL = d
	}
	if st := loadSettings(); st.SessionTTL != "" {
		if sd, err := parseTTL(st.SessionTTL); err == nil {
			authTTL = sd
		}
	}
	log.Printf("Access password enabled (session TTL %s)\n", authTTL)

	if err := os.MkdirAll(filepath.Join("data", "files"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "text"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "notepad"), 0755); err != nil {
		log.Fatal(err)
	}
	log.Println("Data directory created/reused without errors.")
	if err := initSessionSecurity(); err != nil {
		log.Fatalf("Failed to initialize session security: %v", err)
	}
	configuredAPIKey := strings.TrimSpace(*apiKeyFlag)
	if configuredAPIKey == "" {
		configuredAPIKey = strings.TrimSpace(os.Getenv("FENGCHUAN_API_KEY"))
	}
	var err error
	authAPIKey, err = loadOrCreateAPIKey(configuredAPIKey)
	if err != nil {
		log.Fatalf("Failed to initialize API key: %v", err)
	}
	log.Printf("API key initialized; key material is stored in %s", apiKeyFile)
	createFileIfNotExists("notepad/md.file", mdPlaceholder)
	createFileIfNotExists("links.file", "")

	// Initialize the expiration tracker
	expirationTracker = initExpirationTracker()
	customExpiry := os.Getenv("DEFAULT_EXPIRY")
	if customExpiry != "" {
		switch customExpiry {
		case "1d":
			expirationOptions = []string{"1 day", "Never", "1 hour", "4 hours", "Custom"}
		case "4h":
			expirationOptions = []string{"4 hours", "Never", "1 hour", "1 day", "Custom"}
		case "1h":
			expirationOptions = []string{"1 hour", "Never", "4 hours", "1 day", "Custom"}
		default:
			expirationOptions = append([]string{customExpiry}, expirationOptions...)
		}
	}

	// Goroutine to periodically expire files
	go func() {
		ticker := time.NewTicker(3 * time.Minute) // 3 minutes is sparse enough, load is extremely minimal as the operation is fast (in memory tracker)
		defer ticker.Stop()
		for range ticker.C {
			expirationTracker.CleanupExpired()
		}
	}()

	routes := http.NewServeMux()
	registerRoutes(routes)

	// Start server
	log.Fatal(http.ListenAndServe(*listenAddress, authMiddleware(routes)))
}

func registerRoutes(routes *http.ServeMux) {
	tmpl := template.Must(template.ParseFS(content, "templates/*.html"))

	routes.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Clean up expired files on page load
		expirationTracker.CleanupExpired()
		if r.URL.Path == "/admin" {
			logAccess(r, "访问管理页", "")
			tmpl.ExecuteTemplate(w, "admin.html", nil)
			return
		}
		logAccess(r, "访问主页", "")
		tmpl.ExecuteTemplate(w, "index.html", nil)
	})

	routes.HandleFunc("/md", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "md.html", nil)
	})

	// Retrieve custom expiration options
	routes.HandleFunc("/api/batch-download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		logAccess(r, "批量下载", fmt.Sprintf("%d 个文件", len(ids)))
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", contentDisposition("fengchuan.zip"))
		zw := zip.NewWriter(w)
		defer zw.Close()
		for _, id := range ids {
			fullPath, err := safeDataPath(id)
			if err != nil {
				continue
			}
			file, err := os.Open(fullPath)
			if err != nil {
				continue
			}
			entryName := filepath.ToSlash(strings.TrimPrefix(fullPath, "data"+string(os.PathSeparator)))
			fw, err := zw.Create(entryName)
			if err != nil {
				file.Close()
				continue
			}
			io.Copy(fw, file)
			file.Close()
		}
	})

	routes.HandleFunc("/api/delete-batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		for _, id := range ids {
			if err := removeStoredFile(id); err != nil {
				log.Printf("Rejected or failed batch delete for %q: %v", id, err)
			}
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	routes.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		entries := collectEntries()
		textN, fileN, linkN := 0, 0, 0
		var totalSize int64
		for _, e := range entries {
			switch e.Type {
			case "text":
				textN++
				totalSize += int64(len(e.Content))
			case "file":
				fileN++
				if st, err := os.Stat(filepath.Join("data", e.ID)); err == nil {
					totalSize += st.Size()
				}
			case "link":
				linkN++
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"text":      textN,
			"file":      fileN,
			"link":      linkN,
			"totalSize": totalSize,
		})
	})

	routes.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		logAccess(r, "打开登录页", "")
		tmpl.ExecuteTemplate(w, "login.html", nil)
	})

	routes.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"name":    "FengChuan",
			"version": appVersion,
		})
	})

	routes.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rateKey := authRateKey(r)
		if !authLimiter.allow(rateKey) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "too many authentication attempts"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var payload struct {
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(payload.Password), []byte(authPassword)) != 1 {
			authLimiter.fail(rateKey)
			logAccess(r, "登录失败", "密码错误")
			time.Sleep(250 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "密码错误"})
			return
		}
		authLimiter.reset(rateKey)
		ttl := getAuthTTL()
		if payload.Remember {
			ttl = 365 * 24 * time.Hour
		}
		setAuthCookie(w, tokenFor(time.Now().Add(ttl).Unix()), int(ttl.Seconds()))
		logAccess(r, "登录成功", "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	routes.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "Invalid or oversized upload", http.StatusRequestEntityTooLarge)
			return
		}
		sender := strings.TrimSpace(r.FormValue("sender"))
		if sender == "" {
			sender = "API"
		}
		expiry := strings.TrimSpace(r.FormValue("expiry"))
		if expiry == "" {
			expiry = "Never"
		}
		var headers []*multipart.FileHeader
		for _, field := range []string{"file", "files", "file-upload"} {
			headers = append(headers, r.MultipartForm.File[field]...)
		}
		note := strings.TrimSpace(r.FormValue("note"))
		if len(headers) == 0 && note == "" {
			http.Error(w, "No files or note supplied", http.StatusBadRequest)
			return
		}
		files := make([]apiAsset, 0, len(headers))
		for _, fileHeader := range headers {
			file, err := fileHeader.Open()
			if err != nil {
				http.Error(w, "Failed to read upload", http.StatusBadRequest)
				return
			}
			output, uniqueFileName, err := createUniqueFile(filepath.Join("data", "files"), fileHeader.Filename)
			if err != nil {
				file.Close()
				http.Error(w, "Failed to save upload", http.StatusInternalServerError)
				return
			}
			destination := output.Name()
			size, copyErr := io.Copy(output, file)
			closeErr := output.Close()
			file.Close()
			if copyErr != nil || closeErr != nil {
				os.Remove(destination)
				http.Error(w, "Failed to save upload", http.StatusInternalServerError)
				return
			}
			writeMeta("files", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: sender})
			if expiry != "Never" {
				expirationTracker.SetExpiration("files/"+uniqueFileName, expiry)
			}
			id := "files/" + uniqueFileName
			files = append(files, apiAsset{
				ID:          id,
				Filename:    uniqueFileName,
				Size:        size,
				DownloadURL: "/download/" + url.PathEscape(id),
			})
			logAccess(r, "API上传文件", uniqueFileName)
		}
		var noteAsset *apiAsset
		if note != "" {
			noteName := strings.TrimSpace(r.FormValue("note_name"))
			if noteName == "" {
				noteName = "API更新说明-" + time.Now().Format("20060102-150405") + ".md"
			}
			uniqueNoteName, err := writeUniqueFile(filepath.Join("data", "text"), noteName, []byte(note))
			if err != nil {
				http.Error(w, "Failed to save note", http.StatusInternalServerError)
				return
			}
			writeMeta("text", uniqueNoteName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: sender})
			id := "text/" + uniqueNoteName
			noteAsset = &apiAsset{
				ID:          id,
				Filename:    uniqueNoteName,
				Size:        int64(len([]byte(note))),
				DownloadURL: "/download/" + url.PathEscape(id),
			}
			logAccess(r, "API更新说明", uniqueNoteName)
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    true,
			"files": files,
			"note":  noteAsset,
		})
	})

	routes.HandleFunc("/api/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
		var payload struct {
			Content string `json:"content"`
			Name    string `json:"name"`
			Sender  string `json:"sender"`
			Expiry  string `json:"expiry"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(payload.Content) == "" {
			http.Error(w, "Content cannot be empty", http.StatusBadRequest)
			return
		}
		if payload.Sender == "" {
			payload.Sender = "API"
		}
		if payload.Expiry == "" {
			payload.Expiry = "Never"
		}
		fileName := payload.Name
		if fileName == "" {
			fileName = "API消息-" + time.Now().Format("20060102-150405")
		}
		uniqueFileName, err := writeUniqueFile(filepath.Join("data", "text"), fileName, []byte(payload.Content))
		if err != nil {
			http.Error(w, "Failed to save message", http.StatusInternalServerError)
			return
		}
		writeMeta("text", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: payload.Sender})
		if payload.Expiry != "Never" {
			expirationTracker.SetExpiration("text/"+uniqueFileName, payload.Expiry)
		}
		logAccess(r, "API发消息", payload.Sender)
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":       true,
			"id":       "text/" + uniqueFileName,
			"filename": uniqueFileName,
		})
	})

	routes.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("lc_auth"); err == nil {
			if expiry, ok := tokenExpiry(c.Value); ok {
				if err := revokeSessionToken(c.Value, expiry); err != nil {
					log.Printf("Failed to revoke session: %v", err)
				}
			}
		}
		clearAuthCookie(w)
		logAccess(r, "退出登录", "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	routes.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "POST" {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			var payload struct {
				DefaultTheme string `json:"defaultTheme"`
				SessionTTL   string `json:"sessionTTL"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			cur := loadSettings()
			if payload.DefaultTheme != "" {
				if payload.DefaultTheme != "auto" && payload.DefaultTheme != "light" && payload.DefaultTheme != "dark" {
					http.Error(w, "invalid theme", http.StatusBadRequest)
					return
				}
				cur.DefaultTheme = payload.DefaultTheme
			}
			if payload.SessionTTL != "" {
				d, err := parseTTL(payload.SessionTTL)
				if err != nil {
					http.Error(w, "invalid sessionTTL", http.StatusBadRequest)
					return
				}
				cur.SessionTTL = payload.SessionTTL
				setAuthTTL(d)
			}
			if err := saveSettings(cur); err != nil {
				http.Error(w, "Failed to save settings", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
			return
		}
		s := loadSettings()
		if s.SessionTTL == "" {
			s.SessionTTL = getAuthTTL().String()
		}
		json.NewEncoder(w).Encode(s)
	})

	routes.HandleFunc("/access-log", func(w http.ResponseWriter, r *http.Request) {
		logAccess(r, "访问记录页", "")
		tmpl.ExecuteTemplate(w, "accesslog.html", nil)
	})

	routes.HandleFunc("/api/access-log", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		offset, _ := strconv.Atoi(qs.Get("offset"))
		limit, _ := strconv.Atoi(qs.Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 200
		}
		total, records := readAccessLogFiltered(qs.Get("date"), qs.Get("event"), qs.Get("ip"), qs.Get("q"), offset, limit)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"total": total, "records": records})
	})

	routes.HandleFunc("/api/access-log-clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		accessLogMu.Lock()
		err := atomicWriteFile(accessLogPath, []byte{}, 0644)
		accessLogMu.Unlock()
		if err != nil {
			http.Error(w, "Failed to clear access log", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	routes.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"version": appVersion})
	})

	routes.HandleFunc("/api/changelog", func(w http.ResponseWriter, r *http.Request) {
		data, err := content.ReadFile(changelogPath)
		if err != nil {
			http.Error(w, "CHANGELOG.md not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	})

	routes.HandleFunc("/getExpiryOptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expirationOptions)
	})

	// Serve static files from embedded filesystem
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		log.Fatalf("Failed to create static sub-filesystem: %v", err)
	}
	routes.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	routes.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("style.css")
		if err != nil {
			http.Error(w, "Style not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "text/css")
		io.Copy(w, file)
	})

	routes.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("manifest.json")
		if err != nil {
			http.Error(w, "Manifest not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, file)
	})

	routes.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("sw.js")
		if err != nil {
			http.Error(w, "Service worker not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/javascript")
		io.Copy(w, file)
	})

	routes.HandleFunc("/md.js", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("md.js")
		if err != nil {
			http.Error(w, "JavaScript not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/javascript")
		io.Copy(w, file)
	})

	// Handle favicon and icons
	routes.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("favicon.ico")
		if err != nil {
			http.Error(w, "Favicon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/x-icon")
		io.Copy(w, file)
	})

	routes.HandleFunc("/icon-192.png", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("icon-192.png")
		if err != nil {
			http.Error(w, "Icon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/png")
		io.Copy(w, file)
	})

	routes.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("icon-512.png")
		if err != nil {
			http.Error(w, "Icon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/png")
		io.Copy(w, file)
	})

	// API endpoint to load notepad content
	routes.HandleFunc("/notepad/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" { // && filename != "rtext.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			content, err := os.ReadFile(filepath.Join("data", "notepad", filename))
			if err != nil {
				http.Error(w, "Error reading notepad file", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(content)
			return
		case "POST":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" { // && filename != "rtext.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
			content, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}
			err = atomicWriteFile(filepath.Join("data", "notepad", filename), content, 0644)
			if err != nil {
				http.Error(w, "Error saving notepad file", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Saved"))
			log.Printf("Saved notepad content to %s\n", filename)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	routes.HandleFunc("/api/entries", func(w http.ResponseWriter, r *http.Request) {
		entries := collectEntries()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(entries)
	})

	routes.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		contentType := r.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "application/json") {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
			var payload struct {
				Content string `json:"content"`
				Name    string `json:"name"`
				Type    string `json:"type"`
				Expiry  string `json:"expiry"`
				Sender  string `json:"sender"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			if payload.Type == "link" {
				if payload.Content == "" {
					http.Error(w, "URL content cannot be empty", http.StatusBadRequest)
					return
				}
				u, err := url.ParseRequestURI(payload.Content)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
					http.Error(w, "Invalid URL format. Must start with http:// or https://", http.StatusBadRequest)
					return
				}
				f, err := os.OpenFile(filepath.Join("data", "links.file"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err != nil {
					http.Error(w, "Failed to save link", http.StatusInternalServerError)
					return
				}
				defer f.Close()
				if _, err := f.WriteString(payload.Content + "\n"); err != nil {
					http.Error(w, "Failed to save link", http.StatusInternalServerError)
					return
				}
				logAccess(r, "发链接", payload.Content)
			} else {
				if payload.Content == "" {
					http.Error(w, "Content cannot be empty", http.StatusBadRequest)
					return
				}
				filename := payload.Name
				if filename == "" {
					filename = time.Now().Format("Jan-02 15-04-05")
				}
				uniqueFileName, err := writeUniqueFile(filepath.Join("data", "text"), filename, []byte(payload.Content))
				if err != nil {
					http.Error(w, "Failed to save message", http.StatusInternalServerError)
					return
				}
				writeMeta("text", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: payload.Sender})
				logAccess(r, "发消息", payload.Sender)
				if payload.Expiry != "" && payload.Expiry != "Never" {
					expirationTracker.SetExpiration("text/"+uniqueFileName, payload.Expiry)
				}
			}
			notifyContentChange()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "Invalid or oversized upload", http.StatusRequestEntityTooLarge)
			return
		}
		entryType := r.FormValue("type")
		expiryOption := r.FormValue("expiry")
		content := r.FormValue("content")
		name := r.FormValue("name")
		if entryType == "link" {
			// Handle link submission
			if content == "" {
				http.Error(w, "URL content cannot be empty", http.StatusBadRequest)
				return
			}
			u, err := url.ParseRequestURI(content)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Invalid URL format. Must start with http:// or https://", http.StatusBadRequest)
				return
			}
			linksFilePath := filepath.Join("data", "links.file")
			f, err := os.OpenFile(linksFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				http.Error(w, "Failed to save link", http.StatusInternalServerError)
				return
			}
			defer f.Close()
			if _, err := f.WriteString(content + "\n"); err != nil {
				http.Error(w, "Failed to save link", http.StatusInternalServerError)
				return
			}
			log.Printf("Saved link %s\n", content)
		} else {
			// Handle file and text submission
			files := r.MultipartForm.File["file-upload"]
			if len(files) > 0 {
				// File submission
				for _, fileHeader := range files {
					err := func() error {
						file, err := fileHeader.Open()
						if err != nil {
							return err
						}
						defer file.Close()
						fileName := name
						if fileName == "" {
							fileName = fileHeader.Filename
						}
						f, uniqueFileName, err := createUniqueFile(filepath.Join("data", "files"), fileName)
						if err != nil {
							return err
						}
						cleanup := true
						defer func() {
							if cleanup {
								f.Close()
								os.Remove(f.Name())
							}
						}()
						if _, err := io.Copy(f, file); err != nil {
							return err
						}
						if err := f.Close(); err != nil {
							return err
						}
						cleanup = false
						writeMeta("files", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: r.FormValue("sender")})
						logAccess(r, "上传文件", uniqueFileName)
						if expiryOption != "Never" {
							fileID := "files/" + uniqueFileName
							expirationTracker.SetExpiration(fileID, expiryOption)
						}
						log.Printf("Saved file %s with expiry %s\n", uniqueFileName, expiryOption)
						return nil
					}()
					if err != nil {
						http.Error(w, "Failed to save upload", http.StatusInternalServerError)
						return
					}
				}
			} else if content != "" {
				// Text snippet submission
				filename := name
				if filename == "" {
					filename = time.Now().Format("Jan-02 15-04-05")
				}
				uniqueFileName, err := writeUniqueFile(filepath.Join("data", "text"), filename, []byte(content))
				if err != nil {
					http.Error(w, "Failed to save message", http.StatusInternalServerError)
					return
				}
				writeMeta("text", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: r.FormValue("sender")})
				logAccess(r, "发消息", r.FormValue("sender"))
				if expiryOption != "Never" {
					fileID := "text/" + uniqueFileName
					expirationTracker.SetExpiration(fileID, expiryOption)
				}
				log.Printf("Saved text snippet %s with expiry %s\n", uniqueFileName, expiryOption)
			}
		}
		notifyContentChange()
		// Send succes for AJAX
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Success"))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	routes.HandleFunc("/rename/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		oldPathID := strings.TrimPrefix(r.URL.Path, "/rename/")
		newName := r.FormValue("newname")
		if newName == "" {
			http.Error(w, "New name cannot be empty", http.StatusBadRequest)
			return
		}
		oldFullPath, err := safeDataPath(oldPathID)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		baseDir := filepath.Dir(oldFullPath)
		newName, err = reserveUniquePath(baseDir, newName)
		if err != nil {
			http.Error(w, "Failed to allocate filename", http.StatusInternalServerError)
			return
		}
		newPath := filepath.Join(baseDir, newName)
		newPathID := filepath.ToSlash(strings.TrimPrefix(newPath, "data"+string(os.PathSeparator)))
		oldPathID = filepath.ToSlash(strings.TrimPrefix(oldFullPath, "data"+string(os.PathSeparator)))
		// Rename the file
		err = os.Rename(oldFullPath, newPath)
		if err != nil {
			os.Remove(newPath)
			http.Error(w, "Failed to rename file", http.StatusInternalServerError)
			return
		}
		os.Rename(oldFullPath+".meta", newPath+".meta")
		expirationTracker.mu.Lock()
		if expiryTime, hasExpiry := expirationTracker.Expirations[oldPathID]; hasExpiry {
			delete(expirationTracker.Expirations, oldPathID)
			expirationTracker.Expirations[newPathID] = expiryTime
			expirationTracker.saveToFile()
		}
		expirationTracker.mu.Unlock()
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Renamed %s to %s\n", oldPathID, newName)
	})

	routes.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/raw/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Only text files can be accessed", http.StatusBadRequest)
			return
		}
		fullPath, err := safeDataPath(id)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		logAccess(r, "查看", strings.TrimPrefix(id, "text/"))
		content, err := os.ReadFile(fullPath)
		if err != nil {
			http.Error(w, "File not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(content)
	})

	routes.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/download/")
		filePath, err := safeDataPath(filename)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		accept := r.Header.Get("Accept")
		if !strings.HasPrefix(accept, "image/") && !strings.HasPrefix(accept, "video/") && !strings.HasPrefix(accept, "audio/") {
			logAccess(r, "查看/下载", filepath.Base(filename))
		}
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "Failed to open file", http.StatusInternalServerError)
			return
		}
		defer file.Close()

		// Brute force method to determine content type
		ext := strings.ToLower(filepath.Ext(filename))
		var contentType string
		switch ext {
		case ".pdf":
			contentType = "application/pdf"
		case ".jpg", ".jpeg":
			contentType = "image/jpeg"
		case ".png":
			contentType = "image/png"
		case ".gif":
			contentType = "image/gif"
		case ".svg":
			contentType = "image/svg+xml"
		case ".mp4", ".m4v":
			contentType = "video/mp4"
		case ".webm":
			contentType = "video/webm"
		case ".ogv", ".ogg":
			contentType = "video/ogg"
		case ".mov":
			contentType = "video/quicktime"
		case ".mp3":
			contentType = "audio/mpeg"
		case ".wav":
			contentType = "audio/wav"
		default:
			buffer := make([]byte, 512)
			_, err = file.Read(buffer)
			if err != nil && err != io.EOF {
				http.Error(w, "Failed to read file", http.StatusInternalServerError)
				return
			}
			contentType = http.DetectContentType(buffer)
			_, err = file.Seek(0, 0)
			if err != nil {
				http.Error(w, "Failed to read file", http.StatusInternalServerError)
				return
			}
		}
		baseFilename := filepath.Base(filename)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", contentDisposition(baseFilename))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, baseFilename, fileInfo.ModTime(), file)
		log.Printf("Served %s for download\n", filename)
	})

	routes.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/view/")
		filePath, err := safeDataPath(filename)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		baseFilename := filepath.Base(filename)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition(baseFilename))
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, baseFilename, info.ModTime(), file)
		log.Printf("Served %s for viewing\n", filename)
	})

	routes.HandleFunc("/video-thumb/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/video-thumb/")
		if !strings.HasPrefix(id, "files/") {
			http.Error(w, "Invalid video path", http.StatusBadRequest)
			return
		}
		videoPath, err := safeDataPath(id)
		if err != nil {
			http.Error(w, "Invalid video path", http.StatusBadRequest)
			return
		}
		extension := strings.ToLower(filepath.Ext(id))
		switch extension {
		case ".mp4", ".m4v", ".webm", ".ogv", ".ogg", ".mov":
		default:
			http.Error(w, "Not a video", http.StatusNotFound)
			return
		}
		videoInfo, err := os.Stat(videoPath)
		if err != nil || videoInfo.IsDir() {
			http.Error(w, "Video not found", http.StatusNotFound)
			return
		}
		thumbnailPath := videoThumbnailPath(id)
		thumbnailMux.Lock()
		thumbnailInfo, thumbnailErr := os.Stat(thumbnailPath)
		if thumbnailErr != nil || thumbnailInfo.ModTime().Before(videoInfo.ModTime()) {
			thumbnailErr = generateVideoThumbnail(videoPath, thumbnailPath)
		}
		thumbnailMux.Unlock()
		if thumbnailErr != nil {
			w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Write([]byte(videoPosterPlaceholder))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, thumbnailPath)
	})

	routes.HandleFunc("/delete/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/delete/")
		logAccess(r, "删除", id)
		// Handle link deletion
		if after, ok := strings.CutPrefix(id, "link/"); ok {
			linkToDelete := after
			linksFilePath := filepath.Join("data", "links.file")
			data, err := os.ReadFile(linksFilePath)
			if err != nil {
				http.Error(w, "Failed to read links file for deletion", http.StatusInternalServerError)
				return
			}
			lines := strings.Split(string(data), "\n")
			var newLines []string
			var found bool
			for _, line := range lines {
				if strings.TrimSpace(line) == strings.TrimSpace(linkToDelete) && !found {
					found = true // Remove only the first occurrence
					continue
				}
				if strings.TrimSpace(line) != "" {
					newLines = append(newLines, line)
				}
			}
			output := strings.Join(newLines, "\n")
			// Add newline for correctness
			if output != "" {
				output += "\n"
			}
			err = atomicWriteFile(linksFilePath, []byte(output), 0644)
			if err != nil {
				http.Error(w, "Failed to write links file after deletion", http.StatusInternalServerError)
				return
			}
			notifyContentChange()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "ok"}`))
			log.Printf("Deleted link %s\n", linkToDelete)
			return
		}
		// Handle file and snippet deletion
		if err := removeStoredFile(id); err != nil {
			log.Printf("Failed to delete %s: %v", id, err)
			http.Error(w, "Failed to delete file", http.StatusBadRequest)
			return
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
		log.Printf("Deleted %s\n", id)
	})

	routes.HandleFunc("/edit/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/edit/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Can only edit text snippets", http.StatusBadRequest)
			return
		}
		fullPath, err := safeDataPath(id)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		content := r.FormValue("content")
		if content == "" {
			http.Error(w, "Content cannot be empty", http.StatusBadRequest)
			return
		}
		err = atomicWriteFile(fullPath, []byte(content), 0644)
		if err != nil {
			http.Error(w, "Failed to save text", http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Edited %s\n", id)
	})

	// SSE Updates for content refresh
	routes.HandleFunc("/api/updates", handleContentUpdates)

}

// Helper function to create files if they don't exist
func createFileIfNotExists(filename string, defaultContent string) {
	dir := filepath.Dir(filepath.Join("data", filename))
	if dir != "." && dir != "data" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v\n", dir, err)
		}
	}
	filePath := filepath.Join("data", filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		err := os.WriteFile(filePath, []byte(defaultContent), 0644)
		if err != nil {
			log.Printf("Error creating file %s: %v\n", filename, err)
		} else {
			log.Printf("Created file %s with default content\n", filename)
		}
	}
}
