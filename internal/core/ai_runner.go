package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// reviewCLIs are the coding-agent CLIs a review may run, because they are the
// ones the runner can confine to the checkout (tested with a planted file:
// claude with --tools Read,Grep,Glob and path rules, opencode with a deny
// permission for outside directories). A pull request's text goes into the
// prompt, so a CLI that can read the whole disk (codex's read-only sandbox)
// or can't be limited to one folder (cursor-agent's rules only deny paths)
// stays out until it can be confined.
var reviewCLIs = []string{"claude", "opencode"}

func reviewCLIError(cli string) string {
	return fmt.Sprintf("%s can't be confined to the checkout yet, so it isn't used to read pull requests; use one of %s or a provider model", cli, strings.Join(reviewCLIs, ", "))
}

// runnerCallMin is the least a runner entry may take: a checkout plus a CLI
// reading it is slower than a model call. The run's own limit still bounds it.
var runnerCallMin = 15 * time.Minute

// runnerPollEvery is how often a waiting call looks at its job.
var runnerPollEvery = 2 * time.Second

// runnerScope says where a bot run's runner entries run: whose runners
// (the automation's owner, 0: nobody's), and what they check out.
type runnerScope struct {
	owner   int64
	repo    string
	base    string
	runID   int64
	attempt int
	denied  string
	seq     atomic.Int64
}

type runnerScopeKey struct{}

func withRunnerScope(ctx context.Context, sc *runnerScope) context.Context {
	return context.WithValue(ctx, runnerScopeKey{}, sc)
}

func runnerScopeOf(ctx context.Context) *runnerScope {
	sc, _ := ctx.Value(runnerScopeKey{}).(*runnerScope)
	return sc
}

// runnerEntryName is how an attempt names a runner entry: "claude sonnet
// (runner)".
func runnerEntryName(cm ChainModel) string {
	return strings.TrimSpace(cm.CLI+" "+cm.Model) + " (runner)"
}

// prRunnerBase is what a runner checks out to see the pull request: its head
// commit, which the checkout's branch fetch brings; for a fork, whose branches
// the clone doesn't have, the forge's ref for the pull request (it is the head
// the run was told about unless a push has just moved it, and the run
// already checked the head hasn't changed).
func prRunnerBase(f *Forge, repo string, number int, fp *ForgePR) string {
	if fp.HeadRepo != "" && !strings.EqualFold(fp.HeadRepo, repo) {
		if f.Kind == "gitlab" {
			return fmt.Sprintf("refs/merge-requests/%d/head", number)
		}
		return fmt.Sprintf("refs/pull/%d/head", number)
	}
	return fp.HeadSHA
}

// botRunnerScope gives ctx the runner scope of a bot run, and stops jobs an
// earlier attempt of the run left behind.
func (s *Service) botRunnerScope(ctx context.Context, r *BotRun, f *Forge, fp *ForgePR) context.Context {
	sc := &runnerScope{runID: r.ID, attempt: r.Attempts, repo: (&stepRepo{Forge: f, Repo: r.PR.Repo}).cloneURL(),
		base: prRunnerBase(f, r.PR.Repo, r.PR.Number, fp)}
	if r.AutomationID != nil {
		if a, err := s.automationByID(ctx, *r.AutomationID); err == nil {
			sc.owner = automationRunnerOwner(a)
		}
	}
	sc.denied = s.runnerGate(ctx, r.ProjectID, r.PR.Repo, fp)
	s.cancelBotRunJobs(ctx, r.ID)
	return withRunnerScope(ctx, sc)
}

// runnerGate says why a pull request may not be handed to a coding agent on
// a runner ("" when it may). A diff is text anyone can write, and the agent
// runs on its owner's machine with its owner's files, so only the work of
// people with a say in the project goes there: the head branch must live in
// the repository itself (pushing one takes write access; a fork, or a head
// the forge didn't name, is refused) and its author, as the forge reports
// it, must be a member of the project who can edit it. A mirror's copies
// of GitHub pull requests are authored by its own account, so they never
// pass. (D14: the same rule fix_ci follows.)
func (s *Service) runnerGate(ctx context.Context, projectID int64, repo string, fp *ForgePR) string {
	if fp.HeadRepo == "" || !strings.EqualFold(fp.HeadRepo, repo) {
		return "this pull request comes from a fork (or its source repository isn't known), and a fork's text isn't handed to a coding agent on a runner; use a provider model for it"
	}
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? AND m.role IN ('owner','editor') AND u.disabled = 0 AND u.username = ? COLLATE NOCASE`, projectID, fp.Author).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read whether a pull request's author is a project member", "project", projectID, "repo", repo, "err", err)
		}
		return "whether its author is a member of this project couldn't be read, so it isn't handed to a coding agent on a runner now"
	}
	if fp.Author == "" || n == 0 {
		return fmt.Sprintf("%s isn't a member of this project, and only a member's pull request is handed to a coding agent on a runner", cmp(fp.Author, "its author"))
	}
	return ""
}

// cancelBotRunJobs stops the runner jobs of a bot run that are still
// waiting or running: a retried run (after a failure or a restart) starts
// its own, and the old ones must not keep a person's machine busy.
func (s *Service) cancelBotRunJobs(ctx context.Context, runID int64) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM runner_jobs WHERE source = 'bot_run' AND source_id = ? AND state IN ('queued','running')`, runID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.cancelStepJob(context.WithoutCancel(ctx), id, "The bot run was started again.")
	}
}

