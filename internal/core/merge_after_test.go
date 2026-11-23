package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// taskFor files a task linked to pull request n as its own.
func (be *bsEnv) taskFor(title string, n int) string {
	be.t.Helper()
	task := be.issue(be.admin, IssueInput{Project: be.p.Key, Title: title})
	if _, err := be.s.AddLink(be.ctx, be.admin, task.Key, be.pr(n).URL, ""); err != nil {
		be.t.Fatal(err)
	}
	return task.Key
}

func (be *bsEnv) readinessOf(n int) *PRReadiness {
	be.t.Helper()
	rd, err := be.s.readiness(be.ctx, be.pr(n))
	if err != nil {
		be.t.Fatal(err)
	}
	return rd
}

// A pull request set to merge after one behind it in the line gives up its
// place: the other goes first and merges, and then it gets its turn
// (WP-912).
func TestMergeAfterLetsTheOtherGoFirst(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	d, err := be.s.SetPRMergeAfter(be.ctx, be.agent, be.pr(7).ID, "", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.MergeAfter) != 1 || d.MergeAfter[0].Number != 8 || !d.MergeAfter[0].Set || !strings.HasPrefix(d.MergeAfter[0].Why, "set by ") {
		t.Fatalf("merge after: %+v", d.MergeAfter)
	}
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	be.sweep()
	if pr := be.pr(7); pr.ReadyAt != nil || pr.QueuedAt != nil {
		t.Fatalf("#7 holds the line: %+v", pr)
	}
	if b := be.babysitOf(7); b.State != BabysitWatching || !strings.HasPrefix(b.Wait, "merges after #8, which is still open (set by ") {
		t.Fatalf("#7's babysit: %s %q", b.State, b.Wait)
	}
	if pr := be.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#8 not at the front: %+v (babysit %q)", pr, be.babysitOf(8).Wait)
	}
	be.sg.trees = map[string]string{"4e7ced0ffee": "t8", head8: "t8"}
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 24m"})
	if m := be.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v", m)
	}
	// No sweep: #8's merge looks at #7 again at once.
	be.s.Wait()
	if rd := be.readinessOf(7); mergeWaitIn(rd.Reasons) != "" {
		t.Fatalf("#7 still waits: %v", rd.Reasons)
	}
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not at the front after #8 merged: %+v (babysit %q)", pr, be.babysitOf(7).Wait)
	}
}

