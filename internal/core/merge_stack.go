package core

// The speculative merge queue (WP-404, WP-ADR33). The merge queue (ADR7,
// ADR19) merged one pull request per full run: the next one was brought up
// to date with the base only after the front merged, and its full run
// started then. Now the two drafts behind the front are tested while the
// front's full run goes on, each on a stack: the base's tip with every head
// ahead of it merged in, then its own, built by the workflow's push run on a
// branch Waypoint makes for it (wp-queue/...), whose one commit adds a file
// listing those heads. When the front merges, the next one's passed stack
// stands in for the update and the full run: it is marked ready with no full
// run to come and merges as soon as its own per-pull-request conditions (the
// review gate, comments, audits, the quiet period) are met.
//
// A stack holds only while what it was built on does: the heads ahead of it
// merged at the heads it merged, or still ahead in the same order at the
// same heads, and its own head unchanged. Anything else ends it, cancels its
// run and the stacks built on it, and the pull request goes back to the
// queue as before (waiting as it is for its turn at the front, WP-510).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Stack states. running, passed and ready are live; the rest have ended.
const (
	stackRunning   = "running"   // its run is going
	stackPassed    = "passed"    // its run passed
	stackReady     = "ready"     // its pull request was marked ready on it
	stackFailed    = "failed"    // its run failed, or never started
	stackMerged    = "merged"    // its pull request merged on it
	stackCancelled = "cancelled" // what it was built on changed
	stackRefused   = "refused"   // the forge refused the merge it allowed
	stackConflict  = "conflict"  // its own head doesn't merge onto the heads before it (WP-535)
)

const (
	stackPrefix  = "wp-queue/"
	stackFile    = ".waypoint-queue"
	stackContext = "waypoint/stack"
	// stackStartWait: a stack whose branch has no status this long after it
	// was made counts as failed, and the base gets no stacks for stackOffFor
	// (the workflow doesn't run on its pushes after all).
	stackStartWait = 10 * time.Minute
	// stackRunMax: a stack still running (a job stuck pending, logs that
	// can't be read) this long after it was made fails, so a front waiting
	// on it takes the usual update and full run instead.
	stackRunMax = 2 * time.Hour
	// stackDepth: the places behind the front that are tested on a stack.
	stackDepth  = 2
	stackOptTTL = 10 * time.Minute
	stackOffFor = 24 * time.Hour
	// stackPauseRuns ended stack or batch runs on a line with no pass pause
	// its stacks and batches for stackPauseFor after the newest (WP-386).
	stackPauseRuns = 10
	stackPauseFor  = 2 * time.Hour
	// stackTreeMark starts the log line where the run says what tree it built.
	stackTreeMark = "waypoint-stack-tree "
)

// stackHead is one pull request head a stack merges.
type stackHead struct {
	PRID   int64  `json:"pr_id"`
	Number int    `json:"number"`
	SHA    string `json:"sha"`
}

// queueStack is a row of queue_stacks: the heads (front first) merged onto
// BaseSHA by the run on Branch's CommitSHA. Heads[Pos] is its own pull
// request's: the last on a stack of its own, any of them in a batch
// (WP-509), where each pull request that rides it has a row of its own on
// the one branch and commit.
type queueStack struct {
	ID        int64
	ForgeID   int64
	Repo      string
	BaseRef   string
	PRID      int64
	BaseSHA   string
	Heads     []stackHead
	Pos       int
	Branch    string
	CommitSHA string
	State     string
	Tree      string
	RunURL    string
	Note      string
	CreatedAt time.Time
	Ended     bool
}

func (st *queueStack) live() bool {
	return !st.Ended && (st.State == stackRunning || st.State == stackPassed || st.State == stackReady)
}

// own is the stack's own pull request's head.
func (st *queueStack) own() stackHead { return st.Heads[st.Pos] }

// inBatch: the run merges heads after its own too (WP-509), so pull requests
// behind it ride the same run.
func (st *queueStack) inBatch() bool { return st.Pos < len(st.Heads)-1 }

// name says what the stack is: "main + #7 + #8".
func (st *queueStack) name() string {
	parts := []string{cmp(st.BaseRef, "the base")}
	for _, h := range st.Heads {
		parts = append(parts, fmt.Sprintf("#%d", h.Number))
	}
	return strings.Join(parts, " + ")
}

const stackCols = `id, forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, tree, run_url, note, created_at, ended_at IS NOT NULL`

func scanStack(row interface{ Scan(...any) error }) (*queueStack, error) {
	var st queueStack
	var heads string
	var created int64
	if err := row.Scan(&st.ID, &st.ForgeID, &st.Repo, &st.BaseRef, &st.PRID, &st.BaseSHA, &heads, &st.Pos, &st.Branch, &st.CommitSHA,
		&st.State, &st.Tree, &st.RunURL, &st.Note, &created, &st.Ended); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(heads), &st.Heads); err != nil || len(st.Heads) == 0 {
		return nil, fmt.Errorf("stack %d lists no heads", st.ID)
	}
	if st.Pos < 0 || st.Pos >= len(st.Heads) {
		st.Pos = len(st.Heads) - 1 // a stack of its own
	}
	st.CreatedAt = time.UnixMilli(created)
	return &st, nil
}

