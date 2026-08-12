package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"proxy/internal/config"
	"proxy/internal/utils"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

var sizeLimitRegex = regexp.MustCompile(`(?i)too large\s*\(>\s*(\d+)\s*MB\)`)

type linkRaceResult struct {
	upstreamURL string
	statusCode  int
	header      http.Header
	body        []byte
	err         error
}

type linkCacheEntry struct {
	mu        sync.RWMutex
	expiresAt time.Time
	upstreams map[string]*linkRaceResult
}

var globalLinkCache sync.Map

func buildCacheKey(originalURL string, r *http.Request) string {
	if originalURL == "" {
		return ""
	}

	cleanQuery := removeProxyParams(r.URL.Query())
	sortedParams := cleanQuery.Encode()

	if sortedParams != "" {
		return originalURL + "|" + sortedParams
	}

	return originalURL
}

func getLinkCacheEntry(cacheKey string) *linkCacheEntry {
	if cacheKey == "" {
		return nil
	}
	v, ok := globalLinkCache.Load(cacheKey)
	if !ok {
		entry := &linkCacheEntry{
			expiresAt: time.Now().Add(1 * time.Minute),
			upstreams: make(map[string]*linkRaceResult),
		}
		v, _ = globalLinkCache.LoadOrStore(cacheKey, entry)
		return v.(*linkCacheEntry)
	}
	entry := v.(*linkCacheEntry)

	entry.mu.RLock()
	expired := time.Now().After(entry.expiresAt)
	entry.mu.RUnlock()

	if expired {
		entry.mu.Lock()
		if time.Now().After(entry.expiresAt) {
			entry.upstreams = make(map[string]*linkRaceResult)
			entry.expiresAt = time.Now().Add(1 * time.Minute)
		}
		entry.mu.Unlock()
	}
	return entry
}

func cacheResult(cacheKey string, res *linkRaceResult) {
	if cacheKey == "" || res == nil || res.body == nil {
		return
	}
	entry := getLinkCacheEntry(cacheKey)
	if entry != nil {
		entry.mu.Lock()
		entry.upstreams[res.upstreamURL] = res
		entry.mu.Unlock()
	}
}

