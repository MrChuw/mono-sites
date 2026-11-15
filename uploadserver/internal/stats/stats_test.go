package stats

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"

	"uploadserver/internal/db"
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

func resetState() {
	txBytes.Store(0)
	rxBytes.Store(0)
	storedBytes.Store(0)
	upFiles.Store(0)
	delFiles.Store(0)
	freedTotal.Store(0)
	dedupHits.Store(0)
	dedupSaved.Store(0)
	ogHits.Store(0)
	authFails.Store(0)
	busyErrs.Store(0)
	previewFails.Store(0)
	previewQueueDepth.Store(0)
	previewDurationEMA.Store(0)

	mu.Lock()
	current = make(map[dimKey]*bucket)
	mu.Unlock()
	fileMu.Lock()
	fileCurrent = make(map[string]*fileServe)
	fileMu.Unlock()
}

func countRows(ctx context.Context, t *testing.T, client *bun.DB, model any) int {
	t.Helper()
	var n int
	if err := client.NewSelect().Model(model).ColumnExpr("count(*)").Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClassifyMIME(t *testing.T) {
	cases := map[string]string{
		"video/mp4":                   ClassVideo,
		"audio/flac":                  ClassAudio,
		"image/png":                   ClassImage,
		"application/zip":             ClassArchive,
		"application/x-7z-compressed": ClassArchive,
		"application/x-rar":           ClassArchive,
		"application/pdf":             ClassDoc,
		"text/plain":                  ClassDoc,
		"application/json":            ClassDoc,
		"application/msword":          ClassDoc,
		"application/octet-stream":    ClassOther,
		"":                            ClassOther,
	}
	for mime, want := range cases {
		if got := ClassifyMIME(mime); got != want {
			t.Errorf("ClassifyMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestClassifyPreviewExt(t *testing.T) {
	cases := map[string]string{
		".webp": ClassWEBP,
		".gif":  ClassGIF,
		".jpg":  ClassJPG,
		".jpeg": ClassJPG,
		".png":  ClassBytePlot,
		".txt":  ClassBytePlot,
	}
	for ext, want := range cases {
		if got := ClassifyPreviewExt(ext); got != want {
			t.Errorf("ClassifyPreviewExt(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestRecordAndFlush(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	resetState()

	RecordFileGet(ClassImage, 1000)
	RecordFileServed("movie", 1000)
	RecordPreviewGet(ClassWEBP, 500)
	RecordResolverOG(300)
	RecordRedirect()
	RecordNotFound()
	RecordAuthFailure()
	RecordUpload(7, ClassVideo, 900, 800)
	RecordDedup(123)
	RecordUploadReject(ClassVideo)
	RecordFileDeleted(ClassManual)
	RecordTrashPurge(456)
	RecordPreviewGen(ClassJPG, 200)
	RecordPreviewFail(ClassWEBP)
	RecordBusy()

	flushOnce(ctx, client)

	row, err := db.GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	want := db.ServiceStats{
		ID: 1, TransferredBytes: 1500, ReceivedBytes: 900, StoredBytes: 800,
		FilesUploaded: 1, FilesDeleted: 1, BytesFreed: 456,
		DedupHits: 1, DedupSavedBytes: 123,
		ResolverHits: 1, AuthFailures: 1, BusyErrors: 1, PreviewFailures: 1,
	}
	if row != want {
		t.Fatalf("service_stats mismatch:\n got %+v\nwant %+v", row, want)
	}

	if n := countRows(ctx, t, client, (*db.StatusDelta)(nil)); n != 14 {
		t.Fatalf("deltas = %d, want 14", n)
	}
	if n := countRows(ctx, t, client, (*db.StatusHourly)(nil)); n != 14 {
		t.Fatalf("hourly = %d, want 14", n)
	}
	if n := countRows(ctx, t, client, (*db.StatusDaily)(nil)); n != 14 {
		t.Fatalf("daily = %d, want 14", n)
	}

	var uploads []db.StatusDaily
	if err := client.NewSelect().Model(&uploads).
		Where("kind = ?", KindUpload).Scan(ctx, &uploads); err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 || uploads[0].APIKeyID != 7 || uploads[0].ContentClass != ClassVideo ||
		uploads[0].BytesIn != 900 || uploads[0].BytesOut != 800 || uploads[0].Files != 1 {
		t.Fatalf("upload rollup wrong: %+v", uploads)
	}

	files, err := db.GetTopFiles(ctx, client, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FileName != "movie" ||
		files[0].Requests != 1 || files[0].BytesOut != 1000 || files[0].LastServed.IsZero() {
		t.Fatalf("file_stats wrong: %+v", files)
	}

	flushOnce(ctx, client)
	if n := countRows(ctx, t, client, (*db.StatusDelta)(nil)); n != 14 {
		t.Fatalf("deltas after empty flush = %d, want 14", n)
	}

	RecordFileGet(ClassVideo, 50)
	flushOnce(ctx, client)
	row, err = db.GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if row.TransferredBytes != 1550 {
		t.Fatalf("transferred after second flush = %d, want 1550", row.TransferredBytes)
	}
	if n := countRows(ctx, t, client, (*db.StatusDelta)(nil)); n != 15 {
		t.Fatalf("deltas after second flush = %d, want 15", n)
	}
}

func TestSeedRestoresCounters(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	resetState()

	if err := db.SetServiceStats(ctx, client, &db.ServiceStats{
		ID: 1, TransferredBytes: 77, FilesUploaded: 3, DedupSavedBytes: 9,
	}); err != nil {
		t.Fatal(err)
	}
	seed(ctx, client)
	if Transferred() != 77 || FilesUploaded() != 3 || DedupSavedBytes() != 9 {
		t.Fatalf("seed did not restore counters: tx=%d up=%d dedup=%d",
			Transferred(), FilesUploaded(), DedupSavedBytes())
	}

	flushOnce(ctx, client)
	row, err := db.GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if row.TransferredBytes != 77 || row.FilesUploaded != 3 || row.DedupSavedBytes != 9 {
		t.Fatalf("flush drifted the seeded row: %+v", row)
	}
}

func TestBackfill(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	resetState()

	if err := db.SetServiceStats(ctx, client, &db.ServiceStats{ID: 1, TransferredBytes: 12345}); err != nil {
		t.Fatal(err)
	}
	seed(ctx, client)

	cutoff := time.Now()
	day := func(offset int) time.Time {
		return cutoff.AddDate(0, 0, offset).UTC().Truncate(24 * time.Hour)
	}

	files := []db.UploadedFile{
		{Filename: "a1.bin", SavedPath: "/x/a1", FileSize: 1000, FileHash: "dup", UploadedAt: day(-10), DeletionToken: "t1", APIKeyID: 1},
		{Filename: "a2.bin", SavedPath: "/x/a2", FileSize: 1000, FileHash: "dup", UploadedAt: day(-9), DeletionToken: "t2", APIKeyID: 2},
		{Filename: "b.bin", SavedPath: "/x/b", FileSize: 500, FileHash: "solo", UploadedAt: day(-9), DeletionToken: "t3", APIKeyID: 1},
		{Filename: "post.bin", SavedPath: "/x/post", FileSize: 9999, FileHash: "post", UploadedAt: cutoff.Add(time.Hour), DeletionToken: "t4", APIKeyID: 1},
	}
	for i := range files {
		if _, err := client.NewInsert().Model(&files[i]).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	logs := []db.DeletedFileLog{
		{OriginalID: "o1", Filename: "l1.bin", FileSize: 300, FileHash: "h1", UploadedAt: day(-10), DeletedAt: day(-8).Add(2 * time.Hour), PurgeAt: day(-8).Add(3 * time.Hour), TrashPath: "trash/l1"},
		{OriginalID: "o2", Filename: "l2.bin", FileSize: 200, FileHash: "h2", UploadedAt: day(-7), DeletedAt: day(-7).Add(2 * time.Hour), PurgeAt: day(-7).Add(3 * time.Hour)},
	}
	for i := range logs {
		if _, err := client.NewInsert().Model(&logs[i]).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	RecordFileGet(ClassImage, 100)
	RecordUpload(3, ClassVideo, 700, 600)

	runBackfill(ctx, client, cutoff)

	for name, tc := range map[string]struct {
		got, want int64
	}{
		"txBytes":     {txBytes.Load(), 12445},
		"rxBytes":     {rxBytes.Load(), 3700},
		"storedBytes": {storedBytes.Load(), 3600},
		"upFiles":     {upFiles.Load(), 6},
		"delFiles":    {delFiles.Load(), 2},
		"freedTotal":  {freedTotal.Load(), 300},
		"dedupHits":   {dedupHits.Load(), 1},
		"dedupSaved":  {dedupSaved.Load(), 1000},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", name, tc.got, tc.want)
		}
	}

	row, err := db.GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if !row.BackfillDone {
		t.Error("backfill_done flag not set")
	}
	if row.TransferredBytes != 12445 || row.ReceivedBytes != 3700 || row.StoredBytes != 3600 ||
		row.FilesUploaded != 6 || row.FilesDeleted != 2 || row.BytesFreed != 300 ||
		row.DedupHits != 1 || row.DedupSavedBytes != 1000 {
		t.Errorf("unexpected finalized row: %+v", row)
	}

	type bucketKey struct {
		day  string
		key  uint
		kind string
	}
	wantBuckets := map[bucketKey][3]int64{
		{day(-10).Format(time.DateOnly), 1, KindUpload}: {1, 1000, 1000},
		{day(-10).Format(time.DateOnly), 0, KindUpload}: {1, 300, 300},
		{day(-9).Format(time.DateOnly), 2, KindUpload}:  {1, 1000, 1000},
		{day(-9).Format(time.DateOnly), 1, KindUpload}:  {1, 500, 500},
		{day(-8).Format(time.DateOnly), 0, KindDelete}:  {1, 0, 0},
		{day(-7).Format(time.DateOnly), 0, KindUpload}:  {1, 200, 200},
		{day(-7).Format(time.DateOnly), 0, KindDelete}:  {1, 0, 0},
	}
	var buckets []db.StatusDaily
	if err := client.NewSelect().Model(&buckets).Scan(ctx, &buckets); err != nil {
		t.Fatal(err)
	}
	if len(buckets) != len(wantBuckets) {
		t.Fatalf("got %d daily buckets, want %d: %+v", len(buckets), len(wantBuckets), buckets)
	}
	for _, b := range buckets {
		k := bucketKey{b.TS.Format(time.DateOnly), b.APIKeyID, b.Kind}
		w, ok := wantBuckets[k]
		if !ok {
			t.Errorf("unexpected bucket %+v", b)
			continue
		}
		if b.Files != w[0] || b.BytesIn != w[1] || b.BytesOut != w[2] {
			t.Errorf("bucket %v: files=%d in=%d out=%d, want %d/%d/%d", k, b.Files, b.BytesIn, b.BytesOut, w[0], w[1], w[2])
		}
		if b.ContentClass != ClassOther {
			t.Errorf("bucket %v: class %q, want %q", k, b.ContentClass, ClassOther)
		}
	}

	runBackfill(ctx, client, cutoff)
	if upFiles.Load() != 6 || dedupHits.Load() != 1 {
		t.Errorf("re-run mutated counters: upFiles=%d dedupHits=%d", upFiles.Load(), dedupHits.Load())
	}
	if n := countRows(ctx, t, client, (*db.StatusDaily)(nil)); n != len(wantBuckets) {
		t.Errorf("re-run changed daily bucket count: %d, want %d", n, len(wantBuckets))
	}
	row2, err := db.GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if row2 != row {
		t.Errorf("re-run changed the all-time row:\n got %+v\nwant %+v", row2, row)
	}
}
