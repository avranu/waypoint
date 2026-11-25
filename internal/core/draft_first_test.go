package core

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// EditPRTitle makes the shipping fake edit titles, as the forge does: a
// "WIP: " title is a draft.
func (g *shipForge) EditPRTitle(ctx context.Context, f *Forge, repo string, number int, title string) error {
	g.sm.Lock()
	g.titleEdits = append(g.titleEdits, title)
	g.sm.Unlock()
	g.mu.Lock()
	if pr := g.prs[mk(f, repo, number)]; pr != nil {
		pr.Title = title
		pr.State = "open"
		if draftTitle(title) {
			pr.State = "draft"
		}
	}
	g.mu.Unlock()
	if g.afterTitleEdit != nil {
		g.afterTitleEdit(title)
	}
	return nil
}

func (g *shipForge) edits() []string {
	g.sm.Lock()
	defer g.sm.Unlock()
	return append([]string(nil), g.titleEdits...)
}

func TestStripWIP(t *testing.T) {
	for in, want := range map[string]string{"WIP: Add x": "Add x", "wip:Add x": "Add x", "[WIP] Add x": "Add x", "Add x": "Add x", "WIP: WIP: x": "x", "Add WIP: x": "Add WIP: x"} {
		if got := stripWIP(in); got != want {
			t.Errorf("stripWIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFullRunReasons(t *testing.T) {
	at := time.Now()
	const b, e, codeql = "ci / b (pull_request)", "ci / e (pull_request)", "forge-checks/codeql"
	pr := &PullRequest{WipAuto: true, State: "open", HeadSHA: "abc1234", readySHA: "abc1234", ReadyAt: &at, readyStatusID: 10, readyChecks: []string{b, e, codeql}}
	ran := func(name string, id int64) PRCheck {
		return PRCheck{Name: name, State: CheckSuccess, StatusID: id, Description: "Successful in 3m"}
	}
	skipped := PRCheck{Name: e, State: CheckSuccess, StatusID: 12, Description: "Has been skipped"}
	cases := []struct {
		name   string
		checks []PRCheck
		want   string
	}{
		{"draft tier only", []PRCheck{ran(b, 9), ran(e, 8)}, "0 of 2"},
		{"one reported", []PRCheck{ran(b, 11), ran(e, 8)}, "1 of 2"},
		{"always-skipped check, newer than ready", []PRCheck{ran(b, 11), skipped}, ""},
		{"docs-only full run: lint ran, tests skipped", []PRCheck{ran(b, 11), skipped}, ""},
		{"no-op run: everything skipped after ready blocks", []PRCheck{{Name: b, State: CheckSuccess, StatusID: 11, Description: "Has been skipped"}, skipped}, "2 of 2"},
		{"body edit during the full run: real pending outranks", []PRCheck{ran(b, 11), {Name: e, State: CheckPending, StatusID: 13}}, "1 of 2"},
		{"skipped before ready and again", []PRCheck{ran(b, 11), {Name: e, State: CheckSuccess, StatusID: 6, Description: "Has been skipped"}}, ""},
		{"an external check posts nothing for an edit", []PRCheck{ran(b, 11), ran(e, 12), ran(codeql, 7)}, ""},
		{"one missing", []PRCheck{ran(b, 11)}, "1 of 2"},
		{"pending", []PRCheck{ran(b, 11), {Name: e, State: CheckPending, StatusID: 12}}, "1 of 2"},
		{"full run", []PRCheck{ran(b, 11), ran(e, 12)}, ""},
	}
	for _, c := range cases {
		got := fullRunReasons(pr, c.checks)
		if c.want == "" && len(got) != 0 || c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want) || !isFullRunReason(got[0])) {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	other := *pr
	other.readySHA = "def5678"
	if got := fullRunReasons(&other, nil); len(got) != 0 {
		t.Errorf("another head: %v", got)
	}
	other = *pr
	other.readyChecks = []string{codeql}
	if got := fullRunReasons(&other, []PRCheck{ran(codeql, 7)}); len(got) != 0 {
		t.Errorf("only checks an edit doesn't run: %v", got)
	}
	other = *pr
	other.readySkipped = []string{e}
	if got := fullRunReasons(&other, []PRCheck{ran(b, 11), ran(e, 8)}); len(got) != 0 {
		t.Errorf("draft-only job skipped by the full run: %v", got)
	}
	if got := fullRunReasons(&other, []PRCheck{ran(b, 9), ran(e, 8)}); len(got) != 1 {
		t.Errorf("only skips and draft statuses: %v", got)
	}
	// A person's: held only when their own edit marked it at this head
	// (markReadyByEdit, WP-1808).
	other = *pr
	other.WipAuto = false
	if got := fullRunReasons(&other, []PRCheck{ran(b, 9), ran(e, 8)}); len(got) != 1 {
		t.Errorf("a person's, marked by their edit: %v", got)
	}
	other.readySHA = "def5678"
	if got := fullRunReasons(&other, nil); len(got) != 0 {
		t.Errorf("a person's, not marked at this head: %v", got)
	}
}

// stage is the pull request's stage as the page shows it.
func (be *bsEnv) stage() string {
	be.t.Helper()
	d, err := be.s.GetPullRequest(be.ctx, be.admin, be.pr(7).ID)
	if err != nil || d.Shipping == nil {
		be.t.Fatalf("detail: %v", err)
	}
	return d.Shipping.Stage
}

func (be *bsEnv) draftChecks(sha string, backend, e2e CommitCheck) {
	backend.Name, e2e.Name = "ci / backend (pull_request)", "ci / e2e (pull_request)"
	be.sg.setChecks(sha, backend, e2e)
	be.poll()
	be.s.Wait()
}

// A draft whose fast tier is green is marked ready; it merges only after
// a full run, whatever the draft tier's skipped "successes" say. A failed
// full run's round keeps it ready at the front, so the fix runs the full
// tier at once; when a round's push fails it too, the next round puts it
// back to draft.
func TestDraftFirstLifecycle(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if pr.State != "draft" || pr.Title != "Add export" {
		t.Fatalf("opened: state %q title %q", pr.State, pr.Title)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)

	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	pr = be.pr(7)
	if pr.State != "open" || pr.ReadyAt == nil || pr.readyStatusID != 6 || pr.Title != "Add export" || !sameSHA(pr.readySHA, head) {
		t.Fatalf("not marked ready: %+v", pr)
	}
	if e := be.sg.edits(); len(e) != 1 || e[0] != "Add export" {
		t.Fatalf("title edits %v", e)
	}
	rd, _ := be.s.readiness(be.ctx, pr)
	if rd.Ready || !strings.Contains(strings.Join(rd.Reasons, ";"), "full run hasn't finished since it was marked ready") {
		t.Fatalf("ready on the draft tier's statuses: %+v", rd)
	}
	if st := be.stage(); st != "ready (full run pending)" {
		t.Errorf("stage %q", st)
	}

	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 10, Description: "Failing after 20m"})
	be.rerunFails()
	pr = be.pr(7)
	if b := be.babysit(); b.Round != 1 || pr.State != "open" || pr.ReadyAt == nil || pr.QueuedAt == nil {
		t.Fatalf("after the failed full run: round %d state %q ready_at %v queued_at %v; babysit %+v ci %q", b.Round, pr.State, pr.ReadyAt, pr.QueuedAt, b, pr.CIState)
	}
	if e := be.sg.edits(); len(e) != 1 {
		t.Fatalf("title edits %v", e)
	}

	if b := be.babysit(); b.JobID != 0 {
		be.finish(b.JobID, true)
	}
	p.Title = "Add export"
	p.HeadSHA = "8888888aaaa"
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
	be.draftChecks("8888888aaaa", CommitCheck{State: CheckSuccess, ID: 15, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 16, Description: "Failing after 20m"})
	pr = be.pr(7)
	if b := be.babysit(); b.Round != 2 || pr.State != "draft" || pr.ReadyAt != nil || pr.QueuedAt != nil {
		t.Fatalf("after the round's push failed: round %d state %q ready_at %v queued_at %v; ci %q", b.Round, pr.State, pr.ReadyAt, pr.QueuedAt, pr.CIState)
	}
	if e := be.sg.edits(); len(e) != 2 || e[1] != "WIP: Add export" {
		t.Fatalf("title edits %v", e)
	}

	if b := be.babysit(); b.JobID != 0 {
		be.finish(b.JobID, true)
	}
	p.Title = "WIP: Add export"
	p.HeadSHA = "8888888bbbb"
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
	be.draftChecks("8888888bbbb", CommitCheck{State: CheckSuccess, ID: 20, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 21, Description: "Has been skipped"})
	pr = be.pr(7)
	if pr.State != "open" || pr.ReadyAt == nil || pr.readyStatusID != 21 {
		t.Fatalf("not ready again: %+v", pr)
	}

	be.draftChecks("8888888bbbb", CommitCheck{State: CheckSuccess, ID: 22, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 23, Description: "Successful in 24m"})
	if len(be.sg.merges) != 1 {
		r, _ := be.s.readiness(be.ctx, be.pr(7))
		t.Fatalf("merges %d; readiness %v", len(be.sg.merges), r.Reasons)
	}
}

// A round that pushes nothing after a passed full run (say its review
// findings were all dismissed) puts the pull request back to draft and then
// ready again at the same head. The workflow's run for that edit skips its
// checks, since the head passed its full run (WP-353), so readiness takes
// the checks it has instead of waiting for newer ones. At a head whose full
// run failed, it waits for a new run as before.
func TestReadyAgainAtAPassedHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		e2e    CommitCheck
		passed bool
	}{
		{"passed", CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 20m"}, true},
		{"failed", CommitCheck{State: CheckFailed, ID: 10, Description: "Failing after 20m"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
			const head = "7777777aaaa"
			p := bsPR
			p.Title = "WIP: Add export"
			be.pullHook("opened", p)
			be.s.Wait()
			be.drain()
			pr := be.pr(7)
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
				t.Fatal(err)
			}
			backend := func(c CommitCheck) CommitCheck { c.Name = "ci / backend (pull_request)"; return c }
			e2e := func(c CommitCheck) CommitCheck { c.Name = "ci / e2e (pull_request)"; return c }
			be.sg.setChecks(head, backend(CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}), e2e(CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"}))
			if err := be.s.markReady(be.ctx, be.pr(7)); err != nil {
				t.Fatal(err)
			}
			if pr = be.pr(7); pr.readyStatusID != 6 {
				t.Fatalf("first ready: %+v", pr)
			}
			be.sg.setChecks(head, backend(CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}), e2e(tc.e2e))
			if err := be.s.markDraft(be.ctx, be.pr(7)); err != nil {
				t.Fatal(err)
			}
			if pr = be.pr(7); pr.State != "draft" || pr.ReadyAt != nil {
				t.Fatalf("back to draft: %+v", pr)
			}
			be.sg.setChecks(head, backend(CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}), e2e(tc.e2e))
			if err := be.s.markReady(be.ctx, be.pr(7)); err != nil {
				t.Fatal(err)
			}
			pr = be.pr(7)
			checks, err := be.s.prChecks(be.ctx, pr)
			if err != nil {
				t.Fatal(err)
			}
			waits := len(fullRunReasons(pr, checks)) > 0
			if tc.passed && (waits || pr.readyStatusID != 0) {
				t.Fatalf("waits for a full run its head already passed: status id %d, reasons %v", pr.readyStatusID, fullRunReasons(pr, checks))
			}
			if !tc.passed && (!waits || pr.readyStatusID != 10) {
				t.Fatalf("takes a failed full run as passed: status id %d, reasons %v", pr.readyStatusID, fullRunReasons(pr, checks))
			}
		})
	}
}

