package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Chain entries that name a coding agent on a runner (WP-6).

const museModel = "opencode/muse-spark-1.3-contributor-free"

type chainResult struct {
	used     ChainModel
	attempts []ModelAttempt
	err      error
	out      struct {
		Findings []Finding `json:"findings"`
	}
}

// runnerChainEnv is the agent environment with a fast job poll and a bot
// run's runner scope for the admin.
func runnerChainEnv(t *testing.T) (*agentEnv, context.Context) {
	t.Helper()
	old := runnerPollEvery
	runnerPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { runnerPollEvery = old })
	ae := newAgentEnv(t)
	ctx := withRunnerScope(ae.ctx, &runnerScope{owner: ae.admin.User.ID, repo: "https://git.example.com/me/app.git", base: "abc1234", runID: 7, attempt: 1})
	return ae, ctx
}

func (ae *agentEnv) providerID() int64 {
	ae.t.Helper()
	chain, err := ae.s.taskChain(ae.ctx, "review")
	if err != nil || len(chain) == 0 {
		ae.t.Fatal(chain, err)
	}
	return chain[0].ProviderID
}

func (ae *agentEnv) startChain(ctx context.Context, chain []ChainModel) <-chan *chainResult {
	done := make(chan *chainResult, 1)
	go func() {
		r := &chainResult{}
		r.used, r.attempts, r.err = ae.s.runChain(ctx, chain, "sys", "review this", reviewSchema, &r.out, 10*time.Millisecond)
		done <- r
	}()
	return done
}

// takeSoon is the runner taking the job the chain queues in the background.
func (ae *agentEnv) takeSoon() *RunnerClaim {
	ae.t.Helper()
	for range 400 {
		if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
			return w.Job
		}
		time.Sleep(5 * time.Millisecond)
	}
	ae.t.Fatal("the chain queued no job")
	return nil
}

func (ae *agentEnv) result(done <-chan *chainResult) *chainResult {
	ae.t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		ae.t.Fatal("runChain didn't return")
		return nil
	}
}

func TestRunChainRunsACLIEntryOnARunner(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude", Model: "sonnet"}, {ProviderID: ae.providerID(), Model: museModel}})
	claim := ae.takeSoon()
	in := agentJobOf(t, claim)
	if claim.Kind != JobReview || in.CLI != "claude" || in.Model != "sonnet" || !in.ReadOnly || in.Push || in.Branch != "" ||
		in.Repo != "https://git.example.com/me/app.git" || in.Base != "abc1234" || !strings.Contains(in.Prompt, "sys") || !strings.Contains(in.Prompt, "review this") ||
		!strings.Contains(in.JSONSchema, "findings") {
		t.Fatalf("job = %v %+v", claim.Kind, in)
	}
	ae.finish(claim.ID, "done", map[string]any{"answer": "ignored", "structured": map[string]any{"findings": []any{
		map[string]any{"file": "a.go", "line": 3, "severity": "high", "claim": "Bug.", "suggestion": ""}}}}, "")
	r := ae.result(done)
	if r.err != nil || r.used.Model != "claude sonnet (runner)" || len(r.attempts) != 1 || r.attempts[0].Outcome != "ok" || r.attempts[0].Model != "claude sonnet (runner)" {
		t.Fatalf("result = %+v", r)
	}
	if len(r.out.Findings) != 1 || r.out.Findings[0].File != "a.go" {
		t.Errorf("findings = %+v", r.out.Findings)
	}
	if len(ae.llm.calls) != 0 {
		t.Errorf("a provider was called too: %v", ae.llm.calls)
	}
}

