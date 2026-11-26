package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// automergeLabel arms a pull request on the forge.
const automergeLabel = "automerge"

// automationAgentPrefix marks a pull request armed by an automation: the
// armed_agent column holds "automation/<name>".
const automationAgentPrefix = "automation/"

// automationOf is the automation's name in an armed_agent value, or "".
func automationOf(agent string) string {
	name, _ := strings.CutPrefix(agent, automationAgentPrefix)
	if name == agent {
		return ""
	}
	return name
}

// PRReadiness says whether a pull request can be merged now, and if not,
// why not.
type PRReadiness struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons"`
	// OnlyWaiting: an armed pull request waits only for reviews to settle
	// and the quiet period (WP-133), or for pull requests it merges after
	// (WP-912); a person may still merge it now.
	OnlyWaiting bool `json:"only_waiting,omitempty"`
	// Checking: the forge said it doesn't merge, but is still working that out
	// after a push or a base move (WP-138); it is no conflict yet.
	Checking bool `json:"checking,omitempty"`
	// Notes say what doesn't hold the merge but is worth knowing, like an
	// audit that ended without findings (WP-355).
	Notes  []string `json:"notes,omitempty"`
	wake   *time.Time
	unread int // how many of reviewHolds' reasons say only that a read failed
	behind bool
	stack  *queueStack
}

// PRArmed is merge-when-ready set on a pull request.
type PRArmed struct {
	At    time.Time `json:"at"`
	By    string    `json:"by,omitempty"` // a person's name; empty when the label armed it
	Agent string    `json:"agent,omitempty"`
	Style string    `json:"style,omitempty"`
	Label bool      `json:"label,omitempty"` // the automerge label did
	// Setting: the repo's Merge automatically setting did (WP-133).
	Setting bool `json:"setting,omitempty"`
	// Scope is the setting's base branch scope, "" for any branch:
	// "into release/**" (WP-1041).
	Scope string `json:"scope,omitempty"`
	Note  string `json:"note,omitempty"` // why it hasn't merged, when it tried
}

// PRShipping is a pull request's way to merge: checks, the review gate,
// findings and whether it is ready.
type PRShipping struct {
	CI        string        `json:"ci"` // pending, passed, failed, none; "" unread
	Checks    []PRCheck     `json:"checks"`
	Gate      PRGate        `json:"gate"`
	Findings  []BotFinding  `json:"findings"`
	Mergeable *bool         `json:"mergeable"`
	Readiness PRReadiness   `json:"readiness"`
	Armed     *PRArmed      `json:"armed"`
	CanMerge  bool          `json:"can_merge"`
	Runs      []ShippingRun `json:"runs,omitempty"` // review runs on this head still going
	// Stage: "draft" or "ready (full run pending)" for a pull request Waypoint
	// holds as a draft first (WP-158), and its place in the merge queue when
	// it waits behind others (WP-222).
	Stage string `json:"stage,omitempty"`
	// Holds is what held it back while armed, newest last (WP-893): the
	// latest prHoldsShown that held a minute or more, or still hold.
	Holds []PRHold `json:"holds,omitempty"`
}

// PRHold is one thing that held an armed pull request back (merge_holds,
// WP-491): the reason with its numbers made N, on which head, from when to
// when (Until nil: it still holds).
type PRHold struct {
	What  string     `json:"what"`
	Head  string     `json:"head"`
	From  time.Time  `json:"from"`
	Until *time.Time `json:"until,omitempty"`
}

// prHoldsShown is how many holds a pull request's detail lists.
const prHoldsShown = 8

// prHolds is a pull request's latest holds of a minute or more, oldest
// first.
func (s *Service) prHolds(ctx context.Context, prID int64) ([]PRHold, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT what, head_sha, first_at, cleared_at FROM merge_holds
		WHERE pr_id = ? AND (cleared_at IS NULL OR cleared_at - first_at >= 60000)
		ORDER BY first_at DESC, id DESC LIMIT ?`, prID, prHoldsShown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRHold
	for rows.Next() {
		var h PRHold
		var from int64
		var until sql.NullInt64
		if err := rows.Scan(&h.What, &h.Head, &from, &until); err != nil {
			return nil, err
		}
		h.From = time.UnixMilli(from).UTC()
		if until.Valid {
			t := time.UnixMilli(until.Int64).UTC()
			h.Until = &t
		}
		h.Head = short(h.Head)
		out = append(out, h)
	}
	slices.Reverse(out)
	return out, rows.Err()
}

// ShippingRun is a bot run that is queued or running.
type ShippingRun struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// changesRequestedBy lists the people whose latest review of the pull
// request asked for changes.
func (s *Service) changesRequestedBy(ctx context.Context, prID int64) ([]string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT actor, kind FROM pr_events WHERE pr_id = ? AND bot = 0 AND actor != '' AND kind IN ('approved','changes_requested') ORDER BY created_at, id`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := map[string]string{}
	var order []string
	for rows.Next() {
		var who, kind string
		if err := rows.Scan(&who, &kind); err != nil {
			return nil, err
		}
		if _, ok := last[who]; !ok {
			order = append(order, who)
		}
		last[who] = kind
	}
	var out []string
	for _, who := range order {
		if last[who] == "changes_requested" {
			out = append(out, who)
		}
	}
	return out, rows.Err()
}

// readiness works out whether pr can be merged, from what Waypoint knows;
// an armed one also waits for its reviews to settle.
func (s *Service) readiness(ctx context.Context, pr *PullRequest) (*PRReadiness, error) {
	return s.readinessFor(ctx, pr, pr.ArmedAt != nil)
}

