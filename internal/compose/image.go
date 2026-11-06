package compose

import (
	"strings"

	"composesafe/internal/version"
)

// ParseRef parses "registry:5000/group/name:tag@sha256:...". A missing
// registry means docker.io; a single-segment docker.io repo gets "library/".
// A missing tag and digest means "latest".
func ParseRef(raw string) ImageRef {
	ref := ImageRef{Raw: raw}
	s := raw
	if i := strings.Index(s, "@"); i >= 0 {
		ref.Digest = s[i+1:]
		s = s[:i]
	}
	// The tag is whatever follows a colon after the last slash.
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		ref.Tag = s[i+1:]
		s = s[:i]
	}
	ref.Registry = "docker.io"
	if i := strings.Index(s, "/"); i >= 0 {
		first := s[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			ref.Registry = first
			s = s[i+1:]
		}
	}
	if ref.Registry == "docker.io" && !strings.Contains(s, "/") {
		s = "library/" + s
	}
	ref.Repo = s
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	if ref.Tag != "" {
		ref.Version, ref.Variant = version.ParseTag(ref.Tag)
	}
	return ref
}

// WithTag returns raw with its tag replaced by tag and any digest dropped.
// The registry and repository text is kept exactly as the user wrote it, so
// the result can go straight back into a compose file.
func WithTag(raw, tag string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndex(raw, ":"); i > strings.LastIndex(raw, "/") {
		raw = raw[:i]
	}
	return raw + ":" + tag
}
