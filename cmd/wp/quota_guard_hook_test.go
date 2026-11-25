package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var guardT0 = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

func guardPolicyForTest() *guardPolicyCopy {
	p := guardDefaults
	p.Models = []string{"claude-sonnet-5-5", "claude-haiku-5-5"}
	return &p
}

var guardTestPrices = []burnPrice{
	{Model: "claude-opus-5-5", Output: 25},
	{Model: "claude-sonnet-5-5", Output: 15},
	{Model: "claude-haiku-5-5", Output: 5},
}

// look is one tool call at minute m with the tree at cost.
func guardAt(m int, cost float64) guardLook {
	return guardLook{now: guardT0.Add(time.Duration(m) * time.Minute), cost: cost, auto: true, model: "claude-sonnet-5-5",
		policy: guardPolicyForTest(), prices: guardTestPrices, task: "WP-1"}
}

func TestGuardChurnTellsThenStops(t *testing.T) {
	g := &guardLocal{LastCost: 5}
	if text, stop, trip := g.decide(guardAt(0, 5)); text != "" || stop != "" || trip != nil {
		t.Fatalf("first look spoke: %q %q %+v", text, stop, trip)
	}
	if text, _, _ := g.decide(guardAt(10, 20)); text != "" {
		t.Fatalf("$15 without a change told: %s", text)
	}
	text, stop, trip := g.decide(guardAt(20, 26))
	if stop != "" || !strings.Contains(text, "spent $21 since its last change") || !strings.Contains(text, "wp release WP-1") || trip == nil || trip.Rule != "churn" || trip.Action != "told" {
		t.Fatalf("at the line: %q %q %+v", text, stop, trip)
	}
	if text, _, trip := g.decide(guardAt(21, 27)); text != "" || trip != nil {
		t.Fatalf("told twice: %q", text)
	}
	l := guardAt(22, 28)
	l.progress = true
	g.decide(l)
	if text, stop, _ := g.decide(guardAt(30, 50)); !strings.Contains(text, "spent $22 since its last change") || stop != "" {
		t.Fatalf("$22 after a change is told again, not stopped: %q %q", text, stop)
	}
	text, stop, trip = g.decide(guardAt(31, 60))
	if text != "" || !strings.HasPrefix(stop, "Quota guard stopped this session: $32 since its last change") || !strings.HasSuffix(stop, guardStopTail) || trip.Action != "stopped" || g.Stopped != "churn" {
		t.Fatalf("past 1.5 times: %q %q %+v", text, stop, trip)
	}
	if _, stop, trip := g.decide(guardAt(32, 61)); !strings.Contains(stop, "stays stopped") || trip != nil {
		t.Fatalf("after the stop: %q %+v", stop, trip)
	}
}

