// Command distribution guards image publication and inspects GHCR.
// It is CI tooling, excluded from the application's container build context.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const repository = "Yaasha/near-e2ee-proxy"
const image = "ghcr.io/yaasha/near-e2ee-proxy"
const source = "https://github.com/" + repository

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// distribution runs the publication command with explicit process boundaries.
// Production uses the real tools and fixed registry; tests supply synthetic
// command/HTTP responses without starting child processes or changing globals.
type distribution struct {
	getenv         func(string) string
	command        func(context.Context, string, ...string) ([]byte, error)
	registry       *http.Client
	stdout, stderr io.Writer
}

func newDistribution() *distribution {
	return &distribution{getenv: os.Getenv, command: output, registry: newRegistryClient(), stdout: os.Stdout, stderr: os.Stderr}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := newDistribution().execute(ctx, os.Args[1:]); err != nil {
		os.Exit(1)
	}
}

func (d *distribution) execute(ctx context.Context, args []string) error {
	err := d.run(ctx, args)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		if path := d.getenv("GITHUB_STEP_SUMMARY"); path != "" {
			if summary, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); openErr == nil {
				fmt.Fprintf(summary, "Publication refused: %s\n", err)
				summary.Close()
			}
		}
	}
	return err
}

func (d *distribution) run(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "prepare" {
		return d.prepare(ctx)
	}
	if len(args) == 2 && args[0] == "recover" {
		if err := d.gate(ctx); err != nil {
			return err
		}
		return d.promoteRelease(ctx, args[1], true)
	}
	if len(args) == 1 && args[0] == "gate" {
		if err := d.gate(ctx); err != nil {
			return err
		}
		fmt.Fprintln(d.stdout, "eligible")
		return nil
	}
	if len(args) == 3 && args[0] == "verify" {
		if err := d.verify(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Fprintln(d.stdout, "verified linux/amd64 and linux/arm64 metadata, provenance and SBOM")
		return nil
	}
	if len(args) == 2 && args[0] == "promote" {
		return d.promote(ctx, args[1])
	}
	return errors.New("usage: distribution gate | prepare | verify <revision> <digest> | promote <digest> | recover <digest>")
}

func (d *distribution) promote(ctx context.Context, digest string) error {
	if err := d.gate(ctx); err != nil {
		return err
	}
	if strings.HasPrefix(d.getenv("GITHUB_REF"), "refs/tags/") {
		return d.promoteRelease(ctx, digest, false)
	}
	if err := d.verify(ctx, d.getenv("GITHUB_SHA"), digest); err != nil {
		return err
	}
	if err := d.publicPackage(ctx); err != nil {
		return err
	}
	// The workflow holds the shared publication concurrency lock. Check again
	// immediately before the mutable write: inspection may have taken minutes.
	// A later publisher cannot run concurrently and an older rerun fails here.
	if err := d.gate(ctx); err != nil {
		return err
	}
	if _, err := d.command(ctx, "docker", "buildx", "imagetools", "create", "--tag", image+":main", image+"@"+digest); err != nil {
		return err
	}
	actual, err := d.command(ctx, "docker", "buildx", "imagetools", "inspect", image+":main", "--format", "{{.Manifest.Digest}}")
	if err != nil || strings.TrimSpace(string(actual)) != digest {
		return errors.New("main tag does not match the inspected digest")
	}
	fmt.Fprintf(d.stdout, "Published %s:main and %s:sha-%s\nImage digest: %s\n", image, image, d.getenv("GITHUB_SHA"), digest)
	fmt.Fprintln(d.stdout, "Verified linux/amd64 and linux/arm64 metadata, SLSA v1 provenance and SPDX SBOM. GHCR package is public and linked to "+repository+".")
	return nil
}

func (d *distribution) publicPackage(ctx context.Context) error {
	// Use only the workflow's package token, never a separate long-lived key.
	// Public source alone is insufficient: confirm GHCR's actual package state.
	data, err := d.command(ctx, "gh", "api", "users/Yaasha/packages/container/near-e2ee-proxy")
	if err != nil {
		return err
	}
	var pkg struct {
		Visibility string
		Repository struct {
			FullName string `json:"full_name"`
		}
	}
	if json.Unmarshal(data, &pkg) != nil || pkg.Visibility != "public" || pkg.Repository.FullName != repository {
		return errors.New("GHCR package must be public and linked to the source repository before promotion")
	}
	return nil
}

func (d *distribution) gate(ctx context.Context) error {
	if d.getenv("GITHUB_REPOSITORY") != repository || !commitPattern.MatchString(d.getenv("GITHUB_SHA")) {
		return errors.New("publication requires trusted repository and source revision")
	}
	if event := d.getenv("GITHUB_EVENT_NAME"); event != "push" && event != "workflow_dispatch" {
		return errors.New("event cannot publish images")
	}
	var checks map[string]struct{ Result string }
	if json.Unmarshal([]byte(d.getenv("VALIDATION_RESULTS")), &checks) != nil || checks["check"].Result != "success" || checks["containers"].Result != "success" {
		return errors.New("all validation jobs must succeed before publication")
	}
	checkout, err := d.command(ctx, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(checkout)) != d.getenv("GITHUB_SHA") {
		return errors.New("checkout does not match validated source")
	}
	ref := d.getenv("GITHUB_REF")
	if ref != "refs/heads/main" {
		if !strings.HasPrefix(ref, "refs/tags/") {
			return errors.New("publication requires main or a version tag")
		}
		if _, err := releaseVersion(strings.TrimPrefix(ref, "refs/tags/")); err != nil {
			return err
		}
		// Fetch the current remote ref and peel annotated tags. Never label an
		// artifact with an event revision when its tag now resolves elsewhere.
		if _, err := d.command(ctx, "git", "fetch", "--no-tags", "origin", ref); err != nil {
			return err
		}
		tagged, err := d.command(ctx, "git", "rev-parse", "FETCH_HEAD^{commit}")
		if err != nil || strings.TrimSpace(string(tagged)) != d.getenv("GITHUB_SHA") {
			return errors.New("release tag no longer matches validated source")
		}
	}
	head, err := d.command(ctx, "gh", "api", "repos/"+repository+"/git/ref/heads/main", "--jq", ".object.sha")
	if err != nil || !commitPattern.MatchString(strings.TrimSpace(string(head))) {
		return errors.New("main cannot be checked")
	}
	if ref != "refs/heads/main" {
		main := strings.TrimSpace(string(head))
		if _, err := d.command(ctx, "git", "fetch", "--no-tags", "origin", main); err != nil {
			return err
		}
		if _, err := d.command(ctx, "git", "merge-base", "--is-ancestor", d.getenv("GITHUB_SHA"), main); err != nil {
			return errors.New("release commit must be reachable from main")
		}
		return nil
	}
	if strings.TrimSpace(string(head)) != d.getenv("GITHUB_SHA") {
		return errors.New("main has advanced or cannot be checked; refusing stale publication")
	}
	return nil
}

// Buildx loads the actual registry configs and attestation blobs. The command
// checks those contents, not just an attestation annotation or a build flag.
const inspection = `{"manifest":{{json .Manifest}},"images":{{json .Image}},"provenance":{{json .Provenance}},"sbom":{{json .SBOM}}}`

type registryImage struct {
	Manifest struct {
		Digest    string
		Manifests []struct {
			Digest      string
			Platform    struct{ OS, Architecture string }
			Annotations map[string]string
		}
	}
	Images map[string]struct {
		Config struct{ Labels map[string]string }
	}
	Provenance map[string]struct {
		SLSA struct {
			BuildDefinition struct {
				BuildType            string
				ResolvedDependencies []struct {
					URI    string
					Digest map[string]string
				}
			}
		}
	}
	SBOM map[string]struct {
		SPDX struct {
			SPDXVersion string
			SPDXID      string
			Packages    []struct{ Name string }
		}
	}
}

func (d *distribution) verify(ctx context.Context, revision, digest string) error {
	return d.verifyVersion(ctx, revision, digest, "sha-"+revision)
}

func (d *distribution) inspect(ctx context.Context, digest string) (registryImage, error) {
	var result registryImage
	if !digestPattern.MatchString(digest) {
		return result, errors.New("invalid image digest")
	}
	data, err := d.command(ctx, "docker", "buildx", "imagetools", "inspect", image+"@"+digest, "--format", inspection)
	if err != nil {
		return result, err
	}
	if json.Unmarshal(data, &result) != nil || result.Manifest.Digest != digest {
		return result, errors.New("registry returned invalid image metadata")
	}
	return result, nil
}

func (d *distribution) verifyVersion(ctx context.Context, revision, digest, version string) error {
	result, err := d.inspect(ctx, digest)
	if err != nil {
		return err
	}
	return verifyArtifact(result, revision, version)
}

func verifyArtifact(result registryImage, revision, version string) error {
	if !commitPattern.MatchString(revision) {
		return errors.New("expected a full source revision")
	}
	platforms := map[string]bool{}
	for _, manifest := range result.Manifest.Manifests {
		if manifest.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			if manifest.Platform.OS != "unknown" || manifest.Platform.Architecture != "unknown" {
				return errors.New("attestation must not be a runnable platform")
			}
			continue
		}
		platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
		if (platform != "linux/amd64" && platform != "linux/arm64") || platforms[platform] {
			return errors.New("unexpected or duplicate runnable image platform")
		}
		platforms[platform] = true
	}
	for _, arch := range []string{"amd64", "arm64"} {
		platform := "linux/" + arch
		manifestDigest := ""
		for _, manifest := range result.Manifest.Manifests {
			if manifest.Platform.OS == "linux" && manifest.Platform.Architecture == arch {
				manifestDigest = manifest.Digest
			}
		}
		if !digestPattern.MatchString(manifestDigest) {
			return fmt.Errorf("missing %s image", platform)
		}
		attached := false
		for _, manifest := range result.Manifest.Manifests {
			if manifest.Annotations["vnd.docker.reference.type"] == "attestation-manifest" && manifest.Annotations["vnd.docker.reference.digest"] == manifestDigest {
				attached = true
			}
		}
		if !attached {
			return fmt.Errorf("missing %s attestation attachment", platform)
		}
		labels := result.Images[platform].Config.Labels
		for name, value := range map[string]string{
			"title": "NEAR E2EE Proxy", "description": "Verified model-key E2EE Chat Completions proxy",
			"source": source, "revision": revision, "version": version, "licenses": "MIT",
		} {
			if labels["org.opencontainers.image."+name] != value {
				return fmt.Errorf("incorrect %s %s label", platform, name)
			}
		}
		if _, err := time.Parse(time.RFC3339, labels["org.opencontainers.image.created"]); err != nil {
			return fmt.Errorf("invalid %s creation timestamp", platform)
		}
		provenance := result.Provenance[platform].SLSA.BuildDefinition
		boundSource := false
		for _, material := range provenance.ResolvedDependencies {
			uri, _, _ := strings.Cut(strings.TrimPrefix(material.URI, "git+"), "#")
			if uri == source+".git" && material.Digest["sha1"] == revision {
				boundSource = true
			}
		}
		if provenance.BuildType != "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md" || !boundSource {
			return fmt.Errorf("missing %s provenance for expected source", platform)
		}
		sbom := result.SBOM[platform].SPDX
		if !strings.HasPrefix(sbom.SPDXVersion, "SPDX-2.") || sbom.SPDXID != "SPDXRef-DOCUMENT" || len(sbom.Packages) == 0 {
			return fmt.Errorf("missing %s SPDX packages", platform)
		}
	}
	return nil
}

// Registry inspection output can include large SBOMs. Bound it and discard raw
// stderr so CI never echoes credentials or arbitrary upstream errors.
type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 32<<20 {
		return 0, errors.New("command output limit exceeded")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var result boundedOutput
	cmd.Stdout = &result
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s command failed", name)
	}
	return result.data, nil
}
