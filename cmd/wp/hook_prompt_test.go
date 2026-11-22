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

// What a Stop says still runs carries a person's text: a cron's prompt and a
// task's description are redacted like a prompt, and --no-prompts leaves the
// cron's prompt out (WP-1470).
func TestStopPendingRedactsAndHonoursNoPrompts(t *testing.T) {
	const tok = "wpt_supersecrettoken123"
	raw := hookEventInput{
		BackgroundTasks: json.RawMessage(`[{"type":"shell","description":"curl with ` + tok + ` here"}]`),
		SessionCrons:    json.RawMessage(`[{"schedule":"*/5 * * * *","prompt":"check using ` + tok + ` and password=hunter2hunter2"}]`),
	}
	p := stopPending(raw, []string{tok}, false)
	if strings.Contains(p.Tasks[0].Description, tok) || strings.Contains(p.Crons[0].Prompt, tok) || strings.Contains(p.Crons[0].Prompt, "hunter2hunter2") || !strings.Contains(p.Crons[0].Prompt, "check using") {
		t.Errorf("pending = %+v", p)
	}
	p = stopPending(raw, []string{tok}, true)
	if p.Crons[0].Prompt != "" || p.Crons[0].Schedule == "" || strings.Contains(p.Tasks[0].Description, tok) {
		t.Errorf("under --no-prompts: %+v", p)
	}
}
