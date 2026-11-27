package core

import (
	"errors"
	"math"
	"testing"
)

func TestFateOf(t *testing.T) {
	for _, c := range []struct {
		f    statFinding
		want string
	}{
		{statFinding{state: FindingFixed}, fateUseful},
		{statFinding{state: FindingOpen}, fateUnjudged},
		{statFinding{state: FindingOpen, taken: true}, fateUseful},
		{statFinding{state: FindingResolved}, fateUnjudged},
		{statFinding{state: FindingResolved, taken: true}, fateUseful},
		{statFinding{state: FindingDismissed}, fateFalse},
		{statFinding{state: FindingWithdrawn}, fateFalse},
		{statFinding{state: FindingDropped, verify: VerifyUnchecked, reason: evidenceMissing}, fateEvidence},
		{statFinding{state: FindingDropped, verify: VerifyDropped, reason: "not a bug"}, fateWithheld},
		{statFinding{state: FindingOpen, audit: "confirmed"}, fateUseful},
		{statFinding{state: FindingFixed, audit: "rejected"}, fateFalse},
		{statFinding{state: FindingOpen, audit: "open"}, fateUnjudged},
		{statFinding{state: FindingFixed, audit: "open"}, fateUseful},
	} {
		if got := fateOf(c.f); got != c.want {
			t.Errorf("fateOf(%+v) = %s, want %s", c.f, got, c.want)
		}
	}
}

func TestJudgedInterval(t *testing.T) {
	var j Judged
	j.settle()
	if j.Precision != nil || j.Low != nil {
		t.Fatalf("nothing judged = %+v", j)
	}
	j = Judged{Useful: 8, False: 2, Unjudged: 5}
	j.settle()
	if *j.Precision != 0.8 {
		t.Errorf("precision = %v", *j.Precision)
	}
	if lo, hi := *j.Low, *j.High; math.Abs(lo-0.530) > 0.002 || math.Abs(hi-0.921) > 0.002 {
		t.Errorf("interval = %v..%v, want about 0.530..0.921", lo, hi)
	}
	j = Judged{False: 1}
	j.settle()
	if *j.Precision != 0 || math.Abs(*j.Low-round3(1-math.Sqrt(0.95))) > 1e-9 || math.Abs(*j.High-round3(1-math.Sqrt(0.05))) > 1e-9 {
		t.Errorf("0 of 1 = %v %v..%v", *j.Precision, *j.Low, *j.High)
	}
}