// A task blocked by another task makes its pull request wait for the
// other's, with nothing set; a cancelled blocker holds nothing, and a pair
// that waits both ways holds neither.
func TestMergeAfterFromTaskLinks(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	be.queuedDraft(8, "add-import", "8888888cccc")
	a, b := be.taskFor("Export", 7), be.taskFor("Import", 8)
	if err := be.s.AddRelation(be.ctx, be.admin, b, "blocks", a); err != nil {
		t.Fatal(err)
	}
	want := "merges after #8, which is still a draft (its task blocks this one's)"
	if rd := be.readinessOf(7); !slices.Contains(rd.Reasons, want) {
		t.Fatalf("#7's reasons: %v", rd.Reasons)
	}
	if rd := be.readinessOf(8); mergeWaitIn(rd.Reasons) != "" {
		t.Fatalf("#8 waits: %v", rd.Reasons)
	}
	if queueable(be.pr(7), be.readinessOf(7).Reasons) {
		t.Fatal("#7 queueable while it waits")
	}
	d, err := be.s.GetPullRequest(be.ctx, be.admin, be.pr(7).ID)
	if err != nil || len(d.MergeAfter) != 1 || d.MergeAfter[0].Set || d.MergeAfter[0].Why != b+" blocks "+a {
		t.Fatalf("detail: %+v %v", d, err)
	}

	// A link written another way names the same pull request by repo and
	// number on a forge of its kind; one from another kind of forge doesn't.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE external_links SET url = url || '/files' WHERE kind = 'pull'`); err != nil {
		t.Fatal(err)
	}
	if rd := be.readinessOf(7); !slices.Contains(rd.Reasons, want) {
		t.Fatalf("#7's reasons, links written another way: %v", rd.Reasons)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE external_links SET provider = 'github' WHERE kind = 'pull' AND number = 8`); err != nil {
		t.Fatal(err)
	}
	if rd := be.readinessOf(7); mergeWaitIn(rd.Reasons) != "" {
		t.Fatalf("#7 waits on a github pull request: %v", rd.Reasons)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE external_links SET provider = 'forgejo' WHERE kind = 'pull'`); err != nil {
		t.Fatal(err)
	}
	// Nor one on another forge of the same kind.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE external_links SET url = replace(url, 'https://', 'https://other.') WHERE kind = 'pull' AND number = 8`); err != nil {
		t.Fatal(err)
	}
	if rd := be.readinessOf(7); mergeWaitIn(rd.Reasons) != "" {
		t.Fatalf("#7 waits on another forge's pull request: %v", rd.Reasons)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE external_links SET url = replace(url, 'https://other.', 'https://') WHERE kind = 'pull'`); err != nil {
		t.Fatal(err)
	}
	if rd := be.readinessOf(7); !slices.Contains(rd.Reasons, want) {
		t.Fatalf("#7's reasons, links back: %v", rd.Reasons)
	}

	// Waiting both ways: neither holds, and the readiness says why.
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, be.pr(8).ID, "", 7, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{7, 8} {
		rd := be.readinessOf(n)
		if mergeWaitIn(rd.Reasons) != "" || !slices.ContainsFunc(rd.Notes, func(s string) bool { return strings.Contains(s, "neither waits") }) {
			t.Fatalf("#%d: %v %v", n, rd.Reasons, rd.Notes)
		}
	}
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, be.pr(8).ID, "", 7, false); err != nil {
		t.Fatal(err)
	}

	// A cancelled blocker holds nothing.
	canceled := "Canceled"
	if _, err := be.s.UpdateIssue(be.ctx, be.admin, b, IssuePatch{Status: &canceled}); err != nil {
		t.Fatal(err)
	}
	if rd := be.readinessOf(7); mergeWaitIn(rd.Reasons) != "" {
		t.Fatalf("#7 waits on a cancelled task's pull request: %v", rd.Reasons)
	}
}

func TestSetPRMergeAfterRefuses(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	id := be.pr(7).ID
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, id, "", 7, true); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("itself: %v", err)
	}
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, id, "", 99, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

// A wait on a pull request in a repo that shares no project with this
// one's doesn't name it in the merge check reason anyone who sees this one
// reads.
func TestMergeAfterAnotherProject(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	pr := be.pr(7)
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'secret/lib', 3, 'u3', 'open', 0, 0)`, pr.ForgeID)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_merge_after(pr_id, after_id, created_at) VALUES (?, ?, 0)`, pr.ID, other); err != nil {
		t.Fatal(err)
	}
	if got := mergeWaitIn(be.readinessOf(7).Reasons); got != "merges after a pull request in another project, which is still open" {
		t.Fatalf("reason: %q", got)
	}
	// Attached to the same project: named.
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'secret/lib', 0)`, be.p.ID, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	if got := mergeWaitIn(be.readinessOf(7).Reasons); !strings.HasPrefix(got, "merges after secret/lib#3, which is still open (set by ") {
		t.Fatalf("reason: %q", got)
	}
}

// A person merging now doesn't wait for the pull requests it merges after:
// the merge check says only that, and their merge goes through.
func TestMergeAfterPersonMergesNow(t *testing.T) {
	se := readyForgejo(t)
	p := testPR
	p.Number, p.HeadRef, p.HeadSHA = 6, "other", "6666666bbbb"
	se.pullHook("opened", p)
	se.drain()
	id := se.pr(5).ID
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, id, "", 6, true); err != nil {
		t.Fatal(err)
	}
	rd, err := se.s.PRReadiness(se.ctx, se.admin, id)
	if err != nil || rd.Ready || !rd.OnlyWaiting || len(rd.Reasons) != 1 || !strings.HasPrefix(rd.Reasons[0], "merges after #6, which is still open") {
		t.Fatalf("readiness: %+v %v", rd, err)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges: %+v", m)
	}
}

// A wait doesn't bring the pull request up to date with its base: it can't
// merge until the other one has, so a new head would be CI for nothing.
func TestMergeAfterNotUpdatedWhileWaiting(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.pullHook("opened", testPR)
	p := testPR
	p.Number, p.HeadRef, p.HeadSHA = 6, "other", "6666666bbbb"
	se.pullHook("opened", p)
	se.drain()
	id := se.pr(5).ID
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, id, "", 6, true); err != nil {
		t.Fatal(err)
	}
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	se.inLine(id)
	if len(se.sg.merged()) != 0 || se.sg.updated() != 0 {
		t.Fatalf("merges %+v, updates %d", se.sg.merged(), se.sg.updated())
	}
	// The wait dropped: it is brought up to date as before.
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, id, "", 6, false); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	se.inLine(id)
	if se.sg.updated() != 1 {
		t.Fatalf("updates %d after the wait dropped", se.sg.updated())
	}
}

