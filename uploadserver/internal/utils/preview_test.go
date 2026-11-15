package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"uploadserver/internal/config"
)

func TestPreviewPlan(t *testing.T) {
	cases := []struct {
		name       string
		srcW       int
		srcH       int
		wantScale  float64
		wantFrames int
	}{
		{"1080p clamps to the bar", 1920, 1080, 540.0 / 1080.0, 10},
		{"720p clamps to the bar", 1280, 720, 540.0 / 720.0, 10},
		{"4k clamps to the bar", 3840, 2160, 540.0 / 2160.0, 10},
		{"4:3 clamps to the bar", 1440, 1080, 540.0 / 1080.0, 13},
		{"portrait clamps short edge", 1080, 1920, 540.0 / 1080.0, 10},
		{"480p stays native", 854, 480, 1.0, 12},
		{"360p stays native", 640, 360, 1.0, 22},
		{"tiny source keeps all frames", 320, 180, 1.0, 30},
		{"odd dims honor the frame floor", 1234, 567, 540.0 / 567.0, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scale, frames := previewPlan(tc.srcW, tc.srcH)
			if scale != tc.wantScale {
				t.Fatalf("scale = %v, want %v", scale, tc.wantScale)
			}
			if frames != tc.wantFrames {
				t.Fatalf("frames = %d, want %d", frames, tc.wantFrames)
			}
			pw, ph := planDims(tc.srcW, tc.srcH, scale)
			if pw*ph*frames*4 > chatterinoMaxDecodedBytes {
				t.Fatalf("plan exceeds budget: %dx%d @ %d frames = %d bytes",
					pw, ph, frames, pw*ph*frames*4)
			}
			srcShort := tc.srcW
			if tc.srcH < srcShort {
				srcShort = tc.srcH
			}
			if srcShort > previewShortEdgeCap {
				short := pw
				if ph < short {
					short = ph
				}
				if short > previewShortEdgeCap {
					t.Fatalf("short edge %d exceeds cap %d", short, previewShortEdgeCap)
				}
			}
		})
	}
}

func TestPreviewPlanFrameFloorTradesResolution(t *testing.T) {
	scale, frames := previewPlan(2560, 1080)
	if frames != previewMinFrames {
		t.Fatalf("frames = %d, want %d", frames, previewMinFrames)
	}
	if scale >= 540.0/1080.0 {
		t.Fatalf("scale %v not reduced below the bar", scale)
	}
	pw, ph := planDims(2560, 1080, scale)
	if pw*ph*frames*4 > chatterinoMaxDecodedBytes {
		t.Fatalf("plan exceeds budget: %dx%d @ %d frames", pw, ph, frames)
	}
}

func TestPreviewPlanRespectsBudget(t *testing.T) {
	dims := [][2]int{
		{1920, 1080}, {1280, 720}, {3840, 2160}, {7680, 4320},
		{1440, 1080}, {1080, 1920}, {854, 480}, {640, 360},
		{320, 180}, {2560, 1080}, {2560, 1440}, {1920, 64},
		{64, 1920}, {1234, 567}, {9999, 100}, {100, 9999},
		{540, 540}, {541, 541},
	}
	for _, d := range dims {
		scale, frames := previewPlan(d[0], d[1])
		if scale <= 0 || scale > 1.0 {
			t.Fatalf("%dx%d: scale %v out of range", d[0], d[1], scale)
		}
		if frames < previewMinFrames || frames > previewMaxFrames {
			t.Fatalf("%dx%d: frames %d outside [%d,%d]",
				d[0], d[1], frames, previewMinFrames, previewMaxFrames)
		}
		pw, ph := planDims(d[0], d[1], scale)
		if pw*ph*frames*4 > chatterinoMaxDecodedBytes {
			t.Fatalf("%dx%d: plan %dx%d @ %d frames = %d bytes > %d",
				d[0], d[1], pw, ph, frames, pw*ph*frames*4, chatterinoMaxDecodedBytes)
		}
	}
}

func generateTestVideo(t *testing.T, path, size string, seconds float64) {
	t.Helper()
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=30:duration=%v", size, seconds),
		"-c:v", "mpeg4", "-q:v", "5", "-y", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating test video: %v (%s)", err, out)
	}
}

func probeWebp(t *testing.T, path string) (int, int, int) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=width,height,nb_read_frames",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe on %s: %v (%s)", path, err, out)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) != 3 {
		t.Fatalf("unexpected ffprobe output: %q", string(out))
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	frames, errF := strconv.Atoi(parts[2])
	if errW != nil || errH != nil || errF != nil {
		t.Fatalf("unexpected ffprobe output: %q", string(out))
	}
	return w, h, frames
}

func TestOptimizeWebpHDFitsChatterinoBudget(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "1920x1080", 1.5)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 960 || h != 540 {
		t.Fatalf("preview dims %dx%d, want 960x540", w, h)
	}
	if frames != 10 {
		t.Fatalf("preview frames %d, want 10", frames)
	}
	if w*h*frames*4 > chatterinoMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
}

func TestOptimizeWebpSmallSourceStaysNative(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 3.0)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 640 || h != 360 {
		t.Fatalf("preview dims %dx%d, want 640x360", w, h)
	}
	if frames != 22 {
		t.Fatalf("preview frames %d, want 22", frames)
	}
	if w*h*frames*4 > chatterinoMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
}
