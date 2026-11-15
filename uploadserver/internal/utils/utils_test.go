package utils

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barasher/go-exiftool"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	_ "modernc.org/sqlite"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

func TestMain(m *testing.M) {
	var err error
	ExifDaemon, err = exiftool.NewExiftool(exiftool.ClearFieldsBeforeWriting())
	if err != nil {
		fmt.Fprintln(os.Stderr, "exiftool init failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	ExifDaemon.Close()
	os.Exit(code)
}

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

func setupDirs(t *testing.T) {
	t.Helper()
	config.UploadDir = t.TempDir()
	config.TrashDir = t.TempDir()
	config.ThumbDir = t.TempDir()
	config.MaxNameLength = 64
	config.InitialTokenLength = 5
	config.ReservedNames = map[string]bool{"api": true, "static": true, "assets": true}
}

func seedFile(t *testing.T, ctx context.Context, client *bun.DB, dir, name, content string, expires *time.Time) *db.UploadedFile {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	file := &db.UploadedFile{
		ID: "fid-" + name, Filename: name, SavedPath: path,
		FileSize: int64(len(content)), FileHash: computeSHA256([]byte(content)),
		UploadedAt: time.Now().UTC().Add(-time.Hour), DeletionToken: "tok-" + name,
		ExpiresAt: expires,
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSecureRandomString(t *testing.T) {
	for _, length := range []int{1, 5, 64} {
		s := SecureRandomString(length)
		if len(s) != length {
			t.Fatalf("len = %d, want %d", len(s), length)
		}
		for _, c := range s {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				t.Fatalf("unexpected char %q in %q", c, s)
			}
		}
	}
	if SecureRandomString(32) == SecureRandomString(32) {
		t.Fatal("two draws produced the same string")
	}
}

func TestSanitizeSubfolderName(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		fails bool
	}{
		{in: "", want: ""},
		{in: "My Scope!", want: "myscope"},
		{in: "UP_per-Case", want: "up_per-case"},
		{in: "screenshots 2026", want: "screenshots2026"},
		{in: "!!!", fails: true},
		{in: "api", want: "_api"},
		{in: "api2", want: "api2"},
		{in: string(make([]byte, 100)), fails: true},
	}
	for _, tc := range cases {
		got, err := SanitizeSubfolderName(tc.in)
		if tc.fails {
			if err == nil {
				t.Errorf("SanitizeSubfolderName(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SanitizeSubfolderName(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SanitizeSubfolderName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	long := "a"
	for len(long) <= config.MaxNameLength {
		long += "a"
	}
	if _, err := SanitizeSubfolderName(long); err == nil {
		t.Error("over-long name accepted")
	}
}

func TestParseTimeRange(t *testing.T) {
	filters, err := ParseTimeRange("", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	cutoff, ok := filters["uploaded_at >= ?"].(time.Time)
	if !ok || time.Since(cutoff) > 8*24*time.Hour {
		t.Fatalf("days filter wrong: %v", filters)
	}

	filters, err = ParseTimeRange("2026-01-02", "2026-02-03", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != 2 {
		t.Fatalf("from/to filters: %v", filters)
	}
	if t0 := filters["uploaded_at >= ?"].(time.Time); t0.Format(time.DateOnly) != "2026-01-02" {
		t.Fatalf("from = %v", t0)
	}

	if _, err = ParseTimeRange("bogus", "", 0); err == nil {
		t.Fatal("bad from date accepted")
	}

	filters, err = ParseTimeRange("2026-01-02", "2026-02-03", 5)
	if err != nil || len(filters) != 1 {
		t.Fatalf("days should override explicit dates: %v err=%v", filters, err)
	}
}

func TestExecuteFileDeletion(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	file := seedFile(t, ctx, client, config.UploadDir, "f1.bin", "payload-1", nil)

	if err := ExecuteFileDeletion(ctx, client, file); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(file.SavedPath); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	entries, err := os.ReadDir(config.TrashDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash entries = %d err=%v, want 1", len(entries), err)
	}
	trashName := entries[0].Name()
	if want := filepath.Base(file.SavedPath); len(trashName) <= len(want) || trashName[len(trashName)-len(want):] != want {
		t.Fatalf("trash name %q does not end with the original base %q", trashName, want)
	}

	logs, err := db.GetDeletedLogsByFilters(ctx, client, map[string]any{})
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %d err=%v", len(logs), err)
	}
	if logs[0].OriginalID != file.ID || logs[0].FileSize != file.FileSize {
		t.Fatalf("log fields wrong: %+v", logs[0])
	}
	if logs[0].TrashPath == "" {
		t.Fatal("log lost the trash path")
	}
	if n, err := client.NewSelect().Model((*db.UploadedFile)(nil)).Count(ctx); err != nil || n != 0 {
		t.Fatalf("uploaded_files = %d err=%v, want 0", n, err)
	}
}

func TestExecuteFileDeletionSharedPath(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	content := "shared-bytes"
	seedFile(t, ctx, client, config.UploadDir, "share_a.bin", content, nil)
	shared := seedFile(t, ctx, client, config.UploadDir, "share_b.bin", content, nil)
	if _, err := client.NewUpdate().
		Model((*db.UploadedFile)(nil)).
		Set("saved_path = ?", filepath.Join(config.UploadDir, "share_a.bin")).
		Where("id = ?", shared.ID).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	shared.SavedPath = filepath.Join(config.UploadDir, "share_a.bin")

	if err := ExecuteFileDeletion(ctx, client, shared); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(config.TrashDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("trash entries = %d err=%v, want 0", len(entries), err)
	}
	logs, err := db.GetDeletedLogsByFilters(ctx, client, map[string]any{})
	if err != nil || len(logs) != 1 || logs[0].TrashPath != "" {
		t.Fatalf("log wrong: %+v err=%v", logs, err)
	}
}

func seedTrashLog(t *testing.T, ctx context.Context, client *bun.DB, content string) *db.DeletedFileLog {
	t.Helper()
	trashPath := filepath.Join(config.TrashDir, "trashed_"+SecureRandomString(6)+".bin")
	if err := os.WriteFile(trashPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	log := &db.DeletedFileLog{
		OriginalID: "fid-x", Filename: "x.bin", FileSize: int64(len(content)),
		FileHash: "h", UploadedAt: time.Now().UTC().Add(-24 * time.Hour),
		DeletedAt: time.Now().UTC().Add(-2 * time.Hour),
		PurgeAt:   time.Now().Add(-time.Minute), TrashPath: trashPath,
	}
	if _, err := client.NewInsert().Model(log).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestPurgeTrashOnStartup(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	freedBefore := stats.BytesFreed()
	log := seedTrashLog(t, ctx, client, "freed-bytes")
	noTrash := &db.DeletedFileLog{
		OriginalID: "fid-y", Filename: "y.bin", FileSize: 99, FileHash: "h",
		UploadedAt: time.Now().UTC(), DeletedAt: time.Now().UTC(),
		PurgeAt: time.Now().Add(-time.Minute), TrashPath: "",
	}
	if _, err := client.NewInsert().Model(noTrash).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := PurgeTrashOnStartup(ctx, client); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(log.TrashPath); !os.IsNotExist(err) {
		t.Fatalf("trash file survived: %v", err)
	}
	if stats.BytesFreed() != freedBefore+log.FileSize {
		t.Fatalf("bytes freed = %d, want +%d", stats.BytesFreed()-freedBefore, log.FileSize)
	}

	logs, err := db.GetDeletedLogsByFilters(ctx, client, map[string]any{})
	if err != nil || len(logs) != 2 {
		t.Fatalf("logs = %d err=%v", len(logs), err)
	}
	for _, l := range logs {
		if !l.Processed {
			t.Fatalf("log %d not marked processed", l.ID)
		}
	}
}

func TestRunPurge(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	deletedBefore := stats.FilesDeleted()
	past := time.Now().Add(-time.Hour)
	ttlFile := seedFile(t, ctx, client, config.UploadDir, "ttl.bin", "expiring", &past)

	freedBefore := stats.BytesFreed()
	log := seedTrashLog(t, ctx, client, "purge-me")

	future := &db.DeletedFileLog{
		OriginalID: "fid-z", Filename: "z.bin", FileSize: 10, FileHash: "h",
		UploadedAt: time.Now().UTC(), DeletedAt: time.Now().UTC(),
		PurgeAt: time.Now().Add(time.Hour), TrashPath: "",
	}
	if _, err := client.NewInsert().Model(future).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	runPurge(ctx, client)

	if _, err := os.Stat(ttlFile.SavedPath); !os.IsNotExist(err) {
		t.Fatalf("expired file still on disk: %v", err)
	}
	entries, err := os.ReadDir(config.TrashDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("trash entries = %d, want 1", len(entries))
	}
	if stats.FilesDeleted() != deletedBefore+1 {
		t.Fatalf("files deleted stat delta wrong")
	}

	if _, err := os.Stat(log.TrashPath); !os.IsNotExist(err) {
		t.Fatalf("seeded trash file survived: %v", err)
	}
	if stats.BytesFreed() != freedBefore+log.FileSize {
		t.Fatalf("bytes freed delta wrong")
	}

	var processed int
	if err := client.NewSelect().
		Model((*db.DeletedFileLog)(nil)).
		ColumnExpr("count(*)").
		Where("processed = ?", true).
		Scan(ctx, &processed); err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("processed logs = %d, want 1", processed)
	}
}