// readinessFor works out whether pr can be merged; with settle, as an armed
// merge: also after its reviews settled and the quiet period (WP-133).
func (s *Service) readinessFor(ctx context.Context, pr *PullRequest, settle bool) (*PRReadiness, error) {
	r := &PRReadiness{Reasons: []string{}}
	no := func(format string, args ...any) { r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...)) }
	switch pr.State {
	case "merged":
		no("already merged")
	case "closed":
		no("closed")
	case "draft":
		no(reasonDraft)
	}
	if pr.State != "open" && pr.State != "draft" {
		return r, nil
	}
	if pr.HeadSHA == "" {
		no("its head isn't known yet")
		return r, nil
	}
	switch conflict, checking := pr.mergeState(s.now()); {
	case conflict:
		no("%s", reasonConflict(pr.BaseRef))
	case checking:
		r.Checking = true
		no("the forge is still checking whether it merges with %s", cmp(pr.BaseRef, "its base"))
	}
	checks, err := s.prChecks(ctx, pr)
	if err != nil {
		return nil, err
	}
	if pr.ciSHA != pr.HeadSHA {
		no("checks haven't been read yet")
	} else if ok, why := ciSummary(pr.CIState, checks); !ok {
		no("%s", why)
	}
	r.Reasons = append(r.Reasons, fullRunReasons(pr, checks)...)
	pol, attached, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return nil, err
	}
	if attached {
		r.Reasons = append(r.Reasons, requiredChecksReasons(pol, checks, pr.HeadSHA)...)
	}
	holds, err := s.reviewHolds(ctx, pr, settle, r)
	if err != nil {
		return nil, err
	}
	r.Reasons = append(r.Reasons, holds...)
	after, notes := s.mergeWaitReasons(ctx, pr)
	r.Notes = append(r.Notes, notes...)
	if settle && attached && len(r.Reasons) == 0 && len(after) == 0 {
		tip, behind, stale, err := s.behindBaseErr(ctx, pr)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "read whether a pull request is behind its base", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
			}
			no("whether it is behind %s couldn't be read", cmp(pr.BaseRef, "its base"))
			if next := s.now().Add(driftRetry); r.wake == nil || next.Before(*r.wake) {
				r.wake = &next
			}
		} else if behind || stale {
			if st := s.readyCand(ctx, pr, tip); st != nil && !stale {
				r.stack = st
			} else {
				r.behind = true
				no("%s", reasonBehind(pr.BaseRef))
			}
		} else if st := s.readyCand(ctx, pr, tip); st != nil {
			r.stack = st
		}
	}
	if settle && attached {
		waits, wake, err := s.settleWaits(ctx, pr, pol, checks)
		if err != nil {
			return nil, err
		}
		r.OnlyWaiting = len(r.Reasons) == 0 && len(waits) > 0
		r.Reasons = append(r.Reasons, waits...)
		if wake != nil && (r.wake == nil || wake.Before(*r.wake)) {
			r.wake = wake
		}
	}
	if len(after) > 0 && (len(r.Reasons) == 0 || r.OnlyWaiting) {
		r.OnlyWaiting = true
	}
	r.Reasons = append(r.Reasons, after...)
	r.Ready = len(r.Reasons) == 0
	return r, nil
}

// reviewHolds is what holds pr's head for its reviewers and the rounds they
// lead to: the review gate, a person's request for changes, review comments
// not dealt with, the newest audit's worst findings, a babysitting round at
// work and, with settle, an adversarial review of one of its tasks. Each of
// them can still move the head. It notes on r what it learns besides (an
// audit's note, when to look again).
func (s *Service) reviewHolds(ctx context.Context, pr *PullRequest, settle bool, r *PRReadiness) ([]string, error) {
	var reasons []string
	no := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	retry := func() {
		r.unread++
		if next := s.now().Add(driftRetry); r.wake == nil || next.Before(*r.wake) {
			r.wake = &next
		}
	}
	unread := func(what, reason string, err error) {
		if readFault(err) {
			s.log.WarnContext(ctx, what, "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
		}
		no("%s", reason)
		retry()
	}
	if on, sev, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo); err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read a repo's review gate setting for its merge", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
		}
		no("the review gate setting couldn't be read")
		retry()
	} else if on {
		switch {
		case pr.GateState == GateSuccess && pr.gateSHA == pr.HeadSHA:
		case pr.GateState == "" || pr.gateSHA != pr.HeadSHA:
			no("the review gate hasn't been evaluated for this head")
		default:
			no("review gate: %s", cmp(pr.gateNote, "not green ("+priority(sev)+" and above)"))
		}
	}
	people, err := s.changesRequestedBy(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		no("%s asked for changes", p)
	}
	undealt, err := s.undealtComments(ctx, pr)
	if err != nil {
		return nil, err
	}
	if n := len(undealt); n > 0 {
		no("%d review comment%s not dealt with", n, plural(n))
	}
	going, _, auditErr := s.auditGoingErr(ctx, pr.ID)
	if auditErr != nil {
		if readFault(auditErr) {
			s.log.WarnContext(ctx, "read a pull request's running audits for its merge", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", auditErr)
		}
		no("the running audits couldn't be read")
		retry()
	}
	holds, note, err := s.auditHolds(ctx, pr.ID, going)
	if err != nil {
		unread("read a pull request's audit findings for its merge", "the audit findings couldn't be read", err)
	}
	reasons = append(reasons, holds...)
	if note != "" {
		r.Notes = append(r.Notes, note)
	}
	if why, err := s.footprintDue(ctx, pr); err != nil {
		unread("read a pull request's babysit for its footprint check", "its babysit couldn't be read for a footprint check", err)
	} else if why != "" {
		no("%s", why)
	}
	driftWhy, driftAt := s.driftHold(ctx, pr, settle)
	if driftWhy != "" {
		no("%s", driftWhy)
		if driftWhy == driftUnread {
			r.unread++
		}
	}
	if driftAt != nil && (r.wake == nil || driftAt.Before(*r.wake)) {
		r.wake = driftAt
	}
	if running, err := s.roundRunning(ctx, pr.ID); err != nil {
		unread("read whether a babysitting round is running", "whether a babysitting round is still working on it couldn't be read", err)
	} else if running {
		no("a babysitting round is still working on it")
	}
	if why, err := s.babysitParked(ctx, pr); err != nil {
		unread("read whether a babysit is parked", "whether its babysit is parked couldn't be read", err)
	} else if why != "" {
		no("%s", why)
	}
	if going != 0 && settle {
		at, err := s.auditDeadline(ctx, going)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "read when a running audit stops holding a merge", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "audit", going, "err", err)
			}
			next := s.now().Add(driftRetry)
			at = &next
		}
		if at != nil && (r.wake == nil || at.Before(*r.wake)) {
			r.wake = at
		}
	}
	if settle {
		if why, until, err := s.reviewHold(ctx, pr); err != nil {
			unread("read the adversarial reviews that hold a pull request", "whether an adversarial review is still working on it couldn't be read", err)
		} else if why != "" {
			no("%s", why)
			if r.wake == nil || until.Before(*r.wake) {
				r.wake = until
			}
		}
	}
	return reasons, nil
}

