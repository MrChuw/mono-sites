package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
)

func injectKey(r *http.Request, key *db.APIKey) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey, *key))
}

func postJSON(t *testing.T, target string, payload any) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestCreateKeyHandler(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "ownerkey", "alice", db.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "normalkey", "bob", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	ownerKey, _ := db.GetAPIKeyByKey(ctx, client, "ownerkey")
	normalKey, _ := db.GetAPIKeyByKey(ctx, client, "normalkey")

	t.Run("non-owner forbidden", func(t *testing.T) {
		req := postJSON(t, "/api/keys", map[string]any{"owner": "carol", "role": "vip"})
		rec := httptest.NewRecorder()
		CreateKeyHandler(client)(rec, injectKey(req, normalKey))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader("{nope"))
		rec := httptest.NewRecorder()
		CreateKeyHandler(client)(rec, injectKey(req, ownerKey))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", rec.Code)
		}
	})

	t.Run("owner required", func(t *testing.T) {
		req := postJSON(t, "/api/keys", map[string]any{"role": "vip"})
		rec := httptest.NewRecorder()
		CreateKeyHandler(client)(rec, injectKey(req, ownerKey))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", rec.Code)
		}
	})

	t.Run("creates vip key with size cap", func(t *testing.T) {
		req := postJSON(t, "/api/keys", map[string]any{
			"owner": "carol", "role": "vip", "max_size_mb": 5.0,
		})
		rec := httptest.NewRecorder()
		CreateKeyHandler(client)(rec, injectKey(req, ownerKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
		}
		var res map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["owner"] != "carol" || res["role"] != "vip" {
			t.Fatalf("response: %v", res)
		}
		if !strings.HasPrefix(res["key"], "sk_") || len(res["key"]) != 3+48 {
			t.Fatalf("key shape wrong: %q (len %d)", res["key"], len(res["key"]))
		}

		created, err := db.GetAPIKeyByKey(ctx, client, res["key"])
		if err != nil {
			t.Fatal(err)
		}
		if created.Role != db.RoleVIP {
			t.Fatalf("role = %q", created.Role)
		}
		if created.MaxUploadSize == nil || *created.MaxUploadSize != 5*1024*1024 {
			t.Fatalf("max size = %v", created.MaxUploadSize)
		}
	})

	t.Run("unknown role falls back to normal", func(t *testing.T) {
		req := postJSON(t, "/api/keys", map[string]any{"owner": "dave", "role": "hacker"})
		rec := httptest.NewRecorder()
		CreateKeyHandler(client)(rec, injectKey(req, ownerKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		created, err := db.GetAPIKeyByKey(ctx, client, decodeResponse(t, rec)["key"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if created.Role != db.RoleNormal {
			t.Fatalf("role = %q, want normal", created.Role)
		}
	})
}

func TestSharexConfigHandler(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "sk_sharex", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	key, _ := db.GetAPIKeyByKey(ctx, client, "sk_sharex")

	req := httptest.NewRequest(http.MethodGet, "http://im.example.com/sharex/config", nil)
	rec := httptest.NewRecorder()
	SharexConfigHandler(client)(rec, injectKey(req, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("body not json: %q", rec.Body.String())
	}
	if cfg["Name"] != "Local File Server (alice)" {
		t.Fatalf("name = %v", cfg["Name"])
	}
	if cfg["RequestURL"] != "http://i.example.com/api/upload" {
		t.Fatalf("request url = %v", cfg["RequestURL"])
	}
	headers := cfg["Headers"].(map[string]any)
	if headers["X-API-Key"] != "sk_sharex" {
		t.Fatalf("headers = %v", headers)
	}
	if cfg["FileFormName"] != "file" || cfg["URL"] != "{json:url}" {
		t.Fatalf("sharex wiring: %v", cfg)
	}
}

func seedOwnerData(ctx context.Context, t *testing.T, client *bun.DB) {
	t.Helper()
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "sk_alice_owner", "alice", db.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "sk_alice_norm", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "sk_bob_norm", "bob", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	first := time.Now().UTC().Add(-2 * time.Hour)
	last := time.Now().UTC().Add(-time.Hour)
	files := []db.UploadedFile{
		{ID: "a1", Filename: "a1", SavedPath: "/a1", FileSize: 100, FileHash: "h1", UploadedAt: first, DeletionToken: "t1", APIKeyID: 1},
		{ID: "a2", Filename: "a2", SavedPath: "/a2", FileSize: 300, FileHash: "h2", UploadedAt: last, DeletionToken: "t2", APIKeyID: 1},
	}
	for i := range files {
		if _, err := client.NewInsert().Model(&files[i]).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := client.Exec(`INSERT INTO deleted_file_logs
		(original_id, filename, file_size, file_hash, uploaded_at, deleted_at, purge_at, trash_path, meta_json, processed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"gone", "gone.bin", 50, "h9", first, time.Now().UTC(), time.Now().UTC().Add(time.Hour), "", `{"owner":"alice"}`, false,
	); err != nil {
		t.Fatal(err)
	}
}

func TestGetUserMetricsHandler(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	seedOwnerData(ctx, t, client)

	key, err := db.GetAPIKeyByKey(ctx, client, "sk_alice_norm")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unauthorized without key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		GetUserMetricsHandler(client)(rec, httptest.NewRequest(http.MethodGet, "/api/metrics/user", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("summary aggregates alice's keys", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://i.example.com/api/metrics/user", nil)
		rec := httptest.NewRecorder()
		GetUserMetricsHandler(client)(rec, injectKey(req, key))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
		}

		var payload struct {
			Owner   string `json:"owner"`
			Summary struct {
				TotalUploads        int64   `json:"total_uploads"`
				ActiveFiles         int64   `json:"active_files"`
				DeletedFiles        int64   `json:"deleted_files"`
				CurrentBytesUsed    int64   `json:"current_bytes_used"`
				HistoricalBytesSent int64   `json:"historical_bytes_sent"`
				AverageFileSize     float64 `json:"average_file_size"`
				FirstUpload         *string `json:"first_upload"`
				LastUpload          *string `json:"last_upload"`
			} `json:"summary"`
			Breakdown []struct {
				KeyPrefix   string `json:"key_prefix"`
				Role        string `json:"role"`
				ActiveFiles int64  `json:"active_files"`
				BytesUsed   int64  `json:"bytes_used"`
			} `json:"api_keys_breakdown"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}

		s := payload.Summary
		if payload.Owner != "alice" {
			t.Fatalf("owner = %q", payload.Owner)
		}
		if s.ActiveFiles != 2 || s.CurrentBytesUsed != 400 || s.AverageFileSize != 200 {
			t.Fatalf("active metrics wrong: %+v", s)
		}
		if s.TotalUploads != 3 || s.DeletedFiles != 1 || s.HistoricalBytesSent != 450 {
			t.Fatalf("historical metrics wrong: %+v", s)
		}
		if s.FirstUpload == nil || s.LastUpload == nil {
			t.Fatalf("first/last missing: %+v", s)
		}
		gotFirst, err := time.Parse(time.RFC3339, *s.FirstUpload)
		if err != nil || gotFirst.Sub(time.Now().UTC().Add(-2*time.Hour)).Abs() > time.Minute {
			t.Fatalf("first_upload = %v (%v)", *s.FirstUpload, err)
		}
		if len(payload.Breakdown) != 2 {
			t.Fatalf("breakdown = %d entries, want 2", len(payload.Breakdown))
		}
		if payload.Breakdown[0].Role != "owner" || payload.Breakdown[0].ActiveFiles != 2 || payload.Breakdown[0].BytesUsed != 400 {
			t.Fatalf("breakdown[0]: %+v", payload.Breakdown[0])
		}
	})

	t.Run("owner without files reports zeros", func(t *testing.T) {
		ghost := &db.APIKey{ID: 999, Owner: "ghost", Role: db.RoleNormal}
		req := httptest.NewRequest(http.MethodGet, "http://i.example.com/api/metrics/user", nil)
		rec := httptest.NewRecorder()
		GetUserMetricsHandler(client)(rec, injectKey(req, ghost))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		var payload struct {
			Summary struct {
				TotalUploads  int64   `json:"total_uploads"`
				ActiveFiles   int64   `json:"active_files"`
				FirstUpload   *string `json:"first_upload"`
				LastUpload    *string `json:"last_upload"`
				AverageFileSz float64 `json:"average_file_size"`
			} `json:"summary"`
			Breakdown []any `json:"api_keys_breakdown"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Summary.ActiveFiles != 0 || payload.Summary.TotalUploads != 0 ||
			payload.Summary.FirstUpload != nil || payload.Summary.LastUpload != nil {
			t.Fatalf("zero summary wrong: %+v", payload.Summary)
		}
		if len(payload.Breakdown) != 0 {
			t.Fatalf("breakdown = %v, want []", payload.Breakdown)
		}
	})
}

func TestGetAdminMetricsHandler(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	seedOwnerData(ctx, t, client)

	ownerKey, _ := db.GetAPIKeyByKey(ctx, client, "sk_alice_owner")
	normalKey, _ := db.GetAPIKeyByKey(ctx, client, "sk_bob_norm")

	t.Run("non-owner forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/metrics/admin", nil)
		rec := httptest.NewRecorder()
		GetAdminMetricsHandler(client)(rec, injectKey(req, normalKey))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
	})

	t.Run("owner sees globals", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://i.example.com/api/metrics/admin", nil)
		rec := httptest.NewRecorder()
		GetAdminMetricsHandler(client)(rec, injectKey(req, ownerKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
		}
		var payload struct {
			GeneralUse struct {
				TotalActiveFiles       int64 `json:"total_active_files"`
				TotalHistoricalUploads int64 `json:"total_historical_uploads"`
				TotalHistoricalDels    int64 `json:"total_historical_deletions"`
				CurrentOccupiedBytes   int64 `json:"current_occupied_bytes"`
			} `json:"general_use"`
			Health map[string]string `json:"server_health_and_storage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		g := payload.GeneralUse
		if g.TotalActiveFiles != 2 || g.CurrentOccupiedBytes != 400 {
			t.Fatalf("active totals wrong: %+v", g)
		}
		if g.TotalHistoricalUploads != 3 || g.TotalHistoricalDels != 1 {
			t.Fatalf("historical totals wrong: %+v", g)
		}
		if payload.Health["database"] != "ok" || payload.Health["storage"] != "ok" {
			t.Fatalf("health: %v", payload.Health)
		}
	})
}
