package core

import (
	"context"
	"log/slog"
)

type Service struct {
	log   *slog.Logger
	bgCtx context.Context
}

func (s *Service) sweep(ctx context.Context) {
	s.log.Warn("sweep failed", "err", nil) // want `use WarnContext\(ctx, …\)`
	s.log.Error("sweep failed")            // want `use ErrorContext\(ctx, …\)`
	s.log.WarnContext(ctx, "sweep failed")
	s.log.Info("swept") // only warnings and errors are counted
	func() {
		s.log.Warn("inside a closure") // want `use WarnContext\(ctx, …\)`
	}()
}

// A context declared later isn't in scope yet.
func (s *Service) later() {
	s.log.Warn("before")
	c := context.Background()
	s.log.Warn("after") // want `use WarnContext\(c, …\)`
	_ = c
}

// No context at hand: a field is the server's, not the caller's.
func (s *Service) none() {
	s.log.Warn("no context")
	s.log.WarnContext(s.bgCtx, "background")
}

func (s *Service) discarded(_ context.Context) {
	s.log.Warn("the parameter is unnamed")
}

func (s *Service) waived(ctx context.Context) {
	//wplint:ignore logctx this one reports on the server, not the request
	s.log.Warn("waived")
	_ = ctx
}

func plain(ctx context.Context, l *slog.Logger) {
	l.Error("a logger from anywhere") // want `use ErrorContext\(ctx, …\)`
	slog.Warn("the package function is not a Logger method")
}
