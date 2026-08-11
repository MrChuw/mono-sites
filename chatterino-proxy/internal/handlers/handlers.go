// Package handlers
package handlers

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"proxy/internal/config"
	"proxy/internal/utils"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type Handler struct {
	Config *config.Config
	Client *http.Client
}

func NewHandler(cfg *config.Config, client *http.Client) *Handler {
	return &Handler{
		Config: cfg,
		Client: client,
	}
}

type raceResult struct {
	upstreamURL string
	resp        *http.Response
	err         error
}

const upstreamQueryParam = "chatterino-proxy-upstream"

func buildTargetURL(baseURL, suffix string) string {
	cleanSuffix := strings.TrimPrefix(suffix, "/")
	if strings.Contains(baseURL, "%1") {
		return strings.ReplaceAll(baseURL, "%1", cleanSuffix)
	}
	base := strings.TrimSuffix(baseURL, "/")
	if cleanSuffix == "" {
		return base
	}
	return base + "/" + cleanSuffix
}

func copyHeaders(src, dst http.Header, userAgent string) {
	for k, vv := range src {
		lowerK := strings.ToLower(k)

		if utils.HopByHopHeaders[lowerK] || strings.EqualFold(k, "User-Agent") {
			continue
		}

		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	dst.Set("User-Agent", userAgent)
}

func writeResponse(w http.ResponseWriter, resp *http.Response, upstreamURL string, startTime time.Time) {
	defer resp.Body.Close()

	elapsedDuration := time.Since(startTime)
	elapsedMs := float64(elapsedDuration.Microseconds()) / 1000.0
	elapsedSec := elapsedDuration.Seconds()

	encoding := strings.ToLower(resp.Header.Get("Content-Encoding"))

	for k, vv := range resp.Header {
		lowerK := strings.ToLower(k)
		if utils.HopByHopHeaders[lowerK] || lowerK == "content-length" || lowerK == "transfer-encoding" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.Header().Set("X-Proxy-Instance", upstreamURL)
	w.Header().Set("X-Proxy-Time", fmt.Sprintf("%.2fms", elapsedMs))

	var reader io.Reader = resp.Body

	switch encoding {
	case "gzip":
		gzReader, gzErr := gzip.NewReader(resp.Body)
		if gzErr == nil {
			defer gzReader.Close()
			reader = gzReader
		}
	case "zstd":
		zReader, zErr := zstd.NewReader(resp.Body)
		if zErr == nil {
			defer zReader.Close()
			reader = zReader
		}
	case "br":
		reader = brotli.NewReader(resp.Body)
	}

	respBody, err := io.ReadAll(reader)
	if err != nil {
		w.WriteHeader(resp.StatusCode)
		return
	}

	var jsonMap map[string]interface{}
	if jsonErr := json.Unmarshal(respBody, &jsonMap); jsonErr == nil {
		jsonMap["proxy_instance"] = upstreamURL
		jsonMap["proxy_elapsed"] = map[string]interface{}{
			"ms": elapsedMs,
			"s":  elapsedSec,
		}

		buf := &bytes.Buffer{}
		encoder := json.NewEncoder(buf)
		encoder.SetEscapeHTML(false)

		if marshalErr := encoder.Encode(jsonMap); marshalErr == nil {
			modifiedJSON := buf.Bytes()

			var compressedBuf bytes.Buffer
			var compressErr error

			switch encoding {
			case "gzip":
				gzWriter := gzip.NewWriter(&compressedBuf)
				_, compressErr = gzWriter.Write(modifiedJSON)
				_ = gzWriter.Close()
			case "zstd":
				zWriter, _ := zstd.NewWriter(&compressedBuf)
				_, compressErr = zWriter.Write(modifiedJSON)
				_ = zWriter.Close()
			case "br":
				brWriter := brotli.NewWriter(&compressedBuf)
				_, compressErr = brWriter.Write(modifiedJSON)
				_ = brWriter.Close()
			default:
				compressedBuf.Write(modifiedJSON)
			}

			if compressErr == nil {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(resp.StatusCode)
				w.Write(compressedBuf.Bytes())
				return
			}
		}
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func buildSuffixAndGetUpstream(r *http.Request, servicePrefix string) (suffix string, upstreamParam string) {
	rawPath := strings.TrimPrefix(r.URL.EscapedPath(), servicePrefix)
	rawPath = strings.TrimPrefix(rawPath, "/")

	query := r.URL.Query()
	upstreamParam = query.Get(upstreamQueryParam)
	query.Del(upstreamQueryParam)
	newQuery := query.Encode()

	suffix = "/" + rawPath
	if newQuery != "" {
		suffix += "?" + newQuery
	}
	return
}

func selectUpstreams(cfg *config.ServiceConfig, upstreamParam string) (selected []*config.Upstream, isAll bool) {
	if upstreamParam == "" || upstreamParam == "all" {
		return cfg.Upstreams, true
	}

	pathMap := make(map[string]*config.Upstream)
	for _, u := range cfg.Upstreams {
		if u.Path != "" {
			pathMap[u.Path] = u
		}
	}

	var result []*config.Upstream
	parts := strings.Split(upstreamParam, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if u, ok := pathMap[p]; ok {
			result = append(result, u)
		} else {
			log.Printf("Unknown upstream '%s' in parameter, using all", p)
			return cfg.Upstreams, true
		}
	}
	return result, false
}

func parseBypassParams(r *http.Request) map[string][]string {
	bypassMap := make(map[string][]string)

	if val := r.URL.Query().Get("chatterino-proxy-bypass"); val != "" {
		parseBypassValue(val, bypassMap)
	}

	for key, vals := range r.URL.Query() {
		if strings.HasPrefix(key, "chatterino-proxy-bypass") && key != "chatterino-proxy-bypass" {
			for _, val := range vals {
				parseBypassValue(val, bypassMap)
			}
		}
	}

	return bypassMap
}

func parseBypassValue(val string, m map[string][]string) {
	parts := strings.Split(val, ",")
	for i := 0; i+1 < len(parts); i += 2 {
		ident := strings.ToLower(strings.TrimSpace(parts[i]))
		upstream := strings.TrimSpace(parts[i+1])
		if ident != "" && upstream != "" {
			m[ident] = append(m[ident], upstream)
		}
	}
}

func filterBypassed(upstreams []*config.Upstream, bypassList []string) []*config.Upstream {
	filtered := make([]*config.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if !slices.Contains(bypassList, u.Path) {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

func filterUnsupported(upstreams []*config.Upstream, identifier string) []*config.Upstream {
	if identifier == "" {
		return upstreams
	}
	filtered := make([]*config.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		skip := false
		for _, uns := range u.Unsupported {
			if strings.EqualFold(uns, identifier) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

func filterUnsupportedHostname(upstreams []*config.Upstream, hostname string) []*config.Upstream {
	if hostname == "" {
		return upstreams
	}
	lowerHost := strings.ToLower(hostname)
	filtered := make([]*config.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		skip := false
		for _, uns := range u.Unsupported {
			if uns == "" {
				continue
			}
			if strings.Contains(lowerHost, strings.ToLower(uns)) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

func parseReplaceParams(r *http.Request) map[string]map[string]string {
	replaceMap := make(map[string]map[string]string)

	if val := r.URL.Query().Get("chatterino-proxy-replace"); val != "" {
		parseReplaceValue(val, replaceMap)
	}

	for key, vals := range r.URL.Query() {
		if strings.HasPrefix(key, "chatterino-proxy-replace") && key != "chatterino-proxy-replace" {
			for _, val := range vals {
				parseReplaceValue(val, replaceMap)
			}
		}
	}

	return replaceMap
}

func mergeReplaceMaps(static, dynamic map[string]map[string]string) map[string]map[string]string {
	merged := make(map[string]map[string]string)
	for domain, rules := range static {
		merged[domain] = make(map[string]string, len(rules))
		for find, repl := range rules {
			merged[domain][find] = repl
		}
	}
	for domain, rules := range dynamic {
		if _, ok := merged[domain]; !ok {
			merged[domain] = make(map[string]string, len(rules))
		}
		for find, repl := range rules {
			merged[domain][find] = repl
		}
	}
	return merged
}

func parseReplaceValue(val string, m map[string]map[string]string) {
	parts := strings.Split(val, ",")
	for i := 0; i+2 < len(parts); i += 3 {
		domain := strings.TrimSpace(parts[i])
		find := strings.TrimSpace(parts[i+1])
		repl := strings.TrimSpace(parts[i+2])
		if domain != "" && find != "" {
			if _, ok := m[domain]; !ok {
				m[domain] = make(map[string]string)
			}
			m[domain][find] = repl
		}
	}
}

func applyReplacesOnURL(rawURL string, replaces map[string]map[string]string) string {
	if len(replaces) == 0 {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := parsed.Hostname()
	var bestDomain string
	for domain := range replaces {
		if strings.Contains(host, domain) || strings.Contains(host, "."+domain) || host == domain {
			if len(domain) > len(bestDomain) {
				bestDomain = domain
			}
		}
	}
	if bestDomain != "" {
		for find, repl := range replaces[bestDomain] {
			parsed.Path = strings.ReplaceAll(parsed.Path, find, repl)
		}
	}
	return parsed.String()
}

func removeProxyParams(query url.Values) url.Values {
	newQuery := url.Values{}
	for key, values := range query {
		if !strings.HasPrefix(key, "chatterino-proxy-") {
			newQuery[key] = values
		}
	}
	return newQuery
}
