package kernel

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/db"
)

func newKernel(t *testing.T) *Kernel {
	t.Helper()
	d, err := db.OpenFresh(context.Background(), filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d, nil)
}

func countEvents(t *testing.T, k *Kernel, kind string) int {
	t.Helper()
	var n int
	if err := k.DB.R.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWriteCommitsAndCallsAfterCommitInOrder(t *testing.T) {
	k := newKernel(t)
	var got []string
	var gotActor *Actor
	k.AfterCommit = func(_ context.Context, a *Actor, evs []*Event) {
		gotActor = a
		for _, e := range evs {
			got = append(got, e.Kind)
		}
	}
	actor := SystemActor()
	err := k.Write(context.Background(), actor, func(tx *Tx) error {
		if _, err := tx.ExecContext(tx.Ctx, `CREATE TABLE scratch(n INTEGER)`); err != nil {
			return err
		}
		tx.Emit(&Event{Kind: "a"})
		tx.Emit(&Event{Kind: "b"})
		tx.Emit(&Event{Kind: "c"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("AfterCommit events = %v, want %v", got, want)
	}
	if gotActor != actor {
		t.Errorf("AfterCommit actor = %v, want the writer", gotActor)
	}
	var n int
	if err := k.DB.R.QueryRow(`SELECT COUNT(*) FROM scratch`).Scan(&n); err != nil {
		t.Errorf("the write did not commit: %v", err)
	}
}

func TestWriteErrorRollsBackAndCallsNothing(t *testing.T) {
	k := newKernel(t)
	k.Persist("kept")
	called := false
	k.AfterCommit = func(context.Context, *Actor, []*Event) { called = true }
	boom := errors.New("boom")
	err := k.Write(context.Background(), SystemActor(), func(tx *Tx) error {
		tx.Emit(&Event{Kind: "kept"})
		tx.Defer(func() { called = true })
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if called {
		t.Error("AfterCommit or deferred work ran after a failed write")
	}
	if n := countEvents(t, k, "kept"); n != 0 {
		t.Errorf("a failed write left %d event rows", n)
	}
}

func TestWriteRefusesReadOnlyActor(t *testing.T) {
	k := newKernel(t)
	ran := false
	err := k.Write(context.Background(), &Actor{ReadOnly: true}, func(*Tx) error { ran = true; return nil })
	if !errors.Is(err, ErrForbidden) || ran {
		t.Errorf("read-only write: err %v, ran %v", err, ran)
	}
}

func TestDeferRunsBeforeCommitIncludingWorkItQueues(t *testing.T) {
	k := newKernel(t)
	var order []string
	err := k.Write(context.Background(), SystemActor(), func(tx *Tx) error {
		tx.Defer(func() {
			order = append(order, "first")
			tx.Defer(func() { order = append(order, "queued by first") })
		})
		tx.Defer(func() { order = append(order, "second") })
		order = append(order, "body")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"body", "first", "second", "queued by first"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestEmitPersistsRegisteredKindsOnly(t *testing.T) {
	k := newKernel(t)
	k.Persist("kept")
	if !k.Persisted("kept") || k.Persisted("lost") {
		t.Fatal("Persisted does not report the registered kinds")
	}
	var seen []*Event
	k.AfterCommit = func(_ context.Context, _ *Actor, evs []*Event) { seen = evs }
	err := k.Write(context.Background(), SystemActor(), func(tx *Tx) error {
		tx.Emit(&Event{Kind: "kept", Field: "f", New: "v"})
		tx.Emit(&Event{Kind: "lost"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(t, k, "kept") != 1 || countEvents(t, k, "lost") != 0 {
		t.Errorf("rows: kept %d, lost %d", countEvents(t, k, "kept"), countEvents(t, k, "lost"))
	}
	if len(seen) != 2 || seen[0].ID == 0 || seen[1].ID != 0 {
		t.Errorf("event ids after commit: %+v", seen)
	}
}

func TestOnEmitStampsEveryEvent(t *testing.T) {
	k := newKernel(t)
	k.OnEmit(func(_ *Tx, e *Event) { e.Draft = e.IssueID == 7 })
	var seen []*Event
	k.AfterCommit = func(_ context.Context, _ *Actor, evs []*Event) { seen = evs }
	_ = k.Write(context.Background(), SystemActor(), func(tx *Tx) error {
		tx.Emit(&Event{Kind: "x", IssueID: 7})
		tx.Emit(&Event{Kind: "x", IssueID: 8})
		return nil
	})
	if len(seen) != 2 || !seen[0].Draft || seen[1].Draft {
		t.Errorf("stamps: %+v", seen)
	}
}

func TestPartDropsEventsWritesAndDeferredWorkOfAFailedPart(t *testing.T) {
	k := newKernel(t)
	k.Persist("kept", "dropped")
	var seen []*Event
	k.AfterCommit = func(_ context.Context, _ *Actor, evs []*Event) { seen = evs }
	boom := errors.New("boom")
	var ran []string
	err := k.Write(context.Background(), SystemActor(), func(tx *Tx) error {
		tx.Emit(&Event{Kind: "kept"})
		tx.Defer(func() { ran = append(ran, "before the part") })
		if err := tx.Part(tx.Ctx, func() error {
			tx.Emit(&Event{Kind: "dropped"})
			tx.Defer(func() { ran = append(ran, "in the part") })
			return boom
		}); !errors.Is(err, boom) {
			t.Errorf("part err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "before the part" {
		t.Errorf("deferred work run = %v", ran)
	}
	if len(seen) != 1 || seen[0].Kind != "kept" || countEvents(t, k, "dropped") != 0 || countEvents(t, k, "kept") != 1 {
		t.Errorf("seen %+v, dropped rows %d", seen, countEvents(t, k, "dropped"))
	}
}

type testHint struct{ n int }
type otherHint int

func TestAttachRoundTrip(t *testing.T) {
	e := &Event{Kind: "k"}
	if v, ok := Attached[*testHint](e); ok || v != nil {
		t.Errorf("absent pointer attachment = %v, %v", v, ok)
	}
	if v, ok := Attached[otherHint](e); ok || v != 0 {
		t.Errorf("absent value attachment = %v, %v", v, ok)
	}
	h := &testHint{n: 3}
	Attach(e, h)
	Attach(e, otherHint(9))
	if v, ok := Attached[*testHint](e); !ok || v != h {
		t.Errorf("pointer attachment = %v, %v", v, ok)
	}
	if v, ok := Attached[otherHint](e); !ok || v != 9 {
		t.Errorf("value attachment = %v, %v", v, ok)
	}
	Attach(e, otherHint(10))
	if v, _ := Attached[otherHint](e); v != 10 {
		t.Errorf("attaching a second value of a type kept %v", v)
	}
}

func TestAttachmentsSurviveACopy(t *testing.T) {
	e := &Event{Kind: "k"}
	Attach(e, &testHint{n: 1})
	c := *e
	if v, ok := Attached[*testHint](&c); !ok || v.n != 1 {
		t.Fatalf("copy lost the attachment: %v, %v", v, ok)
	}
	Attach(&c, otherHint(5))
	if _, ok := Attached[otherHint](e); ok {
		t.Error("attaching to the copy reached the original")
	}
}

func TestClockIsInjectable(t *testing.T) {
	k := newKernel(t)
	if d := time.Since(k.Now()); d < 0 || d > time.Minute {
		t.Errorf("default clock is off by %v", d)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	k.SetClock(func() time.Time { return at })
	if !k.Now().Equal(at) || k.NowMS() != at.UnixMilli() {
		t.Errorf("Now %v NowMS %d", k.Now(), k.NowMS())
	}
	var seen []*Event
	k.AfterCommit = func(_ context.Context, _ *Actor, evs []*Event) { seen = evs }
	_ = k.Write(context.Background(), SystemActor(), func(tx *Tx) error { tx.Emit(&Event{Kind: "x"}); return nil })
	if len(seen) != 1 || !seen[0].At.Equal(at) {
		t.Errorf("an emitted event is stamped %v, want %v", seen[0].At, at)
	}
}
