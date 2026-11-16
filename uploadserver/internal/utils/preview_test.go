package utils

import (
	"fmt"
	"math"
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
		{"1080p clamps to the bar", 1920, 1080, 113.0 / 1080.0, 208},
		{"720p clamps to the bar", 1280, 720, 113.0 / 720.0, 208},
		{"4k clamps to the bar", 3840, 2160, 113.0 / 2160.0, 208},
		{"4:3 clamps to the bar", 1440, 1080, 113.0 / 1080.0, 256},
		{"portrait clamps short edge", 1080, 1920, 113.0 / 1080.0, 208},
		{"480p clamps to the bar", 854, 480, 113.0 / 480.0, 208},
		{"360p clamps to the bar", 640, 360, 113.0 / 360.0, 208},
		{"tiny source stays native", 160, 90, 1.0, 256},
		{"odd dims scale to the bar", 1234, 567, 113.0 / 567.0, 170},
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
			if pw*ph*frames*4 > previewMaxDecodedBytes {
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
	scale, frames := previewPlan(25000, 450)
	if frames != previewMinFrames {
		t.Fatalf("frames = %d, want %d", frames, previewMinFrames)
	}
	if scale >= 113.0/450.0 {
		t.Fatalf("scale %v not reduced below the bar", scale)
	}
	pw, ph := planDims(5000, 450, scale)
	if pw*ph*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("plan exceeds budget: %dx%d @ %d frames", pw, ph, frames)
	}
}

func TestPreviewPlanRespectsBudget(t *testing.T) {
	dims := [][2]int{
		{1920, 1080}, {1280, 720}, {3840, 2160}, {7680, 4320},
		{1440, 1080}, {1080, 1920}, {854, 480}, {640, 360},
		{320, 180}, {2560, 1080}, {2560, 1440}, {1920, 64},
		{64, 1920}, {1234, 567}, {9999, 100}, {100, 9999},
		{270, 270}, {271, 271}, {5000, 450},
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
		if pw*ph*frames*4 > previewMaxDecodedBytes {
			t.Fatalf("%dx%d: plan %dx%d @ %d frames = %d bytes > %d",
				d[0], d[1], pw, ph, frames, pw*ph*frames*4, previewMaxDecodedBytes)
		}
	}
}

func TestPlaybackRate(t *testing.T) {
	cases := []struct {
		name    string
		frames  int
		dur     float64
		wantFps int
	}{
		{"short clip stays real-time", 90, 4.5, 20},
		{"tiny clip stays real-time", 20, 1.0, 20},
		{"budget-length clip stays real-time", 208, 20.885, 20},
		{"long video slows toward half source speed", 208, 60, 7},
		{"exactly at the floor boundary", 208, 104, 4},
		{"very long video floors at min fps", 208, 2640, 4},
		{"unknown duration falls back to max", 208, 0, 20},
		{"single frame floors at min fps", 1, 1000, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fps := playbackRate(tc.frames, tc.dur); fps != tc.wantFps {
				t.Fatalf("playbackRate(%d, %v) = %d, want %d", tc.frames, tc.dur, fps, tc.wantFps)
			}
		})
	}
}

func TestComputePlaybackPlan(t *testing.T) {
	setupDirs(t)
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 24)

	scale, frames, sampleFps, playbackFps := computePlaybackPlan(source, previewMaxFrames, previewPlaybackFps)
	if scale != 113.0/360.0 {
		t.Fatalf("scale = %v, want %v", scale, 113.0/360.0)
	}
	if frames != 208 {
		t.Fatalf("frames = %d, want 208 (area budget)", frames)
	}
	dur, err := videoDuration(source)
	if err != nil {
		t.Fatalf("videoDuration: %v", err)
	}
	if math.Abs(sampleFps-float64(frames)/dur) > 1e-9 {
		t.Fatalf("sampleFps = %v, want %v (frames/duration)", sampleFps, float64(frames)/dur)
	}
	if playbackFps != 17 {
		t.Fatalf("playbackFps = %d, want 17 (~2x fast-forward for a 24s source)", playbackFps)
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

func frameDelay(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-i", path, "-vf", "showinfo",
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decoding %s: %v (%s)", path, err, out)
	}
	var times []float64
	for _, part := range strings.Split(string(out), "pts_time:") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if v, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
			times = append(times, v)
		}
	}
	if len(times) < 2 {
		t.Fatalf("could not read frame timings from %s", path)
	}
	return times[1] - times[0]
}

