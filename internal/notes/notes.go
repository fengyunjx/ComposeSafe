// Package notes fetches release notes for an upgrade range. Every note keeps
// its URL: the report and the LLM citation check both rely on provenance.
package notes

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"composesafe/internal/version"
)

// maxBody caps one note so a huge changelog cannot blow up reports or LLM
// input. The migration/breaking-change sections come first in practice.
const maxBody = 20000

// maxNotes caps how many notes one range returns (newest kept).
const maxNotes = 30

type ReleaseNote struct {
	Tag         string    `json:"tag"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
}

// ErrRateLimited is returned when GitHub's API limit is hit.
var ErrRateLimited = errors.New("GitHub API rate limit reached (set GITHUB_TOKEN to raise it)")

// Source says where the release notes of an image live.
type Source struct {
	Kind string `json:"kind"` // "github" | "postgres" | "" (unknown)
	Repo string `json:"repo,omitempty"`
}

func (s Source) String() string {
	switch s.Kind {
	case "github":
		return "github.com/" + s.Repo
	case "postgres":
		return "postgresql.org"
	}
	return ""
}

// builtinRepos maps image repositories to their GitHub project, for images
// that carry no source label. Each entry was checked to publish GitHub
// releases with bodies. Extend as users report more.
var builtinRepos = map[string]string{
	"gitea/gitea":              "go-gitea/gitea",
	"library/redis":            "redis/redis",
	"grafana/grafana":          "grafana/grafana",
	"library/traefik":          "traefik/traefik",
	"vaultwarden/server":       "dani-garcia/vaultwarden",
	"louislam/uptime-kuma":     "louislam/uptime-kuma",
	"portainer/portainer-ce":   "portainer/portainer",
	"immich-app/immich-server": "immich-app/immich",
}

var githubURL = regexp.MustCompile(`github\.com[/:]([\w.-]+/[\w.-]+?)(?:\.git)?(?:[/#?].*)?$`)

// ParseGitHub extracts owner/repo from a GitHub URL or "owner/repo".
func ParseGitHub(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if m := githubURL.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	if strings.Count(s, "/") == 1 && !strings.ContainsAny(s, ": ") {
		return s, true
	}
	return "", false
}

// ResolveSource decides where to read release notes from. Precedence: the
// source_repo set in composesafe.yaml, official Postgres, the image's source
// label, then the built-in table.
func ResolveSource(override, imageRepo, label string) Source {
	switch {
	case override == "postgres":
		return Source{Kind: "postgres"}
	case override != "":
		if r, ok := ParseGitHub(override); ok {
			return Source{Kind: "github", Repo: r}
		}
	}
	if imageRepo == "library/postgres" {
		return Source{Kind: "postgres"}
	}
	if r, ok := ParseGitHub(label); ok && strings.Contains(label, "github.com") {
		return Source{Kind: "github", Repo: r}
	}
	if r, ok := builtinRepos[imageRepo]; ok {
		return Source{Kind: "github", Repo: r}
	}
	return Source{}
}

// tagVersion extracts the version from a release tag such as "v1.2.3",
// "release-1.2.3" or "app/v1.2.3". Pre-release suffixes make it unusable.
func tagVersion(tag string) *version.Version {
	i := strings.IndexAny(tag, "0123456789")
	if i < 0 {
		return nil
	}
	v, variant := version.ParseTag(tag[i:])
	if v == nil {
		return nil
	}
	low := strings.ToLower(variant)
	for _, pre := range []string{"rc", "beta", "alpha", "pre", "dev", "nightly", "snapshot"} {
		if strings.Contains(low, pre) {
			return nil
		}
	}
	return v
}

func truncate(s string) (string, bool) {
	if len(s) <= maxBody {
		return s, false
	}
	cut := maxBody
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut], true
}
