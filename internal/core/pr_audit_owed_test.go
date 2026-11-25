package core

import (
	"strings"
	"testing"
)

// A head that owes its audit is not settled, and the audit is asked for
// where the line looks, once; where nothing would create the audit it owes
// none and is settled as before (WP-1852).

const notAudited = "its head hasn't been audited yet"

func TestUnsettledAskForTheOwedAuditOnce(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr := be.openMine() // the setting is off: no audit yet
	be.auditOn()
	if _, owed := be.s.auditOwed(be.ctx, pr); !owed {
		t.Fatal("the unaudited head owes no audit")
	}
	if why := be.s.unsettled(be.ctx, pr); why != notAudited {
		t.Fatalf("unsettled = %q, want %q", why, notAudited)
	}
	if got := be.audits(); len(got) != 1 || !strings.Contains(got[0], pr.HeadSHA) {
		t.Fatalf("audits after the first look: %v", got)
	}
	// Asked once: the audit that exists holds the head (going), and no second
	// is made by looking again or by Waypoint's own ready mark.
	if _, owed := be.s.auditOwed(be.ctx, pr); owed {
		t.Error("still owed with the audit made")
	}
	if why := be.s.unsettled(be.ctx, pr); why == "" || why == notAudited {
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
	if _, owed := be.s.auditOwed(be.ctx, pr); owed {
		t.Error("a failed audit leaves the head owing one")
	}
	if why := be.s.unsettled(be.ctx, pr); why == notAudited {
		t.Errorf("a failed audit holds the line: %q", why)
	}
}

// Each case is a head whose audit nothing would create: it owes none, and
// unsettled neither waits for one nor makes one.
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
				if _, owed := be.s.auditOwed(be.ctx, pr); !owed {
					t.Fatal("the refused case should still owe by the predicate")
				}
			} else if _, owed := be.s.auditOwed(be.ctx, pr); owed {
				t.Fatal("owes an audit nothing would create")
			}
			if why := be.s.unsettled(be.ctx, pr); why == notAudited {
				t.Fatalf("waits for an audit nobody will create: %q", why)
			}
			if got := be.audits(); len(got) != 0 {
				t.Fatalf("audits made: %v", got)
			}
		})
	}
}