func (h *Handler) LinkResolverHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	service := "link_resolver"

	services := h.Config.GetServices()
	serviceCfg, exists := services[service]
	if !exists || len(serviceCfg.Upstreams) == 0 {
		log.Printf("[PROXY] Service '%s' not found | Method: %s", service, r.Method)
		http.Error(w, fmt.Sprintf("Service '%s' not found", service), http.StatusNotFound)
		return
	}

	originalURLParam := r.URL.Query().Get("url")
	pathEmbedded := originalURLParam == ""
	if pathEmbedded {
		rawPath := strings.TrimPrefix(r.URL.EscapedPath(), "/link_resolver")
		rawPath = strings.TrimPrefix(rawPath, "/")
		originalURLParam = rawPath
	}

	var hostname string
	var originalDecodedURL string
	if originalURLParam != "" {
		decoded, err := url.QueryUnescape(originalURLParam)
		if err == nil {
			originalDecodedURL = decoded
			if parsed, err := url.Parse(decoded); err == nil && parsed.Host != "" {
				hostname = parsed.Hostname()
			}
		}
	}

	fallbackEnabled := isFallbackEnabled(r)

	suffix, upstreamParam := buildSuffixAndGetUpstream(r, "/link_resolver")
	candidateUpstreams, isAll := selectUpstreams(serviceCfg, upstreamParam)

	if hostname != "" {
		candidateUpstreams = filterUnsupportedHostname(candidateUpstreams, hostname)
		if len(candidateUpstreams) == 0 {
			candidateUpstreams = filterUnsupportedHostname(serviceCfg.Upstreams, hostname)
			isAll = true
		}
		if len(candidateUpstreams) == 0 {
			log.Printf("[PROXY] All upstreams unsupported for hostname '%s'", hostname)
			http.Error(w, fmt.Sprintf("No upstream supports domain '%s'", hostname), http.StatusBadRequest)
			return
		}
	}

	var bypassList []string
	if hostname != "" {
		bypassMap := parseBypassParams(r)
		bypassList = bypassMap[strings.ToLower(hostname)]
		if len(bypassList) > 0 {
			filtered := filterBypassed(candidateUpstreams, bypassList)

			if len(filtered) == 0 && upstreamParam != "" {
				log.Printf("[PROXY] Requested upstream(s) '%s' bypassed for hostname '%s', falling back to all", upstreamParam, hostname)
				allUpstreams := filterUnsupportedHostname(serviceCfg.Upstreams, hostname)
				filtered = filterBypassed(allUpstreams, bypassList)
				isAll = true
			}

			if len(filtered) == 0 {
				log.Printf("[PROXY] All upstreams bypassed for hostname '%s'", hostname)
				http.Error(w, fmt.Sprintf("All upstreams bypassed for domain '%s'", hostname), http.StatusBadRequest)
				return
			}
			candidateUpstreams = filtered
		}
	}

	var healthyCandidates []*config.Upstream
	for _, u := range candidateUpstreams {
		if u.IsHealthy() {
			healthyCandidates = append(healthyCandidates, u)
		}
	}

	if len(healthyCandidates) == 0 && !isAll && fallbackEnabled {
		log.Printf("[PROXY] Requested upstream(s) '%s' offline, chatterino-proxy-fallback enabled, falling back to all", upstreamParam)
		fallbackCandidates := filterUnsupportedHostname(serviceCfg.Upstreams, hostname)
		if len(bypassList) > 0 {
			fallbackCandidates = filterBypassed(fallbackCandidates, bypassList)
		}
		for _, u := range fallbackCandidates {
			if u.IsHealthy() {
				healthyCandidates = append(healthyCandidates, u)
			}
		}
		isAll = true
	}
	if len(healthyCandidates) == 0 {
		log.Printf("[PROXY] No healthy upstreams for '%s'", service)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status":  500,
			"message": fmt.Sprintf("No healthy upstreams available for '%s'", service),
		})
		return
	}

	explicitAttempt := !isAll
	if !serviceCfg.Race && isAll {
		sort.SliceStable(healthyCandidates, func(i, j int) bool {
			return healthyCandidates[i].GetLatency() < healthyCandidates[j].GetLatency()
		})
	}

	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "Error reading request body", http.StatusBadRequest)
			return
		}
	}

	var success bool
	var lastError string
	var lastAppResp *linkRaceResult

	cacheKey := buildCacheKey(originalDecodedURL, r)
	cacheEntry := getLinkCacheEntry(cacheKey)
	var candidatesToFetch []*config.Upstream

	if cacheEntry != nil {
		cacheEntry.mu.RLock()
		for _, u := range healthyCandidates {
			if res, ok := cacheEntry.upstreams[u.URL]; ok {
				if ok, _ := isLinkResolverSuccess(res.statusCode, res.body); ok {
					cacheEntry.mu.RUnlock()
					log.Printf("[CACHE-WIN] Service: %s | Time: %v | Winner: %s", service, time.Since(startTime), res.upstreamURL)
					writeLinkResolverResponse(w, res.header, res.statusCode, res.body, res.upstreamURL, startTime, originalDecodedURL)
					return
				}
				lastAppResp = pickBetterErrorResp(lastAppResp, res)
			} else {
				candidatesToFetch = append(candidatesToFetch, u)
			}
		}
		cacheEntry.mu.RUnlock()
	} else {
		candidatesToFetch = healthyCandidates
	}

	if len(candidatesToFetch) > 0 {
		var fetchAppResp *linkRaceResult
		if serviceCfg.Race {
			success, lastError, fetchAppResp = h.tryLinkResolverRace(w, r, candidatesToFetch, suffix, bodyBytes, originalURLParam, pathEmbedded, originalDecodedURL, cacheKey, service, startTime)
		} else {
			success, lastError, fetchAppResp = h.tryLinkResolverSequential(w, r, candidatesToFetch, suffix, bodyBytes, originalURLParam, pathEmbedded, originalDecodedURL, cacheKey, service, startTime)
		}
		if success {
			return
		}
		if fetchAppResp != nil {
			lastAppResp = pickBetterErrorResp(lastAppResp, fetchAppResp)
		}
	} else {
		lastError = "all requested candidates were cached as failures"
	}

	if explicitAttempt && fallbackEnabled {
		fallbackCandidates := filterUnsupportedHostname(serviceCfg.Upstreams, hostname)
		if len(bypassList) > 0 {
			fallbackCandidates = filterBypassed(fallbackCandidates, bypassList)
		}
		fallbackCandidates = excludeUpstreams(fallbackCandidates, healthyCandidates)

		var fallbackHealthy []*config.Upstream
		for _, u := range fallbackCandidates {
			if u.IsHealthy() {
				fallbackHealthy = append(fallbackHealthy, u)
			}
		}

		if len(fallbackHealthy) > 0 {
			var fallbackCandidatesToFetch []*config.Upstream

			if cacheEntry != nil {
				cacheEntry.mu.RLock()
				for _, u := range fallbackHealthy {
					if res, ok := cacheEntry.upstreams[u.URL]; ok {
						if ok, _ := isLinkResolverSuccess(res.statusCode, res.body); ok {
							cacheEntry.mu.RUnlock()
							log.Printf("[CACHE-FALLBACK-WIN] Service: %s | Time: %v | Winner: %s", service, time.Since(startTime), res.upstreamURL)
							writeLinkResolverResponse(w, res.header, res.statusCode, res.body, res.upstreamURL, startTime, originalDecodedURL)
							return
						}
						lastAppResp = pickBetterErrorResp(lastAppResp, res)
					} else {
						fallbackCandidatesToFetch = append(fallbackCandidatesToFetch, u)
					}
				}
				cacheEntry.mu.RUnlock()
			} else {
				fallbackCandidatesToFetch = fallbackHealthy
			}

			if len(fallbackCandidatesToFetch) > 0 {
				log.Printf("[PROXY] Requested upstream(s) '%s' failed for '%s' (%s), chatterino-proxy-fallback enabled, racing remaining upstreams", upstreamParam, service, lastError)
				var fbAppResp *linkRaceResult
				success, lastError, fbAppResp = h.tryLinkResolverRace(w, r, fallbackCandidatesToFetch, suffix, bodyBytes, originalURLParam, pathEmbedded, originalDecodedURL, cacheKey, service, startTime)
				if success {
					return
				}
				if fbAppResp != nil {
					lastAppResp = pickBetterErrorResp(lastAppResp, fbAppResp)
				}
			}
		}
	}

	if lastAppResp != nil && lastAppResp.body != nil {
		log.Printf("[PROXY-FAIL] All upstreams failed for '%s' | Returning last upstream application error | Last Error: %s", service, lastError)
		writeLinkResolverResponse(w, lastAppResp.header, lastAppResp.statusCode, lastAppResp.body, lastAppResp.upstreamURL, startTime, originalDecodedURL)
		return
	}

	log.Printf("[PROXY-FAIL] All upstreams failed for '%s' | Last Error: %s", service, lastError)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"status":  500,
		"message": fmt.Sprintf("All upstreams for '%s' failed. Last error: %s", service, lastError),
	})
}