func TestGuardSessionHoursAndModel(t *testing.T) {
	g := &guardLocal{}
	g.decide(guardAt(0, 0))
	for i, cost := range []float64{20, 40, 61} {
		l := guardAt(i+1, cost)
		l.progress = true
		text, stop, _ := g.decide(l)
		if cost == 61 && (!strings.Contains(text, "$61 on its task") || stop != "") {
			t.Fatalf("session line: %q %q", text, stop)
		}
	}
	l := guardAt(5, 91)
	l.progress = true
	if _, stop, trip := g.decide(l); !strings.Contains(stop, "$91 on its task") || trip.Rule != "session" {
		t.Fatalf("session stop: %q", stop)
	}

	g = &guardLocal{}
	g.decide(guardAt(0, 0))
	if text, _, _ := g.decide(guardAt(12*60, 1)); !strings.Contains(text, "for 12 hours") {
		t.Fatalf("hours line: %q", text)
	}
	if _, stop, _ := g.decide(guardAt(18*60, 2)); !strings.Contains(stop, "drove it for 18 hours") {
		t.Fatalf("hours stop: %q", stop)
	}

	g = &guardLocal{}
	g.decide(guardAt(0, 0))
	for i := 1; i <= 4; i++ {
		l := guardAt(i, float64(i)*10)
		l.model = "claude-haiku-5-5"
		l.progress = true
		if text, stop, _ := g.decide(l); strings.Contains(text+stop, "runs on") || strings.Contains(stop, "doesn't usually use") {
			t.Fatalf("a cheaper model: %q %q", text, stop)
		}
	}
	l = guardAt(5, 44)
	l.model, l.progress = "claude-opus-5-5", true
	if text, _, _ := g.decide(l); !strings.Contains(text, "runs on claude-opus-5-5") || !strings.Contains(text, "spent $4 on it") {
		t.Fatalf("model line: %q", text)
	}
	l = guardAt(6, 50)
	l.model, l.progress = "claude-opus-5-5", true
	if _, stop, trip := g.decide(l); !strings.Contains(stop, "$10 on claude-opus-5-5") || trip.Rule != "model" {
		t.Fatalf("model stop: %q", stop)
	}

	if guardUnusualModel("claude-opus-5-5", nil, guardTestPrices) {
		t.Error("no usual models, yet unusual")
	}
	if guardUnusualModel("claude-sonnet-5-5[1m]", []string{"claude-sonnet-5-5"}, guardTestPrices) {
		t.Error("a usual model's variant counted as unusual")
	}
}

func TestGuardHoldTellsThenStops(t *testing.T) {
	g := &guardLocal{}
	g.decide(guardAt(0, 0))
	hold := &guardHoldCopy{Rule: "burst", Reason: "automation spent $200 in the last hour, above the $60 line.", Since: guardT0, Until: guardT0.Add(time.Hour)}
	l := guardAt(1, 1)
	l.hold = hold
	text, stop, trip := g.decide(l)
	if stop != "" || !strings.Contains(text, "held back: automation spent $200 in the last hour, above the $60 line. Put down") || trip.Rule != "hold" {
		t.Fatalf("hold tell: %q %q", text, stop)
	}
	l = guardAt(5, 2)
	l.hold = hold
	if text, stop, _ := g.decide(l); text != "" || stop != "" {
		t.Fatalf("within 10 minutes and $2: %q %q", text, stop)
	}
	l = guardAt(11, 2.5)
	l.hold = hold
	if _, stop, trip := g.decide(l); !strings.Contains(stop, "automation is held back (automation spent $200") || trip.Action != "stopped" {
		t.Fatalf("hold stop: %q", stop)
	}

	g = &guardLocal{}
	g.decide(guardAt(0, 0))
	l = guardAt(1, 1)
	l.hold = hold
	g.decide(l)
	l = guardAt(2, 3.5)
	l.hold = hold
	if _, stop, _ := g.decide(l); stop == "" {
		t.Fatal("$2.50 after the tell didn't stop it")
	}
}

func TestGuardIdleAndPerson(t *testing.T) {
	g := &guardLocal{}
	person := func(m int, cost float64) guardLook {
		l := guardAt(m, cost)
		l.auto = false
		return l
	}
	g.decide(person(0, 0))
	if text, stop, _ := g.decide(person(60, 200)); text != "" || stop != "" {
		t.Fatalf("an hour of heavy use by a person: %q %q", text, stop)
	}
	g = &guardLocal{}
	g.decide(person(0, 0))
	if text, _, _ := g.decide(person(9*60, 70)); !strings.Contains(text, "nobody has typed in this session for 9 hours") {
		t.Fatalf("idle line: %q", text)
	}
	if _, stop, trip := g.decide(person(9*60+5, 95)); !strings.Contains(stop, "$95 in the 9 hours since you last typed") || trip.Rule != "idle" {
		t.Fatalf("idle stop: %q", stop)
	}

	g = &guardLocal{Person: true}
	if text, stop, _ := g.decide(guardAt(0, 0)); text != "" || stop != "" {
		t.Fatal(text, stop)
	}
	if text, stop, _ := g.decide(guardAt(60, 500)); text != "" || stop != "" {
		t.Fatalf("a person's prompt didn't take the session back: %q %q", text, stop)
	}
}

