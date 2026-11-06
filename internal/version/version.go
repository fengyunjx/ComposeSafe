// Package version parses the loose version numbers that appear in image tags
// and tool output ("16.3-alpine", "v2.29.1-desktop.1", "29.8.2").
package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is the numeric prefix of a tag. Parts is how many components the
// tag actually spelled out ("16" has Parts=1, "16.3" has Parts=2).
type Version struct {
	Major, Minor, Patch int
	Parts               int
	Raw                 string
}

var numRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)`)

// ParseTag splits an image tag into its numeric version and the remaining
// variant suffix ("16.3-alpine3.19" -> 16.3, "alpine3.19"). It returns a nil
// version for tags that are not versions: "latest", commit hashes, and date
// tags such as "2024.10.01" (major >= 1000).
func ParseTag(tag string) (*Version, string) {
	m := numRe.FindStringSubmatch(tag)
	if m == nil {
		return nil, ""
	}
	rest := tag[len(m[0]):]
	if rest != "" && !strings.ContainsRune("-_+", rune(rest[0])) {
		return nil, "" // e.g. "3f2a9c1": a hash that happens to start with digits
	}
	parts := strings.Split(m[1], ".")
	nums := make([]int, 3)
	for i, p := range parts {
		if len(p) > 9 {
			return nil, ""
		}
		n, _ := strconv.Atoi(p)
		if i < 3 {
			nums[i] = n
		}
	}
	if nums[0] >= 1000 {
		return nil, ""
	}
	v := &Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Parts: len(parts), Raw: m[1]}
	return v, strings.TrimLeft(rest, "-_+")
}

// Parse parses a version reported by a tool (docker, compose). The variant
// suffix is ignored.
func Parse(s string) (*Version, error) {
	v, _ := ParseTag(strings.TrimSpace(s))
	if v == nil {
		return nil, fmt.Errorf("cannot parse version %q", s)
	}
	return v, nil
}

// Compare returns -1, 0 or 1 comparing the Major.Minor.Patch triple.
func Compare(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return sign(a.Major - b.Major)
	case a.Minor != b.Minor:
		return sign(a.Minor - b.Minor)
	default:
		return sign(a.Patch - b.Patch)
	}
}

// AtLeast reports whether v >= major.minor.
func (v Version) AtLeast(major, minor int) bool {
	return Compare(v, Version{Major: major, Minor: minor}) >= 0
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
