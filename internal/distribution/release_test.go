package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type registryFixture struct {
	sync.Mutex
	tags      map[string]string
	artifacts map[string][]byte
	writes    []string
	faults    map[string]int
	bodies    map[string]string
}

// Replace only the external HTTP boundary in the CLI subprocess, never its
// policy. No production registry URL override or test mode is introduced.
type fixtureTransport struct{ target *url.URL }

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	u := *r.URL
	u.Scheme = f.target.Scheme
	u.Host = f.target.Host
	r.URL = &u
	return (&http.Transport{Proxy: nil, DisableKeepAlives: true}).RoundTrip(r)
}

func TestReleaseDockerHelper(t *testing.T) {
	if os.Getenv("RELEASE_DOCKER_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	endpoint := ""
	if len(args) >= 6 && args[2] == "create" {
		tag := strings.TrimPrefix(args[4], image+":")
		d := strings.TrimPrefix(args[5], image+"@")
		endpoint = "/write?tag=" + url.QueryEscape(tag) + "&digest=" + url.QueryEscape(d)
	} else if len(args) >= 6 && args[2] == "inspect" {
		d := strings.TrimPrefix(args[3], image+"@")
		endpoint = "/artifact/" + d
	} else {
		os.Exit(2)
	}
	resp, err := http.Get(os.Getenv("FIXTURE_REGISTRY_URL") + endpoint)
	if err != nil {
		os.Exit(2)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		os.Exit(2)
	}
	io.Copy(os.Stdout, resp.Body)
	os.Exit(0)
}

func releaseFixture(t *testing.T, tag string) *registryFixture {
	t.Helper()
	dir := fixture(t)
	t.Setenv("GITHUB_REF", "refs/tags/"+tag)
	t.Setenv("GITHUB_ACTOR", "synthetic-actor")
	t.Setenv("GH_TOKEN", "synthetic-workflow-token")
	t.Setenv("RECOVER_DIGEST", "")
	state := &registryFixture{tags: map[string]string{}, artifacts: map[string][]byte{}, faults: map[string]int{}, bodies: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		defer state.Unlock()
		if status := state.faults[r.URL.Path]; status != 0 {
			if status == -1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			w.WriteHeader(status)
			fmt.Fprint(w, state.bodies[r.URL.Path])
			return
		}
		switch {
		case r.URL.Path == "/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "synthetic-actor" || pass != "synthetic-workflow-token" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"token":"synthetic-registry-token"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/yaasha/near-e2ee-proxy/manifests/"):
			if r.Header.Get("Authorization") != "Bearer synthetic-registry-token" {
				w.WriteHeader(401)
				return
			}
			tag := strings.TrimPrefix(r.URL.Path, "/v2/yaasha/near-e2ee-proxy/manifests/")
			if d := state.tags[tag]; d != "" {
				w.Header().Set("Docker-Content-Digest", d)
				fmt.Fprint(w, `{"schemaVersion":2}`)
			} else {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			}
		case r.URL.Path == "/write":
			tag, d := r.URL.Query().Get("tag"), r.URL.Query().Get("digest")
			if status := state.faults["write:"+tag]; status != 0 {
				w.WriteHeader(status)
				return
			}
			state.tags[tag] = d
			state.writes = append(state.writes, tag)
		case strings.HasPrefix(r.URL.Path, "/artifact/"):
			data := state.artifacts[strings.TrimPrefix(r.URL.Path, "/artifact/")]
			if data == nil {
				w.WriteHeader(404)
				return
			}
			w.Write(data)
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("FIXTURE_REGISTRY_URL", server.URL)
	t.Setenv("RELEASE_DOCKER_HELPER", "1")
	t.Setenv("FIXTURE_TEST_BINARY", os.Args[0])
	if os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec \"$FIXTURE_TEST_BINARY\" -test.run=^TestReleaseDockerHelper$ -- \"$@\"\n"), 0700) != nil {
		t.Fatal("write Docker boundary")
	}
	addArtifact(t, state, strings.TrimPrefix(tag, "v"), digest)
	return state
}

