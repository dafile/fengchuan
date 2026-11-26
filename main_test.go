package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *http.ServeMux {
	t.Helper()
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	testDir := t.TempDir()
	if err := os.Chdir(testDir); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"data/files", "data/text", "data/thumbs", "data/notepad"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	authPassword = "web-password"
	authAPIKey = "api-token"
	authTTL = 10 * time.Minute
	if err := initSessionSecurity(); err != nil {
		t.Fatal(err)
	}
	expirationTracker = initExpirationTracker()
	resetAuthRateLimiter()
	t.Cleanup(func() {
		os.Chdir(previousDir)
		authPassword = ""
		authAPIKey = ""
		resetAuthRateLimiter()
	})
	mux := http.NewServeMux()
	registerRoutes(mux)
	return mux
}

func doRequest(t *testing.T, handler http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func loginCookie(t *testing.T, app http.Handler) *http.Cookie {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"password": authPassword})
	res := doRequest(t, app, http.MethodPost, "/api/login", bytes.NewReader(payload), map[string]string{
		"Content-Type": "application/json",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", res.Code, res.Body.String())
	}
	cookies := res.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != "lc_auth" {
		t.Fatal("login did not set lc_auth")
	}
	return cookies[0]
}

func multipartFile(t *testing.T, filename, content string, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	for key, value := range extra {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func TestSafeDataPathRejectsTraversal(t *testing.T) {
	cases := []string{
		"files\\..\\..\\secret.txt",
		"files%5C..%5C..%5Csecret.txt",
		"text\\..\\..\\secret.txt",
		"files/../../secret.txt",
		"files/C:/secret.txt",
		"files/CON",
		"..\\..\\secret.txt",
		"/secret.txt",
		"notepad/md.file",
		"settings.json",
		"",
	}
	for _, id := range cases {
		got, err := safeDataPath(id)
		if err == nil {
			t.Fatalf("safeDataPath(%q) = %q, want error", id, got)
		}
	}
	got, err := safeDataPath("files/app.txt")
	if err != nil || filepath.ToSlash(got) != "data/files/app.txt" {
		t.Fatalf("safeDataPath(files/app.txt) = %q, %v", got, err)
	}
}

func TestAuthMiddlewareRejectsTraversalAndKeepsOutsideFile(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	outside := filepath.Join("secret.txt")
	if err := os.WriteFile(outside, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	cookie := loginCookie(t, app)
	targets := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/download/files%5C..%5Csecret.txt", ""},
		{http.MethodGet, "/raw/text%5C..%5Csecret.txt", ""},
		{http.MethodGet, "/view/files%5C..%5Csecret.txt", ""},
		{http.MethodGet, "/video-thumb/files%5C..%5Csecret.txt", ""},
		{http.MethodPost, "/delete/files%5C..%5Csecret.txt", ""},
		{http.MethodPost, "/edit/text%5C..%5Csecret.txt", "new content"},
		{http.MethodPost, "/rename/files%5C..%5Csecret.txt", "newname=renamed.txt"},
	}
	for _, tc := range targets {
		res := doRequest(t, app, tc.method, tc.path, strings.NewReader(tc.body), map[string]string{
			"Cookie":       cookie.Name + "=" + cookie.Value,
			"Content-Type": "application/x-www-form-urlencoded",
		})
		if res.Code < 400 {
			t.Fatalf("%s %s status = %d, want >= 400; body=%s", tc.method, tc.path, res.Code, res.Body.String())
		}
		data, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("outside file disappeared while testing %s: %v", tc.path, err)
		}
		if string(data) != "keep me" {
			t.Fatalf("outside file changed while testing %s: %q", tc.path, data)
		}
	}
}

func TestLoginCookieIsHttpOnlyAndSameSiteLax(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	cookie := loginCookie(t, app)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: HttpOnly=%v SameSite=%v", cookie.HttpOnly, cookie.SameSite)
	}
}

func TestLoginRateLimit(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	payload := []byte(`{"password":"wrong"}`)
	for i := 0; i < 5; i++ {
		res := doRequest(t, app, http.MethodPost, "/api/login", bytes.NewReader(payload), map[string]string{
			"Content-Type": "application/json",
		})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, body = %s", i+1, res.Code, res.Body.String())
		}
	}
	res := doRequest(t, app, http.MethodPost, "/api/login", bytes.NewReader(payload), map[string]string{
		"Content-Type": "application/json",
	})
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestLogoutRevokesExistingToken(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	cookie := loginCookie(t, app)
	logout := doRequest(t, app, http.MethodPost, "/api/logout", nil, map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
	})
	if logout.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logout.Code, logout.Body.String())
	}
	reuse := doRequest(t, app, http.MethodGet, "/", nil, map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
	})
	if reuse.Code != http.StatusSeeOther {
		t.Fatalf("revoked token status = %d, want redirect to login", reuse.Code)
	}
}

func TestLoginPasswordIsNotAPIToken(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	body, contentType := multipartFile(t, "app.txt", "payload", nil)
	res := doRequest(t, app, http.MethodPost, "/api/upload", body, map[string]string{
		"Authorization": "Bearer " + authPassword,
		"Content-Type":  contentType,
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("old password API status = %d, body = %s", res.Code, res.Body.String())
	}
	if _, err := os.Stat("data/files/app.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected upload created a file")
	}
}

