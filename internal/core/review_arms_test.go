package core

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestBetaQuantile(t *testing.T) {
	for _, c := range []struct{ q, a, b, want float64 }{
		{0.9, 1, 1, 0.9},
		{0.9, 2, 1, math.Sqrt(0.9)},
		{0.5, 3, 3, 0.5},
		{0.9, 1, 2, 1 - math.Sqrt(0.1)},
	} {
		if got := betaQuantile(c.q, c.a, c.b); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("q%.1f Beta(%g,%g) = %f, want %f", c.q, c.a, c.b, got, c.want)
		}
	}
	if u := betaQuantile(armBenchQ, 1, 16); u >= armBenchBound {
		t.Errorf("0 of 15 bound = %f", u)
	}
	if u := betaQuantile(armBenchQ, 2, 15); u < armBenchBound {
		t.Errorf("1 of 15 bound = %f", u)
	}
	var s Service
	sum := 0.0
	for range 4000 {
		sum += s.sampleBeta(3, 7)
	}
	if m := sum / 4000; math.Abs(m-0.3) > 0.02 {
		t.Errorf("Beta(3,7) sample mean = %f", m)
	}
}

func TestArmWeight(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	at := func(days int) int64 { return now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli() }
	for _, c := range []struct {
		days    int
		version string
		want    float64
	}{
		{1, "cur", 1}, {29, "cur", 1}, {31, "cur", 0.5}, {89, "cur", 0.5}, {91, "cur", 0},
		{1, "prev", 0.5}, {40, "prev", 0.25}, {1, "older", 0}, {1, "", 0},
	} {
		if got := armWeight(now, armFinding{created: at(c.days), version: c.version}, "cur", "prev"); got != c.want {
			t.Errorf("%d days, %s = %v, want %v", c.days, c.version, got, c.want)
		}
	}
	fs := []armFinding{
		{strategy: "diff", version: "a", created: 1}, {strategy: "diff", version: "b", created: 5},
		{strategy: "diff", version: strategyDiff.promptVersion(), created: 9},
	}
	if p := previousVersions(fs); p["diff"] != "b" || p["diff+files"] != "" {
		t.Errorf("previous = %v", p)
	}
	for f, want := range map[armFinding]string{
		{strategy: "diff", verify: VerifyKept}:          "diff/verify",
		{strategy: "diff", verify: VerifyCorrected}:     "diff/verify",
		{strategy: "diff+files", verify: VerifySkipped}: "diff+files/none",
		{strategy: "diff", verify: VerifyDropped}:       "",
		{strategy: "diff", verify: VerifyUnchecked}:     "",
	} {
		if got, _ := armOf(f); got != want {
			t.Errorf("armOf(%+v) = %q, want %q", f, got, want)
		}
	}
}

// tallies with every arm untried and available.
func freshTallies(canVerify bool) map[string]*armTally {
	out := map[string]*armTally{}
	for _, a := range reviewArms() {
		out[a.key()] = &armTally{cost: 1, available: canVerify || !a.verify, explore: true}
	}
	return out
}

func keys(arms []reviewArm) []string {
	var out []string
	for _, a := range arms {
		out = append(out, a.key())
	}
	return out
}

