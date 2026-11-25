package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A head that owes its audit is not settled, and the audit is asked for
// where the line looks, once; where nothing would create the audit it owes
// none and is settled as before (WP-1852).

const notAudited = "its head hasn't been audited yet"

func (be *bsEnv) owes(pr *PullRequest) bool {
	be.t.Helper()
	owner, err := be.s.auditOwed(be.ctx, pr)
	if err != nil {
		be.t.Fatal(err)
	}
	return owner != 0
}

func (be *bsEnv) why(pr *PullRequest) string {
	why, _ := be.s.unsettled(be.ctx, pr)
	return why
}

func TestUnsettledAskForTheOwedAuditOnce(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr := be.openMine() // the setting is off: no audit yet
	be.auditOn()
	if !be.owes(pr) {
		t.Fatal("the unaudited head owes no audit")
	}
	if why := be.why(pr); why != notAudited {
		t.Fatalf("unsettled = %q, want %q", why, notAudited)
	}
	if got := be.audits(); len(got) != 1 || !strings.Contains(got[0], pr.HeadSHA) {
		t.Fatalf("audits after the first look: %v", got)
	}
	// Asked once: the audit that exists holds the head (going), and no second
	// is made by looking again or by Waypoint's own ready mark.
	if be.owes(pr) {
		t.Error("still owed with the audit made")
	}
	if why := be.why(pr); why == "" || why == notAudited {
		t.Errorf("with its audit going unsettled = %q, want the audit's own hold", why)
	}
	be.ready(pr)
	if got := be.audits(); len(got) != 1 {
		t.Fatalf("audits after another look and the ready mark: %v", got)
	}
	// A failed audit still counts as the head's: it doesn't hold the line.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audits SET status = ? WHERE pr_id = ?`, AuditFailed, pr.ID); err != nil {
		t.Fatal(err)
	}
	if be.owes(pr) {
		t.Error("a failed audit leaves the head owing one")
	}
	if why := be.why(pr); why == notAudited {
		t.Errorf("a failed audit holds the line: %q", why)
	}
}

// Each case is a head whose audit nothing would create (it owes none, or, with
// no runner, the request is refused): unsettled neither waits for one nor
// makes one.
func TestAuditOwedFailsOpen(t *testing.T) {
	for name, tc := range map[string]struct {
		box    bool   // a runner that can audit is online
		setup  string // SQL run after the pull request is opened (setting still off)
		stray  bool   // opened by a stranger-owned login (no babysit owner)
		offset bool   // leave the project setting off
	}{
		"setting off":      {box: true, offset: true},
		"project archived": {box: true, setup: `UPDATE projects SET archived = 1`},
		"outside":          {box: true, setup: `UPDATE pull_requests SET outside = 1`},
		"fork head":        {box: true, setup: `UPDATE pull_requests SET head_repo = 'someone/fork'`},
		"no owner":         {box: true, stray: true},
		"request refused":  {box: false}, // no runner to audit on: RequestAudit errors
	} {
		t.Run(name, func(t *testing.T) {
			be := newBabysitEnv(t)
			if tc.box {
				be.auditBox()
			}
			var pr *PullRequest
			if tc.stray {
				p := bsPR
				p.Author = "stranger"
				be.pullHook("opened", p)
				be.s.Wait()
				be.drain()
				pr = be.pr(7)
			} else {
				pr = be.openMine()
			}
			if !tc.offset {
				be.auditOn()
			}
			if tc.setup != "" {
				if _, err := be.s.db.W.ExecContext(be.ctx, tc.setup); err != nil {
					t.Fatal(err)
				}
				pr = be.pr(7)
			}
			if !tc.box {
				if !be.owes(pr) {
					t.Fatal("the refused case should still owe by the predicate")
				}
			} else if be.owes(pr) {
				t.Fatal("owes an audit nothing would create")
			}
			if why := be.why(pr); why == notAudited {
				t.Fatalf("waits for an audit nobody will create: %q", why)
			}
			if got := be.audits(); len(got) != 0 {
				t.Fatalf("audits made: %v", got)
			}
		})
	}
}