// runnerAttempt asks one runner entry. It returns the attempt for the run's
// log and, when it answered, the entry that did.
func (s *Service) runnerAttempt(ctx context.Context, cm ChainModel, system, prompt string, schema map[string]any, out any, perCall time.Duration) (ChainModel, ModelAttempt, error) {
	name := runnerEntryName(cm)
	start := time.Now()
	fail := func(outcome, msg string, cause error) (ChainModel, ModelAttempt, error) {
		if cause == nil {
			cause = errors.New(msg)
		}
		return ChainModel{}, ModelAttempt{Model: name, Outcome: outcome, Error: excerpt(msg, 300), MS: time.Since(start).Milliseconds()}, cause
	}
	sc := runnerScopeOf(ctx)
	switch {
	case !slices.Contains(reviewCLIs, cm.CLI):
		return fail("unavailable", reviewCLIError(cm.CLI), nil)
	case sc == nil:
		return fail("unavailable", "coding agents on a runner work only in review and reply rules", nil)
	case sc.denied != "":
		return fail("unavailable", sc.denied, nil)
	case sc.owner == 0:
		return fail("unavailable", "this rule has no owner for runners: save it yourself to use your runners", nil)
	}
	owner := "its owner"
	if u, err := s.userByID(ctx, s.db.R, sc.owner); err == nil {
		owner = u.DisplayName
	}
	runners, err := s.ownerRunners(ctx, sc.owner)
	if err != nil {
		s.log.WarnContext(ctx, "list runners for a bot run", "err", err)
		return fail("error", "couldn't look up the runners", err)
	}
	if !slices.ContainsFunc(runners, func(r *Runner) bool {
		return r.Online && runnerCanTake(r, jobNeeds{cli: cm.CLI})
	}) {
		return fail("unavailable", fmt.Sprintf("%s has no runner with %s signed in: connect one under Settings → Runners", owner, cm.CLI), nil)
	}
	var online []*Runner
	for _, r := range runners {
		if r.Online {
			online = append(online, r)
		}
	}
	if !slices.ContainsFunc(online, func(r *Runner) bool { return runnerCanTake(r, jobNeeds{kind: JobReview, cli: cm.CLI}) }) {
		return fail("unavailable", oldRunnerWhy(online, jobNeeds{cli: cm.CLI}), nil)
	}
	call := max(perCall, runnerCallMin)
	in := AgentJob{CLI: cm.CLI, Model: cm.Model, Prompt: strings.TrimSpace(system + "\n\n" + prompt), Repo: sc.repo, Base: sc.base, ReadOnly: true, Permission: AgentRead}
	if schema != nil {
		b, _ := json.Marshal(schema)
		in.JSONSchema = string(b)
	}
	raw, _ := json.Marshal(in)
	key := fmt.Sprintf("botrun#%d#%d#%d", sc.runID, sc.attempt, sc.seq.Add(1))
	queued, err := s.EnqueueRunnerJob(ctx, SystemActor(), RunnerJobInput{Kind: JobReview, OwnerID: sc.owner, Input: raw,
		Source: "bot_run", SourceID: sc.runID, StepKey: key, Timeout: int(call.Seconds())})
	switch {
	case errors.Is(err, ErrNoRunner):
		return fail("unavailable", fmt.Sprintf("%s has no runner with %s signed in: connect one under Settings → Runners", owner, cm.CLI), nil)
	case err != nil:
		return fail("error", "couldn't queue the runner job: "+cmp(PublicError(err), "failed"), err)
	}
	job, timedOut, err := s.waitRunnerJob(ctx, queued.ID, call)
	if err != nil {
		s.cancelStepJob(context.WithoutCancel(ctx), queued.ID, "The bot run couldn't read this job.")
		return fail("error", "couldn't read the runner job", err)
	}
	if timedOut || ctx.Err() != nil {
		why := "The bot run stopped waiting for this job."
		s.cancelStepJob(context.WithoutCancel(ctx), queued.ID, why)
		if ctx.Err() != nil {
			return ChainModel{}, ModelAttempt{Model: name, Outcome: "cancelled", MS: time.Since(start).Milliseconds()}, ctx.Err()
		}
		return fail("timeout", fmt.Sprintf("the runner job didn't finish within %s and was cancelled", call.Round(time.Second)), nil)
	}
	if job.State != "done" {
		msg := cmp(strings.TrimSpace(job.Error), "the runner job "+job.State)
		return fail(classifyRunnerJob(job), msg, nil)
	}
	if err := decodeRunnerAnswer(job.Result, out); err != nil {
		return fail("bad_output", (&badOutputError{err}).Error(), &badOutputError{err})
	}
	a := ModelAttempt{Model: name, Outcome: "ok", MS: time.Since(start).Milliseconds()}
	return ChainModel{CLI: cm.CLI, Model: name}, a, nil
}

