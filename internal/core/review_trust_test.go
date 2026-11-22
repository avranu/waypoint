package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A trusted reviewer's findings cost no verifier call, say so, and count for
// neither arm.
func TestTrustedReviewerSkipsVerifier(t *testing.T) {
	re := newReviewEnv(t)
	re.s.trustSeeds = []string{"muse-spark-1.3"}
	re.asGateway(t)
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"drop","reason":"no","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	if run.Arm != "diff+files/verify" || len(run.Findings) != 1 || run.Findings[0].Verify != VerifyTrusted {
		t.Fatalf("run = %+v", run)
	}
	if slices.Contains(re.llm.calls, mimoModel) {
		t.Errorf("a verifier was asked: %v", re.llm.calls)
	}
	if strings.Contains(run.Summary, "checked by") || !strings.Contains(run.Summary, "1 from a trusted reviewer") {
		t.Errorf("summary = %q", run.Summary)
	}
	if _, err := re.s.db.W.Exec(`UPDATE bot_findings SET state = ? WHERE run_id = ?`, FindingFixed, run.ID); err != nil {
		t.Fatal(err)
	}
	tallies, err := re.s.armTallies(re.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for k, a := range tallies {
		if a.judged() != 0 {
			t.Errorf("arm %s counted a trusted finding: %+v", k, a)
		}
	}
}

