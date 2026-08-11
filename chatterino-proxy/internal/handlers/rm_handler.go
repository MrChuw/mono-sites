package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"proxy/internal/config"
)

func (h *Handler) RMHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	service := "rm"

	services := h.Config.GetServices()
	serviceCfg, exists := services[service]
	if !exists || len(serviceCfg.Upstreams) == 0 {
		log.Printf("[PROXY] Service '%s' not found | Method: %s", service, r.Method)
		http.Error(w, fmt.Sprintf("Service '%s' not found", service), http.StatusNotFound)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/rm")
	path = strings.TrimPrefix(path, "/")
	segments := strings.Split(path, "/")
	username := ""
	if len(segments) > 0 && segments[0] != "" {
		username = strings.ToLower(segments[0])
	}

	rawPath := strings.TrimPrefix(r.URL.EscapedPath(), "/rm")
	rawPath = strings.TrimPrefix(rawPath, "/")
	segments2 := strings.Split(rawPath, "/")
	for i, seg := range segments2 {
		if seg != "" && !strings.Contains(seg, "?") {
			segments2[i] = strings.ToLower(seg)
		}
	}
	rawPathLower := strings.Join(segments2, "/")

	query := r.URL.Query()
	upstreamParam := query.Get(upstreamQueryParam)
	query = removeProxyParams(query)
	newQuery := query.Encode()

	suffix := "/" + rawPathLower
	if newQuery != "" {
		suffix += "?" + newQuery
	}

	candidateUpstreams, isAll := selectUpstreams(serviceCfg, upstreamParam)

	if username != "" {
		candidateUpstreams = filterUnsupported(candidateUpstreams, username)
		if len(candidateUpstreams) == 0 {
			log.Printf("[PROXY] All upstreams unsupported for user '%s'", username)
			http.Error(w, fmt.Sprintf("All upstreams unsupported for user '%s'", username), http.StatusBadRequest)
			return
		}
	}

	if username != "" {
		bypassMap := parseBypassParams(r)
		if bypassList, ok := bypassMap[username]; ok && len(bypassList) > 0 {
			filtered := filterBypassed(candidateUpstreams, bypassList)

			if len(filtered) == 0 && upstreamParam != "" {
				log.Printf("[PROXY] Requested upstream(s) '%s' bypassed for user '%s', falling back to all", upstreamParam, username)
				allUpstreams := filterUnsupported(serviceCfg.Upstreams, username)
				filtered = filterBypassed(allUpstreams, bypassList)
				isAll = true
			}

			if len(filtered) == 0 {
				log.Printf("[PROXY] All upstreams bypassed for user '%s'", username)
				http.Error(w, fmt.Sprintf("All upstreams bypassed for user '%s'", username), http.StatusBadRequest)
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
	if len(healthyCandidates) == 0 && !isAll {
		log.Printf("[PROXY] Selected upstreams offline, falling back to all (filtered) candidates")
		for _, u := range candidateUpstreams {
			if u.IsHealthy() {
				healthyCandidates = append(healthyCandidates, u)
			}
		}
		isAll = true
	}
	if len(healthyCandidates) == 0 {
		log.Printf("[PROXY] No healthy upstreams for '%s'", service)
		http.Error(w, fmt.Sprintf("No healthy upstreams available for '%s'", service), http.StatusBadGateway)
		return
	}

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

	if serviceCfg.Race {
		h.executeRaceRM(w, r, healthyCandidates, suffix, bodyBytes, service, startTime)
		return
	}

	var lastError string
	for _, upstream := range healthyCandidates {
		target := buildTargetURL(upstream.URL, suffix)
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
		if err != nil {
			lastError = err.Error()
			continue
		}
		copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)
		resp, err := h.Client.Do(outReq)
		if err != nil {
			lastError = err.Error()
			continue
		}
		if resp.StatusCode < 400 {
			log.Printf("[PROXY] Service: %s | Status: %d | Time: %v | Upstream: %s", service, resp.StatusCode, time.Since(startTime), upstream.URL)
			writeResponse(w, resp, upstream.URL, startTime)
			return
		}
		resp.Body.Close()
		lastError = fmt.Sprintf("Status %d", resp.StatusCode)
	}

	log.Printf("[PROXY-FAIL] All upstreams failed for '%s' | Last Error: %s", service, lastError)
	http.Error(w, fmt.Sprintf("All upstreams for '%s' failed. Last error: %s", service, lastError), http.StatusBadGateway)
}

func (h *Handler) executeRaceRM(w http.ResponseWriter, r *http.Request, upstreams []*config.Upstream, suffix string, bodyBytes []byte, service string, startTime time.Time) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	results := make(chan raceResult, len(upstreams))
	var wg sync.WaitGroup

	for _, upstream := range upstreams {
		wg.Add(1)
		go func(u *config.Upstream) {
			defer wg.Done()
			target := buildTargetURL(u.URL, suffix)

			var bodyReader io.Reader
			if len(bodyBytes) > 0 {
				bodyReader = strings.NewReader(string(bodyBytes))
			}

			outReq, err := http.NewRequestWithContext(ctx, r.Method, target, bodyReader)
			if err != nil {
				results <- raceResult{err: err}
				return
			}

			copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)

			resp, err := h.Client.Do(outReq)
			if err != nil {
				results <- raceResult{err: err}
				return
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				results <- raceResult{resp: resp, upstreamURL: u.URL}
			} else {
				resp.Body.Close()
				results <- raceResult{err: fmt.Errorf("status %d from %s", resp.StatusCode, target)}
			}
		}(upstream)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var lastErr error
	for res := range results {
		if res.resp != nil {
			log.Printf("[RACE-WIN] Service: %s | Status: %d | Time: %v | Winner: %s", service, res.resp.StatusCode, time.Since(startTime), res.upstreamURL)
			writeResponse(w, res.resp, res.upstreamURL, startTime)
			return
		}
		if res.err != nil {
			lastErr = res.err
		}
	}

	log.Printf("[RACE-FAIL] All parallel upstreams failed for '%s' | Last Error: %v", service, lastErr)
	http.Error(w, fmt.Sprintf("All parallel upstreams failed for '%s'. Last error: %v", service, lastErr), http.StatusBadGateway)
}