func buildTargetWithReplaces(upstream *config.Upstream, originalURLParam, suffix string, pathEmbedded bool, r *http.Request) string {
	decodedURL, err := url.QueryUnescape(originalURLParam)
	if err != nil {
		decodedURL = originalURLParam
	}

	replaceMap := parseReplaceParams(r)
	mergedReplace := mergeReplaceMaps(upstream.Replace, replaceMap)
	if len(mergedReplace) > 0 {
		decodedURL = applyReplacesOnURL(decodedURL, mergedReplace)
	}

	encodedURL := url.QueryEscape(decodedURL)
	cleanQuery := removeProxyParams(r.URL.Query())

	if pathEmbedded {
		newQuery := cleanQuery.Encode()
		newSuffix := "/" + encodedURL
		if newQuery != "" {
			newSuffix += "?" + newQuery
		}
		return buildTargetURL(upstream.URL, newSuffix)
	}

	cleanQuery.Set("url", encodedURL)
	newQuery := cleanQuery.Encode()

	parsedSuffix, err := url.Parse(suffix)
	if err != nil {
		return buildTargetURL(upstream.URL, suffix)
	}
	parsedSuffix.RawQuery = newQuery
	newSuffix := parsedSuffix.String()
	return buildTargetURL(upstream.URL, newSuffix)
}

func extractDecompressedBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	encoding := strings.ToLower(resp.Header.Get("Content-Encoding"))

	var reader io.Reader = resp.Body
	switch encoding {
	case "gzip":
		if gzReader, err := gzip.NewReader(resp.Body); err == nil {
			defer gzReader.Close()
			reader = gzReader
		}
	case "zstd":
		if zReader, err := zstd.NewReader(resp.Body); err == nil {
			defer zReader.Close()
			reader = zReader
		}
	case "br":
		reader = brotli.NewReader(resp.Body)
	}

	const maxResponseSize = 50 << 20

	body, err := io.ReadAll(io.LimitReader(reader, maxResponseSize+1))
	if err != nil {
		return nil, err
	}

	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("response too large (>10MB)")
	}

	return body, nil
}

