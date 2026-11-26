package core

import (
	"fmt"
	"testing"
)

func TestClientVersionFrom(t *testing.T) {
	for in, want := range map[string]string{
		"wp/v0.80.0":             "v0.80.0",
		" wp/0.25.1-3-gabc ":     "0.25.1-3-gabc",
		"wp/dev":                 "dev",
		"wp/":                    "",
		"wp/v1 (and a sentence)": "",
		"Mozilla/5.0":            "",
		"":                       "",
	} {
		if got := ClientVersionFrom(in); got != want {
			t.Errorf("ClientVersionFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

// What a run had of v0.80.0. The cases a release date gets wrong: a session
// that started before the deploy, wp older on that machine, hooks not running.
func TestRunFeatureState(t *testing.T) {
	const rel = "v0.80.0"
	run := func(inSession bool, sessionServer, sessionWP string, kv ...string) *runFacts {
		r := &runFacts{inSession: inSession, values: map[string]string{featureSessionServer: sessionServer, featureSessionWP: sessionWP}}
		for n := 0; n < len(kv); n += 2 {
			r.values[kv[n]] = kv[n+1]
		}
		return r
	}
	for _, c := range []struct {
		name string
		run  *runFacts
		want string
	}{
		{"nothing recorded", run(true, "", ""), FeatureUnknown},
		{"a dev build", run(true, "dev", "dev", featureServer, "dev", featureWP, "dev"), FeatureUnknown},
		{"everything had it", run(true, "v0.80.0", "v0.81.0", featureServer, "v0.81.0", featureWP, "v0.80.0"), FeatureHad},
		{"nothing did", run(true, "v0.79.0", "v0.79.0", featureServer, "v0.79.0", featureWP, "v0.79.0"), FeatureBefore},
		{"session from before the deploy", run(true, "v0.79.0", "v0.79.0", featureServer, "v0.80.0", featureWP, "v0.80.0"), FeaturePartial},
		{"wp older on that machine", run(true, "v0.80.0", "v0.79.0", featureServer, "v0.80.0", featureWP, "v0.79.0"), FeaturePartial},
		{"no hooks", run(false, "", "", featureServer, "v0.80.0", featureWP, "v0.80.0"), FeaturePartial},
		{"no hooks, before", run(false, "", "", featureServer, "v0.79.0", featureWP, "v0.79.0"), FeatureBefore},
		{"deployed during the run", run(true, "v0.79.0", "v0.79.0", featureServer, "v0.79.0", featureServerEnd, "v0.80.0", featureWP, "v0.79.0"), FeaturePartial},
		{"a web claim records no wp", run(true, "v0.80.0", "v0.80.0", featureServer, "v0.80.0"), FeatureHad},
		{"a session from before recording", run(true, "", "", featureServer, "v0.80.0", featureWP, "v0.80.0"), FeatureUnknown},
		{"a dev wp", run(true, "v0.80.0", "v0.80.0", featureServer, "v0.80.0", featureWP, "dev"), FeatureUnknown},
		{"a session on a dev wp", run(true, "v0.80.0", "dev", featureServer, "v0.80.0", featureWP, "v0.80.0"), FeatureUnknown},
		{"before, even with a dev wp", run(true, "v0.79.0", "v0.79.0", featureServer, "v0.79.0", featureWP, "dev"), FeatureBefore},
		{"moved to a later session", run(true, "v0.79.0", "v0.79.0", featureServer, "v0.80.0", featureSessionServerEnd, "v0.80.0", featureSessionWPEnd, "v0.80.0"), FeaturePartial},
	} {
		if got := c.run.state(rel); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	had, before := run(true, "v0.80.0", "v0.80.0", featureServer, "v0.80.0"), run(true, "v0.79.0", "v0.79.0", featureServer, "v0.79.0")
	if got := taskFeatureState([]*runFacts{had, had}, rel); got != FeatureHad {
		t.Errorf("two runs that had it: %s", got)
	}
	if got := taskFeatureState([]*runFacts{before, had}, rel); got != FeaturePartial {
		t.Errorf("runs on both sides: %s", got)
	}
	if got := taskFeatureState([]*runFacts{run(true, "", ""), had}, rel); got != FeaturePartial {
		t.Errorf("one run unrecorded: %s", got)
	}
	if got := taskFeatureState(nil, rel); got != FeatureUnknown {
		t.Errorf("no runs: %s", got)
	}
}

func (e *uaEnv) runFeatures(issue int64) map[string]string {
	e.t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT feature, value FROM run_features WHERE run_id = (SELECT max(id) FROM agent_runs WHERE issue_id = ?)`, issue)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			e.t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// A claim notes the versions, and the end adds only what changed.
func TestRunFeaturesRecordedAtClaimAndEnd(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.me, "Alpha", "kanban")
	i := e.issue(e.me, IssueInput{Project: p.Key, Title: "Search"})
	e.s.cfg.Version = "v0.80.0"
	ag := agentActor(e.me, "anthropic/claude-opus-5-5")
	ag.ClientVersion = "v0.79.0"
	if _, err := e.s.AgentTake(e.ctx, ag, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	got := e.runFeatures(i.ID)
	if got[featureServer] != "v0.80.0" || got[featureWP] != "v0.79.0" || got[featureServerEnd] != "" || got[featureWPEnd] != "" {
		t.Fatalf("after the claim: %v", got)
	}

	e.s.cfg.Version = "v0.81.0"
	ag.ClientVersion = "v0.81.0"
	if _, err := e.s.AgentDone(e.ctx, ag, i.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	got = e.runFeatures(i.ID)
	if got[featureServer] != "v0.80.0" || got[featureWP] != "v0.79.0" || got[featureServerEnd] != "v0.81.0" || got[featureWPEnd] != "v0.81.0" {
		t.Fatalf("after the end: %v", got)
	}

	same := e.issue(e.me, IssueInput{Project: p.Key, Title: "Export"})
	if _, err := e.s.AgentDone(e.ctx, ag, same.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	got = e.runFeatures(same.ID)
	if got[featureServer] != "v0.81.0" || got[featureServerEnd] != "" || got[featureWPEnd] != "" {
		t.Fatalf("a run that never claimed: %v", got)
	}
}

// A run that was already open when recording began isn't given the versions
// of its end as its start.
func TestRunFeaturesNotBackdated(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.me, "Alpha", "kanban")
	i := e.issue(e.me, IssueInput{Project: p.Key, Title: "Search"})
	e.s.cfg.Version = "v0.80.0"
	ag := agentActor(e.me, "anthropic/claude-opus-5-5")
	ag.ClientVersion = "v0.80.0"
	if _, err := e.s.AgentTake(e.ctx, ag, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `DELETE FROM run_features`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, ag, i.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if got := e.runFeatures(i.ID); len(got) != 0 {
		t.Fatalf("the end was recorded as the start: %v", got)
	}
}

// A pause, a deploy under a run that then lapses, and a move to a later
// session all leave their mark.
func TestRunFeaturesChangesWithoutAFinish(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.me, "Alpha", "kanban")
	e.s.cfg.Version = "v0.79.0"
	ag := agentActor(e.me, "anthropic/claude-opus-5-5")
	ag.ClientVersion = "v0.79.0"

	paused := e.issue(e.me, IssueInput{Project: p.Key, Title: "Paused"})
	if _, err := e.s.AgentTake(e.ctx, ag, paused.Key, ""); err != nil {
		t.Fatal(err)
	}
	lapsed := e.issue(e.me, IssueInput{Project: p.Key, Title: "Lapsed"})
	if _, err := e.s.AgentTake(e.ctx, ag, lapsed.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.s.cfg.Version = "v0.80.0"
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.runFeatures(lapsed.ID); got[featureServer] != "v0.79.0" || got[featureServerEnd] != "v0.80.0" {
		t.Errorf("open across the deploy: %v", got)
	}
	ag.ClientVersion = "v0.80.0"
	if _, err := e.s.AgentPause(e.ctx, ag, paused.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.runFeatures(paused.ID); got[featureServerEnd] != "v0.80.0" || got[featureWPEnd] != "v0.80.0" {
		t.Errorf("paused: %v", got)
	}

	moved := e.issue(e.me, IssueInput{Project: p.Key, Title: "Moved"})
	started := func(cli, version string) int64 {
		sess := e.session(e.me, "m1", cli, 0)
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE agent_sessions SET started_server = ?, started_wp = ? WHERE id = ?`, version, version, sess); err != nil {
			t.Fatal(err)
		}
		return sess
	}
	ag.Session = started("old", "v0.79.0")
	if _, err := e.s.AgentTake(e.ctx, ag, moved.Key, ""); err != nil {
		t.Fatal(err)
	}
	ag.Session = started("new", "v0.80.0")
	if _, err := e.s.AgentTake(e.ctx, ag, moved.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.runFeatures(moved.ID); got[featureSessionServer] != "v0.79.0" || got[featureSessionServerEnd] != "v0.80.0" || got[featureSessionWPEnd] != "v0.80.0" {
		t.Errorf("moved session: %v", got)
	}
}

// Each run keeps the Claude Code it had, whatever the session updates to.
func TestRunKeepsItsCLIVersion(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.me, "Alpha", "kanban")
	e.s.cfg.Version = "v0.80.0"
	ag := agentActor(e.me, "anthropic/claude-opus-5-5")
	ag.Session = e.session(e.me, "m1", "s1", 0)
	version := func(v string) {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO usage_sessions(user_id, machine, session, version) VALUES (?, 'm1', 's1', ?)
			ON CONFLICT(user_id, machine, session) DO UPDATE SET version = excluded.version`, e.me.id(), v); err != nil {
			t.Fatal(err)
		}
	}
	version("2.1.0")
	i := e.issue(e.me, IssueInput{Project: p.Key, Title: "Search"})
	if _, err := e.s.AgentTake(e.ctx, ag, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	version("2.1.9")
	o, err := e.s.UsageForTask(e.ctx, e.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Runs[0].Features[featureCLIVersion]; got != "2.1.0" {
		t.Errorf("cli_version = %s", got)
	}
}

type featureEnv struct {
	*uaEnv
	p   *Project
	env int64
	n   int
	fg  *Forge
}

// newFeatureEnv is a server on v0.80.0, live since hour -50.
func newFeatureEnv(t *testing.T) *featureEnv {
	e := newAnalyticsEnv(t)
	f := &featureEnv{uaEnv: e, p: e.project(e.me, "Alpha", "kanban")}
	e.s.cfg.Version = "v0.80.0"
	r, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO environments(project_id, name, kind, created_at, updated_at) VALUES (?, 'production', 'production', 0, 0)`, f.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.env, _ = r.LastInsertId()
	f.deploy("v0.79.0", -100)
	f.deploy("v0.80.0", -50)
	return f
}

func (f *featureEnv) deploy(version string, hour int) { f.rollout(version, hour, hour) }

func (f *featureEnv) rollout(version string, started, finished int) {
	f.t.Helper()
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO deployments(environment_id, version, status, started_at, finished_at) VALUES (?, ?, 'succeeded', ?, ?)`,
		f.env, version, f.hour(started), f.hour(finished)); err != nil {
		f.t.Fatal(err)
	}
}

// mark adds what the issue's newest run had.
func (f *featureEnv) mark(issue int64, kv ...string) {
	f.t.Helper()
	for n := 0; n < len(kv); n += 2 {
		if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT OR REPLACE INTO run_features(run_id, feature, value, recorded_at)
			SELECT max(id), ?, ?, 0 FROM agent_runs WHERE issue_id = ?`, kv[n], kv[n+1], issue); err != nil {
			f.t.Fatal(err)
		}
	}
}

// task records one task's run at an hour, as recorded by the versions given
// (server "" records nothing), using `tokens` of input.
func (f *featureEnv) task(hour int, server, session string, tokens int64) *IssueDetail {
	f.t.Helper()
	f.n++
	i := f.issue(f.me, IssueInput{Project: f.p.Key, Title: fmt.Sprintf("Task %d", f.n)})
	cli := fmt.Sprintf("s%d", f.n)
	sess := f.session(f.me, "m1", cli, 0)
	f.run(sess, i.ID, f.hour(hour), f.hour(hour+1))
	if server != "" {
		f.mark(i.ID, featureServer, server, featureSessionServer, session, featureSessionWP, session)
	}
	f.ingest(f.me, "chunk-"+cli, "m1", UsageChunk{Messages: []UsageMessageRow{msg(f.hour(hour), cli, "acme/web", "claude-opus-5-5", "main", 3, tokens, 10, 0, 0, 0)}})
	return i
}

func stat(t *testing.T, o *UsageFeature, key string) CompareStat {
	t.Helper()
	for _, s := range o.Stats {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no %s in %+v", key, o.Stats)
	return CompareStat{}
}

// Tasks are sorted by what their runs had, even where that disagrees with
// the release's date.
func TestUsageFeatureComparesByWhatRunsHad(t *testing.T) {
	f := newFeatureEnv(t)
	for n := 0; n < 5; n++ {
		f.task(-80+n, "v0.79.0", "v0.79.0", 1000)
		f.task(-40+n, "v0.80.0", "v0.80.0", 3000)
	}
	stale := f.task(-30, "v0.80.0", "v0.79.0", 900000)
	old := f.task(-90, "", "", 900000)

	o, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: "0.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Release != "v0.80.0" || o.LiveAt.UnixMilli() != f.hour(-50) || o.Window != 30 || o.DaysAfter != 3 {
		t.Errorf("release = %s at %v, window %d, %d days after", o.Release, o.LiveAt, o.Window, o.DaysAfter)
	}
	if o.Counts[FeatureHad] != 5 || o.Counts[FeatureBefore] != 5 || o.Counts[FeaturePartial] != 1 || o.Counts[FeatureUnknown] != 1 {
		t.Errorf("counts = %v", o.Counts)
	}
	s := stat(t, o, "tokens")
	if s.Before.N != 5 || s.After.N != 5 || s.Before.Median != 1010 || s.After.Median != 3010 {
		t.Errorf("tokens = before %+v, after %+v", s.Before, s.After)
	}
	states := map[string]string{}
	for _, row := range o.Tasks {
		states[row.Key] = row.State
	}
	if states[stale.Key] != FeaturePartial || states[old.Key] != FeatureUnknown {
		t.Errorf("states = %v", states)
	}
	if o.TaskRows != 12 || o.Tasks[0].Key != old.Key {
		t.Errorf("%d rows, first %+v", o.TaskRows, o.Tasks[0])
	}
}

func (f *featureEnv) mergedPR(issue int64, number, hour int) {
	f.t.Helper()
	if f.fg == nil {
		f.fg = f.forge("github", "", false)
	}
	url := fmt.Sprintf("https://x/%d", number)
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, additions, deletions, changed_files, created_at, updated_at, merged_at)
		VALUES (?, 'acme/web', ?, ?, 'merged', 30, 12, 3, 0, 0, ?)`, f.fg.ID, number, url, f.hour(hour)); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'github', 'pull', ?, 'acme/web', ?, 'merged', 0)`,
		issue, url, number); err != nil {
		f.t.Fatal(err)
	}
}

