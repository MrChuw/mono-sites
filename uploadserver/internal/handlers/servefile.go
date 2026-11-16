package handlers

import (
	"context"
	"fmt"
	"html"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/stats"
	"uploadserver/internal/umami"
)

func ServeFileHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filePath := strings.TrimPrefix(r.URL.Path, "/")
		filePath = strings.TrimLeft(filepath.ToSlash(filePath), "/")

		parts := strings.Split(filePath, "/")
		for _, part := range parts {
			if part == ".trash" || strings.HasPrefix(part, ".") {
				stats.RecordForbidden()
				http.Error(w, "Access denied", http.StatusForbidden)
				return
			}
		}

		umami.Instance.TrackPageViewAsync(r, "Asset View: "+filePath, "/"+filePath)
		fullPath := filepath.Join(config.UploadDir, filepath.FromSlash(filePath))

		absUpload, err := filepath.Abs(config.UploadDir)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		absFile, err := filepath.Abs(fullPath)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		rel, err := filepath.Rel(absUpload, absFile)
		if err != nil || strings.HasPrefix(rel, "..") {
			stats.RecordForbidden()
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}

		isPreview := strings.Contains(r.URL.Path, "/preview/")

		info, err := os.Stat(absFile)
		if err != nil || info.IsDir() {
			if isPreview && waitForPreviewAsset(r.Context(), absFile) {
				info, err = os.Stat(absFile)
			}
			if err != nil || info.IsDir() {
				stats.RecordNotFound()
				http.Error(w, "File not found", http.StatusNotFound)
				return
			}
		}

		if !isPreview && isLinkResolver(r.Header.Get("User-Agent")) {
			serveOpenGraph(w, r, fullPath, info)
			return
		}

		etag := fmt.Sprintf(`"%d-%d"`, info.ModTime().Unix(), info.Size())

		if isPreview {
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
			w.Header().Set("Vary", "Origin")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		}
		w.Header().Set("ETag", etag)

		hostType := r.Header.Get("X-Host-Type")

		if hostType == "no-cache" {
			if !r.URL.Query().Has("b") {
				var targetHost string
				if strings.Contains(r.Host, "localhost") {
					targetHost = strings.Replace(r.Host, "ip.localhost", "upload.localhost", 1)
				} else {
					targetHost = strings.Replace(r.Host, config.Proxy+".", config.Cdn+".", 1)
				}
				targetURL := config.ForwardedProto + "://" + targetHost + r.URL.RequestURI()
				w.Header().Set("Location", targetURL)
				stats.RecordRedirect()
				http.Redirect(w, r, targetURL, http.StatusFound)
				return
			}
		}

		mime := mime.TypeByExtension(strings.ToLower(filepath.Ext(fullPath)))

		if strings.HasPrefix(mime, "text/html") ||
			mime == "image/svg+xml" ||
			mime == "application/xhtml+xml" {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")

		served := estimateServedBytes(r, info.Size())

		if r.Header.Get("X-Handled-By") == "Caddy" {
			w.Header().Set("X-Accel-Redirect", "/internal-media/"+filePath)
			recordDelivery(r, isPreview, fullPath, mime, served)
			w.WriteHeader(http.StatusOK)
			return
		}

		recordDelivery(r, isPreview, fullPath, mime, served)
		http.ServeFile(w, r, absFile)
	}
}

var previewWaitTimeout = 5 * time.Minute

