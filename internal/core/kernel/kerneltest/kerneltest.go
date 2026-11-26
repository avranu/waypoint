// Package kerneltest is a temporary database and a kernel with a fixed clock,
// for a domain's tests. It imports the kernel and nothing above it: not core,
// not a domain.
package kerneltest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/db"
)

// Env is a kernel over a fresh database, on a clock that moves only when the
// test moves it.
type Env struct {
	K   *kernel.Kernel
	Ctx context.Context

	t   *testing.T
	mu  sync.Mutex
	now time.Time
	saw []*kernel.Event
}

// New opens a fresh database in t.TempDir() and a kernel over it. The clock
// starts at 2026-10-05 12:00 UTC.
func New(t *testing.T) *Env {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	e := &Env{t: t, Ctx: ctx, now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	e.K = kernel.New(d, nil)
	e.K.SetClock(e.clock)
	e.K.AfterCommit = func(_ context.Context, _ *kernel.Actor, events []*kernel.Event) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.saw = append(e.saw, events...)
	}
	return e
}

func (e *Env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

// Advance moves the clock forward by d.
func (e *Env) Advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

// User inserts a users row and returns its actor. It writes the row directly:
// the users domain is not a package yet, so there is no service to call.
// The email is name@example.com.
func (e *Env) User(name string, admin bool) *kernel.Actor {
	e.t.Helper()
	now := e.clock()
	email := name + "@example.com"
	res, err := e.K.DB.W.ExecContext(e.Ctx, `INSERT INTO users(username, email, is_admin, created_at, updated_at) VALUES (?,?,?,?,?)`,
		name, email, admin, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		e.t.Fatalf("insert user %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.t.Fatal(err)
	}
	return &kernel.Actor{User: &kernel.User{ID: id, Username: name, Email: email, IsAdmin: admin, Timezone: "UTC", CreatedAt: now}}
}

// Events are the events that committed writes emitted since the last call,
// in order, as AfterCommit got them. No domain's reactions run: the bus is not
// published to.
func (e *Env) Events() []*kernel.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.saw
	e.saw = nil
	return out
}