func TestGuardPrompt(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	old := burnNow
	burnNow = func() time.Time { return guardT0 }
	defer func() { burnNow = old }()
	c.withBurnState("s1", func(st *burnState) bool {
		st.Guard = &guardLocal{Stopped: "hours", AutoAt: guardT0.Add(-20 * time.Hour), Told: map[string]float64{"hours": 0}}
		return true
	})
	if c.guardStopped("s1") != "hours" {
		t.Fatal("stop not kept")
	}
	if r := c.guardPrompt("s1", true); !strings.Contains(r, "takes no prompts from automation") {
		t.Fatalf("automation prompted a stopped session: %q", r)
	}
	if r := c.guardPrompt("s1", false); r != "" || c.guardStopped("s1") != "" {
		t.Fatalf("a person's prompt didn't lift the stop: %q", r)
	}
	if r := c.guardPrompt("s1", true); r != "" {
		t.Fatalf("automation after the person: %q", r)
	}
	var st burnState
	if err := readJSON(c.burnPath("s1"), &st); err != nil || st.Guard.Person || !st.Guard.AutoAt.Equal(guardT0) || st.Guard.Told != nil {
		t.Fatalf("automation's prompt didn't start the counts: %+v", st.Guard)
	}
	burnNow = func() time.Time { return guardT0.Add(3 * time.Hour) }
	if r := c.guardPrompt("s1", true); r != "" {
		t.Fatal(r)
	}
	if err := readJSON(c.burnPath("s1"), &st); err != nil || !st.Guard.AutoAt.Equal(guardT0) {
		t.Fatalf("a second automation prompt restarted the counts: %+v", st.Guard)
	}

	stop := func() {
		c.withBurnState("s2", func(st *burnState) bool {
			st.Guard = &guardLocal{Stopped: "hours", AutoAt: guardT0.Add(-20 * time.Hour)}
			return true
		})
	}
	stop()
	c.saveGuard(json.RawMessage(`{"policy":{"churn_usd":20,"session_usd":60,"hours":24,"idle_hours":8,"model_usd":3,"burst_usd":60,"pace":true}}`))
	if c.guardStopped("s2") != "" {
		t.Error("raised line: still stopped")
	}
	if r := c.guardPrompt("s2", true); r != "" {
		t.Errorf("raised line: automation refused: %q", r)
	}
	stop()
	c.saveGuard(json.RawMessage(`{"off":true}`))
	if c.guardStopped("s2") != "" {
		t.Error("guard off: still stopped")
	}
	if r := c.guardPrompt("s2", true); r != "" {
		t.Errorf("guard off: automation refused: %q", r)
	}
	g := &guardLocal{Stopped: "model", StopModel: "claude-opus-5-5", ModelCost: 20}
	pol := guardDefaults
	if !g.stopStands(&pol, nil, guardT0, 0) {
		t.Error("model stop lifted with the line unchanged")
	}
	pol.Models = []string{"claude-sonnet-5-5", "claude-opus-5-5"}
	if g.stopStands(&pol, nil, guardT0, 0) {
		t.Error("model stop stands after the person allowed the model")
	}
	if (&guardLocal{Stopped: "hold"}).stopStands(&pol, nil, guardT0, 0) {
		t.Error("hold stop stands with no hold")
	}

	t.Setenv("WAYPOINT_SESSION", "")
	t.Setenv("WP_TASK", "")
	t.Setenv("WAYPOINT_FOLLOWUP", "")
	t.Setenv("WAYPOINT_UNATTENDED", "1")
	if personCanType() {
		t.Error("a runner job counted as typed into")
	}
	t.Setenv("WAYPOINT_SESSION", "auto-1")
	if !personCanType() {
		t.Error("an autopilot tmux session counted as untyped")
	}
	t.Setenv("WAYPOINT_SESSION", "")
	t.Setenv("WAYPOINT_UNATTENDED", "")
	t.Setenv("WP_TASK", "WP-12")
	if !personCanType() {
		t.Error("a person's wp start session counted as untyped")
	}
	t.Setenv("WAYPOINT_FOLLOWUP", "7")
	if personCanType() {
		t.Error("a runner follow-up counted as typed into")
	}
}

