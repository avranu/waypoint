package core

import (
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/meaning"
)

// downMeaning is a meaning service whose breaker is open until retryAt.
type downMeaning struct {
	fakeMeaning
	retryAt time.Time
}

func (d *downMeaning) Health() meaning.Health {
	return meaning.Health{State: "down", RetryAt: &d.retryAt, LastError: "no answer"}
}

// The server's status carries the meaning service's health when its client
// keeps one, and leaves it out when it doesn't.
func TestServerStatusMeaningHealth(t *testing.T) {
	e := newGoalCtxEnv(t)
	e.useMeaning(&fakeMeaning{})
	st, err := e.s.ServerStatus(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Meaning != nil {
		t.Fatalf("a client with no health reported one: %+v", st.Meaning)
	}
	at := e.clockNow().Add(10 * time.Minute)
	e.useMeaning(&downMeaning{retryAt: at})
	if st, err = e.s.ServerStatus(e.ctx, e.admin); err != nil {
		t.Fatal(err)
	}
	if st.Meaning == nil || st.Meaning.State != "down" || st.Meaning.RetryAt == nil || !st.Meaning.RetryAt.Equal(at) || st.Meaning.LastError != "no answer" {
		t.Fatalf("meaning health = %+v", st.Meaning)
	}
}

// While the breaker is open a pass is set for when it next lets a call
// through, so the judgements resume without a goal changing (WP-1602).
func TestMeaningPassSetForBreakerRetry(t *testing.T) {
	e := newGoalCtxEnv(t)
	timer := func() *time.Timer {
		e.s.goals.mu.Lock()
		defer e.s.goals.mu.Unlock()
		return e.s.goals.judgeTimer
	}
	e.useMeaning(&fakeMeaning{})
	e.meaning()
	if timer() != nil {
		t.Fatal("a pass is set though nothing failed and the service has no breaker")
	}
	e.useMeaning(&downMeaning{retryAt: e.clockNow().Add(10 * time.Minute)})
	e.meaning()
	if timer() == nil {
		t.Fatal("no pass set for when the breaker lets a call through")
	}
}
