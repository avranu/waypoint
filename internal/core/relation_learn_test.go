package core

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func suggestedKeys(t *testing.T, e *env, a *Actor, key string) map[string]RelationSuggestion {
	t.Helper()
	res, err := e.s.SuggestRelations(e.ctx, a, key)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]RelationSuggestion{}
	for _, s := range res.Suggestions {
		out[s.Key] = s
	}
	return out
}

func TestRelationSuggestionDecisions(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, nil)
	shown := suggestedKeys(t, e, f.member, f.src.Key)
	if len(shown) == 0 {
		t.Fatal("nothing suggested")
	}
	type row struct {
		source, outcome string
		times           int
		word            float64
	}
	rowOf := func(issue, other *IssueDetail) (row, bool) {
		var r row
		err := e.s.db.R.QueryRowContext(e.ctx, `SELECT source, coalesce(outcome, ''), times_shown, word FROM relation_suggestions WHERE issue_id = ? AND other_id = ?`,
			issue.ID, other.ID).Scan(&r.source, &r.outcome, &r.times, &r.word)
		return r, err == nil
	}
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM relation_suggestions`).Scan(&n); err != nil || n != len(shown) {
		t.Fatalf("recorded %d of %d shown (%v)", n, len(shown), err)
	}
	if r, _ := rowOf(f.src, f.waits); r.source != sugText {
		t.Errorf("stated dependency recorded as %+v", r)
	}
	if r, _ := rowOf(f.src, f.alike); r.source != sugWords || r.word <= 0 || r.outcome != "" {
		t.Errorf("alike recorded as %+v", r)
	}

	// A read-only token's look writes nothing.
	ro := *f.member
	ro.ReadOnly = true
	if len(suggestedKeys(t, e, &ro, f.alike.Key)) == 0 {
		t.Fatal("nothing suggested for the alike issue")
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM relation_suggestions WHERE issue_id = ?`, f.alike.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("a read-only look recorded %d (%v)", n, err)
	}

	// An agent looking records nothing: only people's decisions teach.
	agent := &Actor{User: f.member.User, Agent: "claude"}
	suggestedKeys(t, e, agent, f.src.Key)
	if r, _ := rowOf(f.src, f.alike); r.times != 1 {
		t.Errorf("an agent's look counted: %+v", r)
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, agent, f.src.Key, f.otherProj.Key); err == nil {
		t.Error("an agent dismissed a suggestion")
	}

	// Relating the pair, from the other end, accepts it.
	if err := e.s.AddRelation(e.ctx, f.member, f.alike.Key, "relates", f.src.Key); err != nil {
		t.Fatal(err)
	}
	if r, _ := rowOf(f.src, f.alike); r.outcome != "accepted" {
		t.Errorf("related pair: %+v", r)
	}

	// A dismissal keeps the pair apart, from either end.
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.otherProj.Key); err != nil {
		t.Fatal(err)
	}
	if r, _ := rowOf(f.src, f.otherProj); r.outcome != "dismissed" {
		t.Errorf("dismissed: %+v", r)
	}
	if _, ok := suggestedKeys(t, e, f.member, f.src.Key)[f.otherProj.Key]; ok {
		t.Error("a dismissed suggestion came back")
	}
	if _, ok := suggestedKeys(t, e, f.member, f.otherProj.Key)[f.src.Key]; ok {
		t.Error("a dismissed suggestion came back from the other end")
	}
	// Shown again, a decided suggestion keeps its decision.
	if r, _ := rowOf(f.src, f.otherProj); r.outcome != "dismissed" || r.times != 1 {
		t.Errorf("dismissed, then shown again: %+v", r)
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.alike.Key); err == nil {
		t.Error("dismissed a pair that is related")
	}

	// Removing the relation undoes the acceptance: the pair can be
	// suggested and dismissed again.
	d, err := e.s.GetIssue(e.ctx, f.member, f.src.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range d.Relations {
		if rel.Issue.Key == f.alike.Key {
			if err := e.s.RemoveRelation(e.ctx, f.member, f.src.Key, rel.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if r, _ := rowOf(f.src, f.alike); r.outcome != "" {
		t.Errorf("relation removed, still %+v", r)
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.alike.Key); err != nil {
		t.Errorf("dismissing once unrelated: %v", err)
	}
}

func TestFitLogistic(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var x [][]float64
	var y []bool
	for range 400 {
		f := relFeatures{Word: rng.Float64() * 0.5, SharedGoal: rng.IntN(2) == 1, AgeDays: rng.Float64() * 100}
		// People take what shares a goal, whatever the words.
		z := -2 + 4*b2f(f.SharedGoal)
		x = append(x, f.x())
		y = append(y, rng.Float64() < 1/(1+math.Exp(-z)))
	}
	r := fitLogistic(x, y)
	again := fitLogistic(x, y)
	for j := range r.W {
		if r.W[j] != again.W[j] {
			t.Fatalf("not deterministic: %v vs %v", r.W, again.W)
		}
	}
	goal := r.W[1+4]
	if goal < 1 || math.Abs(r.W[1]) > goal/3 {
		t.Errorf("weights %v: shared goal should lead", r.W)
	}
	scores := make([]float64, len(x))
	for i, row := range x {
		scores[i] = r.score(row)
	}
	if a := aucOf(scores, y); a < 0.75 {
		t.Errorf("AUC %.3f", a)
	}
}

func TestAUC(t *testing.T) {
	for _, c := range []struct {
		score []float64
		y     []bool
		want  float64
	}{
		{[]float64{1, 2, 3, 4}, []bool{false, false, true, true}, 1},
		{[]float64{1, 2, 3, 4}, []bool{true, true, false, false}, 0},
		{[]float64{1, 1, 1, 1}, []bool{true, false, true, false}, 0.5},
		{[]float64{1, 2, 2, 3}, []bool{false, true, false, true}, 0.875},
	} {
		if got := aucOf(c.score, c.y); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("aucOf(%v, %v) = %v, want %v", c.score, c.y, got, c.want)
		}
	}
	if !math.IsNaN(aucOf([]float64{1, 2}, []bool{true, true})) {
		t.Error("one class has no AUC")
	}
}

// seedDecisions records n decided suggestions between the issues ids, the
// first half before the held-out month, the rest in it; take says whether
// a person took one with these features.
func seedDecisions(t *testing.T, e *env, ids []int64, n int, rng *rand.Rand, take func(relFeatures) bool) {
	t.Helper()
	now := e.clockNow()
	k := 0
	for a := 0; a < len(ids) && k < n; a++ {
		for b := 0; b < len(ids) && k < n; b++ {
			if a == b {
				continue
			}
			f := relFeatures{Word: 0.1 + rng.Float64()*0.4, Concept: rng.Float64(), HasConcept: true,
				SameProject: true, SharedGoal: rng.IntN(3) == 0, AgeDays: rng.Float64() * 60}
			at := now.Add(-time.Duration(40+rng.IntN(60)) * 24 * time.Hour)
			if k%2 == 1 {
				at = now.Add(-time.Duration(2+rng.IntN(25)) * 24 * time.Hour)
			}
			var outcome any
			switch {
			case take(f):
				outcome = "accepted"
			case rng.IntN(2) == 0:
				outcome = "dismissed"
			}
			if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO relation_suggestions(issue_id, other_id, kind, source, confidence, shown_at,
					word, concept, same_project, shared_parent, shared_goal, age_days, source_done, blend, outcome)
				VALUES (?,?,'relates',?,'low',?,?,?,?,0,?,?,0,?,?)`, ids[a], ids[b], sugWords, at.UnixMilli(),
				f.Word, f.Concept, f.SameProject, f.SharedGoal, f.AgeDays, f.blend(), outcome); err != nil {
				t.Fatal(err)
			}
			k++
		}
	}
	if k < n {
		t.Fatalf("only %d pairs", k)
	}
}

func TestRelationRankerAdoption(t *testing.T) {
	newIssues := func(e *env) []int64 {
		p := e.project(e.admin, "Learn", "")
		var ids []int64
		for n := range 24 {
			ids = append(ids, e.issue(e.admin, IssueInput{Project: p.Key, Title: "Issue " + string(rune('a'+n))}).ID)
		}
		return ids
	}

	t.Run("too few", func(t *testing.T) {
		e := newEnv(t)
		seedDecisions(t, e, newIssues(e), 60, rand.New(rand.NewPCG(3, 4)), func(f relFeatures) bool { return f.SharedGoal })
		f, err := e.s.fitRelationRanker(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Adopted || !strings.Contains(f.Note, "needs 200") {
			t.Errorf("fit %+v", f)
		}
	})

	t.Run("beats the blend", func(t *testing.T) {
		e := newEnv(t)
		rng := rand.New(rand.NewPCG(5, 6))
		seedDecisions(t, e, newIssues(e), 500, rng, func(f relFeatures) bool { return f.SharedGoal && rng.IntN(5) > 0 })
		f, err := e.s.fitRelationRanker(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("fit %+v", *f)
		if !f.Adopted || f.AUCModel <= f.AUCBlend || f.Wins < rankerWinShare || f.Ranker == nil {
			t.Fatalf("fit %+v", f)
		}
		if f.Ranker.Line <= 0 || f.Ranker.Line >= 1 {
			t.Errorf("line %v", f.Ranker.Line)
		}
		if err := e.s.recordFit(e.ctx, f); err != nil {
			t.Fatal(err)
		}
		// A restart loads it, and doesn't fit again within the day.
		e.s.ranker = rankerState{}
		r := e.s.relationRanker(e.ctx)
		if r == nil || r.Line != f.Ranker.Line {
			t.Fatalf("loaded %+v", r)
		}
		if e.s.ranker.fitting {
			t.Error("fitting again within a day")
		}
	})

	t.Run("no better than the blend", func(t *testing.T) {
		e := newEnv(t)
		rng := rand.New(rand.NewPCG(7, 8))
		seedDecisions(t, e, newIssues(e), 500, rng, func(f relFeatures) bool { return f.blend() > 0.6 })
		f, err := e.s.fitRelationRanker(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Adopted {
			t.Errorf("adopted a ranker no better than the blend: %+v", f)
		}
	})
}

func TestRankerLineDropsSuggestions(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, nil)
	// A ranker that thinks nothing alike is worth showing: what the text
	// states and the same title still are.
	w := make([]float64, len(relFeatureNames)+1)
	sd := make([]float64, len(relFeatureNames))
	for j := range sd {
		sd[j] = 1
	}
	e.s.ranker = rankerState{loaded: true, at: e.clockNow(),
		r: &relRanker{Features: relFeatureNames, Mean: make([]float64, len(sd)), SD: sd, W: w, Line: 0.9}}
	got := suggestedKeys(t, e, f.member, f.src.Key)
	if _, ok := got[f.alike.Key]; ok {
		t.Errorf("the ranker's line kept %s: %v", f.alike.Key, got)
	}
	if _, ok := got[f.waits.Key]; !ok {
		t.Errorf("a stated dependency went: %v", got)
	}
	// Under a line it clears, the alike issue is back, with its score kept.
	e.s.ranker.r.Line = 0.4
	if _, ok := suggestedKeys(t, e, f.member, f.src.Key)[f.alike.Key]; !ok {
		t.Error("alike issue missing under a line it clears")
	}
	var p *float64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT model_p FROM relation_suggestions WHERE issue_id = ? AND other_id = ?`, f.src.ID, f.alike.ID).Scan(&p); err != nil || p == nil || *p != 0.5 {
		t.Errorf("model_p = %v (%v)", p, err)
	}
}

// Who decides, and how a dismissal reaches the pair (WP-958 review).
func TestRelationSuggestionWhoDecides(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, nil)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := e.s.db.R.QueryRowContext(e.ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	outcome := func(issue, other *IssueDetail) string {
		var o *string
		err := e.s.db.R.QueryRowContext(e.ctx, `SELECT outcome FROM relation_suggestions WHERE issue_id = ? AND other_id = ?`, issue.ID, other.ID).Scan(&o)
		switch {
		case err != nil:
			return "missing"
		case o == nil:
			return ""
		}
		return *o
	}

	// A viewer, who can neither add nor dismiss, isn't recorded.
	vic := e.user("vic", false)
	if err := e.s.SetMember(e.ctx, e.admin, strings.SplitN(f.src.Key, "-", 2)[0], "vic", "viewer"); err != nil {
		t.Fatal(err)
	}
	if len(suggestedKeys(t, e, vic, f.src.Key)) == 0 {
		t.Fatal("nothing suggested to a viewer")
	}
	if n := count(`SELECT count(*) FROM relation_suggestions`); n != 0 {
		t.Errorf("a viewer's look recorded %d", n)
	}

	// Shown to a person from both ends; an agent relating the pair isn't
	// the person taking it, and the pair no longer counts as ignored.
	suggestedKeys(t, e, f.member, f.src.Key)
	suggestedKeys(t, e, f.member, f.alike.Key)
	if outcome(f.alike, f.src) != "" {
		t.Fatalf("not shown from the other end: %q", outcome(f.alike, f.src))
	}
	e.setClock(e.clockNow().Add(48 * time.Hour))
	before, err := e.s.fitRelationRanker(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: f.member.User, Agent: "claude"}
	if err := e.s.AddRelation(e.ctx, agent, f.src.Key, "relates", f.alike.Key); err != nil {
		t.Fatal(err)
	}
	if o := outcome(f.src, f.alike); o != "" {
		t.Errorf("an agent's relation recorded as %q", o)
	}
	after, err := e.s.fitRelationRanker(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Accepted != 0 || after.Ignored != before.Ignored-2 {
		t.Errorf("after an agent related the pair: accepted %d, ignored %d (was %d)", after.Accepted, after.Ignored, before.Ignored)
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.alike.Key); err == nil || !strings.Contains(err.Error(), "related already") {
		t.Errorf("dismissing a related pair: %v", err)
	}

	// A dismissal decides the pair from both ends.
	suggestedKeys(t, e, f.member, f.otherProj.Key)
	if outcome(f.src, f.otherProj) != "" || outcome(f.otherProj, f.src) != "" {
		t.Fatalf("not shown both ways: %q %q", outcome(f.src, f.otherProj), outcome(f.otherProj, f.src))
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.otherProj.Key); err != nil {
		t.Fatal(err)
	}
	if outcome(f.src, f.otherProj) != "dismissed" || outcome(f.otherProj, f.src) != "dismissed" {
		t.Errorf("dismissed one way: %q %q", outcome(f.src, f.otherProj), outcome(f.otherProj, f.src))
	}

	// Undone, it is undecided again from both ends and comes back.
	if err := e.s.UndoRelationDismissal(e.ctx, agent, f.src.Key, f.otherProj.Key); err == nil {
		t.Error("an agent undid a dismissal")
	}
	if err := e.s.UndoRelationDismissal(e.ctx, f.member, f.src.Key, f.otherProj.Key); err != nil {
		t.Fatal(err)
	}
	if outcome(f.src, f.otherProj) != "" || outcome(f.otherProj, f.src) != "" {
		t.Errorf("undone: %q %q", outcome(f.src, f.otherProj), outcome(f.otherProj, f.src))
	}
	if _, ok := suggestedKeys(t, e, f.member, f.src.Key)[f.otherProj.Key]; !ok {
		t.Error("an undone dismissal stayed hidden")
	}

	// A suggestion whose look wasn't recorded can still be dismissed, and
	// doesn't train the ranker.
	if _, err := e.s.db.W.ExecContext(e.ctx, `DELETE FROM relation_suggestions WHERE (issue_id = ? AND other_id = ?) OR (issue_id = ? AND other_id = ?)`,
		f.src.ID, f.waits.ID, f.waits.ID, f.src.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.waits.Key); err != nil {
		t.Fatal(err)
	}
	if _, ok := suggestedKeys(t, e, f.member, f.src.Key)[f.waits.Key]; ok {
		t.Error("an unrecorded suggestion came back after its dismissal")
	}
	if n := count(`SELECT count(*) FROM relation_suggestions WHERE source IN (?, ?) AND issue_id = ? AND other_id = ?`, sugWords, sugConcepts, f.src.ID, f.waits.ID); n != 0 {
		t.Errorf("an unrecorded dismissal would train the ranker")
	}
	if err := e.s.DismissRelationSuggestion(e.ctx, f.member, f.src.Key, f.src.Key); err == nil {
		t.Error("dismissed an issue against itself")
	}
}