func TestGuardCopy(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	now := time.Now()
	if p, h := c.guardNow(now); p == nil || p.ChurnUSD != 20 || h != nil {
		t.Fatalf("no copy: %+v %+v", p, h)
	}
	c.saveGuard(json.RawMessage(`null`))
	if c.readPause().Guard != nil {
		t.Fatal("null saved")
	}
	c.saveGuard(json.RawMessage(`{"policy":{"churn_usd":45,"session_usd":120,"hours":12,"idle_hours":8,"model_usd":3,"burst_usd":90,"pace":true},
		"hold":{"rule":"burst","reason":"r","since":"` + now.Add(-time.Minute).Format(time.RFC3339) + `","until":"` + now.Add(time.Hour).Format(time.RFC3339) + `"}}`))
	p, h := c.guardNow(now)
	if p == nil || p.ChurnUSD != 45 || h == nil || h.Rule != "burst" {
		t.Fatalf("copy: %+v %+v", p, h)
	}
	if p, h := c.guardNow(now.Add(pauseFresh + time.Minute)); p.ChurnUSD != 45 || h != nil {
		t.Fatalf("stale: %+v %+v", p, h)
	}
	c.saveGuard(json.RawMessage(`{"off":true}`))
	if p, _ := c.guardNow(now); p != nil {
		t.Fatal("off kept a policy")
	}
}

func TestGuardPulse(t *testing.T) {
	var got map[string]any
	pulses := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/sessions/pulse" && r.Method == "POST" && pulses:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			_, _ = w.Write([]byte(`{"pause":null,"guard":{"policy":{"churn_usd":33,"session_usd":60,"hours":12,"idle_hours":8,"model_usd":3,"burst_usd":60,"pace":true}}}`))
		case r.URL.Path == "/api/v1/sessions/pause" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"pause":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	c := &cli{dir: t.TempDir()}
	c.writeGuardLive("s1", guardLive{Cost: 12.5, Auto: true, Model: "claude-sonnet-5-5", At: time.Now()})
	c.queueGuardTrip(guardTrip{Session: "s1", Rule: "churn", Action: "told", Cost: 12.5, At: time.Now()})

	hookClaudePause(c, strings.NewReader(`{}`), false)
	sessions, _ := got["sessions"].([]any)
	trips, _ := got["trips"].([]any)
	if len(sessions) != 1 || len(trips) != 1 {
		t.Fatalf("pulse sent %v", got)
	}
	if s := sessions[0].(map[string]any); s["session"] != "s1" || s["cost"] != 12.5 || s["auto"] != true {
		t.Fatalf("session %v", s)
	}
	if p, _ := c.guardNow(time.Now()); p.ChurnUSD != 33 {
		t.Fatalf("guard not kept: %+v", p)
	}
	if tr, _ := c.takeGuardTrips(); len(tr) != 0 {
		t.Fatalf("trips sent twice: %v", tr)
	}

	pulses = false
	c.queueGuardTrip(guardTrip{Session: "s1", Rule: "churn", Action: "stopped", Cost: 40, At: time.Now()})
	hookClaudePause(c, nil, true)
	if tr, _ := c.takeGuardTrips(); len(tr) != 1 {
		t.Fatalf("a failed pulse lost its trips: %v", tr)
	}
}
