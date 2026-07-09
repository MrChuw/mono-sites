package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func CreateKeyHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok || apiKey.Role != db.RoleOwner {
			http.Error(w, "Privileged administrative action required", http.StatusForbidden)
			return
		}

		var req struct {
			Owner     string      `json:"owner"`
			Role      db.UserRole `json:"role"`
			MaxSizeMB *float64    `json:"max_size_mb"`
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

		newKey, err := db.CreateAndInsertAPIKey(r.Context(), client, key, req.Owner, req.Role, maxSize)
		if err != nil {
			http.Error(w, "Failed to create key", http.StatusInternalServerError)
			return
		}

		respondJSON(w, http.StatusOK, map[string]any{
			"owner": newKey.Owner,
			"key":   newKey.Key,
			"role":  newKey.Role,
		})
	}
}
