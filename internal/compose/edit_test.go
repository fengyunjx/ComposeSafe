package compose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

const commented = `# my stack
x-common: &common
  restart: unless-stopped   # keep it up

services:
  db:   # the database
    image: "postgres:16.3-alpine"   # pinned
    <<: *common
    volumes:
      - pgdata:/var/lib/postgresql/data   # data
      - ./init:/docker-entrypoint-initdb.d:ro

  app:
    image: x/app:1.0
    depends_on: [db]
    ports: ["8080:80"]

volumes:
  pgdata:
  # uploads are separate
  uploads: {}
`

func TestEditImageKeepsEveryOtherByte(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yml", commented)
	edits, err := EditImage([]string{f}, nil, "db", "postgres:16.3-alpine", "postgres:16.4-alpine")
	if err != nil || len(edits) != 1 {
		t.Fatalf("%v %v", edits, err)
	}
	want := strings.Replace(commented, `"postgres:16.3-alpine"`, `"postgres:16.4-alpine"`, 1)
	if string(edits[0].After) != want {
		t.Errorf("only the image may change.\n--- got\n%s\n--- want\n%s", edits[0].After, want)
	}
	if string(edits[0].Before) != commented {
		t.Error("Before must be the original")
	}
	if !strings.Contains(edits[0].Summary, "postgres:16.3-alpine → postgres:16.4-alpine") {
		t.Errorf("summary: %s", edits[0].Summary)
	}

	// plain scalar, other service
	edits, err = EditImage([]string{f}, nil, "app", "x/app:1.0", "x/app:2.0")
	if err != nil || string(edits[0].After) != strings.Replace(commented, "image: x/app:1.0", "image: x/app:2.0", 1) {
		t.Errorf("plain: %v\n%s", err, edits[0].After)
	}
}

func TestEditImageSingleQuotesAndCRLF(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "c.yml", "services:\r\n  a:\r\n    image: 'x/a:1'\r\n")
	edits, err := EditImage([]string{f}, nil, "a", "x/a:1", "x/a:2")
	if err != nil || string(edits[0].After) != "services:\r\n  a:\r\n    image: 'x/a:2'\r\n" {
		t.Errorf("%v %q", err, edits[0].After)
	}
}

func TestEditImageNonASCIIBeforeTheValue(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "c.yml", "# 数据库栈 — ünïcode\nservices:\n  a:\n    image: x/a:1\n    labels: {note: \"héllo\"}\n")
	edits, err := EditImage([]string{f}, nil, "a", "x/a:1", "x/a:2")
	if err != nil || !strings.Contains(string(edits[0].After), "image: x/a:2\n") || !strings.HasPrefix(string(edits[0].After), "# 数据库栈") {
		t.Errorf("%v %q", err, edits[0].After)
	}
}

func TestEditImageLastFileThatSetsItWins(t *testing.T) {
	dir := t.TempDir()
	base := write(t, dir, "compose.yml", "services:\n  a:\n    image: x/a:1\n")
	over := write(t, dir, "compose.override.yml", "services:\n  a:\n    image: x/a:1-custom\n  b:\n    image: y\n")
	edits, err := EditImage([]string{base, over}, nil, "a", "x/a:1-custom", "x/a:2-custom")
	if err != nil || edits[0].Path != over {
		t.Fatalf("%v %+v", err, edits)
	}
	// the base file is untouched; an old value that does not match what Compose resolves is refused
	if _, err := EditImage([]string{base, over}, nil, "a", "x/a:1", "x/a:2"); err == nil {
		t.Error("stale old value must be refused")
	}
}