// PRReadiness is the public form: who may see the pull request may ask.
func (s *Service) PRReadiness(ctx context.Context, actor *Actor, prID int64) (*PRReadiness, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	return s.readiness(ctx, pr)
}

// prShipping fills a pull request's way to merge for the actor.
func (s *Service) prShipping(ctx context.Context, actor *Actor, pr *PullRequest) (*PRShipping, error) {
	sh := &PRShipping{CI: pr.CIState, Mergeable: pr.Mergeable, Findings: []BotFinding{}}
	if _, checking := pr.mergeState(s.now()); checking {
		sh.Mergeable = nil
	}
	if pr.ciSHA != pr.HeadSHA {
		sh.CI = ""
	}
	var err error
	if sh.Checks, err = s.prChecks(ctx, pr); err != nil {
		return nil, err
	}
	on, sev, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return nil, err
	}
	sh.Gate = PRGate{Enabled: on, Severity: sev, State: pr.GateState, Note: pr.gateNote, SHA: pr.gateSHA, Open: pr.gateOpen}
	findings, err := s.prFindings(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil, err
	}
	history, err := s.findingHistory(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range findings {
		f.History = history[f.ID]
		if f.State == FindingOpen && on && !blocksGate(f.Severity, sev) {
			sh.Gate.Below++
		}
		sh.Findings = append(sh.Findings, f.BotFinding)
	}
	rd, err := s.readiness(ctx, pr)
	if err != nil {
		return nil, err
	}
	sh.Readiness = *rd
	sh.Stage = prStage(pr, sh.Checks)
	if q := s.queueStage(ctx, pr); q != "" {
		sh.Stage = strings.TrimPrefix(sh.Stage+", "+q, ", ")
	}
	if pr.ArmedAt != nil {
		a := &PRArmed{At: *pr.ArmedAt, Agent: pr.ArmedAgent, Style: pr.ArmedStyle, Label: pr.ArmedLabel, Note: pr.armedNote}
		if a.Agent == mergeSettingAgent {
			a.Agent, a.Setting = "", true
			if pol, ok, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo); err == nil && ok {
				a.Scope = pol.scopeText(s.defaultBranchKnown(pr))
			}
		}
		if pr.armedBy != nil {
			if u, err := s.userByID(ctx, s.db.R, *pr.armedBy); err == nil {
				a.By = u.DisplayName
			}
		}
		sh.Armed = a
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, kind, status FROM bot_runs WHERE pr_id = ? AND status IN ('queued','running') ORDER BY id`, pr.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x ShippingRun
		if err := rows.Scan(&x.ID, &x.Kind, &x.Status); err != nil {
			rows.Close()
			return nil, err
		}
		sh.Runs = append(sh.Runs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if sh.Holds, err = s.prHolds(ctx, pr.ID); err != nil {
		return nil, err
	}
	sh.CanMerge = s.mayMerge(ctx, actor, pr) == nil
	return sh, nil
}

// agentFamily is the part of an agent identity that names the model line:
// "anthropic/claude-opus-5-5" is claude.
func agentFamily(identity string) string {
	if _, rest, ok := strings.Cut(identity, "/"); ok {
		identity = rest
	}
	fam, _, _ := strings.Cut(strings.ToLower(identity), "-")
	return fam
}

// mayMerge says whether the actor may merge or arm a pull request: editors
// of a project that has its repo, and agents only for pull requests they
// wrote.
func (s *Service) mayMerge(ctx context.Context, actor *Actor, pr *PullRequest) error {
	_, err := s.mayMergeIn(ctx, actor, pr)
	return err
}

// mayMergeIn is mayMerge and also says which project lets the actor merge:
// the first that has the repo and gives them editor rights (WP-999).
func (s *Service) mayMergeIn(ctx context.Context, actor *Actor, pr *PullRequest) (int64, error) {
	if actor == nil || actor.User == nil {
		return 0, ErrUnauthorized
	}
	pid, err := s.prAuthority(ctx, actor, pr, RoleEditor)
	if err != nil {
		return 0, err
	}
	if actor.IsAgent() {
		own := actor.Agent != "" && pr.Agent != "" && agentFamily(actor.Agent) == agentFamily(pr.Agent)
		if !own {
			return 0, forbidden("agents merge only the pull requests they wrote")
		}
	}
	return pid, nil
}

// MergeInput says how to merge.
type MergeInput struct {
	Style        string // merge, squash, rebase; "" is the repo's default
	DeleteBranch *bool  // default: delete the branch
	Title        string
	Message      string
	WhenReady    bool // arm the pull request instead of merging now
	// Head, when set, is the only head to merge: if the pull request's
	// head moved since (wp pr wait --merge waited on it), it isn't merged.
	Head   string
	armed  bool
	notice *string
}

// say tells the caller of a merge a notice, when they asked for it.
func (in MergeInput) say(asked bool, text string) {
	if asked && in.notice != nil {
		*in.notice = text
	}
}

// afterSettle is pr as the record has it once settleStack may have moved its
// base; pr itself when that can't be read.
func (s *Service) afterSettle(ctx context.Context, pr *PullRequest) *PullRequest {
	if cur, err := s.prByID(ctx, s.db.R, pr.ID); err == nil {
		return cur
	}
	return pr
}

// MergeResult is what a merge request did.
type MergeResult struct {
	Merged  bool     `json:"merged"`
	Armed   bool     `json:"armed"`
	Reasons []string `json:"reasons,omitempty"` // armed but not ready: why
	// ByHand: an agent asked to arm a pull request whose repo has Merge
	// automatically off; it wasn't armed, a person merges it (WP-520).
	ByHand bool   `json:"by_hand,omitempty"`
	SHA    string `json:"sha,omitempty"` // merged: the commit it became, when the forge says
	// Warning: another session holds this pull request (WP-117).
	Warning string `json:"warning,omitempty"`
}

// refuseDiagnoseMerge refuses an agent's merge or arm of a pull request linked
// to a task whose objective is in a diagnose round: a diagnose round ships no
// code, and the merge would end it as an ordinary reading (WP-1449). A person
// may still merge it; a pull request already armed merges on its own as it
// would, since Waypoint's automatic merge doesn't come through here.
func (s *Service) refuseDiagnoseMerge(ctx context.Context, pr *PullRequest) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT el.issue_id FROM external_links el
		WHERE el.kind = 'pull' AND (el.url = ? OR (el.repo = ? COLLATE NOCASE AND el.number = ?))`, pr.URL, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		o, err := s.objectiveRow(ctx, s.db.R, id)
		if err != nil {
			return err
		}
		if o != nil && o.State == ObjectiveActive && o.diagnoseRound() {
			return invalid("%s's round %d is a diagnose round, which ships no code: record the diagnosis first (wp objective diagnose %s -f FILE), then ship in the round that reopens. A person can merge this pull request, or skip the diagnosis with wp objective next %s",
				o.Key, o.Round, o.Key, o.Key)
		}
	}
	return nil
}

