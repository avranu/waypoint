package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// holdReads makes the snapshot read wait for the returned release: the only
// database reader is held, so the build's first query queues behind it.
func holdReads(t *testing.T, be *bsEnv) (release func()) {
	t.Helper()
	be.s.db.R.SetMaxOpenConns(1)
	t.Cleanup(func() { be.s.db.R.SetMaxOpenConns(4) })
	conn, err := be.s.db.R.Conn(be.ctx)
	if err != nil {
		t.Fatal(err)
	}
	once := false
	release = func() {
		if !once {
			once = true
			_ = conn.Close()
		}
	}
	t.Cleanup(release)
	return release
}

func (be *bsEnv) buildingNow() bool {
	c := &be.s.ciBoard
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.building != nil
}

func (be *bsEnv) waitBuild(t *testing.T, running bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); be.buildingNow() != running; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("building is still %v", !running)
		}
	}
}

// WP-1851: a caller whose own time ends leaves with its error, and the read
// goes on and fills the snapshot for the next caller.
func TestCISnapshotCallerLeavesBuildFinishes(t *testing.T) {
	be := newBabysitEnv(t)
	c := &be.s.ciBoard
	release := holdReads(t, be)
	ctx, cancel := context.WithCancel(be.ctx)
	errc := make(chan error, 1)
	go func() { _, err := be.s.ciSnapshot(ctx); errc <- err }()
	be.waitBuild(t, true)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want its own cancellation", err)
	}
	c.mu.Lock()
	had := c.snap
	c.mu.Unlock()
	if had != nil || !be.buildingNow() {
		t.Fatalf("snapshot %v before the build finished, building %v", had, be.buildingNow())
	}
	release()
	be.waitBuild(t, false)
	c.mu.Lock()
	filled := c.snap
	c.mu.Unlock()
	if filled == nil {
		t.Fatal("the build ended without filling the snapshot")
	}
	got, err := be.s.ciSnapshot(be.ctx)
	if err != nil || got != filled || be.buildingNow() {
		t.Fatalf("next call: %p (%v), want the filled snapshot %p without a new read", got, err, filled)
	}
}

// WP-1851: callers waiting on a failing read all get its error, none retries,
// and the next call starts a new read that can succeed.
func TestCISnapshotWaitersShareFailure(t *testing.T) {
	be := newBabysitEnv(t)
	c := &be.s.ciBoard
	if _, err := be.s.db.W.ExecContext(be.ctx, `ALTER TABLE server_settings RENAME TO server_settings_away`); err != nil {
		t.Fatal(err)
	}
	renamed := true
	restore := func() {
		if renamed {
			renamed = false
			if _, err := be.s.db.W.ExecContext(context.Background(), `ALTER TABLE server_settings_away RENAME TO server_settings`); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(restore)
	release := holdReads(t, be)
	errc := make(chan error, 2)
	for range 2 {
		go func() { _, err := be.s.ciSnapshot(be.ctx); errc <- err }()
	}
	be.waitBuild(t, true)
	time.Sleep(100 * time.Millisecond) // both are waiting on the one build
	release()
	e1, e2 := <-errc, <-errc
	if e1 == nil || e2 == nil || e1 != e2 {
		t.Fatalf("waiters got %v and %v, want the build's error, the same one", e1, e2)
	}
	be.waitBuild(t, false)
	c.mu.Lock()
	had := c.snap
	c.mu.Unlock()
	if had != nil {
		t.Fatalf("a failed read left a snapshot")
	}
	restore()
	snap, err := be.s.ciSnapshot(be.ctx)
	if err != nil || snap == nil {
		t.Fatalf("the next call after the failure: %v, %v", snap, err)
	}
}
