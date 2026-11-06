package runlog

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
)

// minSecretLen avoids masking short values (like "1" or "on") that would
// mangle unrelated text.
const minSecretLen = 4

var (
	kvRe  = regexp.MustCompile(`(?i)(password|secret|token|key)=\S+`)
	urlRe = regexp.MustCompile(`(://[^/\s:@]+):[^/\s@]+@`)
)

// Redactor masks secrets in anything written to the run log, reports or the
// terminal.
type Redactor struct {
	secrets []string // longest first, so overlapping values mask fully
}

// NewRedactor masks the given literal values plus common KEY=value and
// URL-credential patterns.
func NewRedactor(values []string) *Redactor {
	seen := map[string]bool{}
	var ss []string
	for _, v := range values {
		if len(v) >= minSecretLen && !seen[v] {
			seen[v] = true
			ss = append(ss, v)
		}
	}
	sort.Slice(ss, func(i, j int) bool { return len(ss[i]) > len(ss[j]) })
	return &Redactor{secrets: ss}
}

// Redact returns s with secrets replaced by "***". A nil Redactor still
// applies the pattern rules.
func (r *Redactor) Redact(s string) string {
	if r != nil {
		for _, v := range r.secrets {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	s = kvRe.ReplaceAllString(s, "$1=***")
	s = urlRe.ReplaceAllString(s, "$1:***@")
	return s
}

// SecretsFromEnvFiles returns the values of all KEY=VALUE lines in the given
// dotenv files. Missing files are skipped.
func SecretsFromEnvFiles(paths ...string) []string {
	var out []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			_, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			out = append(out, v)
		}
		f.Close()
	}
	return out
}
