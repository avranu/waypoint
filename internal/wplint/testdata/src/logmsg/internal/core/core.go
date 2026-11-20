package core

import (
	"context"
	"fmt"
	"log/slog"
)

type Service struct{ log *slog.Logger }

const prefix = "sweep: "

func (s *Service) sweep(ctx context.Context, round int, repo string) {
	s.log.WarnContext(ctx, "sweep failed", "round", round)
	s.log.WarnContext(ctx, prefix+"failed") // constant
	s.log.WarnContext(ctx, ("sweep failed"))
	s.log.WarnContext(ctx, fmt.Sprintf("sweep of round %d failed", round)) // want `a warning's message is fixed text`
	s.log.ErrorContext(ctx, "sync failed for "+repo)                       // want `a warning's message is fixed text`
	s.log.Warn(fmt.Sprint("round ", round))                                // want `a warning's message is fixed text`
	s.log.Error(repo[:3])                                                  // want `a warning's message is fixed text`
	s.log.InfoContext(ctx, fmt.Sprintf("swept round %d", round))           // only warnings and errors make problems
}

// A helper told what to call its step logs one of a few fixed texts.
func (s *Service) step(ctx context.Context, what string, err error) {
	s.log.WarnContext(ctx, what, "err", err)
}

func (s *Service) waived(ctx context.Context, n int) {
	//wplint:ignore logmsg there are only three of these
	s.log.WarnContext(ctx, fmt.Sprintf("stage %d failed", n))
}

// Log and LogAttrs take the level first: the message is the third argument,
// and a level below Warn makes no problem.
func (s *Service) leveled(ctx context.Context, round int, level slog.Level) {
	s.log.Log(ctx, slog.LevelWarn, fmt.Sprintf("round %d", round))                 // want `a warning's message is fixed text`
	s.log.LogAttrs(ctx, slog.LevelError, "round "+fmt.Sprint(round))               // want `a warning's message is fixed text`
	s.log.Log(ctx, level, fmt.Sprintf("round %d", round))                          // want `a warning's message is fixed text`
	s.log.Log(ctx, slog.LevelInfo, fmt.Sprintf("round %d", round))                 // below a warning
	s.log.LogAttrs(ctx, slog.LevelDebug, fmt.Sprintf("round %d", round))           // below a warning
	s.log.Log(ctx, slog.LevelWarn, "round failed", "round", round)                 // fixed text
	s.log.LogAttrs(ctx, slog.LevelError, "round failed", slog.Int("round", round)) // fixed text
}

// The package-level functions log through the default logger.
func packageLevel(ctx context.Context, round int) {
	slog.Warn(fmt.Sprintf("round %d", round))              // want `a warning's message is fixed text`
	slog.Error("round " + fmt.Sprint(round))               // want `a warning's message is fixed text`
	slog.WarnContext(ctx, fmt.Sprintf("round %d", round))  // want `a warning's message is fixed text`
	slog.ErrorContext(ctx, fmt.Sprint("round ", round))    // want `a warning's message is fixed text`
	slog.Log(ctx, slog.LevelError, fmt.Sprint(round))      // want `a warning's message is fixed text`
	slog.Log(ctx, slog.LevelInfo, fmt.Sprint(round))       // below a warning
	slog.Info(fmt.Sprintf("round %d", round))              // only warnings and errors make problems
	slog.Warn("round failed", "round", round)              // fixed text
	slog.ErrorContext(ctx, "round failed", "round", round) // fixed text
}

// A local variable is as good as its values: one built from a call or a
// concatenation makes a problem of each value.
func (s *Service) variable(ctx context.Context, round int, repo string, err error) {
	msg := fmt.Sprintf("round %d failed", round)
	s.log.WarnContext(ctx, msg) // want `a warning's message is fixed text`

	var joined string
	joined = "sync failed for " + repo
	s.log.ErrorContext(ctx, joined) // want `a warning's message is fixed text`

	// one assignment among several is enough
	step := "load"
	if round > 3 {
		step = fmt.Sprint("load ", round)
	}
	s.log.Warn(step) // want `a warning's message is fixed text`

	tail := "failed"
	tail += repo
	s.log.Warn(tail) // want `a warning's message is fixed text`

	// only ever given constants
	fixed := "sync failed"
	if err != nil {
		fixed = "sync failed hard"
	}
	s.log.WarnContext(ctx, fixed, "err", err)
	var declared = prefix + "failed"
	s.log.Error(declared)
}

// A function literal outside any function is checked too.
var onFail = func(l *slog.Logger, round int) {
	l.Warn(fmt.Sprintf("round %d failed", round)) // want `a warning's message is fixed text`
	l.Warn("round failed", "round", round)
}
