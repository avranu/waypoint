package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// drillLLM flags the texts whose titles it holds, as a drifted model might
// miss the rest; fail makes every call fail.
type drillLLM struct {
	mu   sync.Mutex
	hold []string
	fail bool
}

func (f *drillLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (f *drillLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, "", errors.New("rate limited")
	}
	drift := strings.HasPrefix(prompt, "Check pull request")
	for _, t := range f.hold {
		if strings.Contains(prompt, t) {
			if drift {
				return json.RawMessage(`{"kind": "contradicts", "goal": 1, "reason": "works against the goal"}`), "", nil
			}
			return json.RawMessage(`{"flags": ["other_hosts"], "reason": "acts on another machine"}`), "", nil
		}
	}
	if drift {
		return json.RawMessage(`{"kind": "none", "goal": 0, "reason": ""}`), "", nil
	}
	return json.RawMessage(`{"flags": [], "reason": ""}`), "", nil
}

func TestSafetyDrill(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	sam := e.user("sam", false)
	notices := func() []string {
		t.Helper()
		var out []string
		rows, err := e.s.db.R.Query(`SELECT title FROM notifications WHERE kind = ? ORDER BY id`, NotifySafety)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	status := func() *SafetyDrills {
		t.Helper()
		st, err := e.s.SafetyDrillStatus(e.ctx, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Without a screen model the rules alone decide: nothing to drill.
	if _, err := e.s.RunSafetyDrill(e.ctx, e.admin); !IsInvalid(err) {
		t.Fatalf("drill without a model: %v", err)
	}
	e.s.maybeSafetyDrill(e.ctx)
	e.s.bg.Wait()
	if st := status(); st.Configured || st.Last != nil || st.NextAt != nil {
		t.Fatalf("no model: %+v", st)
	}
	if _, err := e.s.SafetyDrillStatus(e.ctx, sam); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin status: %v", err)
	}

	// A model that holds every bad text passes; the verdicts stay out of
	// the real screen's table.
	llm := &drillLLM{}
	for _, c := range drillCases {
		if c.want == SafetyHold {
			llm.hold = append(llm.hold, c.title)
		}
	}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	e.s.maybeSafetyDrill(e.ctx)
	e.s.bg.Wait()
	st := status()
	if st.Last == nil || st.Last.Passed != len(drillCases) || st.Last.Failed != 0 || st.Last.StartedBy != nil || len(st.Last.Cases) != len(drillCases) {
		t.Fatalf("passing drill: %+v", st.Last)
	}
	if len(st.Models) != 1 || st.Models[0].Passed != len(drillCases) || st.Models[0].Total != len(drillCases) || !strings.Contains(st.Models[0].Model, "big-pickle") {
		t.Fatalf("models: %+v", st.Models)
	}
	if st.NextAt == nil || !st.NextAt.Equal(e.clockNow().Add(drillEvery)) {
		t.Errorf("next at %v", st.NextAt)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM task_safety`).Scan(&n); err != nil || n != 0 {
		t.Errorf("task_safety rows: %d, %v", n, err)
	}
	if got := notices(); len(got) != 0 {
		t.Errorf("passing drill notified: %v", got)
	}
	// Not due again within the week.
	e.setClock(e.clockNow().Add(6 * 24 * time.Hour))
	e.s.maybeSafetyDrill(e.ctx)
	e.s.bg.Wait()
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM safety_drills`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("drills within the week: %d, %v", n, err)
	}

	// A week on, the model misses the subtle one: the admins hear of it.
	e.setClock(e.clockNow().Add(24 * time.Hour))
	llm.hold = slices.DeleteFunc(llm.hold, func(s string) bool { return s == "Fix the flaky e2e test for big boards" })
	e.s.maybeSafetyDrill(e.ctx)
	e.s.bg.Wait()
	st = status()
	if st.Last.Failed != 1 || st.Last.Passed != len(drillCases)-1 {
		t.Fatalf("failing drill: %+v", st.Last)
	}
	for _, c := range st.Last.Cases {
		if c.Pass != (c.Name != "prose_only") || c.Label == "" {
			t.Errorf("case %+v", c)
		}
	}
	if st.Models[0].Passed != 2*len(drillCases)-1 || st.Models[0].Total != 2*len(drillCases) {
		t.Errorf("pass rate: %+v", st.Models[0])
	}
	if got := notices(); len(got) != 1 || got[0] != "The safety screen let a bad task through" {
		t.Fatalf("notices: %v", got)
	}

	// An admin runs one by hand; a model that fails throughout is no
	// failure, and the drill is tried again the next day.
	if _, err := e.s.RunSafetyDrill(e.ctx, sam); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin run: %v", err)
	}
	llm.fail = true
	if _, err := e.s.RunSafetyDrill(e.ctx, e.admin); err != nil {
		t.Fatal(err)
	}
	e.s.bg.Wait()
	st = status()
	if st.Last.Errors != len(drillCases) || st.Last.Failed != 0 || st.Last.StartedBy == nil || st.Last.Cases[0].Error == "" {
		t.Fatalf("failed models: %+v", st.Last)
	}
	if !st.NextAt.Equal(e.clockNow().Add(drillRetry)) {
		t.Errorf("next after an outage: %v", st.NextAt)
	}
	if got := notices(); len(got) != 1 {
		t.Errorf("an outage notified: %v", got)
	}
}
