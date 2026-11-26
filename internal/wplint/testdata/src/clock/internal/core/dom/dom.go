// Package dom is a domain under internal/core: its time comes from the kernel.
package dom

import (
	"time"

	"clock/internal/core/kernel"
)

type Service struct{ k *kernel.Kernel }

func (s *Service) Stamp() time.Time {
	return time.Now() // want `use s.now\(\), the clock tests control`
}

func (s *Service) Good() time.Time { return s.k.Now() }

func (s *Service) Took() time.Duration {
	start := time.Now()
	return time.Since(start)
}

func inTx(t *kernel.Tx) time.Time {
	return time.Now() // want `use s.now\(\)`
}

func withKernel(k *kernel.Kernel) int64 {
	return time.Now().UnixMilli() // want `use s.now\(\)`
}
