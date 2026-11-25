package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A compact prep prints three terms; only those are told to the server as
// shown, under the task's project, and a person at a terminal records none.
func TestPrepRecordsOnlyThePrintedTermsAsShown(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/knowledge/shown" {
			var in struct {
				Via   string      `json:"via"`
				Items []shownItem `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var refs []string
			for _, it := range in.Items {
				refs = append(refs, it.Type+":"+it.Ref+"@"+it.Project)
			}
			bodies = append(bodies, in.Via+" "+strings.Join(refs, ","))
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	var n prepNotes
	if err := json.Unmarshal([]byte(`{"terms": [
		{"term": "A", "definition": "a", "why": "w"}, {"term": "B", "definition": "b", "why": "w"},
		{"term": "C", "definition": "c", "why": "w"}, {"term": "D", "definition": "d", "why": "w"}, {"term": "E", "definition": "e", "why": "w"}]}`), &n); err != nil {
		t.Fatal(err)
	}
	p := &prepView{State: "ready", Notes: &n}

	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-code")
	out, _ := c.renderPrepShown("WP-12", p, nil, 4000, true)
	if strings.Contains(out, "D: d") || !strings.Contains(out, "C: c") {
		t.Fatalf("compact prep printed the wrong terms:\n%s", out)
	}
	if len(bodies) != 1 || bodies[0] != "prep glossary:A@WP,glossary:B@WP,glossary:C@WP" {
		t.Errorf("recorded %q, want the three printed terms", bodies)
	}
	bodies = nil
	c.renderPrepShown("WP-12", p, nil, 4000, false)
	if len(bodies) != 1 || !strings.HasSuffix(bodies[0], "glossary:E@WP") {
		t.Errorf("full prep recorded %q, want all five", bodies)
	}
	bodies = nil
	t.Setenv("WAYPOINT_AGENT", "none")
	c.renderPrepShown("WP-12", p, nil, 4000, true)
	if len(bodies) != 0 {
		t.Errorf("a person at a terminal recorded %q", bodies)
	}
}
