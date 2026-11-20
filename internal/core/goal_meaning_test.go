package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// judgeLLM answers each prompt with fn, and keeps the prompts it was asked.
type judgeLLM struct {
	mu      sync.Mutex
	prompts []string
	fn      func(prompt string) (string, error)
}

func (j *judgeLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (j *judgeLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	j.mu.Lock()
	j.prompts = append(j.prompts, prompt)
	fn := j.fn
	j.mu.Unlock()
	ans, err := fn(prompt)
	return json.RawMessage(ans), "", err
}

// count is how many prompts held marker.
func (j *judgeLLM) count(marker string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, p := range j.prompts {
		if strings.Contains(p, marker) {
			n++
		}
	}
	return n
}

func (j *judgeLLM) since(n int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.prompts[n:]...)
}

const (
	overlapMarker   = "How much do these two goals overlap"
	restatesMarker  = "Does goal A say anything beyond"
	coverMarker     = "Does this task text already convey"
	constructMarker = "Write four short texts"
)

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var quotedRE = regexp.MustCompile(`(?s): "([^"]*)"`)

// grader answers the judgement prompts with grade(question, a, b) and the
// case-building prompt with four fixed cases.
func grader(grade func(j, a, b string) int) func(string) (string, error) {
	return func(prompt string) (string, error) {
		if strings.Contains(prompt, constructMarker) {
			m := quotedRE.FindStringSubmatch(prompt)
			return jsonOf(map[string]string{"paraphrase": "In other words: " + m[1], "trap": m[1] + " is not wanted",
				"narrower": m[1] + " on phones", "sibling": "Servers restart quickly"}), nil
		}
		m := quotedRE.FindAllStringSubmatch(prompt, 2)
		if len(m) < 2 {
			return "", errors.New("no pair in the prompt")
		}
		j := judgeCover
		switch {
		case strings.Contains(prompt, overlapMarker):
			j = judgeOverlap
		case strings.Contains(prompt, restatesMarker):
			j = judgeRestates
		}
		return jsonOf(map[string]any{"score": grade(j, m[0][1], m[1][1]), "why": "the texts say so"}), nil
	}
}

// useJudge gives the goals task a model that answers with fn.
func (e *goalCtxEnv) useJudge(fn func(prompt string) (string, error)) *judgeLLM {
	e.t.Helper()
	j := &judgeLLM{fn: fn}
	e.quiet()
	e.s.LLM = j
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		e.t.Fatal(err)
	}
	e.s.goalsSettle(e.ctx)
	return j
}

// quiet waits out the trace and judgement passes the test's own writes
// started, so it can swap the models they read without a data race; a pass
// started after the swap sees the new value.
func (e *goalCtxEnv) quiet() {
	e.s.quiesceGoals(e.t)
}

// useMeaning gives the goals task the meaning service m.
func (e *goalCtxEnv) useMeaning(m MeaningModels) {
	e.quiet()
	e.s.Meaning = m
}

// meaning runs the judgements to rest, after any the background started.
func (e *goalCtxEnv) meaning() {
	e.t.Helper()
	e.quiet()
	for range 50 {
		if !e.s.refreshMeaning(e.ctx) {
			return
		}
	}
	e.t.Fatal("the judgements never came to rest")
}

// fakeMeaning is the meaning service: vectors from the words, entailment
// from nli.
type fakeMeaning struct {
	mu    sync.Mutex
	texts int
	nli   func(premise, hypothesis string) float64
}

func (f *fakeMeaning) Embed(ctx context.Context, texts []string) (string, [][]float32, error) {
	f.mu.Lock()
	f.texts += len(texts)
	f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 16)
		for _, w := range meaningWordRE.FindAllString(strings.ToLower(t), -1) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%16]++
		}
		out[i] = v
	}
	return "e5@test", out, nil
}

func (f *fakeMeaning) Entail(ctx context.Context, pairs []NLIPair) (string, []float64, error) {
	out := make([]float64, len(pairs))
	for i, p := range pairs {
		out[i] = 0.1
		if f.nli != nil {
			out[i] = f.nli(p.Premise, p.Hypothesis)
		}
	}
	return "nli@test", out, nil
}

func (f *fakeMeaning) embedded() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.texts
}

