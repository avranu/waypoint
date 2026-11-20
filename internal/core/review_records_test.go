package core

import (
	"strings"
	"sync"
	"testing"
)

func TestNormalizeModel(t *testing.T) {
	same := []string{"groq/openai/gpt-oss-120b", "cloudflare/@cf/openai/gpt-oss-120b", "ollama-eshu/gpt-oss:120b", "GPT-OSS-120B"}
	for _, a := range same {
		for _, b := range same {
			if !sameModel(a, b) {
				t.Errorf("%q and %q should be the same model (%q vs %q)", a, b, normalizeModel(a), normalizeModel(b))
			}
		}
	}
	for _, p := range [][2]string{{"groq/openai/gpt-oss-20b", "groq/openai/gpt-oss-120b"}, {"opencode/mimo-v2.6-flash-free", "opencode/muse-spark-1.3-contributor-free"}, {"", ""}} {
		if sameModel(p[0], p[1]) {
			t.Errorf("%q and %q are different models", p[0], p[1])
		}
	}
}

func TestPromptVersion(t *testing.T) {
	a, b := strategyDiff.promptVersion(), strategyDiffFiles.promptVersion()
	if len(a) != 12 || len(b) != 12 || a == b {
		t.Errorf("versions = %q, %q", a, b)
	}
	if a != strategyDiff.promptVersion() {
		t.Error("the version is not stable")
	}
	edited := reviewStrategy{name: "diff", template: strategyDiff.template + " more"}
	if edited.promptVersion() == a {
		t.Error("an edit to the template kept the version")
	}
}

func TestEvidenceHolds(t *testing.T) {
	file := "package app\n\nfunc Login() error {\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n}\n"
	for _, ev := range []string{"if user != nil {", "4| \tif user != nil {", "if   user\t!= nil {", ""} {
		if !evidenceHolds(ev, file) {
			t.Errorf("evidence %q should hold", ev)
		}
	}
	if evidenceHolds("if user == nil {", file) {
		t.Error("a line that isn't in the file held")
	}
}

func TestNumberedFileBudget(t *testing.T) {
	var lines []string
	for i := 0; i < 400; i++ {
		lines = append(lines, "line of code number "+strings.Repeat("x", 20))
	}
	text := strings.Join(lines, "\n")
	diff := "@@ -200,2 +200,3 @@ f\n a\n+b\n c\n"
	whole, cut := numberedFile(text, diff, 1<<20)
	if cut || !strings.Contains(whole, "1| ") || !strings.Contains(whole, "400| ") {
		t.Errorf("a file inside the budget was cut=%v", cut)
	}
	part, cut := numberedFile(text, diff, 6000)
	if !cut || len(part) > 6000 || !strings.Contains(part, "200| ") || strings.Contains(part, "\n10| ") || strings.Contains(part, "\n390| ") {
		t.Errorf("cut=%v len=%d:\n%s", cut, len(part), part)
	}
}

// reviewEnv is a PR environment with one reviewer (muse) and one verifier
// (mimo), a head file the evidence below is found in, and a scripted draw.
type reviewEnv struct {
	*prEnv
	draws []float64
	mu    sync.Mutex
}

const mimoModel = "opencode/mimo-v2.6-flash-free"

// newReviewEnv is a review with a verifier set up; draws script what
// Service.draw returns (posting the verifier's drops anyway), 0.99 after.
func newReviewEnv(t *testing.T, draws ...float64) *reviewEnv {
	t.Helper()
	pe := newPREnv(t, "github")
	chain, err := pe.s.taskChain(pe.ctx, "review")
	if err != nil || len(chain) == 0 {
		t.Fatal(chain, err)
	}
	list, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Name != "Review pull requests" {
			continue
		}
		r := a.Rule
		r.Actions[0].Extra = map[string]any{"verify_models": []any{map[string]any{"provider_id": float64(chain[0].ProviderID), "model": mimoModel}}}
		if _, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, a.ID, AutomationInput{Rule: &r}); err != nil {
			t.Fatal(err)
		}
	}
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() error {\n\tuser := find()\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n\treturn nil\n}\n"
	pe.fg.mu.Unlock()
	re := &reviewEnv{prEnv: pe, draws: draws}
	pe.s.rnd = func() float64 {
		re.mu.Lock()
		defer re.mu.Unlock()
		if len(re.draws) == 0 {
			return 0.99
		}
		d := re.draws[0]
		re.draws = re.draws[1:]
		return d
	}
	return re
}

