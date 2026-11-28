package kernel

import (
	"context"
	"time"
)

type Actor struct{}

type Tx struct{ Actor *Actor }

func (t *Tx) Part(ctx context.Context, f func() error) error { return f() }

type Kernel struct{ clock func() time.Time }

// Now is the clock itself: the one place time.Now is the answer.
func (k *Kernel) Now() time.Time { return time.Now() }

func (k *Kernel) NowMS() int64 { return time.Now().UnixMilli() }

func (k *Kernel) Write(ctx context.Context, a *Actor, f func(t *Tx) error) error {
	return f(&Tx{Actor: a})
}

func (k *Kernel) WriteSkip(ctx context.Context, a *Actor, skip int, f func(t *Tx) error) error {
	return f(&Tx{Actor: a})
}

func work() {}

func (t *Tx) Stamp() time.Time {
	return time.Now() // want `use s.now\(\), the clock tests control`
}

func (k *Kernel) Took() time.Duration {
	start := time.Now()
	work()
	return time.Since(start)
}

func (k *Kernel) Since(at time.Time) time.Duration {
	return time.Since(at) // want `time.Since measures against the real clock`
}

// stamp holds no Tx or Kernel: nothing to take the clock from.
func stamp() time.Time { return time.Now() }
