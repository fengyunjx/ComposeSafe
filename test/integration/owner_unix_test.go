//go:build integration

package integration

import (
	"os"
	"syscall"
)

func fileUID(fi os.FileInfo) int { return int(fi.Sys().(*syscall.Stat_t).Uid) }
