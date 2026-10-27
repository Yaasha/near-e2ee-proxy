//go:build container

// Package container tests the shipped image through Docker and real HTTP only.
// It is opt-in: ordinary Go checks need neither Docker nor application secrets.
package container

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"debug/elf"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const nearKey = "synthetic-container-near-key"
const proxyKey = "synthetic-container-proxy-key-32-characters"

func docker(t *testing.T, input string, args ...string) ([]byte, error) {
	t.Helper()
	timeout := 30 * time.Second
	if len(args) > 0 && args[0] == "buildx" {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}

func mustDocker(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	output, err := docker(t, input, args...)
	if err != nil {
		// Arguments and output can contain synthetic credentials; keep failures
		// fixed just like production errors. Never dump container environments.
		t.Fatalf("Docker %s failed: %v", args[0], err)
	}
	return bytes.TrimSpace(output)
}

func TestContainer(t *testing.T) {
	arch := os.Getenv("CONTAINER_ARCH")
	if arch == "" {
		arch = runtime.GOARCH
	}
	if arch != "amd64" && arch != "arm64" {
		t.Fatal("CONTAINER_ARCH must be amd64 or arm64")
	}
	platform := "linux/" + arch
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("find repository")
	}
	buildContext := t.TempDir()
	// Copy only named build inputs; never walk real local state or env files.
	for _, name := range []string{"Dockerfile", ".dockerignore", "go.mod", "go.sum", "mise.toml", "mise.lock", "tools/check-go.sh", "cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			relative, _ := filepath.Rel(root, path)
			if (name == "cmd" || name == "internal") && !strings.HasSuffix(relative, ".go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			writeFile(t, filepath.Join(buildContext, relative), data)
			return nil
		})
		if err != nil {
			t.Fatal("copy explicit synthetic build context")
		}
	}
	sentinels := []string{".env", ".env.production", ".aws/credentials", ".git/config", ".cache/token", "bin/proxy", "mise.local.toml", "unrelated.go", "cmd/.env", "cmd/near-e2ee-proxy/.env", "internal/proxy/secret.json", "internal/secret.json", "tools/secret", "nested/local.go"}
	for _, name := range sentinels {
		writeFile(t, filepath.Join(buildContext, name), []byte("synthetic-excluded-local-state"))
	}
	t.Run("build context excludes local state", func(t *testing.T) {
		destination := t.TempDir()
		mustDocker(t, "FROM scratch\nCOPY . /\n", "buildx", "build", "--platform", platform, "--provenance=false", "--file", "-", "--output", "type=local,dest="+destination, buildContext)
		for _, name := range sentinels {
			if _, err := os.Stat(filepath.Join(destination, name)); !os.IsNotExist(err) {
				t.Fatalf("forbidden synthetic input reached build context: %s", name)
			}
		}
		if _, err := os.Stat(filepath.Join(destination, "go.mod")); err != nil {
			t.Fatal("context exclusion test must include a real allowed input")
		}
	})
	t.Run("compiler drift fails before dependency download", func(t *testing.T) {
		for _, name := range []string{"mise.toml", "mise.lock", "go.mod"} {
			path := filepath.Join(buildContext, name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("read synthetic toolchain input")
			}
			defer writeFile(t, path, original)
			// Keep all repository version declarations consistent but deliberately
			// different from the pinned build base's actual compiler.
			writeFile(t, path, bytes.ReplaceAll(original, []byte("1.27.3"), []byte("1.27.2")))
		}
		output, err := docker(t, "", "buildx", "build", "--platform", platform, "--target", "build", "--output=type=cacheonly", buildContext)
		if err == nil || !bytes.Contains(output, []byte("Go compiler must match mise.toml, mise.lock and go.mod exactly")) {
			t.Fatal("compiler drift must fail at the explicit version gate")
		}
	})
	image := "near-e2ee-proxy-test:" + arch + "-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { _, _ = docker(t, "", "image", "rm", "-f", image) })
	mustDocker(t, "", "buildx", "build", "--load", "--platform", platform, "--provenance=false", "--tag", image, buildContext)
	var images []struct {
		Architecture string
		Config       struct {
			User       string
			Entrypoint []string
			Env        []string
		}
	}
	if json.Unmarshal(mustDocker(t, "", "image", "inspect", image), &images) != nil || len(images) != 1 || images[0].Architecture != arch || images[0].Config.User != "65532:65532" || strings.Join(images[0].Config.Entrypoint, " ") != "/near-e2ee-proxy" {
		t.Fatal("image must match requested architecture and run directly as non-root")
	}
	for _, setting := range images[0].Config.Env {
		if strings.HasPrefix(setting, "NEAR_AI_API_KEY=") || strings.HasPrefix(setting, "PROXY_API_KEY=") {
			t.Fatal("credentials must not be image defaults")
		}
	}
	network := "near-e2ee-test-" + arch + "-" + time.Now().Format("150405.000000000")
	mustDocker(t, "", "network", "create", network)
	t.Cleanup(func() { _, _ = docker(t, "", "network", "rm", network) })
	create := func(t *testing.T, port string, env []string, arguments ...string) string {
		t.Helper()
		// Only liveness and unauthenticated routes are exercised. Redirect the
		// fixed NEAR origin to container loopback as an additional egress guard.
		args := []string{"create", "--platform", platform, "--network", network, "--add-host=cloud-api.near.ai:127.0.0.1", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--pids-limit=128", "--memory=256m"}
		if port != "" {
			args = append(args, "--publish", "127.0.0.1::"+port)
		}
		for _, value := range env {
			args = append(args, "--env", value)
		}
		args = append(args, image)
		args = append(args, arguments...)
		id := string(mustDocker(t, "", args...))
		t.Cleanup(func() { _, _ = docker(t, "", "rm", "-f", id) })
		return id
	}
	t.Run("static binary and runtime trust material", func(t *testing.T) {
		id := create(t, "", nil)
		archive := filepath.Join(t.TempDir(), "runtime.tar")
		mustDocker(t, "", "export", "--output", archive, id)
		file, err := os.Open(archive)
		if err != nil {
			t.Fatal("open exported runtime")
		}
		defer file.Close()
		reader := tar.NewReader(file)
		certificates, binary, timezone := false, false, false
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal("read exported runtime")
			}
			switch strings.TrimPrefix(header.Name, "./") {
			case "etc/ssl/certs/ca-certificates.crt":
				data, _ := io.ReadAll(io.LimitReader(reader, 2<<20))
				pool := x509.NewCertPool()
				certificates = pool.AppendCertsFromPEM(data) && len(pool.Subjects()) > 0
			case "usr/share/zoneinfo/Etc/UTC":
				timezone = true
			case "near-e2ee-proxy":
				data, _ := io.ReadAll(io.LimitReader(reader, 128<<20))
				executable, err := elf.NewFile(bytes.NewReader(data))
				if err != nil {
					t.Fatal("runtime binary is not ELF")
				}
				for _, program := range executable.Progs {
					if program.Type == elf.PT_INTERP {
						t.Fatal("runtime binary requires a dynamic loader")
					}
				}
				binary = true
			case "bin/sh", "bin/bash", "usr/bin/go", "usr/local/go/bin/go":
				t.Fatal("runtime must exclude shells and build tooling")
			}
		}
		if !certificates || !binary || !timezone {
			t.Fatal("runtime lacks static application, CA trust or timezone material")
		}
	})
	t.Run("CLI without credentials", func(t *testing.T) {
		for _, argument := range []string{"-help", "-generate-key"} {
			id := create(t, "", nil, argument)
			mustDocker(t, "", "start", id)
			if string(mustDocker(t, "", "wait", id)) != "0" {
				t.Fatal("CLI must work without credentials")
			}
		}
	})
	for name, env := range map[string][]string{
		"missing credentials": nil,
		"missing proxy key":   {"NEAR_AI_API_KEY=" + nearKey},
		"missing NEAR key":    {"PROXY_API_KEY=" + proxyKey},
		"invalid proxy key":   {"NEAR_AI_API_KEY=" + nearKey, "PROXY_API_KEY=synthetic-short"},
		"invalid NEAR key":    {"NEAR_AI_API_KEY=synthetic invalid", "PROXY_API_KEY=" + proxyKey},
		"equal keys":          {"NEAR_AI_API_KEY=" + proxyKey, "PROXY_API_KEY=" + proxyKey},
		"invalid listener":    {"NEAR_AI_API_KEY=" + nearKey, "PROXY_API_KEY=" + proxyKey, "LISTEN_ADDR=invalid"},
		"invalid models":      {"NEAR_AI_API_KEY=" + nearKey, "PROXY_API_KEY=" + proxyKey, "MODELS=../invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			id := create(t, "", env)
			mustDocker(t, "", "start", id)
			if string(mustDocker(t, "", "wait", id)) != "1" {
				t.Fatal("invalid configuration must exit with failure")
			}
			logs := mustDocker(t, "", "logs", id)
			if bytes.Contains(logs, []byte("synthetic")) {
				t.Fatal("configuration failure disclosed credential values")
			}
		})
	}
	for _, override := range []bool{false, true} {
		name, port := "default listener", "8080"
		env := []string{"NEAR_AI_API_KEY=" + nearKey, "PROXY_API_KEY=" + proxyKey}
		if override {
			name, port = "listener override", "9090"
			env = append(env, "LISTEN_ADDR=0.0.0.0:9090", "MODELS=synthetic-model")
		}
		t.Run(name, func(t *testing.T) {
			id := create(t, port, env)
			mustDocker(t, "", "start", id)
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := docker(t, "", "exec", id, "/near-e2ee-proxy", "-healthcheck"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("container did not become live")
				}
				time.Sleep(100 * time.Millisecond)
			}
			published := string(mustDocker(t, "", "port", id, port+"/tcp"))
			client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			for _, path := range []string{"/healthz", "/readyz", "/v1/models", "/v1/chat/completions"} {
				method, wanted := "GET", http.StatusUnauthorized
				if path == "/healthz" {
					wanted = http.StatusOK
				}
				if path == "/v1/chat/completions" {
					method = "POST"
				}
				request, _ := http.NewRequest(method, "http://"+published+path, nil)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("published loopback port is unreachable")
				}
				response.Body.Close()
				if response.StatusCode != wanted {
					t.Fatal("liveness or unauthenticated protected route returned wrong status")
				}
			}
			if _, err := docker(t, "", "exec", "--env", "LISTEN_ADDR=127.0.0.1:1", id, "/near-e2ee-proxy", "-healthcheck"); err == nil {
				t.Fatal("probe must fail for an unavailable listener")
			}
			started := time.Now()
			mustDocker(t, "", "stop", "--signal=SIGTERM", "--timeout=15", id)
			if time.Since(started) >= 15*time.Second || string(mustDocker(t, "", "wait", id)) != "0" {
				t.Fatal("SIGTERM must reach PID 1 and exit cleanly within the grace period")
			}
		})
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, data, 0600) != nil {
		t.Fatal("write synthetic build input")
	}
}