// Whose usage is compared decides whose runs count.
func TestUsageFeatureOnlyJudgesTheRunsAskedAbout(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-40, "v0.80.0", "v0.80.0", 1000)
	f.mark(i.ID, featureWP, "v0.80.0")
	sam := f.user("sam", false)
	f.run(f.session(sam, "m2", "sx", 0), i.ID, f.hour(-39), f.hour(-38))
	f.mark(i.ID, featureServer, "v0.79.0", featureSessionServer, "v0.79.0", featureSessionWP, "v0.79.0")

	mine, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: "v0.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	if mine.Counts[FeatureHad] != 1 || mine.Counts[FeaturePartial] != 0 {
		t.Errorf("mine = %v", mine.Counts)
	}
	all, err := f.s.UsageFeature(f.ctx, f.admin, UsageQuery{User: "all"}, FeatureQuery{Ref: "v0.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	if all.Counts[FeaturePartial] != 1 {
		t.Errorf("all = %v", all.Counts)
	}
}

// Two products can both have released a version called the same.
func TestFeatureReleaseWithAVersionTwoProjectsShare(t *testing.T) {
	f := newFeatureEnv(t)
	shipped := f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Shipped"})
	f.mergedPR(shipped.ID, 7, -60)
	other := f.project(f.me, "Other", "kanban")
	r, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO environments(project_id, name, kind, position, created_at, updated_at) VALUES (?, 'production', 'production', -1, 0, 0)`, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := r.LastInsertId()
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO deployments(environment_id, version, status, started_at, finished_at) VALUES (?, 'v0.80.0', 'succeeded', ?, ?)`, env, f.hour(-55), f.hour(-55)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: "v0.80.0"}); !IsInvalid(err) {
		t.Errorf("a version two projects deployed: %v", err)
	}
	o, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: shipped.Key})
	if err != nil {
		t.Fatal(err)
	}
	if o.LiveAt.UnixMilli() != f.hour(-50) {
		t.Errorf("live at %v, the other project's", o.LiveAt)
	}
}

