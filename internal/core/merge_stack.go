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
	"encoding/json"
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
	stackOptTTL = 10 * time.Minute
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
	// RerunAt: when its failed jobs were run again (ms; 0: not yet, once
	// only). EndReason: one word for how it ended. ConflictPR, ConflictAhead:
	// the head that didn't merge, and whether it only conflicts with the
	// heads ahead of it (WP-1659).
	RerunAt       int64
	EndReason     string
	ConflictPR    int64
	ConflictAhead bool
	// Merged is read with the base's tip, not stored: how many of Heads the
	// tip already holds (lineGather).
	Merged int
}

// name says what the stack is: "main + #7 + #8".
func (st *queueStack) name() string {
	parts := []string{cmp(st.BaseRef, "the base")}
	for _, h := range st.Heads {
		parts = append(parts, fmt.Sprintf("#%d", h.Number))
	}
	return strings.Join(parts, " + ")
}

const stackCols = `id, forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, tree, run_url, note, created_at, ended_at IS NOT NULL, reran, end_reason, conflict_pr, conflict_ahead`

func scanStack(row interface{ Scan(...any) error }) (*queueStack, error) {
	var st queueStack
	var heads string
	var created int64
	if err := row.Scan(&st.ID, &st.ForgeID, &st.Repo, &st.BaseRef, &st.PRID, &st.BaseSHA, &heads, &st.Pos, &st.Branch, &st.CommitSHA,
		&st.State, &st.Tree, &st.RunURL, &st.Note, &created, &st.Ended, &st.RerunAt, &st.EndReason, &st.ConflictPR, &st.ConflictAhead); err != nil {
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

// candLineByCommit is the line of the live candidate on this commit.
func (s *Service) candLineByCommit(ctx context.Context, forgeID int64, repo, sha string) (lineRef, bool) {
	if sha == "" {
		return lineRef{}, false
	}
	ref := lineRef{forge: forgeID}
	if err := s.db.R.QueryRowContext(ctx, `SELECT repo, base_ref FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND commit_sha = ? AND ended_at IS NULL LIMIT 1`,
		forgeID, repo, strings.ToLower(sha)).Scan(&ref.repo, &ref.base); err != nil {
		return lineRef{}, false
	}
	return ref, true
}

// failedOn: pr's checks failed on sha.
func failedOn(pr *PullRequest, sha string) bool {
	return pr.CIState == CIFailed && sameSHA(pr.ciSHA, sha)
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
// request.
func (s *Service) stacksOn(ctx context.Context, f *Forge, pr *PullRequest) (string, bool) {
	key := baseKey(pr)
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
	// A pull request that merges after another (WP-912) can't merge yet, so
	// it holds no place in the line: a candidate for it would be CI for nothing.
	if after, _ := s.mergeWaitReasons(ctx, pr); len(after) > 0 {
		return after[0]
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

func (s *Service) stackByID(ctx context.Context, id int64) (*queueStack, error) {
	return scanStack(s.db.R.QueryRowContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE id = ?`, id))
}