func addArtifact(t *testing.T, s *registryFixture, version, d string) {
	t.Helper()
	data, err := os.ReadFile("testdata/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(strings.ReplaceAll(string(data), "sha-"+revision, version), digest, d))
	s.Lock()
	s.artifacts[d] = data
	s.Unlock()
}

func (s *registryFixture) setTag(tag, d string)  { s.Lock(); defer s.Unlock(); s.tags[tag] = d }
func (s *registryFixture) tag(tag string) string { s.Lock(); defer s.Unlock(); return s.tags[tag] }
func (s *registryFixture) written() []string {
	s.Lock()
	defer s.Unlock()
	return append([]string(nil), s.writes...)
}
func (s *registryFixture) setFault(path string, status int, body string) {
	s.Lock()
	defer s.Unlock()
	s.faults[path] = status
	s.bodies[path] = body
}
func (s *registryFixture) changeArtifact(d, from, to string) {
	s.Lock()
	defer s.Unlock()
	s.artifacts[d] = []byte(strings.Replace(string(s.artifacts[d]), from, to, 1))
}

func TestStableReleasePublication(t *testing.T) {
	state := releaseFixture(t, "v1.2.3")
	out, err := command("promote", digest)
	if err != nil {
		t.Fatalf("release must publish: %s", out)
	}
	if got := strings.Join(state.written(), ","); got != "1.2.3,1.2,latest" {
		t.Fatalf("published tags %q", got)
	}
	if !strings.Contains(string(out), digest) {
		t.Fatal("summary must record digest")
	}
}

// Used by the helper process before entering the actual CLI.
func installRegistryFixture() {
	if raw := os.Getenv("FIXTURE_REGISTRY_URL"); raw != "" {
		target, err := url.Parse(raw)
		if err != nil {
			panic(err)
		}
		registryHTTP.Transport = fixtureTransport{target}
	}
}

func TestReleaseRetryAndRecovery(t *testing.T) {
	state := releaseFixture(t, "v1.2.3")
	state.setTag("1.2.3", digest)
	out, err := command("promote", digest)
	if err == nil || !strings.Contains(string(out), "replacement refused") || len(state.written()) != 0 {
		t.Fatalf("retry must refuse replacement: %s", out)
	}
	summary, readErr := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	if readErr != nil || !strings.Contains(string(summary), "replacement refused") {
		t.Fatal("refused replacement must explain itself in the workflow summary")
	}
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	out, err = command("recover", digest)
	if err != nil || strings.Join(state.written(), ",") != "1.2,latest" {
		t.Fatalf("recovery must only repair aliases: %s; %v", out, state.written())
	}
}

func TestReleaseAliasOrdering(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, tag, minor, latest, want string }{
		{"numeric patch", "v1.2.10", "1.2.9", "1.2.9", "1.2.10,1.2,latest"},
		{"older patch", "v1.2.9", "1.2.10", "1.2.10", "1.2.9"},
		{"older minor", "v1.9.1", "1.9.0", "1.10.0", "1.9.1,1.9"},
		{"numeric major", "v10.0.0", "", "9.99.99", "10.0.0,10.0,latest"},
		{"prerelease", "v2.0.0-rc.10", "", "1.9.1", "2.0.0-rc.10"},
		{"stable after prerelease", "v2.0.0", "", "1.9.1", "2.0.0,2.0,latest"},
		{"large numbers", "v99999999999999999999999.0.0", "", "10.0.0", "99999999999999999999999.0.0,99999999999999999999999.0,latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := releaseFixture(t, tc.tag)
			minor := strings.Join(strings.Split(strings.TrimPrefix(tc.tag, "v"), ".")[:2], ".")
			if tc.minor != "" {
				addArtifact(t, state, tc.minor, oldDigest)
				state.setTag(minor, oldDigest)
				state.setTag(tc.minor, oldDigest)
			}
			if tc.latest != "" {
				d := "sha256:" + strings.Repeat("b", 64)
				addArtifact(t, state, tc.latest, d)
				state.setTag("latest", d)
				state.setTag(tc.latest, d)
				if tc.latest == tc.minor {
					state.setTag(minor, d)
				}
			}
			if tc.name == "stable after prerelease" {
				state.setTag("2.0.0-rc.1", oldDigest)
				addArtifact(t, state, "2.0.0-rc.1", oldDigest)
			}
			out, err := command("promote", digest)
			if err != nil || strings.Join(state.written(), ",") != tc.want {
				t.Fatalf("writes=%v want %s: %s", state.written(), tc.want, out)
			}
		})
	}
}

