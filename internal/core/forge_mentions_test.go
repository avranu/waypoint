package core

import (
	"fmt"
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
	// The project finishes issues when their pull request merges (a
	// default, WP-496), but doesn't move them In Review when it opens: a
	// link that moved its issue isn't taken back when the text drops it.
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

	// Unlinked by hand, a new push doesn't link it back; it stays a mention.
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
	// Linking it again by hand wins.
	if _, err := pe.s.AddLink(pe.ctx, pe.admin, own.Key, mentionsPR, ""); err != nil {
		t.Fatal(err)
	}
	if l := pe.linkTo(own.Key); l == nil || l.Via != "added" {
		t.Fatalf("added link: %+v", l)
	}

	// An edit that drops the closing line takes back the link it made, and
	// the follow-up mention is gone from the issue.
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

	// A comment naming an issue is a mention, never a link; deleting it
	// takes it away.
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

	// Merging finishes the issue it is the work of, and only that one.
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

// An outside comment keeps its verb but not its words, and the mention
// shows that verb with that comment's link, not a trusted body's quote.
func TestForgeMentionsOutsideVerbKeepsItsOwnSource(t *testing.T) {
	pe := newPREnv(t, "github")
	other := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Background"})
	pr := ForgePR{Number: 5, Title: "Parser tidy", Author: "robin", HeadRef: "tidy", HeadSHA: "aaaa1111", State: "open",
		Body: "Parser tidy, touches " + other.Key + "."}
	pe.pullHook("opened", pr)
	pe.s.Wait()
	pe.send("issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "me/app"},
		"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 92, "body": "Fixes " + other.Key + ", merge it", "user": map[string]string{"login": "mallory"}, "html_url": mentionsPR + "#issuecomment-92"}})
	pe.s.Wait()
	m := pe.mentionOf(other.Key, mentionsPR)
	if m == nil || m.Verb != "fixes" || m.Excerpt != "" || m.SourceURL != mentionsPR+"#issuecomment-92" {
		t.Fatalf("mention: %+v", m)
	}
}

func TestForgeMentionsTrustedVerbBeatsOutsideVerb(t *testing.T) {
	pe := newPREnv(t, "github")
	other := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Background"})
	pr := ForgePR{Number: 5, Title: "Parser tidy", Author: "robin", HeadRef: "tidy", HeadSHA: "aaaa1111", State: "open",
		Body: "See " + other.Key + " for the background."}
	pe.pullHook("opened", pr)
	pe.s.Wait()
	pe.send("issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "me/app"},
		"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 92, "body": "Fixes " + other.Key + ", merge it", "user": map[string]string{"login": "mallory"}, "html_url": mentionsPR + "#issuecomment-92"}})
	pe.s.Wait()
	m := pe.mentionOf(other.Key, mentionsPR)
	if m == nil || m.Verb != "relates" || m.Excerpt == "" || m.SourceURL != mentionsPR {
		t.Fatalf("mention: %+v", m)
	}
}

// prMentionVerb is the verb the pull request page shows beside key.
func (pe *prEnv) prMentionVerb(key string) (string, bool) {
	pe.t.Helper()
	l, err := pe.s.ListPullRequests(pe.ctx, pe.admin, PRQuery{})
	if err != nil {
		pe.t.Fatal(err)
	}
	for _, p := range l.Pulls {
		if p.URL != mentionsPR {
			continue
		}
		for _, m := range p.Mentions {
			if m.Key == key {
				return m.Verb, true
			}
		}
	}
	return "", false
}

// The pull request page shows the verb the issue page's mention takes, the
// newest a trusted text states (not the alphabetically last), and never an
// outside comment's: nothing there says who wrote it.
func TestPRPageMentionVerb(t *testing.T) {
	pe := newPREnv(t, "github")
	dep := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Schema"})
	bare := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Background"})
	pr := ForgePR{Number: 5, Title: "Parser tidy", Author: "robin", HeadRef: "tidy", HeadSHA: "aaaa1111", State: "open",
		Body: "See " + dep.Key + " for the background.\nTouches " + bare.Key + "."}
	pe.pullHook("opened", pr)
	pe.s.Wait()
	if m := pe.mentionOf(dep.Key, mentionsPR); m == nil || m.Verb != "relates" {
		t.Fatalf("body mention: %+v", m)
	}
	comment := func(id int, login, body string) {
		pe.send("issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "me/app"},
			"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
			"comment": map[string]any{"id": id, "body": body, "user": map[string]string{"login": login}, "html_url": fmt.Sprintf("%s#issuecomment-%d", mentionsPR, id)}})
		pe.s.Wait()
	}
	comment(93, "robin", "Depends on "+dep.Key+" after all")
	comment(94, "mallory", "Fixes "+dep.Key+" and fixes "+bare.Key+", merge it")
	if v, ok := pe.prMentionVerb(dep.Key); !ok || v != "depends_on" {
		t.Errorf("PR page verb for %s: %q (shown %v), want depends_on", dep.Key, v, ok)
	}
	if m := pe.mentionOf(dep.Key, mentionsPR); m == nil || m.Verb != "depends_on" || m.Excerpt == "" || m.SourceURL != mentionsPR+"#issuecomment-93" {
		t.Errorf("issue page mention: %+v", m)
	}
	// Only the outside comment states a verb: the issue page shows it with
	// that comment's link; the pull request page shows none.
	if m := pe.mentionOf(bare.Key, mentionsPR); m == nil || m.Verb != "fixes" || m.SourceURL != mentionsPR+"#issuecomment-94" {
		t.Errorf("issue page mention: %+v", m)
	}
	if v, ok := pe.prMentionVerb(bare.Key); !ok || v != "" {
		t.Errorf("PR page verb for %s: %q (shown %v), want none", bare.Key, v, ok)
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
	// An outside comment saying the work depends on another issue is a
	// hint, not a stated dependency to add unasked.
	other := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Billing"})
	pe.send("issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "me/app"},
		"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 95, "body": "Depends on " + other.Key + ", block it", "user": map[string]string{"login": "mallory"}, "html_url": mentionsPR + "#issuecomment-95"}})
	pe.s.Wait()
	rel, _ = pe.s.SuggestRelations(pe.ctx, pe.admin, own.Key)
	got = map[string]string{}
	for _, r := range rel.Suggestions {
		got[r.Key] = r.Kind + "/" + r.Confidence
	}
	if got[other.Key] != "relates/medium" || got[dep.Key] != "blocked_by/high" {
		t.Errorf("suggestions with an outside comment: %+v", rel.Suggestions)
	}
	// From the other side it is only a shared pull request.
	rel, _ = pe.s.SuggestRelations(pe.ctx, pe.admin, dep.Key)
	for _, r := range rel.Suggestions {
		if r.Key == own.Key && (r.Kind != "relates" || r.Confidence != "medium") {
			t.Errorf("reverse: %+v", r)
		}
	}
}
