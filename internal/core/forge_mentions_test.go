package core

import (
	"slices"
	"strings"
	"testing"
)

const mentionsPR = "https://git.example.com/me/app/pulls/5"

func (pe *prEnv) linkTo(key string) *ExternalLink {
	pe.t.Helper()
	for _, l := range pe.get(pe.admin, key).Links {
		if l.URL == mentionsPR {
			return &l
		}
	}
	return nil
}

func (pe *prEnv) mentionOf(key, url string) *ForgeMention {
	pe.t.Helper()
	for _, m := range pe.get(pe.admin, key).ForgeMentions {
		if m.URL == url {
			return &m
		}
	}
	return nil
}

// A pull request links the issues it is the work of (its branch, title or
// a closing verb) and records the rest as mentions, which an unlink, an
// edit, a comment or a merge treat as such (WP-379).
func TestForgeMentionsLinkOwnWorkOnly(t *testing.T) {
	pe := newPREnv(t, "github")
	autos, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range autos {
		if a.Rule.Trigger.Event == "pr_opened" {
			if err := pe.s.DeleteAutomation(pe.ctx, pe.admin, a.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	own := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Parser"})
	follows := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Old parser bug"})
	closes := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Crash"})
	pr := ForgePR{Number: 5, Title: "Parser tidy", Author: "robin", HeadRef: strings.ToLower(own.Key) + "-tidy", HeadSHA: "aaaa1111", State: "open",
		Body: "Follow-up to " + follows.Key + ".\n\nCloses " + closes.Key}
	pe.pullHook("opened", pr)
	pe.s.Wait()

	if l := pe.linkTo(own.Key); l == nil || l.Via != "branch" || l.State != "open" {
		t.Fatalf("branch link: %+v", l)
	}
	if l := pe.linkTo(closes.Key); l == nil || l.Via != "body" {
		t.Fatalf("closing link: %+v", l)
	}
	if l := pe.linkTo(follows.Key); l != nil {
		t.Fatalf("a follow-up linked: %+v", l)
	}
	m := pe.mentionOf(follows.Key, mentionsPR)
	if m == nil || m.Verb != "follows" || m.Kind != "pull" || !slices.Equal(m.Places, []string{"body"}) || m.Excerpt != "Follow-up to "+follows.Key+"." || m.Own || m.Linked {
		t.Fatalf("mention: %+v", m)
	}
	if m := pe.mentionOf(own.Key, mentionsPR); m != nil {
		t.Errorf("a linked item is listed as a mention too: %+v", m)
	}
	all, err := pe.s.ForgeMentions(pe.ctx, pe.admin, own.Key, true)
	if err != nil || len(all) != 1 || !all[0].Own || !all[0].Linked {
		t.Errorf("all mentions of the own issue: %+v %v", all, err)
	}

	if err := pe.s.RemoveLink(pe.ctx, pe.admin, own.Key, pe.linkTo(own.Key).ID); err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = "bbbb2222"
	pe.pullHook("synchronize", pr)
	pe.s.Wait()
	if l := pe.linkTo(own.Key); l != nil {
		t.Fatalf("relinked after an unlink: %+v", l)
	}
	if m := pe.mentionOf(own.Key, mentionsPR); m == nil || !m.Own {
		t.Errorf("unlinked own work isn't a mention: %+v", m)
	}
	if _, err := pe.s.AddLink(pe.ctx, pe.admin, own.Key, mentionsPR, ""); err != nil {
		t.Fatal(err)
	}
	if l := pe.linkTo(own.Key); l == nil || l.Via != "added" {
		t.Fatalf("added link: %+v", l)
	}

	pr.Body = "Tidies the parser."
	pe.pullHook("edited", pr)
	pe.s.Wait()
	if l := pe.linkTo(closes.Key); l != nil {
		t.Errorf("link kept after the text dropped it: %+v", l)
	}
	if m := pe.mentionOf(follows.Key, mentionsPR); m != nil {
		t.Errorf("gone mention still listed: %+v", m)
	}
	if all, _ := pe.s.ForgeMentions(pe.ctx, pe.admin, follows.Key, true); len(all) != 1 || !all[0].Gone {
		t.Errorf("gone mention not kept: %+v", all)
	}

	comment := func(action, body string) {
		pe.send("issue_comment", map[string]any{"action": action, "repository": map[string]any{"full_name": "me/app"},
			"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
			"comment": map[string]any{"id": 91, "body": body, "user": map[string]string{"login": "alex"}, "html_url": mentionsPR + "#issuecomment-91"}})
	}
	comment("created", "Fixes "+follows.Key+" too, I think")
	pe.s.Wait()
	if m := pe.mentionOf(follows.Key, mentionsPR); m == nil || !slices.Equal(m.Places, []string{"comment"}) || m.SourceURL != mentionsPR+"#issuecomment-91" || m.Title != "Parser tidy" {
		t.Errorf("comment mention: %+v", m)
	}
	if pe.linkTo(follows.Key) != nil {
		t.Error("a comment linked")
	}
	comment("deleted", "")
	pe.s.Wait()
	if m := pe.mentionOf(follows.Key, mentionsPR); m != nil {
		t.Errorf("deleted comment's mention: %+v", m)
	}

	pr.Body = "Tidies the parser. See " + closes.Key + "."
	pr.State = "merged"
	pe.pullHook("closed", pr)
	pe.s.Wait()
	if got := pe.get(pe.admin, own.Key); got.Status.Category != CatDone {
		t.Errorf("own issue after merge: %s", got.Status.Name)
	}
	if got := pe.get(pe.admin, closes.Key); got.Status.Category == CatDone {
		t.Error("a mentioned issue was finished by the merge")
	}
	if m := pe.mentionOf(closes.Key, mentionsPR); m == nil || m.Verb != "relates" || m.State != "merged" {
		t.Errorf("mention after merge: %+v", m)
	}
}

// A commit links by its subject; its body's keys are mentions.
func TestForgeMentionsCommits(t *testing.T) {
	pe := newPREnv(t, "github")
	a := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "A"})
	b := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "B"})
	url := "https://git.example.com/me/app/commit/cccc3333"
	pe.send("push", map[string]any{"ref": "refs/heads/topic", "before": "aaaa", "after": "cccc3333", "repository": map[string]any{"full_name": "me/app"},
		"commits": []map[string]any{{"message": "Queue order (" + a.Key + ")\n\nThis also changes what " + b.Key + " sees.", "url": url,
			"author": map[string]string{"username": "robin"}}}})
	pe.s.Wait()
	var linked bool
	for _, l := range pe.get(pe.admin, a.Key).Links {
		linked = linked || (l.URL == url && l.Kind == "commit" && l.Via == "subject")
	}
	if !linked {
		t.Errorf("subject didn't link: %+v", pe.get(pe.admin, a.Key).Links)
	}
	if l := pe.get(pe.admin, b.Key).Links; len(l) != 0 {
		t.Errorf("body linked: %+v", l)
	}
	if m := pe.mentionOf(b.Key, url); m == nil || m.Kind != "commit" || m.Title != "Queue order ("+a.Key+")" || !slices.Equal(m.Places, []string{"message"}) {
		t.Errorf("commit mention: %+v", m)
	}
}