func TestChooseArms(t *testing.T) {
	s := &Service{beta: func(a, b float64) float64 { return a / (a + b) }, rnd: func() float64 { return 0.99 }}
	if got := keys(s.chooseArms(freshTallies(true), 1)); !slices.Equal(got, []string{"diff+files/verify"}) {
		t.Errorf("fresh = %v", got)
	}
	if got := keys(s.chooseArms(freshTallies(false), 1)); !slices.Equal(got, []string{"diff+files/none"}) {
		t.Errorf("no verifier = %v", got)
	}
	tl := freshTallies(true)
	tl["diff/verify"].useful, tl["diff/verify"].falsePos = 8, 2
	if got := keys(s.chooseArms(tl, 1)); got[0] != "diff/verify" {
		t.Errorf("best = %v", got)
	}
	tl["diff/verify"].cost = 3
	if got := keys(s.chooseArms(tl, 1)); got[0] == "diff/verify" {
		t.Errorf("three times the tokens still won: %v", got)
	}
	tl = freshTallies(true)
	for k, t := range tl {
		t.useful, t.falsePos, t.explore = 10, 10, false
		if k == "diff/none" {
			t.useful, t.falsePos, t.benched = 30, 0.5, true
		}
	}
	if got := keys(s.chooseArms(tl, 1)); got[0] == "diff/none" {
		t.Errorf("a benched arm won: %v", got)
	}
	s.rnd = func() float64 { return 0.01 }
	if got := keys(s.chooseArms(tl, 1)); got[0] != "diff/none" || !tl["diff/none"].benchedDraw {
		t.Errorf("the benched arm's 1 in 20: %v", got)
	}
	s.rnd = func() float64 { return 0.99 }
	tl = freshTallies(true)
	tl["diff+files/verify"].pin = "off"
	if got := keys(s.chooseArms(tl, 1)); got[0] != "diff+files/none" {
		t.Errorf("pinned off = %v", got)
	}
	tl["diff/none"].pin = "on"
	if got := keys(s.chooseArms(tl, 2)); !slices.Equal(got, []string{"diff/none", "diff/none"}) {
		t.Errorf("pinned on = %v", got)
	}
	tl = freshTallies(true)
	tl["diff+files/verify"].useful, tl["diff+files/verify"].falsePos, tl["diff+files/verify"].explore = 20, 5, false
	tl["diff/verify"].useful = 1
	if got := keys(s.chooseArms(tl, 3)); !slices.Equal(got, []string{"diff+files/verify", "diff/verify", "diff+files/verify"}) {
		t.Errorf("slots = %v", got)
	}
}

func TestExcludePatterns(t *testing.T) {
	ctx := withGatewayExclude(t.Context(), []string{"opencode/muse-spark-1.3", "groq/openai/gpt-oss-120b", "", "opencode/muse-spark-1.3"})
	want := []string{"opencode/muse-spark-1.3", "*/muse-spark-1.3", "groq/openai/gpt-oss-120b", "*/gpt-oss-120b"}
	if got := gatewayExcludeOf(ctx); !slices.Equal(got, want) {
		t.Errorf("exclude = %v", got)
	}
	if gatewayExcludeOf(withoutGatewayExclude(ctx)) != nil {
		t.Error("withoutGatewayExclude kept them")
	}
	if withGatewayExclude(t.Context(), nil) != t.Context() {
		t.Error("nothing to exclude still set a value")
	}
	if got := excludePatterns("free-*"); !slices.Equal(got, []string{"free-*"}) {
		t.Errorf("a pattern = %v", got)
	}
}

func TestBudgetFor(t *testing.T) {
	if got := budgetFor(8000); got != reviewBudgetMin {
		t.Errorf("8k window = %d", got)
	}
	if got := budgetFor(1_000_000); got != reviewBudgetMax {
		t.Errorf("1M window = %d", got)
	}
	if got := budgetFor(131072); got != 131072*3/2-reviewChunkChars-reviewPromptOther {
		t.Errorf("128k window = %d", got)
	}
}

