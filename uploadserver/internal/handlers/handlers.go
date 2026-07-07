package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"uploadserver/internal/utils"
	"uploadserver/internal/config"
	"uploadserver/internal/umami"
	"uploadserver/internal/db"
)

type contextKey string

const (
	apiKeyContextKey contextKey = "apiKey"
	cloudflareKey    contextKey = "cloudflare"
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

func AuthMiddleware(client *gorm.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				http.Error(w, "Missing X-API-Key header", http.StatusUnauthorized)
				return
			}
			var keyRecord db.APIKey
			if err := client.Where("key = ?", apiKey).First(&keyRecord).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					http.Error(w, "Invalid or revoked API Key", http.StatusForbidden)
				} else {
					http.Error(w, "Database error", http.StatusInternalServerError)
				}
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyContextKey, keyRecord)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func DeleteFileHandler(client *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if len(token) != 64 {
			http.Error(w, "Malformed token", http.StatusBadRequest)
			return
		}
		var file db.UploadedFile
		if err := client.Preload("APIKey").Where("deletion_token = ?", token).First(&file).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, "Database error", http.StatusInternalServerError)
			}
			return
		}
		umamiData := umami.BuildUmamiData(r, file.APIKey.Owner,
		    umami.WithFilename(file.Filename),
		    umami.WithUploadedAt(file.UploadedAt),
		)

		umami.Instance.TrackEventAsync(r,
			"file_deletion",
            "File Deletion Event",
            "/api/delete/"+token,
		    umamiData,
		  )
		if err := utils.ExecuteFileDeletion(client, &file); err != nil {
			http.Error(w, "Deletion failed", http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "success"})
	}
}

