package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// mergeSettingAgent marks a pull request armed by the repo's Merge
// automatically setting, in the armed_agent column.
const mergeSettingAgent = "setting/merge-automatically"

// mergeSettingName is how the audit log names the setting as an actor.
const mergeSettingName = "Merge automatically (repo setting)"

// Merge policy limits and defaults.
const (
	defaultReviewWait = 30
	defaultQuiet      = 10
	maxPolicyMinutes  = 24 * 60
	maxPolicyEntries  = 20
)

// noAutoArmLabels keep Merge automatically off a pull request.
var noAutoArmLabels = []string{"do-not-merge", "wip"}

// releaseTitleRE: "release 1.2", "chore: release 1.2", "chore(main): release 1.2".
var releaseTitleRE = lazyre.New(`(?i)^(chore(\(.*\))?: )?release\b`)

// MergePolicy is a repo's Merge automatically settings.
type MergePolicy struct {
	AutoMerge bool `json:"auto_merge"`
	// AutoMergeInto is which base branches Merge automatically arms pull
	// requests into (WP-1041): IntoAny, IntoDefault, IntoOthers, or
	// IntoMatch (the AutoMergeBases globs).
	AutoMergeInto  string   `json:"auto_merge_into"`
	AutoMergeBases []string `json:"auto_merge_bases"`
	// RequiredChecks must have passed on the head, on top of every check
	// passing (status contexts or check names).
	RequiredChecks    []string `json:"required_checks"`
	ReviewWaitMinutes int      `json:"review_wait_minutes"`
	QuietMinutes      int      `json:"quiet_minutes"`
	// ExternalReviewers are bots outside Waypoint whose review is expected
	// on each head: forge logins, or status contexts (with a "/").
	ExternalReviewers []string `json:"external_reviewers"`
	into              []baseScope
}

// Which base branches Merge automatically arms pull requests into.
const (
	IntoAny     = "any"
	IntoDefault = "default"
	IntoOthers  = "others"
	IntoMatch   = "match"
)

// baseScope is one project's base branch scope.
type baseScope struct {
	into  string
	bases []string
}

func (p MergePolicy) scopes() []baseScope {
	if len(p.into) > 0 {
		return p.into
	}
	return []baseScope{{p.AutoMergeInto, p.AutoMergeBases}}
}

// needsDefault says whether telling a base in scope takes the repo's
// default branch.
func (p MergePolicy) needsDefault() bool {
	return slices.ContainsFunc(p.scopes(), func(sc baseScope) bool { return sc.into == IntoDefault || sc.into == IntoOthers })
}

// baseSkip says why the policy's base scope leaves base out, or "". def is
// the repo's default branch, known whenever needsDefault.
func (p MergePolicy) baseSkip(base, def string) string {
	for _, sc := range p.scopes() {
		switch sc.into {
		case IntoDefault:
			if base != def {
				return "into " + cmp(base, "an unknown branch") + ", not the default branch (" + def + ")"
			}
		case IntoOthers:
			if base == "" {
				return "into an unknown branch"
			}
			if base == def {
				return "into the default branch (" + def + ")"
			}
		case IntoMatch:
			if gs, err := newGlobSet(sc.bases); err != nil || base == "" || !gs.match(base) {
				return "into " + cmp(base, "an unknown branch") + ", which matches none of " + cmp(strings.Join(sc.bases, ", "), "no patterns")
			}
		}
	}
	return ""
}

// scopeText says which base branches the policy arms into, "" for any:
// "into release/**", "into branches other than main".
func (p MergePolicy) scopeText(def string) string {
	var parts []string
	for _, sc := range p.scopes() {
		switch sc.into {
		case IntoDefault:
			parts = append(parts, "into "+cmp(def, "the default branch"))
		case IntoOthers:
			parts = append(parts, "into branches other than "+cmp(def, "the default"))
		case IntoMatch:
			parts = append(parts, "into "+cmp(strings.Join(sc.bases, " or "), "no branch"))
		}
	}
	return strings.Join(parts, ", and ")
}