// A sync reads listed pull requests' text too: a repo whose webhook never
// delivers still links an open one's own work.
func TestForgeMentionsRefresh(t *testing.T) {
	pe := newPREnv(t, "github")
	a := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "A"})
	b := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "B"})
	pe.fg.listed = []*ForgePR{{Number: 5, Title: "Thing", Author: "robin", HeadRef: "topic", HeadSHA: "dddd4444", State: "open", URL: mentionsPR,
		Body: "Implements " + a.Key + "; related to " + b.Key}}
	pe.s.RefreshPullRequests(pe.ctx)
	pe.s.Wait()
	if l := pe.linkTo(a.Key); l == nil || l.State != "open" || l.Via != "body" {
		t.Errorf("refresh link: %+v", l)
	}
	if l := pe.linkTo(b.Key); l != nil {
		t.Errorf("refresh linked a mention: %+v", l)
	}
	if m := pe.mentionOf(b.Key, mentionsPR); m == nil || m.Verb != "relates" {
		t.Errorf("refresh mention: %+v", m)
	}
}

// Links made by scanning text before WP-379 are read again: own work
// keeps its link, a key in passing becomes a mention, and a link whose key
// the text doesn't name (added by hand) stays.
func TestBackfillForgeMentions(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	own := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Own"})
	passing := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Passing"})
	manual := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Manual"})
	url := "https://git.example.com/me/app/pulls/9"
	pe.fg.mu.Lock()
	pe.fg.issues[mk(pe.f, "me/app")] = append(pe.fg.issues[mk(pe.f, "me/app")], ForgeItem{Number: 9, IsPR: true, URL: url, State: "merged",
		Title: "Thing (" + own.Key + ")", Body: "Also changes what " + passing.Key + " shows.", Author: "robin"})
	pe.fg.mu.Unlock()
	for _, i := range []*IssueDetail{own, passing, manual} {
		if _, err := pe.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'me/app', 9, 'Thing', 'merged', 0)`, i.ID, url); err != nil {
			t.Fatal(err)
		}
	}
	res, err := pe.s.BackfillForgeMentions(pe.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 1 || res.Kept != 1 || res.Demoted != 1 || res.Failed != 0 {
		t.Errorf("backfill: %s", res)
	}
	has := func(key string) bool {
		for _, l := range pe.get(pe.admin, key).Links {
			if l.URL == url {
				return true
			}
		}
		return false
	}
	if !has(own.Key) || has(passing.Key) || !has(manual.Key) {
		t.Errorf("after backfill: own %v, passing %v, manual %v", has(own.Key), has(passing.Key), has(manual.Key))
	}
	if m := pe.mentionOf(passing.Key, url); m == nil {
		t.Error("demoted link isn't a mention")
	}
	if again, _ := pe.s.BackfillForgeMentions(pe.ctx, false); again.Items != 0 {
		t.Errorf("backfill ran twice: %s", again)
	}
}

// Issues named by the same pull request are suggested as related; one its
// own pull request says it depends on is a stated dependency (WP-379).
func TestForgeCoMentionsSuggestRelations(t *testing.T) {
	pe := newPREnv(t, "github")
	own := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Export"})
	dep := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Storage"})
	near := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Formats"})
	pe.pullHook("opened", ForgePR{Number: 5, Title: "Export (" + own.Key + ")", Author: "robin", HeadRef: "topic", HeadSHA: "eeee5555", State: "open",
		Body: "Depends on " + dep.Key + ".\n\nSee " + near.Key})
	pe.s.Wait()
	rel, err := pe.s.SuggestRelations(pe.ctx, pe.admin, own.Key)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rel.Suggestions {
		got[r.Key] = r.Kind + "/" + r.Confidence
	}
	if got[dep.Key] != "blocked_by/high" || got[near.Key] != "relates/medium" {
		t.Errorf("suggestions: %+v", rel.Suggestions)
	}
	rel, _ = pe.s.SuggestRelations(pe.ctx, pe.admin, dep.Key)
	for _, r := range rel.Suggestions {
		if r.Key == own.Key && (r.Kind != "relates" || r.Confidence != "medium") {
			t.Errorf("reverse: %+v", r)
		}
	}
}