// A marked-ready front tested on its own branch gets a head from Waypoint's
// update (the base merged into the audited head). Its full run goes on past
// frontGrace: when the line recorded the head as the same change (WP-1808's
// carry) it owes no audit and keeps the front; when the base merge came from
// a round that resolved conflicts (not carried) the audit is owed (WP-1852).
func TestFrontWithABaseMergeHeadOwesNoAudit(t *testing.T) {
	for name, carried := range map[string]bool{"clean base merge": true, "conflicts resolved by a round": false} {
		t.Run(name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.auditBox()
			be.auditOn()
			pr := be.openMine() // audited at its first head
			first := pr.HeadSHA
			if got := be.audits(); len(got) != 1 {
				t.Fatalf("audits at the first head: %v", got)
			}
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audits SET status = ? WHERE pr_id = ?`, AuditDone, pr.ID); err != nil {
				t.Fatal(err)
			}
			be.pushMine("8888888bbbb") // the update's merge head
			now := be.clockNow()
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET state = 'open', wip_auto = 1, ready_at = ?, ready_sha = head_sha, head_at = ? WHERE id = ?`,
				now.Add(-45*time.Minute).UnixMilli(), now.Add(-40*time.Minute).UnixMilli(), pr.ID); err != nil {
				t.Fatal(err)
			}
			if carried {
				if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET carried_sha = head_sha, carried_from_sha = ?, carried_at = ? WHERE id = ?`,
					first, now.Add(-60*time.Minute).UnixMilli(), pr.ID); err != nil {
					t.Fatal(err)
				}
			}
			p := be.pr(7)
			why := be.s.lineLeft(be.ctx, p) // its full run is going: 40 minutes past the head's move
			if carried {
				if why != "" {
					t.Fatalf("the front gave way (%q) for a head that only merged the base in", why)
				}
				if got := be.audits(); len(got) != 1 {
					t.Fatalf("audits for a carried head: %v", got)
				}
				return
			}
			if why != notAudited {
				t.Fatalf("lineLeft = %q, want %q for a head a round's merge made", why, notAudited)
			}
			if got := be.audits(); len(got) != 2 || !strings.Contains(got[1], "8888888bbbb") {
				t.Fatalf("audits: %v, want one for the new head", got)
			}
		})
	}
}

// A head that moves under the marked-ready front is asked for its audit at the
// first look, within the grace, and the audit then holds its merge (WP-1852).
func TestFrontNewHeadIsAskedForItsAuditWithinGrace(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.pushMine("8888888bbbb")
	now := be.clockNow()
	at := now.Add(-time.Minute).UnixMilli()
	be.exec(fmt.Sprintf(`UPDATE pull_requests SET state = 'open', wip_auto = 1, ready_at = %d, ready_sha = head_sha, head_at = %d WHERE id = %d`, at, at, pr.ID))
	if why := be.s.lineLeft(be.ctx, be.pr(7)); why != "" {
		t.Fatalf("the front left %q within its grace", why)
	}
	if got := be.audits(); len(got) != 2 || !strings.Contains(got[1], "8888888bbbb") {
		t.Fatalf("audits: %v, want one for the new head", got)
	}
	var rd PRReadiness
	holds, err := be.s.reviewHolds(be.ctx, be.pr(7), true, &rd)
	if err != nil || !slicesHas(holds, "waiting for audit #") {
		t.Fatalf("the merge isn't held for it: %q (%v)", holds, err)
	}
}

// The audit reads the commit it was asked for when the forge shows another, so
// the head counts as audited. A request whose key already names an audit of
// another commit isn't waited for: nothing would replace it.
func TestAuditOfAHeadTheForgeDoesNotShow(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	be.openMine()
	be.auditOn()
	be.exec(`UPDATE pull_requests SET head_sha = 'aaaaaaa1111'`) // the forge still shows the old head
	if why := be.why(be.pr(7)); why != notAudited {
		t.Fatalf("unsettled = %q, want %q", why, notAudited)
	}
	if got := be.audits(); len(got) != 1 || !strings.Contains(got[0], "aaaaaaa1111") {
		t.Fatalf("audits: %v, want one of the head asked for", got)
	}
	if be.owes(be.pr(7)) {
		t.Fatal("the head still owes an audit")
	}
	be.exec(`UPDATE code_audits SET ref = 'bbbbbbb2222'`)
	if why := be.why(be.pr(7)); why == notAudited {
		t.Fatalf("waits for an audit nobody will create: %q", why)
	}
}

// An audit of any head carrying the same change covers a later clean base
// merge, not only one of the head the change was first seen at.
func TestAuditOfALaterCarriedHeadCovers(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr := be.openMine()
	be.auditOn()
	be.exec(`UPDATE pull_requests SET head_sha = 'ccccccc3333', carried_sha = 'ccccccc3333', carried_from_sha = 'aaaaaaa1111'`)
	for _, h := range []string{"aaaaaaa1111", "bbbbbbb2222", "ccccccc3333"} {
		be.exec(fmt.Sprintf(`INSERT INTO reviewed_heads(pr_id, automation_id, head_sha, diff_hash, summary, content_sha, content_at, carried, from_sha, created_at)
			VALUES (%d, 0, '%s', 'h', '', 'aaaaaaa1111', 0, 0, '', 0)`, pr.ID, h))
	}
	if !be.owes(be.pr(7)) {
		t.Fatal("a change no head of which was audited owes none")
	}
	if _, err := be.s.RequestAudit(be.ctx, SystemActor(), AuditInput{PRID: pr.ID, Via: "system", owner: be.s.babysitOwnerFor(be.ctx, be.pr(7)),
		head: "bbbbbbb2222", requestKey: "test"}); err != nil {
		t.Fatal(err)
	}
	if be.owes(be.pr(7)) {
		t.Fatal("the same change, audited at a later head, owes another")
	}
}

// A failed read of the audits leaves the head unsettled, not audited.
func TestUnsettledOnFailedAuditRead(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr := be.openMine()
	be.auditOn()
	be.exec(`ALTER TABLE code_audits RENAME TO code_audits_x`)
	if why, unread := be.s.unsettled(be.ctx, be.pr(pr.Number)); why != "its audits couldn't be read" || !unread {
		t.Fatalf("unsettled = %q (unread %v)", why, unread)
	}
}

// A repo in two projects owes the audit either of them asks for.
func TestAuditOwedBySecondProject(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr := be.openMine()
	be.auditOn()
	if !be.owes(pr) {
		t.Fatal("owes none in one project")
	}
	var first int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT project_id FROM project_repos WHERE forge_id = ?`, pr.ForgeID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// The lower id is the one with audits off.
	other, err := be.s.CreateProject(be.ctx, be.admin, ProjectInput{Key: str("TWO"), Name: str("Two")})
	if err != nil {
		t.Fatal(err)
	}
	be.exec(fmt.Sprintf(`UPDATE projects SET settings = (SELECT settings FROM projects WHERE id = %d) WHERE id = %d`, first, other.ID))
	be.exec(fmt.Sprintf(`UPDATE projects SET settings = '{}' WHERE id = %d`, first))
	if be.owes(pr) {
		t.Fatal("owes one with audits off")
	}
	be.exec(fmt.Sprintf(`INSERT INTO project_repos(project_id, forge_id, repo, bots, created_at) SELECT %d, forge_id, repo, 1, created_at FROM project_repos WHERE project_id = %d`, other.ID, first))
	if !be.owes(pr) {
		t.Fatal("the second project's setting asks for none")
	}
}