// Putting a pull request back to draft makes Forgejo run its workflow for
// the edit, every job skipped, and those statuses can land before the round
// is handed out. The round's work packet still has the failed full run.
func TestRoundPacketBeforeBackToDraft(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	if pr = be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("not marked ready: %+v", pr)
	}

	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 8, Description: "Failing after 20m"})
	be.rerunFails()
	be.finish(be.babysit().JobID, true)
	const head2 = "7777777bbbb"
	p.Title, p.HeadSHA = "Add export", head2
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()

	be.sg.logs["ci / e2e (pull_request)"] = "--- FAIL: TestExport (0.01s)\n"
	be.sg.afterTitleEdit = func(title string) {
		if draftTitle(title) {
			be.sg.setChecks(head2,
				CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess, ID: 11, Description: "Has been skipped"},
				CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess, ID: 12, Description: "Has been skipped"})
			if err := be.hook(be.f, "status", map[string]any{"sha": head2, "repository": map[string]any{"full_name": "me/app"}}); err != nil {
				t.Error(err)
			}
		}
	}
	be.runnerOnline()
	be.draftChecks(head2, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 10, Description: "Failing after 20m"})
	if e := be.sg.edits(); len(e) != 2 || e[1] != "WIP: Add export" {
		t.Fatalf("title edits %v", e)
	}
	jobs := be.jobs()
	if len(jobs) != 2 {
		t.Fatalf("round jobs %v", jobs)
	}
	if in := be.jobInput(jobs[len(jobs)-1]); !strings.Contains(in.Prompt, "### ci / e2e (pull_request)") || !strings.Contains(in.Prompt, "--- FAIL: TestExport") {
		t.Fatalf("the round's packet lost the failed check:\n%s", in.Prompt)
	}
}

