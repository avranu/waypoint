package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func promptEvent(t *testing.T, ds *dropServer, prompt string, noPrompts bool) sessionEvent {
	t.Helper()
	b, _ := json.Marshal(in("p1", "UserPromptSubmit", t.TempDir(), map[string]any{"prompt": prompt}))
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	ds.mu.Lock()
	n := len(ds.calls)
	ds.mu.Unlock()
	hookClaudeEvent(c, bytes.NewReader(b), &out, noPrompts)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for _, call := range ds.calls[n:] {
		for _, e := range call {
			if e.Event == "UserPromptSubmit" {
				return e
			}
		}
	}
	t.Fatalf("no UserPromptSubmit reached the server (%d calls)", len(ds.calls))
	return sessionEvent{}
}

// The prompt's text gets the redaction other text leaving the machine gets
// (WP-1470): this machine's own token never goes up in a prompt.
func TestHookPromptIsRedactedBeforeItIsSent(t *testing.T) {
	dropEnv(t)
	ds := &dropServer{}
	ds.start(t)
	tok := freshCLI().token()
	if len(tok) < 8 {
		t.Skip("no token to redact")
	}
	ev := promptEvent(t, ds, "use "+tok+" and password=hunter2hunter2 please", false)
	if strings.Contains(ev.Prompt, tok) || strings.Contains(ev.Prompt, "hunter2hunter2") || !strings.Contains(ev.Prompt, "please") {
		t.Errorf("prompt sent as %q", ev.Prompt)
	}
}

// Under --no-prompts a typed prompt is sent with no text, and says so.
func TestHookNoPromptsSaysThePromptWasWithheld(t *testing.T) {
	dropEnv(t)
	ds := &dropServer{}
	ds.start(t)
	ev := promptEvent(t, ds, "my secret plan", true)
	if ev.Prompt != "" || !ev.PromptWithheld {
		t.Errorf("--no-prompts sent prompt %q withheld=%v", ev.Prompt, ev.PromptWithheld)
	}
	ev = promptEvent(t, ds, "my plan", false)
	if ev.PromptWithheld || ev.Prompt != "my plan" {
		t.Errorf("without --no-prompts: %q withheld=%v", ev.Prompt, ev.PromptWithheld)
	}
}