func TestEditImageVariableInDotenv(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yml", "services:\n  db:\n    image: postgres:${PG_TAG:-16}\n")
	env := write(t, dir, ".env", "# versions\nPG_TAG=\"16.3-alpine\"   \nOTHER=1\n")
	edits, err := EditImage([]string{f}, []string{env}, "db", "postgres:16.3-alpine", "postgres:16.4-alpine")
	if err != nil || len(edits) != 1 || edits[0].Path != env {
		t.Fatalf("%v %+v", err, edits)
	}
	if string(edits[0].After) != "# versions\nPG_TAG=\"16.4-alpine\"\nOTHER=1\n" && string(edits[0].After) != "# versions\nPG_TAG=\"16.4-alpine\"   \nOTHER=1\n" {
		t.Errorf("env after: %q", edits[0].After)
	}
	if string(edits[0].Before) == string(edits[0].After) {
		t.Error("no change made")
	}

	// the variable is not in .env (default or real environment): the user must act
	var me *ManualEditError
	noEnv := write(t, dir, "other.env", "UNRELATED=1\n")
	if _, err := EditImage([]string{f}, []string{noEnv}, "db", "postgres:16", "postgres:17"); !errors.As(err, &me) || !strings.Contains(err.Error(), "PG_TAG") {
		t.Errorf("err = %v", err)
	}
	// .env says one thing, Compose resolved another (shell environment wins)
	if _, err := EditImage([]string{f}, []string{env}, "db", "postgres:15", "postgres:16"); !errors.As(err, &me) {
		t.Errorf("mismatch err = %v", err)
	}
	// the repository part is not allowed to change through a variable
	if _, err := EditImage([]string{f}, []string{env}, "db", "postgres:16.3-alpine", "other/db:16.4"); !errors.As(err, &me) {
		t.Errorf("repo change err = %v", err)
	}
}

func TestEditImageRefusesIndirection(t *testing.T) {
	dir := t.TempDir()
	var me *ManualEditError
	cases := map[string]string{
		"alias":    "x-img: &img x/a:1\nservices:\n  a:\n    image: *img\n",
		"merge":    "x-c: &c\n  image: x/a:1\nservices:\n  a:\n    <<: *c\n",
		"extends":  "services:\n  a:\n    extends: {file: other.yml, service: base}\n",
		"include":  "include: [other.yml]\n",
		"two vars": "services:\n  a:\n    image: ${R}/a:${T}\n",
		"missing":  "services:\n  b:\n    image: y\n",
	}
	for name, body := range cases {
		f := write(t, dir, name+".yml", body)
		if _, err := EditImage([]string{f}, nil, "a", "x/a:1", "x/a:2"); !errors.As(err, &me) {
			t.Errorf("%s: err = %v, want a manual-edit error", name, err)
		}
	}
	f := write(t, dir, "ok.yml", "services:\n  a:\n    image: x/a:1\n")
	if _, err := EditImage([]string{f}, nil, "a", "x/a:1", "x/a:2; rm -rf"); !errors.As(err, &me) {
		t.Errorf("unsafe new value: %v", err)
	}
}

func TestSetEnvValue(t *testing.T) {
	src := "A=1\nexport B=old # why\n#B=commented\nB=last\nC='q'\r\n"
	out, old, ok := SetEnvValue([]byte(src), "B", "new")
	if !ok || old != "last" || string(out) != "A=1\nexport B=old # why\n#B=commented\nB=new\nC='q'\r\n" {
		t.Errorf("%v %q %q", ok, old, out)
	}
	out, old, _ = SetEnvValue([]byte(src), "C", "z")
	if old != "q" || !strings.HasSuffix(string(out), "C='z'\r\n") {
		t.Errorf("quoted + CRLF: %q %q", old, out)
	}
	if _, _, ok := SetEnvValue([]byte(src), "NOPE", "x"); ok {
		t.Error("missing variable")
	}
	out, old, _ = SetEnvValue([]byte("X=5 # trailing\nY=1"), "X", "6")
	if old != "5" || !strings.HasPrefix(string(out), "X=6\n") {
		t.Errorf("inline comment: %q %q", old, out)
	}
	out, _, _ = SetEnvValue([]byte("Y=1"), "Y", "2") // no trailing newline
	if string(out) != "Y=2" {
		t.Errorf("no newline: %q", out)
	}
}