// autoMergeBaseSkip says why the repo's Merge automatically scope leaves
// pr's base out, or "". known is false when that depends on the repo's
// default branch and the forge can't say which it is. fresh asks the forge
// rather than remembering: a merge is about to happen, and the default
// branch may have been renamed.
func (s *Service) autoMergeBaseSkip(ctx context.Context, pr *PullRequest, pol MergePolicy, fresh bool) (why string, known bool) {
	def := ""
	if pol.needsDefault() {
		d, ok := s.repoDefaultBranch(ctx, pr.ForgeID, pr.Repo, fresh)
		if !ok {
			return "", false
		}
		def = d
	}
	return pol.baseSkip(pr.BaseRef, def), true
}

// defaultBranchKnown is pr's repo's default branch if it is remembered,
// else "" (no forge call: for wording).
func (s *Service) defaultBranchKnown(pr *PullRequest) string {
	if v, ok := s.defaultBranches.Load(fmt.Sprintf("%d/%s", pr.ForgeID, strings.ToLower(pr.Repo))); ok {
		return v.(defaultBranchEntry).name
	}
	return ""
}

// defaultBranchTTL is how long a repo's default branch is remembered.
const defaultBranchTTL = time.Hour

type defaultBranchEntry struct {
	name string
	at   time.Time
}

// repoDefaultBranch is a repo's default branch as the forge says,
// remembered for an hour unless fresh; ok is false when the forge can't say.
func (s *Service) repoDefaultBranch(ctx context.Context, forgeID int64, repo string, fresh bool) (string, bool) {
	key := fmt.Sprintf("%d/%s", forgeID, strings.ToLower(repo))
	if v, ok := s.defaultBranches.Load(key); ok && !fresh {
		if e := v.(defaultBranchEntry); s.now().Sub(e.at) < defaultBranchTTL {
			return e.name, true
		}
	}
	dc, ok := s.Forges.(branchDefaulter)
	if !ok {
		return "", false
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return "", false
	}
	d, err := dc.DefaultBranch(ctx, f, repo)
	if err != nil || d == "" {
		return "", false
	}
	s.defaultBranches.Store(key, defaultBranchEntry{d, s.now()})
	return d, true
}

// readPolicy decodes a project_repos row's policy columns. A repo that
// names no external reviewers (NULL) expects none, on every forge kind
// (WP-1185).
func readPolicy(auto bool, into, bases, required string, wait, quiet int, reviewers sql.NullString) MergePolicy {
	p := MergePolicy{AutoMerge: auto, AutoMergeInto: cmp(into, IntoAny), ReviewWaitMinutes: wait, QuietMinutes: quiet, RequiredChecks: []string{}}
	_ = json.Unmarshal([]byte(required), &p.RequiredChecks)
	_ = json.Unmarshal([]byte(bases), &p.AutoMergeBases)
	if p.AutoMergeBases == nil {
		p.AutoMergeBases = []string{}
	}
	if reviewers.Valid {
		_ = json.Unmarshal([]byte(reviewers.String), &p.ExternalReviewers)
	}
	if p.RequiredChecks == nil {
		p.RequiredChecks = []string{}
	}
	if p.ExternalReviewers == nil {
		p.ExternalReviewers = []string{}
	}
	return p
}