func (e *goalCtxEnv) assignedCovered(id int64) *string {
	e.t.Helper()
	var c *string
	if err := e.s.db.R.QueryRow(`SELECT covered FROM goal_context_assignments WHERE issue_id = ?`, id).Scan(&c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *goalCtxEnv) coverJudged(id int64) int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM goal_meaning_pairs WHERE judgement = 'cover' AND a_id = ?`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func chainIDs(g *IssueGoal) []int64 {
	var out []int64
	for _, s := range g.Chain {
		out = append(out, s.ID)
	}
	return out
}

func (e *goalCtxEnv) wording(o *GoalsOverview, id int64) string {
	e.t.Helper()
	for _, g := range o.Goals {
		if g.Note.ID == id {
			return strings.Join(g.Wording, " | ")
		}
	}
	e.t.Fatalf("goal %d missing", id)
	return ""
}

// coversTop grades only the top goal as conveyed by a task.
func (e *goalCtxEnv) coversTop() func(string) (string, error) {
	return grader(func(j, a, b string) int {
		if j == judgeCover && b == e.top.Title {
			return 3
		}
		return 0
	})
}

// The pruned arm leaves out a parent judged conveyed, though it shares no
// word with the task, and shows one that shares most of its words with the
// task but was judged not conveyed (WP-ADR133).
func TestMeaningPrunedUsesJudgement(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	e.useJudge(e.coversTop())
	e.meaning()
	if n := e.coverJudged(i.ID); n != 2 {
		t.Fatalf("%d coverage judgements, want one per parent", n)
	}
	e.take(e.ag, i.ID, "next")
	if c := e.assignedCovered(i.ID); c == nil || *c != fmt.Sprintf("[%d]", e.top.ID) {
		t.Fatalf("recorded covered = %v", c)
	}
	e.exec(`UPDATE goal_context_assignments SET arm = 'pruned' WHERE issue_id = ?`, i.ID)
	g := e.task(e.ag, i.ID).Issue.Goal
	if ids := chainIDs(g); len(ids) != 1 || ids[0] != e.mid.ID || g.Above != 1 {
		t.Fatalf("pruned shows %v above %d, want only %d", ids, g.Above, e.mid.ID)
	}
}

// Every parent judged covered: pruned still differs from goal, by the count
// of parents left out, so it is a candidate and shows that count.
func TestMeaningPrunedAllCovered(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	e.useJudge(grader(func(j, _, _ string) int {
		if j == judgeCover {
			return 3
		}
		return 0
	}))
	e.meaning()
	e.take(e.ag, i.ID, "next")
	var cands string
	if err := e.s.db.R.QueryRow(`SELECT candidates FROM goal_context_assignments WHERE issue_id = ?`, i.ID).Scan(&cands); err != nil {
		t.Fatal(err)
	}
	if cands != "goal,chain,pruned" {
		t.Errorf("candidates = %q", cands)
	}
	e.exec(`UPDATE goal_context_assignments SET arm = 'pruned' WHERE issue_id = ?`, i.ID)
	if g := e.task(e.ag, i.ID).Issue.Goal; len(g.Chain) != 0 || g.Above != 2 {
		t.Errorf("pruned shows %v above %d, want none above 2", chainIDs(g), g.Above)
	}
}

// With no judgement nothing is left out and covered is NULL; one made
// after the draw doesn't change what the drawn task shows.
func TestMeaningNoJudgementShowsAll(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	e.take(e.ag, i.ID, "next")
	if c := e.assignedCovered(i.ID); c != nil {
		t.Fatalf("covered = %q, want NULL", *c)
	}
	e.exec(`UPDATE goal_context_assignments SET arm = 'pruned' WHERE issue_id = ?`, i.ID)
	if ids := chainIDs(e.task(e.ag, i.ID).Issue.Goal); len(ids) != 2 {
		t.Fatalf("no judgement: shown %v", ids)
	}
	e.useJudge(grader(func(j, a, b string) int { return 3 }))
	e.meaning()
	if e.coverJudged(i.ID) == 0 {
		t.Fatal("no judgement stored")
	}
	g := e.task(e.ag, i.ID).Issue.Goal
	if ids := chainIDs(g); len(ids) != 2 || g.Above != 0 {
		t.Fatalf("a later judgement changed the drawn task: %v above %d", ids, g.Above)
	}
}

// A judgement made on another text of the task, or of a parent, counts as
// none.
func TestMeaningCoverageStale(t *testing.T) {
	e := newGoalCtxEnv(t)
	text := e.serving("Make navigation feel predictable", e.leaf)
	fresh := e.serving("Make the sidebar feel predictable", e.leaf)
	e.useJudge(e.coversTop())
	e.meaning()
	e.exec(`UPDATE issues SET description = 'Now about something else' WHERE id = ?`, text.ID)
	for _, c := range []struct {
		i    *IssueDetail
		want bool
	}{{text, false}, {fresh, true}} {
		e.take(e.ag, c.i.ID, "next")
		if got := e.assignedCovered(c.i.ID) != nil; got != c.want {
			t.Errorf("%s: judgement used = %v, want %v", c.i.Key, got, c.want)
		}
	}
	// A parent reworded: the judgement of the task against it is stale.
	// (Judged at the old title, then no model can judge the new one.)
	other := e.serving("Make the toolbar feel predictable", e.leaf)
	e.meaning()
	e.useJudge(func(string) (string, error) { return "", errors.New("the model is down") })
	e.exec(`UPDATE design_notes SET title = 'Customers enjoy a calm interface' WHERE id = ?`, e.top.ID)
	e.meaning()
	e.take(e.ag, other.ID, "next")
	if c := e.assignedCovered(other.ID); c != nil {
		t.Errorf("a stale parent's judgement was used: %q", *c)
	}
}

// The Goals view's suggestions come from stored judgements: goals that
// share most words but were judged apart get none; a title edit drops a
// pair; a read asks no model.
func TestMeaningPairsFromJudgements(t *testing.T) {
	e := newGoalCtxEnv(t)
	parent := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	repeat := e.child(e.admin, parent, "People can get reports into a sheet")
	twin := e.goal(e.admin, e.p.Key, "Finance staff download figures as workbooks")
	lookalike := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets quickly")
	loner := e.goal(e.admin, e.p.Key, "Mobile screens stay readable")
	j := e.useJudge(grader(func(j, a, b string) int {
		switch {
		case j == judgeRestates && a == repeat.Title:
			return 3
		case j == judgeOverlap && (a == twin.Title || b == twin.Title) && (a == parent.Title || b == parent.Title):
			return 2
		}
		return 0
	}))
	e.meaning()
	before := j.count("")
	o := e.goals(e.admin, e.p.Key)
	if j.count("") != before {
		t.Error("reading the Goals view asked a model")
	}
	if w := e.wording(o, repeat.ID); !strings.Contains(w, fmt.Sprintf("repeats its parent goal #%d (“Reports export cleanly to spreadsheets”)", parent.ID)) ||
		!strings.Contains(w, "fold it into that goal") || !strings.Contains(w, "the texts say so") {
		t.Errorf("restates = %q", w)
	}
	if w := e.wording(o, twin.ID); !strings.Contains(w, fmt.Sprintf("overlaps goal #%d", parent.ID)) || !strings.Contains(w, "merge them, or say how they differ") {
		t.Errorf("overlap = %q", w)
	}
	if w := e.wording(o, parent.ID); strings.Contains(w, "overlaps") || strings.Contains(w, "repeats") {
		t.Errorf("the older goal is left alone: %q", w)
	}
	for _, id := range []int64{lookalike.ID, loner.ID, e.top.ID, e.mid.ID, e.leaf.ID} {
		if w := e.wording(o, id); w != "" {
			t.Errorf("goal %d flagged without a judgement: %q", id, w)
		}
	}
	// What a person was shown is kept, for the labels.
	var shown int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM goal_meaning_shown`).Scan(&shown)
	if shown != 2 {
		t.Errorf("%d suggestions recorded as shown, want 2", shown)
	}
	// A title edit makes the pair's judgement stale.
	e.exec(`UPDATE design_notes SET title = 'Finance staff download figures as workbooks, weekly' WHERE id = ?`, twin.ID)
	if w := e.wording(e.goals(e.admin, e.p.Key), twin.ID); w != "" {
		t.Errorf("edited goal still flagged: %q", w)
	}
}