func TestSwitchMountShortAndLongSyntax(t *testing.T) {
	rename := func(old string, bind bool) string { return old + "_rb" }

	out, old, bind, err := SwitchMount([]byte(commented), "db", "/var/lib/postgresql/data", rename)
	if err != nil || old != "pgdata" || bind {
		t.Fatalf("%q %v %v", old, bind, err)
	}
	want := strings.Replace(commented, "- pgdata:/var/lib", "- pgdata_rb:/var/lib", 1)
	if string(out) != want {
		t.Errorf("got\n%s", out)
	}

	// bind mount, relative path kept relative
	out, old, bind, err = SwitchMount([]byte(commented), "db", "/docker-entrypoint-initdb.d", rename)
	if err != nil || old != "./init" || !bind || !strings.Contains(string(out), "- ./init_rb:/docker-entrypoint-initdb.d:ro") {
		t.Errorf("bind: %q %v %v\n%s", old, bind, err, out)
	}

	long := "services:\n  db:\n    volumes:\n      - type: volume\n        source: pgdata\n        target: /data\n"
	out, old, _, err = SwitchMount([]byte(long), "db", "/data", rename)
	if err != nil || old != "pgdata" || !strings.Contains(string(out), "source: pgdata_rb\n") {
		t.Errorf("long syntax: %v\n%s", err, out)
	}

	var me *ManualEditError
	if _, _, _, err := SwitchMount([]byte(commented), "db", "/nope", rename); !errors.As(err, &me) {
		t.Errorf("unknown target: %v", err)
	}
	if _, _, _, err := SwitchMount([]byte(commented), "app", "/x", rename); !errors.As(err, &me) {
		t.Errorf("service without volumes: %v", err)
	}
	if _, _, _, err := SwitchMount([]byte(commented), "ghost", "/x", rename); !errors.As(err, &me) {
		t.Errorf("unknown service: %v", err)
	}
	bad := func(string, bool) string { return "x; y" }
	if _, _, _, err := SwitchMount([]byte(commented), "db", "/var/lib/postgresql/data", bad); !errors.As(err, &me) {
		t.Errorf("unsafe replacement: %v", err)
	}
}

func TestSwitchMountToMovesTheMountPoint(t *testing.T) {
	to := func(old string, bind bool) string { return old + "_pg18" }
	out, old, _, err := SwitchMountTo([]byte(commented), "db", "/var/lib/postgresql/data", "/var/lib/postgresql", to)
	if err != nil || old != "pgdata" {
		t.Fatalf("%q %v", old, err)
	}
	want := strings.Replace(commented, "- pgdata:/var/lib/postgresql/data", "- pgdata_pg18:/var/lib/postgresql", 1)
	if string(out) != want {
		t.Errorf("short syntax, got\n%s", out)
	}

	withMode := "services:\n  db:\n    volumes:\n      - ./pg:/var/lib/postgresql/data:rw\n"
	out, _, bind, err := SwitchMountTo([]byte(withMode), "db", "/var/lib/postgresql/data", "/var/lib/postgresql", to)
	if err != nil || !bind || !strings.Contains(string(out), "- ./pg_pg18:/var/lib/postgresql:rw\n") {
		t.Errorf("mode suffix kept: %v\n%s", err, out)
	}

	for _, long := range []string{
		"services:\n  db:\n    volumes:\n      - type: volume\n        source: pgdata\n        target: /var/lib/postgresql/data\n",
		"services:\n  db:\n    volumes:\n      - target: /var/lib/postgresql/data\n        source: pgdata\n        type: volume\n",
	} {
		out, _, _, err = SwitchMountTo([]byte(long), "db", "/var/lib/postgresql/data", "/var/lib/postgresql", to)
		if err != nil || !strings.Contains(string(out), "source: pgdata_pg18\n") || !strings.Contains(string(out), "target: /var/lib/postgresql\n") ||
			strings.Contains(string(out), "/data") {
			t.Errorf("long syntax: %v\n%s", err, out)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Errorf("not valid YAML: %v", err)
		}
	}
}

