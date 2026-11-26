package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// wp ship A B opens one pull request linked to both tasks (WP-1426).
func TestShipSeveralTasksLinksEveryTask(t *testing.T) {
	a, g := shipEnvOnForgeHost(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second"})
	res, err := a.ship(first.Key, ShipSkip, func(in *OpenPRInput) { in.Reason, in.Also = "test", []string{strings.ToLower(second.Key)} })
	if err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 1 || res.Pull == nil || res.Warning != "" {
		t.Fatalf("created %d, warning %q", len(g.created), res.Warning)
	}
	if want := []string{second.Key}; !slices.Equal(res.AlsoLinked, want) {
		t.Errorf("also linked = %v, want %v", res.AlsoLinked, want)
	}
	a.s.Wait()
	for _, k := range []string{first.Key, second.Key} {
		d, err := a.s.GetIssue(a.ctx, a.admin, k)
		if err == nil {
			t.Logf("%s after the open and its link refresh: %s", k, d.Status.Name)
		}
		if err != nil || len(d.Links) != 1 || d.Links[0].URL != res.Pull.URL {
			t.Fatalf("%s links: %v %+v", k, err, d.Links)
		}
	}
	// The merge finishes every issue linked to the pull request, once.
	a.s.applyLinkState(a.ctx, res.Pull.URL, &ForgeItem{IsPR: true, State: "merged", Title: "First"})
	a.s.Wait()
	for _, k := range []string{first.Key, second.Key} {
		d, err := a.s.GetIssue(a.ctx, a.admin, k)
		if err != nil || d.Status.Category != CatDone {
			t.Errorf("%s after the merge: %v %+v", k, err, d.Status)
		}
	}
}

// What can't go along is refused before the forge is asked for anything,
// and the error names the key and what to run.
func TestShipRefusesTasksThatCannotGoAlong(t *testing.T) {
	a, _, g := firstEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	closed := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Closed", Agent: "claude"})
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, closed.Key, IssuePatch{Status: str("Done")}); err != nil {
		t.Fatal(err)
	}
	other := a.project(a.admin, "Elsewhere", "")
	away := a.issue(a.admin, IssueInput{Project: other.Key, Title: "Away"})
	var many []string
	for i := 0; i < 6; i++ {
		many = append(many, a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "More", Agent: "claude"}).Key)
	}
	for _, tc := range []struct {
		also []string
		want string
	}{
		{[]string{closed.Key}, closed.Key + " is closed"},
		{[]string{away.Key}, away.Key + " isn't in " + a.p.Key},
		{[]string{"WP-9999"}, "WP-9999"},
		{many, "at most 6 tasks"},
	} {
		_, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Also = tc.also })
		if !IsInvalid(err) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "wp ") {
			t.Errorf("also %v: %v, want %q", tc.also, err, tc.want)
		}
	}
	if len(g.created) != 0 {
		t.Fatalf("a refused ship opened %d pull requests", len(g.created))
	}
}

func skipWhy(in *OpenPRInput) { in.Reason = "test" }

// A task shipped onto another's open pull request is linked to it, so the
// merge finishes it with the other and not before (WP-1426).
func TestShipOntoAnOpenPullRequestLinksTheTask(t *testing.T) {
	a, g := shipEnvOnForgeHost(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second"})
	res, err := a.ship(first.Key, ShipSkip, skipWhy)
	if err != nil || res.Pull == nil {
		t.Fatalf("first: %v", err)
	}
	again, err := a.ship(second.Key, ShipSkip, skipWhy)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 1 || !again.Existing || len(again.OtherTasks) != 1 || again.OtherTasks[0] != first.Key {
		t.Fatalf("second: created %d, %+v", len(g.created), again)
	}
	d, err := a.s.GetIssue(a.ctx, a.admin, second.Key)
	if err != nil || len(d.Links) != 1 || d.Links[0].URL != res.Pull.URL {
		t.Fatalf("%s isn't linked: %v %+v", second.Key, err, d.Links)
	}
	if d.Status.Category == CatDone {
		t.Errorf("%s is Done before the merge: %+v", second.Key, d.Status)
	}
}

// One review of the change, given every linked task's description: the round
// is the first task's (the one it was asked for), and the other tasks ride in
// its own record (beforePR.Also), so no review storage is re-keyed.
func TestReviewRoundCarriesEveryTasksDescription(t *testing.T) {
	a, _, _ := firstEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true), SpacingMinutes: ptrOf(120)}}); err != nil {
		t.Fatal(err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Description: "FIRST-NEEDS the widget to blink", Agent: "claude"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Description: "SECOND-NEEDS the gadget to hum", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Also = []string{second.Key} })
	if err != nil || res.Held == "" {
		t.Fatalf("held: %v %+v", err, res)
	}
	round := a.round(a.newestRoundID())
	text := a.s.roundContext(a.ctx, round)
	for _, want := range []string{"FIRST-NEEDS", "SECOND-NEEDS", second.Key} {
		if !strings.Contains(text, want) {
			t.Errorf("the review's context lacks %q:\n%s", want, text)
		}
	}
	if b := roundBeforePR(a.ctx, a.s.db.R, round.id); b == nil || !slices.Equal(b.Also, []string{second.Key}) {
		t.Errorf("before_pr = %+v", b)
	}
}