// With no model and no meaning service nothing is suggested, nothing is
// hidden, and nothing fails.
func TestMeaningNoScorers(t *testing.T) {
	e := newGoalCtxEnv(t)
	e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	e.goal(e.admin, e.p.Key, "Reports export to spreadsheets")
	i := e.serving("Make navigation feel predictable", e.leaf)
	e.meaning()
	for _, g := range e.goals(e.admin, e.p.Key).Goals {
		if len(g.Wording) != 0 {
			t.Errorf("goal %d: %v", g.Note.ID, g.Wording)
		}
	}
	if e.coverJudged(i.ID) != 0 {
		t.Error("a coverage judgement with no scorer")
	}
}

// With the meaning service and no language model, the prior decides
// overlaps by NLI; coverage, which only the language model judges well, is
// left undecided.
func TestMeaningNLIAlone(t *testing.T) {
	e := newGoalCtxEnv(t)
	a := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	b := e.goal(e.admin, e.p.Key, "Finance staff download figures as workbooks")
	e.goal(e.admin, e.p.Key, "Mobile screens stay readable")
	i := e.serving("Make navigation feel predictable", e.leaf)
	fm := &fakeMeaning{nli: func(p, h string) float64 {
		if (p == a.Title && h == b.Title) || (p == b.Title && h == a.Title) {
			return 0.9
		}
		return 0.1
	}}
	e.useMeaning(fm)
	e.meaning()
	o := e.goals(e.admin, e.p.Key)
	if w := e.wording(o, b.ID); !strings.Contains(w, fmt.Sprintf("overlaps goal #%d", a.ID)) || strings.Contains(w, "the goals model") {
		t.Errorf("overlap by NLI = %q", w)
	}
	if e.coverJudged(i.ID) != 0 {
		t.Error("coverage decided without the language model")
	}
	// Only new text is embedded again.
	n := fm.embedded()
	e.meaning()
	if fm.embedded() != n+1 { // the probe
		t.Errorf("embedded %d texts again with nothing new", fm.embedded()-n-1)
	}
	e.exec(`UPDATE design_notes SET title = 'Mobile screens stay easy to read' WHERE title = 'Mobile screens stay readable'`)
	n = fm.embedded()
	e.meaning()
	if got := fm.embedded() - n - 1; got != 1 {
		t.Errorf("a reworded goal embedded %d texts, want 1", got)
	}
}