func TestRunChainReadsJSONFromTheAnswer(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude"}})
	claim := ae.takeSoon()
	ae.finish(claim.ID, "done", map[string]any{"answer": "Here you go:\n```json\n{\"findings\": []}\n```"}, "")
	r := ae.result(done)
	if r.err != nil || r.used.Model != "claude (runner)" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunChainBadAnswerMovesOn(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}})
	claim := ae.takeSoon()
	ae.finish(claim.ID, "done", map[string]any{"answer": "I looked and it seems fine."}, "")
	r := ae.result(done)
	if r.err != nil || len(r.attempts) != 2 || r.attempts[0].Outcome != "bad_output" || r.attempts[1].Outcome != "ok" || r.used.Model != museModel {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunChainModelFailureMovesOn(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}})
	claim := ae.takeSoon()
	ae.finish(claim.ID, "failed", nil, "claude reported an error: 429 rate limit, try again at 5pm")
	r := ae.result(done)
	if r.err != nil || len(r.attempts) != 2 || r.attempts[0].Outcome != "rate_limit" || !strings.Contains(r.attempts[0].Error, "rate limit") || r.used.Model != museModel {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunChainWithoutARunnerIsUnavailable(t *testing.T) {
	ae, _ := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	// Bob's only runner has opencode; the rule is bob's, and claude isn't on it. The admin's
	// runner (which has claude) never serves it.
	bob := ae.user("bob", false)
	ae.runner(bob, "bobbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "opencode"}}
		r.Features = []string{FeatureConfine}
	})
	ctx := withRunnerScope(ae.ctx, &runnerScope{owner: bob.User.ID, repo: "https://git.example.com/me/app.git", base: "abc", runID: 11, attempt: 1})
	r := ae.result(ae.startChain(ctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}}))
	if r.err != nil || len(r.attempts) != 2 || r.attempts[0].Outcome != "unavailable" || r.attempts[0].Model != "claude (runner)" ||
		!strings.Contains(r.attempts[0].Error, "bob has no runner with claude signed in: connect one under Settings → Runners") || r.attempts[1].Outcome != "ok" {
		t.Fatalf("result = %+v", r)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("the admin's runner took bob's rule's job: %+v", w.Job)
	}
}

func TestRunChainRefusesCLIsItCannotConfine(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	for _, cli := range []string{"codex", "cursor"} {
		r := ae.result(ae.startChain(ctx, []ChainModel{{CLI: cli}, {ProviderID: pid, Model: museModel}}))
		if r.err != nil || r.attempts[0].Outcome != "unavailable" || !strings.Contains(r.attempts[0].Error, "can't be confined") || r.attempts[1].Outcome != "ok" {
			t.Errorf("%s: %+v", cli, r)
		}
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a job was queued: %+v", w.Job)
	}
}

func TestRunChainRefusesWhatTheGateDenied(t *testing.T) {
	ae, _ := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	ctx := withRunnerScope(ae.ctx, &runnerScope{owner: ae.admin.User.ID, repo: "x", base: "abc", runID: 12, denied: "mallory isn't a member of this project"})
	r := ae.result(ae.startChain(ctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}}))
	if r.err != nil || r.attempts[0].Outcome != "unavailable" || !strings.Contains(r.attempts[0].Error, "mallory") || r.attempts[1].Outcome != "ok" {
		t.Fatalf("result = %+v", r)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a job was queued: %+v", w.Job)
	}
}

func TestRunnerGate(t *testing.T) {
	ae, _ := runnerChainEnv(t)
	p := ae.p
	ae.user("vic", false)
	ae.s.SetMember(ae.ctx, ae.admin, p.Key, "vic", "viewer")
	ae.user("eddie", false)
	ae.s.SetMember(ae.ctx, ae.admin, p.Key, "eddie", "editor")
	gate := func(author, headRepo string) string {
		return ae.s.runnerGate(ae.ctx, p.ID, "me/app", &ForgePR{Author: author, HeadRepo: headRepo})
	}
	for _, c := range []struct {
		author, head string
		ok           bool
		why          string
	}{
		{"robin", "me/app", true, ""},           // the project's owner
		{"EDDIE", "ME/app", true, ""},           // an editor, any case
		{"vic", "me/app", false, "member"},      // a viewer can't edit
		{"mallory", "me/app", false, "member"},  // a stranger with push access
		{"robin", "mallory/app", false, "fork"}, // a member's name on a fork
		{"robin", "", false, "fork"},            // the forge didn't say
		{"", "me/app", false, "member"},
	} {
		got := gate(c.author, c.head)
		if (got == "") != c.ok || (!c.ok && !strings.Contains(got, c.why)) {
			t.Errorf("gate(%q, %q) = %q", c.author, c.head, got)
		}
	}
	// A stranger's comment in a thread keeps it off the runners; a member's or a bot's doesn't.
	scope := func() *runnerScope { return &runnerScope{owner: ae.admin.User.ID} }
	thread := func(authors ...string) []ReviewThread {
		var cs []ThreadComment
		for _, a := range authors {
			cs = append(cs, ThreadComment{Author: a, Body: "hello"})
		}
		return []ReviewThread{{Comments: cs}}
	}
	for _, c := range []struct {
		authors []string
		denied  bool
	}{{[]string{"robin", "eddie"}, false}, {[]string{"robin", "renovate[bot]"}, false}, {[]string{"robin", "mallory"}, true}, {[]string{"vic"}, true}} {
		sc := scope()
		ae.s.gateThreads(withRunnerScope(ae.ctx, sc), p.ID, thread(c.authors...))
		if (sc.denied != "") != c.denied {
			t.Errorf("threads by %v: denied = %q", c.authors, sc.denied)
		}
	}
}