// The forge's webhook for the edit that marks a draft ready can be recorded
// before markReady's own write: the pull request is open by then, and it
// still waits for its full run (#94 merged on its draft tier's checks).
func TestMarkReadyAfterTheEditsWebhook(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.sg.afterTitleEdit = func(title string) {
		if !draftTitle(title) {
			q := p
			q.Title, q.State = title, "open"
			be.pullHook("edited", q)
		}
	}

	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	pr = be.pr(7)
	if pr.State != "open" || pr.ReadyAt == nil || pr.readyStatusID != 6 || !sameSHA(pr.readySHA, head) {
		t.Fatalf("not marked ready: %+v", pr)
	}
	if len(be.sg.merges) != 0 {
		t.Fatalf("merged on the draft tier's checks")
	}
	rd, _ := be.s.readiness(be.ctx, pr)
	if rd.Ready || !strings.Contains(strings.Join(rd.Reasons, ";"), "full run hasn't finished since it was marked ready") {
		t.Fatalf("ready on the draft tier's statuses: %+v", rd)
	}
}

// A pull request nobody holds as a draft behaves as before.
func TestNotDraftFirst(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	pr := be.open()
	if pr.WipAuto || pr.State != "open" {
		t.Fatalf("%+v", pr)
	}
	if got := fullRunReasons(pr, nil); len(got) != 0 {
		t.Errorf("reasons %v", got)
	}
}

