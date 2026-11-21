package main

import (
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Autopilot's prompt is worded by people (WP-308), so it needn't start with
// autopilotPromptMark, which is how the server tells autopilot's own prompts
// from a person's. The runner notes each prompt it pastes, by the session it
// goes to; that session's UserPromptSubmit hook, on this machine, finds it
// there and sends it marked.

// pastedPrompt is one prompt autopilot pasted: the task, and a digest of
// its words (so the state file never holds the task's text).
type pastedPrompt struct {
	Key string    `json:"key"`
	Sum string    `json:"sum"`
	At  time.Time `json:"at"`
}

// pastedKeep: a note older than this is dropped (the paste failed, or the
// session never sent it).
const pastedKeep = time.Hour

// promptSum digests a prompt's words (core.PromptSum, the server's own), so
// whitespace the terminal changed in the paste doesn't matter.
func promptSum(p string) string { return core.PromptSum(p) }

// notePastedPrompt records that autopilot is about to paste prompt, for
// task key, into session.
func (c *cli) notePastedPrompt(session, key, prompt string) {
	_ = c.updateState(func(s *State) {
		if s.Pasted == nil {
			s.Pasted = map[string]*pastedPrompt{}
		}
		for id, p := range s.Pasted {
			if time.Since(p.At) > pastedKeep {
				delete(s.Pasted, id)
			}
		}
		s.Pasted[session] = &pastedPrompt{Key: key, Sum: promptSum(prompt), At: time.Now()}
	})
}

// isPastedPrompt: prompt, sent in session, is the one autopilot pasted there.
// Unlike pastedPromptKey it leaves the note for it.
func (c *cli) isPastedPrompt(session, prompt string) bool {
	if session == "" {
		return false
	}
	st := c.state
	if c.dir != "" {
		st = c.loadState()
	}
	p := st.Pasted[session]
	return p != nil && p.Sum == promptSum(prompt) && time.Since(p.At) <= pastedKeep
}

// pastedPromptKey: the task autopilot pasted prompt for, when prompt, sent
// in session, is the one it pasted there. The note is used up.
func (c *cli) pastedPromptKey(session, prompt string) (string, bool) {
	if session == "" {
		return "", false
	}
	st := c.state // a test's bare cli keeps it in memory
	if c.dir != "" {
		st = c.loadState() // no lock or write for the prompts that aren't autopilot's
	}
	if st.Pasted[session] == nil {
		return "", false
	}
	var key string
	sum := promptSum(prompt)
	_ = c.updateState(func(s *State) {
		if p := s.Pasted[session]; p != nil && p.Sum == sum && time.Since(p.At) <= pastedKeep {
			key = p.Key
			delete(s.Pasted, session)
		}
	})
	return key, key != ""
}
