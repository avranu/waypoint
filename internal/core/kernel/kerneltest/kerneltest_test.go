package kerneltest

import (
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

func TestEnv(t *testing.T) {
	e := New(t)
	if got := e.K.Now(); !got.Equal(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("clock starts at %v", got)
	}
	e.Advance(time.Hour)
	if got := e.K.Now().Hour(); got != 13 {
		t.Errorf("after Advance the hour is %d", got)
	}

	admin, sam := e.User("robin", true), e.User("sam", false)
	if !admin.IsAdmin() || sam.IsAdmin() || admin.User.ID == sam.User.ID || sam.User.Email != "sam@example.com" {
		t.Errorf("users: %+v %+v", admin.User, sam.User)
	}
	var n int
	if err := e.K.DB.R.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("admins in the table: %d %v", n, err)
	}

	err := e.K.Write(e.Ctx, admin, func(tx *kernel.Tx) error {
		tx.Emit(&kernel.Event{Kind: "a"})
		tx.Emit(&kernel.Event{Kind: "b"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := e.Events()
	if len(evs) != 2 || evs[0].Kind != "a" || evs[1].Kind != "b" || evs[0].Actor == nil || evs[0].Actor.ID != admin.User.ID {
		t.Errorf("events: %+v", evs)
	}
	if again := e.Events(); len(again) != 0 {
		t.Errorf("events were not cleared: %+v", again)
	}
}
