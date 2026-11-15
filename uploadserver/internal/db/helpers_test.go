package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

func TestSaveFileWithTagsAndLookups(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if _, err := CreateAndInsertAPIKey(ctx, client, "k1", "alice", RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	uploaded := time.Now().UTC().Add(-time.Hour)
	file := &UploadedFile{
		Filename: "x.bin", SavedPath: "/u/x.bin", FileSize: 100, FileHash: "hash1",
		UploadedAt: uploaded, DeletionToken: "tok1", APIKeyID: 1,
	}
	if err := SaveFileWithTags(ctx, client, file, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if file.ID == "" {
		t.Fatal("SaveFileWithTags left the file without an ID")
	}

	byID, err := GetFileByID(ctx, client, file.ID)
	if err != nil || byID.Filename != "x.bin" || byID.APIKeyID != 1 {
		t.Fatalf("GetFileByID: %+v err=%v", byID, err)
	}
	byHash, err := GetFileByHash(ctx, client, "hash1")
	if err != nil || byHash.ID != file.ID {
		t.Fatalf("GetFileByHash: %+v err=%v", byHash, err)
	}
	if _, err := GetFileByHash(ctx, client, "missing"); err == nil {
		t.Fatal("GetFileByHash accepted an unknown hash")
	}
	byToken, err := GetFileByDeletionToken(ctx, client, "tok1")
	if err != nil || byToken.ID != file.ID {
		t.Fatalf("GetFileByDeletionToken: %+v err=%v", byToken, err)
	}
	withKey, err := GetUploadedFileByID(ctx, client, file.ID)
	if err != nil || withKey.APIKey == nil || withKey.APIKey.Owner != "alice" {
		t.Fatalf("GetUploadedFileByID relation: %+v err=%v", withKey, err)
	}
	byTokenRel, err := GetUploadedFileByToken(ctx, client, "tok1")
	if err != nil || byTokenRel.APIKey == nil {
		t.Fatalf("GetUploadedFileByToken relation: %+v err=%v", byTokenRel, err)
	}

	files, err := GetFilesByAPIKey(ctx, client, 1, 10, 0)
	if err != nil || len(files) != 1 {
		t.Fatalf("GetFilesByAPIKey: %d files err=%v", len(files), err)
	}

	var tagNames []string
	if err := client.NewSelect().
		Model((*Tag)(nil)).
		Join("JOIN file_tags AS ft ON ft.tag_id = t.id").
		ColumnExpr("t.name").
		Where("ft.uploaded_file_id = ?", file.ID).
		Scan(ctx, &tagNames); err != nil {
		t.Fatal(err)
	}
	if len(tagNames) != 2 {
		t.Fatalf("tag count = %d, want 2: %v", len(tagNames), tagNames)
	}
}

func TestGetOrCreateTagsDeduplicates(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	first, err := GetOrCreateTags(ctx, client, []string{"Foo Bar!", "foobar", "ok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("got %d tags, want 3: %+v", len(first), first)
	}
	if first[0].ID != first[1].ID || first[0].Name != "foobar" || first[2].Name != "ok-1" {
		t.Fatalf("tag cleaning/dedup wrong: %+v", first)
	}
	for _, tag := range first {
		if tag.CreatedAt.IsZero() {
			t.Fatalf("fresh tag returned with zero CreatedAt: %+v", tag)
		}
	}

	again, err := GetOrCreateTags(ctx, client, []string{"foobar", "ok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].ID != first[0].ID {
		t.Fatalf("re-create made new rows: %+v vs %+v", again, first)
	}
	if n := countRows(ctx, t, client, (*Tag)(nil)); n != 2 {
		t.Fatalf("tags table = %d rows, want 2", n)
	}
}

func TestAssociateTagsIgnoresDuplicates(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	tags, err := GetOrCreateTags(ctx, client, []string{"dup"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := AssociateTags(ctx, client, "file1", tags); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(ctx, t, client, (*FileTag)(nil)); n != 1 {
		t.Fatalf("file_tags = %d rows, want 1", n)
	}
}

func TestAPIKeyLookups(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	created, err := CreateAndInsertAPIKey(ctx, client, "sk_test", "alice", RoleOwner, nil)
	if err != nil {
		t.Fatal(err)
	}
	found, err := GetAPIKeyByKey(ctx, client, "sk_test")
	if err != nil || found.ID != created.ID || found.Role != RoleOwner {
		t.Fatalf("GetAPIKeyByKey: %+v err=%v", found, err)
	}
	if _, err := GetAPIKeyByKey(ctx, client, "sk_unknown"); err != sql.ErrNoRows {
		t.Fatalf("unknown key err = %v, want sql.ErrNoRows", err)
	}

	if _, err := CreateAndInsertAPIKey(ctx, client, "sk_other", "alice", RoleVIP, nil); err != nil {
		t.Fatal(err)
	}
	keys, err := GetAPIKeysByOwner(ctx, client, "alice")
	if err != nil || len(keys) != 2 {
		t.Fatalf("GetAPIKeysByOwner: %d keys err=%v", len(keys), err)
	}
}

func TestDeletedFileLogLifecycle(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if _, err := CreateAndInsertAPIKey(ctx, client, "k1", "alice", RoleNormal, nil); err != nil {
		t.Fatal(err)
	}
	file := &UploadedFile{
		ID: "fid-1", Filename: "gone.bin", SavedPath: "/u/gone.bin", FileSize: 77,
		FileHash: "h", UploadedAt: time.Now().UTC().Add(-time.Hour),
		DeletionToken: "tok", IPAddress: "1.2.3.4", Country: "BR", APIKeyID: 1,
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := InsertDeletedFileLog(ctx, client, file, "trash/gone_x.bin"); err != nil {
		t.Fatal(err)
	}
	logs, err := GetDeletedLogsByFilters(ctx, client, map[string]any{})
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %d err=%v", len(logs), err)
	}
	log := logs[0]
	if log.OriginalID != "fid-1" || log.FileSize != 77 || log.TrashPath != "trash/gone_x.bin" {
		t.Fatalf("log fields wrong: %+v", log)
	}
	if log.PurgeAt.Before(time.Now()) {
		t.Fatalf("purge_at in the past: %v", log.PurgeAt)
	}
	var wrapper string
	if err := json.Unmarshal([]byte(log.MetaJSON), &wrapper); err != nil {
		t.Fatalf("meta json not a JSON string: %q (%v)", log.MetaJSON, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(wrapper), &meta); err != nil {
		t.Fatalf("inner meta not JSON: %q (%v)", wrapper, err)
	}
	if meta["ip_address"] != "1.2.3.4" || meta["country"] != "BR" {
		t.Fatalf("meta content wrong: %v", meta)
	}

	purgable, err := GetUnprocessedExpiredLogs(ctx, client, time.Now())
	if err != nil || len(purgable) != 0 {
		t.Fatalf("fresh log purgable: %d err=%v", len(purgable), err)
	}

	if _, err := client.NewUpdate().
		Model((*DeletedFileLog)(nil)).
		Set("purge_at = ?", time.Now().Add(-time.Minute)).
		Where("id = ?", log.ID).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	purgable, err = GetUnprocessedExpiredLogs(ctx, client, time.Now())
	if err != nil || len(purgable) != 1 {
		t.Fatalf("expired log not purgable: %d err=%v", len(purgable), err)
	}
	if err := MarkDeletedLogAsProcessed(ctx, client, log.ID); err != nil {
		t.Fatal(err)
	}
	purgable, err = GetUnprocessedExpiredLogs(ctx, client, time.Now())
	if err != nil || len(purgable) != 0 {
		t.Fatalf("processed log still purgable: %d err=%v", len(purgable), err)
	}
}

func TestGetExpiredUploadedFiles(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	for i, expires := range []*time.Time{&past, &future, nil} {
		file := &UploadedFile{
			Filename: "f.bin", SavedPath: "/u/f.bin", FileSize: 1, FileHash: "h",
			UploadedAt: time.Now().UTC(), DeletionToken: "tok", APIKeyID: 0,
		}
		file.ExpiresAt = expires
		if i > 0 {
			file.DeletionToken = "tok" + string(rune('0'+i))
		}
		if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	expired, err := GetExpiredUploadedFiles(ctx, client, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ExpiresAt == nil ||
		expired[0].ExpiresAt.Sub(past).Abs() >= time.Microsecond {
		t.Fatalf("expired = %d rows, want exactly the past-TTL one: %+v", len(expired), expired)
	}
}

func TestCountOtherFilesSharingPath(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	mk := func(id, token string) {
		file := &UploadedFile{
			ID: id, Filename: "f.bin", SavedPath: "/u/f.bin", FileSize: 1, FileHash: "h",
			UploadedAt: time.Now().UTC(), DeletionToken: token,
		}
		if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mk("id1", "t1")
	mk("id2", "t2")
	mk2 := &UploadedFile{
		ID: "id3", Filename: "f.bin", SavedPath: "/u/other.bin", FileSize: 1, FileHash: "h",
		UploadedAt: time.Now().UTC(), DeletionToken: "t3",
	}
	if _, err := client.NewInsert().Model(mk2).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if n, err := CountOtherFilesSharingPath(ctx, client, "/u/f.bin", "id1"); err != nil || n != 1 {
		t.Fatalf("shared count = %d err=%v, want 1", n, err)
	}
	if n, err := CountOtherFilesSharingPath(ctx, client, "/u/f.bin", "id2"); err != nil || n != 1 {
		t.Fatalf("shared count = %d err=%v, want 1", n, err)
	}
	if n, err := CountOtherFilesSharingPath(ctx, client, "/u/other.bin", "id3"); err != nil || n != 0 {
		t.Fatalf("unshared count = %d err=%v, want 0", n, err)
	}
}

func TestUpdateFilePreview(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	file := &UploadedFile{
		ID: "id1", Filename: "f.png", SavedPath: "/u/f.png", FileSize: 1, FileHash: "h",
		UploadedAt: time.Now().UTC(), DeletionToken: "t1",
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := UpdateFilePreview(ctx, client, "id1", "processing", "", nil); err != nil {
		t.Fatal(err)
	}
	row, _ := GetFileByID(ctx, client, "id1")
	if row.PreviewStatus != "processing" || row.PreviewError != "" {
		t.Fatalf("status update wrong: %+v", row)
	}

	if err := UpdateFilePreview(ctx, client, "id1", "done", "", &PreviewFields{
		ThumbnailPath: "/p/t.png", GifPath: "/p/g.gif", WebpPath: "/p/w.webp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFilePreview(ctx, client, "id1", "error", "boom", &PreviewFields{}); err != nil {
		t.Fatal(err)
	}
	row, _ = GetFileByID(ctx, client, "id1")
	if row.PreviewStatus != "error" || row.PreviewError != "boom" {
		t.Fatalf("error report wrong: %+v", row)
	}
	if row.ThumbnailPath != "/p/t.png" || row.PreviewGifPath != "/p/g.gif" || row.PreveiwWebpPath != "/p/w.webp" {
		t.Fatalf("paths were wiped by an empty report: %+v", row)
	}
}

func TestGlobalAndUserMetrics(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if _, err := CreateAndInsertAPIKey(ctx, client, "k1", "alice", RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAndInsertAPIKey(ctx, client, "k2", "bob", RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	files := []UploadedFile{
		{ID: "a1", Filename: "a1", SavedPath: "/a1", FileSize: 100, FileHash: "h1", UploadedAt: now.Add(-2 * time.Hour), DeletionToken: "t1", APIKeyID: 1},
		{ID: "a2", Filename: "a2", SavedPath: "/a2", FileSize: 300, FileHash: "h2", UploadedAt: now.Add(-1 * time.Hour), DeletionToken: "t2", APIKeyID: 1},
		{ID: "b1", Filename: "b1", SavedPath: "/b1", FileSize: 50, FileHash: "h3", UploadedAt: now.Add(-3 * time.Hour), DeletionToken: "t3", APIKeyID: 2},
	}
	for i := range files {
		if _, err := client.NewInsert().Model(&files[i]).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	activeCount, activeSize, err := GetGlobalActiveMetrics(ctx, client)
	if err != nil || activeCount != 3 || activeSize != 450 {
		t.Fatalf("global metrics: count=%d size=%d err=%v", activeCount, activeSize, err)
	}
	if deleted, err := GetGlobalDeletedCount(ctx, client); err != nil || deleted != 0 {
		t.Fatalf("global deleted: %d err=%v", deleted, err)
	}

	count, size, avg, err := GetUserActiveMetrics(ctx, client, []uint{1}, nil)
	if err != nil || count != 2 || size != 400 || avg != 200 {
		t.Fatalf("alice metrics: count=%d size=%d avg=%f err=%v", count, size, avg, err)
	}
	count, size, _, err = GetUserActiveMetrics(ctx, client, []uint{1}, map[string]any{"file_size >= ?": int64(200)})
	if err != nil || count != 1 || size != 300 {
		t.Fatalf("filtered metrics: count=%d size=%d err=%v", count, size, err)
	}

	breakdown, err := GetAPIKeysBreakdown(ctx, client, []uint{1})
	if err != nil || len(breakdown) != 1 || breakdown[0].APIKeyID != 1 || breakdown[0].Count != 2 || breakdown[0].Size != 400 {
		t.Fatalf("breakdown: %+v err=%v", breakdown, err)
	}

	first, last, err := GetFirstAndLastUpload(ctx, client, []uint{1})
	if err != nil || first.ID != "a1" || last.ID != "a2" {
		t.Fatalf("first/last: %s %s err=%v", first.ID, last.ID, err)
	}

	firstEver, err := GetFirstUploadTime(ctx, client)
	if err != nil || firstEver == nil ||
		firstEver.Sub(now.Add(-3*time.Hour)).Abs() >= time.Microsecond {
		t.Fatalf("first upload time: %v err=%v", firstEver, err)
	}
}

func TestGetFirstUploadTimeEmpty(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	first, err := GetFirstUploadTime(ctx, client)
	if err != nil || first != nil {
		t.Fatalf("empty table: %v err=%v, want nil,nil", first, err)
	}
}
