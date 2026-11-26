package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxUploadBytes = int64(2 << 30)
	maxJSONBodyBytes      = int64(32 << 20)
	maxMessageBytes       = int64(4 << 20)
	apiKeyFile            = "data/api-key"
	sessionKeyFile        = "data/session-key"
	revokedSessionsFile   = "data/revoked-sessions.json"
)

var maxUploadBytes = defaultMaxUploadBytes

var (
	authAPIKey         string
	authLimiter        = newAuthAttemptLimiter(5, time.Minute)
	sessionTokenSecret []byte
	revokedSessionsMu  sync.Mutex
	revokedSessions    = make(map[string]int64)
)

func safeDataPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsRune(id, '\x00') || strings.Contains(id, ":") {
		return "", fmt.Errorf("invalid path")
	}
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(id)))
	if normalized == "." || normalized == ".." ||
		strings.HasPrefix(normalized, "../") ||
		filepath.IsAbs(filepath.FromSlash(id)) {
		return "", fmt.Errorf("invalid path")
	}
	if normalized != "text" && !strings.HasPrefix(normalized, "text/") &&
		normalized != "files" && !strings.HasPrefix(normalized, "files/") {
		return "", fmt.Errorf("path prefix not allowed")
	}
	if normalized == "text" || normalized == "files" {
		return "", fmt.Errorf("directory path not allowed")
	}
	for _, component := range strings.Split(normalized, "/") {
		base := strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(component, filepath.Ext(component))))
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			(base >= "COM1" && base <= "COM9") || (base >= "LPT1" && base <= "LPT9") {
			return "", fmt.Errorf("reserved path")
		}
	}
	fullPath := filepath.Join("data", filepath.FromSlash(normalized))
	relative, err := filepath.Rel("data", fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid path")
	}
	return fullPath, nil
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r >= 0x80:
			builder.WriteRune(r)
		case r == ' ' || r == '.' || r == '-' || r == '_' || r == '(' || r == ')' || r == '[' || r == ']':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	name = strings.Trim(builder.String(), " .")
	if name == "" {
		name = "file"
	}
	const maxFilenameRunes = 180
	runes := []rune(name)
	if len(runes) > maxFilenameRunes {
		extension := filepath.Ext(name)
		baseLimit := maxFilenameRunes - len([]rune(extension))
		if baseLimit < 1 {
			baseLimit = maxFilenameRunes
			extension = ""
		}
		name = string(runes[:baseLimit]) + extension
	}
	return name
}

func createUniqueFile(baseDir, baseName string) (*os.File, string, error) {
	baseName = sanitizeFilename(baseName)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := baseName
		if attempt > 0 {
			randomBytes := make([]byte, 4)
			if _, err := rand.Read(randomBytes); err != nil {
				return nil, "", err
			}
			candidate = fmt.Sprintf("%s-%s", hex.EncodeToString(randomBytes), baseName)
		}
		fullPath := filepath.Join(baseDir, candidate)
		file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			return file, candidate, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("unable to allocate a unique filename")
}

func writeUniqueFile(baseDir, baseName string, content []byte) (string, error) {
	file, name, err := createUniqueFile(baseDir, baseName)
	if err != nil {
		return "", err
	}
	fullPath := file.Name()
	cleanup := func() {
		file.Close()
		os.Remove(fullPath)
	}
	if _, err := file.Write(content); err != nil {
		cleanup()
		return "", err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(fullPath)
		return "", err
	}
	return name, nil
}

func reserveUniquePath(baseDir, baseName string) (string, error) {
	file, name, err := createUniqueFile(baseDir, baseName)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return name, nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".fengchuan-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() {
		temp.Close()
		os.Remove(tempPath)
	}
	if err := temp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return err
	}
	return nil
}

func contentDisposition(filename string) string {
	value := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if value == "" {
		return "attachment"
	}
	return value
}

func setAuthCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "lc_auth",
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookie(w http.ResponseWriter) {
	setAuthCookie(w, "", -1)
}

func loadOrCreateAPIKey(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if err := os.MkdirAll(filepath.Dir(apiKeyFile), 0755); err != nil {
			return "", err
		}
		if err := atomicWriteFile(apiKeyFile, []byte(configured+"\n"), 0600); err != nil {
			return "", err
		}
		return configured, nil
	}
	if data, err := os.ReadFile(apiKeyFile); err == nil {
		key := strings.TrimSpace(string(data))
		if len(key) >= 24 {
			return key, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", err
	}
	key := hex.EncodeToString(randomBytes)
	if err := os.MkdirAll(filepath.Dir(apiKeyFile), 0755); err != nil {
		return "", err
	}
	if err := atomicWriteFile(apiKeyFile, []byte(key+"\n"), 0600); err != nil {
		return "", err
	}
	return key, nil
}