// repoMergePolicy is the policy for a repo's pull requests, the strictest
// of the projects that attach it; ok is false when none does.
func (s *Service) repoMergePolicy(ctx context.Context, forgeID int64, repo string) (pol MergePolicy, ok bool, err error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.auto_merge, r.auto_merge_into, r.auto_merge_bases, r.required_checks, r.review_wait_minutes, r.quiet_minutes, r.external_reviewers
		FROM project_repos r WHERE r.forge_id = ? AND r.repo = ? COLLATE NOCASE ORDER BY r.id`, forgeID, repo)
	if err != nil {
		return pol, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var auto bool
		var into, bases, required string
		var wait, quiet int
		var reviewers sql.NullString
		if err := rows.Scan(&auto, &into, &bases, &required, &wait, &quiet, &reviewers); err != nil {
			return pol, false, err
		}
		p := readPolicy(auto, into, bases, required, wait, quiet, reviewers)
		if !ok {
			pol, ok = p, true
			pol.into = p.scopes()
			continue
		}
		pol.AutoMerge = pol.AutoMerge && p.AutoMerge
		pol.into = append(pol.into, p.scopes()...)
		pol.RequiredChecks = unionFold(pol.RequiredChecks, p.RequiredChecks)
		pol.ExternalReviewers = unionFold(pol.ExternalReviewers, p.ExternalReviewers)
		pol.ReviewWaitMinutes = max(pol.ReviewWaitMinutes, p.ReviewWaitMinutes)
		pol.QuietMinutes = max(pol.QuietMinutes, p.QuietMinutes)
	}
	return pol, ok, rows.Err()
}

// unionFold adds b's entries a lacks (ignoring case).
func unionFold(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.ContainsFunc(out, func(y string) bool { return strings.EqualFold(x, y) }) {
			out = append(out, x)
		}
	}
	return out
}

// cleanPolicyList trims, drops empty and repeated entries (ignoring case
// unless exact: branch names have case) and checks the size.
func cleanPolicyList(list []string, what string, exact bool) ([]string, error) {
	out := []string{}
	for _, x := range list {
		x = strings.TrimSpace(x)
		if x == "" || slices.ContainsFunc(out, func(y string) bool { return x == y || !exact && strings.EqualFold(x, y) }) {
			continue
		}
		if len(x) > 200 {
			return nil, invalid("%s: %q is too long", what, excerpt(x, 40))
		}
		out = append(out, x)
	}
	if len(out) > maxPolicyEntries {
		return nil, invalid("at most %d %s", maxPolicyEntries, what)
	}
	return out, nil
}

// MergePolicyInput changes a repo's merge policy; nil leaves a setting alone.
type MergePolicyInput struct {
	AutoMerge         *bool
	AutoMergeInto     *string
	AutoMergeBases    *[]string
	RequiredChecks    *[]string
	ReviewWaitMinutes *int
	QuietMinutes      *int
	ExternalReviewers *[]string
	// LineCandidates turns the merge line's candidate branches on or off.
	LineCandidates *bool
}

// SetRepoMergePolicy changes an attached repo's merge policy (owners). Its
// open pull requests are armed, or the setting's own arming taken off, to
// match.
func (s *Service) SetRepoMergePolicy(ctx context.Context, actor *Actor, projectRef string, id int64, in MergePolicyInput) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	var sets, changes []string
	var args []any
	minutes := func(v *int, col, what string) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > maxPolicyMinutes {
			return invalid("%s must be 0 to %d minutes", what, maxPolicyMinutes)
		}
		sets, args = append(sets, col+" = ?"), append(args, *v)
		changes = append(changes, fmt.Sprintf("%s %d min", what, *v))
		return nil
	}
	list := func(v *[]string, col, what string) error {
		if v == nil {
			return nil
		}
		l, err := cleanPolicyList(*v, what, col == "auto_merge_bases")
		if err != nil {
			return err
		}
		b, _ := json.Marshal(l)
		sets, args = append(sets, col+" = ?"), append(args, string(b))
		changes = append(changes, what+": "+cmp(strings.Join(l, ", "), "none"))
		return nil
	}
	if in.LineCandidates != nil {
		sets, args = append(sets, "line_candidates = ?"), append(args, boolInt(*in.LineCandidates))
		changes = append(changes, map[bool]string{true: "turned on", false: "turned off"}[*in.LineCandidates]+" merge line candidates")
	}
	scopeAt := -1
	if in.AutoMerge != nil {
		sets, args = append(sets, "auto_merge = ?"), append(args, boolInt(*in.AutoMerge))
		changes = append(changes, map[bool]string{true: "turned on", false: "turned off"}[*in.AutoMerge]+" Merge automatically")
	}
	if in.AutoMergeInto != nil {
		if !slices.Contains([]string{IntoAny, IntoDefault, IntoOthers, IntoMatch}, *in.AutoMergeInto) {
			return invalid("auto_merge_into must be any, default, others or match")
		}
		sets, args = append(sets, "auto_merge_into = ?"), append(args, *in.AutoMergeInto)
		scopeAt = len(changes)
		changes = append(changes, "")
	}
	if in.AutoMergeBases != nil {
		if _, err := newGlobSet(*in.AutoMergeBases); err != nil {
			return invalid("branch pattern: %v", err)
		}
	}
	for _, err := range []error{
		list(in.AutoMergeBases, "auto_merge_bases", "branch patterns"),
		list(in.RequiredChecks, "required_checks", "required checks"),
		minutes(in.ReviewWaitMinutes, "review_wait_minutes", "review wait"),
		minutes(in.QuietMinutes, "quiet_minutes", "quiet"),
		list(in.ExternalReviewers, "external_reviewers", "external reviewers"),
	} {
		if err != nil {
			return err
		}
	}
	if scopeAt >= 0 {
		var bases []string
		if in.AutoMergeBases != nil {
			bases, _ = cleanPolicyList(*in.AutoMergeBases, "branch patterns", true)
		}
		scope := MergePolicy{AutoMergeInto: *in.AutoMergeInto, AutoMergeBases: bases}.scopeText("")
		if *in.AutoMergeInto == IntoMatch && in.AutoMergeBases == nil {
			scope = "into the branch patterns already set"
		} else if scope == "" && *in.AutoMergeInto != IntoAny {
			scope = "into " + *in.AutoMergeInto
		}
		changes[scopeAt] = "Merge automatically arms " + cmp(scope, "into any branch")
	}
	if len(sets) == 0 {
		return nil
	}
	var forgeID int64
	var repo string
	err = s.write(ctx, actor, func(t *tx) error {
		var wasOn bool
		if err := t.QueryRowContext(ctx, `SELECT forge_id, repo, auto_merge FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).Scan(&forgeID, &repo, &wasOn); err != nil {
			return notFound("project repo")
		}
		// #nosec G202 -- only fixed column names and ? placeholders are joined in
		if _, err := t.ExecContext(ctx, `UPDATE project_repos SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...); err != nil {
			return err
		}
		nowOn := wasOn
		if in.AutoMerge != nil {
			nowOn = *in.AutoMerge
		}
		if nowOn && (!wasOn || in.AutoMergeInto != nil || in.AutoMergeBases != nil) {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET arm_declined = 0 WHERE arm_declined = 2 AND forge_id = ? AND repo = ? COLLATE NOCASE`, forgeID, repo); err != nil {
				return err
			}
		}
		t.audit(AuditEntry{Action: "repo.merge_policy", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id, Target: repo,
			Detail: "merge policy of " + repo + ": " + strings.Join(changes, ", ")})
		return nil
	})
	if err != nil {
		return err
	}
	s.goBG(func() { s.applyMergePolicy(context.WithoutCancel(ctx), forgeID, repo) })
	return nil
}