func TestFeatureReleaseFromTask(t *testing.T) {
	f := newFeatureEnv(t)
	shipped := f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Shipped"})
	f.mergedPR(shipped.ID, 7, -60)
	f.rollout("v0.79.5", -62, -58)
	f.mergedPR(shipped.ID, 9, -20)
	f.deploy("v0.81.0", -15)

	o, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: shipped.Key, Window: 500})
	if err != nil {
		t.Fatal(err)
	}
	if o.Release != "v0.80.0" || o.Title != "Shipped" || o.Window != 90 {
		t.Errorf("release = %s, title %q, window %d", o.Release, o.Title, o.Window)
	}

	waiting := f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Waiting"})
	f.mergedPR(waiting.ID, 8, -10)
	unmerged := f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Unmerged"})
	elsewhere := f.project(f.me, "Other", "kanban")
	foreign := f.issue(f.me, IssueInput{Project: elsewhere.Key, Title: "Other product"})
	for ref, why := range map[string]string{"": "no ref", waiting.Key: "merged, not deployed", unmerged.Key: "not merged", foreign.Key: "another project", "v9.9.9": "never deployed"} {
		if _, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: ref}); !IsInvalid(err) {
			t.Errorf("%s (%q): %v", why, ref, err)
		}
	}

	f.s.cfg.Version = "dev"
	if _, err := f.s.UsageFeature(f.ctx, f.me, UsageQuery{}, FeatureQuery{Ref: "v0.80.0"}); !IsInvalid(err) {
		t.Errorf("on a dev build: %v", err)
	}
}