func TestUncertainRegistryStateNeverPublishes(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"authentication", "/token", `{}`, 401},
		{"missing token", "/token", `{}`, 200},
		{"registry forbidden", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{}`, 403},
		{"network failure", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{}`, -1},
		{"registry offline", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{}`, 503},
		{"ambiguous 404", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `not found`, 404},
		{"repository absent", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{"errors":[{"code":"NAME_UNKNOWN"}]}`, 404},
		{"empty errors", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{"errors":[]}`, 404},
		{"missing digest", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{}`, 200},
		{"redirect", "/v2/yaasha/near-e2ee-proxy/manifests/1.2.3", `{}`, 302},
		{"alias lookup failure", "/v2/yaasha/near-e2ee-proxy/manifests/latest", `{}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := releaseFixture(t, "v1.2.3")
			state.setFault(tc.path, tc.status, tc.body)
			out, err := command("promote", digest)
			if err == nil || len(state.written()) != 0 {
				t.Fatalf("uncertain state must block all writes: %s", out)
			}
		})
	}
}

func TestReleaseArtifactsAndStableAliasIntegrity(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"wrong version", "1.2.3", "1.2.4"},
		{"wrong source revision", revision, strings.Repeat("2", 40)},
		{"missing architecture", `"architecture":"arm64"`, `"architecture":"s390x"`},
		{"missing provenance", `"SLSA"`, `"absent"`},
		{"missing SBOM", `"SPDX"`, `"absent"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := releaseFixture(t, "v1.2.3")
			state.changeArtifact(digest, tc.from, tc.to)
			out, err := command("promote", digest)
			if err == nil || len(state.written()) != 0 {
				t.Fatalf("invalid release artifact must block publication: %s", out)
			}
		})
	}
	for _, bad := range []string{"sha-" + revision, "1.2.4-rc.1", "1.3.0"} {
		t.Run("bad minor alias "+bad, func(t *testing.T) {
			state := releaseFixture(t, "v1.2.3")
			d := "sha256:" + strings.Repeat("a", 64)
			addArtifact(t, state, bad, d)
			state.setTag("1.2", d)
			state.setTag(bad, d)
			out, err := command("promote", digest)
			if err == nil || len(state.written()) != 0 {
				t.Fatalf("untrusted alias metadata must block writes: %s", out)
			}
		})
	}
}

func TestSerializedOutOfOrderAndDuplicateReleases(t *testing.T) {
	for _, order := range [][]string{{"v1.2.9", "v1.2.10", "v1.2.9"}, {"v1.2.10", "v1.2.9", "v1.2.10"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			state := releaseFixture(t, order[0])
			seen := map[string]bool{}
			// Model either possible order under the workflow's shared publication lock.
			// Workflow configuration tests separately enforce that every writer holds it.
			for _, tag := range order {
				t.Setenv("GITHUB_REF", "refs/tags/"+tag)
				d := digest
				if tag == "v1.2.9" {
					d = "sha256:" + strings.Repeat("a", 64)
				}
				addArtifact(t, state, strings.TrimPrefix(tag, "v"), d)
				before := len(state.written())
				out, err := command("promote", d)
				if seen[tag] {
					if err == nil || len(state.written()) != before {
						t.Fatal("duplicate must never replace full version")
					}
				} else if err != nil {
					t.Fatalf("new release failed: %s", out)
				}
				seen[tag] = true
			}
			if state.tag("latest") != digest || state.tag("1.2") != digest {
				t.Fatal("out-of-order publication regressed stable aliases")
			}
			if state.tag("1.2.9") != "sha256:"+strings.Repeat("a", 64) || state.tag("1.2.10") != digest {
				t.Fatal("full versions were replaced")
			}
		})
	}
}

func TestPrepareReleaseBuildOrRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, event, existing, recovery string
		allowed, build                  bool
	}{
		{"new", "push", "", "", true, true},
		{"repeated trigger", "push", digest, "", false, false},
		{"manual retry", "workflow_dispatch", digest, "", false, false},
		{"manual recovery", "workflow_dispatch", digest, digest, true, false},
		{"automatic recovery forbidden", "push", digest, digest, false, false},
		{"absent recovery forbidden", "workflow_dispatch", "", digest, false, false},
		{"different recovery forbidden", "workflow_dispatch", digest, "sha256:" + strings.Repeat("a", 64), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := releaseFixture(t, "v1.2.3")
			state.setTag("1.2.3", tc.existing)
			t.Setenv("RECOVER_DIGEST", tc.recovery)
			t.Setenv("GITHUB_EVENT_NAME", tc.event)
			t.Setenv("GITHUB_RUN_ID", "123")
			t.Setenv("GITHUB_RUN_ATTEMPT", "2")
			out, err := command("prepare")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v want %v: %s", err == nil, tc.allowed, out)
			}
			if tc.allowed && (!strings.Contains(string(out), "version=1.2.3\n") || !strings.Contains(string(out), fmt.Sprintf("build=%t\n", tc.build))) {
				t.Fatalf("incorrect build plan: %s", out)
			}
			if len(state.written()) != 0 {
				t.Fatal("preflight must not write")
			}
		})
	}
}

