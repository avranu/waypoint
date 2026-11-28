package core

import (
	"context"
	"net/http"

	"writeclosure/internal/core/kernel"
)

type Actor struct{}

// tx wraps the kernel's transaction, as core's does.
type tx struct{ *kernel.Tx }

type Service struct{ k *kernel.Kernel }

func (s *Service) write(ctx context.Context, a *Actor, f func(t *tx) error) error {
	if err := f(&tx{}); err != nil {
		return err
	}
	s.dispatch(ctx)
	return nil
}

func (s *Service) dispatch(ctx context.Context) {}

func (s *Service) notify(msg string) {}

func (s *Service) goBG(f func()) { go f() }

func (s *Service) tell() { s.notify("x") }

// ensure writes on its own when it isn't given a transaction.
func (s *Service) ensure(ctx context.Context) error {
	return s.write(ctx, nil, func(t *tx) error { return nil })
}

func noteRead() {}

func (s *Service) Bad(ctx context.Context, a *Actor) error {
	return s.write(ctx, a, func(t *tx) error {
		s.notify("now")                           // want `a notification \(notify\) inside Service.write`
		s.tell()                                  // want `tell reaches a notification \(tell → notify\)`
		_, _ = http.Get("http://example.invalid") // want `an HTTP call \(Get\) inside Service.write`
		s.goBG(func() { s.notify("later") })      // want `a notification \(notify\)`
		return nil
	})
}

func (s *Service) Good(ctx context.Context, a *Actor) error {
	if err := s.write(ctx, a, func(t *tx) error {
		if err := s.ensure(ctx); err != nil {
			return err
		}
		s.goBG(noteRead)
		s.goBG(func() { noteRead() })
		return nil
	}); err != nil {
		return err
	}
	s.notify("after the commit")
	return nil
}

func (s *Service) Waived(ctx context.Context, a *Actor) error {
	return s.write(ctx, a, func(t *tx) error {
		//wplint:ignore writeclosure a test of the waiver
		s.notify("now")
		return nil
	})
}

// Wrapped reaches the kernel's Part through core's tx and Write through the
// kernel, as core does now.
func (s *Service) Wrapped(ctx context.Context, a *Actor) error {
	return s.k.Write(ctx, nil, func(kt *kernel.Tx) error {
		t := &tx{Tx: kt}
		return t.Part(ctx, func() error {
			s.notify("now") // want `a notification \(notify\) inside Kernel.Write`
			return nil
		})
	})
}
