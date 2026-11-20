package core

import (
	"errors"
	"strings"
	"testing"
)

func TestOutcomesOfProviderErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&modelError{name: "APIError", msg: "Upstream request failed: Model is unavailable."}, "unavailable"},
		{&modelError{name: "APIError", msg: "Free usage exceeded, subscribe to continue"}, "rate_limit"},
		{&modelError{name: "APIError", msg: "Rate limit exceeded"}, "rate_limit"},
		{&llmError{status: 500, body: "UnknownError"}, "server"},
		{&llmError{status: 502, body: "all 6 candidate models failed"}, "server"},
		{&llmError{status: 429, body: "all 32 candidate models failed"}, "rate_limit"},
		{errors.New("the model's answer wasn't valid JSON"), "error"},
		{&badOutputError{errors.New("missing findings")}, "bad_output"},
	} {
		if got := classifyModelError(c.err); got != c.want {
			t.Errorf("%v: outcome %q, want %q", c.err, got, c.want)
		}
	}
	// A pass of provider-side failures ends the run; a model's bad answer
	// does not.
	down := []reviewerAnswer{{log: []ModelAttempt{{Outcome: "unavailable"}, {Outcome: "rate_limit"}}}}
	if !providersDown(down) {
		t.Error("unavailable and rate limited should count as providers down")
	}
	if providersDown(append(down, reviewerAnswer{log: []ModelAttempt{{Outcome: "error"}}})) {
		t.Error("an invalid answer is the model's, not the provider's")
	}
}

func TestRunErrorSaysWhatHappened(t *testing.T) {
	log := []ModelAttempt{{Outcome: "rate_limit", Error: "secret-host:4096"}, {Outcome: "rate_limit"}, {Outcome: "server"}, {Outcome: "unavailable"}}
	got := runErrorText(&errProvidersDown{}, log)
	if !strings.Contains(got, "rate limited (2), server errors (1), unavailable (1)") || strings.Contains(got, "secret") {
		t.Errorf("providers down: %q", got)
	}
	if got := runErrorText(errAllModelsFailed, []ModelAttempt{{Outcome: "bad_output"}, {Outcome: "error"}}); got != "every model failed: bad answers (1), other errors (1)" {
		t.Errorf("all failed: %q", got)
	}
	if got := runErrorText(errors.New("dial tcp 10.0.0.1: boom"), log); got != "could not connect" {
		t.Errorf("other errors keep the public text: %q", got)
	}
}

func TestStaleRunsAreSkippedWhenQueued(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pr := ForgePR{Number: 12, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "aaaaaaa1111", State: "open"}
	pe.pullHook("opened", pr)
	pr.HeadSHA = "bbbbbbb2222"
	pe.pullHook("synchronize", pr) // before the first run was claimed
	runs := pe.runs()
	if len(runs) != 2 || runs[1].Status != "skipped" || !strings.Contains(runs[1].Summary, "newer push") || runs[0].Status != "queued" {
		t.Fatalf("the older head's run should be skipped: %+v", runs)
	}
	// A pull request that merged while its run waited: skipped when
	// claimed, with no model called.
	pr.State = "merged"
	pe.pullHook("closed", pr)
	before := len(pe.llm.calls)
	if done := pe.drain(); len(done) != 0 || len(pe.llm.calls) != before {
		t.Errorf("a merged pull request's run was carried out: %+v", done)
	}
	if runs := pe.runs(); runs[0].Status != "skipped" || !strings.Contains(runs[0].Summary, "merged") {
		t.Errorf("merged: %+v", runs[0])
	}
}
