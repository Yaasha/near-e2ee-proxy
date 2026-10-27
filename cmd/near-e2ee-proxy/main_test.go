package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"near-e2ee-proxy/internal/proxy"
)

func TestHealthcheckCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "proxy")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.RequestURI() != "/healthz" || r.Header.Get("Authorization") != "" {
			t.Error("probe must only request unauthenticated local liveness")
		}
		proxy.NewServer(proxy.Config{}, nil).ServeHTTP(w, r)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	run := func(t *testing.T, listen string, success bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-healthcheck")
		// Deliberately omit credentials and poison ambient proxy settings. The
		// probe must work independently of inference and outbound routing.
		cmd.Env = []string{"LISTEN_ADDR=" + listen, "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1"}
		started := time.Now()
		output, err := cmd.CombinedOutput()
		if (err == nil) != success || len(output) != 0 {
			t.Fatalf("healthcheck success=%v, wanted %v; probe output must be empty", err == nil, success)
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("healthcheck exceeded its bounded deadline")
		}
	}
	t.Run("liveness without credentials", func(t *testing.T) { run(t, address, true) })
	t.Run("wildcard listener", func(t *testing.T) {
		_, port, _ := net.SplitHostPort(address)
		run(t, "0.0.0.0:"+port, true)
	})
	for _, listen := range []string{"missing-port", "example.com:8080", "192.0.2.1:8080", "127.0.0.1:0"} {
		t.Run("invalid or remote address "+listen, func(t *testing.T) { run(t, listen, false) })
	}
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			before := calls.Load()
			failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", server.URL+"/healthz")
				w.WriteHeader(status)
			}))
			defer failure.Close()
			run(t, strings.TrimPrefix(failure.URL, "http://"), false)
			if calls.Load() != before {
				t.Fatal("probe followed a redirect")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer slow.Close()
		run(t, strings.TrimPrefix(slow.URL, "http://"), false)
	})
	server.Close()
	t.Run("stopped listener", func(t *testing.T) { run(t, address, false) })
}
