package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Delivery stages, in the order work moves through them.
const (
	StageWorking   = "working"
	StageBlocked   = "blocked"
	StageReview    = "review"
	StageStuck     = "stuck"
	StageMerged    = "merged"
	StageLive      = "live"
	StageUntracked = "untracked"
)

// deliveryDefaultDays is how far back merged work is listed.
const deliveryDefaultDays = 7

// deliveryForgeTime bounds the forge questions one Delivery asks.
const deliveryForgeTime = 15 * time.Second

// DeliveryQuery picks what Delivery lists.
type DeliveryQuery struct {
	Project string // a project key; "" for every project the actor sees
	Days    int    // merged work this recent; open work always
}

// Delivery is where recent work stands on its way to production.
type Delivery struct {
	Environments []*DeliveryEnv  `json:"environments"`
	Items        []*DeliveryItem `json:"items"`
}

// DeliveryEnv is a deployed environment and whether it has what merged.
type DeliveryEnv struct {
	ID         int64      `json:"id"`
	Project    string     `json:"project"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	Repo       string     `json:"repo"`
	Branch     string     `json:"branch"`
	Commit     string     `json:"commit"`
	CommitURL  string     `json:"commit_url,omitempty"`
	Version    string     `json:"version"`
	DeployedAt *time.Time `json:"deployed_at"`
	// Deploying: a deploy started and hasn't finished.
	Deploying bool `json:"deploying"`
	// Missing counts the listed merged pull requests it doesn't have yet;
	// Unknown those Waypoint couldn't tell about (the forge didn't answer).
	Missing int `json:"missing"`
	Unknown int `json:"unknown"`
	// Behind: its branch has moved on since the deployed commit.
	Behind bool `json:"behind"`
}

// DeliveryItem is one piece of work: a pull request with the tasks it is
// for, or a task an agent is on that has no pull request yet.
type DeliveryItem struct {
	Stage  string         `json:"stage"`
	Detail string         `json:"detail"` // what it waits on, in words
	Tasks  []PRIssue      `json:"tasks"`
	Agent  string         `json:"agent,omitempty"`
	PR     *DeliveryPR    `json:"pr,omitempty"`
	Envs   []*DeliveryRun `json:"envs"` // where a merged pull request runs
	At     time.Time      `json:"at"`   // its last move
	// QueueAhead counts the pull requests ahead of it in the merge queue.
	QueueAhead *int `json:"queue_ahead,omitempty"`
}

// DeliveryPR is the pull request an item went out on.
type DeliveryPR struct {
	ID       int64      `json:"id"`
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	URL      string     `json:"url"`
	State    string     `json:"state"`
	CI       string     `json:"ci,omitempty"`
	Gate     string     `json:"gate,omitempty"`
	Armed    bool       `json:"armed,omitempty"`
	Babysit  string     `json:"babysit,omitempty"`
	OpenedAt *time.Time `json:"opened_at"`
	MergedAt *time.Time `json:"merged_at"`
}

// DeliveryRun says whether a merged pull request runs in an environment.
type DeliveryRun struct {
	Environment int64      `json:"environment"`
	Project     string     `json:"project"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	In          bool       `json:"in"`
	Unknown     bool       `json:"unknown,omitempty"` // the forge couldn't say
	Since       *time.Time `json:"since,omitempty"`   // the first deploy after the merge
}

