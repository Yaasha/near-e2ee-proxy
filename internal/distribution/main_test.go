package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const revision = "1111111111111111111111111111111111111111"
const digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// Run the real CLI entry point in a subprocess. Only the external GitHub,
// Git and Docker commands are replaced; ordinary tests never contact a service.
func TestCommandHelper(t *testing.T) {
	if os.Getenv("DISTRIBUTION_TEST_COMMAND") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"distribution"}, os.Args[i+1:]...)
			installRegistryFixture()
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{
		"gh": `#!/bin/sh
if [ "$2" = users/Yaasha/packages/container/near-e2ee-proxy ]; then
  cat "$FIXTURE_PACKAGE"
elif [ -f "$FIXTURE_HEAD_FILE" ]; then
  cat "$FIXTURE_HEAD_FILE"
else
  printf '%s\n' "$FIXTURE_HEAD"
fi
`,
		"git": `#!/bin/sh
case "$1" in
  fetch) exit "${FIXTURE_FETCH_EXIT:-0}" ;;
  merge-base) exit "${FIXTURE_ANCESTRY_EXIT:-0}" ;;
  rev-parse)
    if [ "$2" = 'FETCH_HEAD^{commit}' ]; then printf '%s\n' "$FIXTURE_TAG_COMMIT"; else printf '%s\n' "$FIXTURE_CHECKOUT"; fi ;;
esac
`,
		"docker": `#!/bin/sh
if [ "$3" = create ]; then
  printf '%s\n' "$*" >> "$FIXTURE_WRITES"
elif [ "$6" = '{{.Manifest.Digest}}' ]; then
  printf '%s\n' "$FIXTURE_DIGEST"
else
  cat "$FIXTURE_REGISTRY"
  if [ -n "$FIXTURE_AFTER_INSPECT_HEAD" ]; then printf '%s\n' "$FIXTURE_AFTER_INSPECT_HEAD" > "$FIXTURE_HEAD_FILE"; fi
fi
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISTRIBUTION_TEST_COMMAND", "1")
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary"))
	t.Setenv("GITHUB_EVENT_NAME", "push")
	t.Setenv("GITHUB_REPOSITORY", "Yaasha/near-e2ee-proxy")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_SHA", revision)
	t.Setenv("VALIDATION_RESULTS", `{"check":{"result":"success"},"containers":{"result":"success"}}`)
	t.Setenv("FIXTURE_HEAD", revision)
	t.Setenv("FIXTURE_HEAD_FILE", filepath.Join(dir, "head"))
	t.Setenv("FIXTURE_AFTER_INSPECT_HEAD", "")
	t.Setenv("FIXTURE_DIGEST", digest)
	t.Setenv("FIXTURE_CHECKOUT", revision)
	t.Setenv("FIXTURE_TAG_COMMIT", revision)
	t.Setenv("FIXTURE_ANCESTRY_EXIT", "0")
	t.Setenv("FIXTURE_FETCH_EXIT", "0")
	t.Setenv("FIXTURE_WRITES", filepath.Join(dir, "writes"))
	t.Setenv("FIXTURE_REGISTRY", filepath.Join(dir, "registry.json"))
	t.Setenv("FIXTURE_PACKAGE", filepath.Join(dir, "package.json"))
	if os.WriteFile(os.Getenv("FIXTURE_PACKAGE"), []byte(`{"visibility":"public","repository":{"full_name":"Yaasha/near-e2ee-proxy"}}`), 0600) != nil {
		t.Fatal("write package fixture")
	}
	return dir
}

func command(args ...string) ([]byte, error) {
	return exec.Command(os.Args[0], append([]string{"-test.run=^TestCommandHelper$", "--"}, args...)...).CombinedOutput()
}

func TestTrustedMainCanPublish(t *testing.T) {
	fixture(t)
	output, err := command("gate")
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
			dir := fixture(t)
			t.Setenv(test.setting, test.value)
			_, err := command("gate")
			if (err == nil) != test.allowed {
				t.Fatalf("publication allowed=%v, want %v", err == nil, test.allowed)
			}
			if _, err := os.Stat(filepath.Join(dir, "writes")); !os.IsNotExist(err) {
				t.Fatal("eligibility check must never write to the registry")
			}
		})
	}
}

func TestInspectPublishedImage(t *testing.T) {
	fixture(t)
	data, err := os.ReadFile("testdata/registry.json")
	if err != nil || os.WriteFile(os.Getenv("FIXTURE_REGISTRY"), data, 0600) != nil {
		t.Fatal("prepare registry fixture")
	}
	output, err := command("verify", revision, digest)
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
			fixture(t)
			data := strings.Replace(string(original), test.from, test.to, 1)
			if os.WriteFile(os.Getenv("FIXTURE_REGISTRY"), []byte(data), 0600) != nil {
				t.Fatal("write fixture")
			}
			if _, err := command("verify", revision, digest); err == nil {
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
			dir := fixture(t)
			data, _ := os.ReadFile("testdata/registry.json")
			if os.WriteFile(os.Getenv("FIXTURE_REGISTRY"), data, 0600) != nil {
				t.Fatal("write fixture")
			}
			t.Setenv("FIXTURE_AFTER_INSPECT_HEAD", test.changedHead)
			output, err := command("promote", digest)
			writes, readErr := os.ReadFile(filepath.Join(dir, "writes"))
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
		fixture(t)
		registry, _ := os.ReadFile("testdata/registry.json")
		if os.WriteFile(os.Getenv("FIXTURE_REGISTRY"), registry, 0600) != nil || os.WriteFile(os.Getenv("FIXTURE_PACKAGE"), []byte(data), 0600) != nil {
			t.Fatal("write fixtures")
		}
		_, err := command("promote", digest)
		_, writeErr := os.Stat(os.Getenv("FIXTURE_WRITES"))
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
			fixture(t)
			t.Setenv("GITHUB_EVENT_NAME", test.event)
			t.Setenv("GITHUB_REF", test.ref)
			if test.setting != "" {
				t.Setenv(test.setting, test.value)
			}
			out, err := command("gate")
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v want %v: %s", err == nil, test.allowed, out)
			}
		})
	}
}