// An agent's pull request opens as a draft Waypoint holds; --ready, a draft
// asked for, a person, and another forge's don't.
func TestOpenDraftFirst(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	g.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	open := func(who *Actor, head string, mod func(*OpenPRInput)) *PRDetail {
		in := OpenPRInput{Project: p.Key, Head: head, Title: "Do " + head}
		if mod != nil {
			mod(&in)
		}
		res, err := e.s.OpenPullRequest(e.ctx, who, in)
		if err != nil {
			t.Fatal(err)
		}
		return res.Pull
	}
	d := open(agent, "a", nil)
	if d.State != "draft" || !d.WipAuto || d.Title != "Do a" || g.created[0].Draft != true {
		t.Errorf("agent: %+v created %+v", d.PRSummary, g.created[0])
	}
	for head, mod := range map[string]func(*OpenPRInput){"b": func(in *OpenPRInput) { in.Ready = true }, "c": func(in *OpenPRInput) { in.Draft = true }} {
		d := open(agent, head, mod)
		if d.WipAuto || (head == "b" && d.State != "open") {
			t.Errorf("%s: %+v", head, d.PRSummary)
		}
	}
	if d := open(e.admin, "d", nil); d.WipAuto || d.State != "open" {
		t.Errorf("person: %+v", d.PRSummary)
	}
	g.files[".forgejo/workflows/ci.yml"] = ciNotOnEdit
	if d := open(agent, "e", nil); d.WipAuto || d.State != "open" {
		t.Errorf("CI not on edits: %+v", d.PRSummary)
	}
}

// countUpdates counts the pr.updated events fired for each reason.
func countUpdates(be *bsEnv) func() map[string]int {
	var mu sync.Mutex
	seen := map[string]int{}
	prev := be.s.Hub.observe
	be.s.Hub.observe = func(e *Event) {
		prev(e)
		if e.Kind == EvPRUpdated {
			mu.Lock()
			seen[e.New]++
			mu.Unlock()
		}
	}
	return func() map[string]int {
		be.t.Helper()
		be.s.Wait()
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(seen)
	}
}

