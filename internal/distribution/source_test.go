package main

import (
	"strings"
	"testing"
)

// Synthetic responses exercise source admission through the same command entry
// as publication. Real Git tag peeling is also exercised by hosted tag releases.
func TestReleaseAncestorMayDifferFromCurrentMain(t *testing.T) {
	for _, tc := range []struct {
		name, ancestry string
		allowed        bool
	}{
		{"reachable ancestor", "0", true}, {"unmerged source", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			f.setenv("GITHUB_REF", "refs/tags/v1.2.3")
			f.setenv("FIXTURE_HEAD", strings.Repeat("2", 40))
			f.setenv("FIXTURE_ANCESTRY_EXIT", tc.ancestry)
			out, err := f.command("gate")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v want %v: %s", err == nil, tc.allowed, out)
			}
		})
	}
}