// shipEnvOnForgeHost is firstEnv with the forge at the host the fake forge's
// pull request URLs are on, so a link to one reads as a pull request link.
func shipEnvOnForgeHost(t *testing.T) (*apEnv, *shipForge) {
	t.Helper()
	a, _, g := firstEnv(t)
	var fid int64
	if err := a.s.db.R.QueryRow(`SELECT id FROM forges`).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.UpdateForge(a.ctx, a.admin, fid, ForgeInput{BaseURL: str("https://git.example.com")}); err != nil {
		t.Fatal(err)
	}
	return a, g
}

// A ship of A and B held for its review is opened by Waypoint after the
// review: the pull request is linked to both, both are In Review, and both
// are Done on the merge (WP-1426).
func TestHeldShipOfSeveralTasksLinksAndMovesEvery(t *testing.T) {
	a, g := shipEnvOnForgeHost(t)
	_, _ = a.s.db.W.Exec(`DELETE FROM runners WHERE name = 'auditbox'`)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Body, in.Also = "What changed.", []string{second.Key} })
	if err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	// The review can't run, so Waypoint opens the pull request itself.
	a.failJob(1, "failed", "tmux went away")
	a.tick(2 * time.Minute)
	if len(g.created) != 1 {
		t.Fatalf("created %d pull requests", len(g.created))
	}
	var pullURL string
	if err := a.s.db.R.QueryRow(`SELECT url FROM pull_requests`).Scan(&pullURL); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	for _, k := range []string{first.Key, second.Key} {
		d, err := a.s.GetIssue(a.ctx, a.admin, k)
		if err != nil || len(d.Links) != 1 || d.Links[0].URL != pullURL {
			t.Fatalf("%s links: %v %+v", k, err, d.Links)
		}
		if d.Status.Name != "In Review" {
			t.Errorf("%s is %s after the pull request opened, want In Review", k, d.Status.Name)
		}
	}
	a.s.applyLinkState(a.ctx, pullURL, &ForgeItem{IsPR: true, State: "merged", Title: "First"})
	a.s.Wait()
	for _, k := range []string{first.Key, second.Key} {
		d, err := a.s.GetIssue(a.ctx, a.admin, k)
		if err != nil || d.Status.Category != CatDone {
			t.Errorf("%s after the merge: %v %+v", k, err, d.Status)
		}
	}
}

// wp ship A B asks CheckShipTasks before it commits: it refuses what the open
// would refuse, a diagnose round among the extra tasks too, every key from
// one copy of the rules, and accepts exactly maxShipTasks.
func TestCheckShipTasksRefusesWhatTheOpenWould(t *testing.T) {
	o := newObjEnv(t)
	first := o.task("First")
	diag := o.toDiagnose(2, 2, ObjectiveInput{})
	closed := o.task("Closed")
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, closed.Key, IssuePatch{Status: str("Done")}); err != nil {
		t.Fatal(err)
	}
	var six []string
	for range maxShipTasks {
		six = append(six, o.task("More").Key)
	}
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{first.Key, diag.Key}, "diagnose round"},
		{[]string{first.Key, closed.Key}, "is closed"},
		{[]string{first.Key, o.p.Key + "-999"}, o.p.Key + "-999"},
		{append([]string{first.Key}, six...), "at most 6 tasks"},
	} {
		if err := o.s.CheckShipTasks(o.ctx, o.ag, o.p.Key, tc.keys); !IsInvalid(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.keys, err)
		}
	}
	if err := o.s.CheckShipTasks(o.ctx, o.ag, o.p.Key, six); err != nil {
		t.Errorf("exactly %d tasks: %v", maxShipTasks, err)
	}
}
