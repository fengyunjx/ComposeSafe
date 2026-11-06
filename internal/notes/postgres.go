package notes

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"composesafe/internal/version"
)

// Postgres reads the official release notes at
// https://www.postgresql.org/docs/release/<version>/ (verified to serve
// every release as 200 with a "sect1" section holding the notes).
type Postgres struct {
	BaseURL string // default https://www.postgresql.org/docs/release/
	HTTP    *http.Client
}

const pgMaxMinors = 10

// Fetch returns the notes that matter for going from `from` to `to`:
//   - within one major: the release notes of each minor in between (newest
//     pgMaxMinors);
//   - across majors: "<major>.0" for every major in between (these carry the
//     "Migration to Version N" section) plus the target minor's own notes.
//
// Pages that cannot be read become warnings, not errors: a missing page must
// not hide the others.
func (p Postgres) Fetch(ctx context.Context, from, to version.Version) (notes []ReleaseNote, warnings []string) {
	if to.Major < 10 || from.Major < 10 {
		return nil, []string{"release notes of Postgres versions before 10 are not supported"}
	}
	var vers []string
	if from.Major == to.Major {
		start := from.Minor + 1
		if to.Minor-start+1 > pgMaxMinors {
			start = to.Minor - pgMaxMinors + 1
		}
		for m := start; m <= to.Minor; m++ {
			vers = append(vers, fmt.Sprintf("%d.%d", to.Major, m))
		}
	} else {
		for maj := from.Major + 1; maj <= to.Major; maj++ {
			vers = append(vers, fmt.Sprintf("%d.0", maj))
		}
		if to.Parts >= 2 && to.Minor > 0 {
			vers = append(vers, fmt.Sprintf("%d.%d", to.Major, to.Minor))
		}
	}
	for _, v := range vers {
		n, err := p.page(ctx, v)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("release notes %s: %v", v, err))
			continue
		}
		notes = append(notes, n)
	}
	return notes, warnings
}

func (p Postgres) page(ctx context.Context, ver string) (ReleaseNote, error) {
	base := p.BaseURL
	if base == "" {
		base = "https://www.postgresql.org/docs/release/"
	}
	url := strings.TrimRight(base, "/") + "/" + ver + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ReleaseNote{}, err
	}
	req.Header.Set("User-Agent", "composesafe")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ReleaseNote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReleaseNote{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ReleaseNote{}, err
	}
	title, body := ExtractPGNotes(string(raw))
	if body == "" {
		return ReleaseNote{}, fmt.Errorf("page layout not recognised")
	}
	if title == "" {
		title = "Release " + ver
	}
	text, cut := truncate(body)
	return ReleaseNote{Tag: ver, Title: title, Body: text, URL: url, Truncated: cut}, nil
}

var (
	pgTitleRe = regexp.MustCompile(`(?s)<h2 class="title"[^>]*>(.*?)</h2>`)
	scriptRe  = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blockRe   = regexp.MustCompile(`(?i)</(p|li|h[1-6]|div|tr|dt|dd|pre|table|ul|ol)>|<br\s*/?>`)
	liRe      = regexp.MustCompile(`(?i)<li[^>]*>`)
	tagRe     = regexp.MustCompile(`<[^>]+>`)
	spaceRe   = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankRe   = regexp.MustCompile(`\n{3,}`)
	listGapRe = regexp.MustCompile(`\n\n(- )`)
)

// ExtractPGNotes returns the heading and plain text of the release-notes
// section of a postgresql.org release page: from the first `sect1` block to
// the page footer, skipping the navigation header.
func ExtractPGNotes(page string) (title, body string) {
	i := strings.Index(page, `<div class="sect1"`)
	if i < 0 {
		return "", ""
	}
	section := page[i:]
	if j := strings.Index(section, `<div class="navfooter"`); j >= 0 {
		section = section[:j]
	}
	if m := pgTitleRe.FindStringSubmatch(section); m != nil {
		title = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(HTMLText(m[1])), "#"))
	}
	return title, HTMLText(section)
}

// HTMLText converts HTML to readable plain text: block tags become line
// breaks, list items get a dash, entities are decoded.
func HTMLText(s string) string {
	s = scriptRe.ReplaceAllString(s, "")
	s = liRe.ReplaceAllString(s, "\n- ")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRe.ReplaceAllString(l, " "))
	}
	s = strings.Join(lines, "\n")
	s = blankRe.ReplaceAllString(s, "\n\n")
	s = listGapRe.ReplaceAllString(s, "\n$1") // </li> already ended the previous item
	return strings.TrimSpace(s)
}