func TestIndependentAPIKeyCanUpload(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	body, contentType := multipartFile(t, "app.txt", "payload", map[string]string{
		"sender": "TestAgent",
		"note":   "release notes",
	})
	res := doRequest(t, app, http.MethodPost, "/api/upload", body, map[string]string{
		"Authorization": "Bearer " + authAPIKey,
		"Content-Type":  contentType,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("API upload status = %d, body = %s", res.Code, res.Body.String())
	}
	data, err := os.ReadFile("data/files/app.txt")
	if err != nil || string(data) != "payload" {
		t.Fatalf("uploaded file = %q, %v", data, err)
	}
	if _, err := os.Stat("data/files/app.txt.meta"); err != nil {
		t.Fatalf("upload metadata missing: %v", err)
	}
	entries, _ := os.ReadDir("data/text")
	noteCount := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".meta") && strings.HasSuffix(entry.Name(), ".md") {
			noteCount++
		}
	}
	if noteCount != 1 {
		t.Fatalf("API note not saved: %d entries", noteCount)
	}
}

func TestAPIUploadRejectsTooLargeBody(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	oldLimit := maxUploadBytes
	maxUploadBytes = 8
	defer func() { maxUploadBytes = oldLimit }()
	body, contentType := multipartFile(t, "large.txt", "this payload is too large", nil)
	res := doRequest(t, app, http.MethodPost, "/api/upload", body, map[string]string{
		"Authorization": "Bearer " + authAPIKey,
		"Content-Type":  contentType,
	})
	if res.Code != http.StatusRequestEntityTooLarge && res.Code != http.StatusBadRequest {
		t.Fatalf("oversized upload status = %d, body = %s", res.Code, res.Body.String())
	}
	entries, _ := os.ReadDir("data/files")
	if len(entries) != 0 {
		t.Fatalf("oversized upload saved %d files", len(entries))
	}
}

func TestAPIUploadConcurrentSameNamesDoNotOverwrite(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	const count = 20
	responses := make([]string, count)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("payload-%02d", i)
			body, contentType := multipartFile(t, "same.txt", content, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/upload", body)
			req.Header.Set("Authorization", "Bearer "+authAPIKey)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)
			mu.Lock()
			responses[i] = fmt.Sprintf("%d|%s", rec.Code, rec.Body.String())
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	files := []string{}
	for _, entry := range mustReadDir(t, "data/files") {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		files = append(files, entry.Name())
	}
	if len(files) != count {
		t.Fatalf("saved %d files, want %d; responses=%v", len(files), count, responses)
	}
	names := map[string]bool{}
	for _, name := range files {
		if names[name] {
			t.Fatalf("duplicate filename %q", name)
		}
		names[name] = true
	}
	pattern := regexp.MustCompile(`^(?:[0-9a-f]{8}-)?same\.txt$`)
	for _, name := range files {
		if !pattern.MatchString(name) {
			t.Fatalf("unexpected filename %q", name)
		}
	}
}

func TestViewForcesAttachment(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	body, contentType := multipartFile(t, "unsafe.html", `<script>alert(1)</script>`, nil)
	res := doRequest(t, app, http.MethodPost, "/api/upload", body, map[string]string{
		"Authorization": "Bearer " + authAPIKey,
		"Content-Type":  contentType,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", res.Code, res.Body.String())
	}
	cookie := loginCookie(t, app)
	view := doRequest(t, app, http.MethodGet, "/view/files/unsafe.html", nil, map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
	})
	if view.Code != http.StatusOK {
		t.Fatalf("view status = %d, body = %s", view.Code, view.Body.String())
	}
	if got := view.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want attachment", got)
	}
	if got := view.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := view.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
}

func TestRenameMovesFileAndMetadata(t *testing.T) {
	app := authMiddleware(newTestApp(t))
	cookie := loginCookie(t, app)
	if err := os.WriteFile("data/files/old.txt", []byte("rename me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("data/files/old.txt.meta", []byte(`{"sender":"before"}`), 0644); err != nil {
		t.Fatal(err)
	}
	res := doRequest(t, app, http.MethodPost, "/rename/files/old.txt", strings.NewReader("newname=new.txt"), map[string]string{
		"Cookie":       cookie.Name + "=" + cookie.Value,
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("rename status = %d, body = %s", res.Code, res.Body.String())
	}
	if _, err := os.Stat("data/files/old.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old file still exists: %v", err)
	}
	if data, err := os.ReadFile("data/files/new.txt"); err != nil || string(data) != "rename me" {
		t.Fatalf("renamed file = %q, %v", data, err)
	}
	if _, err := os.Stat("data/files/new.txt.meta"); err != nil {
		t.Fatalf("renamed metadata missing: %v", err)
	}
}

func TestUniqueFileCreationRace(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	names := make(chan string, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			file, name, err := createUniqueFile(dir, "race.txt")
			if err != nil {
				errs <- err
				return
			}
			file.Close()
			names <- name
		}()
	}
	wg.Wait()
	close(names)
	close(errs)
	for err := range errs {
		t.Fatalf("unique creation failed: %v", err)
	}
	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Fatalf("duplicate unique name %q", name)
		}
		seen[name] = true
	}
	if len(seen) != 16 {
		t.Fatalf("created %d unique files, want 16", len(seen))
	}
}

func TestAtomicWriteFileOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := atomicWriteFile(path, []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("second"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("content = %q, want second", data)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
