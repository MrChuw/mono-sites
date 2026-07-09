package handlers

import (
	"fmt"
	"net/http"

	"github.com/uptrace/bun"

	"uploadserver/internal/umami"
)

func SharexConfigHandler(client *bun.DB) http.HandlerFunc {
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

		config := map[string]any{
			"Version":         "14.0.1",
			"Name":            fmt.Sprintf("Local File Server (%s)", apiKey.Owner),
			"DestinationType": "ImageUploader, FileUploader",
			"RequestMethod":   "POST",
			"RequestURL":      baseURL + "/api/upload",
			"Headers": map[string]string{
				"X-API-Key": apiKey.Key,
			},
			"Body":         "MultipartFormData",
			"FileFormName": "file",
			"URL":          "{json:url}",
			"ThumbnailURL": "{json:thumbnail_url}",
			"DeletionURL":  "{json:deletion_url}",
			"ErrorMessage": "{json:error}",
		}

		respondJSON(w, http.StatusOK, config)
	}
}
