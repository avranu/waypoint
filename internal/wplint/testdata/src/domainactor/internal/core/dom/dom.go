// Package dom is a domain under internal/core: core.Service embeds its Service.
package dom

import (
	"context"

	"domainactor/internal/core/kernel"
)

type Service struct {
	Open          int // want `Service.Open is exported`
	hidden        int
	*kernel.Actor // want `Service.Actor is exported`
}

func (s *Service) Get(ctx context.Context, actor *kernel.Actor, id int64) error { return nil }

func (s *Service) Later(ctx context.Context, id int64, actor *kernel.Actor) error { return nil }

func (s *Service) Bare(ctx context.Context, id int64) error { return nil } // want `exported Service.Bare has no \*kernel.Actor parameter`

func (s Service) ByValue(ctx context.Context, actor kernel.Actor) error { return nil } // want `exported Service.ByValue has no \*kernel.Actor parameter`

func (s *Service) Wrong(ctx context.Context, actor *Actor) error { return nil } // want `exported Service.Wrong has no \*kernel.Actor parameter`

func (s *Service) internal(id int64) {}

// Actor is this domain's own type, not the kernel's.
type Actor struct{}

// Helper is not a method of Service.
type Helper struct{}

func (h *Helper) Anything() {}

func Free(id int64) {}
