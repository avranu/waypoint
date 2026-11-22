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
