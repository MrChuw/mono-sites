package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"

	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

func openTestDB(t *testing.T) *bun.DB {
	t.Helper()
	sqldb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	client := bun.NewDB(sqldb, sqlitedialect.New())
	t.Cleanup(func() { client.Close() })
	if err := db.MigrateDB(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return client
}

func seedStatusData(ctx context.Context, t *testing.T, client *bun.DB) {
	t.Helper()
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "ownerkey", "alice", db.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "normalkey", "bob", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := db.UpsertStatusRollup(ctx, client, "status_daily", []db.StatusDelta{
		{TS: today, APIKeyID: 1, Kind: stats.KindUpload, ContentClass: stats.ClassVideo, BytesIn: 900, BytesOut: 800, Files: 2},
		{TS: today, Kind: stats.KindFileGet, ContentClass: stats.ClassImage, BytesOut: 500, Requests: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileStats(ctx, client, []db.FileStatFlush{
		{FileName: "bigvideo", Requests: 4, BytesOut: 4000, LastServed: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusHandlerAuth(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	seedStatusData(ctx, t, client)
	h := StatusHandler(client)

	t.Run("post valid owner key sets cookie and redirects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader("key=ownerkey"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("code = %d, want 303", rec.Code)
		}
		var cookie *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == statusAuthCookie {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no status_auth cookie set")
		}
		if !cookie.HttpOnly || cookie.Path != "/status" || cookie.MaxAge != statusAuthMaxAge ||
			cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
			t.Fatalf("cookie flags wrong: %+v", cookie)
		}
	})

	t.Run("post invalid key rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader("key=nope"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Invalid API key") {
			t.Fatalf("error message not rendered: %q", rec.Body.String())
		}
	})

	t.Run("post non-owner key rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader("key=normalkey"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("json without key unauthorized", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/status?format=json", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
		var errPayload map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &errPayload); err != nil {
			t.Fatalf("body not json: %q", rec.Body.String())
		}
		if errPayload["error"] != "unauthorized" {
			t.Fatalf("error payload: %v", errPayload)
		}
	})

	t.Run("json with non-owner key unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status?format=json", nil)
		req.Header.Set("X-API-Key", "normalkey")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("json export shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status?format=json", nil)
		req.Header.Set("X-API-Key", "ownerkey")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}

		var payload struct {
			Uptime       string `json:"uptime"`
			ServingSince string `json:"serving_since"`
			Transferred  int64  `json:"transferred_bytes"`
			Days         []struct {
				Day          string `json:"day"`
				ServedBytes  int64  `json:"served_bytes"`
				ReceivedByte int64  `json:"received_bytes"`
				Uploads      int64  `json:"uploads"`
			} `json:"days_30"`
			Kinds []struct {
				Name     string `json:"name"`
				BytesIn  int64  `json:"bytes_in"`
				BytesOut int64  `json:"bytes_out"`
				Files    int64  `json:"files"`
			} `json:"kinds_30"`
			Keys []struct {
				Owner    string `json:"owner"`
				Uploads  int64  `json:"uploads"`
				Received int64  `json:"received_bytes"`
				Stored   int64  `json:"stored_bytes"`
			} `json:"keys_30"`
			TopFiles []struct {
				Name     string `json:"file"`
				Requests int64  `json:"requests"`
				BytesOut int64  `json:"bytes_out"`
			} `json:"top_files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("bad json: %v", err)
		}

		if payload.Uptime == "" || payload.ServingSince != "—" {
			t.Fatalf("live state wrong: uptime=%q serving_since=%q", payload.Uptime, payload.ServingSince)
		}
		if len(payload.Days) != 30 {
			t.Fatalf("days_30 = %d bars, want 30", len(payload.Days))
		}
		today := time.Now().UTC().Truncate(24 * time.Hour).Format(time.DateOnly)
		for _, d := range payload.Days {
			if d.Day != today {
				continue
			}
			if d.ReceivedByte != 900 || d.Uploads != 2 || d.ServedBytes != 500 {
				t.Fatalf("today bar wrong: %+v", d)
			}
		}
		var uploadKind, fileGetKind bool
		for _, k := range payload.Kinds {
			switch k.Name {
			case stats.KindUpload:
				uploadKind = k.BytesIn == 900 && k.Files == 2
			case stats.KindFileGet:
				fileGetKind = k.BytesOut == 500
			}
		}
		if !uploadKind || !fileGetKind {
			t.Fatalf("kinds_30 wrong: %+v", payload.Kinds)
		}
		if len(payload.Keys) != 1 || payload.Keys[0].Owner != "alice" ||
			payload.Keys[0].Uploads != 2 || payload.Keys[0].Received != 900 || payload.Keys[0].Stored != 800 {
			t.Fatalf("keys_30 wrong: %+v", payload.Keys)
		}
		if len(payload.TopFiles) != 1 || payload.TopFiles[0].Name != "bigvideo" ||
			payload.TopFiles[0].Requests != 4 || payload.TopFiles[0].BytesOut != 4000 {
			t.Fatalf("top_files wrong: %+v", payload.TopFiles)
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if got := string(raw["preview_classes_30"]); got != "[]" {
			t.Errorf("preview_classes_30 = %s, want []", got)
		}
		if got := string(raw["file_classes_30"]); !strings.Contains(got, `"name":"image"`) ||
			!strings.Contains(got, `"bytes_out":500`) {
			t.Errorf("file_classes_30 wrong: %s", got)
		}
	})

	t.Run("dashboard html renders for owner cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.Header.Set("Cookie", statusAuthCookie+"=ownerkey")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content type = %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "json export") {
			t.Fatal("dashboard template not rendered")
		}
	})

	t.Run("dashboard html without key asks for one", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "password") {
			t.Fatal("key form not rendered")
		}
	})
}

func TestRootHandler(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k", "owner", db.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	firstUpload := time.Now().AddDate(0, 0, -3).UTC().Truncate(24 * time.Hour)
	file := &db.UploadedFile{
		Filename: "x.bin", SavedPath: "/u/x", FileSize: 1500, FileHash: "h",
		UploadedAt: firstUpload, DeletionToken: "tok", APIKeyID: 1,
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/?auto=1", nil)
	rec := httptest.NewRecorder()
	RootHandler(client)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "1.5 KB") {
		t.Errorf("storage size not rendered humanized: %q", body)
	}
	if !strings.Contains(body, firstUpload.Format(time.DateOnly)) {
		t.Errorf("serving-since date not rendered: %q", body)
	}
}

func TestEstimateServedBytes(t *testing.T) {
	const size = int64(1000)
	cases := []struct {
		name   string
		method string
		rng    string
		want   int64
	}{
		{"whole file", http.MethodGet, "", size},
		{"head delivers nothing", http.MethodHead, "", 0},
		{"open ended", http.MethodGet, "bytes=0-", size},
		{"from offset", http.MethodGet, "bytes=100-", size - 100},
		{"exact span", http.MethodGet, "bytes=100-199", 100},
		{"suffix span", http.MethodGet, "bytes=-200", 200},
		{"suffix larger than file", http.MethodGet, "bytes=-5000", size},
		{"start at eof", http.MethodGet, "bytes=1000-", 0},
		{"end clamped to eof", http.MethodGet, "bytes=999-9999", 1},
		{"multipart not estimated", http.MethodGet, "bytes=0-100,200-300", 0},
		{"garbage falls back to whole file", http.MethodGet, "bytes=abc", size},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/f", nil)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			if got := estimateServedBytes(req, size); got != tc.want {
				t.Errorf("estimateServedBytes(%q) = %d, want %d", tc.rng, got, tc.want)
			}
		})
	}
}
