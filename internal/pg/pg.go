// Package pg talks to a Postgres server through a shell inside its
// container. Callers supply the Exec function, which runs a script in a
// compose service (or in a throwaway container for the restore check), so
// this package never builds docker command lines and never needs a password
// on a command line.
package pg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Exec runs a shell script in the Postgres container, feeding in to its stdin
// and streaming its stdout to out. It returns the script's stderr.
type Exec func(ctx context.Context, script string, in io.Reader, out io.Writer) (stderr string, err error)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shq quotes s for use in a POSIX shell script.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// prelude sets PG_USER and, when the server needs a password, PGPASSWORD
// taken from a variable of the container's own environment. The value never
// leaves the container and never appears on a command line.
func prelude(user, passwordEnv string) (string, string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "PG_USER=%s; ", shq(user))
	host := ""
	if passwordEnv != "" {
		if !envName.MatchString(passwordEnv) {
			return "", "", fmt.Errorf("pg_password_env %q is not a valid variable name", passwordEnv)
		}
		fmt.Fprintf(&b, `PGPASSWORD="$%s"; export PGPASSWORD; `, passwordEnv)
		host = " -h 127.0.0.1" // password auth applies to TCP, not the local socket
	}
	return b.String(), host, nil
}

// ServerUser returns the superuser name configured for the container
// (POSTGRES_USER, default postgres).
func ServerUser(ctx context.Context, ex Exec) (string, error) {
	var out bytes.Buffer
	if _, err := ex(ctx, `printf '%s' "${POSTGRES_USER:-postgres}"`, nil, &out); err != nil {
		return "", err
	}
	u := strings.TrimSpace(out.String())
	if u == "" {
		return "", errors.New("could not determine the Postgres user")
	}
	return u, nil
}

// Version returns the server version, e.g. "16.4".
func Version(ctx context.Context, ex Exec, user, passwordEnv string) (string, error) {
	pre, host, err := prelude(user, passwordEnv)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	script := pre + `psql -U "$PG_USER"` + host + ` -d postgres -X -tAc "show server_version"`
	if stderr, err := ex(ctx, script, nil, &out); err != nil {
		return "", fmt.Errorf("server version: %w: %s", err, strings.TrimSpace(stderr))
	}
	f := strings.Fields(out.String())
	if len(f) == 0 {
		return "", errors.New("server version: empty answer")
	}
	return f[0], nil
}

// tableCountSQL counts user tables (ordinary and partitioned) of one database.
const tableCountSQL = `select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace ` +
	`where c.relkind in ('r','p') and n.nspname not in ('pg_catalog','information_schema') and n.nspname not like 'pg_toast%'`

// TableCounts returns the number of user tables in every connectable,
// non-template database. The restore check compares these numbers between the
// source and the restored copy.
func TableCounts(ctx context.Context, ex Exec, user, passwordEnv string) (map[string]int, error) {
	pre, host, err := prelude(user, passwordEnv)
	if err != nil {
		return nil, err
	}
	script := pre + `for d in $(psql -U "$PG_USER"` + host + ` -d postgres -X -tAc "select datname from pg_database where datallowconn and not datistemplate"); do ` +
		`echo "$d $(psql -U "$PG_USER"` + host + ` -d "$d" -X -tAc ` + shq(tableCountSQL) + `)"; done`
	var out bytes.Buffer
	if stderr, err := ex(ctx, script, nil, &out); err != nil {
		return nil, fmt.Errorf("table counts: %w: %s", err, strings.TrimSpace(stderr))
	}
	counts := map[string]int{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(f[len(f)-1])
		if err != nil {
			return nil, fmt.Errorf("table counts: unexpected line %q", sc.Text())
		}
		counts[strings.Join(f[:len(f)-1], " ")] = n
	}
	return counts, nil
}

