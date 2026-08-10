// Package utils
package utils

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"proxy/internal/config"
)

var HopByHopHeaders = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"transfer-encoding": true,
	"upgrade":           true,
	"te":                true,
	"trailer":           true,
}

func StartHeartbeat(ctx context.Context, cfg *config.Config, healthClient *http.Client) {
	ticker := time.NewTicker(time.Duration(cfg.InstanceConfig.HeartbeatInterval * float64(time.Second)))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping heartbeat task...")
			return
		case <-ticker.C:
			services := cfg.GetServices()
			var allUpstreams []*config.Upstream
			for _, serviceCfg := range services {
				allUpstreams = append(allUpstreams, serviceCfg.Upstreams...)
			}

			var wg sync.WaitGroup
			for _, u := range allUpstreams {
				wg.Add(1)
				go func(upstream *config.Upstream) {
					defer wg.Done()
					checkURL := upstream.GetCheckURL()

					req, err := http.NewRequestWithContext(ctx, http.MethodHead, checkURL, nil)
					if err != nil {
						upstream.SetHealthAndLatency(false, 0)
						return
					}

					start := time.Now()
					resp, err := healthClient.Do(req)
					latency := time.Since(start)

					if err != nil {
						log.Printf("Health check failed for %s (%s): %v", upstream.URL, checkURL, err)
						upstream.SetHealthAndLatency(false, 0)
						return
					}
					defer resp.Body.Close()

					var isHealthy bool
					if upstream.HealthEndpoint != "" {
						isHealthy = resp.StatusCode >= 200 && resp.StatusCode < 400
					} else {
						isHealthy = resp.StatusCode < 500
					}

					upstream.SetHealthAndLatency(isHealthy, latency)
				}(u)
			}
			wg.Wait()
		}
	}
}
