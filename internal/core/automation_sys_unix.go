//go:build unix

package core

import (
	"math"
	"syscall"
)

// diskSpace is the free and total bytes of the filesystem holding dir.
func diskSpace(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) // #nosec G115 -- a block size is positive
	return clampInt64(st.Bavail * bs), clampInt64(st.Blocks * bs), nil
}

func clampInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
