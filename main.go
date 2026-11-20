package main

import (
	"archive/zip"
	"crypto/hmac"
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
	"math/rand"
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
	os.WriteFile(filepath.Join("data", dir, name+".meta"), b, 0644)
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
const appVersion = "v1.13.1"
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
				tracker.Expirations = storedTracker.Expirations
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
	if err := os.WriteFile(expirationFile, data, 0644); err != nil {
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
		err := os.Remove(filepath.Join("data", fileID))
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Error removing expired file %s: %v", fileID, err)
		} else {
			log.Printf("Removed expired file: %s", fileID)
		}
		delete(t.Expirations, fileID)
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
		notifyContentChange()
	}
	return expiredFiles
}

// ---------- 访问鉴权与站点设置 ----------

var passwordFlag = flag.String("password", "change-me", "access password for the web UI")
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
	return os.WriteFile(filepath.Join("data", "settings.json"), b, 0644)
}

func tokenFor(exp int64) string {
	mac := hmac.New(sha256.New, []byte(authPassword+"|lc"))
	fmt.Fprintf(mac, "%d", exp)
	return strconv.FormatInt(exp, 10) + ":" + hex.EncodeToString(mac.Sum(nil))
}

func tokenExpiry(tok string) (int64, bool) {
	i := strings.Index(tok, ":")
	if i <= 0 {
		return 0, false
	}
	exp, err := strconv.ParseInt(tok[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	mac := hmac.New(sha256.New, []byte(authPassword+"|lc"))
	fmt.Fprintf(mac, "%d", exp)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(tok[i+1:])) {
		return 0, false
	}
	return exp, true
}

func isAuthed(r *http.Request) bool {
	c, err := r.Cookie("lc_auth")
	if err != nil {
		return false
	}
	exp, ok := tokenExpiry(c.Value)
	if !ok {
		return false
	}
	return time.Now().Unix() < exp
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		public := p == "/login" || p == "/api/login" || p == "/api/logout" ||
			(p == "/api/settings" && r.Method == http.MethodGet) ||
			strings.HasPrefix(p, "/static/") ||
			p == "/favicon.ico" || p == "/manifest.json" || p == "/sw.js" ||
			p == "/icon-192.png" || p == "/icon-512.png" || p == "/style.css"
		if !public && !isAuthed(r) {
			if strings.HasPrefix(p, "/api/") || p == "/submit" {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if !public {
			if c, err := r.Cookie("lc_auth"); err == nil {
				ttl := getAuthTTL()
				if exp, ok := tokenExpiry(c.Value); ok && exp-time.Now().Unix() < int64(ttl.Seconds()/2) {
					http.SetCookie(w, &http.Cookie{Name: "lc_auth", Value: tokenFor(time.Now().Add(ttl).Unix()), Path: "/", MaxAge: int(ttl.Seconds())})
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

func generateUniqueFilename(baseDir, baseName string) string {
	baseName = strings.TrimSpace(baseName)
	// Sanitize: allow only letters (+unicode), numbers, space, dot, hyphen, underscore, () and []
	reg := regexp.MustCompile(`[^\p{L}\p{N}\p{M}\s\.\-_()\[\]]`)
	sanitizedName := reg.ReplaceAllString(baseName, "-")
	log.Printf("Sanitized name %s TO %s\n", baseName, sanitizedName)
	// First try without random prefix
	if _, err := os.Stat(filepath.Join(baseDir, sanitizedName)); os.IsNotExist(err) {
		return sanitizedName
	}
	// If file exists, add random prefix until we find a unique name
	for {
		randChars := fmt.Sprintf("%04d", rand.Intn(10000))
		newName := fmt.Sprintf("%s-%s", randChars, sanitizedName)
		if _, err := os.Stat(filepath.Join(baseDir, newName)); os.IsNotExist(err) {
			return newName
		}
	}
}

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
			ID:       filepath.Join("text", file.Name()),
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
			ID:       filepath.Join("files", file.Name()),
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
	authPassword = *passwordFlag
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

	tmpl := template.Must(template.ParseFS(content, "templates/*.html"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/md", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "md.html", nil)
	})

	// Retrieve custom expiration options
	http.HandleFunc("/api/batch-download", func(w http.ResponseWriter, r *http.Request) {
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logAccess(r, "批量下载", fmt.Sprintf("%d 个文件", len(ids)))
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=\"lanchat.zip\"")
		zw := zip.NewWriter(w)
		defer zw.Close()
		for _, id := range ids {
			rel := filepath.FromSlash(id)
			if strings.Contains(rel, "..") {
				continue
			}
			data, err := os.ReadFile(filepath.Join("data", rel))
			if err != nil {
				continue
			}
			entryName := strings.ReplaceAll(rel, "\\", "/")
			fw, err := zw.Create(entryName)
			if err != nil {
				continue
			}
			fw.Write(data)
		}
	})

	http.HandleFunc("/api/delete-batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, id := range ids {
			rel := filepath.FromSlash(id)
			if strings.Contains(rel, "..") {
				continue
			}
			os.Remove(filepath.Join("data", rel))
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		logAccess(r, "打开登录页", "")
		tmpl.ExecuteTemplate(w, "login.html", nil)
	})

	http.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(payload.Password), []byte(authPassword)) != 1 {
			logAccess(r, "登录失败", "密码错误")
			time.Sleep(400 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "密码错误"})
			return
		}
		ttl := getAuthTTL()
		if payload.Remember {
			ttl = 365 * 24 * time.Hour
		}
		http.SetCookie(w, &http.Cookie{Name: "lc_auth", Value: tokenFor(time.Now().Add(ttl).Unix()), Path: "/", MaxAge: int(ttl.Seconds())})
		logAccess(r, "登录成功", "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	http.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "lc_auth", Value: "", Path: "/", MaxAge: -1})
		logAccess(r, "退出登录", "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	http.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "POST" {
			var payload struct {
				DefaultTheme string `json:"defaultTheme"`
				SessionTTL   string `json:"sessionTTL"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
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
				http.Error(w, err.Error(), http.StatusInternalServerError)
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

	http.HandleFunc("/access-log", func(w http.ResponseWriter, r *http.Request) {
		logAccess(r, "访问记录页", "")
		tmpl.ExecuteTemplate(w, "accesslog.html", nil)
	})

	http.HandleFunc("/api/access-log", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/api/access-log-clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		accessLogMu.Lock()
		os.WriteFile(accessLogPath, []byte{}, 0644)
		accessLogMu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"version": appVersion})
	})

	http.HandleFunc("/api/changelog", func(w http.ResponseWriter, r *http.Request) {
		data, err := content.ReadFile(changelogPath)
		if err != nil {
			http.Error(w, "CHANGELOG.md not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	})

	http.HandleFunc("/getExpiryOptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expirationOptions)
	})

	// Serve static files from embedded filesystem
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		log.Fatalf("Failed to create static sub-filesystem: %v", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	http.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("style.css")
		if err != nil {
			http.Error(w, "Style not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "text/css")
		io.Copy(w, file)
	})

	http.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("manifest.json")
		if err != nil {
			http.Error(w, "Manifest not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, file)
	})

	http.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("sw.js")
		if err != nil {
			http.Error(w, "Service worker not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/javascript")
		io.Copy(w, file)
	})

	http.HandleFunc("/md.js", func(w http.ResponseWriter, r *http.Request) {
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
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("favicon.ico")
		if err != nil {
			http.Error(w, "Favicon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/x-icon")
		io.Copy(w, file)
	})

	http.HandleFunc("/icon-192.png", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("icon-192.png")
		if err != nil {
			http.Error(w, "Icon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/png")
		io.Copy(w, file)
	})

	http.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
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
	http.HandleFunc("/notepad/", func(w http.ResponseWriter, r *http.Request) {
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
			content, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}
			err = os.WriteFile(filepath.Join("data", "notepad", filename), content, 0644)
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

	http.HandleFunc("/api/entries", func(w http.ResponseWriter, r *http.Request) {
		entries := collectEntries()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(entries)
	})

	http.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		contentType := r.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "application/json") {
			var payload struct {
				Content string `json:"content"`
				Name    string `json:"name"`
				Type    string `json:"type"`
				Expiry  string `json:"expiry"`
				Sender  string `json:"sender"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
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
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				defer f.Close()
				if _, err := f.WriteString(payload.Content + "\n"); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
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
				uniqueFileName := generateUniqueFilename("data/text", filename)
				if err := os.WriteFile(filepath.Join("data/text", uniqueFileName), []byte(payload.Content), 0644); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				writeMeta("text", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: payload.Sender})
				logAccess(r, "发消息", payload.Sender)
				if payload.Expiry != "" && payload.Expiry != "Never" {
					expirationTracker.SetExpiration(filepath.Join("text", uniqueFileName), payload.Expiry)
				}
			}
			notifyContentChange()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		if err := r.ParseMultipartForm(100 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()
			if _, err := f.WriteString(content + "\n"); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
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
						uniqueFileName := generateUniqueFilename("data/files", fileName)
						f, err := os.Create(filepath.Join("data/files", uniqueFileName))
						if err != nil {
							return err
						}
						defer f.Close()
						if _, err := io.Copy(f, file); err != nil {
							return err
						}
						writeMeta("files", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: r.FormValue("sender")})
						logAccess(r, "上传文件", uniqueFileName)
						if expiryOption != "Never" {
							fileID := filepath.Join("files", uniqueFileName)
							expirationTracker.SetExpiration(fileID, expiryOption)
						}
						log.Printf("Saved file %s with expiry %s\n", uniqueFileName, expiryOption)
						return nil
					}()
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}
			} else if content != "" {
				// Text snippet submission
				filename := name
				if filename == "" {
					filename = time.Now().Format("Jan-02 15-04-05")
				}
				uniqueFileName := generateUniqueFilename("data/text", filename)
				err := os.WriteFile(filepath.Join("data/text", uniqueFileName), []byte(content), 0644)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				writeMeta("text", uniqueFileName, fileMeta{Ts: time.Now().Unix(), From: clientIP(r), Sender: r.FormValue("sender")})
				logAccess(r, "发消息", r.FormValue("sender"))
				if expiryOption != "Never" {
					fileID := filepath.Join("text", uniqueFileName)
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

	http.HandleFunc("/rename/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		oldPath := strings.TrimPrefix(r.URL.Path, "/rename/")
		newName := r.FormValue("newname")
		if newName == "" {
			http.Error(w, "New name cannot be empty", http.StatusBadRequest)
			return
		}
		baseDir := filepath.Dir(filepath.Join("data", oldPath))
		newName = generateUniqueFilename(baseDir, newName)

		// Get the new full path
		newPath := filepath.Join(baseDir, newName)
		oldFullPath := filepath.Join("data", oldPath)
		// Check if there's an expiration for this file
		expirationTracker.mu.Lock()
		expiryTime, hasExpiry := expirationTracker.Expirations[oldPath]
		if hasExpiry {
			// Remove old entry and add new one
			delete(expirationTracker.Expirations, oldPath)
			relNewPath := strings.TrimPrefix(newPath, "data/")
			relNewPath = strings.ReplaceAll(relNewPath, "\\", "/") // Ensure cross-platform path separators
			expirationTracker.Expirations[relNewPath] = expiryTime
			expirationTracker.saveToFile()
		}
		expirationTracker.mu.Unlock()
		// Rename the file
		err := os.Rename(oldFullPath, newPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Renamed %s to %s\n", oldPath, newName)
	})

	http.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/raw/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Only text files can be accessed", http.StatusBadRequest)
			return
		}
		logAccess(r, "查看", strings.TrimPrefix(id, "text/"))
		content, err := os.ReadFile(filepath.Join("data", id))
		if err != nil {
			http.Error(w, "File not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(content)
	})

	http.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/download/")
		accept := r.Header.Get("Accept")
		if !strings.HasPrefix(accept, "image/") && !strings.HasPrefix(accept, "video/") && !strings.HasPrefix(accept, "audio/") {
			logAccess(r, "查看/下载", filepath.Base(filename))
		}
		filePath := filepath.Join("data", filename)
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			contentType = http.DetectContentType(buffer)
			_, err = file.Seek(0, 0)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		baseFilename := filepath.Base(filename)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", baseFilename))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, baseFilename, fileInfo.ModTime(), file)
		log.Printf("Served %s for download\n", filename)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/view/")
		http.ServeFile(w, r, filepath.Join("data", filename))
		log.Printf("Served %s for viewing\n", filename)
	})

	http.HandleFunc("/video-thumb/", func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/video-thumb/"))
		if id == "." || id == ".." || filepath.IsAbs(id) || strings.HasPrefix(id, ".."+string(os.PathSeparator)) {
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
		videoPath := filepath.Join("data", id)
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

	http.HandleFunc("/delete/", func(w http.ResponseWriter, r *http.Request) {
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
			err = os.WriteFile(linksFilePath, []byte(output), 0644)
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
		err := os.Remove(filepath.Join("data", id))
		if err != nil {
			log.Printf("Failed to delete %s: %v", id, err)
			http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			return
		}
		os.Remove(videoThumbnailPath(id))
		expirationTracker.mu.Lock()
		delete(expirationTracker.Expirations, id)
		expirationTracker.saveToFile()
		expirationTracker.mu.Unlock()
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
		log.Printf("Deleted %s\n", id)
	})

	http.HandleFunc("/edit/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/edit/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Can only edit text snippets", http.StatusBadRequest)
			return
		}
		content := r.FormValue("content")
		if content == "" {
			http.Error(w, "Content cannot be empty", http.StatusBadRequest)
			return
		}
		err := os.WriteFile(filepath.Join("data", id), []byte(content), 0644)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Edited %s\n", id)
	})

	// SSE Updates for content refresh
	http.HandleFunc("/api/updates", handleContentUpdates)

	// Start server
	log.Fatal(http.ListenAndServe(*listenAddress, authMiddleware(http.DefaultServeMux)))
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
