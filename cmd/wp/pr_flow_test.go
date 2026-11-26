package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPRFlowText(t *testing.T) {
	raw := `{"repo":"me/app","days":7,"as_of":"2026-11-26T01:00:00Z","merged":2,"hours":10,
	"opened_to_merged":{"p50_s":18000,"p75_s":21600,"p90_s":25200},
	"groups":[{"group":"converging","hours":6,"share":0.6},{"group":"integrating","hours":4,"share":0.4}],
	"stages":[{"stage":"draft with nothing else holding it (in the merge line)","group":"integrating","hours":4,"share":0.4,"per_pr":{"p50_s":7200,"p75_s":7200,"p90_s":9000}},
	  {"stage":"work to do, no gate holding it, no round sent","group":"converging","hours":1,"share":0.1,"per_pr":{"p50_s":1800,"p75_s":1800,"p90_s":1800},"why":[{"text":"waiting for round N","hours":1}]}],
	"daily":[{"day":"2026-11-25","merged":2,"opened_to_merged_p50_s":18000,"hours":10,"groups":[],"top":[{"stage":"draft with nothing else holding it (in the merge line)","share":0.4}]}],
	"watch":{"heads_per_merge":4.5,"full_runs_per_merge":3,"rounds_per_merge":2.5},
	"still_open":{"count":3,"hours":12,"groups":[{"group":"converging","hours":2,"share":0.2},{"group":"integrating","hours":10,"share":0.8}],"stages":[{"stage":"quiet period","group":"converging","hours":2,"share":0.2}]}}`
	var f prFlow
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	out := f.text()
	for _, want := range []string{
		"2 merged pull requests, open 10 h", "p50 5h0m", "p90 7h0m",
		"converging 6 h (60%), integrating 4 h (40%)",
		"2026-11-25   2 merged  p50 5h0m", "draft with nothing else holding it 40%",
		"  4 h  40%", "120 /  150  integrating",
		"waiting: waiting for round N",
		"4.5 heads, 3.0 full CI runs, 2.5 rounds",
		"Still open: 3 pull requests, 12 h open so far",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPRFlowTextNoneMerged(t *testing.T) {
	var f prFlow
	f.Days = 3
	if out := f.text(); !strings.Contains(out, "No pull requests merged in the span") {
		t.Errorf("%s", out)
	}
}
