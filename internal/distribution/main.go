// Command distribution guards development image publication and inspects GHCR.
// It is CI tooling, excluded from the application's container build context.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "gate" {
		if err := gate(ctx); err != nil {
			return err
		}
		fmt.Println("eligible")
		return nil
	}
	if len(args) == 3 && args[0] == "verify" {
		if err := verify(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Println("verified linux/amd64 and linux/arm64 metadata, provenance and SBOM")
		return nil
	}
	if len(args) == 2 && args[0] == "promote" {
		return promote(ctx, args[1])
	}
	return errors.New("usage: distribution gate | verify <revision> <digest> | promote <digest>")
}

func promote(ctx context.Context, digest string) error {
	if err := gate(ctx); err != nil {
		return err
	}
	if err := verify(ctx, os.Getenv("GITHUB_SHA"), digest); err != nil {
		return err
	}
	// The workflow holds the shared publication concurrency lock. Check again
	// immediately before the mutable write: inspection may have taken minutes.
	// A later publisher cannot run concurrently and an older rerun fails here.
	if err := gate(ctx); err != nil {
		return err
	}
	if _, err := output(ctx, "docker", "buildx", "imagetools", "create", "--tag", image+":main", image+"@"+digest); err != nil {
		return err
	}
	actual, err := output(ctx, "docker", "buildx", "imagetools", "inspect", image+":main", "--format", "{{.Manifest.Digest}}")
	if err != nil || strings.TrimSpace(string(actual)) != digest {
		return errors.New("main tag does not match the inspected digest")
	}
	fmt.Printf("Published %s:main and %s:sha-%s\nImage digest: %s\n", image, image, os.Getenv("GITHUB_SHA"), digest)
	return nil
}

func gate(ctx context.Context) error {
	if os.Getenv("GITHUB_REPOSITORY") != repository || os.Getenv("GITHUB_REF") != "refs/heads/main" || !commitPattern.MatchString(os.Getenv("GITHUB_SHA")) {
		return errors.New("publication requires trusted main source")
	}
	if event := os.Getenv("GITHUB_EVENT_NAME"); event != "push" && event != "workflow_dispatch" {
		return errors.New("event cannot publish development images")
	}
	var checks map[string]struct{ Result string }
	if json.Unmarshal([]byte(os.Getenv("VALIDATION_RESULTS")), &checks) != nil || checks["check"].Result != "success" || checks["containers"].Result != "success" {
		return errors.New("all validation jobs must succeed before publication")
	}
	checkout, err := output(ctx, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(checkout)) != os.Getenv("GITHUB_SHA") {
		return errors.New("checkout does not match validated source")
	}
	head, err := output(ctx, "gh", "api", "repos/"+repository+"/git/ref/heads/main", "--jq", ".object.sha")
	if err != nil || strings.TrimSpace(string(head)) != os.Getenv("GITHUB_SHA") {
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
			BuildType string
			Materials []struct {
				URI    string
				Digest map[string]string
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

func verify(ctx context.Context, revision, digest string) error {
	if !commitPattern.MatchString(revision) || !digestPattern.MatchString(digest) {
		return errors.New("expected a full source revision and sha256 image digest")
	}
	data, err := output(ctx, "docker", "buildx", "imagetools", "inspect", image+"@"+digest, "--format", inspection)
	if err != nil {
		return err
	}
	var result registryImage
	if json.Unmarshal(data, &result) != nil || result.Manifest.Digest != digest {
		return errors.New("registry returned invalid image metadata")
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
			"source": source, "revision": revision, "version": "sha-" + revision, "licenses": "MIT",
		} {
			if labels["org.opencontainers.image."+name] != value {
				return fmt.Errorf("incorrect %s %s label", platform, name)
			}
		}
		if _, err := time.Parse(time.RFC3339, labels["org.opencontainers.image.created"]); err != nil {
			return fmt.Errorf("invalid %s creation timestamp", platform)
		}
		provenance := result.Provenance[platform].SLSA
		boundSource := false
		for _, material := range provenance.Materials {
			uri, _, _ := strings.Cut(strings.TrimPrefix(material.URI, "git+"), "#")
			if uri == source+".git" && material.Digest["sha1"] == revision {
				boundSource = true
			}
		}
		if provenance.BuildType != "https://mobyproject.org/buildkit@v1" || !boundSource {
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
