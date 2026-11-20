package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// wp pr show says what a pull request left open at merge: others' comments
// counted and not acted on, and the audit made from the review's findings.
func TestPRShowLeftOpenAtMerge(t *testing.T) {
	var d prDetail
	raw := `{"repo":"me/app","number":7,"title":"Export","state":"merged","url":"u","author":"sam","base_ref":"main","head_ref":"x","issues":[],
		"left_open":{"others":3,"audits":[{"id":12,"findings":2,"open":2,"fix_task":"APP-9"},{"id":13,"findings":1,"open":1}]}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	out := d.text()
	for _, want := range []string{
		"Left open at merge: 3 comments by others, not acted on",
		"Left open at merge: 2 review findings, audit #12 (2 still open; fix task APP-9)",
		"Left open at merge: 1 review finding, audit #13 (1 still open; wp audit show 13)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	d.LeftOpen = nil
	if strings.Contains(d.text(), "Left open") {
		t.Error("a pull request with nothing left open says so")
	}
}
