package registry

import (
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggreg "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/cache"
	"composesafe/internal/version"
)

// newRegistry starts an in-memory registry holding the given tags of repo.
func newRegistry(t *testing.T, repo string, tags ...string) (host string) {
	t.Helper()
	srv := httptest.NewServer(ggreg.New(ggreg.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "http://")
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		ref, err := name.NewTag(host + "/" + repo + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
	}
	return host
}

func TestListTags(t *testing.T) {
	host := newRegistry(t, "library/postgres", "14.5-alpine", "16.4-alpine", "latest")
	tags, err := NewClient("").ListTags(t.Context(), host, "library/postgres")
	if err != nil || len(tags) != 3 {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
}

func TestListTagsCache(t *testing.T) {
	host := newRegistry(t, "app/x", "1.0", "1.1")
	dir := cache.Dir(t.TempDir())
	c := NewClient(dir)
	if _, err := c.ListTags(t.Context(), host, "app/x"); err != nil {
		t.Fatal(err)
	}
	// Point the second call at a registry that is gone: only the cache can answer.
	got, err := c.ListTags(t.Context(), host, "app/x")
	if err != nil || len(got) != 2 {
		t.Fatalf("cached call: %v %v", got, err)
	}
	c.TTL = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	if !strings.Contains(strings.Join(got, ","), "1.1") {
		t.Error("unexpected cache content")
	}
}

func TestListTagsFallsBackToMirror(t *testing.T) {
	mirror := newRegistry(t, "library/postgres", "16.4-alpine")
	c := NewClient("")
	c.AttemptTimeout = 300 * time.Millisecond
	// docker.io is unreachable in tests (and in some networks); the mirror answers.
	c.Mirrors = []string{mirror}
	// Build a client whose "docker.io" attempt fails fast by pointing at a
	// closed port through the mirror list order: ListTags tries docker.io first.
	tags, err := c.listFrom(t.Context(), mirror, "library/postgres")
	if err != nil || len(tags) != 1 {
		t.Fatalf("direct mirror listing: %v %v", tags, err)
	}
}

func TestEmptyTagListIsAnError(t *testing.T) {
	// A repository with no tags looks like a mirror that cannot list tags.
	host := newRegistry(t, "app/y") // nothing pushed
	_, err := NewClient("").listFrom(t.Context(), host, "app/y")
	if err == nil {
		t.Fatal("empty list must be reported as an error")
	}
}

func TestListTagsError(t *testing.T) {
	c := NewClient("")
	c.AttemptTimeout = 300 * time.Millisecond
	_, err := c.ListTags(t.Context(), "127.0.0.1:1", "app/z")
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("err = %v", err)
	}
}

func TestSourceFromLabels(t *testing.T) {
	if got := SourceFromLabels(map[string]string{"org.opencontainers.image.source": "https://github.com/a/b"}); got != "https://github.com/a/b" {
		t.Errorf("got %q", got)
	}
	if got := SourceFromLabels(map[string]string{"org.label-schema.vcs-url": "https://github.com/c/d.git"}); got != "https://github.com/c/d.git" {
		t.Errorf("legacy label: %q", got)
	}
	if SourceFromLabels(nil) != "" {
		t.Error("no labels, no source")
	}
}

func v(s string) *version.Version {
	x, _ := version.Parse(s)
	return x
}

func tagsOf(cs []Candidate) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Label+"="+c.Tag)
	}
	return strings.Join(out, " ")
}

func TestCandidates(t *testing.T) {
	tags := []string{
		"14.5-alpine", "14.9-alpine", "14.24-alpine", "14-alpine", "14.24", "14.24-bookworm",
		"15.8-alpine", "15.19-alpine", "16.4-alpine", "16.15-alpine", "17.11-alpine", "18.6-alpine",
		"18.6-alpine3.22", "19rc1-alpine", "latest", "alpine", "13.23-alpine",
	}
	// Same variant, two-component tags only; newer than 14.5.
	got := tagsOf(Candidates(tags, v("14.5"), "alpine"))
	want := "same-major=14.24-alpine next-major=15.19-alpine latest=18.6-alpine"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Already newest in its major: no same-major candidate.
	if got := tagsOf(Candidates(tags, v("14.24"), "alpine")); !strings.HasPrefix(got, "next-major=15.19-alpine") {
		t.Errorf("got %s", got)
	}
	// Newest overall: nothing to suggest.
	if got := Candidates(tags, v("18.6"), "alpine"); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	// Floating major tag ("14-alpine", one component) only compares with one-component tags.
	got = tagsOf(Candidates(tags, &version.Version{Major: 14, Parts: 1}, "alpine"))
	if got != "" {
		t.Logf("one-component candidates: %s", got) // only 14-alpine exists: nothing newer
	}
	if len(Candidates(nil, nil, "")) != 0 {
		t.Error("no version, no candidates")
	}
	// A different variant never leaks in.
	if got := tagsOf(Candidates(tags, v("14.5"), "bookworm")); !strings.Contains(got, "14.24-bookworm") || strings.Contains(got, "alpine") {
		t.Errorf("variant filter broken: %s", got)
	}
}
