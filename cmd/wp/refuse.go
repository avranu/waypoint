package main

import (
	"encoding/json"
	"os"
	"time"
)

// hookRefusalFor is the nudge refusals (WP-1411), judged for one PreToolUse
// event: a Grok review that would be cut short (grok-cut-short), a
// pgrep/pkill that matches its own shell (pgrep-self), a worktree beside the
// main checkout (worktree-place) and a sleep on a running background task
// (task-wait) and a command that does nothing but print (noop-poll,
// WP-1443). kind is "" when none applies; only Bash calls are judged.
//
// They are emitted by the guard hook (hookClaudeGuard), not the nudge: Claude
// Code fails open when a hook runs past its timeout, and the nudge's is 2 s
// while the guard's is 10 s.
//
// The state c holds decides whether a no-op poll is refused at all (the
// agent has live children or the session a running background task) and what
// its text says; with c nil only a running background task counts.
func (c *cli) hookRefusalFor(in hookEventInput) (kind, text string) {
	return hookRefusalWith(in, func(cmd string) (string, string, bool) {
		return taskNudge(in.ToolName, cmd, "", c.callerOf(in.SessionID, in.AgentID))
	}, func() string { return c.noopRefusalText(in) })
}

// hookRefusalWith is hookRefusalFor with the task check supplied, so a caller
// that also needs the task note scans the task's output file once (WP-1398).
// task is called only for a foreground Bash call that passed the other checks
// and could be refused.
// noop is called only for a foreground call that does nothing but print; it
// returns the refusal text, or "" when the caller has nothing it could wait
// for, in which case the call is let through.
func hookRefusalWith(in hookEventInput, task func(cmd string) (kind, text string, deny bool), noop func() string) (kind, text string) {
	if in.ToolName != "Bash" {
		return "", ""
	}
	var ti struct {
		Command string  `json:"command"`
		Bg      bool    `json:"run_in_background"`
		Timeout float64 `json:"timeout"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || ti.Command == "" {
		return "", ""
	}
	if text := grokCutShort(ti.Command, ti.Bg, ti.Timeout); text != "" {
		return "grok-cut-short", text
	}
	if text := pgrepSelfNudge(ti.Command); text != "" {
		return "pgrep-self", text
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if text := worktreePlaceNudge(ti.Command, cwd); text != "" {
		return "worktree-place", text
	}
	if !ti.Bg && reSleep.MatchString(ti.Command) {
		if k, text, deny := task(ti.Command); k != "" && deny {
			return k, text
		}
	}
	if !ti.Bg && pureNoop(ti.Command) {
		if text := noop(); text != "" {
			return "noop-poll", text
		}
	}
	return "", ""
}

// countNudge counts one shown nudge or refusal of this kind in the state file.
// It runs after the hook's output is written, so it waits at most wait for the
// lock (hookCountWait): a slow wait would carry the run past its timeout and
// lose the output. A counter lost to a held lock is acceptable; it is a
// statistic.
func (c *cli) countNudge(kind string, wait time.Duration) {
	_ = c.updateStateWithin(wait, func(s *State) {
		if s.Nudges == nil {
			s.Nudges = map[string]*nudgeStat{}
		}
		st := s.Nudges[kind]
		if st == nil {
			st = &nudgeStat{}
			s.Nudges[kind] = st
		}
		st.Shown++
		st.Last = time.Now()
	})
}
