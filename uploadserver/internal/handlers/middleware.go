package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
)

type CloudflareMetadata struct {
	IP      string
	Country string
	UA      string
}

func CloudflareMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cf := CloudflareMetadata{
			IP:      r.Header.Get("CF-Connecting-IP"),
			Country: r.Header.Get("CF-IPCountry"),
			UA:      r.Header.Get("User-Agent"),
		}
		if cf.IP == "" {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				ips := strings.Split(xff, ",")
				if len(ips) > 0 {
					cf.IP = strings.TrimSpace(ips[0])
				}
			}
		}
		if cf.IP == "" {
			cf.IP = "127.0.0.1"
		}
		if cf.Country == "" {
			cf.Country = "XX"
		}
		if cf.UA == "" {
			cf.UA = "Unknown"
		}
		ctx := context.WithValue(r.Context(), cloudflareKey, cf)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func AuthMiddleware(client *bun.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				http.Error(w, "Missing X-API-Key header", http.StatusUnauthorized)
				return
			}

			keyRecord, err := db.GetAPIKeyByKey(r.Context(), client, apiKey)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "Invalid or revoked API Key", http.StatusForbidden)
				} else {
					http.Error(w, "Database error", http.StatusInternalServerError)
				}
				return
			}

			ctx := context.WithValue(r.Context(), apiKeyContextKey, *keyRecord)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
