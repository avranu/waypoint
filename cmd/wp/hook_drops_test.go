package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A fake server for the drop-report tests: it records every call's events,
// and answers with what the test says for that call.
type dropServer struct {
	mu      sync.Mutex
	calls   [][]sessionEvent
	reply   func(call int, evs []sessionEvent) int
	pending string
	bodies  []string
	lists   int
}

func (d *dropServer) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/typed-prompts" {
			d.mu.Lock()
			body := d.pending
			d.lists++
			d.mu.Unlock()
			if body == "" {
				body = "[]"
			}
			_, _ = w.Write([]byte(body))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var in struct{ Events []sessionEvent }
		_ = json.Unmarshal(raw, &in)
		d.mu.Lock()
		d.bodies = append(d.bodies, string(raw))
		d.calls = append(d.calls, in.Events)
		n := len(d.calls) - 1
		d.mu.Unlock()
		status := http.StatusOK
		if d.reply != nil {
			status = d.reply(n, in.Events)
		}
		if status != http.StatusOK {
			http.Error(w, `{"error":"no"}`, status)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
}

// lastDrops is the drops and their id on the last event of call n.
func (d *dropServer) lastDrops(n int) (string, map[string]int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	evs := d.calls[n]
	e := evs[len(evs)-1]
	return e.DropsID, e.Drops
}

func dropEnv(t *testing.T) *sessionEnv {
	t.Helper()
	s := newSessionEnv(t)
	s.asAgent()
	old := processAge
	processAge = func() time.Duration { return 0 }
	t.Cleanup(func() { processAge = old })
	return s
}

func seedDrops(t *testing.T, drops map[string]int64) {
	t.Helper()
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if err := c.updateState(func(s *State) { s.Drops = drops }); err != nil {
		t.Fatal(err)
	}
}

func freshCLI() *cli {
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	return c
}

// Both processes hold the pending counts before either sends: the same id.
func TestDropReportConcurrentProcessesSendTheSameInFlightReport(t *testing.T) {
	dropEnv(t)
	seedDrops(t, map[string]int64{"overflow/Stop": 5})
	ds := &dropServer{reply: func(call int, _ []sessionEvent) int { return http.StatusInternalServerError }}
	ds.start(t)
	a, b := freshCLI(), freshCLI()
	a.sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d1"})
	b.sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d1"})
	id1, d1 := ds.lastDrops(0)
	id2, d2 := ds.lastDrops(1)
	if id1 == "" || id1 != id2 || d1["overflow/Stop"] != 5 || d2["overflow/Stop"] != 5 {
		t.Errorf("ids %q %q, counts %v %v: want one id and the same counts", id1, id2, d1, d2)
	}
}

func TestDropReportFailedSendKeepsTheIDAndSuccessClearsIt(t *testing.T) {
	dropEnv(t)
	seedDrops(t, map[string]int64{"expired/Stop": 2})
	ds := &dropServer{reply: func(call int, _ []sessionEvent) int {
		if call == 0 {
			return http.StatusGatewayTimeout
		}
		return http.StatusOK
	}}
	ds.start(t)
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d2"})
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d2"})
	id1, _ := ds.lastDrops(0)
	id2, d2 := ds.lastDrops(1)
	if id1 == "" || id1 != id2 || d2["expired/Stop"] != 2 {
		t.Errorf("re-send: id %q then %q, counts %v", id1, id2, d2)
	}
	st := freshCLI().state
	if st.DropsOut != nil || len(st.Drops) != 0 {
		t.Errorf("after a successful send the state still holds %+v / %v", st.DropsOut, st.Drops)
	}
}

func TestDropReportSettleAfterAnotherProcessEmptiedTheStateDoesNotPanic(t *testing.T) {
	dropEnv(t)
	seedDrops(t, map[string]int64{"overflow/Stop": 1})
	c := freshCLI()
	rep := c.claimDrops()
	if rep == nil {
		t.Fatal("no report claimed")
	}
	if err := freshCLI().updateState(func(s *State) { s.Drops, s.DropsOut = nil, nil }); err != nil {
		t.Fatal(err)
	}
	c.settleDrops(rep)
	c.settleDrops(&dropReport{ID: "x"})
	c.settleDrops(nil)
	if st := freshCLI().state; st.DropsOut != nil || len(st.Drops) != 0 {
		t.Errorf("state = %+v / %v", st.DropsOut, st.Drops)
	}
}

