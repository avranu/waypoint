package core

import (
	"testing"
	"time"
)

// A pull request a person opened is adopted for the branch but isn't made the
// agent's by it (WP-1370).
func TestAdoptedPersonsPullRequestIsNotStampedAsTheAgents(t *testing.T) {
	e, open := openedElsewhere(t)
	open.Author = "alice"
	e.g.listed = []*ForgePR{open}
	e.tick(time.Second)
	e.s.Wait()
	var agent string
	if err := e.s.db.R.QueryRow(`SELECT agent FROM pull_requests WHERE number = 55`).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if agent != "" {
		t.Fatalf("a person's pull request was recorded as opened by agent %q", agent)
	}
	var author string
	_ = e.s.db.R.QueryRow(`SELECT forge_author FROM pull_requests WHERE number = 55`).Scan(&author)
	if author != "alice" {
		t.Fatalf("its real opener = %q", author)
	}
}

// A pull request from a fork's branch of the same name isn't the task's.
func TestForkPullRequestWithTheSameHeadRefIsNotAdopted(t *testing.T) {
	e, open := openedElsewhere(t)
	open.HeadRepo = "someone/app"
	e.g.listed = []*ForgePR{open}
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("a fork's pull request stood in for the task's: opened %d", len(e.g.created))
	}
}

// An adopted pull request at another commit than the one reviewed isn't armed.
func TestAdoptedPullRequestAtAnotherHeadIsNotArmed(t *testing.T) {
	e, open := openedElsewhere(t)
	open.HeadSHA = tipPushed
	e.g.listed = []*ForgePR{open}
	e.tick(time.Second)
	e.s.Wait()
	var armed int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE number = 55 AND armed_at IS NOT NULL`).Scan(&armed)
	if armed != 0 {
		t.Fatal("an adopted pull request at an unreviewed commit was armed")
	}
}

// A fork's pull request already recorded under the branch's name isn't the
// task's pull request either: the ship opens its own and does not stop on
// one that is linked to no task (WP-1370).
func TestRecordedForkPullRequestIsNotTheBranchsPullRequest(t *testing.T) {
	e, _ := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	e.g.heads = map[string]string{"wp-1-first": tipAudited}
	var fid int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM forges LIMIT 1`).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	fp := &ForgePR{Number: 77, Title: "fork's", State: "open", HeadRef: "wp-1-first", HeadRepo: "someone/app", HeadSHA: tipPushed, URL: "https://git.example.com/acme/app/pulls/77", BaseRef: "main", Author: "someone"}
	if _, err := e.s.recordPR(e.ctx, &Forge{ID: fid}, "acme/app", fp, false); err != nil {
		t.Fatal(err)
	}
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("a fork's recorded pull request stood in for the task's: opened %d", len(e.g.created))
	}
}
