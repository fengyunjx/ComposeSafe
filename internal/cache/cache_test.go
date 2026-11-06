package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTripAndTTL(t *testing.T) {
	d := Dir(filepath.Join(t.TempDir(), "c"))
	var got []string
	if d.Get("k", time.Hour, &got) {
		t.Fatal("empty cache must miss")
	}
	if err := d.Put("k", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if !d.Get("k", time.Hour, &got) || len(got) != 2 {
		t.Fatalf("hit expected, got %v", got)
	}
	if d.Get("other", time.Hour, &got) {
		t.Error("different key must miss")
	}
	time.Sleep(5 * time.Millisecond)
	if d.Get("k", time.Millisecond, &got) {
		t.Error("expired entry must miss")
	}
	entries, _ := os.ReadDir(string(d))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Error("temporary file left behind")
		}
	}
}

func TestDisabledAndCorrupt(t *testing.T) {
	var d Dir
	if err := d.Put("k", 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if d.Get("k", time.Hour, &n) {
		t.Error("disabled cache never hits")
	}

	d = Dir(t.TempDir())
	d.Put("k", 7)
	os.WriteFile(d.path("k"), []byte("{broken"), 0o600)
	if d.Get("k", time.Hour, &n) {
		t.Error("corrupt entry must miss, not fail")
	}
}