// Pull requests that wait for each other round a longer loop hold none of
// them either: each would wait forever.
func TestMergeAfterLongerLoop(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "a", "7777777aaaa")
	be.queuedDraft(8, "b", "8888888cccc")
	be.queuedDraft(9, "c", "9999999dddd")
	for _, w := range [][2]int{{7, 8}, {8, 9}, {9, 7}} {
		if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, be.pr(w[0]).ID, "", w[1], true); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{7, 8, 9} {
		rd := be.readinessOf(n)
		if mergeWaitIn(rd.Reasons) != "" || !slices.ContainsFunc(rd.Notes, func(s string) bool { return strings.HasSuffix(s, "through 1 more, so none of them waits") }) {
			t.Fatalf("#%d: %v %v", n, rd.Reasons, rd.Notes)
		}
	}
	if d, err := be.s.GetPullRequest(be.ctx, be.admin, be.pr(7).ID); err != nil || len(d.MergeAfter) != 1 || !d.MergeAfter[0].Mutual {
		t.Fatalf("detail: %+v %v", d, err)
	}
	// Broken anywhere, the loop holds again.
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, be.pr(9).ID, "", 7, false); err != nil {
		t.Fatal(err)
	}
	if got := mergeWaitIn(be.readinessOf(7).Reasons); !strings.HasPrefix(got, "merges after #8") {
		t.Fatalf("#7 after the loop broke: %q", got)
	}
}

// The tasks a wait comes from are named only to those who may see them: the
// merge check reason, which anyone who sees the pull request reads, says
// only that a task link holds it.
func TestMergeAfterTaskKeysHidden(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	be.queuedDraft(8, "add-import", "8888888cccc")
	secret := be.project(be.admin, "Secret", "")
	blocker := be.issue(be.admin, IssueInput{Project: secret.Key, Title: "Hidden work"})
	if _, err := be.s.AddLink(be.ctx, be.admin, blocker.Key, be.pr(8).URL, ""); err != nil {
		t.Fatal(err)
	}
	a := be.taskFor("Export", 7)
	if err := be.s.AddRelation(be.ctx, be.admin, blocker.Key, "blocks", a); err != nil {
		t.Fatal(err)
	}
	got := mergeWaitIn(be.readinessOf(7).Reasons)
	if got == "" || strings.Contains(got, secret.Key) {
		t.Fatalf("reason: %q", got)
	}
	bob := be.user("bob", false)
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  *Actor
		keys bool
	}{{be.admin, true}, {bob, false}} {
		d, err := be.s.GetPullRequest(be.ctx, c.who, be.pr(7).ID)
		if err != nil || len(d.MergeAfter) != 1 || strings.Contains(d.MergeAfter[0].Why, secret.Key) != c.keys {
			t.Fatalf("%s's detail: %+v %v", c.who.User.Username, d, err)
		}
	}
}

