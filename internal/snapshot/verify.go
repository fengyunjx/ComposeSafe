package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"composesafe/internal/pg"
)

// minArtifactSize is the smallest believable size of a Postgres dump,
// counted uncompressed (an empty cluster dumps to a few KiB but compresses
// below 1 KiB). A constant on purpose. Volume archives have no size floor.
const minArtifactSize = 1024

// Problem is one thing wrong with a snapshot.
type Problem struct {
	Path   string
	Reason string
}

func (p Problem) String() string { return p.Path + ": " + p.Reason }

// Result is the outcome of Verify.
type Result struct {
	Problems []Problem
	// Checksums is true when every listed file exists with the recorded size
	// and SHA-256.
	Checksums bool
	// DumpTrailer is true when every dump ends with the pg_dumpall trailer
	// (vacuously true when there are no dumps).
	DumpTrailer bool
}

func (r Result) OK() bool { return len(r.Problems) == 0 }

// Verify re-reads every artifact of the snapshot in dir: checksum, size,
// stream integrity, and for dumps the completion marker. Any problem means
// the snapshot must not be used to justify an upgrade.
func Verify(dir string, m *Manifest) Result {
	res := Result{Checksums: true, DumpTrailer: true}
	fail := func(a Artifact, reason string, checksum, trailer bool) {
		res.Problems = append(res.Problems, Problem{Path: a.Path, Reason: reason})
		if checksum {
			res.Checksums = false
		}
		if trailer {
			res.DumpTrailer = false
		}
	}
	for _, a := range m.Artifacts {
		full := filepath.Join(dir, a.Path)
		sum, size, err := fileSum(full)
		if err != nil {
			fail(a, "unreadable: "+err.Error(), true, a.Kind == KindPGDump || a.Kind == KindPGUpgradeDump)
			continue
		}
		if size != a.Size {
			fail(a, fmt.Sprintf("size is %d, manifest says %d", size, a.Size), true, false)
			continue
		}
		if sum != a.SHA256 {
			fail(a, "SHA-256 does not match the manifest", true, false)
			continue
		}
		switch a.Kind {
		case KindPGDump, KindPGUpgradeDump:
			if a.RawSize < minArtifactSize {
				fail(a, fmt.Sprintf("dump is only %d bytes: too small to be a cluster dump", a.RawSize), false, true)
				continue
			}
			tail, err := gzipTail(full, 4096)
			if err != nil {
				fail(a, "corrupt gzip stream: "+err.Error(), true, true)
			} else if !bytes.Contains(tail, []byte(pg.Trailer)) {
				fail(a, "dump is truncated: completion marker missing", false, true)
			}
		case KindVolume:
			// No size floor: a small volume legitimately archives to a few
			// hundred bytes. What matters is that the stream is intact and
			// that it holds at least one file.
			if n, err := tarEntries(full); err != nil {
				fail(a, "corrupt archive: "+err.Error(), true, false)
			} else if n == 0 {
				fail(a, "archive contains no files", false, false)
			}
		}
	}
	return res
}

func fileSum(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SumFile returns the SHA-256 and size of a file; exported for the code that
// builds artifacts.
func SumFile(path string) (string, int64, error) { return fileSum(path) }

// gzipTail decompresses the whole file (which also proves the stream is
// intact) and returns its last n bytes.
func gzipTail(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tail := make([]byte, 0, n)
	buf := make([]byte, 64*1024)
	for {
		k, err := gz.Read(buf)
		tail = append(tail, buf[:k]...)
		if len(tail) > n {
			tail = append(tail[:0], tail[len(tail)-n:]...)
		}
		if errors.Is(err, io.EOF) {
			return tail, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// tarEntries counts the entries of a .tgz, reading it to the end.
func tarEntries(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return n, err
		}
		// The top-level "." entry alone is an empty directory.
		if strings.TrimPrefix(h.Name, "./") != "" && h.Name != "." {
			n++
		}
	}
}