func TestDropReportCountsAddedInFlightGoInTheNextReport(t *testing.T) {
	dropEnv(t)
	seedDrops(t, map[string]int64{"overflow/Stop": 1})
	ds := &dropServer{reply: func(call int, _ []sessionEvent) int {
		if call == 0 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}}
	ds.start(t)
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d3"})
	freshCLI().countDrop("unqueued", "PostToolUse", 3)
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d3"})
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d3"})
	id1, _ := ds.lastDrops(0)
	id2, d2 := ds.lastDrops(1)
	id3, d3 := ds.lastDrops(2)
	if id2 != id1 || len(d2) != 1 || d2["overflow/Stop"] != 1 {
		t.Errorf("second send: %q %v, want the first report unchanged", id2, d2)
	}
	if id3 == "" || id3 == id1 || d3["unqueued/PostToolUse"] != 3 || len(d3) != 1 {
		t.Errorf("third send: %q %v, want a new report with the new counts", id3, d3)
	}
}

func TestRefusedEventWithAnEmptyOutboxIsCounted(t *testing.T) {
	dropEnv(t)
	ds := &dropServer{reply: func(int, []sessionEvent) int { return http.StatusBadRequest }}
	ds.start(t)
	seedDrops(t, map[string]int64{"overflow/Stop": 1})
	c := freshCLI()
	if r := c.sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d4"}); r != nil {
		t.Fatalf("reply = %+v", r)
	}
	st := freshCLI().state
	if st.Drops["refused/Stop"] != 1 {
		t.Errorf("drops = %v, want refused/Stop counted", st.Drops)
	}
	if st.DropsOut == nil || st.DropsOut.Counts["overflow/Stop"] != 1 {
		t.Errorf("in flight = %+v", st.DropsOut)
	}
}

func TestQueuedEventRefusedOnItsOwnIsCounted(t *testing.T) {
	dropEnv(t)
	var taken []string
	ds := &dropServer{reply: func(_ int, evs []sessionEvent) int {
		for _, e := range evs {
			if e.Event == "BadQ" {
				return http.StatusBadRequest
			}
		}
		for _, e := range evs {
			taken = append(taken, e.Event)
		}
		return http.StatusOK
	}}
	ds.start(t)
	now := time.Now().UnixMilli()
	c := freshCLI()
	c.queueEvent(sessionEvent{Event: "BadQ", At: now - 3, SessionID: "d5"})
	c.queueEvent(sessionEvent{Event: "GoodQ", At: now - 2, SessionID: "d5"})
	freshCLI().sendSessionEvent(sessionEvent{Event: "Good", At: now, SessionID: "d5"})
	if strings.Join(taken, ",") != "GoodQ,Good" {
		t.Errorf("taken = %v", taken)
	}
	_, d := ds.lastDrops(len(ds.calls) - 1)
	st := freshCLI().state
	if d["refused/BadQ"] != 1 || len(st.Outbox) != 0 || st.DropsOut != nil || len(st.Drops) != 0 {
		t.Errorf("reported %v, outbox %d, state %+v / %v", d, len(st.Outbox), st.DropsOut, st.Drops)
	}
}

func TestSessionCommandPromptIsNotQueuedAndIsCounted(t *testing.T) {
	dropEnv(t)
	c := freshCLI()
	c.queueEvent(sessionEvent{Event: "UserPromptSubmit", At: time.Now().UnixMilli(), SessionID: "d6", Prompt: "/wp next"})
	st := freshCLI().state
	if len(st.Outbox) != 0 || st.Drops["unqueued/UserPromptSubmit"] != 1 {
		t.Errorf("outbox %d, drops %v", len(st.Outbox), st.Drops)
	}
}

// WP-1468: what expired in the outbox goes out in the same call.
func TestExpiredOutboxEntriesAreReportedInTheSameSend(t *testing.T) {
	dropEnv(t)
	c := freshCLI()
	c.queueEvent(sessionEvent{Event: "Notification", At: time.Now().Add(-48 * time.Hour).UnixMilli(), SessionID: "d7"})
	ds := &dropServer{}
	ds.start(t)
	freshCLI().sendSessionEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d7"})
	_, d := ds.lastDrops(0)
	if d["expired/Notification"] != 1 {
		t.Errorf("first send carried %v", d)
	}
}

