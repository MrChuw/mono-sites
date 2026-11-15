package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

func multipartUpload(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not json: %q (%v)", rec.Body.String(), err)
	}
	return out
}

func uploadRequest(t *testing.T, client *bun.DB, url string, key, subfolder string, filename string, content []byte, clean bool) *httptest.ResponseRecorder {
	t.Helper()
	body, ctype := multipartUpload(t, filename, content)
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", ctype)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if subfolder != "" {
		req.Header.Set("X-Name", subfolder)
	}
	h := CloudflareMiddleware(AuthMiddleware(client)(UploadFileHandler(client, clean)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCloudflareMiddleware(t *testing.T) {
	var got CloudflareMetadata
	h := CloudflareMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = getCloudflareFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("CF-Connecting-IP", "1.1.1.1")
	req.Header.Set("CF-IPCountry", "BR")
	req.Header.Set("User-Agent", "sharex/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.IP != "1.1.1.1" || got.Country != "BR" || got.UA != "sharex/1.0" {
		t.Fatalf("explicit headers: %+v", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Forwarded-For", "2.2.2.2, 3.3.3.3")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.IP != "2.2.2.2" || got.Country != "XX" || got.UA != "Unknown" {
		t.Fatalf("fallbacks: %+v", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.IP != "127.0.0.1" {
		t.Fatalf("loopback default: %+v", got)
	}
}

func TestUploadRequiresAuth(t *testing.T) {
	client := setupService(t)

	rec := httptest.NewRecorder()
	UploadFileHandler(client, false)(rec, httptest.NewRequest(http.MethodPost, "/api/upload", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(client)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := getAPIKeyFromContext(r.Context())
		if !ok || key.Owner != "alice" {
			t.Error("authenticated request lost its API key in context")
		}
		w.WriteHeader(http.StatusTeapot)
	}))

	before := stats.AuthFailures()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing header: code = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-API-Key", "not-a-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid key: code = %d, want 403", rec.Code)
	}
	if got := stats.AuthFailures() - before; got != 2 {
		t.Fatalf("auth failures delta = %d, want 2", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-API-Key", "k1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("valid key: code = %d, want 418 (reached next)", rec.Code)
	}
}

func TestUploadHappyPath(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	content := []byte("hello world")
	upBefore, rxBefore, storedBefore := stats.FilesUploaded(), stats.ReceivedBytes(), stats.StoredBytes()

	rec := uploadRequest(t, client, "/api/upload?ttl_seconds=60", "k1", "My Scope!", "hello.txt", content, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
	}
	res := decodeResponse(t, rec)

	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])
	if res["status"] != "success" || res["hash"] != wantHash || res["deduplicated"] != "false" {
		t.Fatalf("response wrong: %v", res)
	}

	urlStr, _ := res["url"].(string)
	if !strings.HasPrefix(urlStr, "http://example.com/myscope/") || !strings.HasSuffix(urlStr, ".txt") {
		t.Fatalf("url wrong: %q", urlStr)
	}
	if thumb, _ := res["thumbnail_url"].(string); !strings.HasSuffix(thumb, "/preview/t/"+strings.TrimSuffix(filepath.Base(urlStr), ".txt")+".png") {
		t.Fatalf("thumbnail_url wrong: %q", thumb)
	}
	if del, _ := res["deletion_url"].(string); len(del) != len("http://example.com/api/delete/")+64 ||
		!strings.HasPrefix(del, "http://example.com/api/delete/") {
		t.Fatalf("deletion_url wrong: %q (len %d)", del, len(del))
	}

	diskPath := filepath.Join(config.UploadDir, "myscope", filepath.Base(urlStr))
	got, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("saved content differs: %q", got)
	}

	file, err := db.GetFileByHash(ctx, client, wantHash)
	if err != nil {
		t.Fatal(err)
	}
	if file.Filename != "hello.txt" || file.FileSize != int64(len(content)) || file.APIKeyID != 1 {
		t.Fatalf("row wrong: %+v", file)
	}
	if file.ExpiresAt == nil || file.ExpiresAt.Sub(time.Now().Add(60*time.Second)).Abs() > 5*time.Second {
		t.Fatalf("expires_at wrong: %v", file.ExpiresAt)
	}

	var tagNames []string
	if err := client.NewSelect().
		Model((*db.Tag)(nil)).
		Join("JOIN file_tags AS ft ON ft.tag_id = t.id").
		ColumnExpr("t.name").
		Where("ft.uploaded_file_id = ?", file.ID).
		Scan(ctx, &tagNames); err != nil {
		t.Fatal(err)
	}
	if len(tagNames) != 2 || !hasString(tagNames, "normal") || !hasString(tagNames, "alice") {
		t.Fatalf("tags wrong: %v", tagNames)
	}

	if got := stats.FilesUploaded() - upBefore; got != 1 {
		t.Errorf("uploads delta = %d, want 1", got)
	}
	if got := stats.ReceivedBytes() - rxBefore; got != int64(len(content)) {
		t.Errorf("received delta = %d, want %d", got, len(content))
	}
	if got := stats.StoredBytes() - storedBefore; got != int64(len(content)) {
		t.Errorf("stored delta = %d, want %d", got, len(content))
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestUploadDeduplicates(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	content := []byte("dedup-me-please")

	first := uploadRequest(t, client, "/api/upload", "k1", "one", "a.txt", content, false)
	if first.Code != http.StatusOK {
		t.Fatalf("first upload: %d %s", first.Code, first.Body.String())
	}
	storedAfterFirst := stats.StoredBytes()

	second := uploadRequest(t, client, "/api/upload", "k1", "two", "b.txt", content, false)
	if second.Code != http.StatusOK {
		t.Fatalf("second upload: %d %s", second.Code, second.Body.String())
	}
	res := decodeResponse(t, second)
	if res["deduplicated"] != "true" {
		t.Fatalf("second upload not deduplicated: %v", res)
	}

	firstURL, _ := decodeResponse(t, first)["url"].(string)
	firstPath := filepath.Join(config.UploadDir, "one", filepath.Base(firstURL))
	secondPath := filepath.Join(config.UploadDir, "two", filepath.Base(res["url"].(string)))
	fi1, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi1, fi2) {
		t.Fatal("dedup did not hardlink the copies")
	}

	if got := stats.DedupHits(); got != 1 {
		t.Errorf("dedup hits = %d, want 1", got)
	}
	if got := stats.StoredBytes() - storedAfterFirst; got != 0 {
		t.Errorf("stored delta after dedup = %d, want 0", got)
	}
	if got := stats.DedupSavedBytes(); got != int64(len(content)) {
		t.Errorf("dedup saved bytes = %d, want %d", got, len(content))
	}
}

func TestUploadOversizeRejected(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	config.DefaultMaxUploadSize = 4
	rec := uploadRequest(t, client, "/api/upload", "k1", "", "big.txt", []byte("way over the limit"), false)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}

	var rows, err2 = client.NewSelect().Model((*db.UploadedFile)(nil)).Count(ctx)
	if err2 != nil {
		t.Fatal(err2)
	}
	if rows != 0 {
		t.Fatalf("rejected upload left %d rows", rows)
	}
	entries, err := os.ReadDir(config.UploadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload dir not clean: %v", entries)
	}
}

func TestUploadWithMetadataClean(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(config.UploadDir, "src.png")
	writeSmallPNG(t, src)
	cmd := exec.Command("exiftool", "-overwrite_original", "-Comment=handler-secret", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("planting metadata: %v (%s)", err, out)
	}
	content, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	originalHash := hex.EncodeToString(sum[:])

	rec := uploadRequest(t, client, "/api/upload", "k1", "", "photo.png", content, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
	}
	res := decodeResponse(t, rec)

	urlStr, _ := res["url"].(string)
	savedPath := filepath.Join(config.UploadDir, filepath.Base(urlStr))
	saved, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("cleaned file missing: %v", err)
	}

	cleanSum := sha256.Sum256(saved)
	if res["hash"] != hex.EncodeToString(cleanSum[:]) {
		t.Fatalf("hash is not of the cleaned file: %v", res["hash"])
	}
	if res["hash"] == originalHash {
		t.Fatal("metadata stripping did not change the file, hash unchanged")
	}

	out, err := exec.Command("exiftool", "-s3", "-Comment", savedPath).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("comment survived the upload: %q", string(out))
	}
}

func writeSmallPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: 128, B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestBuildBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		proto    string
		wantBase string
	}{
		{"proxy host untouched", "i.example.com", "", "http://i.example.com"},
		{"cdn host swaps to proxy", "im.example.com", "", "http://i.example.com"},
		{"upload localhost swaps", "upload.localhost:8080", "", "http://i.localhost:8080"},
		{"plain localhost untouched", "localhost:8080", "", "http://localhost:8080"},
		{"forwarded proto wins", "i.example.com", "https", "https://i.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.ForwardedProto = tc.proto
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
			if got := buildBaseURL(req); got != tc.wantBase {
				t.Errorf("buildBaseURL(%q) = %q, want %q", tc.host, got, tc.wantBase)
			}
		})
	}
}
