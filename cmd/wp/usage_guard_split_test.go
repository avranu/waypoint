package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const guardSplitRaw = `{"who":"pat","days":14,"checks":[{"from":"2026-11-24T03:00:00Z","to":"2026-11-25T03:00:00Z","points":1,"automation":0,"person":1,
	"cost_usd":10,"automation_usd":1,"path":"stored","by_phase":{},"limits":{},
	"split":{"path":"recomputed","parts":[{"source":"cli","machine":"runner1","cost_usd":6},{"source":"cli","machine":"runner2","cost_usd":2},
	{"source":"cli","cost_usd":0.5},{"source":"runner","cost_usd":1}],"parts_usd":9.5,"remainder_usd":0.5},
	"hours":[{"hour":"2026-11-24T03:00:00Z","phase":"under","points":1,"automation":0,"cost_usd":3,"automation_usd":0,
	"split":[{"source":"cli","machine":"runner1","cost_usd":2},{"source":"runner","cost_usd":1}]}]}]}`

func TestGuardTextNamesAllowanceReset(t *testing.T) {
	raw := `{"who":"pat","days":14,"checks":[{"from":"2026-11-24T03:00:00Z","to":"2026-11-25T03:00:00Z","by_phase":{},"limits":{},
		"resets":[{"at":"2026-11-24T16:02:01Z","was_percent":100}],"reset_note":"stored before allowance resets were read"}]}`
	var r guardReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	out := guardReportText(&r, false, "runner1")
	for _, want := range []string{"allowance reset at Nov 24 16:02 UTC (was 100%)", "note: stored before allowance resets were read"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGuardSplitText(t *testing.T) {
	var r guardReport
	if err := json.Unmarshal([]byte(guardSplitRaw), &r); err != nil {
		t.Fatal(err)
	}
	out := guardReportText(&r, true, "runner1")
	for _, want := range []string{
		"list price: $10.00, automation $1.00 (the stored daily check, as kept when it ran)",
		"cli, runner1: $6.00",
		"cli, runner2: $2.00",
		"cli, machine unknown: $0.50",
		"runner jobs: $1.00",
		"remainder: $0.50",
		"this machine (runner1): $6.00 on the server; compare `wp usage where --since 2026-11-24T03:00:00Z --until 2026-11-25T03:00:00Z`",
		"cli, runner1 $2.00 · runner jobs $1.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	r.Checks[0].Split.RemainderUSD = 0
	if strings.Contains(guardReportText(&r, false, "runner2"), "remainder") {
		t.Error("a remainder line with nothing left over")
	}
	if !strings.Contains(guardReportText(&r, false, "runner2"), "this machine (runner2): $2.00") {
		t.Error("runner2's own figure")
	}
}

func TestGuardJSONNamesPath(t *testing.T) {
	// The real command against a server that has no live guard: the output
	// is what cmdUsageGuard builds through guardBothJSON, with this machine's
	// name from machineName().
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/live") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(guardSplitRaw))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	t.Setenv("WAYPOINT_MACHINE", "runner1")
	var buf bytes.Buffer
	c, _ := newCLI(&buf, &bytes.Buffer{})
	if err := cmdUsageGuard(c, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Daily struct {
			Checks []struct {
				Path  string `json:"path"`
				Split struct {
					PartsUSD, RemainderUSD float64
					Parts                  []struct{ Source, Machine string } `json:"parts"`
				} `json:"split"`
				Hours []struct {
					Split []guardPart `json:"split"`
				} `json:"hours"`
			} `json:"checks"`
		} `json:"daily"`
		ThisMachine string `json:"this_machine"`
	}
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	ck := v.Daily.Checks[0]
	if ck.Path != "stored" || len(ck.Split.Parts) != 4 || len(ck.Hours[0].Split) != 2 || v.ThisMachine != "runner1" {
		t.Errorf("json %+v", v)
	}
}