func TestOutboxIsCappedByBytesOldestFirst(t *testing.T) {
	dropEnv(t)
	c := freshCLI()
	big := strings.Repeat("x", 100<<10)
	for i := 0; i < 15; i++ {
		c.queueEvent(sessionEvent{Event: "UserPromptSubmit", At: time.Now().UnixMilli() + int64(i), SessionID: "d8", Prompt: big})
	}
	st := freshCLI().state
	raw, _ := json.Marshal(st.Outbox)
	if len(raw) > outboxMaxBytes {
		t.Errorf("outbox is %d bytes, cap %d", len(raw), outboxMaxBytes)
	}
	if len(st.Outbox) == 0 || len(st.Outbox) >= 15 || st.Drops["overflow/UserPromptSubmit"] != int64(15-len(st.Outbox)) {
		t.Errorf("kept %d, drops %v", len(st.Outbox), st.Drops)
	}
	if last := st.Outbox[len(st.Outbox)-1].Event; last.At < st.Outbox[0].Event.At {
		t.Error("the newest was dropped, not the oldest")
	}
}

// WP-1468: a lock held by another process costs a pre-output state write the
// hook's bounded wait, not 5 s.
func TestPreOutputStateWaitsAreBounded(t *testing.T) {
	dropEnv(t)
	c := freshCLI()
	c.hookLimit = 150 * time.Millisecond
	unlock, err := lockFile(filepath.Join(c.dir, "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var errOut bytes.Buffer
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	start := time.Now()
	c.queueEvent(sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: "d9"})
	c.countDrop("refused", "Stop", 1)
	took := time.Since(start)
	_ = w.Close()
	os.Stderr = old
	_, _ = errOut.ReadFrom(r)
	if took > 2*time.Second {
		t.Errorf("two pre-output waits took %v", took)
	}
	if !strings.Contains(errOut.String(), "lost") {
		t.Errorf("a lost event was not said on stderr: %q", errOut.String())
	}
}

// A prompt the server did not answer is posted again from the outbox: every
// attempt carries the invocation's own key, and the next invocation has its own.
func TestPromptHookKeyIsTheSameOnEveryAttempt(t *testing.T) {
	s := dropEnv(t)
	ds := &dropServer{reply: func(call int, _ []sessionEvent) int {
		if call == 0 {
			return http.StatusGatewayTimeout
		}
		return http.StatusOK
	}}
	ds.start(t)
	s.hook(map[string]any{"session_id": "k1", "hook_event_name": "UserPromptSubmit", "prompt": "first", "cwd": t.TempDir()})
	s.hook(map[string]any{"session_id": "k1", "hook_event_name": "UserPromptSubmit", "prompt": "second", "cwd": t.TempDir()})
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.calls) != 2 || len(ds.calls[0]) != 1 || len(ds.calls[1]) != 2 {
		t.Fatalf("calls = %v", ds.calls)
	}
	first, again, second := ds.calls[0][0], ds.calls[1][0], ds.calls[1][1]
	if first.Key == "" || again.Key != first.Key {
		t.Errorf("the re-post of the first prompt has key %q, the first attempt %q: want the same, not empty", again.Key, first.Key)
	}
	if second.Key == "" || second.Key == first.Key {
		t.Errorf("the next invocation has key %q, the first %q: want its own", second.Key, first.Key)
	}
}

// --no-prompts sends neither the prompt's text nor a digest of it: the hook
// asks which texts Waypoint typed are pending, compares them here, and sends
// the id of the one it matched, or none (WP-1370).
func TestNoPromptsBodiesCarryNeitherTextNorDigest(t *testing.T) {
	const secret = "please rotate the key sk-live-4242 now"
	const typed = "Waypoint typed this one, with sk-live-9999 in it"
	for _, tc := range []struct {
		name, prompt string
		wantID       int64
	}{
		{"no match", secret, 0},
		{"a match", typed, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dropEnv(t)
			ds := &dropServer{pending: `[{"id":7,"sum":"` + core.PromptSum(typed) + `","kind":"review"},{"id":8,"sum":"` + core.PromptSum("another") + `","kind":"compact"}]`}
			ds.start(t)
			s.hook(map[string]any{"session_id": "np1", "hook_event_name": "UserPromptSubmit", "prompt": tc.prompt, "cwd": t.TempDir()}, "--no-prompts")
			ds.mu.Lock()
			defer ds.mu.Unlock()
			if len(ds.calls) != 1 || len(ds.calls[0]) != 1 {
				t.Fatalf("calls = %v", ds.calls)
			}
			ev := ds.calls[0][0]
			if ev.TypedID != tc.wantID {
				t.Errorf("typed_id = %d, want %d", ev.TypedID, tc.wantID)
			}
			for _, body := range ds.bodies {
				for _, leak := range []string{"sk-live", tc.prompt, core.PromptSum(tc.prompt), core.PromptSum(secret), core.PromptSum(typed)} {
					if strings.Contains(body, leak) {
						t.Errorf("the body carries %q:\n%s", leak, body)
					}
				}
				if strings.Contains(body, "prompt_sum") {
					t.Errorf("the body carries a prompt_sum:\n%s", body)
				}
			}
		})
	}
}

