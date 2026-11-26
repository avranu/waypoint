package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"git.jmann.me/claude/waypoint/internal/db"
	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Kernel is what every domain needs: the database, a logger, a clock and the
// write transaction. The owner registers what it knows of its domain (the
// kinds of event to record, how to stamp one, what runs after a commit) so
// the kernel names no domain.
type Kernel struct {
	DB  *db.DB
	Log *slog.Logger
	// AfterCommit, when set, is called with the events a committed write
	// emitted, in order, under the write's span.
	AfterCommit func(ctx context.Context, actor *Actor, events []*Event)

	clock     func() time.Time
	persisted map[string]bool
	stamps    []func(t *Tx, e *Event)
	stages    []*Stage
	batch     []BatchHandler
}

// New returns a Kernel over d, on the real clock.
func New(d *db.DB, log *slog.Logger) *Kernel {
	if log == nil {
		log = slog.Default()
	}
	return &Kernel{DB: d, Log: log, clock: time.Now, persisted: map[string]bool{}}
}

// SetClock replaces the clock; for tests.
func (k *Kernel) SetClock(f func() time.Time) { k.clock = f }

// Now is the time by the kernel's clock.
func (k *Kernel) Now() time.Time { return k.clock() }

// NowMS is Now in Unix milliseconds.
func (k *Kernel) NowMS() int64 { return k.clock().UnixMilli() }

// Persist registers kinds of event that Emit writes to the events table.
func (k *Kernel) Persist(kinds ...string) {
	for _, kind := range kinds {
		k.persisted[kind] = true
	}
}

// Persisted reports whether events of kind are written to the events table.
func (k *Kernel) Persisted(kind string) bool { return k.persisted[kind] }

// OnEmit registers a function Emit runs on every event, after the actor's
// fields are stamped on it and before it is merged or recorded; the owner
// sets what only it knows (an event's draft flag).
func (k *Kernel) OnEmit(f func(t *Tx, e *Event)) { k.stamps = append(k.stamps, f) }

// Write runs f in a write transaction. When f returns nil the deferred work
// runs, the transaction commits and AfterCommit gets the emitted events; when
// f fails or panics the transaction rolls back and nothing is called.
func (k *Kernel) Write(ctx context.Context, actor *Actor, f func(t *Tx) error) error {
	return k.write(ctx, actor, 0, f)
}

// WriteSkip is Write for a caller that wraps it: skip is how many wrapping
// frames lie between the operation and this call, so the trace span is named
// for the operation ("db.write CreateIssue") and not the wrapper.
func (k *Kernel) WriteSkip(ctx context.Context, actor *Actor, skip int, f func(t *Tx) error) error {
	return k.write(ctx, actor, skip, f)
}

func (k *Kernel) write(ctx context.Context, actor *Actor, skip int, f func(t *Tx) error) error {
	if actor != nil && actor.ReadOnly {
		return fmt.Errorf("read-only token: %w", ErrForbidden)
	}
	var span trace.Span
	if tracing.Enabled() {
		ctx, span = tracing.Start(ctx, "db.write "+callerName(3+skip))
		defer span.End()
	}
	queued := time.Now()
	sqlTx, err := k.DB.W.BeginTx(ctx, nil)
	if err != nil {
		if span != nil {
			tracing.Fail(span, "begin")
		}
		return err
	}
	if span != nil {
		span.SetAttributes(attribute.Int64("db.wait_ms", time.Since(queued).Milliseconds()))
	}
	t := &Tx{Tx: sqlTx, Ctx: ctx, Actor: actor, k: k}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()
	if err := f(t); err != nil {
		_ = sqlTx.Rollback()
		if span != nil {
			tracing.Fail(span, errorKind(err))
		}
		return err
	}
	t.RunDeferred()
	if err := sqlTx.Commit(); err != nil {
		if span != nil {
			tracing.Fail(span, "commit")
		}
		return err
	}
	if k.AfterCommit != nil {
		k.AfterCommit(ctx, actor, t.events)
	}
	return nil
}

// callerName is the method skip frames up ("CreateIssue"), for span names.
func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "unknown"
	}
	return operationName(f.Name())
}

// operationName shortens a function's full name to the operation's name:
// "git.jmann.me/…/core.(*Service).CreateIssue.func1" and
// "git.jmann.me/…/email.(*Service).SetEmailSettings" give "CreateIssue" and
// "SetEmailSettings". Any package's type and any pointer receiver is the same:
// "…/users.(*Store).ResetPassword" gives "ResetPassword".
func operationName(full string) string {
	name := full[strings.LastIndex(full, "/")+1:]
	_, name, _ = strings.Cut(name, ".") // the package: core, email, …
	if strings.HasPrefix(name, "(*") {
		if _, rest, ok := strings.Cut(name, ")."); ok {
			name = rest
		}
	}
	name, _, _ = strings.Cut(name, ".")
	return name
}

// errorKind sorts an error into a category safe to put in a trace.
func errorKind(err error) string {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		return "invalid"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrUnauthorized):
		return "forbidden"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "error"
}