// liveStackOf is the pull request's live stack, nil when it has none.
func (s *Service) liveStackOf(ctx context.Context, prID int64) (*queueStack, error) {
	st, err := scanStack(s.db.R.QueryRowContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE pr_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, prID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

// stackHolds: pr has a live stack at its head, so its branch is left as it
// is (an update would change the head the stack tests).
func (s *Service) stackHolds(ctx context.Context, pr *PullRequest) bool {
	st, err := s.liveStackOf(ctx, pr.ID)
	return err == nil && st != nil && st.live() && sameSHA(st.own().SHA, pr.HeadSHA)
}

// stackPRByCommit is the pull request a live stack on this commit is for (0: none).
func (s *Service) stackPRByCommit(ctx context.Context, forgeID int64, repo, sha string) int64 {
	if sha == "" {
		return 0
	}
	var id int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT pr_id FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND commit_sha = ? AND ended_at IS NULL LIMIT 1`,
		forgeID, repo, strings.ToLower(sha)).Scan(&id); err != nil {
		return 0
	}
	return id
}

// failedOn: pr's checks failed on sha.
func failedOn(pr *PullRequest, sha string) bool {
	return pr.CIState == CIFailed && sameSHA(pr.ciSHA, sha)
}

// stackTried: a stack of exactly these heads failed or found a conflict
// before; it isn't run again.
func (s *Service) stackTried(ctx context.Context, prID int64, heads []stackHead) bool {
	rows, err := s.db.R.QueryContext(ctx, `SELECT heads FROM queue_stacks WHERE pr_id = ? AND state IN (?, ?)`, prID, stackFailed, stackConflict)
	if err != nil {
		return true // can't tell: don't spend a run on it
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var was []stackHead
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &was) != nil {
			continue
		}
		if sameHeads(was, heads) {
			return true
		}
	}
	return false
}

// ---- the forge ----

// ForgeStacks is the optional part of a ForgeClient that stacks need, with
// ForgeCommitter (to make the branch) and DeleteBranch.
type ForgeStacks interface {
	// CommitParents lists a commit's parents, first parent first.
	CommitParents(ctx context.Context, f *Forge, repo, sha string) ([]string, error)
	// CancelRunsAt cancels the Actions runs of a commit that haven't
	// finished, and says how many it cancelled.
	CancelRunsAt(ctx context.Context, f *Forge, repo, sha string) (int, error)
	// StackRules: the repo's default merge style, and whether the base's
	// branch protection refuses to merge a pull request behind it.
	StackRules(ctx context.Context, f *Forge, repo, base string) (style string, blockOutdated bool, err error)
}

// branchDeleter deletes a branch; one already gone is no error.
type branchDeleter interface {
	DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error
}

func (c *httpForgeClient) CommitParents(ctx context.Context, f *Forge, repo, sha string) ([]string, error) {
	if f.Kind == "gitlab" {
		return nil, fmt.Errorf("%w: GitLab", ErrUnsupported)
	}
	var r struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repo, url.PathEscape(sha)), nil, &r); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.Parents))
	for _, p := range r.Parents {
		out = append(out, p.SHA)
	}
	return out, nil
}

// runDone: an Actions run in this state has finished.
func runDone(status string) bool {
	switch status {
	case "running", "waiting", "blocked", "queued", "in_progress", "pending":
		return false
	}
	return true
}

func (c *httpForgeClient) CancelRunsAt(ctx context.Context, f *Forge, repo, sha string) (int, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return 0, fmt.Errorf("%w: %s", ErrUnsupported, f.Kind)
	}
	runs, err := c.ActionRunsAt(ctx, f, repo, sha)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range runs {
		if runDone(r.Status) {
			continue
		}
		if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repo, r.ID), nil, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (c *httpForgeClient) StackRules(ctx context.Context, f *Forge, repo, base string) (string, bool, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return "", false, fmt.Errorf("%w: %s", ErrUnsupported, f.Kind)
	}
	var r struct {
		Style string `json:"default_merge_style"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return "", false, err
	}
	var rules []struct {
		Rule     string `json:"rule_name"`
		Branch   string `json:"branch_name"`
		Outdated bool   `json:"block_on_outdated_branch"`
	}
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/branch_protections", repo), nil, &rules)
	if isStatus(err, http.StatusForbidden) || isStatus(err, http.StatusNotFound) {
		// Read only by a repo admin. A refusal of a stacked merge turns
		// stacks off for a day, so not knowing costs a run at most.
		return r.Style, false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, p := range rules {
		pat := cmp(p.Rule, p.Branch)
		if ok, _ := path.Match(pat, base); (ok || pat == base) && p.Outdated {
			return r.Style, true, nil
		}
	}
	return r.Style, false, nil
}

// stackParents reads a commit's parents once: commits don't change.
func (s *Service) stackParents(ctx context.Context, f *Forge, repo, sha string) ([]string, error) {
	key := strings.ToLower(repo) + "\x00" + strings.ToLower(sha)
	if v, ok := s.stackCommits.Load(key); ok {
		return v.([]string), nil
	}
	cl, ok := s.Forges.(ForgeStacks)
	if !ok {
		return nil, ErrUnsupported
	}
	ps, err := cl.CommitParents(ctx, f, repo, sha)
	if err != nil {
		return nil, err
	}
	s.stackCommits.Store(key, ps)
	return ps, nil
}

// ---- whether a base gets stacks ----

// stackOpt is what was found about a base's stacks, kept for stackOptTTL.
type stackOpt struct {
	on    bool
	style string // the repo's default merge style
	at    time.Time
}

// stacksOn: pr's base gets stacks, and the repo's default merge style. Only
// a Forgejo repo that is draft-first, whose workflow on the base runs on
// pushes to wp-queue/ branches, whose base doesn't refuse an outdated pull
// request, and not for a day after the forge refused a stacked merge or a
// stack's run never started.
func (s *Service) stacksOn(ctx context.Context, f *Forge, pr *PullRequest) (string, bool) {
	key := baseKey(pr)
	if v, ok := s.stackOff.Load(key); ok {
		if s.now().Before(v.(time.Time)) {
			return "", false
		}
		s.stackOff.CompareAndDelete(key, v)
	}
	if v, ok := s.stackOpt.Load(key); ok {
		if o := v.(stackOpt); s.now().Sub(o.at) < stackOptTTL {
			return o.style, o.on
		}
	}
	o := stackOpt{at: s.now()}
	o.style, o.on = s.readStackOpt(ctx, f, pr)
	s.stackOpt.Store(key, o)
	return o.style, o.on
}

func (s *Service) readStackOpt(ctx context.Context, f *Forge, pr *PullRequest) (string, bool) {
	cl, ok := s.Forges.(ForgeStacks)
	_, ok2 := s.Forges.(ForgeCommitter)
	_, ok3 := s.Forges.(branchDeleter)
	if !ok || !ok2 || !ok3 || !draftFirstForge(f.Kind) || f.token == "" || pr.BaseRef == "" {
		return "", false
	}
	if onEdit, err := s.ciRunsOnEdit(ctx, f, pr.Repo, pr.BaseRef); err != nil || !onEdit {
		return "", false
	}
	if pushes, err := s.anyWorkflow(ctx, f, pr.Repo, pr.BaseRef, workflowPushesStacks); err != nil || !pushes {
		return "", false
	}
	style, block, err := cl.StackRules(ctx, f, pr.Repo, pr.BaseRef)
	if err != nil {
		s.log.WarnContext(ctx, "read the merge rules for stacks", "repo", pr.Repo, "err", err)
		return "", false
	}
	return style, !block
}

// stacksOff turns stacks off for pr's base for stackOffFor.
func (s *Service) stacksOff(forgeID int64, repo, base string) {
	s.stackOff.Store(baseKey(&PullRequest{ForgeID: forgeID, Repo: repo, BaseRef: base}), s.now().Add(stackOffFor))
}

// stacksPaused: none of the last stackPauseRuns stack or batch runs to end
// on pr's line passed, and the newest of them ended under stackPauseFor ago,
// so the line gets none until then; after it, one more is tried, and a pass
// lifts the pause (WP-386, WP-ADR51; WP-516 proposed it). until is when it ends.
// A run is one branch: a batch's rows share theirs. Conflicts (no run) and
// refusals (a pass the forge wouldn't merge) don't count.
func (s *Service) stacksPaused(ctx context.Context, pr *PullRequest) (until time.Time, paused bool) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT MAX(state IN (?, ?, ?)), MAX(ended_at) FROM queue_stacks
		WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND ended_at IS NOT NULL
		AND state NOT IN (?, ?) GROUP BY branch ORDER BY MAX(ended_at) DESC LIMIT ?`,
		stackPassed, stackReady, stackMerged, pr.ForgeID, pr.Repo, pr.BaseRef, stackConflict, stackRefused, stackPauseRuns)
	if err != nil {
		return time.Time{}, false
	}
	defer rows.Close()
	var n int
	var newest int64
	for rows.Next() {
		var passed bool
		var ended int64
		if err := rows.Scan(&passed, &ended); err != nil || passed {
			return time.Time{}, false
		}
		if n == 0 {
			newest = ended
		}
		n++
	}
	if rows.Err() != nil || n < stackPauseRuns {
		return time.Time{}, false
	}
	until = time.UnixMilli(newest).Add(stackPauseFor)
	return until, s.now().Before(until)
}

// workflowPushesStacks: the workflow runs on pushes to wp-queue/ branches.
func workflowPushesStacks(body string) bool {
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if yaml.Unmarshal([]byte(body), &wf) != nil || wf.On.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(wf.On.Content); i += 2 {
		if wf.On.Content[i].Value != "push" {
			continue
		}
		ev := wf.On.Content[i+1]
		if ev.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(ev.Content); j += 2 {
			if ev.Content[j].Value != "branches" {
				continue
			}
			list := ev.Content[j+1]
			if strings.HasPrefix(list.Value, stackPrefix) {
				return true
			}
			for _, b := range list.Content {
				if strings.HasPrefix(b.Value, stackPrefix) {
					return true
				}
			}
		}
	}
	return false
}

// stackable: pr's head can go in a stack. Merged as a merge commit (so the
// chain can be checked), from the repo itself (a stack's push run has the
// repo's secrets), by a Waypoint person, and not touching a workflow
// directory's tree (.forgejo/, .gitea/, .github/: the stack's run uses the
// base's workflow and actions, not the head's).
func (s *Service) stackable(ctx context.Context, pr *PullRequest, style string) bool {
	if pr.Outside || cmp(pr.ArmedStyle, style) != "merge" || (pr.headRepo != "" && !strings.EqualFold(pr.headRepo, pr.Repo)) {
		return false
	}
	s.loadPRFiles(ctx, pr)
	return pr.filesKnown && !pr.tooLarge && !slices.ContainsFunc(pr.files, touchesCI)
}

// unsettled says why pr's head may still move before its turn comes, ""
// when nothing is left to come: a review still going or queued, a reviewer
// the merge policy waits for, the quiet period, or a hold from its reviews
// (the review gate, comments not dealt with, a request for changes, an
// audit's findings, a round at work). A stack or batch tests heads as they
// are, so a head that moves ends it and its run is thrown away (WP-386, WP-ADR51: in
// the six hours to 2026-11-06 15:20 UTC, 2 of 31 stacked places passed on
// waypoint and 1 of 41 on another repo, and the stacks and batches used 27% to 30%
// of their runner time; a CodeRabbit review lands about 48 minutes after a
// push, long after the draft tier passes).
func (s *Service) unsettled(ctx context.Context, pr *PullRequest) string {
	going, why, err := s.reviewGoingErr(ctx, pr)
	if err != nil {
		return "its reviews couldn't be read" // not settled (WP-748, audit 674.2)
	}
	if going {
		return why
	}
	var rd PRReadiness
	holds, err := s.reviewHolds(ctx, pr, true, &rd)
	if err != nil {
		return "its reviews couldn't be read"
	}
	if len(holds) > 0 {
		return holds[0]
	}
	pol, attached, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return "its merge policy couldn't be read"
	}
	if !attached {
		return ""
	}
	checks, err := s.prChecks(ctx, pr)
	if err != nil {
		return "its checks couldn't be read"
	}
	waits, _, err := s.settleWaits(ctx, pr, pol, checks)
	if err != nil {
		return "its review wait couldn't be read"
	}
	if len(waits) > 0 {
		return waits[0]
	}
	return ""
}

// touchesCI: p is under a directory Forgejo reads workflows from
// (workflowDirs), where the actions and scripts they use live too.
func touchesCI(p string) bool {
	return slices.ContainsFunc(workflowDirs, func(dir string) bool {
		top, _, _ := strings.Cut(dir, "/")
		return strings.HasPrefix(p, top+"/")
	})
}

// ---- a stack's life ----

// stackStep is the stack part of a step of a draft waiting behind the front
// of its queue (behindFront): it ends a stack that no longer holds, makes
// one, and follows its run. It says what the stack is doing, "" when there is
// no live stack, so the draft waits as it is.
func (s *Service) stackStep(ctx context.Context, pr *PullRequest, ahead []*PullRequest) string {
	st, err := s.liveStackOf(ctx, pr.ID)
	if err != nil {
		return ""
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return ""
	}
	if st != nil {
		if why := s.stackBroken(ctx, st, pr, ahead); why != "" {
			s.endStack(ctx, st, stackCancelled, why)
			st = nil
		}
	}
	if st == nil {
		style, on := s.stacksOn(ctx, f, pr)
		if !on || s.knownConflict(ctx, pr, ahead) {
			return ""
		}
		if _, paused := s.stacksPaused(ctx, pr); paused {
			return ""
		}
		// Drafts ahead known not to merge onto the heads before them are
		// passed over and yield their place (WP-535).
		on2, passed := s.passConflicts(ctx, ahead)
		switch {
		case s.poolSaturated(ctx, pr.ForgeID):
			// The runners are all busy: one batch, not a run per place (WP-509).
			st = s.makeBatch(ctx, f, pr, style)
		case len(on2) <= stackDepth:
			if st = s.makeStack(ctx, f, pr, on2, style); st != nil {
				s.yieldTo(ctx, pr, passed)
			}
		}
		if st == nil {
			return ""
		}
	}
	s.pollStack(ctx, f, st)
	what := "tested on a stack (" + st.name() + ")"
	if s.wasBatch(st) {
		what = "rides a merge queue batch (" + st.name() + ")"
	}
	switch st.State {
	case stackRunning:
		return what + ": its run is going"
	case stackPassed, stackReady:
		return what + ": passed; it merges without an update or a full run when its turn comes"
	}
	return ""
}

// makeStack starts a stack for pr, with ahead (the front first) in its
// queue, nil when it can't have one. pr's draft tier passed on its head and
// it merges cleanly with its base; every pull request ahead of it is the
// front, marked ready on a head that holds the base's tip, or one with a
// live stack, which pr's extends.
func (s *Service) makeStack(ctx context.Context, f *Forge, pr *PullRequest, ahead []*PullRequest, style string) *queueStack {
	if len(ahead) == 0 || len(ahead) > stackDepth || pr.CIState != CIPassed || pr.ciSHA != pr.HeadSHA {
		return nil
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		return nil // a conflict; one the forge hasn't worked out yet fails the stack's run
	}
	if !s.stackable(ctx, pr, style) || s.unsettled(ctx, pr) != "" {
		return nil
	}
	last := ahead[len(ahead)-1]
	var base string
	var heads []stackHead
	if was, err := s.liveStackOf(ctx, last.ID); err == nil && was != nil && was.live() && s.stackBroken(ctx, was, last, ahead[:len(ahead)-1]) == "" {
		base, heads = was.BaseSHA, slices.Clone(was.Heads[:was.Pos+1])
	} else if len(ahead) == 1 && queueHolder(last) && !failedOn(last, last.HeadSHA) && s.stackable(ctx, last, style) && s.unsettled(ctx, last) == "" {
		tip, behind, stale := s.behindBase(ctx, last)
		if behind || stale || tip == "" {
			return nil
		}
		base, heads = tip, []stackHead{{PRID: last.ID, Number: last.Number, SHA: last.HeadSHA}}
	} else {
		return nil
	}
	heads = append(heads, stackHead{PRID: pr.ID, Number: pr.Number, SHA: pr.HeadSHA})
	if s.stackTried(ctx, pr.ID, heads) {
		return nil
	}
	// The branch is cut from the base's tip, and making it is a push that
	// starts a run: the tip is read first, so a stack whose parent can't be
	// its base is never made (and cancelled and made again, step after step).
	if tip, _, stale := s.behindBase(ctx, pr); stale {
		return nil
	} else if tip != "" && !sameSHA(tip, base) {
		if base, heads = s.restack(ctx, f, pr.Repo, base, heads, tip); base == "" || s.stackTried(ctx, pr.ID, heads) {
			return nil
		}
	}
	var body strings.Builder
	body.WriteString("# Waypoint's merge queue (WP-404): the run on this branch merges these pull\n# request heads, in order, into this commit's parent and tests the result.\n")
	for _, h := range heads {
		fmt.Fprintf(&body, "%d %s\n", h.Number, h.SHA)
	}
	st := &queueStack{ForgeID: pr.ForgeID, Repo: pr.Repo, BaseRef: pr.BaseRef, PRID: pr.ID, BaseSHA: base, Heads: heads, State: stackRunning,
		Branch: fmt.Sprintf("%s%s/%d-%s", stackPrefix, pr.BaseRef, pr.Number, strconv.FormatInt(s.nowMS(), 36)), CreatedAt: s.now()}
	c, err := s.Forges.(ForgeCommitter).CommitFiles(ctx, f, pr.Repo, CommitInput{Base: pr.BaseRef, Branch: st.Branch,
		Message: "Merge queue stack: " + st.name(), Files: []FileChange{{Path: stackFile, Content: body.String()}}})
	if err != nil {
		s.log.WarnContext(ctx, "make a merge queue stack", "repo", pr.Repo, "number", pr.Number, "err", err)
		return nil
	}
	st.CommitSHA = strings.ToLower(c.SHA)
	// The base may have moved since its tip was read: the stack must sit on
	// the tip its heads were checked against.
	if ps, err := s.stackParents(ctx, f, pr.Repo, st.CommitSHA); err != nil || len(ps) != 1 || !sameSHA(ps[0], base) {
		s.deleteStackBranch(ctx, f, st)
		return nil
	}
	hj, _ := json.Marshal(heads)
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, branch, commit_sha, state, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		st.ForgeID, st.Repo, st.BaseRef, st.PRID, st.BaseSHA, string(hj), st.Branch, st.CommitSHA, st.State, st.CreatedAt.UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "record a merge queue stack", "pr", pr.ID, "err", err)
		s.deleteStackBranch(ctx, f, st)
		return nil
	}
	st.ID, _ = res.LastInsertId()
	s.armNote(ctx, pr.ID, "tested on a stack ("+st.name()+") while the pull requests ahead of it finish")
	return st
}

// restack moves a stack's heads onto the base's tip when the tip is base
// with the stack's first heads merged onto it, in order, at those heads:
// the pull requests at the bottom of the stack it extends merged since that
// stack was made. It returns the tip and the heads left, or "" when the tip
// is anything else.
func (s *Service) restack(ctx context.Context, f *Forge, repo, base string, heads []stackHead, tip string) (string, []stackHead) {
	k := 0
	for ; k < len(heads)-1; k++ {
		p, err := s.prByID(ctx, s.db.R, heads[k].PRID)
		if err != nil || p.State != "merged" || !sameSHA(p.HeadSHA, heads[k].SHA) {
			break
		}
	}
	if k == 0 {
		return "", nil
	}
	if ok, err := s.stackChainN(ctx, f, &queueStack{Repo: repo, BaseSHA: base, Heads: heads}, tip, k); err != nil || !ok {
		return "", nil
	}
	return tip, slices.Clone(heads[k:])
}

// stackBroken says why st no longer holds for pr, with ahead (front first)
// in its queue; "" when it holds. It holds while its own head is pr's, and
// the heads before it are, in order, pull requests merged at those heads
// and then exactly the ones ahead of pr at their heads now.
func (s *Service) stackBroken(ctx context.Context, st *queueStack, pr *PullRequest, ahead []*PullRequest) string {
	if !st.live() {
		return "it has ended"
	}
	own := st.own()
	if own.PRID != pr.ID || !sameSHA(own.SHA, pr.HeadSHA) {
		return fmt.Sprintf("#%d's head moved", pr.Number)
	}
	if !strings.EqualFold(st.Repo, pr.Repo) || st.BaseRef != pr.BaseRef {
		return fmt.Sprintf("#%d's base changed", pr.Number)
	}
	earlier := st.Heads[:st.Pos]
	// The heads a batch merges after this one's (WP-509): the run tested
	// them too, so they must still be the pull requests' heads.
	for _, h := range st.Heads[st.Pos+1:] {
		p, err := s.prByID(ctx, s.db.R, h.PRID)
		switch {
		case err != nil:
			return fmt.Sprintf("#%d is gone", h.Number)
		case p.State != "open" && p.State != "draft":
			return fmt.Sprintf("#%d is %s", h.Number, p.State)
		case !sameSHA(p.HeadSHA, h.SHA):
			return fmt.Sprintf("#%d's head moved", h.Number)
		case failedOn(p, h.SHA):
			return fmt.Sprintf("#%d's checks failed", h.Number)
		}
	}
	i := 0
	for ; i < len(earlier); i++ {
		p, err := s.prByID(ctx, s.db.R, earlier[i].PRID)
		if err != nil {
			return fmt.Sprintf("#%d is gone", earlier[i].Number)
		}
		if p.State != "merged" {
			break
		}
		if !sameSHA(p.HeadSHA, earlier[i].SHA) {
			return fmt.Sprintf("#%d merged at another head", p.Number)
		}
	}
	rest := earlier[i:]
	for j, h := range rest {
		if j >= len(ahead) || ahead[j].ID != h.PRID {
			return fmt.Sprintf("#%d is no longer ahead of it in the queue", h.Number)
		}
		if !sameSHA(ahead[j].HeadSHA, h.SHA) {
			return fmt.Sprintf("#%d's head moved", h.Number)
		}
		if failedOn(ahead[j], h.SHA) {
			// The front keeps its place through the round (WP-455), but the
			// head stacked on won't merge.
			return fmt.Sprintf("#%d's checks failed", h.Number)
		}
	}
	if len(ahead) != len(rest) {
		return "the queue ahead of it changed"
	}
	return ""
}

// endStack ends st: its unfinished run is cancelled, its branch deleted,
// and the stacks built on it end too (unless it merged).
func (s *Service) endStack(ctx context.Context, st *queueStack, state, note string) {
	res, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ?, note = ?, ended_at = ? WHERE id = ? AND ended_at IS NULL`,
		state, excerpt(note, 300), s.nowMS(), st.ID)
	if err != nil {
		s.log.WarnContext(ctx, "end a merge queue stack", "stack", st.ID, "err", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Already ended (a batch's sibling, or the stack it builds on, ended it).
		if !st.Ended {
			_ = s.db.R.QueryRowContext(ctx, `SELECT state, note FROM queue_stacks WHERE id = ?`, st.ID).Scan(&st.State, &st.Note)
			st.Ended = true
		}
		return
	}
	was := st.State
	st.State, st.Ended, st.Note = state, true, note
	s.batchEnded(ctx, st)
	f, ferr := s.forgeByID(ctx, st.ForgeID)
	if ferr != nil {
		return
	}
	if state != stackMerged {
		// A batch's pull requests ride one run: it holds for all of them
		// or none (WP-509). A failed run failed them all.
		// One rider's conflict says nothing of the others: they ride the
		// next batch, without it (WP-535).
		sib := stackCancelled
		if state == stackFailed {
			sib = state
		}
		for _, o := range s.batchSiblings(ctx, st) {
			s.endStack(ctx, o, sib, fmt.Sprintf("the batch it rides (#%d's place in it) ended: %s", st.own().Number, note))
			s.nudgeBabysit(o.PRID, false)
		}
	}
	// A batch's run and branch are its riders' together: one merged by
	// hand while the run goes leaves them to the others.
	if len(s.batchSiblings(ctx, st)) == 0 {
		if was == stackRunning {
			if cl, ok := s.Forges.(ForgeStacks); ok && st.CommitSHA != "" {
				if _, err := cl.CancelRunsAt(ctx, f, st.Repo, st.CommitSHA); err != nil {
					s.log.WarnContext(ctx, "cancel a merge queue stack's run", "stack", st.ID, "err", err)
				}
			}
		}
		s.deleteStackBranch(ctx, f, st) // the last of a batch to end deletes it
	}
	if state == stackMerged {
		return
	}
	// The stacks that merge this one's head before their own were built on it.
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND ended_at IS NULL AND id != ?`,
		st.ForgeID, st.Repo, st.BaseRef, st.ID)
	if err != nil {
		return
	}
	var on []*queueStack
	for rows.Next() {
		if o, err := scanStack(rows); err == nil && o.CommitSHA != st.CommitSHA && slices.ContainsFunc(o.Heads[:o.Pos], func(h stackHead) bool { return h.PRID == st.PRID }) {
			on = append(on, o)
		}
	}
	rows.Close()
	for _, o := range on {
		s.endStack(ctx, o, stackCancelled, fmt.Sprintf("the stack it builds on (#%d's) ended: %s", st.own().Number, note))
		s.nudgeBabysit(o.PRID, false)
	}
}

func (s *Service) deleteStackBranch(ctx context.Context, f *Forge, st *queueStack) {
	if !strings.HasPrefix(st.Branch, stackPrefix) {
		return // never anything but a stack's own branch
	}
	if cl, ok := s.Forges.(branchDeleter); ok {
		if err := cl.DeleteBranch(ctx, f, st.Repo, st.Branch); err != nil {
			s.log.WarnContext(ctx, "delete a merge queue stack's branch", "repo", st.Repo, "branch", st.Branch, "err", err)
		}
	}
}

// pollStack follows a running stack's run: failed, or passed with the tree
// it built.
func (s *Service) pollStack(ctx context.Context, f *Forge, st *queueStack) {
	if st.State != stackRunning {
		return
	}
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return
	}
	all, err := cl.CommitChecks(ctx, f, st.Repo, st.CommitSHA)
	if err != nil {
		return
	}
	var checks []CommitCheck
	for _, c := range dropSuperseded(all) {
		if !ownCheck(c.Name) {
			checks = append(checks, c)
		}
	}
	if len(checks) == 0 {
		if s.now().Sub(st.CreatedAt) >= stackStartWait {
			s.stacksOff(st.ForgeID, st.Repo, st.BaseRef)
			s.endStack(ctx, st, stackFailed, "no CI run started on its branch in "+fmtMinutes(stackStartWait)+"; no stacks for a day")
		}
		return
	}
	ran := false
	for _, c := range checks {
		switch c.State {
		case CheckFailed:
			s.stackRunFailed(ctx, f, st, checks, c.Name+" failed")
			return
		case CheckPending:
			s.stackOverdue(ctx, st, c.Name+" is still running")
			return
		}
		ran = ran || !skippedCheck(c)
	}
	// A job not yet started posts nothing: wait for every run to end.
	if al, ok := s.Forges.(ForgeActionLogs); ok {
		if runs, err := al.ActionRunsAt(ctx, f, st.Repo, st.CommitSHA); err == nil {
			for _, r := range runs {
				if !runDone(r.Status) {
					s.stackOverdue(ctx, st, "its run hasn't ended")
					return
				}
			}
		}
	}
	if !ran {
		s.endStack(ctx, st, stackFailed, "its run skipped every check")
		return
	}
	pol, attached, err := s.repoMergePolicy(ctx, st.ForgeID, st.Repo)
	if err != nil {
		return
	}
	if attached {
		for _, name := range pol.RequiredChecks {
			if ownCheck(name) {
				continue
			}
			want := pushCheckName(name)
			if !slices.ContainsFunc(checks, func(c CommitCheck) bool { return strings.EqualFold(c.Name, want) && c.State == CheckSuccess }) {
				s.endStack(ctx, st, stackFailed, "required check "+want+" didn't pass on it")
				return
			}
		}
	}
	tree, runURL, why := s.stackTree(ctx, f, st, checks)
	if tree == "" && why == "" {
		// No job's log could be read (yet): look again next step.
		s.stackOverdue(ctx, st, "no job log of its run could be read")
		return
	}
	if why != "" {
		// Green checks on a commit nobody merged the heads into test nothing.
		s.stacksOff(st.ForgeID, st.Repo, st.BaseRef)
		// A forge client that can't read logs says nothing about the heads:
		// that is a failure, not a conflict.
		end := stackConflict
		if _, ok := s.Forges.(ForgeJobLogs); !ok {
			end = stackFailed
		}
		s.endStack(ctx, st, end, why+"; no stacks for a day")
		return
	}
	// A batch's rows share the run (WP-509): they all passed.
	if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ?, tree = ?, run_url = ? WHERE state = ? AND ended_at IS NULL
		AND (id = ? OR (forge_id = ? AND repo = ? COLLATE NOCASE AND commit_sha = ? AND commit_sha != ''))`,
		stackPassed, tree, runURL, stackRunning, st.ID, st.ForgeID, st.Repo, st.CommitSHA); err != nil {
		s.log.WarnContext(ctx, "record a passed merge queue stack", "stack", st.ID, "err", err)
		return
	}
	st.State, st.Tree, st.RunURL = stackPassed, tree, runURL
}

// pushCheckName is a pull request run's check as a push run names it:
// "ci / backend (pull_request)" is "ci / backend (push)".
func pushCheckName(name string) string {
	if job, ok := jobOf(name); ok && strings.HasSuffix(name, " (pull_request)") {
		return job + " (push)"
	}
	return name
}

// stackLine is a line the stack's run prints for each head it merged.
var stackLine = lazyre.New(`waypoint-stack #(\d+) ([0-9a-f]{7,64})\b`)

// stackTree reads, from the log of a job of st's run, the tree it built,
// having checked that it merged st's heads. why says what is missing; both
// empty: no log could be read, so it isn't known yet.
func (s *Service) stackTree(ctx context.Context, f *Forge, st *queueStack, checks []CommitCheck) (tree, runURL, why string) {
	jl, ok := s.Forges.(ForgeJobLogs)
	if !ok {
		return "", "", "the forge client can't read job logs, so what the run built isn't known"
	}
	var try []CommitCheck
	for _, c := range checks {
		if !skippedCheck(c) && c.URL != "" {
			try = append(try, c)
		}
	}
	// lint is one job that builds the stack; any other that ran does too.
	slices.SortStableFunc(try, func(a, b CommitCheck) int {
		return boolRank(!strings.Contains(a.Name, "lint")) - boolRank(!strings.Contains(b.Name, "lint"))
	})
	why = "no job of its run said it merged the heads (no " + strings.TrimSpace(stackTreeMark) + " line)"
	read := false
	for i, c := range try {
		if i == 3 {
			break
		}
		log, err := jl.JobLog(ctx, f, st.Repo, st.CommitSHA, c)
		if err != nil {
			continue
		}
		read = true
		t, w := parseStackLog(log, st.Heads)
		if t != "" {
			return t, c.URL, ""
		}
		if w != "" {
			why = w
		}
	}
	if !read && len(try) > 0 {
		// A forge error or a log not there yet says nothing about the run.
		return "", "", ""
	}
	return "", "", why
}

// stackOverdue fails a running stack once it has run stackRunMax, so
// nothing waits on it for ever. Stacks stay on: it says nothing about the
// workflow.
func (s *Service) stackOverdue(ctx context.Context, st *queueStack, what string) {
	if s.now().Sub(st.CreatedAt) >= stackRunMax {
		s.endStack(ctx, st, stackFailed, what+" after "+fmtMinutes(stackRunMax))
	}
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseStackLog finds the tree the stack step printed, first in the log (a
// later line could come from the code under test), after checking that it
// merged exactly heads, in order.
func parseStackLog(log string, heads []stackHead) (tree, why string) {
	at := strings.Index(log, stackTreeMark)
	if at < 0 {
		return "", ""
	}
	before := log[:at]
	var merged []string
	for _, m := range stackLine.FindAllStringSubmatch(before, -1) {
		merged = append(merged, m[1]+" "+m[2])
	}
	if len(merged) != len(heads) {
		return "", fmt.Sprintf("its run merged %d heads, not the stack's %d", len(merged), len(heads))
	}
	for i, h := range heads {
		n, sha, _ := strings.Cut(merged[i], " ")
		if n != strconv.Itoa(h.Number) || !sameSHA(sha, h.SHA) {
			return "", fmt.Sprintf("its run merged #%s at %s where the stack has #%d at %s", n, short(sha), h.Number, short(h.SHA))
		}
	}
	rest := log[at+len(stackTreeMark):]
	if i := strings.IndexAny(rest, " \r\n"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.ToLower(rest)
	if (len(rest) != 40 && len(rest) != 64) || strings.Trim(rest, "0123456789abcdef") != "" {
		return "", "its run printed no tree id"
	}
	return rest, ""
}

// ---- at the front ----

// stackChain checks that the base's tip is st's base with the heads before
// st's own merged onto it, in order, as merge commits of those heads (so
// the base is what the stack's run tested, short of the own head). wait:
// the base can't be read as it is yet (Waypoint's last merge doesn't show),
// or it shows pr's own merge on st before Waypoint has recorded it.
func (s *Service) stackChain(ctx context.Context, f *Forge, pr *PullRequest, st *queueStack) (ok, wait bool) {
	tip, _, stale := s.behindBase(ctx, pr)
	if stale {
		return false, true
	}
	if tip == "" {
		return false, false
	}
	ok, err := s.stackChainFrom(ctx, f, st, tip)
	if err == nil && !ok {
		merged, merr := s.mergedOnStack(ctx, f, st, tip)
		if merr != nil || merged {
			return false, true
		}
	}
	return ok, err != nil
}

// mergedOnStack: tip is st's chain with st's own head merged onto it too.
// The pull request merged on its stack a moment ago and Waypoint's record of
// that hasn't caught up (the merge's own step records it and ends the
// stack): the base didn't move past the stack, so a step that comes in
// between waits instead of dropping the stack and the ones built on it
// (WP-576).
func (s *Service) mergedOnStack(ctx context.Context, f *Forge, st *queueStack, tip string) (bool, error) {
	return s.stackChainN(ctx, f, st, tip, st.Pos+1)
}

func (s *Service) stackChainFrom(ctx context.Context, f *Forge, st *queueStack, tip string) (bool, error) {
	return s.stackChainN(ctx, f, st, tip, st.Pos)
}

// stackChainN: tip is st's base with st's first n heads merged onto it, in
// order, each as a merge commit of that head.
func (s *Service) stackChainN(ctx context.Context, f *Forge, st *queueStack, tip string, n int) (bool, error) {
	cur := tip
	for i := n - 1; i >= 0; i-- {
		ps, err := s.stackParents(ctx, f, st.Repo, cur)
		if err != nil {
			return false, err
		}
		if len(ps) != 2 || !sameSHA(ps[1], st.Heads[i].SHA) {
			return false, nil
		}
		cur = ps[0]
	}
	return sameSHA(cur, st.BaseSHA), nil
}

// stackFront is the stack part of the front's turn (queueStep): a running
// stack is waited for, a passed one whose chain holds marks the draft ready
// with no update or full run to come, and one that doesn't hold is
// discarded. It reports whether the step is done.
func (s *Service) stackFront(ctx context.Context, b *Babysit, pr *PullRequest, rd *PRReadiness) bool {
	st, err := s.liveStackOf(ctx, pr.ID)
	if err != nil {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return false
	}
	if st == nil {
		// While the runners are saturated a draft front rides a batch with
		// the next ones instead of an update and a full run of its own
		// (WP-509).
		if pr.State != "draft" || slices.Contains(rd.Reasons, reasonConflict(pr.BaseRef)) {
			return false
		}
		style, on := s.stacksOn(ctx, f, pr)
		if !on || !s.poolSaturated(ctx, pr.ForgeID) {
			return false
		}
		if st = s.makeBatch(ctx, f, pr, style); st == nil {
			return false
		}
	}
	what := "its stack"
	if s.wasBatch(st) {
		what = "its batch"
	}
	if why := s.stackBroken(ctx, st, pr, nil); why != "" {
		return s.dropStack(ctx, b, pr, st, why)
	}
	if slices.Contains(rd.Reasons, reasonConflict(pr.BaseRef)) {
		return s.dropStack(ctx, b, pr, st, reasonConflict(pr.BaseRef))
	}
	if pr.State == "draft" && st.State == stackReady {
		// Marked ready on it, and back to draft since (a round): the usual way.
		return s.dropStack(ctx, b, pr, st, "it went back to draft")
	}
	s.pollStack(ctx, f, st)
	switch st.State {
	case stackRunning:
		s.setState(ctx, b, pr, BabysitWatching, "front of the merge queue: "+what+" ("+st.name()+") is running the full suite")
		return true
	case stackPassed, stackReady:
	default:
		return false // it failed: the usual update and full run
	}
	ok, wait := s.stackChain(ctx, f, pr, st)
	if wait {
		s.setState(ctx, b, pr, BabysitWatching, "front of the merge queue: waiting for "+cmp(pr.BaseRef, "its base")+" to show the merge just made")
		return true
	}
	if !ok {
		return s.dropStack(ctx, b, pr, st, cmp(pr.BaseRef, "its base")+" isn't the stack's heads merged in order any more")
	}
	if st.State == stackReady {
		// Not ready for another reason (readinessFor took the stack for the update).
		return false
	}
	if pr.State != "draft" {
		return s.dropStack(ctx, b, pr, st, "it was marked ready without it")
	}
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return false
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, pr.HeadSHA, CommitStatusPost{Context: stackContext, State: "success",
		Description: excerpt("passed on the merge queue's stack "+st.name(), 140), TargetURL: cmp(st.RunURL, pr.URL)}); err != nil {
		s.log.WarnContext(ctx, "post the stack's status", "pr", pr.ID, "err", err)
		return false
	}
	if err := s.markReadyOn(ctx, pr, st); err != nil {
		s.log.WarnContext(ctx, "mark a draft ready on its stack", "pr", pr.ID, "err", err)
		s.postStackFailure(ctx, f, pr, st, "Waypoint couldn't mark it ready on the stack")
		return s.dropStack(ctx, b, pr, st, "couldn't mark it ready: "+excerpt(err.Error(), 120))
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ? WHERE id = ? AND ended_at IS NULL`, stackReady, st.ID); err != nil {
		s.log.WarnContext(ctx, "record a stack ready", "stack", st.ID, "err", err)
	}
	s.setState(ctx, b, pr, BabysitWatching, "front of the merge queue: marked ready on "+what+" ("+st.name()+"), which passed; no update or full run")
	return true
}

// dropStack ends pr's stack at the front for the usual way: update, then
// the full run. One that marked pr ready puts it back to draft first, and
// says on its head that the stack no longer counts. It reports whether the
// step is done (it is when pr went back to draft: the next step goes on).
func (s *Service) dropStack(ctx context.Context, b *Babysit, pr *PullRequest, st *queueStack, why string) bool {
	if st.State != stackReady || pr.State != "open" {
		s.endStack(ctx, st, stackCancelled, why)
		return false
	}
	s.discardReadyStack(ctx, pr, st, why)
	s.setState(ctx, b, pr, BabysitWatching, "front of the merge queue: its stack or batch no longer holds ("+excerpt(why, 120)+"); back to draft for the usual update and full run")
	return true
}

// discardReadyStack takes back a ready pull request's stack: waypoint/stack
// fails on its head (so the edit's run doesn't skip its checks), it goes
// back to draft and the stack ends.
func (s *Service) discardReadyStack(ctx context.Context, pr *PullRequest, st *queueStack, why string) {
	if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil {
		s.postStackFailure(ctx, f, pr, st, why)
	}
	if pr.State == "open" && pr.WipAuto {
		if err := s.markDraft(ctx, pr); err != nil {
			s.log.WarnContext(ctx, "put a stacked pull request back to draft", "pr", pr.ID, "err", err)
		}
	}
	s.endStack(ctx, st, stackCancelled, why)
}

func (s *Service) postStackFailure(ctx context.Context, f *Forge, pr *PullRequest, st *queueStack, why string) {
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, pr.HeadSHA, CommitStatusPost{Context: stackContext, State: "failure",
		Description: excerpt("the stack "+st.name()+" no longer counts: "+why, 140), TargetURL: pr.URL}); err != nil {
		s.log.WarnContext(ctx, "post the stack's status", "pr", pr.ID, "err", err)
	}
}

// readyStack is pr's stack that stands in for its being behind its base
// (readinessFor): pr was marked ready on it at its head, and the base's tip
// is the stack's heads ahead of pr merged in order. nil otherwise. It
// changes nothing.
func (s *Service) readyStack(ctx context.Context, pr *PullRequest, tip string) *queueStack {
	if !queueHolder(pr) || !sameSHA(pr.readySHA, pr.HeadSHA) || tip == "" {
		return nil
	}
	st, err := s.liveStackOf(ctx, pr.ID)
	if err != nil || st == nil || st.State != stackReady || st.Ended || !sameSHA(st.own().SHA, pr.HeadSHA) {
		return nil
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return nil
	}
	if ok, err := s.stackChainFrom(ctx, f, st, tip); err != nil || !ok {
		return nil
	}
	return st
}

// dropReadyStackBehind: pr is behind its base (or the base can't be read as
// it is yet) and was marked ready on a stack. While the base's read is
// stale it waits; when the base moved past the stack's heads, pr goes back
// to draft for the usual update and full run. It reports whether pr had
// such a stack (so it isn't updated now).
func (s *Service) dropReadyStackBehind(ctx context.Context, pr *PullRequest) bool {
	st, err := s.liveStackOf(ctx, pr.ID)
	if err != nil || st == nil || st.State != stackReady {
		return false
	}
	tip, _, stale := s.behindBase(ctx, pr)
	if stale || s.readyStack(ctx, pr, tip) != nil {
		return true
	}
	if tip != "" {
		f, err := s.forgeByID(ctx, pr.ForgeID)
		if err != nil {
			return true // can't tell now: wait rather than drop the stack
		}
		ok, err := s.stackChainN(ctx, f, st, tip, st.Pos+1)
		if err != nil || ok {
			return true // it merged on it a moment ago, or can't be read now: wait
		}
	}
	s.discardReadyStack(ctx, pr, st, cmp(pr.BaseRef, "its base")+" moved past the stack's heads")
	s.nudgeBabysit(pr.ID, false)
	return true
}

// stackMerged follows a merge made on a stack: the merge commit is marked
// tested when its tree is the one the stack's run tested, and the stack ends.
// A merge on a batch that more of the batch's heads follow (WP-509) is marked
// tested when it is the batch's base with the heads up to its own merged in
// order: the batch's run vouches for each step of it, as the task asked, so
// main's push run for it doesn't run the suite again while the batch merges.
func (s *Service) stackMerged(ctx context.Context, f *Forge, pr *PullRequest, st *queueStack, merged string) {
	defer s.endStack(ctx, st, stackMerged, "merged as "+short(merged))
	tr, ok := s.Forges.(ForgeTrees)
	cl, ok2 := s.Forges.(ForgeChecks)
	if !ok || !ok2 || st.Tree == "" || merged == "" {
		return
	}
	desc := fmt.Sprintf("same tree as the merge queue's stack %s, which passed every check", st.name())
	if st.inBatch() {
		if ok, err := s.stackChainN(ctx, f, st, merged, st.Pos+1); err != nil || !ok {
			return // not the batch's merges: main's push run tests it
		}
		desc = fmt.Sprintf("step %d of %d of the merge queue batch %s, which passed every check", st.Pos+1, len(st.Heads), st.name())
	} else if mt, err := tr.CommitTree(ctx, f, pr.Repo, merged); err != nil || !strings.EqualFold(mt, st.Tree) {
		return // not what the stack tested: main's push run tests it
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, merged, CommitStatusPost{Context: testedContext, State: "success",
		Description: excerpt(desc, 140), TargetURL: cmp(st.RunURL, pr.URL)}); err != nil {
		s.log.WarnContext(ctx, "mark a stacked merge tested", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// stackRefused: the forge refused a merge a stack allowed. Stacks are off
// for the base for a day, and pr goes back to draft for the usual way.
func (s *Service) stackRefused(ctx context.Context, pr *PullRequest, st *queueStack, why string) {
	s.stacksOff(pr.ForgeID, pr.Repo, pr.BaseRef)
	if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil {
		s.postStackFailure(ctx, f, pr, st, "the forge refused the merge")
	}
	if pr.State == "open" && pr.WipAuto {
		if err := s.markDraft(ctx, pr); err != nil {
			s.log.WarnContext(ctx, "put a stacked pull request back to draft", "pr", pr.ID, "err", err)
		}
	}
	s.endStack(ctx, st, stackRefused, "the forge refused the merge: "+why+"; no stacks for a day")
}

// sweepStacks ends the live stacks whose pull request left its queue (or
// closed, merged elsewhere, moved its head) when no step of its own did.
func (s *Service) sweepStacks(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE ended_at IS NULL ORDER BY id`)
	if err != nil {
		return
	}
	var live []*queueStack
	for rows.Next() {
		if st, err := scanStack(rows); err == nil {
			live = append(live, st)
		}
	}
	rows.Close()
	for _, st := range live {
		if ctx.Err() != nil {
			return
		}
		if st.Ended { // a cascade from an earlier one ended it
			continue
		}
		pr, err := s.prByID(ctx, s.db.R, st.PRID)
		why := ""
		switch {
		case err != nil:
			why = "its pull request is gone"
		case pr.State == "merged" && sameSHA(st.own().SHA, pr.HeadSHA):
			// Merged at the stacked head (by a person, or no step of the
			// queue's saw it): the stacks built on it still hold.
			s.endStack(ctx, st, stackMerged, "merged outside its stack")
			continue
		case pr.State != "open" && pr.State != "draft":
			why = "its pull request is " + pr.State
		case !sameSHA(st.own().SHA, pr.HeadSHA):
			why = fmt.Sprintf("#%d's head moved", pr.Number)
		case !s.inQueue(ctx, pr):
			why = fmt.Sprintf("#%d left the merge queue", pr.Number)
		}
		if why == "" {
			continue
		}
		if pr != nil && st.State == stackReady && pr.State == "open" {
			s.discardReadyStack(ctx, pr, st, why)
		} else {
			s.endStack(ctx, st, stackCancelled, why)
		}
		for _, o := range live {
			if o.ID != st.ID && !o.Ended {
				if cur, err := s.stackByID(ctx, o.ID); err == nil && cur.Ended {
					o.Ended = true
				}
			}
		}
	}
}

func (s *Service) stackByID(ctx context.Context, id int64) (*queueStack, error) {
	return scanStack(s.db.R.QueryRowContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE id = ?`, id))
}