func TestOptimizeWebpHDFitsChatterinoBudget(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "1920x1080", 4.5)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 90 {
		t.Fatalf("preview frames %d, want 90 (4.5s at 20fps)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
	if delay := frameDelay(t, out); delay != 0.05 {
		t.Fatalf("frame delay %v, want 0.05 (20fps playback)", delay)
	}
}

func TestOptimizeWebpShortVideoStaysRealTime(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "1920x1080", 1.0)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 20 {
		t.Fatalf("preview frames %d, want 20 (1s at 20fps)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
	if delay := frameDelay(t, out); delay != 0.05 {
		t.Fatalf("frame delay %v, want 0.05 (real-time loop)", delay)
	}
}

func TestOptimizeWebpSmallSourceStaysNative(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "160x90", 4.5)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 160 || h != 90 {
		t.Fatalf("preview dims %dx%d, want 160x90", w, h)
	}
	if frames != 90 {
		t.Fatalf("preview frames %d, want 90 (4.5s at 20fps)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
}

func TestOptimizeWebp1440pPlanScaleBelowLegacyFloor(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "2560x1440", 4.5)

	// The 1440p bar scale (113/1440 = 0.0785) sits below the byte-pressure
	// shrink floor; the loop must still run at the planned scale.
	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 90 {
		t.Fatalf("preview frames %d, want 90 (4.5s at 20fps)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
}

func TestOptimizeWebpLongVideoSpendsBudgetOnFrames(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "1920x1080", 20.885)

	// The reference feridinha file: long enough that the area budget, not the
	// duration, binds — resolution is traded for a long, smooth loop.
	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 208 {
		t.Fatalf("preview frames %d, want 208 (area budget at 18MB)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
	if delay := frameDelay(t, out); delay != 0.05 {
		t.Fatalf("frame delay %v, want 0.05 (20fps playback)", delay)
	}
}

func TestOptimizeWebpLongVideoSlowsPlayback(t *testing.T) {
	setupDirs(t)
	webpDir := filepath.Join(config.ThumbDir, "w")
	if err := os.MkdirAll(webpDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 60)

	out, err := optimizeWebp(source, webpDir, "clip")
	if err != nil {
		t.Fatalf("optimizeWebp: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 208 {
		t.Fatalf("preview frames %d, want 208 (whole 60s at the area budget)", frames)
	}
	if w*h*frames*4 > previewMaxDecodedBytes {
		t.Fatalf("preview exceeds chatterino budget")
	}
	if delay := frameDelay(t, out); delay < 0.135 || delay > 0.155 {
		t.Fatalf("frame delay %v, want ~0.143 (7fps, ~2x fast-forward)", delay)
	}
}

func TestOptimizeGifShortVideoStaysRealTime(t *testing.T) {
	setupDirs(t)
	gifDir := filepath.Join(config.ThumbDir, "p")
	if err := os.MkdirAll(gifDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 8)

	out, err := optimizeGif(source, gifDir, "clip")
	if err != nil {
		t.Fatalf("optimizeGif: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 96 {
		t.Fatalf("preview frames %d, want 96 (8s at 12fps)", frames)
	}
	if delay := frameDelay(t, out); delay < 0.07 || delay > 0.1 {
		t.Fatalf("frame delay %v, want ~0.083 (real-time 12fps loop)", delay)
	}
}

func TestOptimizeGifLongVideoSlowsPlayback(t *testing.T) {
	setupDirs(t)
	gifDir := filepath.Join(config.ThumbDir, "p")
	if err := os.MkdirAll(gifDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 30)

	out, err := optimizeGif(source, gifDir, "clip")
	if err != nil {
		t.Fatalf("optimizeGif: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 96 {
		t.Fatalf("preview frames %d, want 96 (gif frame cap)", frames)
	}
	if delay := frameDelay(t, out); delay < 0.15 || delay > 0.18 {
		t.Fatalf("frame delay %v, want ~0.167 (6fps, ~2x fast-forward)", delay)
	}
}

func TestOptimizeGifDwellFloor(t *testing.T) {
	setupDirs(t)
	gifDir := filepath.Join(config.ThumbDir, "p")
	if err := os.MkdirAll(gifDir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(config.UploadDir, "clip.mp4")
	generateTestVideo(t, source, "640x360", 80)

	out, err := optimizeGif(source, gifDir, "clip")
	if err != nil {
		t.Fatalf("optimizeGif: %v", err)
	}
	w, h, frames := probeWebp(t, out)
	if w != 200 || h != 112 {
		t.Fatalf("preview dims %dx%d, want 200x112", w, h)
	}
	if frames != 96 {
		t.Fatalf("preview frames %d, want 96 (gif frame cap)", frames)
	}
	if delay := frameDelay(t, out); delay < 0.24 || delay > 0.26 {
		t.Fatalf("frame delay %v, want 0.25 (min playback fps floor)", delay)
	}
}
