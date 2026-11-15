package utils

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

const (
	bytePlotMaxSide = 160
)

var bytePlotPad = color.RGBA{R: 59, G: 59, B: 59, A: 255}

var bytePlotPalette = func() [256]color.RGBA {
	var pal [256]color.RGBA
	for b := 0; b < 256; b++ {
		switch {
		case b == 0x00:
			pal[b] = color.RGBA{R: 0, G: 0, B: 0, A: 255}
		case b == 0x09 || b == 0x0A || b == 0x0D || b == 0x20:
			pal[b] = color.RGBA{R: 30, G: 41, B: 59, A: 255}
		case b == 0xFF:
			pal[b] = color.RGBA{R: 255, G: 255, B: 255, A: 255}
		case b < 0x20 || b == 0x7F:
			pal[b] = color.RGBA{R: 124, G: 58, B: 237, A: 255}
		case b < 0x7F:
			t := float64(b-0x21) / float64(0x7E-0x21)
			pal[b] = hsvToRGBA(140, 0.9, 0.3+t*0.7)
		default:
			hue := float64(b-0x80) / float64(0xFF-0x80)
			pal[b] = hsvToRGBA(hue*360, 1, 1)
		}
	}
	return pal
}()

func hsvToRGBA(h, s, v float64) color.RGBA {
	h = math.Mod(h, 360) / 60
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case h < 1:
		r, g, b = c, x, 0
	case h < 2:
		r, g, b = x, c, 0
	case h < 3:
		r, g, b = 0, c, x
	case h < 4:
		r, g, b = 0, x, c
	case h < 5:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{
		R: uint8(math.Round((r + m) * 255)),
		G: uint8(math.Round((g + m) * 255)),
		B: uint8(math.Round((b + m) * 255)),
		A: 255,
	}
}

func sampleFileBytes(path string, size int64) ([]byte, error) {
	capacity := int64(bytePlotMaxSide) * int64(bytePlotMaxSide)
	if size <= capacity {
		return os.ReadFile(path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stripeLen := int64(bytePlotMaxSide)
	out := make([]byte, 0, capacity)
	buf := make([]byte, stripeLen)
	for i := int64(0); i < int64(bytePlotMaxSide); i++ {
		start := (size - stripeLen) * i / (int64(bytePlotMaxSide) - 1)
		if _, err := io.ReadFull(io.NewSectionReader(f, start, stripeLen), buf); err != nil {
			return nil, err
		}
		out = append(out, buf...)
	}
	return out, nil
}

func bytePlotImage(data []byte) *image.RGBA {
	side := int(math.Ceil(math.Sqrt(float64(len(data)))))
	for side*side < len(data) {
		side++
	}
	if side == 0 {
		side = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.Set(x, y, bytePlotPad)
		}
	}
	for i, b := range data {
		img.Set(i%side, i/side, bytePlotPalette[b])
	}
	return img
}

func processBytePlotPreview(ctx context.Context, client *bun.DB, file *db.UploadedFile) error {
	baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))
	thumbDir := filepath.Join(config.ThumbDir, "t")
	if err := os.MkdirAll(thumbDir, 0755); err != nil {
		return fmt.Errorf("byte plot mkdir failed: %v", err)
	}

	info, err := os.Stat(file.SavedPath)
	if err != nil {
		return fmt.Errorf("byte plot stat failed: %v", err)
	}
	data, err := sampleFileBytes(file.SavedPath, info.Size())
	if err != nil {
		return fmt.Errorf("byte plot sampling failed: %v", err)
	}

	plotPath := filepath.Join(thumbDir, baseName+".png")
	os.Remove(plotPath)
	out, err := os.Create(plotPath)
	if err != nil {
		return fmt.Errorf("byte plot create failed: %v", err)
	}
	encErr := png.Encode(out, bytePlotImage(data))
	if closeErr := out.Close(); encErr == nil {
		encErr = closeErr
	}
	if encErr != nil {
		os.Remove(plotPath)
		return fmt.Errorf("byte plot encode failed: %v", encErr)
	}

	slog.Info("Byte-plot preview generated", "file_id", file.ID, "sampled_bytes", len(data))
	recordPreviewAsset(plotPath, stats.ClassBytePlot)
	return db.UpdateFilePreview(ctx, client, file.ID, "done", "", &db.PreviewFields{
		ThumbnailPath: plotPath,
	})
}