// Only pairs that hold new or reworded text are asked again.
func TestMeaningOnlyNewText(t *testing.T) {
	e := newGoalCtxEnv(t)
	e.serving("Make navigation feel predictable", e.leaf)
	other := e.goal(e.admin, e.p.Key, "Mobile screens stay readable")
	j := e.useJudge(grader(func(string, string, string) int { return 0 }))
	e.meaning()
	n := j.count("")
	if j.count(overlapMarker) == 0 || j.count(restatesMarker) == 0 || j.count(coverMarker) == 0 || j.count(constructMarker) == 0 {
		t.Fatalf("not every question asked: %d prompts", n)
	}
	e.meaning()
	if j.count("") != n {
		for _, p := range j.since(n)[:5] {
			t.Logf("asked: %q", p)
		}
		t.Fatalf("nothing changed and the model was asked %d times", j.count("")-n)
	}
	e.exec(`UPDATE design_notes SET title = 'Mobile screens stay easy to read' WHERE id = ?`, other.ID)
	e.meaning()
	again := j.since(n)
	if len(again) == 0 {
		t.Fatal("a reworded goal wasn't asked about")
	}
	for _, p := range again {
		if !strings.Contains(p, "Mobile screens stay easy to read") {
			t.Errorf("asked about a pair that didn't change: %q", p)
		}
	}
}

// A failed call is kept, not repeated before its back-off, and never
// decides anything.
func TestMeaningBackoff(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	j := e.useJudge(func(string) (string, error) { return "", errors.New("the model is down") })
	e.meaning()
	n := j.count(coverMarker)
	if n == 0 {
		t.Fatal("not asked")
	}
	var errText string
	if err := e.s.db.R.QueryRow(`SELECT error FROM meaning_scores WHERE scorer = 'llm:cover:v1' LIMIT 1`).Scan(&errText); err != nil || errText == "" {
		t.Fatalf("failure not kept: %q %v", errText, err)
	}
	if e.coverJudged(i.ID) != 0 {
		t.Error("a failed call decided")
	}
	e.meaning()
	if j.count(coverMarker) != n {
		t.Error("asked again before the back-off ended")
	}
	e.setClock(e.clockNow().Add(2*meaningRetryBase + time.Minute))
	e.meaning()
	if j.count(coverMarker) <= n {
		t.Error("not asked again after the back-off")
	}
	// The pass that failed set a timer for the next, so a retry doesn't
	// wait for a goal to change.
	e.s.goals.mu.Lock()
	timer := e.s.goals.judgeTimer
	e.s.goals.mu.Unlock()
	if timer == nil {
		t.Error("no pass set for the end of the back-off")
	}
}

