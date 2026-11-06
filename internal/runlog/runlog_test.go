package runlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := NewRedactor([]string{"s3cr3t-value", "ab", "s3cr3t"}) // "ab" is too short to mask
	tests := []struct{ in, want string }{
		{"password is s3cr3t-value ok", "password is *** ok"},
		{"POSTGRES_PASSWORD=hunter2 started", "POSTGRES_PASSWORD=*** started"},
		{"api_key=abc123 token=zzz", "api_key=*** token=***"},
		{"postgres://app:pa55@db:5432/x", "postgres://app:***@db:5432/x"},
		{"nothing sensitive: ab", "nothing sensitive: ab"},
	}
	for _, tc := range tests {
		if got := r.Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	var nilR *Redactor
	if got := nilR.Redact("X_TOKEN=a"); got != "X_TOKEN=***" {
		t.Errorf("nil redactor must still apply patterns, got %q", got)
	}
}

func TestSecretsFromEnvFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# c\nA=plain\nexport B=\"quoted val\"\nC='single'\nEMPTY=\nnoequals\n"), 0o600)
	got := SecretsFromEnvFiles(p, "/does/not/exist")
	want := []string{"plain", "quoted val", "single", ""}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLoggerRedactsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	l, err := Open(path, "run1", NewRedactor([]string{"topsecret"}))
	if err != nil {
		t.Fatal(err)
	}
	l.Begin("snapshot.dump", "docker", "exec", "-e", "PGPASSWORD=topsecret")
	l.Done("snapshot.dump", map[string]any{"bytes": 10, "note": "used topsecret"})
	l.Failed("upgrade.up", errors.New("login failed for topsecret"))
	l.Close()

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "topsecret") {
		t.Fatalf("secret leaked to disk:\n%s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("run log mode = %v, want 0600", fi.Mode().Perm())
	}
	evs, err := Read(path)
	if err != nil || len(evs) != 3 {
		t.Fatalf("read: %v %d", err, len(evs))
	}
	if evs[0].Run != "run1" || evs[0].TS.IsZero() || evs[1].Detail["bytes"] != float64(10) {
		t.Errorf("unexpected events: %+v", evs)
	}
}

func TestReadIgnoresTruncatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	os.WriteFile(path, []byte(`{"step":"a.b","status":"start"}`+"\n"+`{"step":"a.b","stat`), 0o600)
	evs, err := Read(path)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %d events, err %v", len(evs), err)
	}
}

func TestInterrupted(t *testing.T) {
	ev := func(step, status string) Event { return Event{Step: step, Status: status} }
	tests := []struct {
		name string
		evs  []Event
		want string
	}{
		{"empty", nil, ""},
		{"all finished", []Event{ev("a.x", Start), ev("a.x", OK), ev("b.y", Start), ev("b.y", Fail)}, ""},
		{"killed mid-step", []Event{ev("a.x", Start), ev("a.x", OK), ev("upgrade.up", Start)}, "upgrade.up"},
		{"skip counts as finished", []Event{ev("a.x", Start), ev("a.x", Skip)}, ""},
	}
	for _, tc := range tests {
		if got := Interrupted(tc.evs); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
