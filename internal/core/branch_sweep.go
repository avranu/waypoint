package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Verdicts of a remote branch.
const (
	BranchRemovable = "removable"
	BranchKeep      = "keep"
	BranchProtected = "protected"
	BranchStale     = "stale"
)

const (
	branchQuiet      = 2 * time.Hour
	branchStaleAfter = 30 * 24 * time.Hour
	branchSweepEvery = 23 * time.Hour
	tidyCacheFor     = 5 * time.Minute
	maxBranchLookups = 200
)

// RemoteBranch is a branch on the forge with the sweep's verdict.
type RemoteBranch struct {
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Action  string `json:"action,omitempty"` // after a sweep: deleted, or why not
}

// RemoteBranches is a repo's branches as the sweep sees them.
type RemoteBranches struct {
	Repo    string         `json:"repo"`
	Forge   string         `json:"forge"`
	Default string         `json:"default_branch"`
	Enabled bool           `json:"enabled"` // the repo's setting: the sweep deletes the removable ones
	Rows    []RemoteBranch `json:"rows"`
	Counts  map[string]int `json:"counts"`
	Notes   []string       `json:"notes,omitempty"`
}

type tidyCached struct {
	at   time.Time
	plan *RemoteBranches
}

// deletesMergedBranches says whether the repo's setting lets the sweep
// delete branches. A repo attached to several projects needs them all to
// agree: one turning it off keeps its branches.
func (s *Service) deletesMergedBranches(ctx context.Context, forgeID int64, repo string) bool {
	var on, off int
	err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(delete_merged_branches), 0), coalesce(sum(1 - delete_merged_branches), 0) FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE`,
		forgeID, repo).Scan(&on, &off)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read deletes merged branches", "forgeID", forgeID, "repo", repo, "err", err)
		}
		return false
	}
	return on > 0 && off == 0
}

// planBranches judges each branch of a repo on the forge.
func (s *Service) planBranches(ctx context.Context, f *Forge, repo string) (*RemoteBranches, error) {
	cl, ok := s.Forges.(ForgeBranches)
	dc, ok2 := s.Forges.(ForgeCommitter)
	if !ok || !ok2 {
		return nil, invalid("this forge client can't list branches")
	}
	if f.token == "" {
		return nil, invalid("the forge has no API token, so Waypoint can't read its branches")
	}
	def, err := dc.DefaultBranch(ctx, f, repo)
	if err != nil {
		return nil, fmt.Errorf("read the default branch: %w", err)
	}
	branches, err := cl.Branches(ctx, f, repo)
	if err != nil {
		return nil, fmt.Errorf("list the branches: %w", err)
	}
	now := s.now()
	plan := &RemoteBranches{Repo: repo, Forge: f.Kind, Default: def, Enabled: s.deletesMergedBranches(ctx, f.ID, repo), Counts: map[string]int{}}

	holds := map[string]string{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT number, head_ref, head_repo, base_ref FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND state IN ('open','draft')`, f.ID, repo)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n int
		var head, headRepo, base string
		if rows.Scan(&n, &head, &headRepo, &base) != nil {
			continue
		}
		if headRepo == "" || strings.EqualFold(headRepo, repo) {
			holds[head] = fmt.Sprintf("the head of open pull request #%d", n)
		}
		if _, ok := holds[base]; !ok {
			holds[base] = fmt.Sprintf("the base of open pull request #%d", n)
		}
	}
	rows.Close()
	mergedHeads := map[string]int{}
	rows, err = s.db.R.QueryContext(ctx, `SELECT head_ref, head_sha, number FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND state = 'merged' AND head_sha != '' AND merged_at > ?
		ORDER BY merged_at`, f.ID, repo, s.nowMS()-366*24*3600*1000)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, sha string
		var n int
		if rows.Scan(&ref, &sha, &n) == nil {
			mergedHeads[ref+"@"+strings.ToLower(sha)] = n
		}
	}
	rows.Close()
	pushed := map[string]time.Time{}
	rows, err = s.db.R.QueryContext(ctx, `SELECT branch, pushed_at FROM branch_activity WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, f.ID, repo)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b string
		var at int64
		if rows.Scan(&b, &at) == nil {
			pushed[b] = msTime(at)
		}
	}
	rows.Close()

	defTip := ""
	for _, b := range branches {
		if b.Name == def {
			defTip = b.SHA
		}
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	lookups := 0
	idle := func(b ForgeBranch) (time.Duration, bool) {
		newest := time.Time{}
		if at, ok := pushed[b.Name]; ok {
			newest = at
		}
		if lookups < maxBranchLookups {
			lookups++
			if t, err := cl.CommitTime(ctx, f, repo, b.SHA); err == nil && t.After(newest) {
				newest = t
			}
		}
		if newest.IsZero() {
			return 0, false
		}
		return now.Sub(newest), true
	}
	for _, b := range branches {
		row := RemoteBranch{Branch: b.Name, SHA: b.SHA}
		switch {
		case b.Name == def:
			row.Verdict, row.Reason = BranchProtected, "the default branch"
		case b.Protected:
			row.Verdict, row.Reason = BranchProtected, "protected on the forge"
		case strings.HasPrefix(b.Name, "release/"):
			row.Verdict, row.Reason = BranchProtected, "a release branch"
		case holds[b.Name] != "":
			row.Verdict, row.Reason = BranchKeep, holds[b.Name]
		default:
			by := ""
			if n, ok := mergedHeads[b.Name+"@"+strings.ToLower(b.SHA)]; ok {
				by = fmt.Sprintf("pull request #%d merged this very commit", n)
			} else if defTip != "" && (b.SHA == defTip || func() bool {
				n, err := cl.Ahead(ctx, f, repo, def, b.SHA)
				if err != nil {
					if readFault(err) && !isStatus(err, http.StatusNotFound) && !errors.Is(err, ErrNotFound) {
						s.log.WarnContext(ctx, "read whether a branch is ahead of the default branch", "repo", repo, "branch", b.Name, "err", err)
					}
					return false
				}
				return n == 0
			}()) {
				by = "its tip is in " + def
			}
			age, known := idle(b)
			switch {
			case by != "" && !known && b.SHA == defTip:
				row.Verdict, row.Reason = BranchKeep, "same commit as "+def+" and no push seen: it may be new"
			case by != "" && known && age < branchQuiet:
				row.Verdict, row.Reason = BranchKeep, fmt.Sprintf("active: moved %d min ago", int(age.Minutes()))
			case by != "":
				row.Verdict, row.Reason = BranchRemovable, "merged ("+by+"), no open pull request"
			case known && age > branchStaleAfter:
				row.Verdict, row.Reason = BranchStale, fmt.Sprintf("unmerged, no pull request, no commit for %d days", int(age.Hours()/24))
			default:
				row.Verdict, row.Reason = BranchKeep, "unmerged"
			}
		}
		plan.Counts[row.Verdict]++
		plan.Rows = append(plan.Rows, row)
	}
	if !plan.Enabled {
		plan.Notes = append(plan.Notes, "deleting merged branches is off for this repo: nothing is deleted")
	}
	return plan, nil
}

// sweepRepoBranches deletes a repo's removable branches, when its setting
// allows.
func (s *Service) sweepRepoBranches(ctx context.Context, f *Forge, repo string) {
	cl, ok := s.Forges.(ForgeBranches)
	if !ok || f.token == "" || !s.deletesMergedBranches(ctx, f.ID, repo) {
		return
	}
	defer forgeLocks.lock(fmt.Sprintf("branches:%d:%s", f.ID, strings.ToLower(repo)))()
	plan, err := s.planBranches(ctx, f, repo)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			if s.repoMissing(ctx, f, repo) {
				return
			}
			s.markSwept(ctx, f.ID, repo)
			if _, seen := s.goneRepos.LoadOrStore(fmt.Sprintf("%d|%s", f.ID, strings.ToLower(repo)), true); !seen {
				s.log.WarnContext(ctx, "branch sweep skipped: the forge has no such repo", "repo", repo, "err", err)
			}
			return
		}
		s.log.WarnContext(ctx, "plan a branch sweep", "repo", repo, "err", err)
		return
	}
	s.markSwept(ctx, f.ID, repo)
	var doomed []RemoteBranch
	for _, r := range plan.Rows {
		if r.Verdict == BranchRemovable {
			doomed = append(doomed, r)
		}
	}
	if len(doomed) == 0 {
		return
	}
	tips := map[string]string{}
	now, err := cl.Branches(ctx, f, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "branch sweep: list the branches before deleting", "repo", repo, "err", err)
		}
		return
	}
	for _, b := range now {
		tips[b.Name] = b.SHA
	}
	projectID, err := s.attachedProject(ctx, s.db.R, f.ID, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project to delete merged branches", "repo", repo, "err", err)
		}
		return
	}
	for _, r := range doomed {
		if ctx.Err() != nil {
			return
		}
		if tips[r.Branch] != r.SHA {
			continue
		}
		if err := cl.DeleteBranch(ctx, f, repo, r.Branch); err != nil {
			s.log.WarnContext(ctx, "delete a merged branch", "repo", repo, "branch", r.Branch, "err", err)
			continue
		}
		if _, err := s.db.W.ExecContext(ctx, `DELETE FROM branch_activity WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND branch = ?`, f.ID, repo, r.Branch); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
		s.audit(ctx, SystemActor(), AuditEntry{Action: "branch.delete", Category: AuditControl, ProjectID: projectID, TargetType: "repo", Target: repo + ":" + r.Branch,
			Detail: "deleted the merged branch " + r.Branch + " on " + repo + ": " + r.Reason, Actor: "branch sweep"})
	}
	s.tidyCache.Delete(fmt.Sprintf("%d|%s", f.ID, strings.ToLower(repo)))
}

func (s *Service) markSwept(ctx context.Context, forgeID int64, repo string) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE project_repos SET branches_swept_at = ? WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, s.nowMS(), forgeID, repo); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// SweepBranches is the daily sweep: each attached repo whose setting is on
// and that wasn't swept lately.
func (s *Service) SweepBranches(ctx context.Context) {
	if _, ok := s.Forges.(ForgeBranches); !ok {
		jobSkip(ctx, "the forge client cannot list or delete branches")
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo FROM project_repos GROUP BY forge_id, lower(repo)
		HAVING min(delete_merged_branches) = 1 AND (min(coalesce(branches_swept_at, 0)) < ?) ORDER BY forge_id, repo`, s.nowMS()-branchSweepEvery.Milliseconds())
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "branch sweep: find the repos to sweep", "err", err)
		}
		return
	}
	type fr struct {
		forge int64
		repo  string
	}
	var list []fr
	for rows.Next() {
		var x fr
		if err := rows.Scan(&x.forge, &x.repo); err != nil {
			s.log.WarnContext(ctx, "branch sweep: read a repo to sweep", "err", err)
			continue
		}
		list = append(list, x)
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if readFault(err) {
		s.log.WarnContext(ctx, "branch sweep: read the repos to sweep", "err", err)
	}
	for _, x := range list {
		if ctx.Err() != nil {
			return
		}
		f, err := s.forgeByID(ctx, x.forge)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "branch sweep: read the forge", "repo", x.repo, "err", err)
			}
		} else if f.token != "" {
			s.sweepRepoBranches(ctx, f, x.repo)
		}
	}
}

