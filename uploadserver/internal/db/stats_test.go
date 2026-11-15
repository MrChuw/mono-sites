package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *bun.DB {
	t.Helper()
	sqldb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	client := bun.NewDB(sqldb, sqlitedialect.New())
	t.Cleanup(func() { client.Close() })
	if err := MigrateDB(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return client
}

func countRows(ctx context.Context, t *testing.T, dbConn bun.IDB, model any) int {
	t.Helper()
	var n int
	if err := dbConn.NewSelect().Model(model).ColumnExpr("count(*)").Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestServiceStatsRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	row, err := GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if row != (ServiceStats{ID: 1}) {
		t.Fatalf("missing row read: %+v", row)
	}

	in := &ServiceStats{
		ID: 1, TransferredBytes: 100, ReceivedBytes: 200, StoredBytes: 150,
		FilesUploaded: 3, FilesDeleted: 1, BytesFreed: 40,
		DedupHits: 2, DedupSavedBytes: 60, ResolverHits: 5,
		AuthFailures: 6, BusyErrors: 7, PreviewFailures: 8,
	}
	if err := SetServiceStats(ctx, client, in); err != nil {
		t.Fatal(err)
	}
	in.TransferredBytes = 500
	if err := SetServiceStats(ctx, client, in); err != nil {
		t.Fatal(err)
	}
	out, err := GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if out != *in {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, *in)
	}
	if n := countRows(ctx, t, client, (*ServiceStats)(nil)); n != 1 {
		t.Fatalf("service_stats rows = %d, want 1", n)
	}
}