func loadOrCreateRandomSecret(path string, size int) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		decoded, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr == nil && len(decoded) == size {
			return decoded, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	secret := make([]byte, size)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if err := atomicWriteFile(path, []byte(hex.EncodeToString(secret)+"\n"), 0600); err != nil {
		return nil, err
	}
	return secret, nil
}

func initSessionSecurity() error {
	secret, err := loadOrCreateRandomSecret(sessionKeyFile, 32)
	if err != nil {
		return err
	}
	sessionTokenSecret = secret
	revokedSessionsMu.Lock()
	defer revokedSessionsMu.Unlock()
	revokedSessions = make(map[string]int64)
	data, err := os.ReadFile(revokedSessionsFile)
	if err == nil {
		var stored map[string]int64
		if json.Unmarshal(data, &stored) == nil {
			now := time.Now().Unix()
			for tokenHash, expiry := range stored {
				if expiry > now {
					revokedSessions[tokenHash] = expiry
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func makeSessionToken(expiry int64) string {
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(nonceBytes)
	mac := hmac.New(sha256.New, sessionTokenSecret)
	fmt.Fprintf(mac, "%d:%s", expiry, nonce)
	return fmt.Sprintf("%d:%s:%s", expiry, nonce, hex.EncodeToString(mac.Sum(nil)))
}

func parseSessionToken(token string) (int64, bool) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 {
		return 0, false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expiry <= 0 || len(parts[1]) < 16 {
		return 0, false
	}
	mac := hmac.New(sha256.New, sessionTokenSecret)
	fmt.Fprintf(mac, "%d:%s", expiry, parts[1])
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
		return 0, false
	}
	return expiry, true
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sessionTokenRevoked(token string) bool {
	now := time.Now().Unix()
	tokenHash := sessionTokenHash(token)
	revokedSessionsMu.Lock()
	defer revokedSessionsMu.Unlock()
	expiry, exists := revokedSessions[tokenHash]
	if !exists {
		return false
	}
	if expiry <= now {
		delete(revokedSessions, tokenHash)
		return false
	}
	return true
}

func revokeSessionToken(token string, expiry int64) error {
	if token == "" || expiry <= time.Now().Unix() {
		return nil
	}
	revokedSessionsMu.Lock()
	defer revokedSessionsMu.Unlock()
	if revokedSessions == nil {
		revokedSessions = make(map[string]int64)
	}
	revokedSessions[sessionTokenHash(token)] = expiry
	now := time.Now().Unix()
	for tokenHash, tokenExpiry := range revokedSessions {
		if tokenExpiry <= now {
			delete(revokedSessions, tokenHash)
		}
	}
	data, err := json.Marshal(revokedSessions)
	if err != nil {
		return err
	}
	return atomicWriteFile(revokedSessionsFile, data, 0600)
}

type authAttemptLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	attempts map[string][]time.Time
}

func newAuthAttemptLimiter(limit int, window time.Duration) *authAttemptLimiter {
	return &authAttemptLimiter{
		limit:    limit,
		window:   window,
		attempts: make(map[string][]time.Time),
	}
}

func authRateKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return clientIP(r)
}

func (l *authAttemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	attempts := l.attempts[key]
	firstValid := 0
	for firstValid < len(attempts) && attempts[firstValid].Before(cutoff) {
		firstValid++
	}
	attempts = attempts[firstValid:]
	if len(attempts) == 0 {
		delete(l.attempts, key)
	} else {
		l.attempts[key] = attempts
	}
	return len(attempts) < l.limit
}

func (l *authAttemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	attempts := l.attempts[key]
	firstValid := 0
	for firstValid < len(attempts) && attempts[firstValid].Before(cutoff) {
		firstValid++
	}
	attempts = append(attempts[firstValid:], now)
	l.attempts[key] = attempts
}

func (l *authAttemptLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

func resetAuthRateLimiter() {
	authLimiter.mu.Lock()
	authLimiter.attempts = make(map[string][]time.Time)
	authLimiter.mu.Unlock()
}