// The forge's webhook for a new pull request usually beats the answer to the
// call that opened it. It is Waypoint's own draft either way, so the first
// sighting says so: one "opened", and the first review runs (WP-847).
func TestOwnDraftGetsItsFirstReview(t *testing.T) {
	for name, hookFirst := range map[string]bool{"webhook first": true, "answer first": false} {
		t.Run(name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
			be.sg.createAuthor = "claude"
			updates := countUpdates(be)
			var opened ForgePR
			be.sg.afterCreate = func(fp ForgePR) error {
				opened = fp
				if hookFirst {
					be.pullHook("opened", fp)
				}
				return nil
			}
			res, err := be.s.OpenPullRequest(be.ctx, be.agent, OpenPRInput{Project: be.p.Key, Head: "add-export", Title: "Add export", Body: bsPR.Body})
			if err != nil {
				t.Fatal(err)
			}
			if !hookFirst {
				be.pullHook("opened", opened)
			}
			if got := updates(); got["opened"] != 1 {
				t.Errorf("pr.updated events = %v, want one opened", got)
			}
			if pr := be.pr(res.Pull.Number); pr.State != "draft" || !pr.WipAuto {
				t.Errorf("state %s, own draft %v", pr.State, pr.WipAuto)
			}
			runs := be.drain()
			if len(runs) != 1 || runs[0].Kind != "review" || runs[0].Status != "done" {
				t.Errorf("runs = %+v, want its first review", runs)
			}
		})
	}
}

// A slower forge: its webhook is recorded while the call that opened the pull
// request fails, and the opener finds the recorded one (WP-847).
func TestOwnDraftWebhookDuringFailedOpen(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.sg.createAuthor = "claude"
	updates := countUpdates(be)
	be.sg.afterCreate = func(fp ForgePR) error {
		be.pullHook("opened", fp)
		return errors.New("the forge answered 502")
	}
	res, err := be.s.OpenPullRequest(be.ctx, be.agent, OpenPRInput{Project: be.p.Key, Head: "add-export", Title: "Add export", Body: bsPR.Body})
	if err != nil {
		t.Fatal(err)
	}
	if got := updates(); got["opened"] != 1 {
		t.Errorf("pr.updated events = %v, want one opened", got)
	}
	if pr := be.pr(res.Pull.Number); !pr.WipAuto {
		t.Errorf("not marked Waypoint's own: %+v", pr)
	}
}

// A person's draft on a branch nothing is opening stays a draft the rules skip.
func TestPersonsDraftIsNotOwnDraft(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	updates := countUpdates(be)
	p := bsPR
	p.Title, p.State = "WIP: Add export", "draft"
	be.pullHook("opened", p)
	if got := updates(); got["opened"] != 1 {
		t.Errorf("pr.updated events = %v, want one opened", got)
	}
	if pr := be.pr(7); pr.WipAuto {
		t.Errorf("a person's draft was marked Waypoint's own")
	}
	if runs := be.drain(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none for a draft", runs)
	}
}

const (
	ciOnEdit    = "on:\n  push:\n    branches: [main]\n  pull_request:\n    types: [opened, synchronize, reopened, edited]\njobs: {}\n"
	ciNotOnEdit = "on:\n  push:\n    branches: [main]\n  pull_request:\n  workflow_dispatch:\njobs: {}\n"
)

func TestWorkflowRunsOnEdit(t *testing.T) {
	for body, want := range map[string]bool{
		ciOnEdit:                     true,
		ciNotOnEdit:                  false,
		"on: [push, pull_request]\n": false,
		"on: pull_request\n":         false,
		"on:\n  pull_request_target:\n    types: edited\n":                            true,
		"on:\n  pull_request:\n    types: [opened]\n  issues:\n    types: [edited]\n": false,
		"not: [valid": false,
	} {
		if got := workflowRunsOnEdit(body); got != want {
			t.Errorf("%q: %v", body, got)
		}
	}
}

// A draft marked ready in a repo whose CI doesn't run on the edit (as every
// one but Waypoint's was before WP-173) gets no full run: the checks it has
// are the whole run, at marking or once it stalls.
func TestDraftFirstWithoutRunOnEdit(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.draftChecks("7777777aaaa", CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Successful in 9m"})
	if pr := be.pr(7); pr.ReadyAt == nil || pr.readyStatusID != 6 {
		t.Fatalf("not ready: %+v", pr)
	}
	be.sg.files[".forgejo/workflows/ci.yml"] = ciNotOnEdit
	be.setClock(be.clockNow().Add(fullRunStall + time.Minute))
	be.runnerOnline()
	be.step()
	if b := be.babysit(); b.State == BabysitNeedsPerson {
		t.Fatalf("handed to a person: %+v", b)
	}
	be.poll()
	be.s.Wait()
	if len(be.sg.merges) != 1 {
		r, _ := be.s.readiness(be.ctx, be.pr(7))
		t.Fatalf("merges %d; babysit %+v; readiness %v", len(be.sg.merges), be.babysit(), r.Reasons)
	}
}

// A ready pull request that never gets its full run is handed to a person.
func TestDraftFirstFullRunStalls(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.draftChecks("7777777aaaa", CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	if be.pr(7).ReadyAt == nil {
		t.Fatal("not ready")
	}
	be.setClock(be.clockNow().Add(fullRunStall + time.Minute))
	be.runnerOnline()
	be.step()
	b := be.babysit()
	if b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, "full run hasn't finished") {
		t.Fatalf("%+v", b)
	}
}

func (g *shipForge) EditPRBody(ctx context.Context, f *Forge, repo string, number int, body string) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.bodyEdits = append(g.bodyEdits, body)
	return nil
}