func TestTallyReviewStats(t *testing.T) {
	fs := []statFinding{
		{run: 1, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v1", state: FindingFixed, verify: VerifyKept, verifier: "zai/glm-5"},
		{run: 1, model: "ollama-eshu/gpt-oss:120b", strategy: "diff", version: "v1", state: FindingDismissed, verify: VerifyCorrected, verifier: "zai/glm-5"},
		{run: 1, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v1", state: FindingOpen, verify: VerifyDropped, verifier: "zai/glm-5"},
		{run: 1, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v1", state: FindingDismissed, verify: VerifyDropped, verifier: "zai/glm-5"},
		{run: 1, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v1", state: FindingDropped, verify: VerifyDropped, verifier: "zai/glm-5"},
		{run: 1, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v1", state: FindingDropped, verify: VerifyUnchecked, reason: evidenceMissing},
		{run: 2, model: "openrouter/qwen/qwen3-coder", strategy: "diff+files", version: "v2", state: FindingOpen, verify: VerifySkipped, taken: true},
		{run: 2, model: "openrouter/qwen/qwen3-coder", strategy: "diff+files", version: "v2", state: FindingOpen, verify: VerifySkipped, audit: "rejected"},
		{run: 3, model: "groq/openai/gpt-oss-120b", strategy: "diff", version: "v0", state: FindingFixed, verify: VerifyUnchecked},
	}
	runs := []statRun{
		{id: 1, strategy: "diff", version: "v1", arm: "diff/verify", verified: true, review: 1000, verify: 301},
		{id: 2, strategy: "diff+files", version: "v2", arm: "diff+files/none", reviewers: []string{"openrouter/qwen/qwen3-coder", "groq/kimi-k3"}, review: 3001},
		{id: 3, strategy: "diff", version: "v0", review: 500},
		{id: 4, strategy: "diff", version: "v1", review: 70},
	}
	st := tallyReviewStats(fs, runs)

	if tt := st.Totals; tt.Findings != 9 || tt.Useful != 3 || tt.False != 3 || tt.Unjudged != 1 || tt.Evidence != 1 || tt.Withheld != 1 ||
		tt.Runs != 4 || tt.Tokens != 4872 || tt.TokensPerUseful != 1624 || *tt.Precision != 0.5 {
		t.Errorf("totals = %+v", tt)
	}
	byKey := map[string]ReviewerStat{}
	for _, r := range st.Reviewers {
		byKey[r.Model+" "+r.Strategy+" "+r.PromptVersion] = r
	}
	if len(byKey) != 4 {
		t.Fatalf("reviewers = %+v", st.Reviewers)
	}
	g := byKey["gpt-oss-120b diff v1"]
	if g.Findings != 6 || g.Useful != 1 || g.False != 2 || g.Unjudged != 1 || g.Evidence != 1 || g.Withheld != 1 ||
		g.Tokens != 1000 || g.TokensPerUseful != 1000 || g.Runs != 1 || *g.Precision != 0.333 {
		t.Errorf("gpt-oss v1 = %+v", g)
	}
	if len(g.Providers) != 2 || g.Providers[0] != "groq" || g.Providers[1] != "ollama-eshu" {
		t.Errorf("providers = %v", g.Providers)
	}
	if o := byKey["gpt-oss-120b diff v0"]; o.Findings != 1 || o.Useful != 1 || o.Tokens != 500 {
		t.Errorf("gpt-oss v0 = %+v", o)
	}
	q := byKey["qwen3-coder diff+files v2"]
	if q.Useful != 1 || q.False != 1 || q.Tokens != 1501 || q.TokensPerUseful != 1501 {
		t.Errorf("qwen = %+v", q)
	}
	if k := byKey["kimi-k3 diff+files v2"]; k.Findings != 0 || k.Tokens != 1500 || k.Runs != 1 || k.Precision != nil || k.Providers[0] != "groq" {
		t.Errorf("kimi = %+v", k)
	}
	if st.Reviewers[0].Model != "gpt-oss-120b" || st.Reviewers[0].PromptVersion != "v1" {
		t.Errorf("first = %+v", st.Reviewers[0])
	}

	if len(st.Verifiers) != 1 {
		t.Fatalf("verifiers = %+v", st.Verifiers)
	}
	v := st.Verifiers[0]
	if v.Model != "glm-5" || v.Providers[0] != "zai" || v.Kept.Useful != 1 || v.Kept.False != 1 || v.Dropped != 3 ||
		v.Posted.Useful != 0 || v.Posted.False != 1 || v.Posted.Unjudged != 1 || v.Tokens != 301 || v.Runs != 1 {
		t.Errorf("verifier = %+v", v)
	}
	if v.DropAccuracy == nil || *v.DropAccuracy != 1 || *v.DropHigh != round3(1-*v.Posted.Low) {
		t.Errorf("drop accuracy = %v %v..%v", v.DropAccuracy, v.DropLow, v.DropHigh)
	}

	vf, sk := st.Verify.Verified, st.Verify.Skipped
	if vf.Findings != 2 || vf.Useful != 1 || vf.False != 1 || vf.Runs != 1 || vf.Tokens != 1301 || vf.TokensPerUseful != 1301 {
		t.Errorf("verified = %+v", vf)
	}
	if sk.Findings != 2 || sk.Useful != 1 || sk.False != 1 || sk.Runs != 1 || sk.Tokens != 3001 {
		t.Errorf("skipped = %+v", sk)
	}
}

// A run that went without a verifier only because of who wrote its findings
// says nothing about going without one.
func TestTallyTrustedRunIsNotNoVerifier(t *testing.T) {
	haiku := statFinding{run: 1, model: "claude-haiku-5-5", strategy: "diff", version: "v1", state: FindingFixed, verify: VerifyTrusted}
	mixed := haiku
	mixed.run = 2
	other := mixed
	other.verify, other.model = VerifySkipped, "groq/kimi-k3"
	st := tallyReviewStats([]statFinding{haiku, mixed, other}, []statRun{
		{id: 1, strategy: "diff", version: "v1", arm: "diff/none", review: 100},
		{id: 2, strategy: "diff", version: "v1", arm: "diff/none", review: 20},
		{id: 3, strategy: "diff", version: "v1", arm: "diff/none", review: 3},
	})
	if sk := st.Verify.Skipped; sk.Findings != 1 || sk.Runs != 2 || sk.Tokens != 23 {
		t.Errorf("skipped = %+v", sk)
	}
}

func TestReviewStatsService(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.pullHook("opened", ForgePR{Number: 4, Title: "Export", HeadRef: "export", HeadSHA: "abc1234def", Author: "someone"})
	var prID int64
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT id FROM pull_requests WHERE number = 4`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	now := pe.s.nowMS()
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := pe.s.db.W.ExecContext(pe.ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	run := exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at, strategy, prompt_version, arm, verified)
		VALUES (?, ?, 'review', 'abc1234def', 'done', ?, 'diff', 'v1', 'diff/none', 0)`, pe.p.ID, prID, now)
	old := exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, ?, 'review', 'abc', 'done', ?)`, pe.p.ID, prID, now-40*86400000)
	finding := func(run int64, state, model string) int64 {
		return exec(`INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, state, placement, ordinal, created_at, updated_at, strategy, prompt_version, verify)
			VALUES (?, ?, 'x.go', 1, 'high', 'c', ?, ?, 'inline', 1, ?, ?, 'diff', 'v1', 'skipped')`, run, prID, model, state, now, now)
	}
	taken := finding(run, FindingOpen, "groq/kimi-k3")
	exec(`INSERT INTO review_comments(pr_id, kind, ext_id, finding_id, verdict, created_at, updated_at, seen_at) VALUES (?, 'finding', 'f1', ?, ?, ?, ?, ?)`,
		prID, taken, VerdictAccepted, now, now, now)
	finding(run, FindingDismissed, "groq/kimi-k3")
	left := finding(run, FindingOpen, "groq/kimi-k3")
	audit := exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at) VALUES (?, ?, 'me/app', 'pr', ?, ?)`, pe.p.ID, pe.f.ID, pe.admin.User.ID, now)
	af := exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, state) VALUES (?, 1, 't', 'confirmed')`, audit)
	exec(`UPDATE bot_findings SET audit_finding_id = ? WHERE id = ?`, af, left)
	finding(old, FindingFixed, "groq/kimi-k3")

	st, err := pe.s.ReviewStats(pe.ctx, pe.admin, ReviewStatsInput{Project: pe.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if st.Days != 30 || len(st.Reviewers) != 1 {
		t.Fatalf("stats = %+v", st)
	}
	r := st.Reviewers[0]
	if r.Model != "kimi-k3" || r.Findings != 3 || r.Useful != 2 || r.False != 1 {
		t.Errorf("reviewer = %+v", r)
	}
	if st.Verify.Skipped.Runs != 1 || st.Verify.Skipped.Findings != 3 {
		t.Errorf("skipped = %+v", st.Verify.Skipped)
	}
	if st, err = pe.s.ReviewStats(pe.ctx, pe.admin, ReviewStatsInput{Days: 90}); err != nil || st.Totals.Findings != 4 {
		t.Fatalf("90 days = %+v, %v", st, err)
	}
	if _, err := pe.s.ReviewStats(pe.ctx, pe.admin, ReviewStatsInput{Days: 1000}); err == nil {
		t.Error("1000 days allowed")
	}
	bob := pe.user("bob", false)
	if _, err := pe.s.ReviewStats(pe.ctx, bob, ReviewStatsInput{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob, every project: %v", err)
	}
	if _, err := pe.s.ReviewStats(pe.ctx, bob, ReviewStatsInput{Project: pe.p.Key}); err == nil {
		t.Error("bob read a project not theirs")
	}
}
