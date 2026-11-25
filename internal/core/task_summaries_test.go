package core

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSummaryAsks(t *testing.T) {
	cases := []struct {
		name, msg string
		want      []string
	}{
		{"a waiting section and its bullets", "Shipped #140.\n\n**Waiting on you:**\n- Approve the runner install on runner1\n- Pick a name for the tab\n\nEverything else is done.",
			[]string{"Approve the runner install on runner1", "Pick a name for the tab"}},
		{"the item after the colon", "Done.\n\nWaiting on you: the prod deploy of v0.21 (it needs your SSH key).", []string{"the prod deploy of v0.21 (it needs your SSH key)."}},
		{"a heading", "## What changed\nStuff.\n\n## Decisions for you\n1. Keep the old endpoint for a week?\n", []string{"Keep the old endpoint for a week?"}},
		{"a decision mid-message", "I added the flag. Should I also remove the old one from the docs? The tests pass.", []string{"Should I also remove the old one from the docs?"}},
		{"nothing waits", "Done. Nothing is waiting on you.\n\nWaiting on you: nothing.", nil},
		{"a courtesy", "Merged. Let me know if you see any problems with it.", nil},
		{"code is ignored", "Ran it:\n```\nShould I retry? [y/N]\n```\nAll green.", nil},
		{"plain report", "Fixed the race in the merge queue and added a test. PR #141 is armed.", nil},
		{"the dash form", "Done.\n\nWaiting on you — rotate the forge token before Friday.", []string{"rotate the forge token before Friday."}},
		{"a paragraph under the heading", "Done.\n\nWaiting on you:\nRotate the forge token before Friday.\n\nCI is green.", []string{"Rotate the forge token before Friday."}},
		{"a table", "Waiting on you:\n\n| What | Why |\n|---|---|\n| Rotate the token | it expires |\n", []string{"Rotate the token: it expires"}},
		{"negations", "Waiting on you: no action needed.\n\n## Open questions\n- N/A\n\nWaiting on you: nobody.", nil},
	}
	for _, c := range cases {
		got := summaryAsks(c.msg)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The summary of a finished task is kept, its unfiled asks filed once as
// one auto request; asks a request of the session covers are not; a person
// reads them and marks them read.
func TestTaskSummariesAndUnfiledAsks(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	a := te.issue(te.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude"})
	if _, err := te.s.AgentTake(te.ctx, te.ag, a.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := te.s.CreateRequest(te.ctx, te.ag, RequestInput{Kind: RequestQuestion, Title: "May I install the runner on runner1?", Questions: []Question{{Question: "May I install the runner on runner1?"}}}); err != nil {
		t.Fatal(err)
	}
	stopMsg := func(msg string) {
		t.Helper()
		te.setClock(te.clockNow().Add(time.Minute))
		if _, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", te.cli, "Stop", func(h *HookEvent) { h.LastMessage = msg })}); err != nil {
			t.Fatal(err)
		}
	}
	stopMsg("Working on it.")
	var n int
	if err := te.s.db.R.QueryRow(`SELECT count(*) FROM task_summaries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a summary before the task was done: %d", n)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, a.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	msg := "Shipped " + a.Key + " as #12.\n\nWaiting on you:\n- Install the runner on runner1 (asked as a request)\n- Rotate the forge token before Friday\n"
	stopMsg(msg)
	page, err := te.s.TaskSummaries(te.ctx, te.admin, TaskSummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Summaries) != 1 || page.Unreviewed != 1 {
		t.Fatalf("summaries = %+v", page)
	}
	s := page.Summaries[0]
	if s.Issue == nil || s.Issue.Key != a.Key || !strings.Contains(s.Body, "Shipped") || len(s.Asks) != 1 || !strings.Contains(s.Asks[0], "Rotate the forge token") || s.Request == "" {
		t.Fatalf("summary = %+v", s)
	}
	var title, body, source string
	if err := te.s.db.R.QueryRow(`SELECT title, body, source FROM requests WHERE id = (SELECT request_id FROM task_summaries)`).Scan(&title, &body, &source); err != nil {
		t.Fatal(err)
	}
	if source != requestAuto || !strings.Contains(body, "Rotate the forge token") {
		t.Errorf("linked request %q (%s): %q", title, source, body)
	}
	stopMsg(msg + "\nAlso: CI is green.")
	if err := te.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE source = ?`, requestAuto).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("auto requests = %d, want 1", n)
	}
	if _, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", te.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "thanks, what was #12 again?" })}); err != nil {
		t.Fatal(err)
	}
	stopMsg("It's the merge-queue fix.")
	page, _ = te.s.TaskSummaries(te.ctx, te.admin, TaskSummaryQuery{})
	if !strings.Contains(page.Summaries[0].Body, "CI is green") {
		t.Errorf("a reply to the person replaced the summary: %q", page.Summaries[0].Body)
	}
	if ap, err := te.s.TaskSummaries(te.ctx, te.ag, TaskSummaryQuery{}); err != nil || len(ap.Summaries) != 1 || ap.Unreviewed != 1 {
		t.Errorf("an agent reading the summaries: %+v %v", ap, err)
	}
	job := *te.ag
	job.scope = map[int64]int{}
	if ap, err := te.s.TaskSummaries(te.ctx, &job, TaskSummaryQuery{}); err != nil || len(ap.Summaries) != 0 || ap.Unreviewed != 0 {
		t.Errorf("a job elsewhere: %+v %v", ap, err)
	}
	if _, err := te.s.db.W.Exec(`UPDATE issues SET draft = 1 WHERE id = ?`, s.Issue.ID); err != nil {
		t.Fatal(err)
	}
	if ap, err := te.s.TaskSummaries(te.ctx, te.ag, TaskSummaryQuery{}); err != nil || len(ap.Summaries) != 0 || ap.Unreviewed != 0 {
		t.Errorf("an agent and a draft's summary: %+v %v", ap, err)
	}
	if ap, err := te.s.TaskSummaries(te.ctx, te.admin, TaskSummaryQuery{}); err != nil || len(ap.Summaries) != 1 {
		t.Errorf("the person and a draft's summary: %+v %v", ap, err)
	}
	if _, err := te.s.db.W.Exec(`UPDATE issues SET draft = 0 WHERE id = ?`, s.Issue.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := te.s.ReviewTaskSummaries(te.ctx, te.admin, nil, true, "", false); err != nil || n != 1 {
		t.Fatalf("review all = %d %v", n, err)
	}
	page, _ = te.s.TaskSummaries(te.ctx, te.admin, TaskSummaryQuery{})
	if len(page.Summaries) != 0 || page.Unreviewed != 0 {
		t.Errorf("after review: %+v", page)
	}
	if page, _ = te.s.TaskSummaries(te.ctx, te.admin, TaskSummaryQuery{All: true}); len(page.Summaries) != 1 || page.Summaries[0].ReviewedAt == nil {
		t.Errorf("all = %+v", page.Summaries)
	}
}

// stopWithSummary finishes a task and reports a Stop whose last message is msg.
func stopWithSummary(t *testing.T, te *taskEnv, msg string) {
	t.Helper()
	p := te.project(te.admin, "App", "")
	a := te.issue(te.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude"})
	if _, err := te.s.AgentTake(te.ctx, te.ag, a.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, a.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	te.setClock(te.clockNow().Add(time.Minute))
	if _, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", te.cli, "Stop", func(h *HookEvent) { h.LastMessage = msg })}); err != nil {
		t.Fatal(err)
	}
}

// More asks than a request holds are read whole; the first the request holds
// are filed as one request, and a refusal is logged, not a failed Stop. (A
// Stop's own trailing-question rule can file first, so this calls the filing
// step itself.)
func TestTaskSummaryWithMoreAsksThanARequestHolds(t *testing.T) {
	te := newTaskEnv(t, "")
	logs := &bytes.Buffer{}
	te.s.log = slog.New(slog.NewTextHandler(logs, nil))
	var msg string
	for _, ask := range []string{"The runner install on runner1", "The forge token rotation before Friday", "A name for the new tab", "The production deploy window", "How long audit logs are retained", "A billing contact for the invoices", "The mirror host for backups", "The license key renewal", "The domain registration renewal"} {
		msg += "- " + ask + "\n"
	}
	asks := summaryAsks("Waiting on you:\n" + msg)
	if len(asks) != 9 {
		t.Fatalf("a summary with 9 asks reads %d, want 9", len(asks))
	}
	file := func(asks []string) error {
		return te.s.write(te.ctx, te.ag, func(tx *tx) error {
			x, err := te.s.sessionByID(te.ctx, tx, te.ag, te.session)
			if err != nil {
				return err
			}
			return te.s.fileSummaryAsks(te.ctx, tx, te.ag, x, 0, "WP-1", asks, te.s.nowMS())
		})
	}
	if err := file(asks); err != nil {
		t.Fatalf("filing more asks than a request holds: %v, want the refusal logged", err)
	}
	if !strings.Contains(logs.String(), "summary asks refused") || !strings.Contains(logs.String(), "full=false") {
		t.Fatalf("the refusal isn't logged: %q", logs.String())
	}
	if err := file(asks[:maxRequestQuestions]); err != nil {
		t.Fatalf("filing a summary's asks: %v", err)
	}
	var n, questions int
	if err := te.s.db.R.QueryRow(`SELECT count(*), coalesce(max(json_array_length(json_extract(payload, '$.questions'))), 0) FROM requests WHERE source = ?`, requestAuto).Scan(&n, &questions); err != nil {
		t.Fatal(err)
	}
	if n != 1 || questions != maxRequestQuestions {
		t.Errorf("auto requests = %d with %d questions, want 1 with %d", n, questions, maxRequestQuestions)
	}
}

// A person with the most requests open gets no auto request from a summary,
// and the Stop still succeeds.
func TestTaskSummaryAsksAtTheOpenRequestCap(t *testing.T) {
	te := newTaskEnv(t, "")
	logs := &bytes.Buffer{}
	te.s.log = slog.New(slog.NewTextHandler(logs, nil))
	for i := range maxRequestsOpenIn {
		title := fmt.Sprintf("Open question number %d zq%dx", i, i*7919)
		if _, _, err := te.s.CreateRequest(te.ctx, te.ag, RequestInput{Kind: RequestQuestion, Title: title, Questions: []Question{{Question: title}}}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	stopWithSummary(t, te, "Shipped it.\n\nWaiting on you:\n- Rotate the forge token before Friday\n")
	var n int
	if err := te.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE source = ?`, requestAuto).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("auto requests = %d at the cap, want 0", n)
	}
	var summaries, linked int
	if err := te.s.db.R.QueryRow(`SELECT count(*), count(request_id) FROM task_summaries`).Scan(&summaries, &linked); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || linked != 0 {
		t.Errorf("summaries = %d with %d linked to a request, want 1 with none", summaries, linked)
	}
	if !strings.Contains(logs.String(), "summary asks refused") || !strings.Contains(logs.String(), "full=true") {
		t.Errorf("the open-cap refusal isn't logged: %q", logs.String())
	}
}
