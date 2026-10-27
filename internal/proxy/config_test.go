package proxy

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	t.Setenv("NEAR_AI_API_KEY", "synthetic-near-key")
	t.Setenv("PROXY_API_KEY", testKey)
	t.Setenv("MODELS", "")
	t.Setenv("LISTEN_ADDR", "")
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
