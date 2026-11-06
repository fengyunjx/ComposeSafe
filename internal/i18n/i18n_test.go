package i18n

import (
	"regexp"
	"testing"
)

var verbRe = regexp.MustCompile(`%[#+-]?[0-9]*[a-zA-Z]`)

// Both tables must define the same keys with the same number of verbs, or a
// translation silently breaks Sprintf at runtime.
func TestTablesInSync(t *testing.T) {
	for k, e := range en {
		z, ok := zh[k]
		if !ok {
			t.Errorf("key %q missing in zh", k)
			continue
		}
		if a, b := len(verbRe.FindAllString(e, -1)), len(verbRe.FindAllString(z, -1)); a != b {
			t.Errorf("key %q: en has %d verbs, zh has %d", k, a, b)
		}
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q missing in en", k)
		}
	}
}

func TestT(t *testing.T) {
	if got := T("en")("scan.project", "x"); got != "Project: x" {
		t.Errorf("en: %q", got)
	}
	if got := T("zh")("scan.project", "x"); got != "项目:x" {
		t.Errorf("zh: %q", got)
	}
	if got := T("fr")("scan.project", "x"); got != "Project: x" {
		t.Errorf("unknown language falls back to English, got %q", got)
	}
	if got := T("en")("no.such.key"); got != "no.such.key" {
		t.Errorf("missing keys are visible, got %q", got)
	}
}
