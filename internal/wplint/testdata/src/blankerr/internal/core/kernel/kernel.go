package kernel

import (
	"context"
	"errors"
)

type Actor struct{}

type Tx struct{}

func (t *Tx) Part(ctx context.Context, f func() error) error { return f() }

func (t *Tx) Exec() error { return errors.New("down") }

type Kernel struct{}

func (k *Kernel) Write(ctx context.Context, a *Actor, f func(t *Tx) error) error {
	return f(&Tx{})
}

func (k *Kernel) Bad(ctx context.Context) {
	_ = k.Write(ctx, nil, func(t *Tx) error { return nil }) // want `the error from Write is discarded`
	k.Write(ctx, nil, func(t *Tx) error { return nil })     // want `the error from Write is dropped`
	_ = k.Write(ctx, nil, func(t *Tx) error {               // want `the error from Write is discarded`
		t.Part(ctx, nil) // want `the error from Part is dropped`
		_ = t.Exec()     // want `the error from Exec is discarded`
		return nil
	})
}
