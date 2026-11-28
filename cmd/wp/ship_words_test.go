package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oldServer is the server before -m and --summary went with a ship: its
// strict decoder (internal/api decodeBody) refuses the keys it doesn't know.
func oldServer(t *testing.T, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = append(*seen, body)
		for _, k := range []string{"message", "summary"} {
			if _, ok := body[k]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad request: json: unknown field \"` + k + `\""}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"held":"Pushed. Nothing more to run: end your turn."}`))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	return ts
}

// A: -m and --summary are sent only when given (given empty is "").
func TestShipWordsSentOnlyWhenGiven(t *testing.T) {
	for _, tc := range []struct {
		f    prOpenFlags
		want map[string]any
	}{
		{prOpenFlags{}, map[string]any{}},
		{prOpenFlags{summary: " did it ", messageGiven: true}, map[string]any{"message": "did it"}},
		{prOpenFlags{messageGiven: true, shipSummaryGiven: true}, map[string]any{"message": "", "summary": ""}},
		{prOpenFlags{shipSummary: "Done.", shipSummaryGiven: true}, map[string]any{"summary": "Done."}},
	} {
		req := map[string]any{}
		tc.f.addShipWords(req)
		got, _ := json.Marshal(req)
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Errorf("%+v sent %s, want %s", tc.f, got, want)
		}
	}
}

// A: against a server that doesn't know the keys, the ship goes through
// once more without them and says so in one line.
func TestPostOpenRetriesWithoutWordsOnOldServer(t *testing.T) {
	var seen []map[string]any
	oldServer(t, &seen)
	var out, errb bytes.Buffer
	c, _ := newCLI(&out, &errb)
	raw, old, err := c.postOpen("WP", map[string]any{"head": "b", "message": "m", "summary": "s"})
	if err != nil || !old || !strings.Contains(string(raw), "held") {
		t.Fatalf("postOpen = %s, %v, %v", raw, old, err)
	}
	if len(seen) != 2 {
		t.Fatalf("the server was asked %d times", len(seen))
	}
	if _, ok := seen[1]["message"]; ok {
		t.Errorf("the retry still sent message: %v", seen[1])
	}
	if _, ok := seen[1]["summary"]; ok {
		t.Errorf("the retry still sent summary: %v", seen[1])
	}
	if n := strings.Count(strings.TrimSpace(errb.String()), "\n"); n != 0 || !strings.Contains(errb.String(), "older") {
		t.Errorf("the note should be one line about the old server: %q", errb.String())
	}
}

// A: a ship that gave neither is not retried, and sends neither.
func TestPostOpenNoWordsAgainstOldServer(t *testing.T) {
	var seen []map[string]any
	oldServer(t, &seen)
	var out, errb bytes.Buffer
	c, _ := newCLI(&out, &errb)
	_, old, err := c.postOpen("WP", map[string]any{"head": "b"})
	if err != nil || old || len(seen) != 1 || errb.Len() != 0 {
		t.Fatalf("old = %v, err = %v, asked %d times, said %q", old, err, len(seen), errb.String())
	}
}

// A: any other 400 is the ship's own error, not a reason to retry.
func TestPostOpenOtherBadRequestIsNotRetried(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"the pull request needs a title"}`))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if _, _, err := c.postOpen("WP", map[string]any{"message": "m"}); err == nil || n != 1 {
		t.Fatalf("err = %v after %d asks", err, n)
	}
}

// D: a ship needs the agent when the server says its held text or notice
// does, and when the server says nothing (one from before the facts).
func TestShipNeedsAgentFollowsTheServersFacts(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name string
		o    openedPR
		want bool
	}{
		{"held, asks nothing", openedPR{Held: "Pushed. Nothing more to run: end your turn.", HeldNeedsAgent: &no, NoticeNeedsAgent: &no}, false},
		{"held and a notice, both news", openedPR{Held: "h", Notice: "n", HeldNeedsAgent: &no, NoticeNeedsAgent: &no}, false},
		{"held asks", openedPR{Held: "h", HeldNeedsAgent: &yes, NoticeNeedsAgent: &no}, true},
		{"a notice to act on", openedPR{Held: "h", Notice: "n", HeldNeedsAgent: &no, NoticeNeedsAgent: &yes}, true},
		{"an old server says nothing about a held ship", openedPR{Held: "h"}, true},
		{"an old server says nothing about a notice", openedPR{Notice: "n"}, true},
	} {
		o := tc.o
		if got := shipNeedsAgent(&o, "", nil, shipTurn{clean: true}) != ""; got != tc.want {
			t.Errorf("%s: needs the agent = %v", tc.name, got)
		}
	}
}

// The tree decides alike wherever a ship may end the turn: only a git status
// that said the tracked files are clean lets it end.
func TestShipNeedsAgentWantsACleanTree(t *testing.T) {
	no := false
	o := openedPR{HeldNeedsAgent: &no, NoticeNeedsAgent: &no}
	for _, tc := range []struct {
		name string
		t    shipTurn
		want bool
	}{
		{"clean", shipTurn{clean: true}, false},
		{"uncommitted", shipTurn{dirty: true}, true},
		{"git status failed", shipTurn{}, true},
		{"notes put off", shipTurn{clean: true, putOff: "p"}, true},
	} {
		if got := shipNeedsAgent(&o, "", nil, tc.t) != ""; got != tc.want {
			t.Errorf("%s: needs the agent = %v", tc.name, got)
		}
	}
}
