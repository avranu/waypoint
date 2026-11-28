package kernel

import (
	"context"
	"net/http"
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

// Bad is a kernel method that runs a network call before its own commit.
func (k *Kernel) Bad(ctx context.Context) error {
	return k.Write(ctx, nil, func(t *Tx) error {
		_, _ = http.Get("http://example.invalid") // want `an HTTP call \(Get\) inside Kernel.Write`
		return nil
	})
}