func TestRunChainNeedsAnOwner(t *testing.T) {
	ae, _ := runnerChainEnv(t)
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	ownerless := withRunnerScope(ae.ctx, &runnerScope{owner: 0, repo: "https://git.example.com/me/app.git", base: "abc", runID: 8})
	r := ae.result(ae.startChain(ownerless, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}}))
	if r.err != nil || r.attempts[0].Outcome != "unavailable" || !strings.Contains(r.attempts[0].Error, "no owner for runners: save it yourself") || r.attempts[1].Outcome != "ok" {
		t.Fatalf("result = %+v", r)
	}
	// No bot run at all (Admin → AI's tasks): unavailable too.
	r = ae.result(ae.startChain(ae.ctx, []ChainModel{{CLI: "claude"}}))
	if r.err == nil || r.attempts[0].Outcome != "unavailable" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunChainCancelsItsJobWhenStopped(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	cctx, cancel := context.WithCancel(ctx)
	done := ae.startChain(cctx, []ChainModel{{CLI: "claude"}})
	claim := ae.takeSoon()
	cancel()
	r := ae.result(done)
	if r.err == nil || len(r.attempts) != 1 || r.attempts[0].Outcome != "cancelled" {
		t.Fatalf("result = %+v", r)
	}
	info, err := ae.s.GetRunnerJob(ae.ctx, ae.admin, claim.ID, -1)
	if err != nil || info.State != "cancelled" {
		t.Errorf("job = %+v %v", info, err)
	}
}

func TestRunChainCancelsAJobThatOutlastsItsTime(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	old := runnerCallMin
	runnerCallMin = time.Second
	t.Cleanup(func() { runnerCallMin = old })
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}})
	claim := ae.takeSoon() // taken, never finished
	r := ae.result(done)
	if r.err != nil || r.attempts[0].Outcome != "timeout" || r.attempts[1].Outcome != "ok" {
		t.Fatalf("result = %+v", r)
	}
	info, _ := ae.s.GetRunnerJob(ae.ctx, ae.admin, claim.ID, -1)
	if info == nil || info.State != "cancelled" {
		t.Errorf("job = %+v", info)
	}
}

func TestStartingABotRunAgainStopsItsOldJobs(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	done := ae.startChain(ctx, []ChainModel{{CLI: "claude"}})
	claim := ae.takeSoon()
	_ = done
	ae.s.cancelBotRunJobs(ae.ctx, 7)
	info, _ := ae.s.GetRunnerJob(ae.ctx, ae.admin, claim.ID, -1)
	if info == nil || info.State != "cancelled" {
		t.Errorf("job = %+v", info)
	}
	if r := ae.result(done); r.err == nil {
		t.Errorf("a cancelled job gave an answer: %+v", r)
	}
}

func TestPRRunnerBase(t *testing.T) {
	gh := &Forge{Kind: "github"}
	gl := &Forge{Kind: "gitlab"}
	same := &ForgePR{HeadSHA: "abc123", HeadRepo: "me/app"}
	fork := &ForgePR{HeadSHA: "abc123", HeadRepo: "sam/app"}
	if got := prRunnerBase(gh, "me/app", 5, same); got != "abc123" {
		t.Errorf("same repo = %q", got)
	}
	if got := prRunnerBase(gh, "me/app", 5, fork); got != "refs/pull/5/head" {
		t.Errorf("fork = %q", got)
	}
	if got := prRunnerBase(gl, "me/app", 5, fork); got != "refs/merge-requests/5/head" {
		t.Errorf("gitlab fork = %q", got)
	}
}