func TestFinalizeStatsBackfillFlag(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if err := SetServiceStats(ctx, client, &ServiceStats{ID: 1, TransferredBytes: 10}); err != nil {
		t.Fatal(err)
	}

	final := &ServiceStats{ID: 1, TransferredBytes: 10, ReceivedBytes: 900, StoredBytes: 900, FilesUploaded: 7}
	if err := FinalizeStatsBackfill(ctx, client, final); err != nil {
		t.Fatal(err)
	}
	out, err := GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if !out.BackfillDone || out.FilesUploaded != 7 || out.StoredBytes != 900 {
		t.Fatalf("finalize did not persist row+flag: %+v", out)
	}

	if err := SetServiceStats(ctx, client, &ServiceStats{ID: 1, TransferredBytes: 20}); err != nil {
		t.Fatal(err)
	}
	out, err = GetServiceStats(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if !out.BackfillDone {
		t.Fatal("SetServiceStats reset backfill_done")
	}
	if out.TransferredBytes != 20 {
		t.Fatalf("flush write did not apply: %+v", out)
	}
}

func TestInsertStatusDeltasKeepsCallerSlice(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	rows := []StatusDelta{
		{TS: time.Now().UTC(), Kind: "upload", ContentClass: "video", BytesIn: 100, BytesOut: 90, Files: 1},
		{TS: time.Now().UTC(), Kind: "dedup", ContentClass: "other", BytesOut: 50, Files: 1},
	}
	if err := InsertStatusDeltas(ctx, client, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].BytesIn != 100 || rows[1].BytesOut != 50 {
		t.Fatalf("caller slice was mutated: %+v", rows)
	}
	if n := countRows(ctx, t, client, (*StatusDelta)(nil)); n != 2 {
		t.Fatalf("deltas persisted = %d, want 2", n)
	}
}

func TestUpsertStatusRollupAdds(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	day := time.Now().UTC().Truncate(24 * time.Hour)
	first := []StatusDelta{
		{TS: day, APIKeyID: 3, Kind: "upload", ContentClass: "video", BytesIn: 100, BytesOut: 90, Files: 1},
		{TS: day, Kind: "dedup", ContentClass: "other", BytesOut: 10, Files: 1},
	}
	second := []StatusDelta{
		{TS: day, APIKeyID: 3, Kind: "upload", ContentClass: "video", BytesIn: 30, BytesOut: 20, Files: 2},
		{TS: day.Add(24 * time.Hour), APIKeyID: 3, Kind: "upload", ContentClass: "video", BytesIn: 5, Files: 1},
		{TS: day, APIKeyID: 3, Kind: "upload", ContentClass: "image", BytesIn: 1, Files: 1},
		{TS: day, APIKeyID: 4, Kind: "upload", ContentClass: "video", BytesIn: 2, Files: 1},
	}
	for _, table := range []string{"status_hourly", "status_daily"} {
		if err := UpsertStatusRollup(ctx, client, table, first); err != nil {
			t.Fatal(err)
		}
		if err := UpsertStatusRollup(ctx, client, table, second); err != nil {
			t.Fatal(err)
		}
	}

	var daily []StatusDaily
	if err := client.NewSelect().Model(&daily).Order("ts ASC").Scan(ctx, &daily); err != nil {
		t.Fatal(err)
	}
	if len(daily) != 5 {
		t.Fatalf("status_daily rows = %d, want 5", len(daily))
	}
	var merged StatusDaily
	if err := client.NewSelect().Model(&merged).
		Where("ts = ?", day).
		Where("api_key_id = ?", 3).
		Where("kind = ?", "upload").
		Where("content_class = ?", "video").
		Scan(ctx, &merged); err != nil {
		t.Fatal(err)
	}
	if merged.BytesIn != 130 || merged.BytesOut != 110 || merged.Files != 3 {
		t.Fatalf("additive merge wrong: %+v", merged)
	}

	if n := countRows(ctx, t, client, (*StatusHourly)(nil)); n != 5 {
		t.Fatalf("status_hourly rows = %d, want 5", n)
	}
}

func TestPruneStatusDeltas(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	now := time.Now().UTC()
	if err := InsertStatusDeltas(ctx, client, []StatusDelta{
		{TS: now.AddDate(0, 0, -30), Kind: "dedup", ContentClass: "other"},
		{TS: now, Kind: "dedup", ContentClass: "other"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertStatusRollup(ctx, client, "status_daily", []StatusDelta{
		{TS: now.AddDate(0, 0, -30).Truncate(24 * time.Hour), Kind: "dedup", ContentClass: "other", Files: 1},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := PruneStatusDeltas(ctx, client, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	if left := countRows(ctx, t, client, (*StatusDelta)(nil)); left != 1 {
		t.Fatalf("deltas left = %d, want 1", left)
	}
	if left := countRows(ctx, t, client, (*StatusDaily)(nil)); left != 1 {
		t.Fatalf("rollups left = %d, want 1", left)
	}
}

func TestGetStatusDailyByKind(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	old := today.AddDate(0, 0, -40)
	if err := UpsertStatusRollup(ctx, client, "status_daily", []StatusDelta{
		{TS: today, Kind: "upload", ContentClass: "video", BytesIn: 100, BytesOut: 90, Files: 1},
		{TS: today, Kind: "upload", ContentClass: "image", BytesIn: 10, Files: 1},
		{TS: today, Kind: "file_get", ContentClass: "video", BytesOut: 500, Requests: 2},
		{TS: yesterday, Kind: "upload", ContentClass: "video", BytesIn: 60, Files: 1},
		{TS: old, Kind: "upload", ContentClass: "video", BytesIn: 999, Files: 9},
	}); err != nil {
		t.Fatal(err)
	}

	days, err := GetStatusDailyByKind(ctx, client, 30)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		day  string
		kind string
	}
	got := make(map[key]StatusKindDay, len(days))
	for _, d := range days {
		got[key{d.Day.Format(time.DateOnly), d.Kind}] = d
	}
	if len(got) != 3 {
		t.Fatalf("got %d (day,kind) rows, want 3: %+v", len(got), days)
	}
	if d := got[key{yesterday.Format(time.DateOnly), "upload"}]; d.Files != 1 || d.BytesIn != 60 {
		t.Fatalf("yesterday upload wrong: %+v", d)
	}
	if d := got[key{today.Format(time.DateOnly), "upload"}]; d.Files != 2 || d.BytesIn != 110 || d.BytesOut != 90 {
		t.Fatalf("today upload wrong: %+v", d)
	}
	if d := got[key{today.Format(time.DateOnly), "file_get"}]; d.Requests != 2 || d.BytesOut != 500 {
		t.Fatalf("today file_get wrong: %+v", d)
	}
}

func TestGetStatusTotals(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := today.AddDate(0, 0, -40)
	if err := UpsertStatusRollup(ctx, client, "status_daily", []StatusDelta{
		{TS: today, Kind: "upload", ContentClass: "video", BytesIn: 100, BytesOut: 90, Files: 1},
		{TS: today, Kind: "upload", ContentClass: "image", BytesIn: 10, Files: 1},
		{TS: today, Kind: "file_get", ContentClass: "video", BytesOut: 500, Requests: 2},
		{TS: today.AddDate(0, 0, -1), Kind: "upload", ContentClass: "video", BytesIn: 60, Files: 1},
		{TS: old, Kind: "upload", ContentClass: "video", BytesIn: 999, Files: 9},
	}); err != nil {
		t.Fatal(err)
	}

	kinds, err := GetStatusTotals(ctx, client, "kind", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 {
		t.Fatalf("kinds rows = %d, want 2: %+v", len(kinds), kinds)
	}
	if kinds[0].Group != "file_get" || kinds[0].BytesOut != 500 || kinds[0].Requests != 2 {
		t.Fatalf("file_get totals wrong: %+v", kinds[0])
	}
	if kinds[1].Group != "upload" || kinds[1].BytesIn != 170 || kinds[1].Files != 3 {
		t.Fatalf("upload totals wrong: %+v", kinds[1])
	}

	classes, err := GetStatusTotals(ctx, client, "content_class", "upload", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(classes) != 2 {
		t.Fatalf("class rows = %d, want 2: %+v", len(classes), classes)
	}
	if classes[0].Group != "video" || classes[0].BytesIn != 160 || classes[0].Files != 2 {
		t.Fatalf("video class wrong: %+v", classes[0])
	}
	if classes[1].Group != "image" || classes[1].BytesIn != 10 {
		t.Fatalf("image class wrong: %+v", classes[1])
	}

	all, err := GetStatusTotals(ctx, client, "kind", "upload", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].BytesIn != 1169 || all[0].Files != 12 {
		t.Fatalf("all-time upload totals wrong: %+v", all)
	}

	if _, err := GetStatusTotals(ctx, client, "api_key_id", "", 30); err == nil {
		t.Fatal("invalid dimension accepted")
	}
}

func TestGetStatusKeyTotals(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	if _, err := CreateAndInsertAPIKey(ctx, client, "k1", "alice", RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAndInsertAPIKey(ctx, client, "k2", "bob", RoleNormal, nil); err != nil {
		t.Fatal(err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := UpsertStatusRollup(ctx, client, "status_daily", []StatusDelta{
		{TS: today, APIKeyID: 1, Kind: "upload", ContentClass: "video", BytesIn: 500, BytesOut: 400, Files: 2},
		{TS: today, Kind: "upload", ContentClass: "other", BytesIn: 300, Files: 1},
		{TS: today, Kind: "dedup", ContentClass: "other", BytesOut: 50},
	}); err != nil {
		t.Fatal(err)
	}

	keys, err := GetStatusKeyTotals(ctx, client, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("key rows = %d, want 1: %+v", len(keys), keys)
	}
	if keys[0].Owner != "alice" || keys[0].Files != 2 || keys[0].BytesIn != 500 || keys[0].BytesOut != 400 {
		t.Fatalf("alice totals wrong: %+v", keys[0])
	}
}

func TestGetTopFiles(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)

	now := time.Now().UTC()
	if err := UpsertFileStats(ctx, client, []FileStatFlush{
		{FileName: "a", Requests: 1, BytesOut: 100, LastServed: now},
		{FileName: "b", Requests: 5, BytesOut: 900, LastServed: now},
		{FileName: "c", Requests: 2, BytesOut: 500, LastServed: now},
	}); err != nil {
		t.Fatal(err)
	}

	top, err := GetTopFiles(ctx, client, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].FileName != "b" || top[1].FileName != "c" {
		t.Fatalf("top files wrong: %+v", top)
	}
}
