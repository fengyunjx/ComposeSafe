package version

import "testing"

func TestParseTag(t *testing.T) {
	tests := []struct {
		tag     string
		ok      bool
		major   int
		minor   int
		patch   int
		parts   int
		variant string
	}{
		{"16", true, 16, 0, 0, 1, ""},
		{"16.3", true, 16, 3, 0, 2, ""},
		{"16.3-alpine3.19", true, 16, 3, 0, 2, "alpine3.19"},
		{"17-bookworm", true, 17, 0, 0, 1, "bookworm"},
		{"v1.2.3", true, 1, 2, 3, 3, ""},
		{"1.27.3.4", true, 1, 27, 3, 4, ""},
		{"v2.29.1-desktop.1", true, 2, 29, 1, 3, "desktop.1"},
		{"latest", false, 0, 0, 0, 0, ""},
		{"alpine", false, 0, 0, 0, 0, ""},
		{"3f2a9c1", false, 0, 0, 0, 0, ""},   // hash starting with a digit
		{"2024.10.1", false, 0, 0, 0, 0, ""}, // date tag
		{"20241001", false, 0, 0, 0, 0, ""},
		{"", false, 0, 0, 0, 0, ""},
	}
	for _, tc := range tests {
		v, variant := ParseTag(tc.tag)
		if (v != nil) != tc.ok {
			t.Errorf("%q: parsed=%v want %v", tc.tag, v != nil, tc.ok)
			continue
		}
		if v == nil {
			continue
		}
		if v.Major != tc.major || v.Minor != tc.minor || v.Patch != tc.patch || v.Parts != tc.parts || variant != tc.variant {
			t.Errorf("%q: got %+v variant=%q", tc.tag, *v, variant)
		}
	}
}

func TestCompareAndAtLeast(t *testing.T) {
	a, _ := Parse("2.24.0")
	b, _ := Parse("5.5.0")
	if Compare(*a, *b) != -1 || Compare(*b, *a) != 1 || Compare(*a, *a) != 0 {
		t.Fatal("Compare wrong")
	}
	if !b.AtLeast(2, 24) { // Compose 5.x must pass a ">= 2.24" check
		t.Error("5.5.0 should satisfy >= 2.24")
	}
	old, _ := Parse("2.23.9")
	if old.AtLeast(2, 24) {
		t.Error("2.23.9 should not satisfy >= 2.24")
	}
}

func TestParseRejectsNonVersion(t *testing.T) {
	if _, err := Parse("dev"); err == nil {
		t.Fatal("expected error")
	}
}
