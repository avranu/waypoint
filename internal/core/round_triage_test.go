package core

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The headlines below are real free-model findings from 2026-11-02 that a
// round rejected as false (the compile claims), and findings about runtime
// faults or other points that a green build doesn't answer.
func TestCompileClaim(t *testing.T) {
	bot := func(path, body string) *PRReviewComment {
		return &PRReviewComment{Kind: "inline", Author: "waypoint", Bot: true, Path: path, Body: body}
	}
	const sub = " <sub>**Automated review** by `ovh/gpt-oss-120b` · not checked by a person; verify before acting on it.</sub>"
	for _, tc := range []struct {
		rc   *PRReviewComment
		want bool
	}{
		{bot("internal/core/projects.go", "**P0** critical · ProjectSettings struct has duplicate field AuditPRs, causing a compile error."+sub), true},
		{bot("next_ui.go", "**P1** high · Compilation fails because the `strings` package is used without being imported."+sub), true},
		{bot("CardBadges.tsx", "**P1** high · Duplicate import of `Tip` results in a compile error."+sub), true},
		{bot("server.go", "**P1** high · Undefined variable `ui` is used in `setPageSecurityHeaders`, causing a compile‑time error."+sub), true},
		{bot("criteria.ts", "**P1** high · Narrowing PickerEntity introduces a TypeScript type error that will break the build."+sub), true},
		{bot("loaders.go", "[critical] Deletion of folderloader_gen.go removes the FolderLoader type used by other components, causing compilation errors."), true},
		{bot("loaders.go", "**P0** critical · NewBatch function used but not defined"+sub), true},
		{bot("rowSpecs.ts", "**P1** high · Missing import of useMemo in rowSpecs.ts results in an undefined identifier."+sub+"\n<!-- waypoint-finding run=3 n=1 -->"), true},
		{&PRReviewComment{Kind: "relayed", Author: "mirror", Bot: true, Path: "app/a.go",
			Body: "**coderabbitai[bot]** reviewed on `app/a.go` line 12:\n\n_⚠️ Potential issue_ | _🔴 Critical_\n\n**`fmt` is not imported, so this doesn't compile.**\n\nAdd it."}, true},

		{bot("ListTable.tsx", "**P1** high · Accessing item.id throws a TypeError when items is undefined."+sub), false},
		{bot("worker.go", "**P0** critical · A nil pointer dereference crashes the worker when cfg is undefined."+sub), false},
		{bot("hooks.ts", "**P1** high · The effect re-runs on every render, an infinite render loop."+sub), false},
		{bot("server.go", "**P0** critical · Registering the delivery route twice will cause a panic at server start."+sub), false},
		{bot("retry.go", "**P2** medium · The new `CanRetryChecks` method is not used anywhere, making it dead code. It also uses the `slices` package without importing it, which will produce a compile error."+sub), false},
		{bot("store.go", "**P1** high · err is redeclared with := so the Open error is lost."+sub), false},
		{bot("types.go", "**P2** medium · Duplicate field tag `json:\"id\"` on ID and Ref drops one of them when encoding."+sub), false},
		{bot("prompt.go", "**P2** medium · The prompt tells the agent to work without importing the package docs."+sub), false},
		{bot("a.go", "**P1** high · x redeclared in this block."+sub), true},
		{bot("a.ts", "**P1** high · Cannot redeclare block-scoped variable 'x'."+sub), true},
		{bot("a.go", "**P1** high · It calls strings.Join without importing strings."+sub), true},
		{bot("resume_style.py", "**P1** high · `re` is used without being imported, causing a NameError."+sub), false},
		{bot("deploy/probe.sh", "**P1** high · Line 27 has a syntax error."+sub), false},
		{bot("", "**P0** critical · The change has a syntax error."+sub), false},
		{&PRReviewComment{Kind: "inline", Author: "alice", Path: "a.go", Body: "This doesn't compile: fmt isn't imported."}, false},
	} {
		if got := tc.rc.compileClaim(); got != tc.want {
			t.Errorf("compileClaim(%s: %q) = %v, headline %q", tc.rc.Path, tc.rc.Body, got, headline(tc.rc.Body))
		}
	}
	if got := builtChecks([]PRCheck{{Name: "ci / test", State: CheckSuccess}, {Name: "ci / e2e", State: CheckSuccess, Description: "Skipped"},
		{Name: "waypoint/review", State: CheckSuccess}, {Name: "ci / lint", State: CheckPending}}); !slices.Equal(got, []string{"ci / test"}) {
		t.Errorf("builtChecks = %v", got)
	}
}

