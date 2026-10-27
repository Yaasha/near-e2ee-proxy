package main

import (
	"errors"
	"regexp"
	"strings"
)

// OCI tags cannot contain SemVer build metadata (+). Reject it rather than
// silently mapping two source tags to the same published version.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

type version struct {
	full, minor string
	numbers     []string
	prerelease  bool
}

func releaseVersion(tag string) (version, error) {
	v := strings.TrimPrefix(tag, "v")
	if tag == v || len(v) > 128 || !versionPattern.MatchString(v) {
		return version{}, errors.New("release requires vMAJOR.MINOR.PATCH with optional prerelease, no build metadata, and at most 128 image-tag characters")
	}
	core, pre, hasPre := strings.Cut(v, "-")
	for _, identifier := range strings.Split(pre, ".") {
		numeric := identifier != ""
		for _, c := range identifier {
			if c < '0' || c > '9' {
				numeric = false
			}
		}
		if numeric && len(identifier) > 1 && identifier[0] == '0' {
			return version{}, errors.New("numeric prerelease identifiers cannot have leading zeros")
		}
	}
	numbers := strings.Split(core, ".")
	return version{full: v, minor: numbers[0] + "." + numbers[1], numbers: numbers, prerelease: hasPre}, nil
}

// Only stable candidates reach this comparison. Decimal length then byte order
// compares arbitrary-size SemVer numbers without overflow or lexical 9 > 10.
func (v version) newerThan(other version) bool {
	for i, n := range v.numbers {
		m := other.numbers[i]
		if len(n) != len(m) {
			return len(n) > len(m)
		}
		if n != m {
			return n > m
		}
	}
	return false
}
