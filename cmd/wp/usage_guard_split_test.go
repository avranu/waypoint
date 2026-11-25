package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const guardSplitRaw = `{"who":"pat","days":14,"checks":[{"from":"2026-11-24T03:00:00Z","to":"2026-11-25T03:00:00Z","points":1,"automation":0,"person":1,
	"cost_usd":10,"automation_usd":1,"path":"stored","by_phase":{},"limits":{},
	"split":{"path":"recomputed","parts":[{"source":"cli","machine":"chiron","cost_usd":6},{"source":"cli","machine":"legba","cost_usd":2},
	{"source":"cli","cost_usd":0.5},{"source":"runner","cost_usd":1}],"parts_usd":9.5,"remainder_usd":0.5},
	"hours":[{"hour":"2026-11-24T03:00:00Z","phase":"under","points":1,"automation":0,"cost_usd":3,"automation_usd":0,
	"split":[{"source":"cli","machine":"chiron","cost_usd":2},{"source":"runner","cost_usd":1}]}]}]}`

func TestGuardSplitText(t *testing.T) {
	var r guardReport
	if err := json.Unmarshal([]byte(guardSplitRaw), &r); err != nil {
		t.Fatal(err)
	}
	out := guardReportText(&r, true, "chiron")
	for _, want := range []string{
		"list price: $10.00, automation $1.00 (the stored daily check, as kept when it ran)",
		"cli, chiron: $6.00",
		"cli, legba: $2.00",
		"cli, machine unknown: $0.50",
		"runner jobs: $1.00",
		"remainder: $0.50",
		"this machine (chiron): $6.00 on the server; compare `wp usage where --since 2026-11-24T03:00:00Z --until 2026-11-25T03:00:00Z`",
		"cli, chiron $2.00 · runner jobs $1.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	r.Checks[0].Split.RemainderUSD = 0
	if strings.Contains(guardReportText(&r, false, "legba"), "remainder") {
		t.Error("a remainder line with nothing left over")
	}
	if !strings.Contains(guardReportText(&r, false, "legba"), "this machine (legba): $2.00") {
		t.Error("legba's own figure")
	}
}

func TestGuardJSONNamesPath(t *testing.T) {
	var buf bytes.Buffer
	c := &cli{asJSON: true, out: &buf}
	raw := []byte(guardSplitRaw)
	both := []byte(`{"live":null,"daily":` + string(raw) + `,"this_machine":"chiron"}`)
	if err := c.emit(both, func() string { return "" }); err != nil {
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
	if ck.Path != "stored" || len(ck.Split.Parts) != 4 || len(ck.Hours[0].Split) != 2 || v.ThisMachine != "chiron" {
		t.Errorf("json %+v", v)
	}
}