// On an arm without a verifier a trusted reviewer's findings are still
// marked trusted, or that arm would look good on their account.
func TestTrustedReviewerOnNoVerifierArm(t *testing.T) {
	re := newReviewEnv(t)
	re.s.trustSeeds = []string{"muse-spark-1.3"}
	re.pinArm(t, "diff/none")
	re.llm.answers[museModel] = loginFinding
	run := re.review(t)
	if len(run.Findings) != 1 || run.Findings[0].Verify != VerifyTrusted {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(run.Summary, "1 from a trusted reviewer") {
		t.Errorf("summary = %q", run.Summary)
	}
}

// The summary counts the trusted findings the review kept: the ones posted
// and the ones past the shown number, which are kept and given to the agent
// too (WP-1421).
func TestTrustedCountIsWhatWasKept(t *testing.T) {
	re := newReviewEnv(t)
	re.s.trustSeeds = []string{"muse-spark-1.3"}
	var fs []string
	for n := range 12 {
		fs = append(fs, fmt.Sprintf(`{"file":"app/login.go","line":%d,"severity":"high","claim":"Problem number %d.","suggestion":"","evidence":""}`, n+1, n))
	}
	re.llm.answers[museModel] = `{"findings":[` + strings.Join(fs, ",") + `]}`
	run := re.review(t)
	if len(run.Findings) != 12 || !strings.Contains(run.Summary, "8 findings and 4 more kept, not posted") ||
		!strings.Contains(run.Summary, "; 12 from a trusted reviewer") {
		t.Errorf("%d findings, summary = %q", len(run.Findings), run.Summary)
	}
}

// Who wrote the finding decides, not who agreed with it: the claim posted is
// the writer's.
func TestMarkTrustedGoesByTheWriter(t *testing.T) {
	re := newReviewEnv(t)
	re.s.trustSeeds = []string{"muse-spark-1.3"}
	found := re.s.markTrusted(re.ctx, []Finding{
		{Claim: "written by the other", Model: "groq/kimi-k3", Models: []string{"groq/kimi-k3", museModel}},
		{Claim: "written by the trusted", Model: museModel, Models: []string{museModel, "groq/kimi-k3"}},
	})
	if found[0].Verify == VerifyTrusted || found[1].Verify != VerifyTrusted {
		t.Errorf("found = %+v", found)
	}
}

// Records that can't be read leave the findings to the verifier; the review
// goes on.
func TestMarkTrustedWithoutRecords(t *testing.T) {
	re := newReviewEnv(t)
	re.s.trustSeeds = []string{"muse-spark-1.3"}
	ctx, cancel := context.WithCancel(re.ctx)
	cancel()
	found := re.s.markTrusted(ctx, []Finding{{Claim: "c", Model: museModel}})
	if len(found) != 1 || found[0].Verify == VerifyTrusted {
		t.Errorf("found = %+v", found)
	}
}

// The names a seeded model goes by, and those that only look like one.
func TestSeedNames(t *testing.T) {
	trust := reviewerTrust{seeds: trustedReviewerSeeds, measured: map[string]bool{}}
	for model, want := range map[string]bool{
		"claude-subscription/claude-haiku-5-5":           true,
		"claude-opus-5-5-20260401":                       true,
		"cursor/grok-4.7-high":                           true,
		"deepseek/deepseek-v4.1-flash":                   true,
		"claude haiku (runner)":                          true,
		"claude sonnet (runner)":                         true,
		"claude claude-opus-5-5 (runner)":                true,
		"opencode opencode/muse-spark-1.3-free (runner)": true,
		"claude (runner)":                                false,
		"claude some-other-model (runner)":               false,
		"cursor/grok-4.70":                               false,
		"claude-haiku-5-50":                              false,
		"grok-4.7.1":                                     false,
	} {
		if got := trust.trusts(model); got != want {
			t.Errorf("trusts(%q) = %v, want %v", model, got, want)
		}
	}
	if m, _ := trust.recordOf("claude haiku (runner)"); m != "claude-haiku-5-5" {
		t.Errorf("a runner's findings count under %q", m)
	}
}

// Enough judged findings decide trust either way, for every variant of a
// seed; fewer leave it to the seed list, and findings past the window don't
// count.
func TestReviewerTrustFollowsFindings(t *testing.T) {
	re := newReviewEnv(t)
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	var prID int64
	if err := re.s.db.R.QueryRow(`SELECT pr_id FROM bot_runs WHERE id = ?`, run.ID).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	if _, err := re.s.db.W.Exec(`DELETE FROM bot_findings`); err != nil {
		t.Fatal(err)
	}
	now := re.s.nowMS()
	add := func(model, state, verify string, n int, at int64) {
		t.Helper()
		for range n {
			if _, err := re.s.db.W.Exec(`INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, state, placement, ordinal, created_at, updated_at, strategy, prompt_version, verify)
				VALUES (?, ?, 'x.go', 1, 'high', 'c', ?, ?, 'inline', 1, ?, ?, 'diff', 'v1', ?)`, run.ID, prID, model, state, at, at, verify); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("anthropic/claude-haiku-5-5", FindingDismissed, VerifyTrusted, 20, now) // seeded, measured bad
	add("groq/kimi-k3", FindingFixed, VerifyKept, 20, now)                      // not seeded, measured good
	add("zai/glm-5", FindingFixed, VerifySkipped, 20, now)                      // good, with a few false
	add("zai/glm-5", FindingDismissed, VerifyDropped, 2, now)
	add("groq/gpt-oss-120b", FindingFixed, VerifyKept, 5, now) // too few to say
	add("old/nemotron", FindingFixed, VerifyKept, 30, now-100*86400000)
	add("cursor/grok-4.7-high", FindingDismissed, VerifyTrusted, 20, now)                  // one level measured bad
	add("inferx/deepseek-v4-1-flash-basic", FindingDismissed, VerifyTrusted, 20, now)      // another spelling
	add("anthropic/claude-opus-5-5", FindingDismissed, VerifyTrusted, 20, now-40*86400000) // old, but enough
	add("anthropic/claude-sonnet-5-5", FindingDropped, VerifyUnchecked, 20, now)           // quotes not in the file
	if _, err := re.s.db.W.Exec(`UPDATE bot_findings SET verify_reason = ? WHERE model = 'anthropic/claude-sonnet-5-5'`, evidenceMissing); err != nil {
		t.Fatal(err)
	}
	add("opencode/muse-spark-1.3", FindingDropped, VerifyDropped, 20, now) // a seed: the verifier's opinion, not a judgment
	add("groq/filtered", FindingFixed, VerifyKept, 20, now)                // not seeded: good only through its verifier
	add("groq/filtered", FindingDropped, VerifyDropped, 60, now)
	re.s.trustSeeds = nil
	trust, err := re.s.reviewerTrust(re.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{
		"claude-subscription/claude-haiku-5-5": false,
		"anthropic-credits/claude-haiku-5-5":   false,
		"openrouter/kimi-k3":                   true,
		"zai/glm-5":                            true,
		"groq/filtered":                        false,
		"groq/gpt-oss-120b":                    false,
		"old/nemotron":                         false,
		"claude-haiku-5-5-20260401":            false,
		"cursor/grok-4.7-medium":               false,
		"anthropic/claude-opus-5-5":            false,
		"anthropic/claude-sonnet-5-5":          false,
		"deepseek/deepseek-v4.1-flash":         false,
		"deepseek-4.1-flash":                   false,
		museModel:                              true,
		mimoModel:                              false,
		"":                                     false,
	} {
		if got := trust.trusts(model); got != want {
			t.Errorf("trusts(%q) = %v, want %v", model, got, want)
		}
	}
}

// Findings from a trusted reviewer and another: only the other's is put to
// the verifier, and the order of the findings holds.
func TestTrustedFindingsSkipVerifierItems(t *testing.T) {
	re := newReviewEnv(t)
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	re.llm.answers[museModel] = `{"verdicts":[{"n":2,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	files := []fileDiff{{path: "app/login.go", text: "@@ -1 +1 @@\n-a\n+b\n"}}
	found := []Finding{
		{File: "app/login.go", Line: 1, Claim: "alreadytrustedclaim", Model: museModel, Verify: VerifyTrusted},
		{File: "app/login.go", Line: 1, Claim: "stillcheckedclaim", Model: "groq/kimi-k3"},
	}
	hf := newHeadFiles(re.ctx, re.fg, re.f, "me/app", "1111111aaaa")
	v := re.s.verifyNewIn(re.ctx, chain, hf, "me/app", "1111111aaaa", found, files)
	if len(v.kept) != 2 || v.kept[0].Claim != "alreadytrustedclaim" || v.kept[0].Verify != VerifyTrusted || v.kept[1].Verify != VerifyKept {
		t.Errorf("verified = %+v", v)
	}
	if v.unchecked != 0 {
		t.Errorf("unchecked = %d", v.unchecked)
	}
	for _, p := range re.llm.prompts {
		if strings.Contains(p, "alreadytrustedclaim") {
			t.Errorf("the verifier saw the trusted finding: %s", p)
		}
	}
}
