package notes

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"composesafe/internal/cache"
	"composesafe/internal/version"
)

func ver(s string) version.Version {
	v, err := version.Parse(s)
	if err != nil {
		panic(err)
	}
	return *v
}

func TestParseGitHub(t *testing.T) {
	tests := map[string]string{
		"https://github.com/go-gitea/gitea":           "go-gitea/gitea",
		"https://github.com/go-gitea/gitea.git":       "go-gitea/gitea",
		"https://github.com/go-gitea/gitea/tree/main": "go-gitea/gitea",
		"git@github.com:owner/repo.git":               "owner/repo",
		"owner/repo":                                  "owner/repo",
	}
	for in, want := range tests {
		if got, ok := ParseGitHub(in); !ok || got != want {
			t.Errorf("ParseGitHub(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "https://gitlab.com/a/b", "justaword", "a/b/c"} {
		if got, ok := ParseGitHub(bad); ok && !strings.Contains(bad, "github.com") && got != "" && bad != "a/b/c" {
			t.Errorf("ParseGitHub(%q) should fail, got %q", bad, got)
		}
	}
}

func TestResolveSource(t *testing.T) {
	tests := []struct {
		name, override, repo, label string
		want                        Source
	}{
		{"official postgres", "", "library/postgres", "", Source{Kind: "postgres"}},
		{"override wins", "me/fork", "library/postgres", "", Source{Kind: "github", Repo: "me/fork"}},
		{"override postgres", "postgres", "me/pg", "", Source{Kind: "postgres"}},
		{"label", "", "me/app", "https://github.com/me/app", Source{Kind: "github", Repo: "me/app"}},
		{"non-github label ignored", "", "me/app", "https://gitlab.com/me/app", Source{}},
		{"builtin", "", "gitea/gitea", "", Source{Kind: "github", Repo: "go-gitea/gitea"}},
		{"label beats builtin", "", "gitea/gitea", "https://github.com/x/y", Source{Kind: "github", Repo: "x/y"}},
		{"unknown", "", "me/unknown", "", Source{}},
	}
	for _, tc := range tests {
		if got := ResolveSource(tc.override, tc.repo, tc.label); got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
	if (Source{}).String() != "" || (Source{Kind: "github", Repo: "a/b"}).String() != "github.com/a/b" {
		t.Error("Source.String")
	}
}

func TestTagVersion(t *testing.T) {
	ok := map[string]string{"v1.2.3": "1.2.3", "release-1.2.3": "1.2.3", "app/v2.0": "2.0.0", "1.37.5": "1.37.5"}
	for in, want := range ok {
		if v := tagVersion(in); v == nil || v.String() != want {
			t.Errorf("tagVersion(%q) = %v", in, v)
		}
	}
	for _, bad := range []string{"nightly", "v1.2.3-rc1", "8.12-rc2", "1.0.0-beta.1", "v2.0.0-alpha"} {
		if tagVersion(bad) != nil {
			t.Errorf("tagVersion(%q) must be rejected", bad)
		}
	}
}

// githubServer serves one page of releases and counts requests.
func githubServer(t *testing.T, releases string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasPrefix(r.URL.Path, "/repos/o/r/releases") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, releases)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const ghReleases = `[
 {"tag_name":"v2.0.0-rc1","name":"rc","body":"rc","html_url":"u/rc","prerelease":true},
 {"tag_name":"v1.4.0","name":"Release 1.4.0","body":"## Breaking changes\nstuff","html_url":"u/140"},
 {"tag_name":"v1.3.1","name":"","body":"fixes","html_url":"u/131"},
 {"tag_name":"v1.3.0","name":"1.3.0","body":"features","html_url":"u/130"},
 {"tag_name":"draft-1.3.5","name":"d","body":"d","html_url":"u/d","draft":true},
 {"tag_name":"v1.2.0","name":"1.2.0","body":"old","html_url":"u/120"},
 {"tag_name":"nightly","name":"n","body":"n","html_url":"u/n"}
]`

func TestGitHubFetchRange(t *testing.T) {
	srv, _ := githubServer(t, ghReleases)
	g := GitHub{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := g.Fetch(t.Context(), "o/r", ver("1.2.0"), ver("1.4.0"))
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, n := range got {
		tags = append(tags, n.Tag)
	}
	// oldest first; excludes from (1.2.0), prerelease, draft and non-version tags
	if strings.Join(tags, ",") != "v1.3.0,v1.3.1,v1.4.0" {
		t.Errorf("tags = %v", tags)
	}
	if got[1].Title != "v1.3.1" || got[2].URL != "u/140" || !strings.Contains(got[2].Body, "Breaking") {
		t.Errorf("fields: %+v", got)
	}
	// upper bound is inclusive and exclusive of newer releases
	got, _ = g.Fetch(t.Context(), "o/r", ver("1.2.0"), ver("1.3.0"))
	if len(got) != 1 || got[0].Tag != "v1.3.0" {
		t.Errorf("upper bound: %+v", got)
	}
}

func TestGitHubTruncatesBody(t *testing.T) {
	big := strings.Repeat("é", maxBody) // multi-byte on purpose
	srv, _ := githubServer(t, fmt.Sprintf(`[{"tag_name":"v1.1.0","name":"x","body":%q,"html_url":"u"}]`, big))
	got, err := (GitHub{BaseURL: srv.URL}).Fetch(t.Context(), "o/r", ver("1.0.0"), ver("1.1.0"))
	if err != nil || len(got) != 1 || !got[0].Truncated || len(got[0].Body) > maxBody {
		t.Fatalf("%v %+v", err, got)
	}
	if !strings.HasSuffix(got[0].Body, "é") {
		t.Error("truncation split a UTF-8 sequence")
	}
}

func TestGitHubErrors(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer limited.Close()
	_, err := (GitHub{BaseURL: limited.URL}).Fetch(t.Context(), "o/r", ver("1.0.0"), ver("2.0.0"))
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("rate limit: %v", err)
	}

	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	if _, err := (GitHub{BaseURL: missing.URL}).Fetch(t.Context(), "o/r", ver("1.0.0"), ver("2.0.0")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
}

func TestGitHubSendsTokenAndCaches(t *testing.T) {
	var auth string
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, ghReleases)
	}))
	defer srv.Close()
	g := GitHub{BaseURL: srv.URL, Token: "tok", Cache: cache.Dir(t.TempDir())}
	for i := 0; i < 2; i++ {
		if _, err := g.Fetch(t.Context(), "o/r", ver("1.2.0"), ver("1.4.0")); err != nil {
			t.Fatal(err)
		}
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if hits != 1 {
		t.Errorf("second call must come from the cache, server saw %d requests", hits)
	}
}

func TestGitHubKeepsNewestWhenManyNotes(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 60; i >= 1; i-- {
		if i != 60 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"tag_name":"v1.%d.0","name":"n","body":"b","html_url":"u"}`, i)
	}
	sb.WriteString("]")
	srv, _ := githubServer(t, sb.String())
	got, err := (GitHub{BaseURL: srv.URL}).Fetch(t.Context(), "o/r", ver("1.0.0"), ver("1.60.0"))
	if err != nil || len(got) != maxNotes || got[len(got)-1].Tag != "v1.60.0" || got[0].Tag != "v1.31.0" {
		t.Fatalf("len=%d first=%s err=%v", len(got), got[0].Tag, err)
	}
}