// MergePR merges a pull request that is ready, or with WhenReady arms it to
// merge when it becomes ready (at once if it already is). Agents may use it
// on their own pull requests. Merges are audited.
func (s *Service) MergePR(ctx context.Context, actor *Actor, prID int64, in MergeInput) (res *MergeResult, err error) {
	if err := actor.need(PermPullsMerge); err != nil {
		return nil, err
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if err := s.mayMerge(ctx, actor, pr); err != nil {
		return nil, err
	}
	if !validMergeStyle(in.Style) {
		return nil, invalid("merge style must be merge, squash or rebase")
	}
	if pr.State != "open" && pr.State != "draft" {
		return nil, invalid("this pull request is %s", pr.State)
	}
	if actor.IsAgent() {
		if err := s.refuseDiagnoseMerge(ctx, pr); err != nil {
			return nil, err
		}
	}
	warning := s.prClaimWarning(ctx, actor, pr.ID)
	var notice string
	defer func() {
		if err != nil || res == nil || (!res.Merged && !res.Armed) {
			return
		}
		res.Warning = joinNotes(warning, notice)
		if !res.Merged {
			s.claimPR(ctx, actor, pr.ID)
		}
	}()
	if in.WhenReady {
		if s.forgeEnded(ctx, pr.ID) {
			cur, err := s.prByID(ctx, s.db.R, pr.ID)
			if err != nil {
				return nil, err
			}
			return nil, invalid("this pull request is %s", cur.State)
		}
		s.settleStack(ctx, pr, false)
		pr = s.afterSettle(ctx, pr)
		if pr.Outside {
			return nil, errPROutside(pr)
		}
		if actor.IsAgent() {
			pol, ok, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
			if err != nil {
				return nil, err
			}
			byHand := ""
			switch {
			case !ok:
			case !pol.AutoMerge:
				byHand = pr.Repo + " merges by hand (Merge automatically is off): a person merges it"
			default:
				why, known := s.autoMergeBaseSkip(ctx, pr, pol, false)
				switch {
				case !known:
					byHand = pr.Repo + " merges automatically only " + pol.scopeText("") + ", and the forge didn't say which is its default branch: a person merges it"
				case why != "":
					byHand = pr.Repo + " merges " + why + " by hand (Merge automatically arms only pull requests " + pol.scopeText(s.defaultBranchKnown(pr)) + "): a person merges it"
				}
			}
			if byHand != "" {
				if pr.ArmedAt != nil {
					return &MergeResult{Armed: true}, nil
				}
				return &MergeResult{ByHand: true, Reasons: []string{byHand}}, nil
			}
		}
		if in.Head != "" && !sameSHA(pr.HeadSHA, in.Head) {
			return nil, invalid("its head is %s, not %s; not armed", short(pr.HeadSHA), short(in.Head))
		}
		return s.armMerge(ctx, actor, pr, in.Style)
	}
	s.settleStack(ctx, pr, false)
	pr = s.afterSettle(ctx, pr)
	in.notice = &notice
	sha, err := s.mergeNow(ctx, actor, pr, in, "from the pull request page")
	if err != nil {
		return nil, err
	}
	return &MergeResult{Merged: true, SHA: sha}, nil
}

// DisarmMerge takes merge-when-ready off a pull request.
func (s *Service) DisarmMerge(ctx context.Context, actor *Actor, prID int64) error {
	if err := actor.need(PermPullsMerge); err != nil {
		return err
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return err
	}
	if err := s.mayMerge(ctx, actor, pr); err != nil {
		return err
	}
	projectID, rerr := s.prAuthority(ctx, actor, pr, RoleViewer)
	if readFault(rerr) && !missingOrDenied(rerr) {
		s.log.WarnContext(ctx, "read the project of a pull request whose merge is taken off", "pr", pr.ID, "err", rerr)
	}
	defer forgeLocks.lock(fmt.Sprintf("merge:%d", pr.ID))()
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = NULL, armed_by = NULL, armed_agent = '', armed_style = '', armed_note = '',
			armed_label = CASE WHEN armed_label = 1 THEN 2 ELSE armed_label END, arm_declined = 1, merge_wake_at = NULL, merge_retry_n = 0, queued_at = NULL WHERE id = ?`, pr.ID); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.disarm", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "took merge when ready off “" + pr.Title + "”"})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "disarmed", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
}

func (s *Service) armMerge(ctx context.Context, actor *Actor, pr *PullRequest, style string) (*MergeResult, error) {
	projectID, err := s.prAuthority(ctx, actor, pr, RoleEditor)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		agent := actor.agentName()
		if agent == "" && actor.Automation != nil {
			agent = automationAgentPrefix + actor.Automation.Name
		}
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = coalesce(armed_at, ?), armed_by = ?, armed_agent = ?, armed_style = ?, armed_note = '', arm_declined = 0 WHERE id = ?`,
			s.nowMS(), nullUser(actor.User.ID), agent, style, pr.ID); err != nil {
			return err
		}
		detail := "set “" + pr.Title + "” to merge when ready"
		if style != "" {
			detail += " (" + style + ")"
		}
		entry := AuditEntry{Action: "pr.arm", Category: AuditApproval, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail, About: pr.Agent}
		if actor.Automation != nil {
			entry.Actor = "automation “" + actor.Automation.Name + "”"
		}
		t.audit(entry)
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "armed", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	s.maybeAutoMerge(bg, pr.ID)
	pr, err = s.prByID(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil, err
	}
	if pr.State == "merged" {
		return &MergeResult{Merged: true}, nil
	}
	rd, err := s.readiness(ctx, pr)
	if err != nil {
		return nil, err
	}
	return &MergeResult{Armed: true, Reasons: rd.Reasons}, nil
}