func TestComposeConfiguration(t *testing.T) {
	t.Setenv("NEAR_AI_API_KEY", nearKey)
	t.Setenv("PROXY_API_KEY", proxyKey)
	t.Setenv("MODELS", "")
	// Explicitly disable Compose's automatic dotenv loading. No real env file
	// or secret mount is read, even when the caller has development state.
	data := mustDocker(t, "", "compose", "--env-file", os.DevNull, "--file", "../../compose.yaml", "config", "--format", "json")
	var config struct {
		Services map[string]struct {
			Restart         string            `json:"restart"`
			ReadOnly        bool              `json:"read_only"`
			StopGracePeriod string            `json:"stop_grace_period"`
			CapDrop         []string          `json:"cap_drop"`
			SecurityOpt     []string          `json:"security_opt"`
			Environment     map[string]string `json:"environment"`
			Ports           []struct {
				HostIP string `json:"host_ip"`
				Target int    `json:"target"`
			} `json:"ports"`
		} `json:"services"`
	}
	if json.Unmarshal(data, &config) != nil {
		t.Fatal("parse Compose configuration")
	}
	service := config.Services["proxy"]
	if service.Restart != "unless-stopped" || !service.ReadOnly || service.StopGracePeriod != "15s" || strings.Join(service.CapDrop, ",") != "ALL" || strings.Join(service.SecurityOpt, ",") != "no-new-privileges:true" || len(service.Ports) != 1 || service.Ports[0].HostIP != "127.0.0.1" || service.Ports[0].Target != 8080 {
		t.Fatal("Compose must preserve loopback publishing, shutdown and runtime hardening")
	}
	if service.Environment["NEAR_AI_API_KEY"] != nearKey || service.Environment["PROXY_API_KEY"] != proxyKey || service.Environment["LISTEN_ADDR"] != "0.0.0.0:8080" {
		t.Fatal("Compose must supply credentials at runtime and listen inside the container")
	}
}
