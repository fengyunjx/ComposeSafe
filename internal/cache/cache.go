// Package cache is a tiny on-disk JSON cache with a TTL, used for registry
// tag lists and release notes so repeated `plan` runs stay under API limits.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Dir is the cache directory. The zero value disables caching.
type Dir string

func (d Dir) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(string(d), hex.EncodeToString(sum[:])+".json")
}

// Get loads key into v if it was stored less than ttl ago.
func (d Dir) Get(key string, ttl time.Duration, v any) bool {
	if d == "" {
		return false
	}
	p := d.path(key)
	fi, err := os.Stat(p)
	if err != nil || time.Since(fi.ModTime()) > ttl {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// Put stores v under key. Failures are ignored by callers: a cache is only
// an optimisation.
func (d Dir) Put(key string, v any) error {
	if d == "" {
		return nil
	}
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := d.path(key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path(key))
}
