package umami

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
	"strings"
	"fmt"
	"uploadserver/internal/config"
	"uploadserver/internal/db"
)

var Instance *Umami

type UmamiPayload struct {
	Payload struct {
		Website  string                 `json:"website"`
		URL      string                 `json:"url"`
		Title    string                 `json:"title,omitempty"`
		Name     string                 `json:"name,omitempty"`
		Hostname string                 `json:"hostname,omitempty"`
		Referrer string                 `json:"referrer,omitempty"`
		Data     map[string]interface{} `json:"data,omitempty"`
		IP       string 				`json:"ip,omitempty"`
	} `json:"payload"`
	Type string `json:"type"`
}

type Umami struct {
	apiEndpoint string
	websiteID   string
	hostname    string
	enabled     bool
	httpClient  *http.Client
}

func NewInstance() *Umami {
	if config.UmamiURLBase == "" || config.UmamiWebsiteID == "" || config.UmamiHostname == "" {
		log.Println("⚠️ Umami tracking disabled (missing config variables)")
		return &Umami{enabled: false}
	}

	Instance = &Umami{
		apiEndpoint: config.UmamiURLBase + "/api/send",
		websiteID:   config.UmamiWebsiteID,
		hostname:    config.UmamiHostname,
		enabled:     true,
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}
	return Instance
}

func (t *Umami) TrackPageViewAsync(r *http.Request, pageTitle, url string) {
	if !t.enabled {
		return
	}

	userAgent := getDeterministicUserAgent(r.Header.Get("User-Agent"), "")
	acceptLang := r.Header.Get("Accept-Language")
	referrer := r.Referer()

	cfMetadata, ok := getCloudflareFromContext(r.Context())
	var ipAddress string
	if ok {
	    ipAddress = cfMetadata.IP
	} else {
	    ipAddress = r.Header.Get("X-Forwarded-For")
	    if ipAddress == "" {
	        ipAddress = r.RemoteAddr
	    }
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var data UmamiPayload
		data.Type = "event"
		data.Payload.Website = t.websiteID
		data.Payload.URL = url
		data.Payload.Title = pageTitle
		data.Payload.Hostname = t.hostname
		data.Payload.Referrer = referrer
		data.Payload.IP = ipAddress

		jsonData, err := json.Marshal(data)
		if err != nil {
			log.Printf("❌ Umami marshal error: %v", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", t.apiEndpoint, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("❌ Umami request creation error: %v", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", acceptLang)
		req.Header.Set("X-Forwarded-For", ipAddress)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			log.Printf("❌ Umami send error: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("⚠️ Umami returned status: %d", resp.StatusCode)
		}
	}()
}

func (t *Umami) TrackEventAsync(r *http.Request, eventName, title, url string, customData map[string]interface{}) {
	if !t.enabled {
		return
	}

	owner, _ := customData["owner"].(string)
	userAgent := getDeterministicUserAgent(r.Header.Get("User-Agent"), owner)
	acceptLang := r.Header.Get("Accept-Language")
	referrer := r.Referer()

	cfMetadata, ok := getCloudflareFromContext(r.Context())
	var ipAddress string
	if ok {
	    ipAddress = cfMetadata.IP
	} else {
	    ipAddress = r.Header.Get("X-Forwarded-For")
	    if ipAddress == "" {
	        ipAddress = r.RemoteAddr
	    }
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var data UmamiPayload
		data.Type = "event"
		data.Payload.Website = t.websiteID
		data.Payload.URL = url
		data.Payload.Title = title
		data.Payload.Name = eventName
		data.Payload.Hostname = t.hostname
		data.Payload.Referrer = referrer
		data.Payload.Data = customData
		data.Payload.IP = ipAddress

		jsonData, err := json.Marshal(data)
		if err != nil {
			log.Printf("❌ Umami marshal error: %v", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", t.apiEndpoint, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("❌ Umami request creation error: %v", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", acceptLang)
		req.Header.Set("X-Forwarded-For", ipAddress)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			log.Printf("❌ Umami send error: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("⚠️ Umami returned status: %d", resp.StatusCode)
		}
	}()
}

type UmamiDataOption func(map[string]interface{})

func BuildUmamiData(r *http.Request, owner string, opts ...UmamiDataOption) map[string]interface{} {
	data := make(map[string]interface{})

	if owner != "" {
		data["owner"] = owner
	}
	if clientApp := r.Header.Get("User-Agent"); clientApp != "" {
		data["client_app"] = clientApp
	}

	for _, opt := range opts {
		opt(data)
	}

	return data
}

func WithUploadMeta(r *http.Request) UmamiDataOption {
	return func(data map[string]interface{}) {
		if scope := r.Header.Get("X-Name"); scope != "" {
			data["scope"] = scope
		}
		if tags := r.URL.Query().Get("tags"); tags != "" {
			data["tags"] = tags
		}
	}
}

func WithFilename(name string) UmamiDataOption {
	return func(data map[string]interface{}) {
		if name != "" {
			data["filename"] = name
		}
	}
}

func WithUploadedAt(t time.Time) UmamiDataOption {
	return func(data map[string]interface{}) {
		if !t.IsZero() {
			data["uploaded_at"] = t.Format(time.RFC3339)
		}
	}
}

func WithNewOwner(owner string) UmamiDataOption {
	return func(data map[string]interface{}) {
		if owner != "" {
			data["new_owner"] = owner
		}
	}
}

func WithNewRole(role db.UserRole) UmamiDataOption {
	return func(data map[string]interface{}) {
		if role != "" {
			data["new_role"] = string(role)
		}
	}
}

func WithCreatedBy(creator string) UmamiDataOption {
	return func(data map[string]interface{}) {
		if creator != "" {
			data["created_by"] = creator
		}
	}
}

func getDeterministicUserAgent(userAgent, owner string) string {
	uaLower := strings.ToLower(userAgent)

	if userAgent != "" &&
	   !strings.Contains(uaLower, "curl") &&
	   !strings.Contains(uaLower, "postman") &&
	   !strings.Contains(uaLower, "go-http-client") {
		return userAgent
	}

	templates := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/12%d.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/1%d.0 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%d.0) Gecko/20100101 Firefox/%d.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/12%d.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/12%d.0.0.0 Safari/537.36 Edge/12%d.0.0.0",
	}

	sum := 0
	for _, r := range owner {
		sum += int(r)
	}
	if sum == 0 {
		sum = 42
	}

	templateIdx := sum % len(templates)
	versionModifier := (sum % 8)

	switch templateIdx {
	case 0:
		return fmt.Sprintf(templates[0], 0+versionModifier)
	case 1:
		return fmt.Sprintf(templates[1], 5+versionModifier)
	case 2:
		return fmt.Sprintf(templates[2], 20+versionModifier, 20+versionModifier)
	case 3:
		return fmt.Sprintf(templates[3], 0+versionModifier)
	default:
		return fmt.Sprintf(templates[4], 0+versionModifier, 0+versionModifier)
	}
}

type contextKey string

const (
	cloudflareKey    contextKey = "cloudflare"
)

type CloudflareMetadata struct {
	IP      string
	Country string
	City    string
	UA      string
}

func getCloudflareFromContext(ctx context.Context) (CloudflareMetadata, bool) {
	val := ctx.Value(cloudflareKey)
	if val == nil {
		return CloudflareMetadata{}, false
	}
	cf, ok := val.(CloudflareMetadata)
	return cf, ok
}