// wp pr edit retitles on the forge, keeps a draft's prefix, and checks access.
func TestEditPullRequest(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	pr := be.pr(7)
	title, body := "Add CSV export", "New text."
	if err := be.s.EditPullRequest(be.ctx, be.admin, pr.ID, EditPRInput{Title: &title, Body: &body}); err != nil {
		t.Fatal(err)
	}
	if e := be.sg.edits(); len(e) != 1 || e[0] != "WIP: Add CSV export" {
		t.Errorf("forge title %v", e)
	}
	if got := be.pr(7).Title; got != "Add CSV export" {
		t.Errorf("recorded title %q", got)
	}
	if len(be.sg.bodyEdits) != 1 || be.sg.bodyEdits[0] != body {
		t.Errorf("body %v", be.sg.bodyEdits)
	}
	if err := be.s.EditPullRequest(be.ctx, be.admin, pr.ID, EditPRInput{}); !IsInvalid(err) {
		t.Errorf("nothing to change: %v", err)
	}
	outsider := be.user("sam", false)
	if err := be.s.EditPullRequest(be.ctx, outsider, pr.ID, EditPRInput{Title: &title}); err == nil {
		t.Error("an outsider edited it")
	}
}

// Marked ready in a repo whose CI doesn't run on the edit, it merges on the
// checks it has without waiting.
func TestMarkReadyWithoutRunOnEdit(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	be.sg.files[".forgejo/workflows/ci.yml"] = ciNotOnEdit
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.draftChecks("7777777aaaa", CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Successful in 9m"})
	be.step()
	if e := be.sg.edits(); len(e) != 1 || e[0] != "Add export" {
		t.Fatalf("title edits %v", e)
	}
	if pr := be.pr(7); pr.readyStatusID != 0 || len(pr.readyChecks) != 0 {
		t.Errorf("waits for a full run: %d %v", pr.readyStatusID, pr.readyChecks)
	}
	if len(be.sg.merges) != 1 {
		r, _ := be.s.readiness(be.ctx, be.pr(7))
		t.Fatalf("merges %d; babysit %+v; readiness %v", len(be.sg.merges), be.babysit(), r.Reasons)
	}
}

// DirAt lists a directory's files through Forgejo's contents API; a missing
// directory is nil, not an error.
func TestDirAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/app/contents/.forgejo/workflows":
			if r.URL.Query().Get("ref") != "abc" {
				t.Errorf("ref %q", r.URL.Query().Get("ref"))
			}
			_, _ = w.Write([]byte(`[{"path":".forgejo/workflows/ci.yml","type":"file"},{"path":".forgejo/workflows/lib","type":"dir"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	f := &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "tok"}
	got, err := cl.DirAt(context.Background(), f, "acme/app", ".forgejo/workflows", "abc")
	if err != nil || len(got) != 1 || got[0] != ".forgejo/workflows/ci.yml" {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := cl.DirAt(context.Background(), f, "acme/app", ".github/workflows", "abc"); err != nil || got != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
}

// runsAtForge is the shipping fake with Actions runs and jobs, its runs
// found by commit as wp ci log finds them.
type runsAtForge struct {
	*shipForge
	mu   sync.Mutex
	runs []ActionRun
	jobs map[int64][]ActionJob
}

func newRunsAtForge(sg *shipForge) *runsAtForge {
	return &runsAtForge{shipForge: sg, jobs: map[int64][]ActionJob{}}
}

func (a *runsAtForge) ActionRunsAt(_ context.Context, _ *Forge, _ string, sha string) ([]ActionRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ActionRun
	for _, r := range a.runs {
		if sameSHA(r.CommitSHA, sha) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y ActionRun) int { return cmpInt64(y.Index, x.Index) })
	return out, nil
}

func (a *runsAtForge) ActionJobs(_ context.Context, _ *Forge, _ string, runID int64) ([]ActionJob, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.jobs[runID]), nil
}

func (a *runsAtForge) ActionRuns(context.Context, *Forge, string, []string, int, int) ([]ActionRun, error) {
	return nil, nil
}

func (a *runsAtForge) ActionTasks(context.Context, *Forge, string, []string, int, int) ([]ActionTask, error) {
	return nil, nil
}

func (a *runsAtForge) ActionRunners(context.Context, *Forge) ([]ActionRunner, error) {
	return nil, errRunnersHidden
}

func (a *runsAtForge) ActionRunNumber(context.Context, *Forge, string, int64) (*ActionRun, error) {
	return nil, ErrNotFound
}

func (a *runsAtForge) ActionJobLog(context.Context, *Forge, string, int64) (string, error) {
	return "", nil
}

// prRun puts a pull_request run of sha on the forge with these jobs (name
// → status).
func (a *runsAtForge) prRun(n int64, sha, status string, jobs ...[2]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := 100 + n
	a.runs = slices.DeleteFunc(a.runs, func(r ActionRun) bool { return r.ID == id })
	a.runs = append(a.runs, ActionRun{ID: id, Index: n, WorkflowID: "ci.yml", CommitSHA: sha, Event: "pull_request", Status: status})
	a.jobs[id] = nil
	for i, j := range jobs {
		a.jobs[id] = append(a.jobs[id], ActionJob{ID: id*10 + int64(i), RunID: id, Name: j[0], Status: j[1]})
	}
}

// A job the draft tier runs and the full run skips (Assistant/Mail's
// go-unit) keeps the draft tier's success as its status: Forgejo posts none
// for the skip. Once the head's full run has finished, its jobs say the
// check was skipped, and the pull request merges instead of waiting for its
// full run forever (WP-552).
func TestFullRunSkipsADraftOnlyJob(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	af := newRunsAtForge(be.sg)
	be.s.Forges = af
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)

	af.prRun(1, head, "success", [2]string{"backend", "success"}, [2]string{"e2e", "skipped"})
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	if pr = be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("not marked ready: %+v", pr)
	}
	af.prRun(2, head, "running", [2]string{"backend", "skipped"}, [2]string{"e2e", "success"})
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 20m"})
	if len(be.sg.merges) != 0 || be.stage() != "ready (full run pending)" {
		t.Fatalf("merged on an unfinished run: merges %d stage %q", len(be.sg.merges), be.stage())
	}
	af.prRun(2, head, "success", [2]string{"backend", "skipped"}, [2]string{"e2e", "success"})
	be.poll()
	be.s.Wait()
	if pr = be.pr(7); !slices.Equal(pr.readySkipped, []string{"ci / backend (pull_request)"}) {
		t.Fatalf("ready_skipped %v", pr.readySkipped)
	}
	if len(be.sg.merges) != 1 {
		r, _ := be.s.readiness(be.ctx, be.pr(7))
		t.Fatalf("merges %d; readiness %v", len(be.sg.merges), r.Reasons)
	}
}

func TestFullRunSkipped(t *testing.T) {
	const sha = "abc1234"
	const unit, lint = "ci / unit (pull_request)", "ci / lint (pull_request)"
	cases := []struct {
		name string
		runs func(a *runsAtForge)
		want []string
	}{
		{"draft run only", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "success"}, [2]string{"lint", "success"})
		}, nil},
		{"full run skipped unit", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "success"}, [2]string{"lint", "success"})
			a.prRun(2, sha, "success", [2]string{"unit", "skipped"}, [2]string{"lint", "success"})
		}, []string{unit}},
		{"a later no-op run says nothing", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "success"}, [2]string{"lint", "success"})
			a.prRun(2, sha, "failure", [2]string{"unit", "skipped"}, [2]string{"lint", "failure"})
			a.prRun(3, sha, "success", [2]string{"unit", "skipped"}, [2]string{"lint", "skipped"})
		}, []string{unit}},
		{"unfinished run", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "success"}, [2]string{"lint", "success"})
			a.prRun(2, sha, "running", [2]string{"unit", "skipped"}, [2]string{"lint", "running"})
		}, nil},
		{"before the ready mark", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "skipped"}, [2]string{"lint", "success"})
		}, nil},
		{"cancelled run", func(a *runsAtForge) {
			a.prRun(1, sha, "success", [2]string{"unit", "success"}, [2]string{"lint", "success"})
			a.prRun(2, sha, "cancelled", [2]string{"unit", "skipped"}, [2]string{"lint", "success"})
		}, nil},
		{"another commit", func(a *runsAtForge) {
			a.prRun(1, "def5678", "success", [2]string{"unit", "skipped"}, [2]string{"lint", "success"})
		}, nil},
	}
	for _, c := range cases {
		a := newRunsAtForge(nil)
		c.runs(a)
		a.mu.Lock()
		for i := range a.runs {
			if a.runs[i].Index == 1 && c.name == "before the ready mark" {
				a.runs[i].Created = time.Unix(100, 0)
			}
		}
		a.mu.Unlock()
		readyAt := time.Time{}
		if c.name == "before the ready mark" {
			readyAt = time.Unix(200, 0)
		}
		got, err := fullRunSkipped(context.Background(), a, &Forge{}, "me/app", sha, readyAt, []string{unit, lint})
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}

// A person's draft made ready by their own title edit: the head's statuses
// are the draft run's, whose jobs all skipped on a merge-only push (WP-1808),
// so it is held, as markReady holds Waypoint's own, until the edit's run has
// posted newer statuses, and nothing merges on those skips.
func TestPersonsReadyEditPutsCIBackToPending(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Has been skipped"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	pr := be.pr(7)
	if pr.State != "draft" || pr.WipAuto || pr.CIState != CIPassed {
		t.Fatalf("the draft: state %q wip_auto %v ci %q", pr.State, pr.WipAuto, pr.CIState)
	}
	p.Title = "Add export"
	be.pullHook("edited", p)
	be.s.Wait()
	pr = be.pr(7)
	if pr.State != "open" || pr.readyStatusID != 6 || !sameSHA(pr.readySHA, head) {
		t.Fatalf("after the edit: state %q ready at %q status %d", pr.State, pr.readySHA, pr.readyStatusID)
	}
	rd, err := be.s.readiness(be.ctx, pr)
	if err != nil || rd.Ready || !strings.Contains(strings.Join(rd.Reasons, ";"), "full run hasn't finished") {
		t.Fatalf("ready on the draft run's skips: %+v %v", rd, err)
	}
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 9m"})
	pr = be.pr(7)
	if rd, err = be.s.readiness(be.ctx, pr); err != nil || strings.Contains(strings.Join(rd.Reasons, ";"), "full run") || pr.CIState != CIPassed {
		t.Fatalf("after the full run: %+v %v ci %q", rd, err, pr.CIState)
	}
	// An edit that keeps it open changes nothing.
	p.Body = "more words"
	be.pullHook("edited", p)
	be.s.Wait()
	if pr = be.pr(7); pr.readyStatusID != 6 {
		t.Fatalf("a body edit of an open pull request marked it again: %d", pr.readyStatusID)
	}
}

// As above, but the edit comes before any of the draft run's statuses were
// stored: it is still held, and the draft run's skips alone, coming after,
// don't clear it; a run whose jobs really ran does (audit #1748.2).
func TestPersonsReadyEditBeforeStatusesIsHeld(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	const head = "7777777aaaa"
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	p.Title = "Add export"
	be.pullHook("edited", p)
	be.s.Wait()
	pr := be.pr(7)
	if pr.State != "open" || pr.ReadyAt == nil || pr.readyStatusID != unknownReadyStatus || !sameSHA(pr.readySHA, head) || pr.CIState != CIPending {
		t.Fatalf("after the edit: state %q ready at %q status %d ci %q", pr.State, pr.readySHA, pr.readyStatusID, pr.CIState)
	}
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 5, Description: "Has been skipped"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	pr = be.pr(7)
	rd, err := be.s.readiness(be.ctx, pr)
	if err != nil || rd.Ready || !strings.Contains(strings.Join(rd.Reasons, ";"), "full run hasn't finished") || pr.CIState == CIPassed {
		t.Fatalf("ready on the draft run's skips: %+v %v ci %q", rd, err, pr.CIState)
	}
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 9m"})
	pr = be.pr(7)
	if rd, err = be.s.readiness(be.ctx, pr); err != nil || strings.Contains(strings.Join(rd.Reasons, ";"), "full run") || pr.CIState != CIPassed {
		t.Fatalf("after the full run: %+v %v ci %q", rd, err, pr.CIState)
	}
}
