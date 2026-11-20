package core

// Pushes and releases (WP-D21). A push to a branch of an attached repo
// emits repo.pushed with the files it changed, follows the new head's CI and
// is remembered per branch: "branches in work" are the branches pushed
// lately that no merged or closed pull request has finished.

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"
)

// maxPushPaths bounds the changed files an event lists.
const maxPushPaths = 500

// onPush records a push to a branch and announces it.
func (s *Service) onPush(ctx context.Context, f *Forge, ev *forgeEvent) {
	branch, ok := strings.CutPrefix(ev.ref, "refs/heads/")
	if !ok || branch == "" || ev.repo == "" {
		return // a tag, or a payload that doesn't say
	}
	if strings.HasPrefix(branch, stackPrefix) {
		return // a merge queue stack Waypoint made (WP-404): nobody's work
	}
	projects, err := s.projectsOfRepo(ctx, f.ID, ev.repo)
	if err != nil || len(projects) == 0 {
		return
	}
	if ev.deleted {
		if _, err := s.db.W.ExecContext(ctx, `DELETE FROM branch_activity WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND branch = ?`, f.ID, ev.repo, branch); err != nil {
			s.log.WarnContext(ctx, "forget a deleted branch", "repo", ev.repo, "branch", branch, "err", err)
		}
		return
	}
	s.recordInstructionMarks(ctx, f, ev, branch)
	s.onAgentFilesPush(ctx, f, ev, branch) // Agent setup (WP-ADR44)
	var paths []string
	seen := map[string]bool{}
	for _, c := range ev.commits {
		for _, p := range c.paths {
			if !seen[p] && len(paths) < maxPushPaths {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	if paths == nil {
		paths = []string{}
	}
	now := s.nowMS()
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO branch_activity(forge_id, repo, branch, head_sha, before_sha, pusher, commits, created, pushed_at) VALUES (?,?,?,?,?,?,?,?,?)
			ON CONFLICT(forge_id, repo, branch) DO UPDATE SET head_sha = excluded.head_sha, before_sha = excluded.before_sha, pusher = excluded.pusher,
				commits = excluded.commits, created = excluded.created, pushed_at = excluded.pushed_at`,
			f.ID, ev.repo, branch, ev.after, ev.before, ev.pusher, len(ev.commits), boolInt(ev.created), now); err != nil {
			return err
		}
		for _, pid := range projects {
			t.emit(&Event{Kind: EvRepoPushed, ProjectID: pid, New: branch, Data: map[string]any{
				"repo": ev.repo, "branch": branch, "before": ev.before, "after": ev.after, "pusher": ev.pusher, "paths": paths, "created": ev.created}})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record a push", "repo", ev.repo, "branch", branch, "err", err)
		return
	}
	s.onBaseMoved(ctx, f, ev.repo, branch) // pull requests on it may have nothing left to merge (WP-140)
	if _, ok := s.Forges.(ForgeChecks); ok && ev.after != "" && f.token != "" {
		if _, err := s.trackHead(ctx, f.ID, ev.repo, ev.after); err == nil {
			bg := context.WithoutCancel(ctx)
			s.goBG(func() { _ = s.refreshHead(bg, f, ev.repo, ev.after) })
		}
	}
}

// onRelease announces a published release.
func (s *Service) onRelease(ctx context.Context, f *Forge, ev *forgeEvent) {
	if ev.release == nil {
		return
	}
	projects, err := s.projectsOfRepo(ctx, f.ID, ev.repo)
	if err != nil || len(projects) == 0 {
		return
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, pid := range projects {
			t.emit(&Event{Kind: EvRepoRelease, ProjectID: pid, New: ev.release.tag, Data: map[string]any{
				"repo": ev.repo, "tag": ev.release.tag, "name": ev.release.name, "url": ev.release.url}})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "announce a release", "repo", ev.repo, "err", err)
	}
}

// checkAfterActivity re-reads a pull request's threads and gate after a
// review or comment webhook (a person may have replied, or resolved).
func (s *Service) checkAfterActivity(ctx context.Context, f *Forge, repo string, number int) {
	if number == 0 {
		return
	}
	pr, err := s.prByNumber(ctx, s.db.R, f.ID, repo, number)
	if err != nil || pr == nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	if pr.ArmedAt != nil {
		// A reviewer it waits for posted, or the quiet period starts again (WP-133).
		s.goBG(func() { s.maybeAutoMerge(bg, pr.ID) })
	}
	var findings int
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM bot_findings WHERE pr_id = ? AND state != 'dropped'`, pr.ID).Scan(&findings)
	if findings == 0 && pr.GateState == "" {
		return
	}
	s.goBG(func() { s.checkPR(bg, pr.ID, "review or comment") })
}

// ---- branches in work ----

// BranchPR is the pull request a branch has.
type BranchPR struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	State  string `json:"state"` // open, draft, merged, closed
	Gate   string `json:"gate,omitempty"`
	Armed  bool   `json:"armed,omitempty"`
}

// Branch is a branch somebody is working on.
type Branch struct {
	Repo     string    `json:"repo"`
	Forge    string    `json:"forge"` // github, forgejo, gitlab
	Branch   string    `json:"branch"`
	HeadSHA  string    `json:"head_sha"`
	PushedAt time.Time `json:"pushed_at"`
	Pusher   string    `json:"pusher,omitempty"`
	Created  bool      `json:"created,omitempty"` // the last push made the branch
	CI       string    `json:"ci,omitempty"`      // pending, passed, failed, none; "" unknown
	PR       *BranchPR `json:"pr"`
}

const branchesWindow = 30 * 24 * time.Hour

// ListBranches lists the branches pushed lately in a project's repos (or in
// every project the actor can see), newest push first, with each one's pull
// request, CI and gate state.
func (s *Service) ListBranches(ctx context.Context, actor *Actor, projectRef string) ([]Branch, error) {
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
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT r.forge_id, f.kind, r.repo FROM project_repos r JOIN forges f ON f.id = r.forge_id WHERE `+where+` ORDER BY r.forge_id, r.repo`, args...)
	if err != nil {
		return nil, err
	}
	type repoRef struct {
		forge int64
		kind  string
		repo  string
	}
	var repos []repoRef
	seen := map[string]bool{}
	for rows.Next() {
		var x repoRef
		if err := rows.Scan(&x.forge, &x.kind, &x.repo); err != nil {
			rows.Close()
			return nil, err
		}
		if k := strings.ToLower(x.repo) + "|" + itoa64(x.forge); !seen[k] {
			seen[k] = true
			repos = append(repos, x)
		}
	}
	rows.Close()
	now := s.nowMS()
	since := now - branchesWindow.Milliseconds()
	out := []Branch{}
	for _, rp := range repos {
		prs, err := s.recentPRs(ctx, rp.forge, rp.repo, since, actor.HidesDrafts())
		if err != nil {
			return nil, err
		}
		bases := map[string]bool{"main": true, "master": true}
		byHead := map[string]*PullRequest{}
		for _, p := range prs {
			bases[p.BaseRef] = true
			if cur := byHead[p.HeadRef]; cur == nil || p.ID > cur.ID {
				byHead[p.HeadRef] = p
			}
		}
		ci := map[string]string{}
		crows, err := s.db.R.QueryContext(ctx, `SELECT sha, state FROM ci_heads WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND seen_at > ?`, rp.forge, rp.repo, since)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var sha, st string
			if crows.Scan(&sha, &st) == nil {
				ci[sha] = st
			}
		}
		crows.Close()
		brows, err := s.db.R.QueryContext(ctx, `SELECT branch, head_sha, pusher, created, pushed_at FROM branch_activity WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND pushed_at > ? ORDER BY pushed_at DESC LIMIT 200`,
			rp.forge, rp.repo, since)
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for brows.Next() {
			var b Branch
			var created bool
			var at int64
			if err := brows.Scan(&b.Branch, &b.HeadSHA, &b.Pusher, &created, &at); err != nil {
				brows.Close()
				return nil, err
			}
			b.Repo, b.Forge, b.Created, b.PushedAt = rp.repo, rp.kind, created, msTime(at)
			have[b.Branch] = true
			if bases[b.Branch] {
				continue
			}
			pr := byHead[b.Branch]
			if pr != nil && (pr.State == "merged" || pr.State == "closed") {
				end := msOr(pr.MergedAt, msOr(pr.ClosedAt, 0))
				if at <= end+time.Minute.Milliseconds() {
					continue // the branch's work is done
				}
				pr = nil
			}
			b.attach(pr, ci)
			out = append(out, b)
		}
		brows.Close()
		// Open pull requests whose branch Waypoint never saw pushed.
		for head, pr := range byHead {
			if have[head] || bases[head] || (pr.State != "open" && pr.State != "draft") {
				continue
			}
			b := Branch{Repo: rp.repo, Forge: rp.kind, Branch: head, HeadSHA: pr.HeadSHA, PushedAt: pr.UpdatedAt}
			b.attach(pr, ci)
			out = append(out, b)
		}
	}
	slices.SortStableFunc(out, func(a, b Branch) int { return b.PushedAt.Compare(a.PushedAt) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

func (b *Branch) attach(pr *PullRequest, ci map[string]string) {
	b.CI = ci[b.HeadSHA]
	if pr == nil {
		return
	}
	b.PR = &BranchPR{ID: pr.ID, Number: pr.Number, URL: pr.URL, Title: pr.Title, State: pr.State, Gate: pr.GateState, Armed: pr.ArmedAt != nil}
	if pr.CIState != "" && pr.ciSHA == b.HeadSHA {
		b.CI = pr.CIState
	}
}

// recentPRs are a repo's open pull requests and ones touched since a time.
// Ones from outside are for people (WP-281): agents get them with peopleOnly.
func (s *Service) recentPRs(ctx context.Context, forgeID int64, repo string, since int64, peopleOnly bool) ([]*PullRequest, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE
		AND (state IN ('open','draft') OR updated_at > ?) AND (? = 0 OR outside = 0) ORDER BY id DESC LIMIT 300`, forgeID, repo, since, peopleOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullRequest
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
