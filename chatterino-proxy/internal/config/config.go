// Package config
package config

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type InstanceConfig struct {
	Maintainer        string  `json:"maintainer,omitempty"`
	Message           string  `json:"message,omitempty"`
	URL               string  `json:"url,omitempty"`
	Description       string  `json:"description,omitempty"`
	UserAgent         string  `json:"userAgent"`
	HeartbeatInterval float64 `json:"heartbeatInterval"`
	Timeout           float64 `json:"timeout"`
	HealthTimeout     float64 `json:"healthTimeout"`
	Race              bool    `json:"race"`
}

type ServiceConfig struct {
	Race      bool
	Upstreams []*Upstream
}

type Upstream struct {
	URL            string                       `json:"url"`
	Path           string                       `json:"path,omitempty"`
	HealthEndpoint string                       `json:"healthEndpoint,omitempty"`
	Description    string                       `json:"description,omitempty"`
	Metadata       map[string]interface{}       `json:"metadata"`
	Healthy        bool                         `json:"healthy"`
	Latency        time.Duration                `json:"latency"`
	Unsupported    []string                     `json:"unsupported,omitempty"`
	Replace        map[string]map[string]string `json:"replace,omitempty"`
	mu             sync.RWMutex
}

func (u *Upstream) IsHealthy() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.Healthy
}

func (u *Upstream) GetLatency() time.Duration {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.Latency
}

func (u *Upstream) SetHealthAndLatency(status bool, latency time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Healthy = status
	u.Latency = latency
}

func (u *Upstream) GetCheckURL() string {
	if u.HealthEndpoint != "" {
		if strings.HasPrefix(u.HealthEndpoint, "http://") || strings.HasPrefix(u.HealthEndpoint, "https://") {
			return u.HealthEndpoint
		}

		baseURL := strings.TrimRight(u.URL, "/")
		baseURL = strings.ReplaceAll(baseURL, "%1", "")
		parsed, err := url.Parse(baseURL)
		if err == nil {
			healthPath := u.HealthEndpoint
			if !strings.HasPrefix(healthPath, "/") {
				healthPath = "/" + healthPath
			}
			return parsed.Scheme + "://" + parsed.Host + healthPath
		}
	}

	checkURL := strings.TrimRight(u.URL, "/")
	return strings.ReplaceAll(checkURL, "%1", "")
}

type Config struct {
	InstanceConfig InstanceConfig
	Services       map[string]*ServiceConfig
	mu             sync.RWMutex
}

func NewConfig() *Config {
	return &Config{
		InstanceConfig: InstanceConfig{
			UserAgent:         "chatterino-proxy/1.1.0",
			HeartbeatInterval: 30,
			Timeout:           30.0,
			HealthTimeout:     5.0,
			Race:              false,
		},
		Services: make(map[string]*ServiceConfig),
	}
}

func (c *Config) Load(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	var rawConfig map[string]json.RawMessage
	if err := json.NewDecoder(file).Decode(&rawConfig); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if instanceRaw, exists := rawConfig["instance"]; exists {
		var inst InstanceConfig
		if err := json.Unmarshal(instanceRaw, &inst); err == nil {
			if inst.Maintainer != "" {
				c.InstanceConfig.Maintainer = inst.Maintainer
			}
			if inst.Message != "" {
				c.InstanceConfig.Message = inst.Message
			}
			if inst.URL != "" {
				c.InstanceConfig.URL = inst.URL
			}
			if inst.Description != "" {
				c.InstanceConfig.Description = inst.Description
			}
			if inst.UserAgent != "" {
				c.InstanceConfig.UserAgent = inst.UserAgent
			}
			if inst.HeartbeatInterval > 0 {
				c.InstanceConfig.HeartbeatInterval = inst.HeartbeatInterval
			}
			if inst.Timeout > 0 {
				c.InstanceConfig.Timeout = inst.Timeout
			}
			if inst.HealthTimeout > 0 {
				c.InstanceConfig.HealthTimeout = inst.HealthTimeout
			}
			c.InstanceConfig.Race = inst.Race
		}
	}

	c.Services = make(map[string]*ServiceConfig)
	for key, rawValue := range rawConfig {
		if strings.HasSuffix(key, "Instances") && key != "instance" {
			serviceName := strings.TrimSuffix(key, "Instances")

			var genericMap map[string]json.RawMessage
			if err := json.Unmarshal(rawValue, &genericMap); err != nil {
				continue
			}

			serviceRace := c.InstanceConfig.Race
			var upstreams []*Upstream

			for subKey, subRaw := range genericMap {
				if subKey == "race" {
					var raceVal bool
					if err := json.Unmarshal(subRaw, &raceVal); err == nil {
						serviceRace = raceVal
					}
					continue
				}

				var metadata map[string]interface{}
				if err := json.Unmarshal(subRaw, &metadata); err == nil {
					targetURL := subKey
					if strings.TrimSpace(targetURL) != "" {
						healthEp := ""
						if val, ok := metadata["health"]; ok {
							if strVal, isStr := val.(string); isStr {
								healthEp = strVal
							}
						}

						pathVal := ""
						if val, ok := metadata["path"]; ok {
							if strVal, isStr := val.(string); isStr {
								pathVal = strVal
							}
						}

						descVal := ""
						if val, ok := metadata["description"]; ok {
							if strVal, isStr := val.(string); isStr {
								descVal = strVal
							}
						}

						var unsupported []string
						if val, ok := metadata["unsupported"]; ok {
							if arr, ok := val.([]interface{}); ok {
								for _, item := range arr {
									if str, ok := item.(string); ok {
										unsupported = append(unsupported, str)
									}
								}
							}
						}

						replaceMap := make(map[string]map[string]string)
						if val, ok := metadata["replace"]; ok {
							if m, ok := val.(map[string]interface{}); ok {
								for domain, rules := range m {
									if rulesMap, ok := rules.(map[string]interface{}); ok {
										inner := make(map[string]string)
										for find, repl := range rulesMap {
											if replStr, ok := repl.(string); ok {
												inner[find] = replStr
											}
										}
										if len(inner) > 0 {
											replaceMap[domain] = inner
										}
									}
								}
							}
						}

						upstreams = append(upstreams, &Upstream{
							URL:            targetURL,
							Path:           pathVal,
							HealthEndpoint: healthEp,
							Description:    descVal,
							Metadata:       metadata,
							Healthy:        true,
							Unsupported:    unsupported,
							Replace:        replaceMap,
						})
					}
				}
			}

			if len(upstreams) > 0 {
				c.Services[serviceName] = &ServiceConfig{
					Race:      serviceRace,
					Upstreams: upstreams,
				}
			}
		}
	}

	return nil
}

func (c *Config) GetServices() map[string]*ServiceConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	copied := make(map[string]*ServiceConfig, len(c.Services))
	for k, v := range c.Services {
		copied[k] = v
	}
	return copied
}
