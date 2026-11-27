package core

import "testing"

// A merged pull request keeps the commit it became, from its webhook or a
// later sync, so "where it runs" finds squash and merge commits (WP-23).
func TestPullRequestKeepsItsMergeCommit(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	record := func(fp ForgePR) {
		t.Helper()
		if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &fp, false); err != nil {
			t.Fatal(err)
		}
	}
	record(testPR)
	if got := se.pr(5).MergeSHA; got != "" {
		t.Fatalf("open: merge sha %q", got)
	}
	merged := testPR
	merged.State, merged.MergeSHA = "merged", "9999999cccc"
	record(merged)
	if got := se.pr(5).MergeSHA; got != "9999999cccc" {
		t.Fatalf("merged: merge sha %q", got)
	}
	merged.MergeSHA = ""
	record(merged)
	if got := se.pr(5).MergeSHA; got != "9999999cccc" {
		t.Fatalf("kept: merge sha %q", got)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET merge_sha = '' WHERE id = ?`, se.pr(5).ID); err != nil {
		t.Fatal(err)
	}
	merged.MergeSHA = "9999999cccc"
	if err := se.s.refreshPR(se.ctx, se.f, "me/app", &merged); err != nil {
		t.Fatal(err)
	}
	if got := se.pr(5).MergeSHA; got != "9999999cccc" {
		t.Fatalf("synced: merge sha %q", got)
	}
}

// GitHub fills merge_commit_sha with a test merge while a pull request is
// open; only a merged one's counts.
func TestAPIPRMergeSHAOnlyWhenMerged(t *testing.T) {
	open := apiPR{Number: 1, State: "open", MergeCommitSHA: "abc1234"}
	if got := open.pr().MergeSHA; got != "" {
		t.Errorf("open: %q", got)
	}
	merged := apiPR{Number: 1, State: "closed", Merged: true, MergeCommitSHA: "abc1234"}
	if got := merged.pr().MergeSHA; got != "abc1234" {
		t.Errorf("merged: %q", got)
	}
}

func TestGitLabMergeHookNamesItsMergeCommit(t *testing.T) {
	ev, err := parseGitLabHook([]byte(`{"object_kind":"merge_request","user":{"username":"sam"},"project":{"path_with_namespace":"g/app"},
		"object_attributes":{"iid":9,"state":"merged","action":"merge","merge_commit_sha":"def5678","last_commit":{"id":"abc1234"}}}`))
	if err != nil || ev.pr == nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.pr.MergeSHA != "def5678" || ev.pr.HeadSHA != "abc1234" || ev.pr.MergedBy != "sam" {
		t.Fatalf("got merge %q head %q by %q", ev.pr.MergeSHA, ev.pr.HeadSHA, ev.pr.MergedBy)
	}
}
