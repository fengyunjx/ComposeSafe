package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"composesafe/internal/pg"
)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

// tgz builds a tar.gz with the given file names; contents are random so the
// archive does not compress away below the minimum size.
func tgz(t *testing.T, names ...string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, n := range names {
		data := make([]byte, 2048)
		rand.Read(data)
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
	}
	tw.Close()
	return gz(t, raw.Bytes())
}

func dumpText(trailer bool) []byte {
	s := strings.Repeat("-- dump line\n", 200)
	if trailer {
		s += pg.Trailer + "\n"
	}
	return []byte(s)
}

// fixture writes a consistent snapshot with a dump and an archive.
func fixture(t *testing.T) (string, *Manifest) {
	t.Helper()
	dir := t.TempDir()
	m := &Manifest{Schema: SchemaVersion, RunID: "r1"}
	add := func(a Artifact, content []byte) {
		os.WriteFile(filepath.Join(dir, a.Path), content, 0o600)
		sum, size, _ := SumFile(filepath.Join(dir, a.Path))
		a.SHA256, a.Size = sum, size
		m.Artifacts = append(m.Artifacts, a)
	}
	raw := dumpText(true)
	add(Artifact{Kind: KindPGDump, Service: "db", Path: "db.sql.gz", RawSize: int64(len(raw))}, gz(t, raw))
	add(Artifact{Kind: KindVolume, Service: "db", Path: "pgdata.tgz"}, tgz(t, "./", "./PG_VERSION", "./base/1"))
	add(Artifact{Kind: KindCompose, Path: "compose.yml"}, []byte("services: {}\n"))
	return dir, m
}

func TestVerifyAcceptsAGoodSnapshot(t *testing.T) {
	dir, m := fixture(t)
	if r := Verify(dir, m); !r.OK() || !r.Checksums || !r.DumpTrailer {
		t.Fatalf("%+v", r)
	}
}

func TestVerifyCatchesEachKindOfDamage(t *testing.T) {
	tests := []struct {
		name     string
		damage   func(t *testing.T, dir string, m *Manifest)
		want     string
		checksum bool // expected Result.Checksums
		trailer  bool // expected Result.DumpTrailer
	}{
		{"file deleted", func(t *testing.T, dir string, m *Manifest) { os.Remove(filepath.Join(dir, "db.sql.gz")) }, "unreadable", false, false},
		{"bit flip", func(t *testing.T, dir string, m *Manifest) {
			p := filepath.Join(dir, "pgdata.tgz")
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 1
			os.WriteFile(p, b, 0o600)
		}, "SHA-256", false, true},
		{"size differs", func(t *testing.T, dir string, m *Manifest) {
			m.Artifacts[2].Size++
		}, "size is", false, true},
		{"dump truncated (no trailer)", func(t *testing.T, dir string, m *Manifest) {
			setContent(t, dir, m, 0, gz(t, dumpText(false)), int64(len(dumpText(false))))
		}, "completion marker missing", true, false},
		{"dump too small", func(t *testing.T, dir string, m *Manifest) {
			small := []byte(pg.Trailer + "\n")
			setContent(t, dir, m, 0, gz(t, small), int64(len(small)))
		}, "too small", true, false},
		{"gzip stream cut off", func(t *testing.T, dir string, m *Manifest) {
			full := gz(t, dumpText(true))
			setContent(t, dir, m, 0, full[:len(full)-12], 4096)
		}, "corrupt gzip", false, false},
		{"archive with no files", func(t *testing.T, dir string, m *Manifest) {
			setContent(t, dir, m, 1, tgz(t, "./"), 0)
		}, "contains no files", true, true},
		{"archive not gzip", func(t *testing.T, dir string, m *Manifest) {
			setContent(t, dir, m, 1, bytes.Repeat([]byte("x"), 4096), 0)
		}, "corrupt archive", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, m := fixture(t)
			tc.damage(t, dir, m)
			r := Verify(dir, m)
			if r.OK() {
				t.Fatal("damage went unnoticed")
			}
			if !strings.Contains(r.Problems[0].Reason, tc.want) {
				t.Errorf("reason = %q, want containing %q", r.Problems[0].Reason, tc.want)
			}
			if r.Checksums != tc.checksum || r.DumpTrailer != tc.trailer {
				t.Errorf("checksums=%v trailer=%v, want %v %v", r.Checksums, r.DumpTrailer, tc.checksum, tc.trailer)
			}
		})
	}
}

// setContent replaces an artifact's file and updates the manifest so that only
// the property under test is wrong (checksum and size stay consistent).
func setContent(t *testing.T, dir string, m *Manifest, i int, content []byte, raw int64) {
	t.Helper()
	a := &m.Artifacts[i]
	os.WriteFile(filepath.Join(dir, a.Path), content, 0o600)
	a.SHA256, a.Size, _ = func() (string, int64, error) { return SumFile(filepath.Join(dir, a.Path)) }()
	if a.Kind == KindPGDump {
		a.RawSize = raw
	}
}

func TestEmptyTarWithOnlyTheRootEntry(t *testing.T) {
	dir, m := fixture(t)
	// an archive of an empty directory is a valid tar with just "./"; pad the
	// stored size past the minimum with gzip-stored garbage entries is not
	// possible, so assert the entry counter directly
	p := filepath.Join(dir, "only-root.tgz")
	os.WriteFile(p, tgz(t, "./"), 0o600)
	n, err := tarEntries(p)
	if err != nil || n != 0 {
		t.Errorf("entries = %d, err = %v", n, err)
	}
	_ = m
}

func TestManifestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Schema: SchemaVersion, RunID: "r1", Mode: "logical", Complete: true,
		Services: []Service{{Name: "db", PGTables: map[string]int{"postgres": 3}}}}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "manifest.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json.tmp")); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}
	got, err := Load(dir)
	if err != nil || got.RunID != "r1" || !got.Complete || got.Services[0].PGTables["postgres"] != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got := got.Find(KindPGDump, ""); len(got) != 0 {
		t.Error("Find on an empty list")
	}
}

func TestLoadRefusesUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema": 99}`), 0o600)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "newer composesafe") {
		t.Errorf("err = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{broken`), 0o600)
	if _, err := Load(dir); err == nil {
		t.Error("corrupt manifest must fail")
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("missing manifest must fail")
	}
}