func (e *goalCtxEnv) labels(source string) []meaningLabel {
	e.t.Helper()
	rows, err := e.s.db.R.Query(`SELECT judgement, a_hash, b_hash, y, ref FROM meaning_labels WHERE source = ? ORDER BY id`, source)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []meaningLabel
	for rows.Next() {
		var l meaningLabel
		if err := rows.Scan(&l.judgement, &l.aHash, &l.bHash, &l.y, &l.ref); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// What people did with a suggestion labels it: a goal reworded is acted
// on, a dismissal is a no, and a suggestion left 30 days is ignored.
func TestMeaningShownLabels(t *testing.T) {
	e := newGoalCtxEnv(t)
	parent := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	repeat := e.child(e.admin, parent, "People can get reports into a sheet")
	twin := e.goal(e.admin, e.p.Key, "Finance staff download figures as workbooks")
	third := e.goal(e.admin, e.p.Key, "Accountants pull numbers into Excel")
	e.useJudge(grader(func(j, a, b string) int {
		if j == judgeRestates && a == repeat.Title {
			return 3
		}
		if j == judgeOverlap && (a == twin.Title || b == twin.Title) && (a == parent.Title || b == parent.Title || a == third.Title || b == third.Title) {
			return 3
		}
		return 0
	}))
	e.meaning()
	// An agent's read records nothing.
	if _, err := e.s.ListGoals(e.ctx, e.ag, e.p.Key); err == nil {
		var n int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM goal_meaning_shown`).Scan(&n)
		if n != 0 {
			t.Errorf("an agent's read recorded %d", n)
		}
	}
	e.goals(e.admin, e.p.Key)
	e.exec(`UPDATE design_notes SET title = 'Finance staff download figures as workbooks, weekly' WHERE id = ?`, twin.ID)
	key := fmt.Sprintf("%d:repeats its parent goal #%d (“Reports export cleanly to spreadsheets”)", repeat.ID, parent.ID)
	e.exec(`UPDATE users SET prefs = ? WHERE id = ?`, jsonOf(map[string]any{"dismissed_warnings": []string{key}}), e.admin.User.ID)
	e.meaning()
	if l := e.labels(labelActed); len(l) != 2 || !l[0].y || l[0].judgement != judgeOverlap {
		t.Errorf("acted labels = %+v", l)
	}
	if l := e.labels(labelDismissed); len(l) != 1 || l[0].y || l[0].judgement != judgeRestates || l[0].ref != key {
		t.Errorf("dismissed labels = %+v", l)
	}
	// Shown again, the dismissed suggestion is hidden by the view: no
	// second label.
	e.exec(`UPDATE design_notes SET title = 'Reports export to spreadsheets' WHERE id = ?`, parent.ID)
	e.meaning()
	e.goals(e.admin, e.p.Key)
	e.meaning()
	if l := e.labels(labelDismissed); len(l) != 1 {
		t.Errorf("a dismissal labelled %d times", len(l))
	}
	e.setClock(e.clockNow().Add(meaningActWindow + time.Hour))
	e.meaning()
	var open int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM goal_meaning_shown WHERE outcome = ''`).Scan(&open)
	if open != 0 {
		t.Errorf("%d shown suggestions undecided after the window", open)
	}
}

// A drift flag on a parent goal the agent was not shown labels the task as
// not conveying it.
func TestMeaningDriftLabel(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	e.take(e.ag, i.ID, "next")
	e.exec(`UPDATE goal_context_assignments SET arm = 'goal' WHERE issue_id = ?`, i.ID)
	e.exec(`INSERT INTO goal_drift_flags(id, project_id, kind, goal_id, goal_title, created_at, answer) VALUES (1, ?, 'contradicts', ?, ?, 1, 'stop')`, e.p.ID, e.top.ID, e.top.Title)
	e.exec(`INSERT INTO goal_drift_flags(id, project_id, kind, goal_id, goal_title, created_at, answer) VALUES (2, ?, 'contradicts', ?, ?, 1, 'intent')`, e.p.ID, e.mid.ID, e.mid.Title)
	e.exec(`INSERT INTO goal_drift_flags(id, project_id, kind, goal_id, goal_title, created_at) VALUES (4, ?, 'contradicts', ?, ?, 1)`, e.p.ID, e.mid.ID, e.mid.Title)
	e.exec(`INSERT INTO goal_drift_flag_issues(flag_id, issue_id) VALUES (1, ?), (2, ?), (4, ?)`, i.ID, i.ID, i.ID)
	e.meaning()
	l := e.labels(labelDrift)
	if len(l) != 1 || l[0].y || l[0].judgement != judgeCover || l[0].bHash != goalTitleHash(e.top.Title) {
		t.Fatalf("drift labels = %+v", l)
	}
	e.meaning()
	if len(e.labels(labelDrift)) != 1 {
		t.Error("a flag labelled twice")
	}
	// A second flag on the same task and goal is the same pair: no new
	// label, and the passes come to rest (e.meaning fails otherwise).
	e.exec(`INSERT INTO goal_drift_flags(id, project_id, kind, goal_id, goal_title, created_at, answer) VALUES (3, ?, 'contradicts', ?, ?, 2, 'stop')`, e.p.ID, e.top.ID, e.top.Title)
	e.exec(`INSERT INTO goal_drift_flag_issues(flag_id, issue_id) VALUES (3, ?)`, i.ID)
	e.meaning()
	if n := len(e.labels(labelDrift)); n != 1 {
		t.Errorf("%d drift labels after a repeat flag", n)
	}
}

// A goal of an overlap suggestion moved under another goal is acted on.
func TestMeaningMovedIsActed(t *testing.T) {
	e := newGoalCtxEnv(t)
	a := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	b := e.goal(e.admin, e.p.Key, "Finance staff download figures as workbooks")
	e.useJudge(grader(func(j, x, y string) int {
		if j == judgeOverlap && (x == a.Title || y == a.Title) && (x == b.Title || y == b.Title) {
			return 3
		}
		return 0
	}))
	e.meaning()
	e.goals(e.admin, e.p.Key)
	e.exec(`UPDATE design_notes SET parent_id = ? WHERE id = ?`, e.top.ID, b.ID)
	e.meaning()
	if l := e.labels(labelActed); len(l) != 1 || !l[0].y || l[0].judgement != judgeOverlap {
		t.Errorf("acted labels = %+v", l)
	}
}

// The gateway's "wait" is no try: it stops the pass and sets the next one
// for when the wait ends.
func TestMeaningPassWaits(t *testing.T) {
	ps := &meaningPass{}
	if ps.waited(errors.New("the model is down")) || ps.waiting {
		t.Fatal("a failure taken for a wait")
	}
	until := time.UnixMilli(5_000_000)
	if !ps.waited(fmt.Errorf("asking: %w", &aiWaitError{until: until, why: "quota"})) || !ps.waiting || ps.retryAt != until.UnixMilli() {
		t.Errorf("wait: %+v", ps)
	}
	ps.retry(4_000_000)
	ps.retry(0)
	ps.retry(6_000_000)
	if ps.retryAt != 4_000_000 {
		t.Errorf("retryAt = %d, want the soonest", ps.retryAt)
	}
}

// Each goal's title is built from once, and its cases are labelled by how
// they were built.
func TestMeaningConstructed(t *testing.T) {
	e := newGoalCtxEnv(t)
	j := e.useJudge(grader(func(string, string, string) int { return 0 }))
	e.meaning()
	if n := j.count(constructMarker); n != 3 {
		t.Fatalf("built from %d goals, want 3", n)
	}
	l := e.labels(labelConstructed)
	if len(l) != 3*11 {
		t.Fatalf("%d constructed labels, want 33", len(l))
	}
	yes := map[string]int{}
	for _, x := range l {
		if x.y {
			yes[x.judgement+" "+x.ref]++
		}
	}
	want := map[string]int{"overlap paraphrase": 3, "overlap narrower": 3, "restates paraphrase": 3, "cover paraphrase": 3}
	if fmt.Sprint(yes) != fmt.Sprint(want) {
		t.Errorf("yes labels = %v", yes)
	}
	e.meaning()
	if j.count(constructMarker) != 3 {
		t.Error("built again from unchanged titles")
	}
}

// The evaluator adopts a scorer that beats the prior on held-out labels,
// and runs again only on drift or a new scorer version.
func TestMeaningEvaluate(t *testing.T) {
	var rows []meaningRow
	for k := range 80 {
		y := k%2 == 0
		v := 0.1 + float64(k%5)*0.02
		if y {
			v = 0.8 + float64(k%5)*0.02
		}
		rows = append(rows, meaningRow{id: int64(k + 1), y: y, source: labelConstructed, grp: fmt.Sprintf("g%d", k/2),
			f: map[string]float64{"words": 0.3, "nli_min": v}})
	}
	versions := map[string]string{"embed": "", "nli": "nli@test", "llm": meaningLLMVersion, "construct": meaningConstruct}
	if r := meaningEvalReason(nil, rows[:30], versions); r != "" {
		t.Errorf("too few labels evaluated: %q", r)
	}
	if r := meaningEvalReason(nil, rows, versions); r != "first" {
		t.Fatalf("reason = %q", r)
	}
	f := meaningEvaluate(judgeCover, rows, nil)
	if f == nil || f.Decider != "nli_min" || f.Note != "adopted" || f.ErrPos != 0 || f.ErrNeg != 0 {
		t.Fatalf("fit = %+v", f)
	}
	f.Judgement, f.Versions = judgeCover, versions
	if r := meaningEvalReason(f, rows, versions); r != "" {
		t.Errorf("nothing new and evaluated again: %q", r)
	}
	// Labels that agree with it: still nothing.
	more := append([]meaningRow(nil), rows...)
	for k := range 30 {
		more = append(more, meaningRow{id: int64(100 + k), y: true, source: labelActed, grp: "x", f: map[string]float64{"nli_min": 0.9}})
	}
	if r := meaningEvalReason(f, more, versions); r != "" {
		t.Errorf("agreeing labels triggered %q", r)
	}
	// Yes labels it misses, more than its held-out rate allows.
	for k := range 25 {
		more = append(more, meaningRow{id: int64(200 + k), y: true, source: labelActed, grp: "y", f: map[string]float64{"nli_min": 0.05}})
	}
	if r := meaningEvalReason(f, more, versions); r != "drift" {
		t.Errorf("reason = %q, want drift", r)
	}
	v2 := map[string]string{"embed": "", "nli": "nli@new", "llm": meaningLLMVersion, "construct": meaningConstruct}
	if r := meaningEvalReason(f, rows, v2); r != "version" {
		t.Errorf("reason = %q, want version", r)
	}
	// An unknown version (the service down) is no change.
	v3 := map[string]string{"embed": "", "nli": "", "llm": meaningLLMVersion, "construct": meaningConstruct}
	if r := meaningEvalReason(f, rows, v3); r != "" {
		t.Errorf("a service that is down triggered %q", r)
	}
	// Kept when the challenger is no better.
	again := meaningEvaluate(judgeCover, rows, f)
	if again == nil || again.Decider != "nli_min" || again.Note != "kept" {
		t.Errorf("re-evaluation = %+v", again)
	}
}

func TestBinomUpper(t *testing.T) {
	if p := binomUpper(20, 0, 0.1); p != 1 {
		t.Errorf("P(X>=0) = %v", p)
	}
	if p := binomUpper(1, 1, 0.3); math.Abs(p-0.3) > 1e-9 {
		t.Errorf("P(X>=1; 1, .3) = %v", p)
	}
	if p := binomUpper(20, 10, 0.05); p > 1e-5 {
		t.Errorf("10 of 20 at 5%% = %v", p)
	}
}

type meaningFeedbackLLM struct {
	reports []feedbackReport
	during  func() // runs while a report is out
}

func (f *meaningFeedbackLLM) Feedback(ctx context.Context, p *AIProvider, r feedbackReport) error {
	f.reports = append(f.reports, r)
	if d := f.during; d != nil {
		f.during = nil
		d()
	}
	return nil
}

// A judgement through the gateway asks for extract/judge alone, named for
// its report, without the models WP_JUDGE_EXCLUDE names; one through a task's own models
// carries none of that.
func TestMeaningJudgeRequest(t *testing.T) {
	t.Setenv("WP_JUDGE_EXCLUDE", "ollama-slow/*")
	if got := gatewayModels(gatewayJudgePurpose); !slices.Equal(got, []string{"extract/judge"}) {
		t.Errorf("models = %v", got)
	}
	ctx := (&meaningChain{gateway: true}).ctx(t.Context(), judgeOverlap)
	if r := gatewayRoutingOf(ctx); r == nil || r.prefix != "wp-meaning-"+judgeOverlap {
		t.Errorf("routing = %+v", r)
	}
	if ex := gatewayExcludeOf(ctx); !slices.Equal(ex, []string{"ollama-slow/*"}) {
		t.Errorf("exclude = %v", ex)
	}
	if own := (&meaningChain{}).ctx(t.Context(), judgeOverlap); gatewayRoutingOf(own) != nil || gatewayExcludeOf(own) != nil {
		t.Error("a task's own models got the gateway's routing")
	}
}

// A gateway answer a label judges is reported once: verified when its
// grade agreed, a false positive when not. A later label that overturns
// the verdict sends it again (the gateway counts the latest); one that
// agrees sends nothing.
func TestMeaningFeedback(t *testing.T) {
	e := newGoalCtxEnv(t)
	now := e.s.nowMS()
	e.exec(`INSERT INTO meaning_scores(scorer, a_hash, b_hash, score, request_id, scored_at) VALUES
		('llm:overlap:v1', 'a', 'b', 1.0, 'wp-meaning-overlap-1', ?), ('llm:overlap:v1', 'a', 'c', 0.0, 'wp-meaning-overlap-2', ?),
		('llm:overlap:v1', 'a', 'd', 1.0, 'other-3', ?)`, now, now, now)
	e.exec(`INSERT INTO meaning_labels(judgement, a_hash, b_hash, y, source, grp, at) VALUES
		('overlap', 'a', 'b', 0, 'dismissed', 'g', ?), ('overlap', 'a', 'c', 0, 'constructed', 'g', ?), ('overlap', 'a', 'd', 1, 'acted', 'g', ?)`, now, now, now)
	e.s.queueMeaningFeedback(e.ctx)
	fl := &meaningFeedbackLLM{}
	e.s.reportMeaningFeedback(e.ctx, fl, &AIProvider{})
	got := map[string]string{}
	for _, r := range fl.reports {
		got[r.requestID] = fmt.Sprintf("%d/%d", r.verified, r.falsePos)
	}
	if fmt.Sprint(got) != "map[wp-meaning-overlap-1:0/1 wp-meaning-overlap-2:1/0]" {
		t.Errorf("reports = %v", got)
	}
	e.s.queueMeaningFeedback(e.ctx)
	e.s.reportMeaningFeedback(e.ctx, fl, &AIProvider{})
	if len(fl.reports) != 2 {
		t.Errorf("reported again: %d", len(fl.reports))
	}

	// Behaviour overturns a constructed label (a-c graded 0, now acted on as
	// overlapping) and agrees with a dismissal (a-b, now ignored too).
	e.exec(`INSERT INTO meaning_labels(judgement, a_hash, b_hash, y, source, grp, at) VALUES
		('overlap', 'a', 'c', 1, 'acted', 'g', ?), ('overlap', 'a', 'b', 0, 'ignored', 'g', ?)`, now, now)
	e.s.queueMeaningFeedback(e.ctx)
	e.s.reportMeaningFeedback(e.ctx, fl, &AIProvider{})
	if len(fl.reports) != 3 || fl.reports[2] != (feedbackReport{requestID: "wp-meaning-overlap-2", falsePos: 1}) {
		t.Errorf("after the overturn: %+v", fl.reports)
	}

	// A verdict that changes while its report is out is still due after it.
	e.exec(`INSERT INTO meaning_labels(judgement, a_hash, b_hash, y, source, grp, at) VALUES ('overlap', 'a', 'b', 1, 'acted', 'g', ?)`, now)
	e.s.queueMeaningFeedback(e.ctx)
	fl.during = func() {
		e.exec(`INSERT INTO meaning_labels(judgement, a_hash, b_hash, y, source, grp, at) VALUES ('overlap', 'a', 'b', 0, 'drift', 'g', ?)`, now)
		e.s.queueMeaningFeedback(e.ctx)
	}
	e.s.reportMeaningFeedback(e.ctx, fl, &AIProvider{})
	e.s.reportMeaningFeedback(e.ctx, fl, &AIProvider{})
	var last []string
	for _, r := range fl.reports[3:] {
		last = append(last, fmt.Sprintf("%s:%d/%d", r.requestID, r.verified, r.falsePos))
	}
	if fmt.Sprint(last) != "[wp-meaning-overlap-1:1/0 wp-meaning-overlap-1:0/1]" {
		t.Errorf("around a change in flight: %v", last)
	}
}

// A model call that is out must not hold up the trace recompute or a read:
// the judgements run on their own (WP-1035).
func TestMeaningDoesNotBlockTraces(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Make navigation feel predictable", e.leaf)
	later := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Restyle the footer"})
	e.s.goalsSettle(e.ctx)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	answer := grader(func(string, string, string) int { return 0 })
	e.useJudge(func(prompt string) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return answer(prompt)
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	e.s.goalsChanged()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no judgement call started")
	}
	e.serve(e.admin, later.Key, e.leaf.ID)
	begin := time.Now()
	if _, err := e.s.ListGoals(e.ctx, e.admin, e.p.Key); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("ListGoals waited %v on a model call", d)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM goal_traces WHERE item_type = 'issue' AND item_id = ? AND goal_id = ?`, later.ID, e.leaf.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("goal_traces not updated while the model call was out: %d %v", n, err)
	}
	released = true
	close(release)
	e.meaning()
	if e.coverJudged(i.ID) == 0 {
		t.Error("the judgement didn't land after release")
	}
}
