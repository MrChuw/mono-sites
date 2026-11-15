package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

func TestDeleteFileHandler(t *testing.T) {
	ctx := context.Background()
	client := setupService(t)
	if _, err := db.CreateAndInsertAPIKey(ctx, client, "k1", "alice", db.RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	token := strings.Repeat("a", 64)
	content := "delete me please"
	savedPath := filepath.Join(config.UploadDir, "gone.bin")
	if err := os.WriteFile(savedPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	file := &db.UploadedFile{
		ID: "fid-del", Filename: "gone.bin", SavedPath: savedPath,
		FileSize: int64(len(content)), FileHash: "hash-del",
		UploadedAt: time.Now().UTC().Add(-time.Hour), DeletionToken: token,
		IPAddress: "9.9.9.9", Country: "XX", APIKeyID: 1,
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/delete/{token}", DeleteFileHandler(client))
	do := func(token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delete/"+token, nil))
		return rec
	}

	t.Run("malformed token rejected", func(t *testing.T) {
		if rec := do("short"); rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown token not found", func(t *testing.T) {
		if rec := do(strings.Repeat("b", 64)); rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", rec.Code)
		}
	})

	t.Run("valid token deletes and trashes", func(t *testing.T) {
		deletedBefore := stats.FilesDeleted()
		rec := do(token)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
		}
		var res map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("body not json: %q", rec.Body.String())
		}
		if res["status"] != "success" {
			t.Fatalf("payload: %v", res)
		}

		if _, err := os.Stat(savedPath); !os.IsNotExist(err) {
			t.Fatalf("source still exists: %v", err)
		}
		entries, err := os.ReadDir(config.TrashDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("trash entries = %d err=%v, want 1", len(entries), err)
		}

		if _, err := db.GetFileByID(ctx, client, "fid-del"); err == nil {
			t.Fatal("uploaded_files row survived deletion")
		}
		logs, err := db.GetDeletedLogsByFilters(ctx, client, map[string]any{})
		if err != nil || len(logs) != 1 {
			t.Fatalf("logs = %d err=%v", len(logs), err)
		}
		if logs[0].OriginalID != "fid-del" || logs[0].TrashPath == "" {
			t.Fatalf("log wrong: %+v", logs[0])
		}
		if got := stats.FilesDeleted() - deletedBefore; got != 1 {
			t.Fatalf("files deleted delta = %d, want 1", got)
		}
	})
}
