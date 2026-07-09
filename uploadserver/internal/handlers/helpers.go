// Package handlers
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
)

type contextKey string

const (
	apiKeyContextKey contextKey = "apiKey"
	cloudflareKey    contextKey = "cloudflare"
)

func FaviconHandler(client *bun.DB) http.HandlerFunc {
	whitePixel := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x60, 0x18, 0x05, 0xA3,
		0x60, 0x14, 0x8C, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0x03, 0x00, 0x00, 0x06,
		0x00, 0x05, 0x57, 0xBF, 0x30, 0xA4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
		0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	return func(w http.ResponseWriter, r *http.Request) {
		defaultFavicon := filepath.Join(config.UploadDir, "favicon.ico")
		if info, err := os.Stat(defaultFavicon); err == nil && !info.IsDir() {
			http.ServeFile(w, r, defaultFavicon)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(whitePixel)
	}
}

func getAPIKeyFromContext(ctx context.Context) (db.APIKey, bool) {
	val := ctx.Value(apiKeyContextKey)
	if val == nil {
		return db.APIKey{}, false
	}
	key, ok := val.(db.APIKey)
	return key, ok
}

func getCloudflareFromContext(ctx context.Context) (CloudflareMetadata, bool) {
	val := ctx.Value(cloudflareKey)
	if val == nil {
		return CloudflareMetadata{}, false
	}
	cf, ok := val.(CloudflareMetadata)
	return cf, ok
}

func buildBaseURL(r *http.Request) string {
	scheme := config.ForwardedProto
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := r.Host
	if strings.Contains(host, "localhost") {
		if strings.HasPrefix(host, "upload.localhost") {
			host = strings.Replace(host, "upload.localhost", "i.localhost", 1)
		}
	} else {
		cdnPrefix := config.Cdn + "."
		proxyPrefix := config.Proxy + "."

		if strings.HasPrefix(host, cdnPrefix) {
			host = strings.Replace(host, cdnPrefix, proxyPrefix, 1)
		}
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
