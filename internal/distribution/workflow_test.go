package main

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The workflow is the distributed locking/privilege boundary; the CLI cannot
// emulate GitHub's scheduler. Inspect its semantic configuration alongside the
// CLI's stateful, out-of-order publication regressions.
func TestWorkflowPublicationBoundary(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push        struct{ Tags []string }
			PullRequest any `yaml:"pull_request"`
			Dispatch    any `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Permissions map[string]string
		Jobs        map[string]struct {
			Needs       any
			If          string
			Permissions map[string]string
			Concurrency struct {
				Group  string
				Cancel bool `yaml:"cancel-in-progress"`
			}
			Steps []struct {
				Uses, Run, ID, If string
				With              map[string]string
				Env               map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if strings.Join(workflow.On.Push.Tags, ",") != "v*" {
		t.Fatal("version tag pushes must run validation and publication")
	}
	if workflow.Permissions["contents"] != "read" || workflow.Permissions["packages"] != "" {
		t.Fatal("default token must be read-only")
	}
	writers := 0
	for name, job := range workflow.Jobs {
		if job.Permissions["packages"] != "write" {
			continue
		}
		writers++
		if name != "publish" || job.Concurrency.Group != "ghcr-publication" || job.Concurrency.Cancel {
			t.Fatal("all registry writes must share one non-cancellable job and lock")
		}
		if !strings.Contains(job.If, "needs.check.result == 'success'") || !strings.Contains(job.If, "needs.containers.result == 'success'") || !strings.Contains(job.If, "github.event_name == 'push'") || !strings.Contains(job.If, "github.event_name == 'workflow_dispatch'") {
			t.Fatal("publication must require successful validation and trusted events")
		}
		build, prepare, promote := false, false, false
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "./internal/distribution prepare") {
				prepare = true
			}
			if strings.Contains(step.Run, "./internal/distribution promote") && strings.Contains(step.Run, "./internal/distribution recover") {
				promote = true
			}
			if strings.HasPrefix(step.Uses, "docker/build-push-action@") {
				build = true
				if step.If != "steps.plan.outputs.build == 'true'" || step.With["tags"] != "ghcr.io/yaasha/near-e2ee-proxy:${{ steps.plan.outputs.build_tag }}" {
					t.Fatal("build must use guarded candidate/source tag and skip during recovery")
				}
				if step.With["context"] != "https://github.com/Yaasha/near-e2ee-proxy.git#${{ github.sha }}" || step.With["platforms"] != "linux/amd64,linux/arm64" {
					t.Fatal("build must bind validated source and both architectures")
				}
				if step.With["provenance"] != "mode=max,version=v1" || step.With["sbom"] != "true" {
					t.Fatal("release build requires provenance and SBOM")
				}
			}
		}
		if !build || !prepare || !promote {
			t.Fatal("publisher must plan, inspect and promote or explicitly recover")
		}
	}
	if writers != 1 {
		t.Fatal("exactly one serialized publication job is required")
	}
}
