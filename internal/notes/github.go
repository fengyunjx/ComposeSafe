package notes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"composesafe/internal/cache"
	"composesafe/internal/version"
)

// GitHub reads release notes from the GitHub Releases API.
type GitHub struct {
	BaseURL string // default https://api.github.com
	Token   string // optional; raises the rate limit from 60/h
	HTTP    *http.Client
	Cache   cache.Dir
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

const ghMaxPages = 3

// Fetch returns the releases of repo with from < version <= to, oldest first.
func (g GitHub) Fetch(ctx context.Context, repo string, from, to version.Version) ([]ReleaseNote, error) {
	var out []ReleaseNote
	for page := 1; page <= ghMaxPages; page++ {
		rels, err := g.page(ctx, repo, page)
		if err != nil {
			return out, err
		}
		oldest := true // did this page reach versions at or below `from`?
		for _, r := range rels {
			if r.Draft || r.Prerelease {
				continue
			}
			v := tagVersion(r.TagName)
			if v == nil {
				continue
			}
			if version.Compare(*v, from) <= 0 {
				continue
			}
			oldest = false
			if version.Compare(*v, to) > 0 {
				continue
			}
			body, cut := truncate(r.Body)
			title := r.Name
			if title == "" {
				title = r.TagName
			}
			out = append(out, ReleaseNote{Tag: r.TagName, Title: title, Body: body, URL: r.HTMLURL,
				PublishedAt: r.PublishedAt, Truncated: cut})
		}
		// Releases are newest first: once a page holds nothing newer than
		// `from`, older pages cannot help.
		if len(rels) < 100 || oldest {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := tagVersion(out[i].Tag), tagVersion(out[j].Tag)
		return version.Compare(*a, *b) < 0
	})
	if len(out) > maxNotes {
		out = out[len(out)-maxNotes:]
	}
	return out, nil
}

func (g GitHub) page(ctx context.Context, repo string, page int) ([]ghRelease, error) {
	key := fmt.Sprintf("github:%s:%d", repo, page)
	var rels []ghRelease
	if g.Cache.Get(key, 10*time.Minute, &rels) {
		return rels, nil
	}
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", base, repo, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "composesafe")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github releases of %s: %w", repo, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		return nil, ErrRateLimited
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("github repository %s not found (or has no releases)", repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("github releases of %s: HTTP %d", repo, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("github releases of %s: %w", repo, err)
	}
	_ = g.Cache.Put(key, rels)
	return rels, nil
}