func TestConcurrentPublishersUnderWorkflowLock(t *testing.T) {
	state := releaseFixture(t, "v1.2.10")
	older := "sha256:" + strings.Repeat("a", 64)
	addArtifact(t, state, "1.2.9", older)
	var workflowLock sync.Mutex
	var workers sync.WaitGroup
	failures := make(chan string, 3)
	for _, tag := range []string{"v1.2.9", "v1.2.10", "v1.2.10"} {
		workers.Add(1)
		go func(tag string) {
			defer workers.Done()
			// GitHub provides this cross-run mutex; workflow configuration tests ensure
			// every publisher uses the same concurrency group. Scheduling order varies.
			workflowLock.Lock()
			defer workflowLock.Unlock()
			d := digest
			if tag == "v1.2.9" {
				d = older
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCommandHelper$", "--", "promote", d)
			cmd.Env = append(os.Environ(), "GITHUB_REF=refs/tags/"+tag)
			out, err := cmd.CombinedOutput()
			if err != nil {
				failures <- string(out)
			}
		}(tag)
	}
	workers.Wait()
	close(failures)
	refused := 0
	for out := range failures {
		if !strings.Contains(out, "replacement refused") {
			t.Fatalf("unexpected refusal: %s", out)
		}
		refused++
	}
	if refused != 1 || state.tag("1.2.9") != older || state.tag("1.2.10") != digest || state.tag("latest") != digest || state.tag("1.2") != digest {
		t.Fatal("competing jobs replaced a release or regressed aliases")
	}
	counts := map[string]int{}
	for _, tag := range state.written() {
		counts[tag]++
	}
	if counts["1.2.9"] != 1 || counts["1.2.10"] != 1 {
		t.Fatal("full versions must each be created once")
	}
}

func TestInterruptedPublicationRecoversWithoutReplacingRelease(t *testing.T) {
	state := releaseFixture(t, "v1.2.3")
	state.setFault("write:latest", 500, "")
	out, err := command("promote", digest)
	if err == nil || state.tag("1.2.3") != digest || state.tag("1.2") != digest || state.tag("latest") != "" {
		t.Fatalf("expected partial publication: %s", out)
	}
	before := len(state.written())
	out, err = command("promote", digest)
	if err == nil || len(state.written()) != before {
		t.Fatalf("retry must refuse replacement: %s", out)
	}
	state.setFault("write:latest", 0, "")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	out, err = command("recover", digest)
	if err != nil || strings.Join(state.written(), ",") != "1.2.3,1.2,latest" {
		t.Fatalf("recovery must only write missing alias: %s", out)
	}
	out, err = command("recover", digest)
	if err != nil || strings.Join(state.written(), ",") != "1.2.3,1.2,latest" {
		t.Fatalf("repeated recovery must not write tags: %s", out)
	}
}

func TestExistingPrereleaseCannotBeRebuilt(t *testing.T) {
	state := releaseFixture(t, "v1.3.0-rc.1")
	if out, err := command("promote", digest); err != nil {
		t.Fatalf("new prerelease must publish: %s", out)
	}
	for _, event := range []string{"push", "workflow_dispatch"} {
		t.Setenv("GITHUB_EVENT_NAME", event)
		out, err := command("promote", digest)
		if err == nil || !strings.Contains(string(out), "replacement refused") || strings.Join(state.written(), ",") != "1.3.0-rc.1" {
			t.Fatalf("existing prerelease must stay preserved: %s", out)
		}
	}
}

func TestRecoveryPreservesInterveningNewerRelease(t *testing.T) {
	state := releaseFixture(t, "v1.2.3")
	state.setTag("1.2.3", digest)
	newer := "sha256:" + strings.Repeat("a", 64)
	addArtifact(t, state, "1.2.10", newer)
	for _, tag := range []string{"1.2.10", "1.2", "latest"} {
		state.setTag(tag, newer)
	}
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	out, err := command("recover", digest)
	if err != nil || len(state.written()) != 0 || state.tag("1.2") != newer || state.tag("latest") != newer || state.tag("1.2.3") != digest {
		t.Fatalf("recovery must preserve all published versions and newer aliases: %s", out)
	}
}
