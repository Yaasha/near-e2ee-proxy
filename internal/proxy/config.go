// Package proxy implements the small, deliberately limited OpenAI-facing API.
package proxy

import (
	"errors"
	"net"
	"os"
	"strings"
)

type Config struct {
	Listen   string
	NearKey  string
	ProxyKey string
	Models   []string // Advertised by /v1/models and probed by /readyz; not a restriction.
}

// LoadConfig reads only process environment. Mise (in development) or the
// deployment environment handles dotenv files; the application never reads them.
func LoadConfig() (Config, error) {
	var config Config
	config.NearKey = os.Getenv("NEAR_AI_API_KEY")
	if !validToken(config.NearKey, 1, 4096) {
		return Config{}, errors.New("NEAR_AI_API_KEY must contain a nonempty single-line ASCII token")
	}
	config.ProxyKey = os.Getenv("PROXY_API_KEY")
	if !validToken(config.ProxyKey, 32, 256) {
		return Config{}, errors.New("PROXY_API_KEY must contain 32 to 256 non-whitespace ASCII characters")
	}
	if config.ProxyKey == config.NearKey {
		return Config{}, errors.New("the proxy key must differ from the NEAR key")
	}
	config.Listen = os.Getenv("LISTEN_ADDR")
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(config.Listen); err != nil {
		return Config{}, errors.New("LISTEN_ADDR must be a host:port address")
	}
	models := os.Getenv("MODELS")
	if models == "" {
		models = "Qwen/Qwen3.8-27B"
	}
	seen := map[string]bool{}
	for _, model := range strings.Split(models, ",") {
		model = strings.TrimSpace(model)
		if !validModelID(model) || seen[model] {
			return Config{}, errors.New("MODELS must contain unique canonical model IDs")
		}
		seen[model] = true
		config.Models = append(config.Models, model)
	}
	if len(config.Models) > 8 {
		return Config{}, errors.New("at most eight discovery/readiness models are supported")
	}
	return config, nil
}

func validToken(token string, minimum, maximum int) bool {
	if len(token) < minimum || len(token) > maximum {
		return false
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
