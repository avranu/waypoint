package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// noteForge has a pull request's conversation and takes emoji on it.
type noteForge struct {
	core.ForgeClient
	mu      sync.Mutex
	notes   []core.PRNote
	reacted []string
}

func (f *noteForge) GetItem(ctx context.Context, fg *core.Forge, repo string, n int, pr bool) (*core.ForgeItem, error) {
	return &core.ForgeItem{Number: n, Title: "Change", State: "open", IsPR: pr}, nil
}

func (f *noteForge) PRNotes(context.Context, *core.Forge, string, int) ([]core.PRNote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notes), nil
}

func (f *noteForge) ReactTo(ctx context.Context, fg *core.Forge, repo string, n int, kind, id, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reacted = append(f.reacted, kind+":"+id+":"+emoji)
	return nil
}

func TestCLIBabysit(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	fg := &noteForge{notes: []core.PRNote{
		{ID: "701", Kind: "comment", Author: "carol", Body: "Please add a test for the empty case.", At: time.Now()},
		{ID: "702", Kind: "comment", Author: "llm-review", Body: "The retry loop has no limit.", At: time.Now()},
	}}
	e.svc.Forges = fg
	prRepo(t, e, me)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Add the retry limit")

	if out := e.wp("pr", "babysit", "3", "--status"); !strings.Contains(out, "#3 isn't babysat") {
		t.Errorf("status before:\n%s", out)
	}
	if out := e.wp("pr", "babysit", "3"); out != "Waypoint is babysitting #3.\n" {
		t.Errorf("start:\n%q", out)
	}
	e.svc.Wait()
	out := e.wp("pr", "todo", "3")
	for _, want := range []string{"robin/waypoint#3 (b3 → main) at abc1233", "## Open review comments (2)", "### c1 comment by carol", "Please add a test", "### c2 comment by llm-review"} {
		if !strings.Contains(out, want) {
			t.Errorf("todo lacks %q:\n%s", want, out)
		}
	}
	if out, code := e.run("pr", "ack", "c1", "--reject"); code != 2 || !strings.Contains(out, "say why with -m") {
		t.Errorf("reject without a reason: %d %s", code, out)
	}
	if out, code := e.run("pr", "ack", "c1", "--accept", "--reject", "-m", "x"); code != 2 || !strings.Contains(out, "give one of") {
		t.Errorf("two verdicts: %d %s", code, out)
	}
	if out := e.wp("pr", "ack", "c1", "--reject", "-m", "An empty export is covered by TestExportNone."); out != "c1 rejected (👎)\n" {
		t.Errorf("ack:\n%q", out)
	}
	if out := e.wp("pr", "ack", "c2", "--accept"); out != "c2 accepted (👍)\n" {
		t.Errorf("accept:\n%q", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "retry.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "retry.go")
	if out, code := e.run("pr", "fixed", "c2"); code != 2 || !strings.Contains(out, "commit the fix first") {
		t.Errorf("fixed with uncommitted changes: %d %s", code, out)
	}
	sh(t, dir, "commit", "-q", "-m", "Limit the retries")
	if out := e.wp("pr", "fixed", "c2"); out != "c2 fixing (🚀)\n" {
		t.Errorf("fixed:\n%q", out)
	}
	fg.mu.Lock()
	reacted := slices.Clone(fg.reacted)
	fg.mu.Unlock()
	for _, want := range []string{"comment:701:-1", "comment:702:+1", "comment:702:rocket"} {
		if !slices.Contains(reacted, want) {
			t.Errorf("reactions %v lack %s", reacted, want)
		}
	}
	out = e.wp("pr", "babysit", "3", "--status")
	for _, want := range []string{"#3: babysitting, watching", "round 0 of at most 15, sonnet", "Review comments: 2, 1 not dealt with", "c1    rejected", "c2    fixing"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if out, code := e.run("pr", "babysit", "3", "--stop"); code != 1 || !strings.Contains(out, "a person stops this babysit") {
		t.Errorf("an agent stops it: %d %s", code, out)
	}
	var shown struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(e.wp("pr", "show", "3", "--json")), &shown); err != nil || shown.ID == 0 {
		t.Fatalf("pr show --json: %v", err)
	}
	if _, err := e.svc.StopBabysit(context.Background(), &core.Actor{User: me.User}, shown.ID); err != nil {
		t.Fatal(err)
	}
	if out := e.wp("pr", "babysit", "3", "--status"); !strings.Contains(out, "babysitting ended, stopped") {
		t.Errorf("status after:\n%s", out)
	}
	if out := e.wp("help", "pr", "babysit"); !strings.Contains(out, "wakes you within minutes (if other work waits on it)") {
		t.Errorf("help:\n%s", out)
	}
}

func TestBabysitStatusShowsRoundClasses(t *testing.T) {
	raw := `{"babysit":{"state":"working","active":true,"round":2,"max_rounds":5,"model":"sonnet","owner":{"username":"robin"},"rounds":[
		{"n":1,"sha":"aaaaaaaa1","to":"laptop","outcome":"done, nothing pushed","class":"nothing_pushed","work_class":"new","ledger":[{"id":"c1","class":"new"}]},
		{"n":2,"sha":"bbbbbbbb2","to":"laptop","class":"repeat","work_class":"repeat","ledger":[
			{"id":"c5","class":"repeat","of":"c1","round":1},{"id":"c6","class":"caused","round":1},{"id":"ci / test","class":"ci_or_conflict"}]}]}}`
	var v babysitView
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	got := v.text(7)
	for _, want := range []string{
		"    class nothing pushed (given new work): c1 new\n",
		"    class repeat: c5 repeats c1 (fixed in round 1), c6 caused by round 1, ci / test CI or conflict",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
}

func TestRoundStatsText(t *testing.T) {
	st := roundStats{Days: 7, PRs: 3, Rounds: 12, Classed: 10, Class: map[string]int{"new": 4, "repeat": 1, "nothing_pushed": 5},
		WorkClass: map[string]int{"new": 8, "repeat": 2}}
	got := st.text()
	for _, want := range []string{
		"Babysitting rounds, last 7 days: 12 on 3 pull requests\n",
		"10 have a class; the rest are from before the ledger",
		"new                   4   40%      8   80%\n",
		"nothing pushed        5   50%      -",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats lack %q:\n%s", want, got)
		}
	}
	st = roundStats{Days: 7, PRs: 4, Rounds: 20, Strong: 4, Assessor: 2, Parked: 1, Rested: 2, Merged: 3, MergedRounds: 9}
	got = st.text()
	for _, want := range []string{"Merged: 3, after 3.0 rounds each.", "Stronger model: 4 rounds (20%); assessor: 2 (10%); parked for a person after 1; rested, nobody asked, after 2."} {
		if !strings.Contains(got, want) {
			t.Errorf("stats lack %q:\n%s", want, got)
		}
	}
	got = (&roundStats{Days: 7, Rounds: 2, PRs: 1, AuthorHandoffs: 12, AuthorFixed: 8, AuthorFellBack: 3, AuthorCleared: 1}).text()
	if want := "Failed first checks handed to the author: 12 (new head before the deadline 8, fell back to a round 3, checks passed unchanged 1, still pending 0)"; !strings.Contains(got, want) {
		t.Errorf("stats lack %q:\n%s", want, got)
	}
	if got := (&roundStats{Days: 7, Rounds: 2, PRs: 1}).text(); !strings.Contains(got, "--replay classifies them") {
		t.Errorf("unclassified: %s", got)
	}
}
