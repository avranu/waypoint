package kernel

import (
	"context"
	"database/sql"
	"errors"
)

// Tx is a write transaction: the database transaction, the acting user and
// the context of the call, with the events it emitted and the work queued to
// run before it commits.
type Tx struct {
	*sql.Tx
	Ctx   context.Context
	Actor *Actor

	k       *Kernel
	events  []*Event
	pending []func()
}

// Events are the events emitted so far, in order. The slice is the Tx's own:
// do not append to it.
func (t *Tx) Events() []*Event { return t.events }

// Defer queues f to run before the transaction commits, after the function
// that is writing returns, or when RunDeferred is called. f returns no error:
// it logs its failures and the transaction carries on.
func (t *Tx) Defer(f func()) { t.pending = append(t.pending, f) }

// RunDeferred runs the queued work, including work that queues more.
func (t *Tx) RunDeferred() {
	for len(t.pending) > 0 {
		f := t.pending[0]
		t.pending = t.pending[1:]
		f()
	}
}

// Part runs f as a part of the transaction that is undone whole when f
// fails: its writes roll back to a savepoint, and its events and the work it
// deferred are dropped, while the rest of the transaction goes on. For a side
// job a caller logs and moves past, so a half of it doesn't commit without
// the other.
func (t *Tx) Part(ctx context.Context, f func() error) error {
	n, queued := len(t.events), len(t.pending)
	if _, err := t.ExecContext(ctx, `SAVEPOINT part`); err != nil {
		return err
	}
	if err := f(); err != nil {
		t.events = t.events[:n]
		t.pending = t.pending[:min(queued, len(t.pending))]
		if _, rerr := t.ExecContext(ctx, `ROLLBACK TO part`); rerr != nil {
			return errors.Join(err, rerr)
		}
		_, rerr := t.ExecContext(ctx, `RELEASE part`)
		return errors.Join(err, rerr)
	}
	_, err := t.ExecContext(ctx, `RELEASE part`)
	return err
}