// Delivery lists recent work by where it stands on its way to production.
func (s *Service) Delivery(ctx context.Context, actor *Actor, q DeliveryQuery) (*Delivery, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	for _, p := range []Permission{PermEnvironmentsRead, PermPullsRead, PermIssuesRead} {
		if err := actor.need(p); err != nil {
			return nil, err
		}
	}
	if q.Days <= 0 || q.Days > 90 {
		q.Days = deliveryDefaultDays
	}
	prs, err := s.ListPullRequests(ctx, actor, PRQuery{Project: q.Project, Days: q.Days})
	if err != nil {
		return nil, err
	}
	envList, err := s.ListEnvironments(ctx, actor, q.Project, false)
	if err != nil {
		return nil, err
	}
	out := &Delivery{Environments: []*DeliveryEnv{}, Items: []*DeliveryItem{}}
	var envs []*Environment
	for _, e := range envList {
		if (e.Kind == EnvProduction || e.Kind == EnvStaging) && e.Repo != "" {
			envs = append(envs, e)
			de := &DeliveryEnv{ID: e.ID, Project: e.Project, Name: e.Name, Kind: e.Kind, Status: e.Status, Repo: e.Repo, Branch: e.Branch,
				Commit: e.Commit, CommitURL: e.CommitURL, Version: e.Version, DeployedAt: e.DeployedAt, Behind: e.Behind}
			de.Deploying = e.LastDeploy != nil && e.LastDeploy.Status == DeployStarted
			out.Environments = append(out.Environments, de)
		}
	}

	since := s.now().AddDate(0, 0, -q.Days)
	var merged []*PRSummary
	seenTask := map[int64]bool{}
	for _, p := range prs.Pulls {
		switch {
		case p.State == "closed":
			continue
		case p.State == "merged" && (p.MergedAt == nil || p.MergedAt.Before(since)):
			continue
		}
		it := &DeliveryItem{Tasks: p.Issues, At: p.LastActivity, Envs: []*DeliveryRun{},
			PR: &DeliveryPR{ID: p.ID, Repo: p.Repo, Number: p.Number, Title: p.Title, URL: p.URL, State: p.State, CI: p.CI, Gate: p.Gate, Armed: p.Armed,
				OpenedAt: p.OpenedAt, MergedAt: p.MergedAt}}
		if it.Tasks == nil {
			it.Tasks = []PRIssue{}
		}
		if len(p.Agents) > 0 {
			it.Agent = p.Agents[0]
		}
		for _, t := range p.Issues {
			seenTask[t.ID] = true
		}
		out.Items = append(out.Items, it)
		if p.State == "merged" {
			merged = append(merged, p)
		}
	}
	if err := s.stageOpen(ctx, out.Items); err != nil {
		return nil, err
	}
	s.stageMerged(ctx, out, envs, merged)
	if err := s.stageWorking(ctx, actor, q, out, seenTask); err != nil {
		return nil, err
	}
	rank := map[string]int{StageStuck: 0, StageMerged: 1, StageReview: 2, StageBlocked: 3, StageWorking: 4, StageLive: 5, StageUntracked: 6}
	sort.SliceStable(out.Items, func(a, b int) bool {
		x, y := out.Items[a], out.Items[b]
		if rank[x.Stage] != rank[y.Stage] {
			return rank[x.Stage] < rank[y.Stage]
		}
		return x.At.After(y.At)
	})
	return out, nil
}