// applyMergePolicy brings a repo's open pull requests in line with its
// policy after it changed, and merges the armed ones that are now ready.
func (s *Service) applyMergePolicy(ctx context.Context, forgeID int64, repo string) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND state IN ('open','draft') ORDER BY id`, forgeID, repo)
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
		if s.autoArm(ctx, id) {
			s.maybeAutoMerge(ctx, id)
		}
	}
}

// autoArmSkip says why Merge automatically leaves pr alone, or "", its
// base branch aside (autoMergeBaseSkip).
func autoArmSkip(pr *PullRequest, pol MergePolicy) string {
	switch {
	case !pol.AutoMerge:
		return "Merge automatically is off"
	case pr.State != "open":
		return "a " + pr.State + " pull request"
	case pr.headRepo == "":
		return "its source repository isn't known (it may be a fork)"
	case !strings.EqualFold(pr.headRepo, pr.Repo):
		return "from a fork (" + pr.headRepo + ")"
	case releaseTitleRE.MatchString(strings.TrimSpace(pr.Title)),
		strings.HasPrefix(strings.ToLower(pr.HeadRef), "release-please--"):
		return "a release pull request"
	}
	for _, l := range pr.Labels {
		if slices.ContainsFunc(noAutoArmLabels, func(x string) bool { return strings.EqualFold(x, l) }) {
			return "labelled " + l
		}
	}
	return ""
}

// autoArm applies the repo's Merge automatically setting to a pull
// request: it arms an eligible one nobody has disarmed, and takes the
// setting's own arming off one that is no longer eligible (the setting was
// turned off, a do-not-merge label came). An agent's arming comes off when
// the base is outside the setting's branches (narrowed, or the pull
// request retargeted, WP-1041); arming by a person, a label or an
// automation is never touched. It reports whether the pull request is
// armed now.
func (s *Service) autoArm(ctx context.Context, prID int64) bool {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || (pr.State != "open" && pr.State != "draft") {
		return false
	}
	pol, ok, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil || !ok {
		return pr.ArmedAt != nil
	}
	why := autoArmSkip(pr, pol)
	baseWhy, known := "", true
	if pol.AutoMerge {
		baseWhy, known = s.autoMergeBaseSkip(ctx, pr, pol, false)
	}
	mine := pr.ArmedAt != nil && pr.ArmedAgent == mergeSettingAgent
	switch {
	case mine && why != "" && pr.State == "open":
		s.disarmOutOfScope(ctx, pr, why)
		return false
	case scopedArm(pr) && baseWhy != "":
		s.disarmOutOfScope(ctx, pr, baseWhy)
		return false
	case !known:
		return pr.ArmedAt != nil
	case pr.ArmedAt == nil && why == "" && baseWhy == "" && pr.armDeclined == 0:
		return s.armBySetting(ctx, pr)
	}
	return pr.ArmedAt != nil
}

func (s *Service) armBySetting(ctx context.Context, pr *PullRequest) bool {
	armed := false
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = NULL, armed_agent = ?, armed_style = '', armed_note = ''
			WHERE id = ? AND armed_at IS NULL AND arm_declined = 0 AND state = 'open'`, s.nowMS(), mergeSettingAgent, pr.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		armed = true
		t.audit(AuditEntry{Action: "pr.arm", Category: AuditApproval, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "set “" + pr.Title + "” to merge when ready", About: pr.Agent, Actor: mergeSettingName})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "armed", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "merge automatically", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	return armed
}

