package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const revision = "1111111111111111111111111111111111111111"
const digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type commandFixture struct {
	app *distribution
	env map[string]string
	dir string
}

type offlineTransport struct{}

func (offlineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected registry request")
}

// Exercise the command dispatcher, validation and reporting in process. Only
// external Git/GitHub/Docker commands and HTTP are replaced with synthetic data.
func fixture(t *testing.T) *commandFixture {
	t.Helper()
	dir := t.TempDir()
	f := &commandFixture{app: newDistribution(), dir: dir, env: map[string]string{
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
		"GITHUB_EVENT_NAME":   "push", "GITHUB_REPOSITORY": repository,
		"GITHUB_REF": "refs/heads/main", "GITHUB_SHA": revision,
		"VALIDATION_RESULTS": `{"check":{"result":"success"},"containers":{"result":"success"}}`,
		"FIXTURE_HEAD":       revision, "FIXTURE_HEAD_FILE": filepath.Join(dir, "head"),
		"FIXTURE_DIGEST": digest, "FIXTURE_CHECKOUT": revision, "FIXTURE_TAG_COMMIT": revision,
		"FIXTURE_ANCESTRY_EXIT": "0", "FIXTURE_FETCH_EXIT": "0",
		"FIXTURE_WRITES": filepath.Join(dir, "writes"), "FIXTURE_REGISTRY": filepath.Join(dir, "registry.json"),
		"FIXTURE_PACKAGE": filepath.Join(dir, "package.json"),
	}}
	f.app.getenv = f.getenv
	f.app.command = f.output
	f.app.registry.Transport = offlineTransport{}
	if err := os.WriteFile(f.getenv("FIXTURE_PACKAGE"), []byte(`{"visibility":"public","repository":{"full_name":"Yaasha/near-e2ee-proxy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *commandFixture) getenv(name string) string { return f.env[name] }
func (f *commandFixture) setenv(name, value string) { f.env[name] = value }

func (f *commandFixture) command(args ...string) ([]byte, error) { return f.invoke(*f.app, args...) }

func (f *commandFixture) commandForRef(ref string, args ...string) ([]byte, error) {
	app := *f.app
	app.getenv = func(name string) string {
		if name == "GITHUB_REF" {
			return ref
		}
		return f.getenv(name)
	}
	return f.invoke(app, args...)
}

func (f *commandFixture) invoke(app distribution, args ...string) ([]byte, error) {
	var result bytes.Buffer
	app.stdout, app.stderr = &result, &result
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := app.execute(ctx, args)
	return result.Bytes(), err
}

func (f *commandFixture) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch name {
	case "gh":
		if len(args) == 2 && args[0] == "api" && args[1] == "users/Yaasha/packages/container/near-e2ee-proxy" {
			return os.ReadFile(f.getenv("FIXTURE_PACKAGE"))
		}
		if len(args) == 4 && args[0] == "api" && args[1] == "repos/"+repository+"/git/ref/heads/main" && args[2] == "--jq" && args[3] == ".object.sha" {
			if data, err := os.ReadFile(f.getenv("FIXTURE_HEAD_FILE")); err == nil {
				return data, nil
			}
			return []byte(f.getenv("FIXTURE_HEAD") + "\n"), nil
		}
	case "git":
		switch {
		case len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD":
			return []byte(f.getenv("FIXTURE_CHECKOUT") + "\n"), nil
		case len(args) == 2 && args[0] == "rev-parse" && args[1] == "FETCH_HEAD^{commit}":
			return []byte(f.getenv("FIXTURE_TAG_COMMIT") + "\n"), nil
		case len(args) == 4 && args[0] == "fetch" && args[1] == "--no-tags" && args[2] == "origin":
			if f.getenv("FIXTURE_FETCH_EXIT") != "0" {
				return nil, errors.New("git command failed")
			}
			return nil, nil
		case len(args) == 4 && args[0] == "merge-base" && args[1] == "--is-ancestor" && args[2] == f.getenv("GITHUB_SHA") && args[3] == f.getenv("FIXTURE_HEAD"):
			if f.getenv("FIXTURE_ANCESTRY_EXIT") != "0" {
				return nil, errors.New("git command failed")
			}
			return nil, nil
		}
	case "docker":
		if len(args) != 6 || args[0] != "buildx" || args[1] != "imagetools" {
			break
		}
		if args[2] == "create" && args[3] == "--tag" {
			file, err := os.OpenFile(f.getenv("FIXTURE_WRITES"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			_, err = file.WriteString(strings.Join(args, " ") + "\n")
			return nil, err
		}
		if args[2] == "inspect" && args[4] == "--format" {
			if args[5] == "{{.Manifest.Digest}}" {
				return []byte(f.getenv("FIXTURE_DIGEST") + "\n"), nil
			}
			if args[5] != inspection {
				break
			}
			data, err := os.ReadFile(f.getenv("FIXTURE_REGISTRY"))
			if head := f.getenv("FIXTURE_AFTER_INSPECT_HEAD"); head != "" {
				if err := os.WriteFile(f.getenv("FIXTURE_HEAD_FILE"), []byte(head), 0600); err != nil {
					return nil, err
				}
			}
			return data, err
		}
	}
	return nil, errors.New("unexpected external command")
}

func TestTrustedMainCanPublish(t *testing.T) {
	f := fixture(t)
	output, err := f.command("gate")
	if err != nil || strings.TrimSpace(string(output)) != "eligible" {
		t.Fatalf("trusted validated current main must be eligible: %s", output)
	}
}

func TestPublicationEligibility(t *testing.T) {
	for _, test := range []struct {
		name, setting, value string
		allowed              bool
	}{
		{"manual main", "GITHUB_EVENT_NAME", "workflow_dispatch", true},
		{"PR", "GITHUB_EVENT_NAME", "pull_request", false},
		{"privileged PR", "GITHUB_EVENT_NAME", "pull_request_target", false},
		{"other event", "GITHUB_EVENT_NAME", "workflow_run", false},
		{"release", "GITHUB_EVENT_NAME", "release", false},
		{"branch", "GITHUB_REF", "refs/heads/feature", false},
		{"tag", "GITHUB_REF", "refs/tags/v1.0.0", true},
		{"fork", "GITHUB_REPOSITORY", "someone/near-e2ee-proxy", false},
		{"wrong checkout", "FIXTURE_CHECKOUT", strings.Repeat("2", 40), false},
		{"stale run", "FIXTURE_HEAD", strings.Repeat("2", 40), false},
		{"head unavailable", "FIXTURE_HEAD", "", false},
		{"invalid revision", "GITHUB_SHA", "main", false},
		{"failed checks", "VALIDATION_RESULTS", `{"check":{"result":"failure"},"containers":{"result":"success"}}`, false},
		{"partial containers", "VALIDATION_RESULTS", `{"check":{"result":"success"},"containers":{"result":"cancelled"}}`, false},
		{"skipped containers", "VALIDATION_RESULTS", `{"check":{"result":"success"},"containers":{"result":"skipped"}}`, false},
		{"missing checks", "VALIDATION_RESULTS", `{}`, false},
		{"invalid checks", "VALIDATION_RESULTS", `not json`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture(t)
			f.setenv(test.setting, test.value)
			_, err := f.command("gate")
			if (err == nil) != test.allowed {
				t.Fatalf("publication allowed=%v, want %v", err == nil, test.allowed)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "writes")); !os.IsNotExist(err) {
				t.Fatal("eligibility check must never write to the registry")
			}
		})
	}
}

func TestInspectPublishedImage(t *testing.T) {
	f := fixture(t)
	data, err := os.ReadFile("testdata/registry.json")
	if err != nil || os.WriteFile(f.getenv("FIXTURE_REGISTRY"), data, 0600) != nil {
		t.Fatal("prepare registry fixture")
	}
	output, err := f.command("verify", revision, digest)
	if err != nil || !strings.Contains(string(output), "verified linux/amd64 and linux/arm64") {
		t.Fatalf("complete image must verify: %s", output)
	}
}

func TestIncompleteRegistryArtifactsFail(t *testing.T) {
	original, err := os.ReadFile("testdata/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"missing ARM64", `"architecture":"arm64"`, `"architecture":"s390x"`},
		{"unexpected runnable platform", `"os":"unknown","architecture":"unknown"`, `"os":"linux","architecture":"s390x"`},
		{"wrong digest", digest, "sha256:" + strings.Repeat("f", 64)},
		{"wrong revision", revision, strings.Repeat("2", 40)},
		{"wrong source", `"org.opencontainers.image.source":"https://github.com/Yaasha/near-e2ee-proxy"`, `"org.opencontainers.image.source":"https://github.com/other/repo"`},
		{"missing OCI license", `"org.opencontainers.image.licenses":"MIT"`, `"org.opencontainers.image.licenses":""`},
		{"bad creation date", "2026-10-27T22:00:00Z", "yesterday"},
		{"detached attestation", `"vnd.docker.reference.type":"attestation-manifest"`, `"vnd.docker.reference.type":"other"`},
		{"missing provenance", `"SLSA"`, `"missing"`},
		{"unbound provenance", `"sha1":"1111111111111111111111111111111111111111"`, `"sha1":"2222222222222222222222222222222222222222"`},
		{"missing SBOM", `"SPDX"`, `"missing"`},
		{"empty SBOM", `"packages":[{"name":"near-e2ee-proxy"}]`, `"packages":[]`},
		{"invalid JSON", "{", "!"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture(t)
			data := strings.Replace(string(original), test.from, test.to, 1)
			if os.WriteFile(f.getenv("FIXTURE_REGISTRY"), []byte(data), 0600) != nil {
				t.Fatal("write fixture")
			}
			if _, err := f.command("verify", revision, digest); err == nil {
				t.Fatal("incomplete or inconsistent artifact must fail inspection")
			}
		})
	}
}

func TestMainPromotion(t *testing.T) {
	for _, test := range []struct{ name, changedHead string }{
		{"current main", ""},
		{"main advances during inspection", strings.Repeat("2", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture(t)
			data, _ := os.ReadFile("testdata/registry.json")
			if os.WriteFile(f.getenv("FIXTURE_REGISTRY"), data, 0600) != nil {
				t.Fatal("write fixture")
			}
			f.setenv("FIXTURE_AFTER_INSPECT_HEAD", test.changedHead)
			output, err := f.command("promote", digest)
			writes, readErr := os.ReadFile(filepath.Join(f.dir, "writes"))
			if test.changedHead != "" {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatal("stale promotion must fail without registry writes")
				}
				return
			}
			if err != nil || readErr != nil || !strings.Contains(string(writes), "--tag ghcr.io/yaasha/near-e2ee-proxy:main ghcr.io/yaasha/near-e2ee-proxy@"+digest) || !strings.Contains(string(output), digest) {
				t.Fatalf("main must reference the inspected digest: %s", output)
			}
		})
	}
}

func TestPackageMustBePublicAndLinkedBeforePromotion(t *testing.T) {
	for _, data := range []string{
		`{"visibility":"private","repository":{"full_name":"Yaasha/near-e2ee-proxy"}}`,
		`{"visibility":"public","repository":{"full_name":"another/repository"}}`,
		`{"visibility":"public","repository":null}`,
		`not json`,
	} {
		f := fixture(t)
		registry, _ := os.ReadFile("testdata/registry.json")
		if os.WriteFile(f.getenv("FIXTURE_REGISTRY"), registry, 0600) != nil || os.WriteFile(f.getenv("FIXTURE_PACKAGE"), []byte(data), 0600) != nil {
			t.Fatal("write fixtures")
		}
		_, err := f.command("promote", digest)
		_, writeErr := os.Stat(f.getenv("FIXTURE_WRITES"))
		if err == nil || !os.IsNotExist(writeErr) {
			t.Fatal("unreadable, private or unlinked package must block main-tag promotion")
		}
	}
}

func TestReleaseSourceEligibility(t *testing.T) {
	for _, test := range []struct {
		name, event, ref, setting, value string
		allowed                          bool
	}{
		{"stable push", "push", "refs/tags/v1.2.3", "", "", true},
		{"prerelease push", "push", "refs/tags/v1.2.3-rc.1", "", "", true},
		{"manual outside main", "workflow_dispatch", "refs/tags/v1.2.3", "FIXTURE_ANCESTRY_EXIT", "1", false},
		{"manual malformed", "workflow_dispatch", "refs/tags/v01.2.3", "", "", false},
		{"manual failed validation", "workflow_dispatch", "refs/tags/v1.2.3", "VALIDATION_RESULTS", `{}`, false},
		{"manual tag", "workflow_dispatch", "refs/tags/v1.2.3", "", "", true},
		{"PR tag", "pull_request", "refs/tags/v1.2.3", "", "", false},
		{"missing prefix", "push", "refs/tags/1.2.3", "", "", false},
		{"build metadata", "push", "refs/tags/v1.2.3+build.1", "", "", false},
		{"empty prerelease", "push", "refs/tags/v1.2.3-", "", "", false},
		{"empty identifier", "push", "refs/tags/v1.2.3-rc..1", "", "", false},
		{"non ASCII", "push", "refs/tags/v1.2.3-ré", "", "", false},
		{"extra component", "push", "refs/tags/v1.2.3.4", "", "", false},
		{"overlong", "push", "refs/tags/v1.2.3-" + strings.Repeat("a", 125), "", "", false},
		{"incomplete", "push", "refs/tags/v1.2", "", "", false},
		{"leading zero", "push", "refs/tags/v01.2.3", "", "", false},
		{"prerelease leading zero", "push", "refs/tags/v1.2.3-rc.01", "", "", false},
		{"outside main", "push", "refs/tags/v1.2.3", "FIXTURE_ANCESTRY_EXIT", "1", false},
		{"moved tag", "push", "refs/tags/v1.2.3", "FIXTURE_TAG_COMMIT", strings.Repeat("2", 40), false},
		{"source unavailable", "push", "refs/tags/v1.2.3", "FIXTURE_FETCH_EXIT", "1", false},
		{"failed validation", "push", "refs/tags/v1.2.3", "VALIDATION_RESULTS", `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture(t)
			f.setenv("GITHUB_EVENT_NAME", test.event)
			f.setenv("GITHUB_REF", test.ref)
			if test.setting != "" {
				f.setenv(test.setting, test.value)
			}
			out, err := f.command("gate")
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v want %v: %s", err == nil, test.allowed, out)
			}
		})
	}
}