func isLinkResolverSuccess(statusCode int, body []byte) (bool, string) {
	if statusCode >= 400 {
		return false, fmt.Sprintf("status %d", statusCode)
	}

	var jsonMap map[string]any
	if err := json.Unmarshal(body, &jsonMap); err == nil {
		if rawStatus, ok := jsonMap["status"]; ok {
			if statusNum, ok := rawStatus.(float64); ok && statusNum >= 400 {
				if msg, ok := jsonMap["message"].(string); ok && msg != "" {
					return false, fmt.Sprintf("status %d: %s", int(statusNum), msg)
				}
				return false, fmt.Sprintf("status %d", int(statusNum))
			}
		}
	}

	return true, ""
}

func (h *Handler) tryLinkResolverSequential(w http.ResponseWriter, r *http.Request, upstreams []*config.Upstream, suffix string, bodyBytes []byte, originalURLParam string, pathEmbedded bool, originalDecodedURL string, cacheKey string, service string, startTime time.Time) (bool, string, *linkRaceResult) {
	var lastError string
	var lastAppResp *linkRaceResult

	for _, upstream := range upstreams {
		target := buildTargetWithReplaces(upstream, originalURLParam, suffix, pathEmbedded, r)
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
		if err != nil {
			lastError = fmt.Sprintf("invalid request (%s)", upstream.URL)
			continue
		}
		copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)
		resp, err := h.Client.Do(outReq)
		if err != nil {
			lastError = fmt.Sprintf("connection error (%s)", upstream.URL)
			continue
		}

		respBody, readErr := extractDecompressedBody(resp)
		if readErr != nil {
			lastError = fmt.Sprintf("read error (%s)", upstream.URL)
			continue
		}

		if ok, failReason := isLinkResolverSuccess(resp.StatusCode, respBody); ok {
			res := &linkRaceResult{upstreamURL: upstream.URL, statusCode: resp.StatusCode, header: resp.Header, body: respBody}
			cacheResult(cacheKey, res)

			log.Printf("[PROXY] Service: %s | Status: %d | Time: %v | Upstream: %s", service, resp.StatusCode, time.Since(startTime), upstream.URL)
			writeLinkResolverResponse(w, resp.Header, resp.StatusCode, respBody, upstream.URL, startTime, originalDecodedURL)
			return true, "", nil
		} else {
			lastError = fmt.Sprintf("%s (%s)", failReason, upstream.URL)

			currentAppResp := &linkRaceResult{
				upstreamURL: upstream.URL,
				statusCode:  resp.StatusCode,
				header:      resp.Header,
				body:        respBody,
			}
			cacheResult(cacheKey, currentAppResp)
			lastAppResp = pickBetterErrorResp(lastAppResp, currentAppResp)
		}
	}
	return false, lastError, lastAppResp
}