// scopedArm says whether pr's arming answers to Merge automatically's base
// branches (WP-1041): the setting's own, or an agent's (wp ship). A
// person's, a label's or an automation's stands wherever it points.
func scopedArm(pr *PullRequest) bool {
	return pr.ArmedAt != nil && pr.ArmedAgent != "" && automationOf(pr.ArmedAgent) == ""
}

// disarmOutOfScope takes the setting's or an agent's arming off pr, as
// Merge automatically: it no longer covers it (why).
func (s *Service) disarmOutOfScope(ctx context.Context, pr *PullRequest, why string) {
	if !scopedArm(pr) {
		return
	}
	if pr.ArmedAgent != mergeSettingAgent {
		why += ", which merges by hand: " + pr.ArmedAgent + " armed it"
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = NULL, armed_by = NULL, armed_agent = '', armed_style = '', armed_note = '', merge_wake_at = NULL
			WHERE id = ? AND armed_agent = ?`, pr.ID, pr.ArmedAgent)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.disarm", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "took merge when ready off “" + pr.Title + "”: " + why, Actor: mergeSettingName})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "disarmed", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "merge automatically", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// requiredChecksReasons says which of the policy's required checks haven't
// passed on the head.
func requiredChecksReasons(pol MergePolicy, checks []PRCheck, sha string) []string {
	var out []string
	for _, name := range pol.RequiredChecks {
		i := slices.IndexFunc(checks, func(c PRCheck) bool { return strings.EqualFold(c.Name, name) })
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("required check %s hasn't reported on %s", name, short(sha)))
		case checks[i].State != CheckSuccess:
			out = append(out, fmt.Sprintf("required check %s is %s", name, checks[i].State))
		}
	}
	return out
}

// settleWaits is what an armed merge waits for once nothing else stands in
// the way: reviews settled (every expected reviewer posted on this head, or
// the review wait since the head was first seen passed) and quiet (nothing
// posted for the quiet period). wake is the earliest time one of the waits
// ends, nil when it waits for none.
func (s *Service) settleWaits(ctx context.Context, pr *PullRequest, pol MergePolicy, checks []PRCheck) (reasons []string, wake *time.Time, err error) {
	now := s.now()
	until := func(t time.Time) {
		if wake == nil || t.Before(*wake) {
			wake = &t
		}
	}
	if pol.ReviewWaitMinutes > 0 {
		since := pr.UpdatedAt
		if pr.headAt != nil {
			since = *pr.headAt
		}
		deadline := since.Add(time.Duration(pol.ReviewWaitMinutes) * time.Minute)
		if now.Before(deadline) {
			missing, err := s.reviewersMissing(ctx, pr, pol, checks, since)
			if err != nil {
				return nil, nil, err
			}
			if len(missing) > 0 {
				reasons = append(reasons, fmt.Sprintf("waiting for %s on %s until %s", joinAnd(missing), short(pr.HeadSHA), s.clock(deadline)))
				until(deadline)
			}
		}
	}
	if pol.QuietMinutes > 0 {
		last, what, err := s.lastPosted(ctx, pr)
		if err != nil {
			return nil, nil, err
		}
		if !last.IsZero() {
			end := last.Add(time.Duration(pol.QuietMinutes) * time.Minute)
			if now.Before(end) {
				reasons = append(reasons, fmt.Sprintf("quiet until %s (last: %s)", s.clock(end), what))
				until(end)
			}
		}
	}
	return reasons, wake, nil
}

// joinAnd lists names: "a", "a and b", "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// reviewersMissing names the reviewers expected on pr's head that haven't
// posted on it: Waypoint's review rules that act on it (a run for this head
// that finished, failed or skipped counts), and the policy's external
// reviewers (a review or comment on this head, or one since the head was
// first seen; for a status context, any status on the head).
func (s *Service) reviewersMissing(ctx context.Context, pr *PullRequest, pol MergePolicy, checks []PRCheck, since time.Time) ([]string, error) {
	var missing []string
	rules, err := s.reviewRulesFor(ctx, pr)
	if err != nil {
		return nil, err
	}
	for _, a := range rules {
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM bot_runs WHERE pr_id = ? AND automation_id = ? AND kind = 'review' AND head_sha = ? AND status NOT IN ('queued','running')`,
			pr.ID, a.ID, pr.HeadSHA).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			missing = append(missing, "Waypoint's “"+a.Name+"”")
		}
	}
	for _, who := range pol.ExternalReviewers {
		if strings.Contains(who, "/") {
			if !slices.ContainsFunc(checks, func(c PRCheck) bool { return strings.EqualFold(c.Name, who) }) {
				missing = append(missing, who)
			}
			continue
		}
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pr_events WHERE pr_id = ? AND actor = ? COLLATE NOCASE
			AND kind IN ('commented','reviewed','approved','changes_requested') AND (sha = ? OR (sha = '' AND created_at >= ?))`,
			pr.ID, who, pr.HeadSHA, since.UnixMilli()).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			missing = append(missing, who)
		}
	}
	return missing, nil
}

// reviewRulesFor are the enabled automations that review pr when it is
// pushed to: a pull request trigger with a review step whose filter takes
// it, in a project with bots on for its repo (or Admin → Automations).
func (s *Service) reviewRulesFor(ctx context.Context, pr *PullRequest) ([]*Automation, error) {
	projects, err := s.botProjects(ctx, pr.ForgeID, pr.Repo)
	if err != nil || len(projects) == 0 {
		return nil, err
	}
	var all []*Automation
	for _, pid := range projects {
		l, err := s.automations(ctx, s.db.R, pid, true)
		if err != nil {
			return nil, err
		}
		all = append(all, l...)
	}
	g, err := s.adminAutomations(ctx, true)
	if err != nil {
		return nil, err
	}
	all = append(all, g...)
	var out []*Automation
	for _, a := range all {
		if d := triggerDef(a.Rule.Trigger.Event); d == nil || d.Event != EvPRUpdated ||
			!hasStep(a.Rule.Actions, "review") || a.Rule.PR.skip(pr, EvPRUpdated) != "" ||
			slices.ContainsFunc(out, func(x *Automation) bool { return x.ID == a.ID }) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// hasStep says whether steps (or a group in them) include a step of type typ.
func hasStep(steps []Action, typ string) bool {
	return slices.ContainsFunc(steps, func(a Action) bool { return a.Type == typ || hasStep(a.Steps, typ) })
}

// lastPosted is when something was last posted on pr, and what: a review,
// a review comment or a comment by anyone but Waypoint (pushes don't count),
// or a review or reply of Waypoint's review bot. A babysitting round's own
// replies and acks, and Waypoint's notes, don't count: the quiet period
// waits for reviewers to finish, and a round that pushed nothing would
// otherwise wait it out again for its own words (WP-455). Zero when nothing
// was.
func (s *Service) lastPosted(ctx context.Context, pr *PullRequest) (time.Time, string, error) {
	var kind, actor, ext string
	var at int64
	err := s.db.R.QueryRowContext(ctx, `SELECT kind, actor, ext_id, created_at FROM pr_events WHERE pr_id = ? AND kind IN ('commented','reviewed','approved','changes_requested') AND own = 0
		ORDER BY created_at DESC, id DESC LIMIT 1`, pr.ID).Scan(&kind, &actor, &ext, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", err
	}
	what := ""
	if at > 0 {
		who := cmp(actor, "someone")
		switch {
		case kind != "commented":
			what = "review by " + who
		case strings.HasPrefix(ext, "rc"):
			what = "review comment by " + who
		default:
			what = "comment by " + who
		}
	}
	var runKind string
	var runAt int64
	err = s.db.R.QueryRowContext(ctx, `SELECT kind, finished_at FROM bot_runs WHERE pr_id = ? AND kind IN ('review','reply') AND status = 'done' AND finished_at IS NOT NULL
		ORDER BY finished_at DESC LIMIT 1`, pr.ID).Scan(&runKind, &runAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", err
	}
	if runAt > at {
		at, what = runAt, "Waypoint's "+runKind
	}
	if at == 0 {
		return time.Time{}, "", nil
	}
	return msTime(at), what, nil
}

// UndealtComment is a review comment on a pull request nobody has dealt
// with: not accepted and fixed, rejected with a reply, or dismissed.
type UndealtComment struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	URL    string `json:"url,omitempty"`
}

// setMergeWake records when an armed pull request's wait ends, so
// CheckArmedMerges looks at it again then (nil: it waits for no time).
func (s *Service) setMergeWake(ctx context.Context, prID int64, wake *time.Time) {
	var at any
	if wake != nil {
		at = wake.UnixMilli()
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET merge_wake_at = ? WHERE id = ? AND merge_wake_at IS NOT ?`, at, prID, at); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// CheckArmedMerges looks again at armed pull requests whose wait for
// reviews or quiet has ended: nothing else happens at that moment.
func (s *Service) CheckArmedMerges(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE merge_wake_at IS NOT NULL AND merge_wake_at <= ? AND armed_at IS NOT NULL
		AND state IN ('open','draft') ORDER BY merge_wake_at LIMIT 100`, s.nowMS())
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
		if ctx.Err() != nil {
			return
		}
		s.setMergeWake(ctx, id, nil)
		s.maybeAutoMerge(ctx, id)
	}
}