// With no server answer, a --no-prompts prompt is read as the person's: no id,
// no text. Only Waypoint's fixed texts (the whole keep-warm ping, exactly
// /compact or /exit) are sent, as constants, so the server can match its own
// record by digest.
func TestNoPromptsSendsNoIDWhenTheServerDoesntList(t *testing.T) {
	for _, tc := range []struct{ name, prompt, want string }{
		{"ordinary", "anything", ""},
		{"a mark and more", "<task-notification>x", "<task-notification>"},
		{"keep-warm ping", core.KeepWarmPing(), core.KeepWarmPing()},
		{"compact", "/compact", "/compact"},
		{"exit", " /exit ", "/exit"},
		{"compact with words", "/compact please keep the notes", ""},
		{"ping with words", core.KeepWarmPing() + " and my password", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dropEnv(t)
			ds := &dropServer{pending: `not json`}
			ds.start(t)
			s.hook(map[string]any{"session_id": "np2", "hook_event_name": "UserPromptSubmit", "prompt": tc.prompt, "cwd": t.TempDir()}, "--no-prompts")
			ds.mu.Lock()
			defer ds.mu.Unlock()
			if len(ds.calls) != 1 || ds.calls[0][0].TypedID != 0 || ds.calls[0][0].PromptSum != "" || ds.calls[0][0].Prompt != tc.want {
				t.Fatalf("calls = %+v, want prompt %q", ds.calls, tc.want)
			}
		})
	}
}

// WP-1562 under the digest rule: a text Waypoint typed arrives wrapped as
// Claude Code 2.1.328 wraps a paste (the id on the closing tag too), and the
// --no-prompts hook unwraps it, compares its digest with the pending records
// here, and sends only the matched id: neither text nor digest. A person's
// paste with no record sends no id.
func TestNoPromptsMatchesAWrappedTypedPrompt(t *testing.T) {
	wrap := func(p string) string {
		return "\n\n<pasted_content id=\"d29a\">\n" + p + "\n</pasted_content id=\"d29a\">\n"
	}
	review := "Waypoint autopilot review of WP-1: go, with sk-live-1234 in it"
	pending := `[{"id":7,"sum":"` + core.PromptSum(review) + `","kind":"review"},{"id":8,"sum":"` + core.PromptSum("/compact") + `","kind":"compact"},{"id":9,"sum":"` + core.PromptSum(core.KeepWarmPing()) + `","kind":"keep_warm"}]`
	for _, tc := range []struct {
		name, prompt string
		wantID       int64
	}{
		{"review", wrap(review), 7},
		{"compact", wrap("/compact"), 8},
		{"keep-warm", wrap(core.KeepWarmPing()), 9},
		{"a person's paste", wrap("my private notes sk-live-5678"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dropEnv(t)
			ds := &dropServer{pending: pending}
			ds.start(t)
			s.hook(map[string]any{"session_id": "np2", "hook_event_name": "UserPromptSubmit", "prompt": tc.prompt, "cwd": t.TempDir()}, "--no-prompts")
			ds.mu.Lock()
			defer ds.mu.Unlock()
			if len(ds.calls) != 1 || len(ds.calls[0]) != 1 {
				t.Fatalf("calls = %v", ds.calls)
			}
			if got := ds.calls[0][0].TypedID; got != tc.wantID {
				t.Errorf("typed_id = %d, want %d", got, tc.wantID)
			}
			for _, body := range ds.bodies {
				for _, leak := range []string{"sk-live", "private notes", "prompt_sum", core.PromptSum(review), core.PromptSum(core.UnwrapPasted(tc.prompt))} {
					if strings.Contains(body, leak) {
						t.Errorf("the body carries %q:\n%s", leak, body)
					}
				}
			}
		})
	}
}
