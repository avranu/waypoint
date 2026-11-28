// Package dom is a domain under internal/core: its transactions are the kernel's.
package dom

import (
	"context"
	"errors"

	"blankerr/internal/core/kernel"
)

type Service struct{ k *kernel.Kernel }

func (s *Service) read() (string, error) { return "", errors.New("down") }

func (s *Service) Bad(ctx context.Context) {
	_, _ = s.read()                                    // want `the error from read is discarded`
	_ = s.k.Write(ctx, nil, func(t *kernel.Tx) error { // want `the error from Write is discarded`
		t.Part(ctx, func() error { return nil }) // want `the error from Part is dropped`
		_ = t.Exec()                             // want `the error from Exec is discarded`
		return nil
	})
	s.k.Write(ctx, nil, nil) // want `the error from Write is dropped`
}

func (s *Service) Good(ctx context.Context) error {
	return s.k.Write(ctx, nil, func(t *kernel.Tx) error {
		if err := t.Exec(); err != nil {
			return err
		}
		return t.Part(ctx, func() error { return nil })
	})
}
