// Package dom is a domain under internal/core: its Service writes through the
// kernel, and the rule follows it there.
package dom

import (
	"context"
	"net/http"

	"writeclosure/internal/core/kernel"
)

type Service struct{ k *kernel.Kernel }

func (s *Service) write(ctx context.Context, a *kernel.Actor, f func(t *kernel.Tx) error) error {
	return s.k.Write(ctx, a, f)
}

func (s *Service) notify(msg string) {}

func (s *Service) tell() { s.notify("x") }

func (s *Service) Bad(ctx context.Context, a *kernel.Actor) error {
	return s.write(ctx, a, func(t *kernel.Tx) error {
		s.notify("now")                           // want `a notification \(notify\) inside Service.write`
		s.tell()                                  // want `tell reaches a notification \(tell → notify\) inside Service.write`
		_, _ = http.Get("http://example.invalid") // want `an HTTP call \(Get\) inside Service.write`
		return nil
	})
}

// BadDirect writes through the kernel and not through a helper.
func (s *Service) BadDirect(ctx context.Context, a *kernel.Actor) error {
	return s.k.Write(ctx, a, func(t *kernel.Tx) error {
		s.notify("now") // want `a notification \(notify\) inside Kernel.Write`
		return nil
	})
}

func (s *Service) BadSkip(ctx context.Context, a *kernel.Actor) error {
	return s.k.WriteSkip(ctx, a, 1, func(t *kernel.Tx) error {
		s.notify("now") // want `a notification \(notify\) inside Kernel.WriteSkip`
		return nil
	})
}

// BadPart runs the call in a part of the transaction, still before the commit.
func (s *Service) BadPart(ctx context.Context, a *kernel.Actor) error {
	return s.write(ctx, a, func(t *kernel.Tx) error {
		return t.Part(ctx, func() error {
			s.notify("now") // want `a notification \(notify\) inside Service.write`
			return nil
		})
	})
}

func (s *Service) Good(ctx context.Context, a *kernel.Actor) error {
	if err := s.k.Write(ctx, a, func(t *kernel.Tx) error { return nil }); err != nil {
		return err
	}
	s.notify("after the commit")
	return nil
}
