package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseGateWithRealGitAncestry(t *testing.T) {
	for _, tc := range []struct {
		name               string
		outside, annotated bool
	}{
		{"lightweight ancestor", false, false}, {"annotated ancestor", false, true}, {"unmerged branch", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary := fixture(t)
			// Git is exercised against a temporary repository, including fetch and tag
			// peeling. Only GitHub's current-main query remains an external fixture.
			if err := os.Remove(filepath.Join(boundary, "git")); err != nil {
				t.Fatal(err)
			}
			repo := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				options := []string{"-C", repo, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
				out, err := exec.Command("git", append(options, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("synthetic Git setup failed: %s", out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-b", "main")
			git("commit", "--allow-empty", "-m", "main base")
			main := git("rev-parse", "HEAD")
			if tc.outside {
				git("checkout", "-b", "unmerged")
				git("commit", "--allow-empty", "-m", "unmerged source")
			}
			tagged := git("rev-parse", "HEAD")
			if tc.annotated {
				git("tag", "-a", "v1.2.3", "-m", "synthetic release")
			} else {
				git("tag", "v1.2.3")
			}
			if !tc.outside {
				git("commit", "--allow-empty", "-m", "main descendant")
				main = git("rev-parse", "HEAD")
			}
			git("remote", "add", "origin", repo)
			git("checkout", "--detach", tagged)
			t.Setenv("GITHUB_SHA", tagged)
			t.Setenv("GITHUB_REF", "refs/tags/v1.2.3")
			t.Setenv("FIXTURE_HEAD", main)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCommandHelper$", "--", "gate")
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			if (err == nil) == tc.outside {
				t.Fatalf("outside=%v: %s", tc.outside, out)
			}
		})
	}
}