// armByLabel arms (or disarms) a pull request by its `automerge` label, in
// the transaction that recorded it.
func (s *Service) armByLabel(ctx context.Context, t *tx, pr *PullRequest, live bool) error {
	has := live && !pr.Outside && slices.ContainsFunc(pr.Labels, func(l string) bool { return strings.EqualFold(l, automergeLabel) })
	switch {
	case !live && pr.ArmedAt != nil:
		pr.ArmedAt = nil
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = NULL, armed_by = NULL, armed_agent = '', armed_style = '', armed_note = '', queued_at = NULL WHERE id = ?`, pr.ID)
		return err
	case has && pr.ArmedAt == nil && !pr.labelOff:
		now := s.now()
		pr.ArmedAt, pr.ArmedLabel = &now, true
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = NULL, armed_agent = '', armed_style = '', armed_label = 1, armed_note = '' WHERE id = ?`, now.UnixMilli(), pr.ID)
		return err
	case !has && (pr.ArmedLabel || pr.labelOff):
		pr.ArmedAt, pr.ArmedLabel, pr.labelOff = nil, false, false
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = CASE WHEN armed_label = 1 THEN NULL ELSE armed_at END,
			armed_by = CASE WHEN armed_label = 1 THEN NULL ELSE armed_by END, arm_declined = CASE WHEN armed_label = 1 THEN 1 ELSE arm_declined END,
			armed_label = 0, queued_at = CASE WHEN armed_label = 1 THEN NULL ELSE queued_at END WHERE id = ?`, pr.ID)
		return err
	}
	return nil
}

// maybeAutoMerge merges an armed pull request if it is ready now.
func (s *Service) maybeAutoMerge(ctx context.Context, prID int64) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if errors.Is(err, ErrNotFound) {
		return
	}
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read an armed pull request", "pr", prID, "err", err)
		}
		at := s.now().Add(driftRetry)
		s.setMergeWake(ctx, prID, &at)
		return
	}
	if pr.ArmedAt == nil || (pr.State != "open" && pr.State != "draft") || pr.Outside {
		return
	}
	s.ensureArmedBabysit(ctx, pr)
	s.settleStack(ctx, pr, true)
	if cur, err := s.prByID(ctx, s.db.R, pr.ID); err == nil {
		pr = cur
	}
	rd, err := s.readiness(ctx, pr)
	if err != nil {
		s.armedMergeUnread(ctx, pr, "read whether an armed pull request is ready", "whether it is ready couldn't be read", err)
		return
	}
	s.recordHolds(ctx, pr, rd.Reasons)
	s.setMergeWake(ctx, pr.ID, rd.wake)
	if rd.Checking {
		s.scheduleMergeRecheck(ctx, pr.ID)
	}
	if rd.behind || queueable(pr, rd.Reasons) {
		s.joinUnbabysat(ctx, pr)
	}
	if rd.behind {
		return
	}
	if !rd.Ready {
		return
	}
	actor, how := SystemActor(), "armed with the automerge label"
	switch {
	case pr.ArmedAgent == mergeSettingAgent:
		how = "armed by " + mergeSettingName
	case automationOf(pr.ArmedAgent) != "":
		how = "armed by the “" + automationOf(pr.ArmedAgent) + "” automation"
	}
	if pr.armedBy != nil {
		u, err := s.userByID(ctx, s.db.R, *pr.armedBy)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.armedMergeUnread(ctx, pr, "read the person who armed a pull request", "the person who armed it couldn't be read", err)
			return
		}
		if err != nil || u.Disabled {
			s.armNote(ctx, pr.ID, mergeFailedNote+"the person who armed it is gone")
			return
		}
		agent := pr.ArmedAgent
		if automationOf(agent) != "" {
			agent = ""
		}
		actor = &Actor{User: u, Agent: agent}
		if err := s.mayMerge(ctx, actor, pr); err != nil {
			if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrNotFound) {
				s.armedMergeUnread(ctx, pr, "read whether the person who armed a pull request may merge it", "whether "+u.DisplayName+" may merge it couldn't be read", err)
				return
			}
			s.armNote(ctx, pr.ID, mergeFailedNote+u.DisplayName+" can no longer merge it")
			return
		}
		how = "armed by " + u.DisplayName
		if by := automationOf(pr.ArmedAgent); by != "" {
			how += " through the “" + by + "” automation"
		}
	}
	if _, err := s.mergeNow(ctx, actor, pr, MergeInput{Style: pr.ArmedStyle, armed: true}, how); err != nil {
		var fc *forgeChecking
		var down *forgeDownError
		switch {
		case errors.Is(err, errArmOutOfScope):
			return
		case errors.As(err, &fc):
			s.retryArmedMerge(ctx, pr, "the forge is still checking whether it merges")
			return
		case errors.As(err, &down):
			s.retryArmedMerge(ctx, pr, down.what)
			return
		}
		s.log.WarnContext(ctx, "merge when ready", "repo", pr.Repo, "number", pr.Number, "err", err)
		s.armNote(ctx, pr.ID, mergeFailedNote+mergeNote(err))
	}
}

// armedMergeUnread: a read an armed merge needs failed. The merge is held
// (nothing was merged, and nothing is marked that would stop a retry), the
// log names the pull request, and the page says it will look again.
func (s *Service) armedMergeUnread(ctx context.Context, pr *PullRequest, what, note string, err error) {
	if readFault(err) {
		s.log.WarnContext(ctx, what, "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
	}
	at := s.now().Add(driftRetry)
	s.armNote(ctx, pr.ID, note+"; Waypoint will try again at "+at.UTC().Format("15:04")+" UTC")
	s.setMergeWake(ctx, pr.ID, &at)
}

// mergeFailedNote starts the note of an armed merge that was tried and
// didn't happen: the babysit asks a person, so it doesn't hold its merge
// queue (WP-222).
const mergeFailedNote = "couldn't merge: "

// mergeNote says why an armed merge didn't happen, for the page.
func mergeNote(err error) string {
	if IsInvalid(err) {
		return err.Error()
	}
	return "the merge failed; Waypoint's log has the details"
}

func (s *Service) armNote(ctx context.Context, prID int64, note string) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET armed_note = ? WHERE id = ?`, excerpt(note, 300), prID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// heldRead turns a read that failed before a merge into a held merge, tried
// again, not a failure that asks a person: err is returned as it is when it
// is an answer (a refusal, out of scope, not found or denied) or already one
// that retries. what names the read in the log, note what the page says
// (WP-1406).
func (s *Service) heldRead(ctx context.Context, pr *PullRequest, what, note string, err error) error {
	var down *forgeDownError
	var fc *forgeChecking
	if !readFault(err) || IsInvalid(err) || errors.Is(err, errArmOutOfScope) || missingOrDenied(err) || errors.As(err, &down) || errors.As(err, &fc) {
		return err
	}
	s.log.WarnContext(ctx, what, "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
	return &forgeDownError{what: note, then: "try again", err: err}
}

// mergeNow merges after reading the forge afresh: the pull request, its
// checks and its gate. The head it passes is the one it checked.
func (s *Service) mergeNow(ctx context.Context, actor *Actor, pr *PullRequest, in MergeInput, how string) (string, error) {
	defer forgeLocks.lock(fmt.Sprintf("merge:%d", pr.ID))()
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return "", err
	}
	cl, ok := s.Forges.(ForgeMerger)
	prc, ok2 := s.Forges.(ForgePRs)
	if !ok || !ok2 {
		return "", invalid("this forge client can't merge")
	}
	if f.token == "" {
		return "", invalid("the forge has no API token, so Waypoint can't merge")
	}
	start := time.Now()
	fp, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return "", fmt.Errorf("read the pull request: %w", s.forgeUnanswered(f, time.Since(start), err))
	}
	cur, err := s.recordPR(ctx, f, pr.Repo, fp, false)
	if err != nil {
		return "", s.heldRead(ctx, pr, "record the pull request before merging it", "the pull request couldn't be recorded", err)
	}
	if cur.State == "merged" || cur.State == "closed" {
		return "", invalid("this pull request is %s", cur.State)
	}
	if in.Head != "" && !sameSHA(fp.HeadSHA, in.Head) {
		return "", invalid("its head moved to %s since %s; not merged", short(fp.HeadSHA), short(in.Head))
	}
	if in.armed && cur.ArmedAt == nil {
		return "", invalid("merge when ready was taken off")
	}
	if in.armed && scopedArm(cur) {
		if err := s.armStillInScope(ctx, cur); err != nil {
			return "", s.heldRead(ctx, pr, "check an armed merge is still in scope", "whether the arming is still in scope couldn't be read", err)
		}
	}
	start = time.Now()
	if err := s.refreshHead(ctx, f, pr.Repo, fp.HeadSHA); err != nil {
		return "", fmt.Errorf("read the checks: %w", s.forgeUnanswered(f, time.Since(start), err))
	}
	if err := s.checkPRErr(ctx, pr.ID, "merge"); err != nil {
		return "", fmt.Errorf("check the review gate: %w", err)
	}
	if cur, err = s.prByID(ctx, s.db.R, pr.ID); err != nil {
		return "", err
	}
	rd, err := s.readinessFor(ctx, cur, in.armed)
	if err != nil {
		return "", s.heldRead(ctx, pr, "work out whether a pull request may merge", "whether it may merge couldn't be read", err)
	}
	if !in.armed && !actor.IsAgent() {
		if rd.Reasons = withoutMergeWaits(rd.Reasons); len(rd.Reasons) == 0 {
			rd.Ready = true
		}
	}
	if !in.armed && actor.IsAgent() {
		if why, _ := s.driftHold(ctx, cur, true); why != "" && !slices.Contains(rd.Reasons, why) {
			rd.Reasons = append(rd.Reasons, why)
			rd.Ready = false
		}
	}
	if !rd.Ready {
		if rd.Checking {
			return "", checkingErr("not ready to merge: " + strings.Join(rd.Reasons, "; "))
		}
		return "", invalid("not ready to merge: %s", strings.Join(rd.Reasons, "; "))
	}
	del := true
	if in.DeleteBranch != nil {
		del = *in.DeleteBranch
	}
	askedDelete := in.DeleteBranch != nil && *in.DeleteBranch
	var stacked []*PullRequest
	if del {
		if stacked, err = s.prsStackedOn(ctx, cur); err != nil {
			return "", s.heldRead(ctx, pr, "list the pull requests stacked on the branch being merged", "the pull requests stacked on it couldn't be read", err)
		}
		if len(stacked) > 0 {
			switch kept, known := s.longLivedBranch(ctx, f, cur); {
			case !known:
				del, stacked = false, nil
				s.log.WarnContext(ctx, "left a merged branch for the pull requests stacked on it", "repo", cur.Repo, "branch", cur.HeadRef)
				in.say(askedDelete, cur.HeadRef+" was not deleted: pull requests are open into it and the forge didn't say whether the repo keeps it")
			case kept:
				del, stacked = false, nil
				s.log.WarnContext(ctx, "left a kept branch with pull requests open into it", "repo", cur.Repo, "branch", cur.HeadRef)
				in.say(askedDelete, cur.HeadRef+" was not deleted: pull requests are open into it and the repo keeps it")
			}
		}
	}
	style := cmp(in.Style, cur.ArmedStyle)
	projectID, err := s.attachedProjectFor(ctx, actor, cur)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project a merge is audited under", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
		}
		return "", &forgeDownError{what: "the project the merge is audited under couldn't be read", then: "try again", err: err}
	}
	s.noteMerging(cur)
	start = time.Now()
	err = cl.MergePR(ctx, f, pr.Repo, fp, MergeOptions{Style: style, HeadSHA: fp.HeadSHA, DeleteBranch: del && len(stacked) == 0, Title: in.Title, Message: in.Message})
	var kept *branchNotDeleted
	if err != nil && !errors.As(err, &kept) {
		if down := s.forgeUnanswered(f, time.Since(start), err); errors.Is(down, ErrUnavailable) {
			return "", fmt.Errorf("merge: %w", down)
		}
		s.noteNotMerged(cur)
		if transientMergeRefusal(err) {
			return "", checkingErr("the forge is still checking whether it merges, so it refused: " + mergeRefusal(err))
		}
		if rd.stack != nil && (isStatus(err, http.StatusMethodNotAllowed) || isStatus(err, http.StatusUnprocessableEntity)) {
			s.candRefused(ctx, cur, rd.stack, mergeRefusal(err))
		}
		return "", invalid("the forge refused the merge: %s", mergeRefusal(err))
	}
	detail := fmt.Sprintf("merged “%s” into %s from Waypoint (%s)", cur.Title, cmp(cur.BaseRef, "its base"), cmp(style, "the repo's default style"))
	if how != "" {
		detail += ", " + how
	}
	e := AuditEntry{Action: "pr.merge", Category: AuditApproval, ProjectID: projectID, TargetType: "pr", TargetID: cur.ID,
		Target: fmt.Sprintf("%s#%d", cur.Repo, cur.Number), URL: cur.URL, Detail: detail, About: cur.Agent}
	if actor.User.ID == 0 {
		e.Actor = "automerge label"
		if by := automationOf(cur.ArmedAgent); by != "" {
			e.Actor = "automation “" + by + "”"
		}
		if cur.ArmedAgent == mergeSettingAgent {
			e.Actor = mergeSettingName
		}
	}
	if actor.Automation != nil {
		e.Actor = "automation “" + actor.Automation.Name + "”"
	}
	s.audit(ctx, actor, e)
	var sha string
	if fresh, err := prc.GetPR(ctx, f, pr.Repo, pr.Number); err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read a pull request after merging it", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
	} else {
		sha = fresh.MergeSHA
		s.noteMerged(cur, sha)
		if _, err := s.recordPR(ctx, f, pr.Repo, fresh, false); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "record a pull request after merging it", "repo", pr.Repo, "number", pr.Number, "err", err)
			}
		}
	}
	s.afterStackMerge(ctx, f, cur, stacked)
	if rd.stack != nil {
		s.candMergedOn(ctx, f, cur, rd.stack, sha)
	} else {
		s.markTested(ctx, f, cur, fp.HeadSHA, sha)
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET armed_at = NULL, armed_by = NULL, armed_agent = '', armed_style = '', armed_note = '', merge_wake_at = NULL, merge_retry_n = 0 WHERE id = ?`, pr.ID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	return sha, nil
}

// errArmOutOfScope: an armed merge's base is no longer one Merge
// automatically covers, so its arming came off instead.
var errArmOutOfScope = errors.New("its base is outside Merge automatically's branches")

// armStillInScope checks, as an armed merge is about to happen, that the
// setting's or an agent's arming still points at a base Merge automatically
// covers, asking the forge for the default branch afresh (WP-1041): a base
// retargeted or a scope narrowed that no webhook told of, or a renamed
// default branch, never merges. Out of scope, the arming comes off.
func (s *Service) armStillInScope(ctx context.Context, pr *PullRequest) error {
	pol, ok, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return err
	}
	if !ok || !pol.AutoMerge {
		return nil
	}
	why, known := s.autoMergeBaseSkip(ctx, pr, pol, true)
	if !known {
		return &forgeDownError{what: "the forge didn't say which is " + pr.Repo + "'s default branch", then: "not merged", err: errors.New("default branch unknown")}
	}
	if why != "" {
		s.disarmOutOfScope(ctx, pr, why)
		return errArmOutOfScope
	}
	return nil
}

// attachedProjectFor is a project the actor can act in that has the pull
// request's repo (0 if none: the audit entry is then not about a project).
func (s *Service) attachedProjectFor(ctx context.Context, actor *Actor, pr *PullRequest) (int64, error) {
	if actor.User.ID == 0 {
		return s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	}
	return s.prAuthority(ctx, actor, pr, RoleViewer)
}

// FindPullRequest finds a pull request by number among a project's repos
// (repo narrows it when several repos have that number).
func (s *Service) FindPullRequest(ctx context.Context, actor *Actor, projectRef, repo string, number int) (int64, error) {
	if actor == nil || actor.User == nil {
		return 0, ErrUnauthorized
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return 0, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return 0, err
	}
	vis, args := prVisible(actor, p.ID)
	q := `SELECT p.id, p.repo FROM pull_requests p WHERE p.number = ? AND ` + vis
	args = append([]any{number}, args...)
	if repo != "" {
		q += ` AND p.repo = ? COLLATE NOCASE`
		args = append(args, repo)
	}
	// #nosec G202 -- fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, q+` ORDER BY p.id`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	var repos []string
	for rows.Next() {
		var id int64
		var r string
		if err := rows.Scan(&id, &r); err != nil {
			return 0, err
		}
		ids, repos = append(ids, id), append(repos, r)
	}
	switch {
	case len(ids) == 0:
		return 0, notFound(fmt.Sprintf("pull request #%d", number))
	case len(ids) > 1:
		return 0, invalid("#%d is in several repos (%s): say which with --repo", number, strings.Join(repos, ", "))
	}
	return ids[0], rows.Err()
}

// CheckPullRequest reads a pull request's threads, checks and gate from the
// forge now (what "@waypoint check" does) and returns where it stands.
func (s *Service) CheckPullRequest(ctx context.Context, actor *Actor, prID int64) (*PRDetail, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleEditor); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	if f, err := s.forgeByID(bg, pr.ForgeID); err == nil && f.token != "" {
		if prc, ok := s.Forges.(ForgePRs); ok {
			if fp, err := prc.GetPR(bg, f, pr.Repo, pr.Number); err == nil {
				if _, err := s.recordPR(bg, f, pr.Repo, fp, false); err != nil {
					if readFault(err) {
						s.log.WarnContext(bg, "record a pull request after reading it from the forge", "repo", pr.Repo, "number", pr.Number, "err", err)
					}
				}
			}
		}
	}
	s.refreshPRChecks(bg, pr.ID)
	s.checkPR(bg, pr.ID, "asked")
	return s.GetPullRequest(ctx, actor, prID)
}

// holdVaryRE: the parts of a reason that change while it holds the same
// thing back: a head, a time, a count.
var holdVaryRE = lazyre.New(`\s+on [0-9a-f]{7,40}\b|\s+until \d{1,2}:\d{2}(\s+[A-Z]{2,5}\b)?|\b\d+\b`)

// holdKind is what a reason holds a merge for, the same from one check to
// the next: "waiting for scanner/codeql on 1a2b3c4 until 21:05 UTC" is
// "waiting for scanner/codeql".
func holdKind(reason string) string {
	k := holdVaryRE.ReplaceAllStringFunc(reason, func(m string) string {
		if m[0] >= '0' && m[0] <= '9' {
			return "N"
		}
		return ""
	})
	return clipLine(k, 120)
}

// recordHolds keeps what holds an armed pull request back, and since when
// (merge_holds, WP-491): a reason opens a hold, and the first check without
// it, or on another head, ends it. Bookkeeping for wp ci doctor, written
// only when the reasons change. maybeAutoMerge runs from several goroutines
// with no lock per pull request, so the changes are worked out again inside
// the write transaction (one writer): two checks can't open the same hold
// twice. The read before it only spares the writer when nothing changed.
func (s *Service) recordHolds(ctx context.Context, pr *PullRequest, reasons []string) {
	want := map[string]bool{}
	said := map[string]string{}
	for _, r := range reasons {
		k := holdKind(r)
		want[k] = true
		if _, ok := said[k]; !ok {
			said[k] = r
		}
	}
	start, ended, err := holdChanges(ctx, s.db.R, pr, want)
	if err != nil {
		s.log.WarnContext(ctx, "read merge holds", "pr", pr.ID, "err", err)
		return
	}
	if len(start) == 0 && len(ended) == 0 {
		return
	}
	now := s.nowMS()
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		start, ended, err = holdChanges(ctx, t, pr, want)
		if err != nil {
			return err
		}
		for _, h := range ended {
			if _, err := t.ExecContext(ctx, `UPDATE merge_holds SET cleared_at = ? WHERE id = ?`, now, h.id); err != nil {
				return err
			}
		}
		for _, what := range start {
			if _, err := t.ExecContext(ctx, `INSERT INTO merge_holds(pr_id, head_sha, what, first_at) VALUES (?,?,?,?)`, pr.ID, pr.HeadSHA, what, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record merge holds", "pr", pr.ID, "err", err)
		return
	}
	ref := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
	for _, h := range ended {
		s.log.Info("merge hold cleared", "pr", ref, "what", h.what, "held", time.Duration(now-h.firstAt)*time.Millisecond)
	}
	for _, what := range start {
		s.log.Info("merge hold", "pr", ref, "head", short(pr.HeadSHA), "why", said[what])
	}
}

// openHold is a merge hold not yet cleared.
type openHold struct {
	id      int64
	what    string
	firstAt int64
}

// holdChanges reads a pull request's open holds and says which kinds to
// open (wanted, not open on its head) and which holds to end (on another
// head, or no longer wanted).
func holdChanges(ctx context.Context, q queryer, pr *PullRequest, want map[string]bool) (start []string, ended []openHold, err error) {
	rows, err := q.QueryContext(ctx, `SELECT id, head_sha, what, first_at FROM merge_holds WHERE pr_id = ? AND cleared_at IS NULL`, pr.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	open := map[string]bool{}
	for rows.Next() {
		var h openHold
		var sha string
		if err := rows.Scan(&h.id, &sha, &h.what, &h.firstAt); err != nil {
			return nil, nil, err
		}
		if sha == pr.HeadSHA && want[h.what] && !open[h.what] {
			open[h.what] = true
		} else {
			ended = append(ended, h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for what := range want {
		if !open[what] {
			start = append(start, what)
		}
	}
	slices.Sort(start)
	return start, ended, nil
}
