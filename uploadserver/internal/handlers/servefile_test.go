package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"uploadserver/internal/config"
	"uploadserver/internal/stats"
)

func seedServedFile(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(config.UploadDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestServeFileHandler(t *testing.T) {
	client := setupService(t)
	h := ServeFileHandler(client)

	content := "plain text file body"
	seedServedFile(t, "abc123.txt", content)
	seedServedFile(t, ".hidden", "secret")
	seedServedFile(t, filepath.Join("preview", "t", "abc123.jpg"), "jpeg-bytes")

	do := func(target string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://i.example.com"+target, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	t.Run("serves file with cache metadata", func(t *testing.T) {
		txBefore := stats.Transferred()
		rec := do("/abc123.txt", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		if got := rec.Body.String(); got != content {
			t.Fatalf("body = %q", got)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=60") {
			t.Fatalf("cache control = %q", cc)
		}
		if etag := rec.Header().Get("ETag"); etag == "" || !strings.HasPrefix(etag, `"`) {
			t.Fatalf("etag = %q", etag)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("nosniff header missing")
		}
		if got := stats.Transferred() - txBefore; got != int64(len(content)) {
			t.Fatalf("transferred delta = %d, want %d", got, len(content))
		}
	})

	t.Run("dot segments forbidden", func(t *testing.T) {
		for _, target := range []string{"/../outside", "/.hidden"} {
			rec := do(target, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("GET %q: code = %d, want 403", target, rec.Code)
			}
		}
	})

	t.Run("missing file not found", func(t *testing.T) {
		if rec := do("/nope.txt", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", rec.Code)
		}
	})

	t.Run("caddy accel redirect", func(t *testing.T) {
		rec := do("/abc123.txt", map[string]string{"X-Handled-By": "Caddy"})
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		if got := rec.Header().Get("X-Accel-Redirect"); got != "/internal-media/abc123.txt" {
			t.Fatalf("X-Accel-Redirect = %q", got)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("caddy mode must not stream the body, got %d bytes", rec.Body.Len())
		}
	})

	t.Run("no-cache host redirects to cdn", func(t *testing.T) {
		config.ForwardedProto = "https"
		rec := do("/abc123.txt", map[string]string{"X-Host-Type": "no-cache"})
		if rec.Code != http.StatusFound {
			t.Fatalf("code = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "https://im.example.com/abc123.txt" {
			t.Fatalf("Location = %q", loc)
		}
	})

	t.Run("link resolver gets og page", func(t *testing.T) {
		ogBefore := stats.ResolverHits()
		rec := do("/abc123.txt", map[string]string{
			"User-Agent": "chatterino-api-cache/1.0.0 link-resolver",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `og:title`) || !strings.Contains(body, `og:image`) {
			t.Fatalf("og tags missing: %q", body)
		}
		if !strings.Contains(body, "abc123.txt") {
			t.Fatalf("title/filename missing: %q", body)
		}
		if got := stats.ResolverHits() - ogBefore; got != 1 {
			t.Fatalf("resolver hits delta = %d, want 1", got)
		}
	})

	t.Run("preview path uses revalidation cache", func(t *testing.T) {
		txBefore := stats.Transferred()
		rec := do("/preview/t/abc123.jpg", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		if got := rec.Body.String(); got != "jpeg-bytes" {
			t.Fatalf("body = %q", got)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "must-revalidate") {
			t.Fatalf("cache control = %q", cc)
		}
		if got := stats.Transferred() - txBefore; got != int64(len("jpeg-bytes")) {
			t.Fatalf("transferred delta = %d, want %d", got, len("jpeg-bytes"))
		}
	})
}

func TestFaviconHandler(t *testing.T) {
	client := setupService(t)
	h := FaviconHandler(client)

	t.Run("default white pixel", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("content type = %q", ct)
		}
		body := rec.Body.Bytes()
		if len(body) < 8 || string(body[:4]) != "\x89PNG" {
			t.Fatal("default favicon is not a PNG")
		}
	})

	t.Run("custom favicon wins", func(t *testing.T) {
		seedServedFile(t, "favicon.ico", "CUSTOM-ICON")
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
		if got := rec.Body.String(); got != "CUSTOM-ICON" {
			t.Fatalf("body = %q, want the uploaded favicon", got)
		}
	})
}