func TestUsageForTask(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-10, "v0.80.0", "v0.79.0", 500)
	other := f.task(-5, "v0.80.0", "v0.80.0", 700)
	f.mergedPR(i.ID, 7, -8)
	f.ingest(f.me, "tools", "m1", UsageChunk{Tools: []UsageToolRow{
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Edit", Calls: 4, Errors: 1},
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Read", Calls: 9},
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "skill", Tool: "waypoint:pr-workflow", Calls: 1},
		{Hour: f.hour(-5), Session: "s2", Repo: "acme/web", Kind: "builtin", Tool: "Edit", Calls: 50},
	}})
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO experiment_runs(experiment, weights, started_at) VALUES ('prompts', '{}', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO experiment_units(run_id, issue_id, arm, assigned_at) SELECT id, ?, 'short', 0 FROM experiment_runs WHERE experiment = 'prompts'`, i.ID); err != nil {
		t.Fatal(err)
	}

	f.mergedPR(i.ID, 9, -7)
	if _, err := f.s.db.W.ExecContext(f.ctx, `UPDATE pull_requests SET deletions = NULL WHERE number = 9`); err != nil {
		t.Fatal(err)
	}

	o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Runs) != 1 {
		t.Fatalf("runs = %+v", o.Runs)
	}
	got := o.Runs[0].Features
	if got["server"] != "v0.80.0" || got["session_server"] != "v0.79.0" || got["hooks"] != "on" {
		t.Errorf("features = %v", got)
	}
	if o.Input != 500 || o.ToolCalls != 13 || o.ToolErrors != 1 || o.Edits != 3 || o.ActiveHours != 1 || o.Sessions != 1 || o.RunMinutes != 60 {
		t.Errorf("totals = %+v", o)
	}
	if o.PRs != 2 || o.Files != 6 || o.Lines != 72 {
		t.Errorf("prs %d files %d lines %d", o.PRs, o.Files, o.Lines)
	}
	if len(o.Arms) != 0 {
		t.Errorf("a non-admin saw arms %v", o.Arms)
	}
	if o, err := f.s.UsageForTask(f.ctx, f.admin, UsageQuery{}, i.Key); err != nil || o.Arms["prompts"] != "short" {
		t.Errorf("admin: %+v, %v", o, err)
	}
	if len(o.Used) != 3 || o.Used[0].Name != "Read" || o.Used[0].Calls != 9 {
		t.Errorf("used = %+v", o.Used)
	}

	if o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, other.Key); err != nil || o.Edits != 50 || o.Input != 700 {
		t.Errorf("other task = %+v, %v", o, err)
	}
	sam := f.user("sam", false)
	if _, err := f.s.UsageForTask(f.ctx, sam, UsageQuery{}, i.Key); err == nil {
		t.Error("a stranger read the task's usage")
	}
	if err := f.s.SetMember(f.ctx, f.me, f.p.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	if o, err := f.s.UsageForTask(f.ctx, sam, UsageQuery{}, i.Key); err != nil || len(o.Arms) != 0 {
		t.Errorf("a member: arms %v, %v", o, err)
	}
}

// A refused call wrote nothing (a hook block, a denial, a rejection): it is
// not an edit (audit 1907.5). A shell edit that exited non-zero still counts,
// as `sed -i ... && go test` writes before the test fails; an edit-tool call
// that failed does not.
func TestUsageForTaskEditsLeaveOutRefused(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-10, "v0.80.0", "v0.79.0", 500)
	f.ingest(f.me, "tools", "m1", UsageChunk{Tools: []UsageToolRow{
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Edit", Calls: 1, Refusals: 1},
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Write", Calls: 4, Errors: 1, Refusals: 1},
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "shell_edit", Tool: "Bash", Calls: 1, Refusals: 1},
		{Hour: f.hour(-9), Session: "s1", Repo: "acme/web", Kind: "shell_edit", Tool: "Bash", Calls: 1, Errors: 1},
	}})
	o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if o.Edits != 2 || o.ShellEdits != 1 {
		t.Errorf("edits %d, shell edits %d; want 2 and 1", o.Edits, o.ShellEdits)
	}
}

// OTLP counts a rejected tool_decision as calls 0, refusals 1, beside the
// tool_result events (calls 1): a refusal is not inside the calls there, so
// five edit results and two rejects are five edits (audit 1933.9).
func TestUsageForTaskOTLPRefusalsAreNotInCalls(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-10, "v0.80.0", "v0.79.0", 500)
	// The two kinds of event add into one row of the hour (the key has no
	// event kind): calls 5 from the results, refusals 2 from the rejects.
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO tool_hours(user_id, hour, machine, session, repo, model, kind, tool, detail, via, calls, errors, refusals)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.me.User.ID, f.hour(-10), "m1", "s1", "acme/web", "", "builtin", "Edit", "", "otlp", 5, 0, 2); err != nil {
		t.Fatal(err)
	}
	o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if o.Edits != 5 {
		t.Errorf("edits %d; want 5 (the two rejects are not subtracted from the results)", o.Edits)
	}
}

// Bash calls that wrote files are counted per hour the client marked; the
// Bash calls of hours it did not mark leave the figure a floor, not zero.
func TestUsageForTaskShellEdits(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-10, "v0.80.0", "v0.79.0", 500)
	f.ingest(f.me, "tools", "m1", UsageChunk{Tools: []UsageToolRow{
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Bash", Calls: 20, Errors: 2},
		{Hour: f.hour(-10), Session: "s1", Repo: "acme/web", Kind: "shell_edit", Tool: "Bash", Calls: 7, Errors: 1},
		{Hour: f.hour(-9), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Bash", Calls: 5},
	}})
	o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if o.ShellEdits != 7 || o.ShellEditsUncounted != 5 {
		t.Errorf("shell edits %d, uncounted Bash calls %d; want 7 (a failed one still wrote) and 5 (the unmarked hour)", o.ShellEdits, o.ShellEditsUncounted)
	}
	for _, u := range o.Used {
		if u.Kind == "shell_edit" {
			t.Errorf("the marker row is listed as a tool: %+v", u)
		}
	}
}
