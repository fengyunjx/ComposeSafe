// Package registry lists image tags and reads image labels from a registry,
// and picks upgrade candidates from a tag list.
package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/cache"
	"composesafe/internal/version"
)

// Client talks to registries using the credentials in ~/.docker/config.json
// (including credential helpers).
type Client struct {
	Cache cache.Dir     // optional
	TTL   time.Duration // cache lifetime; 10 minutes when zero
	Opts  []remote.Option
	// Mirrors are the Docker Hub mirrors configured on the Docker daemon
	// (hosts only). They are tried when docker.io itself is unreachable,
	// which is common behind restrictive networks where the daemon pulls
	// through a mirror.
	Mirrors []string
	// AttemptTimeout bounds each registry attempt; 10 seconds when zero.
	AttemptTimeout time.Duration
}

// NewClient returns a Client that authenticates like the docker CLI.
func NewClient(c cache.Dir) Client {
	return Client{Cache: c, TTL: 10 * time.Minute, Opts: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}}
}

func (c Client) ttl() time.Duration {
	if c.TTL == 0 {
		return 10 * time.Minute
	}
	return c.TTL
}

func (c Client) opts(ctx context.Context) []remote.Option {
	return append([]remote.Option{remote.WithContext(ctx)}, c.Opts...)
}

// repoName builds the reference ggcr expects: docker.io is implicit.
func repoName(registryHost, repo string) string {
	if registryHost == "" || registryHost == "docker.io" {
		return repo
	}
	return registryHost + "/" + repo
}

// ListTags returns every tag of a repository. For Docker Hub images it falls
// back to the daemon's configured mirrors when docker.io cannot be reached.
func (c Client) ListTags(ctx context.Context, registryHost, repo string) ([]string, error) {
	key := "tags:" + repoName(registryHost, repo)
	var tags []string
	if c.Cache.Get(key, c.ttl(), &tags) && len(tags) > 0 {
		return tags, nil
	}
	hosts := []string{registryHost}
	if registryHost == "" || registryHost == "docker.io" {
		hosts = append([]string{"docker.io"}, c.Mirrors...)
	}
	var errs []string
	for _, h := range hosts {
		tags, err := c.listFrom(ctx, h, repo)
		if err == nil {
			_ = c.Cache.Put(key, tags)
			return tags, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Sprintf("%s: %v", h, err))
	}
	return nil, fmt.Errorf("list tags of %s failed:\n  %s", repo, strings.Join(errs, "\n  "))
}

func (c Client) listFrom(ctx context.Context, host, repo string) ([]string, error) {
	timeout := c.AttemptTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := name.NewRepository(repoName(host, repo))
	if err != nil {
		return nil, err
	}
	tags, err := remote.List(r, c.opts(ctx)...)
	if err == nil && len(tags) == 0 {
		// Pull-through mirrors often answer the tag-list call with an empty
		// list instead of an error; that is not a repository with no tags.
		err = errors.New("returned no tags (a pull-through mirror may not support tag listing)")
	}
	return tags, err
}

// SourceLabel reads the OCI label that points at the project's source
// repository from a remote image. Callers prefer the local image's labels
// and use this only as a fallback.
func (c Client) SourceLabel(ctx context.Context, registryHost, repo, tag string) (string, error) {
	timeout := c.AttemptTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ref, err := name.ParseReference(repoName(registryHost, repo) + ":" + tag)
	if err != nil {
		return "", err
	}
	img, err := remote.Image(ref, c.opts(ctx)...)
	if err != nil {
		return "", fmt.Errorf("read image %s: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", err
	}
	return SourceFromLabels(cfg.Config.Labels), nil
}

// SourceFromLabels extracts the source repository URL from image labels.
func SourceFromLabels(labels map[string]string) string {
	for _, k := range []string{"org.opencontainers.image.source", "org.label-schema.vcs-url"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// Candidate is a suggested upgrade target.
type Candidate struct {
	Tag     string          `json:"tag"`
	Version version.Version `json:"version"`
	Label   string          `json:"label"` // same-major | next-major | latest
}

// Candidates picks targets from a tag list for a current tag: the newest
// version within the same major, the newest of the next major, and the
// overall newest. Only tags with the same variant suffix and the same number
// of version components are considered (so 16-alpine never suggests
// 16.4-alpine, and alpine never suggests bookworm). The caller chooses.
func Candidates(tags []string, current *version.Version, variant string) []Candidate {
	if current == nil {
		return nil
	}
	best := map[int]Candidate{} // newest tag per major
	for _, t := range tags {
		v, vr := version.ParseTag(t)
		if v == nil || vr != variant || v.Parts != current.Parts {
			continue
		}
		if b, ok := best[v.Major]; !ok || version.Compare(*v, b.Version) > 0 {
			best[v.Major] = Candidate{Tag: t, Version: *v}
		}
	}
	var out []Candidate
	seen := map[string]bool{}
	add := func(c Candidate, label string) {
		if c.Tag == "" || seen[c.Tag] || version.Compare(c.Version, *current) <= 0 {
			return
		}
		seen[c.Tag] = true
		c.Label = label
		out = append(out, c)
	}
	add(best[current.Major], "same-major")

	var majors []int
	for m := range best {
		if m > current.Major {
			majors = append(majors, m)
		}
	}
	sort.Ints(majors)
	if len(majors) > 0 {
		add(best[majors[0]], "next-major")
		add(best[majors[len(majors)-1]], "latest")
	}
	return out
}