func TestChainEntriesWithACLIAreChecked(t *testing.T) {
	pe := newPREnv(t, "github")
	chain, _ := pe.s.taskChain(pe.ctx, "review")
	pid := chain[0].ProviderID
	// Admin → AI's tasks have no owner whose runner could serve them.
	if err := pe.s.SetAITask(pe.ctx, pe.admin, "review", pid, museModel, []ChainModel{{CLI: "claude"}}); err == nil || !strings.Contains(err.Error(), "runner") {
		t.Errorf("an AI task took a CLI entry: %v", err)
	}
	list, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	var rule *Automation
	for _, a := range list {
		if a.Name == "Review pull requests" {
			rule = a
		}
	}
	if rule == nil {
		t.Fatal("no review rule")
	}
	save := func(extra map[string]any) error {
		r := rule.Rule
		r.Actions[0].Models = []ChainModel{{CLI: "claude", Model: "sonnet"}, {ProviderID: pid, Model: museModel}}
		r.Actions[0].Extra = extra
		_, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, rule.ID, AutomationInput{Rule: &r})
		return err
	}
	if err := save(map[string]any{
		"second_models": []any{map[string]any{"cli": "claude"}},
		"verify_models": []any{map[string]any{"cli": "opencode", "model": "opencode/big-pickle"}}}); err != nil {
		t.Errorf("a review rule refused coding agents: %v", err)
	}
	for _, cli := range []string{"emacs", "codex", "cursor"} {
		if err := save(map[string]any{"third_models": []any{map[string]any{"cli": cli}}}); err == nil {
			t.Errorf("%s was accepted", cli)
		}
	}
	got, _ := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	for _, a := range got {
		if a.ID == rule.ID {
			if m := a.Rule.Actions[0].Models; len(m) != 2 || m[0].CLI != "claude" || m[0].Model != "sonnet" {
				t.Errorf("saved models = %+v", m)
			}
			if c := extraChain(a.Rule.Actions[0].Extra["second_models"]); len(c) != 1 || c[0].CLI != "claude" {
				t.Errorf("second reviewer = %+v", c)
			}
		}
	}
	// ask_model has the same editor but no runner scope: refused when saved.
	ask := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "ask_model", Extra: map[string]any{"prompt": "hi"},
		Models: []ChainModel{{CLI: "claude"}}}}}
	if _, err := pe.s.CreateAutomation(pe.ctx, pe.admin, pe.p.Key, AutomationInput{Name: str("Ask"), Rule: &ask}); err == nil {
		t.Error("ask_model took a CLI entry")
	}
}

// A review rule whose chain names Claude Code reviews a member's pull request
// on the runner of the person who saved the rule; a fork's, or a stranger's,
// is reviewed by the next model instead, and no job is queued.
func TestReviewRuleOnARunner(t *testing.T) {
	ae, _ := runnerChainEnv(t)
	list, _ := ae.s.ListAutomations(ae.ctx, ae.admin, ae.p.Key)
	var rule *Automation
	for _, a := range list {
		if a.Name == "Review pull requests" {
			rule = a
		}
	}
	if rule == nil {
		t.Fatal("no review rule")
	}
	r := rule.Rule
	r.Actions[0].Models = []ChainModel{{CLI: "claude", Model: "sonnet"}, {ProviderID: ae.providerID(), Model: museModel}}
	if _, err := ae.s.UpdateAutomation(ae.ctx, ae.admin, rule.ID, AutomationInput{Rule: &r}); err != nil {
		t.Fatal(err)
	}
	ae.llm.answers[museModel] = `{"findings":[]}`
	for n, c := range []struct {
		author, head string
		why          string
	}{{"robin", "sam/app", "fork"}, {"ann", "me/app", "isn't a member"}} {
		pr := ForgePR{Number: 6 + n, Title: "Other", Author: c.author, HeadRef: "other", HeadSHA: fmt.Sprintf("%07dccc", n), State: "open", HeadRepo: c.head}
		ae.pullHook("opened", pr)
		runs := ae.drain()
		if len(runs) != 1 || runs[0].Status != "done" || len(runs[0].Log) < 2 || runs[0].Log[0].Outcome != "unavailable" || !strings.Contains(runs[0].Log[0].Error, c.why) {
			t.Fatalf("%s on %s: runs = %+v", c.author, c.head, runs)
		}
		if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
			t.Fatalf("a job was queued for %s's pull request: %+v", c.author, w.Job)
		}
	}
	pr := ForgePR{Number: 5, Title: "Fix login", Author: "robin", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open", HeadRepo: "me/app"}
	ae.pullHook("opened", pr)
	done := make(chan []*BotRun, 1)
	go func() { done <- ae.drain() }()
	claim := ae.takeSoon()
	in := agentJobOf(t, claim)
	if claim.Kind != JobReview || in.CLI != "claude" || in.Model != "sonnet" || in.Base != "1111111aaaa" || in.Repo != "https://git.example.com/me/app.git" || !in.ReadOnly || !in.Confine {
		t.Fatalf("job = %v %+v", claim.Kind, in)
	}
	ae.finish(claim.ID, "done", map[string]any{"structured": map[string]any{"findings": []any{
		map[string]any{"file": "app/login.go", "line": 11, "severity": "high", "claim": "The nil check is inverted.", "suggestion": ""}}}}, "")
	runs := <-done
	if len(runs) != 1 || runs[0].Status != "done" {
		t.Fatalf("runs = %+v", runs)
	}
	if len(runs[0].Log) == 0 || runs[0].Log[0].Model != "claude sonnet (runner)" || runs[0].Log[0].Outcome != "ok" {
		t.Errorf("log = %+v", runs[0].Log)
	}
	if reviews := ae.fg.reviewsOn(ae.f, "me/app", 5); len(reviews) != 1 || !strings.Contains(reviews[0], "claude sonnet (runner)") {
		t.Errorf("reviews = %q", reviews)
	}
}

