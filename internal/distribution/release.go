package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func promoteRelease(ctx context.Context, digest string, recovery bool) error {
	v, err := releaseVersion(strings.TrimPrefix(os.Getenv("GITHUB_REF"), "refs/tags/"))
	if err != nil {
		return err
	}
	if !digestPattern.MatchString(digest) {
		return errors.New("invalid release digest")
	}
	if recovery && os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" {
		return errors.New("recovery requires manual dispatch")
	}
	if err := verifyVersion(ctx, os.Getenv("GITHUB_SHA"), digest, v.full); err != nil {
		return err
	}
	if err := publicPackage(ctx); err != nil {
		return err
	}
	// The workflow holds ghcr-publication across these reads, the candidate build,
	// full-version creation and all alias writes. No other publisher may bypass it.
	if err := gate(ctx); err != nil {
		return err
	}
	existing, err := lookup(ctx, v.full)
	if err != nil {
		return err
	}
	if recovery {
		if existing != digest {
			return errors.New("recovery requires the preserved full version to match the exact digest")
		}
	} else if existing != "" {
		return errors.New("full-version image already exists; replacement refused; use manual digest recovery for aliases")
	}
	tags := []string{}
	if !recovery {
		tags = append(tags, v.full)
	}
	if !v.prerelease {
		for _, alias := range []string{v.minor, "latest"} {
			advance, err := aliasCanAdvance(ctx, alias, v, digest)
			if err != nil {
				return err
			}
			if advance {
				tags = append(tags, alias)
			} else {
				fmt.Printf("Preserved newer or equal stable alias %s\n", alias)
			}
		}
	}
	for _, tag := range tags {
		if err := writeTag(ctx, tag, digest); err != nil {
			return err
		}
		fmt.Printf("Published %s:%s @ %s\n", image, tag, digest)
	}
	fmt.Printf("Release %s; source %s; image digest: %s\n", v.full, os.Getenv("GITHUB_SHA"), digest)
	fmt.Println("Verified linux/amd64 and linux/arm64 OCI metadata, SLSA v1 provenance and SPDX SBOM. Publication does not deploy the service.")
	return nil
}

func aliasCanAdvance(ctx context.Context, alias string, next version, nextDigest string) (bool, error) {
	d, err := lookup(ctx, alias)
	if err != nil {
		return false, err
	}
	if d == "" {
		return true, nil
	}
	result, err := inspect(ctx, d)
	if err != nil {
		return false, err
	}
	labels := result.Images["linux/amd64"].Config.Labels
	current, err := releaseVersion("v" + labels["org.opencontainers.image.version"])
	if err != nil || current.prerelease || (alias != "latest" && current.minor != alias) {
		return false, errors.New("stable alias must reference a stable release in its expected line")
	}
	// Both platform labels and attachments must describe the same preserved full
	// release. Unknown, development or prerelease targets cannot authorize a write.
	if err := verifyArtifact(result, labels["org.opencontainers.image.revision"], current.full); err != nil {
		return false, err
	}
	full, err := lookup(ctx, current.full)
	if err != nil {
		return false, err
	}
	if full != d {
		return false, errors.New("stable alias does not match its preserved full-version image")
	}
	if current.full == next.full && d != nextDigest {
		return false, errors.New("equal version has a different digest")
	}
	return next.newerThan(current), nil
}

func writeTag(ctx context.Context, tag, digest string) error {
	if _, err := output(ctx, "docker", "buildx", "imagetools", "create", "--tag", image+":"+tag, image+"@"+digest); err != nil {
		return err
	}
	actual, err := lookup(ctx, tag)
	if err != nil {
		return err
	}
	if actual != digest {
		return errors.New("published tag does not match inspected digest")
	}
	return nil
}

var runNumberPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func prepare(ctx context.Context) error {
	if err := gate(ctx); err != nil {
		return err
	}
	version, buildTag := "sha-"+os.Getenv("GITHUB_SHA"), "sha-"+os.Getenv("GITHUB_SHA")
	build := true
	recovery := os.Getenv("RECOVER_DIGEST")
	if strings.HasPrefix(os.Getenv("GITHUB_REF"), "refs/tags/") {
		v, err := releaseVersion(strings.TrimPrefix(os.Getenv("GITHUB_REF"), "refs/tags/"))
		if err != nil {
			return err
		}
		version = v.full
		id, attempt := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT")
		if !runNumberPattern.MatchString(id) || !runNumberPattern.MatchString(attempt) {
			return errors.New("candidate requires workflow run ID and attempt")
		}
		buildTag = "candidate-" + id + "-" + attempt
		existing, err := lookup(ctx, v.full)
		if err != nil {
			return err
		}
		if recovery != "" {
			if os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || !digestPattern.MatchString(recovery) || existing != recovery {
				return errors.New("manual recovery requires the preserved full version's exact digest")
			}
			build = false
		} else if existing != "" {
			return errors.New("full-version image already exists; replacement refused; use manual digest recovery for aliases")
		}
	} else if recovery != "" {
		return errors.New("recovery requires a version tag")
	}
	fmt.Printf("version=%s\nbuild_tag=%s\nbuild=%t\n", version, buildTag, build)
	return nil
}