// A bot's compile claim on a head whose build passed is rejected before a
// round, with the passing check named; on a failed build it goes to the
// round.
func TestBabysitTriagesCompileClaims(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	claim := "**P0** critical · Duplicate definition of `Export` causes a compile error, breaking the build."
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "llm-review", Body: claim, SHA: "7777777aaaa", At: at}}}}
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	if b := be.babysit(); b.State != BabysitWorking || len(be.jobs()) != 1 {
		t.Fatalf("a failed build with a compile claim: %+v", b)
	}
	if c := be.comment("501"); c.Dealt {
		t.Fatalf("triaged on a failed build: %+v", c)
	}
	be.finish(be.jobs()[0], true)

	be.push("8888888bbbb")
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	c := be.comment("501")
	if c.Verdict != VerdictRejected || !c.Dealt || !strings.Contains(c.Note, "it builds: `ci / test` passed on 8888888") {
		t.Fatalf("compile claim on a green head = %+v", c)
	}
	if n := len(be.jobs()); n != 1 {
		t.Errorf("%d rounds, want only the first", n)
	}
	be.sg.sm.Lock()
	replies := strings.Join(append(slices.Clone(be.sg.posted), be.sg.replies...), "\n")
	be.sg.sm.Unlock()
	if !strings.Contains(replies, "Not changing this: it builds") {
		t.Errorf("replies = %q", replies)
	}
	var by string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT by_name FROM review_comment_events WHERE verdict = ? ORDER BY id DESC LIMIT 1`, VerdictRejected).Scan(&by); err != nil || by != "triage" {
		t.Errorf("rejected by %q (%v), want triage", by, err)
	}
	if m := be.sg.merged(); len(m) != 1 {
		t.Errorf("not merged once the claim was answered: %v %q", m, be.reasons(7))
	}
}

// A rejected compile claim from Waypoint's own review is a dismissed
// finding: it counts against the reviewer model's precision (WP-362).
func TestTriagedFindingIsDismissed(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	now := be.s.nowMS()
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at, finished_at)
		VALUES (?, ?, 'review', ?, 'done', ?, ?)`, be.p.ID, pr.ID, pr.HeadSHA, now, now)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := res.LastInsertId()
	claim := "Missing import of `fmt` results in an undefined identifier."
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, state, placement, ordinal, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, run, pr.ID, "app/export.go", 4, "critical", claim, "ovh/gpt-oss-120b", FindingOpen, "inline", 1, now, now); err != nil {
		t.Fatal(err)
	}
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "waypoint-bot", Body: "**P0** critical · " + claim + "\n<!-- " + "waypoint-finding run=" + strconv.FormatInt(run, 10) + " n=1 -->", SHA: "7777777aaaa", At: time.Now()}}}}
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	be.step()
	c := be.comment("501")
	if c.FindingID == 0 || c.Verdict != VerdictRejected {
		t.Fatalf("finding comment = %+v", c)
	}
	var state string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT state FROM bot_findings WHERE id = ?`, c.FindingID).Scan(&state); err != nil || state != FindingDismissed {
		t.Errorf("finding is %q (%v), want dismissed", state, err)
	}
	if n := len(be.jobs()); n != 0 {
		t.Errorf("%d rounds for a triaged finding", n)
	}
}

// A fixed comment that an edit on the forge only rewords stays fixed while
// its fix is on the head; a new point, or a fix gone from the head, opens it
// again.
func TestFixedCommentRewordedStaysFixed(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	thread := func(body string) {
		be.sg.sm.Lock()
		be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
			{ID: "501", Author: "alice", Body: body, SHA: "7777777aaaa", At: at}}}}
		be.sg.sm.Unlock()
	}
	thread("This loop never ends when rows is empty.")
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	ref := be.comment("501").Ref
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{ref}, Verdict: VerdictAccepted}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.FixedReviewComments(be.ctx, be.agent, FixedInput{IDs: []string{ref}, SHA: "8888888bbbb"}); err != nil {
		t.Fatal(err)
	}
	be.finish(be.jobs()[0], true)
	be.push("8888888bbbb")
	if c := be.comment("501"); c.Verdict != VerdictFixed {
		t.Fatalf("after the push = %+v", c)
	}

	thread("This loop never ends when the rows slice is empty.")
	be.step()
	if c := be.comment("501"); c.Verdict != VerdictFixed || !strings.Contains(c.Body, "rows slice") {
		t.Errorf("reworded = %+v", c)
	}

	thread("Export also leaks the file handle; close it with defer.")
	be.step()
	if c := be.comment("501"); c.Verdict != VerdictNew {
		t.Errorf("a new point = %+v", c)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE review_comments SET verdict = ?, fix_sha = ? WHERE id = ?`, VerdictFixed, "8888888bbbb", be.comment("501").id); err != nil {
		t.Fatal(err)
	}

	be.push("9999999cccc")
	thread("Export also leaks its file handle; close it with defer.")
	be.step()
	if c := be.comment("501"); c.Verdict != VerdictFixed {
		t.Errorf("reworded on a head with the fix = %+v", c)
	}
	be.sg.sm.Lock()
	be.sg.hidden = map[string]bool{"8888888bbbb": true}
	be.sg.sm.Unlock()
	thread("Export also leaks the file handle; please close it with defer.")
	be.step()
	if c := be.comment("501"); c.Verdict != VerdictNew {
		t.Errorf("reworded with its fix gone from the head = %+v", c)
	}
}

// A round called for a conflict the work packet doesn't show (the forge
// stopped reporting it) doesn't go out: it would only say "nothing to do".
func TestBabysitNoRoundOnEmptyPacket(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	b := be.babysit()
	be.s.babysitRound(be.ctx, b, be.pr(7), nil, true, nil, nil)
	be.s.Wait()
	if b = be.babysit(); b.State != BabysitWatching || !strings.Contains(b.Wait, "work packet is empty") || b.Round != 0 {
		t.Fatalf("babysit after an empty packet = %+v", b)
	}
	if n := len(be.jobs()); n != 0 {
		t.Errorf("%d rounds on an empty packet", n)
	}
	be.s.babysitRound(be.ctx, b, be.pr(7), nil, false, nil, []string{"1.1"})
	be.s.Wait()
	if b = be.babysit(); strings.Contains(b.Wait, "work packet is empty") {
		t.Errorf("a round for an audit finding was held as empty: %+v", b)
	}
}