func TestDeclareExternalVolume(t *testing.T) {
	// existing block mapping: appended after its last entry, same indentation, comments kept
	out, err := DeclareExternalVolume([]byte(commented), "pgdata_rb", "p_pgdata_rb")
	if err != nil {
		t.Fatal(err)
	}
	want := commented + "  pgdata_rb:\n    external: true\n    name: p_pgdata_rb\n"
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	assertParses(t, out, "pgdata_rb")

	// declaration in the middle of a file keeps what follows
	mid := "services:\n  a:\n    image: x\nvolumes:\n  v1:\n    driver: local\n  v2:\nnetworks:\n  n: {}\n"
	out, err = DeclareExternalVolume([]byte(mid), "v3", "p_v3")
	if err != nil || !strings.Contains(string(out), "  v2:\n  v3:\n    external: true\n    name: p_v3\nnetworks:") {
		t.Errorf("middle: %v\n%s", err, out)
	}
	assertParses(t, out, "v3")

	// no volumes section: created at the end, even without a trailing newline
	out, err = DeclareExternalVolume([]byte("services:\n  a:\n    image: x"), "v", "p_v")
	if err != nil || string(out) != "services:\n  a:\n    image: x\nvolumes:\n  v:\n    external: true\n    name: p_v\n" {
		t.Errorf("no section: %v %q", err, out)
	}

	// "volumes:" with nothing under it
	out, err = DeclareExternalVolume([]byte("services: {}\nvolumes:\n"), "v", "p_v")
	if err != nil || !strings.Contains(string(out), "volumes:\n  v:\n    external: true") {
		t.Errorf("empty section: %v %q", err, out)
	}
	assertParses(t, out, "v")

	// flow style is not edited
	var me *ManualEditError
	if _, err := DeclareExternalVolume([]byte("volumes: {a: {}}\n"), "v", "p_v"); !errors.As(err, &me) {
		t.Errorf("flow style: %v", err)
	}
	if !VolumeDeclared(out, "v") || VolumeDeclared(out, "zzz") {
		t.Error("VolumeDeclared")
	}
}

func assertParses(t *testing.T, src []byte, key string) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("result is not valid YAML: %v\n%s", err, src)
	}
	vols, _ := doc["volumes"].(map[string]any)
	v, _ := vols[key].(map[string]any)
	if v == nil || v["external"] != true {
		t.Errorf("volume %q not declared as external: %v", key, vols)
	}
}

func TestApplyEditsAndUndo(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.yml", "one\n")
	b := write(t, dir, "b.yml", "two\n")
	edits := []FileEdit{
		{Path: a, Before: []byte("one\n"), After: []byte("ONE\n")},
		{Path: b, Before: []byte("two\n"), After: []byte("TWO\n")},
	}
	undo, err := ApplyEdits(edits)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(a); string(got) != "ONE\n" {
		t.Errorf("a = %q", got)
	}
	if fi, _ := os.Stat(a); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{a: "one\n", b: "two\n"} {
		if got, _ := os.ReadFile(f); string(got) != want {
			t.Errorf("%s after undo = %q", f, got)
		}
	}
	if _, err := os.Stat(a + ".composesafe-tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}

	// a failing second write undoes the first
	edits[1].Path = filepath.Join(dir, "no-such-dir", "b.yml")
	undo, err = ApplyEdits(edits)
	if err == nil {
		t.Fatal("expected a write error")
	}
	_ = undo()
	if got, _ := os.ReadFile(a); string(got) != "one\n" {
		t.Errorf("a must be restored after a partial failure, got %q", got)
	}
}
