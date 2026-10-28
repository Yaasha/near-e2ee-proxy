package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

func (d *distribution) promoteRelease(ctx context.Context, digest string, recovery bool) error {
	v, err := releaseVersion(strings.TrimPrefix(d.getenv("GITHUB_REF"), "refs/tags/"))
	if err != nil {
		return err
	}
	if !digestPattern.MatchString(digest) {
		return errors.New("invalid release digest")
	}
	if recovery && d.getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" {
		return errors.New("recovery requires manual dispatch")
	}
	if err := d.verifyVersion(ctx, d.getenv("GITHUB_SHA"), digest, v.full); err != nil {
		return err
	}
	if err := d.publicPackage(ctx); err != nil {
		return err
	}
	// The workflow holds ghcr-publication across these reads, the candidate build,
	// full-version creation and all alias writes. No other publisher may bypass it.
	if err := d.gate(ctx); err != nil {
		return err
	}
	existing, err := d.lookup(ctx, v.full)
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
			advance, err := d.aliasCanAdvance(ctx, alias, v, digest)
			if err != nil {
				return err
			}
			if advance {
				tags = append(tags, alias)
			} else {
				fmt.Fprintf(d.stdout, "Preserved newer or equal stable alias %s\n", alias)
			}
		}
	}
	for _, tag := range tags {
		if err := d.writeTag(ctx, tag, digest); err != nil {
			return err
		}
		fmt.Fprintf(d.stdout, "Published %s:%s @ %s\n", image, tag, digest)
	}
	fmt.Fprintf(d.stdout, "Release %s; source %s; image digest: %s\n", v.full, d.getenv("GITHUB_SHA"), digest)
	fmt.Fprintln(d.stdout, "Verified linux/amd64 and linux/arm64 OCI metadata, SLSA v1 provenance and SPDX SBOM. Publication does not deploy the service.")
	return nil
}

func (d *distribution) aliasCanAdvance(ctx context.Context, alias string, next version, nextDigest string) (bool, error) {
	currentDigest, err := d.lookup(ctx, alias)
	if err != nil {
		return false, err
	}
	if currentDigest == "" {
		return true, nil
	}
	result, err := d.inspect(ctx, currentDigest)
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
	full, err := d.lookup(ctx, current.full)
	if err != nil {
		return false, err
	}
	if full != currentDigest {
		return false, errors.New("stable alias does not match its preserved full-version image")
	}
	if current.full == next.full && currentDigest != nextDigest {
		return false, errors.New("equal version has a different digest")
	}
	return next.newerThan(current), nil
}

func (d *distribution) writeTag(ctx context.Context, tag, digest string) error {
	if _, err := d.command(ctx, "docker", "buildx", "imagetools", "create", "--tag", image+":"+tag, image+"@"+digest); err != nil {
		return err
	}
	actual, err := d.lookup(ctx, tag)
	if err != nil {
		return err
	}
	if actual != digest {
		return errors.New("published tag does not match inspected digest")
	}
	return nil
}

var runNumberPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func (d *distribution) prepare(ctx context.Context) error {
	if err := d.gate(ctx); err != nil {
		return err
	}
	version, buildTag := "sha-"+d.getenv("GITHUB_SHA"), "sha-"+d.getenv("GITHUB_SHA")
	build := true
	recovery := d.getenv("RECOVER_DIGEST")
	if strings.HasPrefix(d.getenv("GITHUB_REF"), "refs/tags/") {
		v, err := releaseVersion(strings.TrimPrefix(d.getenv("GITHUB_REF"), "refs/tags/"))
		if err != nil {
			return err
		}
		version = v.full
		id, attempt := d.getenv("GITHUB_RUN_ID"), d.getenv("GITHUB_RUN_ATTEMPT")
		if !runNumberPattern.MatchString(id) || !runNumberPattern.MatchString(attempt) {
			return errors.New("candidate requires workflow run ID and attempt")
		}
		buildTag = "candidate-" + id + "-" + attempt
		existing, err := d.lookup(ctx, v.full)
		if err != nil {
			return err
		}
		if recovery != "" {
			if d.getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || !digestPattern.MatchString(recovery) || existing != recovery {
				return errors.New("manual recovery requires the preserved full version's exact digest")
			}
			build = false
		} else if existing != "" {
			return errors.New("full-version image already exists; replacement refused; use manual digest recovery for aliases")
		}
	} else if recovery != "" {
		return errors.New("recovery requires a version tag")
	}
	fmt.Fprintf(d.stdout, "version=%s\nbuild_tag=%s\nbuild=%t\n", version, buildTag, build)
	return nil
}
