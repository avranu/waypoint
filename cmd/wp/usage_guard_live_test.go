package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGuardLiveText(t *testing.T) {
	raw := `{"who":"pat","settings":{},"policy":{"churn_usd":45,"session_usd":120,"hours":12,"idle_hours":8,"model_usd":3,"models":["claude-sonnet-5-5"],
		"burst_usd":0,"pace":true,"from":{"churn_usd":"usual","session_usd":"set","burst_usd":"set"}},
		"baseline":{"at":"2026-11-15T08:00:00Z","days":28,"sessions":120,"session_p50":14,"session_p90":60,"hours":300,"hour_p95":30,"hour_max":80},
		"hold":{"rule":"burst","reason":"automation spent $200 in the last hour.","since":"2026-11-15T09:00:00Z","until":"2026-11-15T10:00:00Z"},
		"hour_usd":200,"trips":[{"at":"2026-11-15T09:03:00Z","session":12,"title":"WP-1 · fix","rule":"churn","action":"stopped","cost":34}],"days":7}`
	var l guardLiveReport
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	out := guardLiveText(&l, time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC))
	for _, want := range []string{
		"Live quota guard (pat): on. Automation spent $200.00 in the last hour.",
		"HOLD (burst) since Nov 15 09:00 until Nov 15 10:00 UTC: automation spent $200 in the last hour. `wp guard lift` ends it.",
		"churn    $45   usual",
		"session  $120  set",
		"hours    12h   default",
		"spend on a model priced above claude-sonnet-5-5",
		"burst    off   set",
		"Usual usage (28 days to Nov 15 08:00 UTC): 120 automation sessions, p50 $14.00, p90 $60.00; 300 hours, p95 $30.00, max $80.00.",
		"Nov 15 09:03  stopped  churn  session 12 WP-1 · fix  $34.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGuardSetBody(t *testing.T) {
	in, err := guardSetBody(map[string]bool{"churn": true, "burst": true}, map[string]float64{"churn": 40, "burst": 0, "session": 99}, "sonnet,haiku", "off", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(in)
	if got := string(b); got != `{"merge":true,"settings":{"burst_usd":0,"churn_usd":40,"models":["sonnet","haiku"],"pace":false}}` {
		t.Errorf("body %s", got)
	}
	in, _ = guardSetBody(nil, nil, "", "", "all", true, false)
	if b, _ := json.Marshal(in); string(b) != `{"clear":["all"],"merge":true,"on":false,"settings":{}}` {
		t.Errorf("off body %s", b)
	}
	bad := []struct {
		given map[string]bool
		nums  map[string]float64
		pace  string
		off   bool
		on    bool
	}{
		{},
		{off: true, on: true},
		{pace: "maybe"},
		{given: map[string]bool{"hours": true}, nums: map[string]float64{"hours": -1}},
	}
	for i, c := range bad {
		if _, err := guardSetBody(c.given, c.nums, "", c.pace, "", c.off, c.on); err == nil {
			t.Errorf("bad set %d was taken", i)
		}
	}
}