// Dumped describes a finished dump file.
type Dumped struct {
	SHA256  string // of the stored (compressed) file
	Size    int64  // stored size
	RawSize int64  // size before compression
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Dump writes `pg_dumpall` of the whole cluster (roles, tablespaces and all
// databases) to outPath, gzip-compressed, mode 0600. The file must not exist.
// The stream is never held in memory. A failed dump leaves no file behind.
func Dump(ctx context.Context, ex Exec, user, passwordEnv, outPath string) (Dumped, error) {
	pre, host, err := prelude(user, passwordEnv)
	if err != nil {
		return Dumped{}, err
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Dumped{}, err
	}
	h := sha256.New()
	stored := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(stored)
	raw := &countWriter{w: gz}

	stderr, runErr := ex(ctx, pre+`exec pg_dumpall -U "$PG_USER"`+host, nil, raw)
	closeErr := gz.Close()
	if err := f.Close(); closeErr == nil {
		closeErr = err
	}
	if runErr != nil || closeErr != nil {
		_ = os.Remove(outPath)
		if runErr != nil {
			return Dumped{}, fmt.Errorf("pg_dumpall: %w: %s", runErr, strings.TrimSpace(stderr))
		}
		return Dumped{}, closeErr
	}
	return Dumped{SHA256: hex.EncodeToString(h.Sum(nil)), Size: stored.n, RawSize: raw.n}, nil
}

// Trailer is the last line of a complete pg_dumpall file.
const Trailer = "-- PostgreSQL database cluster dump complete"

// restoreIgnorable are errors that are expected when a dump is replayed into a
// fresh cluster: the bootstrap superuser already exists there. Keep this list
// at one entry; add another only for a proven false alarm (DESIGN 5.7.1).
var restoreIgnorable = []*regexp.Regexp{
	regexp.MustCompile(`role ".*" already exists`),
}

// FilterRestoreErrors returns the ERROR lines of psql's stderr that are not on
// the ignore list.
func FilterRestoreErrors(stderr string) []string {
	var out []string
nextLine:
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "ERROR:") {
			continue
		}
		for _, re := range restoreIgnorable {
			if re.MatchString(line) {
				continue nextLine
			}
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

// Restore replays a dump file made by Dump into the server and returns the
// unexpected errors psql reported. ON_ERROR_STOP is off on purpose: replaying
// into a fresh cluster always produces a few harmless errors, so the output is
// filtered instead.
func Restore(ctx context.Context, ex Exec, user, passwordEnv, dumpPath string) ([]string, error) {
	pre, host, err := prelude(user, passwordEnv)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(dumpPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dumpPath, err)
	}
	defer gz.Close()
	script := pre + `exec psql -U "$PG_USER"` + host + ` -d postgres -X -q -v ON_ERROR_STOP=0 -f -`
	stderr, err := ex(ctx, script, gz, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("psql restore: %w: %s", err, strings.TrimSpace(stderr))
	}
	return FilterRestoreErrors(stderr), nil
}

// WaitReady polls pg_isready until the server accepts connections.
func WaitReady(ctx context.Context, ex Exec, user string, attempts int, interval time.Duration, sleep func(time.Duration)) error {
	script := `pg_isready -q -U ` + shq(user)
	var last error
	for i := 0; i < attempts; i++ {
		if _, err := ex(ctx, script, nil, io.Discard); err == nil {
			return nil
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleep(interval)
	}
	return fmt.Errorf("postgres did not become ready (%d attempts): %w", attempts, last)
}

// Analyze refreshes the planner statistics of every database. A dump and
// restore does not carry them over, and queries stay slow until they exist.
func Analyze(ctx context.Context, ex Exec, user, passwordEnv string) error {
	pre, host, err := prelude(user, passwordEnv)
	if err != nil {
		return err
	}
	script := pre + `exec vacuumdb -U "$PG_USER"` + host + ` --all --analyze-only -q`
	if stderr, err := ex(ctx, script, nil, io.Discard); err != nil {
		return fmt.Errorf("analyze: %w: %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

// CompareTables checks that the restored cluster has every database of the
// source with at least as many user tables. It returns a description of the
// first difference, or "".
func CompareTables(source, restored map[string]int) string {
	for db, want := range source {
		got, ok := restored[db]
		switch {
		case !ok:
			return fmt.Sprintf("database %q is missing after the restore", db)
		case got < want:
			return fmt.Sprintf("database %q has %d user tables after the restore, %d before", db, got, want)
		}
	}
	return ""
}