// A run records its arm and reviewers; a finding judged useful counts for
// its arm; Admin → AI shows it; the budget follows the gateway's window
// for the model that answered; a pin decides the next run.
func TestReviewArmsRecorded(t *testing.T) {
	re := newReviewEnv(t)
	re.asGateway(t)
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	if run.Arm != "diff+files/verify" || len(run.Findings) != 1 {
		t.Fatalf("run = %+v", run)
	}
	var models string
	if err := re.s.db.R.QueryRow(`SELECT reviewer_models FROM bot_runs WHERE id = ?`, run.ID).Scan(&models); err != nil || models != `["`+museModel+`"]` {
		t.Errorf("reviewer models = %s %v", models, err)
	}
	for n, m := range re.llm.calls {
		if m == mimoModel && !slices.Contains(re.llm.exclude[n], museModel) {
			t.Errorf("the verifier's exclude = %v", re.llm.exclude[n])
		}
		if m == museModel && re.llm.exclude[n] != nil {
			t.Errorf("the only reviewer excluded %v", re.llm.exclude[n])
		}
	}
	if _, err := re.s.db.W.Exec(`UPDATE bot_findings SET state = ? WHERE run_id = ?`, FindingFixed, run.ID); err != nil {
		t.Fatal(err)
	}
	re.llm.mu.Lock()
	re.llm.windows = map[string]int{museModel: 131072}
	re.llm.mu.Unlock()
	modelLists.Lock()
	clear(modelLists.m)
	modelLists.Unlock()
	arms, err := re.s.ReviewArms(re.ctx, re.admin)
	if err != nil {
		t.Fatal(err)
	}
	if !arms.Gateway {
		t.Error("the gateway picks the model, but the page isn't told")
	}
	if arms.Budget.ContextLength != 131072 || arms.Budget.Model != museModel || arms.Budget.Chars != budgetFor(131072) {
		t.Errorf("budget = %+v", arms.Budget)
	}
	var got ReviewArm
	for _, a := range arms.Arms {
		if a.Key == "diff+files/verify" {
			got = a
		}
	}
	if got.Useful != 1 || got.State != ArmExploring || got.Runs != 1 || len(got.Models) != 1 || got.Models[0].Model != museModel || got.Models[0].Useful != 1 {
		t.Errorf("arm = %+v", got)
	}
	if _, err := re.s.ReviewArms(re.ctx, re.user("mo", false)); err == nil {
		t.Error("a member read the arms")
	}
	if _, err := re.s.PinReviewArm(re.ctx, re.admin, "diff/verify", "maybe"); err == nil {
		t.Error("pin maybe")
	}
	if _, err := re.s.PinReviewArm(re.ctx, re.admin, "nope", "on"); err == nil {
		t.Error("pinned an arm that isn't one")
	}
	arms, err = re.s.PinReviewArm(re.ctx, re.admin, "diff/none", "on")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range arms.Arms {
		if a.Key == "diff/none" && (a.State != ArmPinned || a.Pin != "on") {
			t.Errorf("pinned arm = %+v", a)
		}
	}
	if arms, err = re.s.PinReviewArm(re.ctx, re.admin, "diff/none", ""); err != nil || arms.Arms[3].Pin != "" {
		t.Errorf("unpinned = %+v %v", arms.Arms[3], err)
	}
}

// A second reviewer on the same gateway chain asks it to leave out the
// first reviewer's model; findings in such runs feed the recall counts.
func TestReviewArmsSecondReviewer(t *testing.T) {
	re := newReviewEnv(t)
	re.asGateway(t)
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	list, err := re.s.ListAutomations(re.ctx, re.admin, re.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Name != "Review pull requests" {
			continue
		}
		r := a.Rule
		r.Actions[0].Extra["second_models"] = []any{map[string]any{"provider_id": float64(chain[0].ProviderID), "model": museModel}}
		if _, err := re.s.UpdateAutomation(re.ctx, re.admin, a.ID, AutomationInput{Rule: &r}); err != nil {
			t.Fatal(err)
		}
	}
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	var reviews [][]string
	for n, m := range re.llm.calls {
		if m == museModel {
			reviews = append(reviews, re.llm.exclude[n])
		}
	}
	if len(reviews) != 2 || reviews[0] != nil || !slices.Contains(reviews[1], museModel) {
		t.Fatalf("the reviewers' excludes = %v", reviews)
	}
	if run.Arm != "diff+files/verify" {
		t.Errorf("arm = %q", run.Arm)
	}
	arms, err := re.s.ReviewArms(re.ctx, re.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(arms.Recall) != 1 || arms.Recall[0].Runs != 2 {
		t.Errorf("recall = %+v", arms.Recall)
	}
}