func CreateKeyHandler(client *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok || apiKey.Role != db.RoleOwner {
			http.Error(w, "Privileged administrative action required", http.StatusForbidden)
			return
		}
		var req struct {
			Owner     string   `json:"owner"`
			Role      db.UserRole `json:"role"`
			MaxSizeMB *float64 `json:"max_size_mb"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		if req.Owner == "" {
			http.Error(w, "Owner required", http.StatusBadRequest)
			return
		}
		if req.Role != db.RoleOwner && req.Role != db.RoleVIP && req.Role != db.RoleNormal {
			req.Role = db.RoleNormal
		}

		umamiData := umami.BuildUmamiData(r, apiKey.Owner,
            umami.WithNewOwner(req.Owner),
            umami.WithNewRole(req.Role),
            umami.WithCreatedBy(apiKey.Owner),
        )

        // 3. Dispara o rastreamento assíncrono
        umami.Instance.TrackEventAsync(r,
            "key_provision",
            "Key Provisioning Event",
            "/api/keys",
            umamiData,
        )

		key := "sk_" + utils.SecureRandomString(48)
		var maxSize *int64
		if req.MaxSizeMB != nil {
			mb := int64(*req.MaxSizeMB * 1024 * 1024)
			maxSize = &mb
		}
		newKey := db.APIKey{
			Key:           key,
			Owner:         req.Owner,
			Role:          req.Role,
			MaxUploadSize: maxSize,
		}
		if err := client.Create(&newKey).Error; err != nil {
			http.Error(w, "Failed to create key", http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"owner": newKey.Owner,
			"key":   newKey.Key,
			"role":  newKey.Role,
		})
	}
}

func GetUserMetricsHandler(client *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		days := r.URL.Query().Get("days")
		var daysInt int
		if days != "" {
			d, _ := strconv.Atoi(days)
			daysInt = d
		}
		filters, _ := utils.ParseTimeRange(from, to, daysInt)

		var keys []db.APIKey
		if err := client.Where("owner = ?", apiKey.Owner).Find(&keys).Error; err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		umamiData := umami.BuildUmamiData(r, apiKey.Owner)
		umami.Instance.TrackEventAsync(r,
            "metrics_view",
            "User Metrics View",
            "/api/metrics/user",
            umamiData,
        )

		if len(keys) == 0 {
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"owner": apiKey.Owner,
				"summary": map[string]interface{}{
					"total_uploads":          0,
					"active_files":           0,
					"deleted_files":          0,
					"current_bytes_used":     0,
					"historical_bytes_sent":  0,
					"average_file_size":      0,
					"first_upload":           nil,
					"last_upload":            nil,
				},
				"api_keys_breakdown": []interface{}{},
			})
			return
		}

		keyIDs := make([]uint, len(keys))
		for i, k := range keys {
			keyIDs[i] = k.ID
		}
		activeQuery := client.Model(&db.UploadedFile{}).Where("api_key_id IN ?", keyIDs)
		for cond, val := range filters {
			activeQuery = activeQuery.Where(cond, val)
		}
		var activeCount int64
		var activeSize int64
		activeQuery.Count(&activeCount)
		activeQuery.Select("COALESCE(SUM(file_size), 0)").Scan(&activeSize)

		var avgSize float64
		activeQuery.Select("COALESCE(AVG(file_size), 0)").Scan(&avgSize)

		var deletedLogs []db.DeletedFileLog
		logQuery := client.Model(&db.DeletedFileLog{})
		for cond, val := range filters {
			logQuery = logQuery.Where(cond, val)
		}
		if err := logQuery.Find(&deletedLogs).Error; err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		deletedCount := 0
		var deletedSize int64
		for _, log := range deletedLogs {
			var meta map[string]interface{}
			if err := json.Unmarshal([]byte(log.MetaJSON), &meta); err == nil {
				if owner, ok := meta["owner"]; ok && owner == apiKey.Owner {
					deletedCount++
					deletedSize += log.FileSize
				}
			}
		}

		var first, last db.UploadedFile
		client.Where("api_key_id IN ?", keyIDs).Order("uploaded_at ASC").First(&first)
		client.Where("api_key_id IN ?", keyIDs).Order("uploaded_at DESC").First(&last)

		var breakdown []struct {
			APIKeyID uint
			Count    int64
			Size     int64
		}
		client.Model(&db.UploadedFile{}).Where("api_key_id IN ?", keyIDs).Select("api_key_id, COUNT(*) as count, COALESCE(SUM(file_size), 0) as size").Group("api_key_id").Scan(&breakdown)
		statsMap := make(map[uint]struct{ Count int64; Size int64 })
		for _, item := range breakdown {
			statsMap[item.APIKeyID] = struct{ Count int64; Size int64 }{Count: item.Count, Size: item.Size}
		}

		keysBreakdown := make([]map[string]interface{}, len(keys))
		for i, k := range keys {
			stats := statsMap[k.ID]
			keysBreakdown[i] = map[string]interface{}{
				"key_prefix":   k.Key[:6] + "...",
				"role":         k.Role,
				"active_files": stats.Count,
				"bytes_used":   stats.Size,
			}
		}

		summary := map[string]interface{}{
			"total_uploads":          activeCount + int64(deletedCount),
			"active_files":           activeCount,
			"deleted_files":          deletedCount,
			"current_bytes_used":     activeSize,
			"historical_bytes_sent":  activeSize + deletedSize,
			"average_file_size":      avgSize,
			"first_upload":           nil,
			"last_upload":            nil,
		}
		if first.ID != "" {
			summary["first_upload"] = first.UploadedAt.Format(time.RFC3339)
		}
		if last.ID != "" {
			summary["last_upload"] = last.UploadedAt.Format(time.RFC3339)
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"owner":              apiKey.Owner,
			"summary":            summary,
			"api_keys_breakdown": keysBreakdown,
		})
	}
}

func GetAdminMetricsHandler(client *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok || apiKey.Role != db.RoleOwner {
			http.Error(w, "Privileged administrative access required", http.StatusForbidden)
			return
		}
		var totalFiles int64
		var totalSize int64
		client.Model(&db.UploadedFile{}).Count(&totalFiles)
		client.Model(&db.UploadedFile{}).Select("COALESCE(SUM(file_size), 0)").Scan(&totalSize)

		var deletedCount int64
		client.Model(&db.DeletedFileLog{}).Count(&deletedCount)

		umamiData := umami.BuildUmamiData(r, apiKey.Owner)

		umami.Instance.TrackEventAsync(r,
            "admin_metrics_view",
            "Admin Metrics View",
            "/api/metrics/admin",
            umamiData,
        )

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"general_use": map[string]interface{}{
				"total_active_files":         totalFiles,
				"total_historical_uploads":   totalFiles + deletedCount,
				"total_historical_deletions": deletedCount,
				"current_occupied_bytes":     totalSize,
			},
			"server_health_and_storage": map[string]interface{}{
				"database": "ok",
				"storage":  "ok",
			},
		})
	}
}

func RootHandler(w http.ResponseWriter, r *http.Request) {
	umami.Instance.TrackPageViewAsync(r, "Todo Root", "/")
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte("<html><body><h1>TODO</h1></body></html>"))
}

func SharexConfigHandler(client *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		baseURL := buildBaseURL(r)
		umamiData := umami.BuildUmamiData(r, apiKey.Owner)

		umami.Instance.TrackEventAsync(r,
            "sharex_config_generator",
            "Sharex Config Generator",
            "/sharex/config",
            umamiData,
        )

		config := map[string]interface{}{
			"Version":         "14.0.1",
			"Name":            fmt.Sprintf("Local File Server (%s)", apiKey.Owner),
			"DestinationType": "ImageUploader, FileUploader",
			"RequestMethod":   "POST",
			"RequestURL":      baseURL + "/api/upload",
			"Headers": map[string]string{
				"X-API-Key": apiKey.Key,
			},
			"Body":           "MultipartFormData",
			"FileFormName":   "file",
			"URL":            "{json:url}",
			"ThumbnailURL":   "{json:thumbnail_url}",
			"DeletionURL":    "{json:deletion_url}",
			"ErrorMessage":   "{json:error}",
		}
		respondJSON(w, http.StatusOK, config)
	}
}

func FaviconHandler(client *gorm.DB) http.HandlerFunc {
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

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
