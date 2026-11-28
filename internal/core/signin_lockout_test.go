package core

import (
	"errors"
	"testing"
)

// A session stored before addresses were written canonically still spares
// its address: the stored spelling and the request's are compared alike.
func TestSignedInFromMatchesAnySpelling(t *testing.T) {
	e := newEnv(t)
	id := e.admin.User.ID
	for _, ip := range []string{"::ffff:203.0.113.9", "2001:DB8::7"} {
		if _, err := e.s.users.CreateSession(e.ctx, id, "test", ip); err != nil {
			t.Fatal(err)
		}
	}
	for ip, want := range map[string]bool{"203.0.113.9": true, "::ffff:203.0.113.9": true, "2001:db8::7": true, "203.0.113.10": false, "": false} {
		got, err := e.s.SignedInFrom(e.ctx, id, ip)
		if err != nil || got != want {
			t.Errorf("SignedInFrom(%q) = %v, %v; want %v", ip, got, err, want)
		}
	}
	if got, _ := e.s.SignedInFrom(e.ctx, id+1000, "203.0.113.9"); got {
		t.Error("another account's address spared")
	}
}

// Only a wrong login or password is ErrUnauthorized; a database failure is
// the server's, so a lockout doesn't count it as a guess.
func TestLoginServerFaultIsNotUnauthorized(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.s.Login(e.ctx, "nobody-here", "x", "test", "192.0.2.1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown login: %v", err)
	}
	e.s.db.R.Close() // where the account is looked up
	if _, _, err := e.s.Login(e.ctx, e.admin.User.Username, "x", "test", "192.0.2.1"); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("closed read database: %v, want a server error", err)
	}
}
