package pg

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scripted returns an Exec that answers by looking at the script text.
func scripted(answers map[string]string, seen *[]string) Exec {
	return func(_ context.Context, script string, in io.Reader, out io.Writer) (string, error) {
		if seen != nil {
			*seen = append(*seen, script)
		}
		for key, ans := range answers {
			if strings.Contains(script, key) {
				_, _ = io.WriteString(out, ans)
				return "", nil
			}
		}
		return "", errors.New("unexpected script: " + script)
	}
}

func TestShq(t *testing.T) {
	if got := shq("it's"); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestPreludeValidatesPasswordVariable(t *testing.T) {
	if _, _, err := prelude("u", "DB_PASS; rm"); err == nil {
		t.Error("a variable name with shell syntax must be rejected")
	}
	pre, host, err := prelude("app'user", "DB_PASS")
	if err != nil || !strings.Contains(pre, `PG_USER='app'\''user'`) || !strings.Contains(pre, `PGPASSWORD="$DB_PASS"`) || host == "" {
		t.Errorf("%q %q %v", pre, host, err)
	}
	pre, host, _ = prelude("u", "")
	if strings.Contains(pre, "PGPASSWORD") || host != "" {
		t.Errorf("no password configured: %q %q", pre, host)
	}
}

func TestServerUserAndVersion(t *testing.T) {
	ex := scripted(map[string]string{"POSTGRES_USER": "appuser", "server_version": "16.4 (Debian 16.4-1)\n"}, nil)
	u, err := ServerUser(t.Context(), ex)
	if err != nil || u != "appuser" {
		t.Fatalf("%q %v", u, err)
	}
	v, err := Version(t.Context(), ex, "appuser", "")
	if err != nil || v != "16.4" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := ServerUser(t.Context(), scripted(map[string]string{"POSTGRES_USER": " "}, nil)); err == nil {
		t.Error("empty user must be an error")
	}
}

func TestTableCounts(t *testing.T) {
	ex := scripted(map[string]string{"pg_database": "postgres 1\nmy db 12\nother 0\n\n"}, nil)
	got, err := TableCounts(t.Context(), ex, "u", "")
	if err != nil || got["postgres"] != 1 || got["my db"] != 12 || got["other"] != 0 || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	bad := scripted(map[string]string{"pg_database": "postgres many\n"}, nil)
	if _, err := TableCounts(t.Context(), bad, "u", ""); err == nil {
		t.Error("non-numeric count must be an error")
	}
}

func TestDumpStreamsCompressesAndHashes(t *testing.T) {
	body := strings.Repeat("-- some dump text\n", 500) + Trailer + "\n"
	var script string
	ex := func(_ context.Context, s string, _ io.Reader, out io.Writer) (string, error) {
		script = s
		_, err := io.WriteString(out, body)
		return "", err
	}
	path := filepath.Join(t.TempDir(), "db.sql.gz")
	d, err := Dump(t.Context(), ex, "appuser", "DB_PASS", path)
	if err != nil {
		t.Fatal(err)
	}
	if d.RawSize != int64(len(body)) || d.Size <= 0 || d.Size >= d.RawSize {
		t.Errorf("sizes: %+v", d)
	}
	if !strings.Contains(script, "pg_dumpall") || !strings.Contains(script, "-h 127.0.0.1") || strings.Contains(script, "secret") {
		t.Errorf("script: %s", script)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 || fi.Size() != d.Size {
		t.Errorf("file: %v size %d", fi.Mode(), fi.Size())
	}
	f, _ := os.Open(path)
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var round bytes.Buffer
	round.ReadFrom(gz)
	if round.String() != body {
		t.Error("round trip differs")
	}
	// the file must not be overwritten silently
	if _, err := Dump(t.Context(), ex, "u", "", path); err == nil {
		t.Error("existing output file must be refused")
	}
}

func TestDumpFailureLeavesNoFile(t *testing.T) {
	ex := func(_ context.Context, _ string, _ io.Reader, out io.Writer) (string, error) {
		io.WriteString(out, "partial")
		return "pg_dumpall: error: connection failed", errors.New("exit status 1")
	}
	path := filepath.Join(t.TempDir(), "db.sql.gz")
	_, err := Dump(t.Context(), ex, "u", "", path)
	if err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a failed dump must not leave a file that looks like a backup")
	}
}

func TestFilterRestoreErrors(t *testing.T) {
	stderr := `psql:<stdin>:12: ERROR:  role "postgres" already exists
psql:<stdin>:13: ERROR:  role "appuser" already exists
psql:<stdin>:96: ERROR:  database "appuser" already exists
NOTICE: something
psql:<stdin>:200: ERROR:  relation "t" already exists`
	got := FilterRestoreErrors(stderr)
	if len(got) != 2 || !strings.Contains(got[0], `database "appuser"`) || !strings.Contains(got[1], `relation "t"`) {
		t.Errorf("got %v", got)
	}
	if len(FilterRestoreErrors("")) != 0 {
		t.Error("no stderr, no errors")
	}
}

func TestRestoreReplaysTheDump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.sql.gz")
	Dump(t.Context(), func(_ context.Context, _ string, _ io.Reader, out io.Writer) (string, error) {
		io.WriteString(out, "SELECT 1;\n")
		return "", nil
	}, "u", "", path)

	var gotIn string
	ex := func(_ context.Context, script string, in io.Reader, _ io.Writer) (string, error) {
		b, _ := io.ReadAll(in)
		gotIn = string(b)
		if !strings.Contains(script, "ON_ERROR_STOP=0") {
			t.Errorf("script: %s", script)
		}
		return `psql:<stdin>:3: ERROR:  role "u" already exists` + "\n" + `psql:<stdin>:9: ERROR:  syntax error`, nil
	}
	errs, err := Restore(t.Context(), ex, "u", "", path)
	if err != nil || gotIn != "SELECT 1;\n" {
		t.Fatalf("in=%q err=%v", gotIn, err)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "syntax error") {
		t.Errorf("errs = %v", errs)
	}
	if _, err := Restore(t.Context(), ex, "u", "", filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dump must be an error")
	}
}

func TestWaitReady(t *testing.T) {
	n := 0
	ex := func(context.Context, string, io.Reader, io.Writer) (string, error) {
		n++
		if n < 3 {
			return "", errors.New("no response")
		}
		return "", nil
	}
	slept := 0
	if err := WaitReady(t.Context(), ex, "u", 5, time.Second, func(time.Duration) { slept++ }); err != nil || n != 3 || slept != 2 {
		t.Errorf("err=%v attempts=%d sleeps=%d", err, n, slept)
	}
	n = -100
	if err := WaitReady(t.Context(), ex, "u", 3, time.Second, func(time.Duration) {}); err == nil {
		t.Error("must give up")
	}
}