// A wait that ends by a task change (the blocking task cancelled) looks at
// the waiting pull request again at once, as a merge does.
func TestMergeAfterTaskChangeWakes(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	a, b := be.taskFor("Export", 7), be.taskFor("Import", 8)
	if err := be.s.AddRelation(be.ctx, be.admin, b, "blocks", a); err != nil {
		t.Fatal(err)
	}
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	be.sweep()
	if w := be.babysitOf(7).Wait; !strings.HasPrefix(w, mergeWaitPrefix) {
		t.Fatalf("#7's babysit: %q", w)
	}
	canceled := "Canceled"
	if _, err := be.s.UpdateIssue(be.ctx, be.admin, b, IssuePatch{Status: &canceled}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait() // no sweep
	if w := be.babysitOf(7).Wait; strings.HasPrefix(w, mergeWaitPrefix) {
		t.Fatalf("#7 still waits after its blocker was cancelled: %q", w)
	}
}

// Setting or clearing a wait is recorded and announced, with what was there
// before; asking for what already is changes nothing and says nothing.
func TestSetPRMergeAfterIsRecorded(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	be.queuedDraft(8, "add-import", "8888888cccc")
	sub := be.s.Hub.Subscribe(be.admin.User.ID)
	defer be.s.Hub.Unsubscribe(sub)
	next := func() *Event {
		t.Helper()
		for {
			select {
			case e := <-sub.C:
				if e.Kind == EvPRActivity && e.Data["after_id"] != nil {
					return e
				}
			default:
				return nil
			}
		}
	}
	count := func() (n int) {
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'pr.merge_after'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	set := func(on bool) {
		t.Helper()
		if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, be.pr(7).ID, "", 8, on); err != nil {
			t.Fatal(err)
		}
	}
	set(false) // nothing set: nothing to clear
	if e := next(); e != nil || count() != 0 {
		t.Fatalf("clearing nothing was recorded: %+v", e)
	}
	set(true)
	e := next()
	if e == nil || e.Old != "not set" || !strings.HasPrefix(e.New, "merge after ") || e.Data["on"] != true || e.Data["was_set"] != false || count() != 1 {
		t.Fatalf("set: %+v (audit rows %d)", e, count())
	}
	set(true) // again: no change
	if e := next(); e != nil || count() != 1 {
		t.Fatalf("setting it twice was recorded: %+v", e)
	}
	set(false)
	e = next()
	if e == nil || e.New != "merge after cleared" || !strings.HasPrefix(e.Old, "merge after ") || e.Data["on"] != false || e.Data["was_set"] != true || e.Data["was_created_at"] == nil || count() != 2 {
		t.Fatalf("clear: %+v (audit rows %d)", e, count())
	}
}

// What setting a wait records names the other pull request only as a merge
// check reason does: one in a repo missing from a project this one's is in
// is "a pull request in another project".
func TestSetPRMergeAfterRecordNamesNoHiddenPR(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	pr := be.pr(7)
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'secret/lib', 3, 'u3', 'open', 0, 0)`, pr.ForgeID)
	if err != nil {
		t.Fatal(err)
	}
	hidden, _ := res.LastInsertId()
	// The other repo is in another project the actor sees, so they may set the wait.
	other := be.project(be.admin, "Other", "kanban")
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'secret/lib', 0)`, other.ID, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	sub := be.s.Hub.Subscribe(be.admin.User.ID)
	defer be.s.Hub.Unsubscribe(sub)
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, pr.ID, "secret/lib", 3, true); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT detail FROM audit_log WHERE action = 'pr.merge_after'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "secret") || !strings.Contains(detail, "a pull request in another project") {
		t.Errorf("audit detail = %q", detail)
	}
	var e *Event
	for e == nil {
		select {
		case ev := <-sub.C:
			if ev.Kind == EvPRActivity && ev.Data["on"] != nil {
				e = ev
			}
		default:
			t.Fatal("no event")
		}
	}
	if strings.Contains(e.Old+e.New, "secret") || e.Data["after"] != nil || e.Data["after_id"] != nil {
		t.Errorf("event names the hidden pull request: old=%q new=%q data=%v", e.Old, e.New, e.Data)
	}
	// Attached to the same project: named, as the merge check names it.
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, pr.ID, "secret/lib", 3, false); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'secret/lib', 0)`, be.p.ID, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.SetPRMergeAfter(be.ctx, be.admin, pr.ID, "secret/lib", 3, true); err != nil {
		t.Fatal(err)
	}
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT detail FROM audit_log WHERE action = 'pr.merge_after' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil || !strings.Contains(detail, "secret/lib#3") {
		t.Errorf("audit detail with a shared project = %q (%v), hidden id %d", detail, err, hidden)
	}
}

// With the repo in two projects, where the actor is a viewer in the first and
// an editor in the second, the record goes to the one that lets them set the
// wait (WP-999).
func TestSetPRMergeAfterRecordsUnderTheEditorProject(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.queuedDraft(7, "add-export", "7777777aaaa")
	be.queuedDraft(8, "add-import", "8888888cccc")
	pr := be.pr(7)
	bob := be.user("bob", false)
	b := be.project(be.admin, "Beta", "kanban")
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, b.ID, pr.ForgeID, pr.Repo); err != nil {
		t.Fatal(err)
	}
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := be.s.SetMember(be.ctx, be.admin, b.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	if be.p.ID >= b.ID {
		t.Fatalf("project ids %d, %d: the viewer project must come first", be.p.ID, b.ID)
	}
	if _, err := be.s.SetPRMergeAfter(be.ctx, bob, pr.ID, "", 8, true); err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT project_id FROM audit_log WHERE action = 'pr.merge_after'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if pid != b.ID {
		t.Errorf("audit project_id = %d, want %d (the editor project)", pid, b.ID)
	}
}