// pinArm pins one review arm on, so every review uses it.
func (re *reviewEnv) pinArm(t *testing.T, key string) {
	t.Helper()
	if _, err := re.s.PinReviewArm(re.ctx, re.admin, key, "on"); err != nil {
		t.Fatal(err)
	}
}

func (re *reviewEnv) review(t *testing.T) *BotRun {
	t.Helper()
	re.pullHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"})
	done := re.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	return done[0]
}

const loginFinding = `{"findings":[{"file":"app/login.go","line":5,"severity":"high","claim":"The nil check is inverted.","suggestion":"","evidence":"if user != nil {"}]}`

func TestReviewRecordsKept(t *testing.T) {
	re := newReviewEnv(t) // diff+files and the verifier: the first arm on a tie
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	if len(run.Findings) != 1 {
		t.Fatalf("findings = %+v", run.Findings)
	}
	f := run.Findings[0]
	if f.Strategy != "diff+files" || f.PromptVersion != strategyDiffFiles.promptVersion() || f.Verify != VerifyKept || f.State != FindingOpen {
		t.Errorf("finding = %+v", f)
	}
	// The reviewer saw the whole file at the head.
	var prompt string
	for n, m := range re.llm.calls {
		if m == museModel {
			prompt = re.llm.prompts[n]
		}
	}
	if !strings.Contains(prompt, "func Login() error") || !strings.Contains(prompt, "evidence") {
		t.Errorf("reviewer prompt:\n%s", prompt)
	}
	var strategy, version string
	var verified int
	if err := re.s.db.R.QueryRow(`SELECT strategy, prompt_version, verified FROM bot_runs WHERE id = ?`, run.ID).Scan(&strategy, &version, &verified); err != nil {
		t.Fatal(err)
	}
	if strategy != "diff+files" || version != f.PromptVersion || verified != 1 {
		t.Errorf("run record = %q %q %d", strategy, version, verified)
	}
}