// A runner whose wp predates confinement never gets a review job: not from the
// pre-check, and not from the poll that hands jobs out.
func TestOldRunnersNeverGetAReview(t *testing.T) {
	ae, ctx := runnerChainEnv(t)
	bob := ae.user("bob", false)
	old := ae.runner(bob, "oldbox", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} }) // reports no features
	pid := ae.providerID()
	ae.llm.answers[museModel] = `{"findings":[]}`
	// The pre-check says why and the chain moves on.
	bctx := withRunnerScope(ae.ctx, &runnerScope{owner: bob.User.ID, repo: "https://git.example.com/me/app.git", base: "abc", runID: 21, attempt: 1})
	r := ae.result(ae.startChain(bctx, []ChainModel{{CLI: "claude"}, {ProviderID: pid, Model: museModel}}))
	if r.err != nil || r.attempts[0].Outcome != "unavailable" ||
		!strings.Contains(r.attempts[0].Error, "oldbox runs an older wp that can't confine a review: it updates itself, or run wp upgrade") || r.attempts[1].Outcome != "ok" {
		t.Fatalf("result = %+v", r)
	}
	if w := ae.rpoll(bob, old); w.Job != nil {
		t.Fatalf("the old runner was given a job: %+v", w.Job)
	}
	// A review job queued anyway (while a confining runner existed) is still withheld from it.
	newer := ae.runner(bob, "newbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.Features = []string{FeatureConfine}
	})
	raw, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "review", Repo: "https://git.example.com/me/app.git"})
	job := ae.enqueue(&Actor{User: &User{ID: 0, Username: "system"}}, RunnerJobInput{Kind: JobReview, OwnerID: bob.User.ID, Input: raw})
	if w := ae.rpoll(bob, old); w.Job != nil {
		t.Fatalf("the old runner polled up a review: %+v", w.Job)
	}
	w := ae.rpoll(bob, newer)
	if w.Job == nil || w.Job.ID != job.ID {
		t.Fatalf("the confining runner should take it: %+v", w.Job)
	}
	if in := agentJobOf(t, w.Job); !in.Confine {
		t.Errorf("the job isn't marked Confine: %+v", in)
	}
	// Enqueuing with only the old runner left names the reason.
	ae.s.DeleteRunner(ae.ctx, bob, newer.ID)
	_, err := ae.s.EnqueueRunnerJob(ae.ctx, &Actor{User: &User{ID: 0, Username: "system"}}, RunnerJobInput{Kind: JobReview, OwnerID: bob.User.ID, Input: raw})
	if err == nil || !strings.Contains(err.Error(), "older wp") {
		t.Errorf("enqueue with only an old runner: %v", err)
	}
	_ = ctx
}