func waitForPreviewAsset(ctx context.Context, path string) bool {
	deadline := time.Now().Add(previewWaitTimeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func recordDelivery(r *http.Request, isPreview bool, fullPath, mimeType string, served int64) {
	if isPreview {
		stats.RecordPreviewGet(stats.ClassifyPreviewExt(filepath.Ext(fullPath)), served)
		return
	}
	stats.RecordFileGet(stats.ClassifyMIME(mimeType), served)
	baseName := strings.TrimSuffix(filepath.Base(fullPath), filepath.Ext(fullPath))
	stats.RecordFileServed(baseName, served)
}

func estimateServedBytes(r *http.Request, size int64) int64 {
	if r.Method == http.MethodHead {
		return 0
	}
	spec := r.Header.Get("Range")
	if spec == "" {
		return size
	}
	spec = strings.TrimPrefix(spec, "bytes=")
	if strings.Contains(spec, ",") {
		return 0
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return size
	}
	startStr := strings.TrimSpace(spec[:dash])
	if startStr == "" {
		n, err := strconv.ParseInt(strings.TrimSpace(spec[dash+1:]), 10, 64)
		if err != nil {
			return size
		}
		if n > size {
			n = size
		}
		return n
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return size
	}
	if start >= size {
		return 0
	}
	endStr := strings.TrimSpace(spec[dash+1:])
	if endStr == "" {
		return size - start
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil {
		return size - start
	}
	if end >= size {
		end = size - 1
	}
	if end < start {
		return 0
	}
	return end - start + 1
}

var linkResolvers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)chatterino-api-cache/\d+\.\d+\.\d+ link-resolver`),
	// regexp.MustCompile(`(?i)discordbot/\d+\.\d+`),
}

func isLinkResolver(userAgent string) bool {
	for _, re := range linkResolvers {
		if re.MatchString(userAgent) {
			return true
		}
	}
	return false
}

func serveOpenGraph(w http.ResponseWriter, r *http.Request, fullPath string, info os.FileInfo) {
	baseName := strings.TrimSuffix(filepath.Base(fullPath), filepath.Ext(fullPath))
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fullPath)))

	title := filepath.Base(fullPath)
	size := formatSize(info.Size())
	modTime := info.ModTime().Format("15:04 02/01/2006")
	description := fmt.Sprintf("%s • %s • %s", size, mimeType, modTime)

	scheme := config.ForwardedProto
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	relPath, err := filepath.Rel(config.UploadDir, fullPath)
	if err != nil {
		relPath = filepath.Base(fullPath)
	}

	cleanPath := filepath.ToSlash(relPath)
	if strings.Contains(cleanPath, "uploads/") {
		cleanPath = strings.SplitN(cleanPath, "uploads/", 2)[1]
	}
	webpURL := fmt.Sprintf("%s://%s/preview/w/%s.webp?chatterino", scheme, r.Host, baseName)

	isAnimatedSource := strings.HasPrefix(mimeType, "video/") ||
		mimeType == "image/gif" || mimeType == "image/webp"
	if !isAnimatedSource {
		webpURL = fmt.Sprintf("%s://%s/preview/t/%s.jpg?chatterino", scheme, r.Host, baseName)
		if _, err := os.Stat(filepath.Join(config.ThumbDir, "t", baseName+".jpg")); err != nil {
			if _, err := os.Stat(filepath.Join(config.ThumbDir, "t", baseName+".png")); err == nil {
				webpURL = fmt.Sprintf("%s://%s/preview/t/%s.png?chatterino", scheme, r.Host, baseName)
			}
		}
	}

	var videoTags string
	if strings.HasPrefix(mimeType, "video/") {
		videoURL := fmt.Sprintf("%s://%s/%s", scheme, r.Host, cleanPath)
		videoTags = fmt.Sprintf(`
    <meta property="og:video" content="%s">
    <meta property="og:video:type" content="%s">
    <meta property="og:video:width" content="1280">
    <meta property="og:video:height" content="720">`, videoURL, mimeType)
	}

	titleEscaped := html.EscapeString(title)
	descEscaped := html.EscapeString(description)
	imageEscaped := html.EscapeString(webpURL)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta property="og:title" content="%s">
    <meta property="og:image" content="%s">
    <meta property="og:description" content="%s">
    <meta property="og:type" content="video.other">
    %s
</head>
<body>
    <script>window.location.replace("%s://%s/%s");</script>
</body>
</html>`, titleEscaped, imageEscaped, descEscaped, videoTags, scheme, r.Host, cleanPath)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Vary", "*")
	stats.RecordResolverOG(int64(len(htmlContent)))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(htmlContent))
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