func (h *Handler) tryLinkResolverRace(w http.ResponseWriter, r *http.Request, upstreams []*config.Upstream, suffix string, bodyBytes []byte, originalURLParam string, pathEmbedded bool, originalDecodedURL string, cacheKey string, service string, startTime time.Time) (bool, string, *linkRaceResult) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	results := make(chan linkRaceResult, len(upstreams))
	var wg sync.WaitGroup

	for _, upstream := range upstreams {
		wg.Add(1)
		go func(u *config.Upstream) {
			defer wg.Done()
			target := buildTargetWithReplaces(u, originalURLParam, suffix, pathEmbedded, r)

			var bodyReader io.Reader
			if len(bodyBytes) > 0 {
				bodyReader = strings.NewReader(string(bodyBytes))
			}

			outReq, err := http.NewRequestWithContext(ctx, r.Method, target, bodyReader)
			if err != nil {
				results <- linkRaceResult{err: fmt.Errorf("invalid request (%s)", u.URL)}
				return
			}

			copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)

			resp, err := h.Client.Do(outReq)
			if err != nil {
				results <- linkRaceResult{err: fmt.Errorf("connection error (%s)", u.URL)}
				return
			}

			respBody, readErr := extractDecompressedBody(resp)
			if readErr != nil {
				results <- linkRaceResult{err: fmt.Errorf("read error (%s)", u.URL)}
				return
			}

			if ok, failReason := isLinkResolverSuccess(resp.StatusCode, respBody); ok {
				res := linkRaceResult{upstreamURL: u.URL, statusCode: resp.StatusCode, header: resp.Header, body: respBody}
				cacheResult(cacheKey, &res)
				results <- res
			} else {
				res := linkRaceResult{err: fmt.Errorf("%s (%s)", failReason, u.URL), upstreamURL: u.URL, statusCode: resp.StatusCode, header: resp.Header, body: respBody}
				cacheResult(cacheKey, &res)
				results <- res
			}
		}(upstream)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var lastErr error
	var lastAppResp *linkRaceResult

	for res := range results {
		if res.err == nil {
			log.Printf("[RACE-WIN] Service: %s | Status: %d | Time: %v | Winner: %s", service, res.statusCode, time.Since(startTime), res.upstreamURL)
			writeLinkResolverResponse(w, res.header, res.statusCode, res.body, res.upstreamURL, startTime, originalDecodedURL)
			return true, "", nil
		}
		lastErr = res.err

		if res.body != nil {
			resCopy := res
			lastAppResp = pickBetterErrorResp(lastAppResp, &resCopy)
		}
	}

	if lastErr != nil {
		return false, lastErr.Error(), lastAppResp
	}
	return false, "", lastAppResp
}

func isFallbackEnabled(r *http.Request) bool {
	val := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("chatterino-proxy-fallback")))
	return val == "true" || val == "1" || val == "yes"
}

func excludeUpstreams(upstreams []*config.Upstream, exclude []*config.Upstream) []*config.Upstream {
	if len(exclude) == 0 {
		return upstreams
	}
	skip := make(map[*config.Upstream]bool, len(exclude))
	for _, u := range exclude {
		skip[u] = true
	}
	filtered := make([]*config.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if !skip[u] {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

func writeLinkResolverResponse(w http.ResponseWriter, header http.Header, statusCode int, body []byte, upstreamURL string, startTime time.Time, originalURL string) {
	elapsedDuration := time.Since(startTime)
	elapsedMs := float64(elapsedDuration.Microseconds()) / 1000.0
	elapsedSec := elapsedDuration.Seconds()

	for k, vv := range header {
		lowerK := strings.ToLower(k)
		if utils.HopByHopHeaders[lowerK] || lowerK == "content-length" || lowerK == "transfer-encoding" || lowerK == "content-encoding" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.Header().Set("X-Proxy-Instance", upstreamURL)
	w.Header().Set("X-Proxy-Time", fmt.Sprintf("%.2fms", elapsedMs))

	var jsonMap map[string]any
	if jsonErr := json.Unmarshal(body, &jsonMap); jsonErr == nil {
		if originalURL != "" {
			if _, hasLink := jsonMap["link"]; hasLink {
				jsonMap["link"] = originalURL
			}
		}

		jsonMap["proxy_instance"] = upstreamURL
		jsonMap["proxy_elapsed"] = map[string]any{
			"ms": elapsedMs,
			"s":  elapsedSec,
		}

		buf := &bytes.Buffer{}
		encoder := json.NewEncoder(buf)
		encoder.SetEscapeHTML(false)

		if marshalErr := encoder.Encode(jsonMap); marshalErr == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(statusCode)
			w.Write(buf.Bytes())
			return
		}
	}

	w.WriteHeader(statusCode)
	w.Write(body)
}

func extractSizeLimitFromBody(body []byte) int {
	if len(body) == 0 {
		return -1
	}
	var jsonMap map[string]any
	if err := json.Unmarshal(body, &jsonMap); err == nil {
		if msg, ok := jsonMap["message"].(string); ok {
			matches := sizeLimitRegex.FindStringSubmatch(msg)
			if len(matches) == 2 {
				if val, err := strconv.Atoi(matches[1]); err == nil {
					return val
				}
			}
		}
	}
	return -1
}

func pickBetterErrorResp(a, b *linkRaceResult) *linkRaceResult {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	sizeA := extractSizeLimitFromBody(a.body)
	sizeB := extractSizeLimitFromBody(b.body)

	if sizeA > -1 || sizeB > -1 {
		if sizeA > sizeB {
			return a
		}
		if sizeB > sizeA {
			return b
		}
	}
	return b
}
