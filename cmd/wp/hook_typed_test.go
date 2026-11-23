package main

import (
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func markUnattended(t *testing.T, id string) {
	t.Helper()
	if err := freshCLI().updateState(func(s *State) {
		if s.Unattended == nil {
			s.Unattended = map[string]time.Time{}
		}
		s.Unattended[id] = time.Now()
	}); err != nil {
		t.Fatal(err)
	}
}

func isUnattended(id string) bool {
	_, ok := freshCLI().state.Unattended[id]
	return ok
}

func pendingOf(kind, text string, id int) string {
	return `[{"id":` + string(rune('0'+id)) + `,"sum":"` + core.PromptSum(text) + `","kind":"` + kind + `"}]`
}

// Under --no-prompts a prompt matched to one of Waypoint's own records leaves
// the unattended mark; an ordinary prompt with no record clears it.
func TestNoPromptsMatchedKindsKeepTheUnattendedMark(t *testing.T) {
	for _, tc := range []struct {
		name, kind, prompt string
		pending            func(string) string
		wantMarked         bool
	}{
		{"review", core.TypedReview, "go over the findings", nil, true},
		{"compact", core.TypedCompact, "squeeze it", nil, true},
		{"keep_warm", core.TypedKeepWarm, "still there?", nil, true},
		{"ordinary, no match", "", "fix the typo", func(string) string { return "[]" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dropEnv(t)
			ds := &dropServer{pending: "[]"}
			if tc.kind != "" {
				ds.pending = pendingOf(tc.kind, tc.prompt, 5)
			}
			ds.start(t)
			markUnattended(t, "u1")
			s.hook(map[string]any{"session_id": "u1", "hook_event_name": "UserPromptSubmit", "prompt": tc.prompt, "cwd": t.TempDir()}, "--no-prompts")
			if got := isUnattended("u1"); got != tc.wantMarked {
				t.Errorf("unattended = %v, want %v", got, tc.wantMarked)
			}
		})
	}
}

// In the default mode the hook asks the server only about exactly /compact and
// /exit, and the server's record decides: a match is Waypoint's, no match is
// the person's, and no answer is Waypoint's.
func TestCompactAndExitAreDecidedByTheServersRecord(t *testing.T) {
	for _, text := range []string{"/compact", "/exit"} {
		kind := core.TypedCompact
		if text == "/exit" {
			kind = core.TypedExit
		}
		for _, tc := range []struct {
			name, pending string
			wantMarked    bool
			wantDrop      int64
		}{
			{"matched", pendingOf(kind, text, 3), true, 0},
			{"answered, no match", "[]", false, 0},
			{"did not answer", "not json", true, 1},
		} {
			t.Run(text+" "+tc.name, func(t *testing.T) {
				s := dropEnv(t)
				ds := &dropServer{pending: tc.pending}
				ds.start(t)
				markUnattended(t, "u2")
				s.hook(map[string]any{"session_id": "u2", "hook_event_name": "UserPromptSubmit", "prompt": text, "cwd": t.TempDir()})
				if got := isUnattended("u2"); got != tc.wantMarked {
					t.Errorf("unattended = %v, want %v", got, tc.wantMarked)
				}
				ds.mu.Lock()
				defer ds.mu.Unlock()
				if len(ds.calls) != 1 {
					t.Fatalf("calls = %v", ds.calls)
				}
				ev := ds.calls[0][0]
				// The event still carries the digest and the text, so the server
				// uses up its own record.
				if ev.PromptSum != core.PromptSum(text) || ev.Prompt != text || ev.TypedID != 0 {
					t.Errorf("event = %+v", ev)
				}
				if got := ev.Drops["typed_lookup/UserPromptSubmit"]; got != tc.wantDrop {
					t.Errorf("lookup drop = %d, want %d (%v)", got, tc.wantDrop, ev.Drops)
				}
			})
		}
	}
}

// With no server configured there is no lookup, and /compact is Waypoint's.
func TestCompactWithNoServerIsWaypoints(t *testing.T) {
	s := dropEnv(t)
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	markUnattended(t, "u3")
	s.hook(map[string]any{"session_id": "u3", "hook_event_name": "UserPromptSubmit", "prompt": "/compact", "cwd": t.TempDir()})
	if !isUnattended("u3") {
		t.Error("a /compact with no server cleared the mark")
	}
}

// Any other prompt, in the default mode, is not looked up.
func TestDefaultModeDoesNotLookUpOrdinaryPrompts(t *testing.T) {
	s := dropEnv(t)
	ds := &dropServer{pending: "[]"}
	ds.start(t)
	s.hook(map[string]any{"session_id": "u4", "hook_event_name": "UserPromptSubmit", "prompt": "hello", "cwd": t.TempDir()})
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.lists != 0 {
		t.Errorf("%d lookups for an ordinary prompt", ds.lists)
	}
}