// waitRunnerJob waits for a job to finish: until ctx ends or it has taken
// limit (queue time included).
func (s *Service) waitRunnerJob(ctx context.Context, id int64, limit time.Duration) (job *stepJob, timedOut bool, err error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(runnerPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-deadline.C:
			return nil, true, nil
		case <-tick.C:
		}
		j, err := s.stepJobByID(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, nil
			}
			return nil, false, err
		}
		if j == nil {
			return nil, false, errors.New("the runner job is gone")
		}
		if j.finished() {
			return j, false, nil
		}
	}
}

// classifyRunnerJob names why a finished job gave no answer, in the terms of
// a model attempt.
func classifyRunnerJob(j *stepJob) string {
	text := strings.ToLower(j.Error + "\n" + lastBytes(j.Tail, 1500))
	has := func(words ...string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(text, w) })
	}
	switch {
	case j.State == "expired":
		return "unavailable"
	case has("rate limit", "rate-limit", "quota", "usage limit", "too many requests", "429"):
		return "rate_limit"
	case has("not signed in", "isn't signed in"):
		return "auth"
	case has("model not found", "unknown model", "model_not_found"):
		return "unavailable"
	case has("overloaded", "capacity", "529", "temporarily unavailable"):
		return "server"
	case has("timed out"):
		return "timeout"
	}
	return "error"
}

// decodeRunnerAnswer reads a job's answer into out: the CLI's structured
// output, else JSON in its text (a fence or prose around it is dropped).
func decodeRunnerAnswer(result map[string]any, out any) error {
	if st, ok := result["structured"]; ok && st != nil {
		if b, err := json.Marshal(st); err == nil && json.Unmarshal(b, out) == nil {
			return nil
		}
	}
	text := strings.TrimSpace(anyString(result["answer"]))
	if text == "" {
		return errors.New("the agent ended without an answer")
	}
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		if err := json.Unmarshal([]byte(text[i:j+1]), out); err == nil {
			return nil
		}
	}
	return errors.New("its answer isn't the JSON asked for")
}

// gateThreads adds to the runner gate what the review threads say: people's
// comments in them go into a review's or reply's prompt, so a comment by
// someone who isn't a member of the project keeps the pull request off the
// runners too.
func (s *Service) gateThreads(ctx context.Context, projectID int64, threads []ReviewThread) {
	sc := runnerScopeOf(ctx)
	if sc == nil || sc.denied != "" {
		return
	}
	for _, th := range threads {
		for _, c := range th.Comments {
			if !isPersonComment(c) {
				continue
			}
			var n int
			err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_members m JOIN users u ON u.id = m.user_id
				WHERE m.project_id = ? AND m.role IN ('owner','editor') AND u.disabled = 0 AND u.username = ? COLLATE NOCASE`, projectID, c.Author).Scan(&n)
			if err != nil || c.Author == "" || n == 0 {
				sc.denied = fmt.Sprintf("%s isn't a member of this project and commented in a review thread, and only members' text is handed to a coding agent on a runner", cmp(c.Author, "someone"))
				return
			}
		}
	}
}

// oldRunnerWhy names a runner that has the CLI but whose wp predates
// confining a review's CLI to its checkout.
func oldRunnerWhy(runners []*Runner, n jobNeeds) string {
	for _, r := range runners {
		if runnerCanTake(r, n) && !slices.Contains(r.Features, FeatureConfine) {
			return fmt.Sprintf("%s runs an older wp that can't confine a review: it updates itself, or run wp upgrade", r.Name)
		}
	}
	return "no runner can confine a review"
}