// stageOpen places the items whose pull request is still open.
func (s *Service) stageOpen(ctx context.Context, items []*DeliveryItem) error {
	for _, it := range items {
		if it.PR.State != "open" && it.PR.State != "draft" {
			continue
		}
		pr, err := s.prByID(ctx, s.db.R, it.PR.ID)
		if err != nil {
			return err
		}
		var babysit string
		err = s.db.R.QueryRowContext(ctx, `SELECT state FROM babysits WHERE pr_id = ? ORDER BY id DESC LIMIT 1`, pr.ID).Scan(&babysit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		it.PR.Babysit = babysit
		it.Stage, it.Detail = StageReview, ""
		switch {
		case babysit == BabysitWorking:
			it.Detail = "An agent is fixing what CI or review found."
		case babysit == BabysitNeedsPerson || babysit == BabysitGaveUp:
			it.Stage, it.Detail = StageStuck, "Babysitting handed it to a person."
		case pr.CIState == "failed":
			it.Stage, it.Detail = StageStuck, "CI failed on its latest commit."
		case pr.Mergeable != nil && !*pr.Mergeable && pr.State == "open":
			it.Stage, it.Detail = StageStuck, "It conflicts with its base."
		case pr.GateState == "failure":
			it.Stage, it.Detail = StageStuck, "Review findings are open."
		case pr.armedNote != "":
			it.Detail = "Set to merge, but: " + pr.armedNote
		case pr.State == "draft" && pr.WipAuto:
			it.Detail = "A draft until its checks pass; Waypoint then marks it ready."
		case pr.State == "draft":
			it.Stage, it.Detail = StageStuck, "A draft: someone has to mark it ready."
		case pr.CIState == "pending":
			it.Detail = "CI is running."
		case babysit == BabysitWaitingReview || pr.GateState == "pending":
			it.Detail = "Waiting for review."
		case pr.ArmedAt != nil:
			it.Detail = "Set to merge when ready."
		default:
			it.Stage, it.Detail = StageStuck, "Nobody set it to merge: it waits for a person."
		}
		if pr.ArmedAt != nil && pr.QueuedAt != nil && it.Stage == StageReview {
			if ahead, err := s.queueAhead(ctx, pr); err == nil {
				n := len(ahead)
				it.QueueAhead = &n
				if n > 0 && it.Detail == "Set to merge when ready." {
					it.Detail = fmt.Sprintf("In the merge queue, %d ahead of it.", n)
				}
			}
		}
	}
	return nil
}

// stageMerged places merged items: live where every production environment
// of their repo and branch has the merge (staging when there is no
// production), merged until then, untracked when no environment deploys
// the branch.
func (s *Service) stageMerged(ctx context.Context, out *Delivery, envs []*Environment, merged []*PRSummary) {
	fctx, cancel := context.WithTimeout(ctx, deliveryForgeTime)
	defer cancel()
	type result struct{ in, unknown bool }
	runs := map[[2]int64]result{}
	for _, e := range envs {
		byBase := map[string][]*PRSummary{}
		for _, p := range merged {
			if strings.EqualFold(p.Repo, e.Repo) && (e.Branch == "" || e.Branch == p.BaseRef) {
				byBase[p.BaseRef] = append(byBase[p.BaseRef], p)
			}
		}
		for _, mine := range byBase {
			sort.SliceStable(mine, func(a, b int) bool { return mine[a].MergedAt.Before(*mine[b].MergedAt) })
			in := func(p *PRSummary) (bool, error) { return s.commitHas(fctx, e, p) }
			lo, hi := 0, len(mine)
			for n, p := range mine {
				if p.MergeSHA != "" && sameCommit(e.Commit, p.MergeSHA) {
					lo, hi = n+1, n+1
				}
			}
			var failed bool
			for lo < hi {
				mid := (lo + hi) / 2
				ok, err := in(mine[mid])
				if err != nil {
					failed = true
					break
				}
				if ok {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			for n, p := range mine {
				switch {
				case n < lo:
					runs[[2]int64{p.ID, e.ID}] = result{in: true}
				case n >= hi:
					runs[[2]int64{p.ID, e.ID}] = result{}
				default:
					runs[[2]int64{p.ID, e.ID}] = result{unknown: failed}
				}
			}
		}
	}
	firsts := s.firstDeploys(ctx, envs, merged)
	envByID := map[int64]*DeliveryEnv{}
	for _, de := range out.Environments {
		envByID[de.ID] = de
	}
	for _, it := range out.Items {
		if it.PR.State != "merged" {
			continue
		}
		var targets []*DeliveryRun
		for _, e := range envs {
			r, ok := runs[[2]int64{it.PR.ID, e.ID}]
			if !ok {
				continue
			}
			run := &DeliveryRun{Environment: e.ID, Project: e.Project, Name: e.Name, Kind: e.Kind, In: r.in, Unknown: r.unknown}
			if r.in {
				run.Since = firsts[[2]int64{it.PR.ID, e.ID}]
				if run.Since == nil {
					run.Since = e.DeployedAt
				}
			}
			it.Envs = append(it.Envs, run)
			de := envByID[e.ID]
			switch {
			case r.unknown:
				de.Unknown++
			case !r.in:
				de.Missing++
			}
		}
		for _, kind := range []string{EnvProduction, EnvStaging} {
			for _, r := range it.Envs {
				if r.Kind == kind {
					targets = append(targets, r)
				}
			}
			if len(targets) > 0 {
				break
			}
		}
		if len(targets) == 0 {
			it.Stage, it.Detail = StageUntracked, "Merged; no production or staging environment deploys its branch, so Waypoint can't tell where it runs."
			continue
		}
		var missing, unknown []string
		deploying := false
		for _, r := range targets {
			switch {
			case r.Unknown:
				unknown = append(unknown, r.Name)
			case !r.In:
				missing = append(missing, r.Name)
				deploying = deploying || envByID[r.Environment].Deploying
			}
		}
		switch {
		case len(missing) > 0 && deploying:
			it.Stage, it.Detail = StageMerged, "Merged; a deploy to "+strings.Join(missing, ", ")+" is under way."
		case len(missing) > 0:
			it.Stage, it.Detail = StageMerged, "Merged, not deployed to "+strings.Join(missing, ", ")+" yet."
		case len(unknown) > 0:
			it.Stage, it.Detail = StageMerged, "Merged; the forge didn't say whether "+strings.Join(unknown, ", ")+" has it."
		default:
			it.Stage, it.Detail = StageLive, ""
		}
	}
}

// commitHas says whether the commit e runs contains p's merge.
func (s *Service) commitHas(ctx context.Context, e *Environment, p *PRSummary) (bool, error) {
	head := p.MergeSHA
	fromHead := false
	if head == "" {
		head = p.HeadSHA
		fromHead = true
	}
	if e.Commit == "" {
		return false, nil
	}
	if head == "" {
		return false, ErrUnsupported
	}
	if sameCommit(e.Commit, head) {
		return true, nil
	}
	key := strings.ToLower(e.Repo) + "\x00" + e.Commit + "\x00" + head
	if v, ok := s.contains.Load(key); ok {
		if !v.(bool) && fromHead {
			return false, ErrUnsupported
		}
		return v.(bool), nil
	}
	cl, ok := s.Forges.(ForgeBranches)
	if !ok {
		return false, ErrUnsupported
	}
	var forgeID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id FROM pull_requests WHERE id = ?`, p.ID).Scan(&forgeID); err != nil {
		return false, err
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return false, err
	}
	if f.token == "" {
		return false, ErrUnsupported
	}
	n, err := cl.Ahead(ctx, f, e.Repo, e.Commit, head)
	if err != nil {
		return false, err
	}
	if n != 0 && fromHead {
		return false, ErrUnsupported
	}
	s.contains.Store(key, n == 0)
	return n == 0, nil
}

// firstDeploys finds, per merged pull request and environment, the first
// successful deploy at or after the merge: when it went live there.
func (s *Service) firstDeploys(ctx context.Context, envs []*Environment, merged []*PRSummary) map[[2]int64]*time.Time {
	out := map[[2]int64]*time.Time{}
	if len(envs) == 0 || len(merged) == 0 {
		return out
	}
	oldest := *merged[0].MergedAt
	for _, p := range merged {
		if p.MergedAt.Before(oldest) {
			oldest = *p.MergedAt
		}
	}
	ids := make([]int64, len(envs))
	for n, e := range envs {
		ids[n] = e.ID
	}
	// #nosec G202 -- placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT environment_id, COALESCE(finished_at, started_at) FROM deployments
		WHERE environment_id IN (`+placeholders(len(ids))+`) AND status IN ('succeeded','detected') AND COALESCE(finished_at, started_at) >= ?
		ORDER BY 2`, append(int64Args(ids), oldest.UnixMilli())...)
	if err != nil {
		return out
	}
	defer rows.Close()
	at := map[int64][]time.Time{}
	for rows.Next() {
		var id, ms int64
		if rows.Scan(&id, &ms) == nil {
			at[id] = append(at[id], msTime(ms))
		}
	}
	for _, e := range envs {
		for _, p := range merged {
			list := at[e.ID]
			if n, _ := slices.BinarySearchFunc(list, *p.MergedAt, func(a, b time.Time) int { return a.Compare(b) }); n < len(list) {
				t := list[n]
				out[[2]int64{p.ID, e.ID}] = &t
			}
		}
	}
	return out
}

// stageWorking adds the tasks agents hold that have no pull request listed:
// being worked on, or blocked.
func (s *Service) stageWorking(ctx context.Context, actor *Actor, q DeliveryQuery, out *Delivery, seen map[int64]bool) error {
	res, err := s.Search(ctx, actor, SearchParams{Query: "is:claimed is:open", Project: q.Project, Limit: 200})
	if err != nil {
		return err
	}
	var held []*Issue
	for _, i := range res.Issues {
		if !seen[i.ID] && i.ClaimedBy != "" {
			held = append(held, i)
		}
	}
	blockers, err := s.openBlockers(ctx, actor, held)
	if err != nil {
		return err
	}
	for _, i := range held {
		it := &DeliveryItem{Stage: StageWorking, Agent: i.ClaimedBy, Envs: []*DeliveryRun{}, At: i.UpdatedAt,
			Tasks: []PRIssue{{ID: i.ID, Key: i.Key, Title: i.Title, Status: i.Status.Name, Category: i.Status.Category}}}
		if i.ClaimedAt != nil {
			it.At = *i.ClaimedAt
		}
		if b := blockers[i.ID]; len(b) > 0 {
			it.Stage, it.Detail = StageBlocked, "Blocked by "+strings.Join(b, ", ")+"."
		} else {
			it.Detail = "An agent is working on it."
		}
		out.Items = append(out.Items, it)
	}
	return nil
}
