package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	t.Setenv("NEAR_AI_API_KEY", "synthetic-near-key")
	t.Setenv("PROXY_API_KEY", testKey)
	t.Setenv("MODELS", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("MAX_BODY_BYTES", "")
	t.Setenv("REQUEST_TIMEOUT", "")
	t.Setenv("WRITE_TIMEOUT", "")
	c, err := LoadConfig()
	if err != nil || c.Listen != "127.0.0.1:8080" || len(c.Models) != 1 || c.NearKey != "synthetic-near-key" || c.ProxyKey != testKey {
		t.Fatal("expected safe defaults")
	}
	t.Setenv("MODELS", "provider/A, provider/B")
	if c, err := LoadConfig(); err != nil || len(c.Models) != 2 || c.Models[0] != "provider/A" || c.Models[1] != "provider/B" {
		t.Fatal("load the explicit discovery/readiness list")
	}
	t.Setenv("MODELS", "")
	for _, test := range []struct{ key, value string }{
		{"NEAR_AI_API_KEY", ""}, {"NEAR_AI_API_KEY", "private-marker with spaces"}, {"NEAR_AI_API_KEY", "private-marker\nsecond-line"},
		{"NEAR_AI_API_KEY", strings.Repeat("x", 4097)},
		{"PROXY_API_KEY", ""}, {"PROXY_API_KEY", "short"}, {"PROXY_API_KEY", strings.Repeat("x", 32) + "\n"},
		{"PROXY_API_KEY", strings.Repeat("x", 257)},
		{"MODELS", "a,a"}, {"MODELS", "a,../secret"}, {"MODELS", "a,"}, {"MODELS", "a,b,c,d,e,f,g,h,i"}, {"LISTEN_ADDR", "missing-port"},
	} {
		t.Run(test.key+" invalid", func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid settings must fail")
			}
		})
	}
	t.Setenv("NEAR_AI_API_KEY", testKey)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("inbound and outbound secrets must differ")
	}
}

func TestConfigOperationalLimits(t *testing.T) {
	t.Setenv("NEAR_AI_API_KEY", "synthetic-near-key")
	t.Setenv("PROXY_API_KEY", testKey)
	t.Setenv("MODELS", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("MAX_BODY_BYTES", "")
	t.Setenv("REQUEST_TIMEOUT", "")
	t.Setenv("WRITE_TIMEOUT", "")
	c, err := LoadConfig()
	if err != nil || c.MaxBodyBytes != 0 || c.RequestTimeout != 0 || c.WriteTimeout != 0 {
		t.Fatal("defaults must use SDK body capacity and leave inference lifetime to callers")
	}
	t.Setenv("MAX_BODY_BYTES", "268435456")
	t.Setenv("REQUEST_TIMEOUT", "30m")
	t.Setenv("WRITE_TIMEOUT", "45s")
	c, err = LoadConfig()
	if err != nil || c.MaxBodyBytes != 256<<20 || c.RequestTimeout != 30*time.Minute || c.WriteTimeout != 45*time.Second {
		t.Fatal("operators must be able to raise body limits and choose a deadline")
	}
	for _, test := range []struct{ key, value string }{
		{"MAX_BODY_BYTES", "-1"}, {"MAX_BODY_BYTES", "1.5"},
		{"MAX_BODY_BYTES", "9223372036854775807"}, {"MAX_BODY_BYTES", "private-marker"},
		{"REQUEST_TIMEOUT", "-1s"}, {"REQUEST_TIMEOUT", "10"}, {"REQUEST_TIMEOUT", "private-marker"},
		{"WRITE_TIMEOUT", "-1s"}, {"WRITE_TIMEOUT", "10"}, {"WRITE_TIMEOUT", "private-marker"},
	} {
		t.Run(test.key, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := LoadConfig(); err == nil || strings.Contains(err.Error(), "private-marker") {
				t.Fatal("invalid settings must fail without reflecting their values")
			}
		})
	}
	t.Setenv("MAX_BODY_BYTES", "0")
	t.Setenv("REQUEST_TIMEOUT", "0")
	t.Setenv("WRITE_TIMEOUT", "0")
	if c, err := LoadConfig(); err != nil || c.MaxBodyBytes != 0 || c.RequestTimeout != 0 || c.WriteTimeout != 0 {
		t.Fatal("explicit zero must select SDK body capacity and disable the proxy deadline")
	}
}
