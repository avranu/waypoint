// Package dom is a domain under internal/core.
package dom

import (
	"context"

	"actorused/internal/core/kernel"
)

type Service struct{}

func (s *Service) Get(ctx context.Context, actor *kernel.Actor, id int64) error { // want `Get takes an \*Actor and never reads it`
	return nil
}

func (s *Service) Drop(ctx context.Context, _ *kernel.Actor) error { // want `Drop takes an \*Actor and drops it`
	return nil
}

func (s *Service) Checked(ctx context.Context, actor *kernel.Actor) error {
	if actor == nil {
		return nil
	}
	return nil
}