// RemoteTidy is what the sweep would delete in a project's repos (or in one
// repo, wherever the actor can see it), with the stale branches it only
// lists. It reads the forge, so a listing is reused for a few minutes.
func (s *Service) RemoteTidy(ctx context.Context, actor *Actor, projectRef, repo string) ([]*RemoteBranches, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	vis, args := visibleSQL("r.project_id", actor)
	where := vis
	if projectRef != "" {
		p, err := s.projectByRef(ctx, s.db.R, projectRef)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where += ` AND r.project_id = ?`
		args = append(args, p.ID)
	}
	if repo != "" {
		where += ` AND r.repo = ? COLLATE NOCASE`
		args = append(args, repo)
	}
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT r.forge_id, r.repo FROM project_repos r WHERE `+where+` ORDER BY r.forge_id, r.repo`, args...)
	if err != nil {
		return nil, err
	}
	type fr struct {
		forge int64
		repo  string
	}
	var list []fr
	seen := map[string]bool{}
	for rows.Next() {
		var x fr
		if err := rows.Scan(&x.forge, &x.repo); err != nil {
			rows.Close()
			return nil, err
		}
		if k := fmt.Sprint(x.forge) + "|" + strings.ToLower(x.repo); !seen[k] {
			seen[k] = true
			list = append(list, x)
		}
	}
	rows.Close()
	out := []*RemoteBranches{}
	for _, x := range list {
		f, err := s.forgeByID(ctx, x.forge)
		if err != nil {
			continue
		}
		plan, err := s.cachedPlan(ctx, f, x.repo)
		if err != nil {
			plan = &RemoteBranches{Repo: x.repo, Forge: f.Kind, Rows: []RemoteBranch{}, Counts: map[string]int{}, Notes: []string{"couldn't read the forge: " + PublicError(err)}}
		}
		out = append(out, plan)
	}
	return out, nil
}

func (s *Service) cachedPlan(ctx context.Context, f *Forge, repo string) (*RemoteBranches, error) {
	key := fmt.Sprintf("%d|%s", f.ID, strings.ToLower(repo))
	if v, ok := s.tidyCache.Load(key); ok {
		if c := v.(tidyCached); s.now().Sub(c.at) < tidyCacheFor {
			return c.plan, nil
		}
	}
	plan, err := s.planBranches(ctx, f, repo)
	if err != nil {
		return nil, err
	}
	s.tidyCache.Store(key, tidyCached{at: s.now(), plan: plan})
	return plan, nil
}

// SetRepoBranchCleanup turns deleting a repo's merged branches on the forge
// on or off.
func (s *Service) SetRepoBranchCleanup(ctx context.Context, actor *Actor, projectRef string, id int64, on bool) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	defer s.tidyCache.Clear()
	return s.write(ctx, actor, func(t *tx) error {
		var repo string
		if err := t.QueryRowContext(ctx, `SELECT repo FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).Scan(&repo); err != nil {
			return notFound("project repo")
		}
		if _, err := t.ExecContext(ctx, `UPDATE project_repos SET delete_merged_branches = ? WHERE id = ?`, boolInt(on), id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "repo.branch_cleanup", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id, Target: repo,
			Detail: map[bool]string{true: "turned on", false: "turned off"}[on] + " deleting merged branches on " + repo})
		return nil
	})
}
