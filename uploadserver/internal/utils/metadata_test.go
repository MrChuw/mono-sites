package utils

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: 90, B: 200, A: 255})
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

func exifTag(t *testing.T, path, tag string) string {
	t.Helper()
	out, err := exec.Command("exiftool", "-s3", "-"+tag, path).Output()
	if err != nil {
		t.Fatalf("exiftool read %s: %v", tag, err)
	}
	return strings.TrimSpace(string(out))
}

func TestStripPNGMetadata(t *testing.T) {
	setupDirs(t)
	path := filepath.Join(config.UploadDir, "photo.png")
	writeTestPNG(t, path)

	cmd := exec.Command("exiftool", "-overwrite_original",
		"-Comment=super-secret-comment", "-Artist=mr-chuw", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding metadata: %v (%s)", err, out)
	}
	if got := exifTag(t, path, "Comment"); got != "super-secret-comment" {
		t.Fatalf("metadata not planted: %q", got)
	}

	if err := StripAllMetadata(path, "image/png"); err != nil {
		t.Fatalf("StripAllMetadata: %v", err)
	}

	if got := exifTag(t, path, "Comment"); got != "" {
		t.Errorf("Comment survived: %q", got)
	}
	if got := exifTag(t, path, "Artist"); got != "" {
		t.Errorf("Artist survived: %q", got)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := png.Decode(f); err != nil {
		t.Errorf("stripped file is not a valid PNG: %v", err)
	}
}

func TestStripMP4MetadataViaFallback(t *testing.T) {
	setupDirs(t)
	path := filepath.Join(config.UploadDir, "clip.mp4")

	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=0.5:size=128x96:rate=10",
		"-metadata", "comment=super-secret-comment",
		"-c:v", "mpeg4", "-q:v", "5", "-y", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating mp4: %v (%s)", err, out)
	}
	if got := exifTag(t, path, "Comment"); got != "super-secret-comment" {
		t.Fatalf("metadata not planted: %q", got)
	}

	if err := StripAllMetadata(path, "video/mp4"); err != nil {
		t.Fatalf("StripAllMetadata: %v", err)
	}

	if got := exifTag(t, path, "Comment"); got != "" {
		t.Errorf("Comment survived: %q", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("stripped mp4 missing/empty: %v", err)
	}
}

func TestProcessPreviewJobBytePlot(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	file := seedFile(t, ctx, client, config.UploadDir, "notes.txt",
		"the quick brown fox jumps over the lazy dog\n\x00\x01\x02\xff", nil)

	if err := ProcessPreviewJob(ctx, client, PreviewJob{FileID: file.ID}); err != nil {
		t.Fatal(err)
	}

	base := strings.TrimSuffix(file.SavedPath, filepath.Ext(file.SavedPath))
	plotPath := filepath.Join(config.ThumbDir, "t", filepath.Base(base)+".png")
	fi, err := os.Stat(plotPath)
	if err != nil {
		t.Fatalf("byte plot missing: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("byte plot is empty")
	}

	row, err := db.GetFileByID(ctx, client, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PreviewStatus != "done" || row.ThumbnailPath != plotPath {
		t.Fatalf("row after byte plot: %+v", row)
	}
}

func TestProcessPreviewJobFallsBackForBrokenMedia(t *testing.T) {
	ctx := context.Background()
	client := openTestDB(t)
	setupDirs(t)

	path := filepath.Join(config.UploadDir, "broken.png")
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	file := &db.UploadedFile{
		ID: "fid-broken", Filename: "broken.png", SavedPath: path,
		FileSize: int64(len(body)), FileHash: "h",
		UploadedAt: time.Now().UTC().Add(-time.Hour), DeletionToken: "tok-broken",
	}
	if _, err := client.NewInsert().Model(file).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	failsBefore := stats.PreviewFailures()
	if err := ProcessPreviewJob(ctx, client, PreviewJob{FileID: file.ID}); err != nil {
		t.Fatalf("broken media should fall back, not error: %v", err)
	}

	base := strings.TrimSuffix(path, filepath.Ext(path))
	plotPath := filepath.Join(config.ThumbDir, "t", filepath.Base(base)+".png")
	if _, err := os.Stat(plotPath); err != nil {
		t.Fatalf("fallback byte plot missing: %v", err)
	}
	row, err := db.GetFileByID(ctx, client, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PreviewStatus != "done" {
		t.Fatalf("status after fallback: %+v", row)
	}
	if stats.PreviewFailures() != failsBefore+1 {
		t.Errorf("preview failure was not booked")
	}
}