func TestReviewRecordsDiffStrategyAndSkip(t *testing.T) {
	re := newReviewEnv(t)
	re.pinArm(t, "diff/none")
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[]}`
	run := re.review(t)
	f := run.Findings[0]
	if f.Strategy != "diff" || f.Verify != VerifySkipped || f.State != FindingOpen || f.VerifiedBy != "" {
		t.Errorf("finding = %+v", f)
	}
	for n, m := range re.llm.calls {
		if m == mimoModel {
			t.Errorf("the verifier ran on a skipped run: %s", re.llm.prompts[n])
		}
		if m == museModel && strings.Contains(re.llm.prompts[n], "func Login() error") {
			t.Error("the diff strategy sent the file")
		}
	}
}

func TestReviewRecordsDroppedAndPostedAnyway(t *testing.T) {
	drop := `{"verdicts":[{"n":1,"verdict":"drop","reason":"the check is right","line":0,"severity":"","claim":""}]}`
	// Dropped: stored, not open, not posted.
	re := newReviewEnv(t) // the drop is not posted
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = drop
	run := re.review(t)
	f := run.Findings[0]
	if f.State != FindingDropped || f.Verify != VerifyDropped || f.VerifyReason != "the check is right" {
		t.Errorf("dropped finding = %+v", f)
	}
	if len(re.fg.lines) != 0 && len(re.fg.lines[0]) != 0 {
		t.Errorf("a dropped finding was commented on the line: %+v", re.fg.lines)
	}
	var prID int64
	if err := re.s.db.R.QueryRow(`SELECT pr_id FROM bot_findings WHERE run_id = ?`, run.ID).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	if rows, err := re.s.prFindings(re.ctx, re.s.db.R, prID); err != nil || len(rows) != 0 {
		t.Errorf("open findings include the dropped one: %d %v", len(rows), err)
	}

	// Posted anyway: open, says the verifier doubted it and why.
	re = newReviewEnv(t, 0.0) // the drop is posted anyway
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = drop
	run = re.review(t)
	f = run.Findings[0]
	if f.State != FindingOpen || f.Verify != VerifyDropped || f.VerifyReason != "the check is right" {
		t.Errorf("posted-anyway finding = %+v", f)
	}
	posted := strings.Join(re.fg.reviewsOn(re.f, "me/app", 5), "\n")
	if len(re.fg.lines) > 0 {
		for _, l := range re.fg.lines[0] {
			posted += "\n" + l.Body
		}
	}
	if !strings.Contains(posted, "doubted") || !strings.Contains(posted, "the check is right") {
		t.Errorf("posted text lacks the doubt:\n%s", posted)
	}
}

func TestReviewRecordsEvidenceNotInFile(t *testing.T) {
	re := newReviewEnv(t)
	re.llm.answers[museModel] = `{"findings":[{"file":"app/login.go","line":5,"severity":"high","claim":"Made up.","suggestion":"","evidence":"if user == nil { panic() }"}]}`
	re.llm.answers[mimoModel] = `{"verdicts":[]}`
	run := re.review(t)
	f := run.Findings[0]
	if f.State != FindingDropped || f.Verify != VerifyUnchecked || f.VerifyReason != "evidence not in the file" {
		t.Errorf("finding = %+v", f)
	}
	for _, m := range re.llm.calls {
		if m == mimoModel {
			t.Error("the verifier saw a finding whose evidence was not in the file")
		}
	}
}

// A verifier that is the finding's own reviewer doesn't grade its own work:
// its verdict is discarded and the next model in the chain answers.
func TestVerifyNewSkipsTheReviewersOwnModel(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() {}\n"
	pe.fg.mu.Unlock()
	chain, _ := pe.s.taskChain(pe.ctx, "review")
	pid := chain[0].ProviderID
	vchain := []ChainModel{{ProviderID: pid, Model: "groq/openai/gpt-oss-120b"}, {ProviderID: pid, Model: mimoModel}}
	pe.llm.answers["groq/openai/gpt-oss-120b"] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"mine is fine","line":0,"severity":"","claim":""}]}`
	pe.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"drop","reason":"independent view","line":0,"severity":"","claim":""}]}`
	found := []Finding{{File: "app/login.go", Line: 3, Severity: "high", Claim: "First.", Model: "ollama-eshu/gpt-oss:120b"}}
	cl := pe.s.Forges.(ForgePRs)
	v := pe.s.verifyNew(pe.ctx, vchain, pe.f, cl, "me/app", "abc", found, splitDiff(sampleDiff))
	if len(v.dropped) != 1 || v.dropped[0].by != mimoModel || v.dropped[0].reason != "independent view" || len(v.kept) != 0 {
		t.Errorf("verified = %+v", v)
	}

	// Nothing but its own model left: the finding stays, unchecked.
	v = pe.s.verifyNew(pe.ctx, vchain[:1], pe.f, cl, "me/app", "abc", found, splitDiff(sampleDiff))
	if len(v.kept) != 1 || v.kept[0].Verify != VerifyUnchecked || v.unchecked != 1 || len(v.dropped) != 0 {
		t.Errorf("only the reviewer's own model = %+v", v)
	}
}

// The run keeps its strategy, prompt version and verifier, and a finding
// with no evidence at all (a model that leaves the field out) is still taken.
func TestReviewRecordsRunAndMissingEvidence(t *testing.T) {
	re := newReviewEnv(t)
	re.llm.answers[museModel] = `{"findings":[{"file":"app/login.go","line":5,"severity":"high","claim":"The nil check is inverted.","suggestion":""}]}`
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	if len(run.Findings) != 1 || run.Findings[0].Verify != VerifyKept {
		t.Fatalf("findings = %+v", run.Findings)
	}
	got, err := re.s.botRun(re.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != "diff+files" || !got.Verified || got.PromptVersion == "" {
		t.Errorf("run = strategy %q verified %v version %q", got.Strategy, got.Verified, got.PromptVersion)
	}
}

func TestCheckAnswerEvidenceMayBeLeftOut(t *testing.T) {
	if err := checkAnswer(reviewSchema, []byte(`{"findings":[{"file":"a.go","line":1,"severity":"low","claim":"c","suggestion":""}]}`)); err != nil {
		t.Errorf("a finding without evidence: %v", err)
	}
	if err := checkAnswer(reviewSchema, []byte(`{"findings":[{"file":"a.go","line":1,"severity":"low","suggestion":""}]}`)); err == nil {
		t.Error("a finding without a claim passed")
	}
}
