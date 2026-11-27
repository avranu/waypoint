package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Events (kept off the issue history; UserID scopes them to the owner).
const (
	// Data: fanout (id), items (n), state (asked, running).
	EvFanoutCreated = "fanout.created"
	// Data: fanout, item (id), key, outcome (done, budget, ended, failed), state, spent, budget, model.
	EvFanoutItemEnded = "fanout.item_ended"
	// Data: fanout, state (finished, stopped, declined).
	EvFanoutFinished = "fanout.finished"
)

// Fan-out states.
const (
	FanoutAsked    = "asked"
	FanoutRunning  = "running"
	FanoutFinished = "finished"
	FanoutStopped  = "stopped"
	FanoutDeclined = "declined"
)

// Package (item) states.
const (
	ItemQueued    = "queued"
	ItemStarting  = "starting"
	ItemRunning   = "running"
	ItemDone      = "done"
	ItemStopped   = "stopped"
	ItemFailed    = "failed"
	ItemCancelled = "cancelled"
)

// Item outcomes a client reports (EndFanoutItem).
const (
	OutcomeDone   = "done"
	OutcomeBudget = "budget"
	OutcomeEnded  = "ended"
)

const (
	fanoutDefaultParallel = 3
	fanoutMaxParallel     = 6
	fanoutMaxPackages     = 30
	fanoutMaxBudget       = 500.0
	// fanoutAskPct: an estimate over this share of a week's quota asks the
	// person before anything starts.
	fanoutAskPct = 3.0
	// fanoutLostAfter: a running package whose session Waypoint never saw
	// (or hasn't heard from) this long is taken as ended. Long, because the
	// hook reports only some tool events, so a busy turn can stay quiet.
	fanoutLostAfter = 60 * time.Minute
	// Fallbacks for the estimate, when there isn't enough usage to measure.
	fanoutDefaultCalls = 150
	// X6: $311 of API-equivalent spend was ~17% of the week.
	fanoutDefaultUSDPerPct = 18.3
)

// fanoutDefaultPerCall is the API-equivalent cost of one call, by model
// family, when there isn't enough usage to measure it. Fable and Mythos are
// Opus's $0.10 scaled by the 2.5x list-price ratio.
var fanoutDefaultPerCall = map[string]float64{"opus": 0.10, "sonnet": 0.06, "haiku": 0.01, "fable": 0.25, "mythos": 0.25}

// fanoutFamilies are the model families with a default, in the order the
// basis text lists them.
var fanoutFamilies = []string{"opus", "sonnet", "haiku", "fable", "mythos"}

// fanoutDearest is the per-call default assumed for a model of no known family.
func fanoutDearest() float64 {
	top := 0.0
	for _, c := range fanoutDefaultPerCall {
		top = max(top, c)
	}
	return top
}

// fanoutDefaultsText lists the defaults as "opus $0.10, sonnet $0.06, ...".
func fanoutDefaultsText() string {
	parts := make([]string, 0, len(fanoutFamilies))
	for _, f := range fanoutFamilies {
		parts = append(parts, fmt.Sprintf("%s $%.2f", f, fanoutDefaultPerCall[f]))
	}
	return strings.Join(parts, ", ")
}

var (
	fanoutModelRE  = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._\[\]-]{0,63}$`)
	fanoutBranchRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
)

// FanoutPackage is one work package of a plan.
type FanoutPackage struct {
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Model  string  `json:"model"`
	Budget float64 `json:"budget"` // USD; 0: the plan's, else twice the estimate
	Branch string  `json:"branch"`
	Calls  int     `json:"calls"` // expected model calls, for the estimate
}

// FanoutInput is a plan: where its sessions start and its packages.
type FanoutInput struct {
	Project  string          `json:"project"`
	Parent   string          `json:"parent"`
	Model    string          `json:"model"`
	Budget   float64         `json:"budget"`
	Parallel int             `json:"parallel"`
	Base     string          `json:"base"`
	Machine  string          `json:"machine"`
	Dir      string          `json:"dir"`
	Agent    string          `json:"agent"` // the sessions' identity, when the caller isn't an agent
	Packages []FanoutPackage `json:"packages"`
	DryRun   bool            `json:"dry_run"`
}

// FanoutItem is one package: its task and how it went.
type FanoutItem struct {
	ID       int64      `json:"id"`
	Pos      int        `json:"pos"`
	Key      string     `json:"key,omitempty"`
	Title    string     `json:"title"`
	Status   string     `json:"status,omitempty"` // the task's
	State    string     `json:"state"`
	Model    string     `json:"model"`
	Budget   float64    `json:"budget"`
	Estimate float64    `json:"estimate"`
	Spent    float64    `json:"spent"`
	Branch   string     `json:"branch"`
	AttachID string     `json:"attach_id,omitempty"`
	Note     string     `json:"note,omitempty"`
	PR       string     `json:"pr,omitempty"` // its pull request's URL
	PRNumber int64      `json:"pr_number,omitempty"`
	Started  *time.Time `json:"started_at,omitempty"`
	Ended    *time.Time `json:"ended_at,omitempty"`
	issueID  int64
	told     bool
}

// Fanout is a plan and its packages.
type Fanout struct {
	ID       int64         `json:"id"`
	Project  string        `json:"project"`
	State    string        `json:"state"`
	Parallel int           `json:"parallel"`
	Machine  string        `json:"machine"`
	Dir      string        `json:"dir"`
	Base     string        `json:"base"`
	Agent    string        `json:"agent"`
	Estimate float64       `json:"estimate"`
	WeekPct  float64       `json:"week_pct"`
	Basis    string        `json:"basis"`
	Request  string        `json:"request,omitempty"` // R-12, the ask
	Spent    float64       `json:"spent"`
	Items    []*FanoutItem `json:"items"`
	Created  time.Time     `json:"created_at"`
	Ended    *time.Time    `json:"ended_at,omitempty"`
	// AskOver is the share of a week over which a plan is asked about.
	AskOver   float64 `json:"ask_over"`
	userID    int64
	sessionID int64
	requestID int64
	told      string
}

// Counts are how many packages are in each state.
func (f *Fanout) Counts() map[string]int {
	n := map[string]int{}
	for _, it := range f.Items {
		n[it.State]++
	}
	return n
}

// FanoutClaim is what to start now, or why nothing.
type FanoutClaim struct {
	Fanout  *Fanout       `json:"fanout"`
	Start   []*FanoutItem `json:"start"`
	Waiting string        `json:"waiting,omitempty"`
}

// fanoutRates are what the estimate multiplies: cost per call by model
// family, calls per package, and dollars per percent of a week.
type fanoutRates struct {
	perCall   map[string]float64
	calls     int
	usdPerPct float64
	basis     []string
}

// modelFamily is opus, sonnet, haiku, fable or mythos for Claude models, else the model.
func modelFamily(model string) string {
	m := strings.ToLower(model)
	for _, f := range fanoutFamilies {
		if strings.Contains(m, f) {
			return f
		}
	}
	return m
}

// perCallFor is one call's cost for a model.
func (r *fanoutRates) perCallFor(model string) float64 {
	if c, ok := r.perCall[modelFamily(model)]; ok {
		return c
	}
	return fanoutDearest()
}

// usageCost prices usage_hours rows (grouped by model) with the price book:
// the total cost and calls of the priced rows, per model family.
func (s *Service) usageCost(ctx context.Context, book *priceBook, where string, args ...any) (map[string][2]float64, error) {
	// #nosec G202 -- where is a constant of this file; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT model, sum(messages), sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE `+where+` GROUP BY model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]float64{}
	for rows.Next() {
		var model string
		var msgs, in, outT, cw5, cw1, cr int64
		if err := rows.Scan(&model, &msgs, &in, &outT, &cw5, &cw1, &cr); err != nil {
			return nil, err
		}
		p, ok := book.find(model)
		if !ok {
			continue
		}
		f := modelFamily(model)
		v := out[f]
		v[0] += p.Cost(in, outT, cw5, cw1, cr)
		v[1] += float64(msgs)
		out[f] = v
	}
	return out, rows.Err()
}

// fanoutRates measures the person's own usage where there is enough of it.
func (s *Service) fanoutRates(ctx context.Context, userID int64) *fanoutRates {
	r := &fanoutRates{perCall: map[string]float64{}, calls: fanoutDefaultCalls, usdPerPct: fanoutDefaultUSDPerPct}
	for f, c := range fanoutDefaultPerCall {
		r.perCall[f] = c
	}
	now := s.nowMS()
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "fan-out estimate: prices", "err", err)
		book = newPriceBook(nil)
	}
	measured := []string{}
	if byFam, err := s.usageCost(ctx, book, `user_id = ? AND hour >= ?`, userID, now-14*day); err == nil {
		for _, f := range fanoutFamilies {
			if v := byFam[f]; v[1] >= 50 && v[0] > 0 {
				r.perCall[f] = v[0] / v[1]
				measured = append(measured, fmt.Sprintf("%s $%.3f", f, r.perCall[f]))
			}
		}
	}
	if len(measured) > 0 {
		r.basis = append(r.basis, "cost per call from your last 14 days ("+strings.Join(measured, ", ")+"), defaults for the rest ("+fanoutDefaultsText()+")")
	} else {
		r.basis = append(r.basis, "default cost per call ("+fanoutDefaultsText()+"): too little priced usage to measure")
	}
	if n, ok := s.medianTaskCalls(ctx, userID, now); ok {
		r.calls = n
		r.basis = append(r.basis, fmt.Sprintf("%d calls a package (the median of your task sessions in 30 days)", n))
	} else {
		r.basis = append(r.basis, fmt.Sprintf("%d calls a package (default: fewer than 5 task sessions in 30 days)", r.calls))
	}
	if v, ok := s.usdPerWeekPct(ctx, book, userID, now); ok {
		r.usdPerPct = v
		r.basis = append(r.basis, fmt.Sprintf("$%.1f per 1%% of a week (this week's spend over its quota)", v))
	} else {
		r.basis = append(r.basis, fmt.Sprintf("$%.1f per 1%% of a week (default, from X6: too little of this week's quota used to measure)", r.usdPerPct))
	}
	return r
}

const day = int64(24 * time.Hour / time.Millisecond)

// medianTaskCalls is the median number of calls of the person's task
// sessions that ended in the last 30 days, with at least 5 of them.
func (s *Service) medianTaskCalls(ctx context.Context, userID, now int64) (int, bool) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT cli_id FROM agent_sessions WHERE user_id = ? AND kind = ? AND ended_at IS NOT NULL AND started_at >= ?
		ORDER BY started_at DESC LIMIT 200`, userID, SessionTask, now-30*day)
	if err != nil {
		return 0, false
	}
	var ids []any
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) < 5 {
		return 0, false
	}
	args := append([]any{userID, now - 31*day}, ids...)
	// #nosec G202 -- placeholders only; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT sum(messages) FROM usage_hours WHERE user_id = ? AND hour >= ? AND session IN (`+placeholders(len(ids))+`)
		GROUP BY session HAVING sum(messages) > 0`, args...)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	var calls []int
	for rows.Next() {
		var n int
		if rows.Scan(&n) == nil {
			calls = append(calls, n)
		}
	}
	if len(calls) < 5 {
		return 0, false
	}
	slices.Sort(calls)
	return calls[len(calls)/2], true
}

// usdPerWeekPct is this week's API-equivalent spend over the share of the
// weekly quota it used, once at least 5% is used.
func (s *Service) usdPerWeekPct(ctx context.Context, book *priceBook, userID, now int64) (float64, bool) {
	var resets int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT resets_at FROM quota_samples WHERE user_id = ? AND span = 'seven_day' ORDER BY at DESC LIMIT 1`, userID).Scan(&resets); err != nil {
		return 0, false
	}
	// The points used, not the peak: a mid-week allowance reset starts the
	// reading over (quota_reset.go), so 99, a reset, then 80 is 179 points.
	xs, err := s.weekSamples(ctx, userID, resets, resets-quotaWeek.Milliseconds())
	if err != nil {
		return 0, false
	}
	pct := pointsUsed(xs)
	if resets < now || pct < 5 {
		return 0, false
	}
	byFam, err := s.usageCost(ctx, book, `user_id = ? AND hour >= ?`, userID, resets-7*day)
	if err != nil {
		return 0, false
	}
	var spend float64
	for _, v := range byFam {
		spend += v[0]
	}
	if spend <= 0 {
		return 0, false
	}
	return spend / pct, true
}

// fanoutBranch is a package's default branch: its key and title.
func fanoutBranch(key, title string) string {
	return strings.ToLower(key) + "-" + slug(title, 40)
}

// checkFanout cleans a plan up and says what is wrong with it.
func checkFanout(in *FanoutInput) error {
	if len(in.Packages) == 0 {
		return invalid("a plan needs at least one [[package]]")
	}
	if len(in.Packages) > fanoutMaxPackages {
		return invalid("a plan can have at most %d packages", fanoutMaxPackages)
	}
	if in.Parallel == 0 {
		in.Parallel = fanoutDefaultParallel
	}
	if in.Parallel < 1 || in.Parallel > fanoutMaxParallel {
		return invalid("parallel is 1 to %d sessions at a time", fanoutMaxParallel)
	}
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" {
		in.Model = "sonnet"
	}
	in.Machine, in.Dir, in.Base = strings.TrimSpace(in.Machine), strings.TrimSpace(in.Dir), strings.TrimSpace(in.Base)
	if in.Machine == "" || len(in.Machine) > 200 {
		return invalid("say which machine the sessions start on (machine)")
	}
	if in.Dir == "" || !filepath.IsAbs(in.Dir) || len(in.Dir) > 1000 {
		return invalid("say which repository the worktrees come from (dir, an absolute path)")
	}
	if len(in.Base) > 200 || strings.HasPrefix(in.Base, "-") {
		return invalid("base is a branch or commit to start the worktrees from")
	}
	if in.Budget < 0 || in.Budget > fanoutMaxBudget {
		return invalid("a budget is $0 to $%.0f", fanoutMaxBudget)
	}
	branches := map[string]bool{}
	for n := range in.Packages {
		p := &in.Packages[n]
		where := fmt.Sprintf("package %d", n+1)
		p.Title, p.Body, p.Model, p.Branch = strings.TrimSpace(p.Title), strings.TrimSpace(p.Body), strings.TrimSpace(p.Model), strings.TrimSpace(p.Branch)
		if p.Title == "" {
			return invalid("%s needs a title", where)
		}
		if utf8.RuneCountInString(p.Title) > maxTitleLen {
			return invalid("%s: the title is over %d characters; put the detail in its body", where, maxTitleLen)
		}
		if len(p.Body) > maxDescriptionLen-1000 {
			return invalid("%s: the body is too long", where)
		}
		if p.Model == "" {
			p.Model = in.Model
		}
		if !fanoutModelRE.MatchString(p.Model) {
			return invalid("%s: %q isn't a model name (sonnet, opus, haiku or a model id)", where, p.Model)
		}
		if p.Budget < 0 || p.Budget > fanoutMaxBudget {
			return invalid("%s: a budget is $0 to $%.0f", where, fanoutMaxBudget)
		}
		if p.Calls < 0 || p.Calls > 10000 {
			return invalid("%s: calls is 0 to 10000", where)
		}
		if p.Branch != "" {
			if !fanoutBranchRE.MatchString(p.Branch) || strings.Contains(p.Branch, "..") || strings.HasSuffix(p.Branch, ".lock") || strings.HasSuffix(p.Branch, "/") {
				return invalid("%s: %q isn't a branch name git takes", where, p.Branch)
			}
			if branches[p.Branch] {
				return invalid("%s: branch %s is used twice", where, p.Branch)
			}
			branches[p.Branch] = true
		}
	}
	if !fanoutModelRE.MatchString(in.Model) {
		return invalid("%q isn't a model name (sonnet, opus, haiku or a model id)", in.Model)
	}
	return nil
}

// estimateFanout fills in each package's estimate and budget, and the
// plan's total, share of a week and basis.
func estimateFanout(in *FanoutInput, r *fanoutRates) (items []*FanoutItem, total, weekPct float64) {
	for n, p := range in.Packages {
		calls := p.Calls
		if calls == 0 {
			calls = r.calls
		}
		est := math.Round(r.perCallFor(p.Model)*float64(calls)*100) / 100
		budget := p.Budget
		if budget == 0 {
			budget = in.Budget
		}
		if budget == 0 {
			budget = max(1, math.Ceil(2*est))
		}
		total += est
		items = append(items, &FanoutItem{Pos: n + 1, Title: p.Title, State: ItemQueued, Model: p.Model, Budget: budget, Estimate: est, Branch: p.Branch})
	}
	if r.usdPerPct > 0 {
		weekPct = math.Round(total/r.usdPerPct*10) / 10
	}
	return items, math.Round(total*100) / 100, weekPct
}

// CreateFanout files a plan's packages as tasks and queues them, or with
// DryRun only says what it would cost. Over fanoutAskPct of a week, it asks
// the person and nothing starts until they say Start.
func (s *Service) CreateFanout(ctx context.Context, actor *Actor, in FanoutInput) (*Fanout, error) {
	if err := actor.need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := actor.need(PermIssuesCreate); err != nil {
		return nil, err
	}
	if actor.ReadOnly {
		return nil, invalid("a read-only token can't start sessions")
	}
	agent, err := agentIdentity(actor, in.Agent)
	if err != nil {
		return nil, err
	}
	if err := checkFanout(&in); err != nil {
		return nil, err
	}
	project := strings.TrimSpace(in.Project)
	if project == "" && in.Parent != "" {
		pid, err := s.resolveIssueID(ctx, s.db.R, in.Parent)
		if err != nil {
			return nil, err
		}
		pi, err := s.issueByID(ctx, s.db.R, pid, false)
		if err != nil {
			return nil, err
		}
		project = pi.ProjectKey
	}
	if project == "" {
		return nil, invalid("say which project the tasks go in (project)")
	}
	p, err := s.projectByRef(ctx, s.db.R, project)
	if err != nil {
		return nil, err
	}
	if role, err := s.projectRole(ctx, s.db.R, actor, p.ID); err != nil {
		return nil, err
	} else if role < RoleEditor {
		if role == RoleNone {
			return nil, notFound("project " + project)
		}
		return nil, ErrForbidden
	}
	rates := s.fanoutRates(ctx, actor.id())
	items, total, weekPct := estimateFanout(&in, rates)
	f := &Fanout{Project: p.Key, State: FanoutRunning, Parallel: in.Parallel, Machine: in.Machine, Dir: in.Dir, Base: in.Base, Agent: agent,
		Estimate: total, WeekPct: weekPct, Basis: strings.Join(rates.basis, "; "), Items: items, Created: s.now(), AskOver: fanoutAskPct}
	if weekPct > fanoutAskPct {
		f.State = FanoutAsked
	}
	if in.DryRun {
		return f, nil
	}
	if f.State == FanoutAsked {
		if err := actor.need(PermRequestsCreate); err != nil {
			return nil, err
		}
	}

	now := s.nowMS()
	err = s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `INSERT INTO fanouts(user_id, project_id, session_id, agent, machine, dir, base, parallel, estimate_usd, week_pct, basis, state, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, actor.id(), p.ID, nullID(actor.Session), agent, in.Machine, in.Dir, in.Base, in.Parallel, total, weekPct, f.Basis, FanoutStopped, now)
		if err != nil {
			return err
		}
		f.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	status := ""
	if st, err := s.firstStatusIn(ctx, s.db.R, p.ID, CatBacklog); err == nil {
		status = st.Name
	}
	for n, pk := range in.Packages {
		body := pk.Body
		if body != "" {
			body += "\n\n"
		}
		body += fmt.Sprintf("---\nPackage %d of %d of fan-out #%d (`wp fanout status %d`): %s, budget $%.2f. It runs as its own task session in its own worktree; at its budget it hands off and stops.",
			n+1, len(in.Packages), f.ID, f.ID, items[n].Model, items[n].Budget)
		d, err := s.CreateIssue(ctx, actor, IssueInput{Project: p.Key, Parent: in.Parent, Title: pk.Title, Description: body, Status: status})
		if err != nil {
			s.abandonFanout(ctx, f.ID, items[:n])
			return nil, fmt.Errorf("package %d: %w", n+1, err)
		}
		items[n].Key, items[n].issueID, items[n].Title = d.Key, d.ID, d.Title
		if items[n].Branch == "" {
			items[n].Branch = fanoutBranch(d.Key, d.Title)
		}
	}
	var requestID int64
	if f.State == FanoutAsked {
		r, _, err := s.CreateRequest(ctx, actor, s.fanoutAsk(f, p))
		if err != nil {
			s.abandonFanout(ctx, f.ID, items)
			return nil, fmt.Errorf("asking before the fan-out starts: %w", err)
		}
		requestID = r.ID
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if s.failFanoutItems != nil {
			if err := s.failFanoutItems(); err != nil {
				return err
			}
		}
		for _, it := range items {
			if _, err := t.ExecContext(ctx, `INSERT INTO fanout_items(fanout_id, issue_id, pos, model, budget_usd, estimate_usd, branch, state)
				VALUES (?,?,?,?,?,?,?,?)`, f.ID, it.issueID, it.Pos, it.Model, it.Budget, it.Estimate, it.Branch, ItemQueued); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE fanouts SET state = ?, request_id = ? WHERE id = ?`, f.State, nullID(requestID), f.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvFanoutCreated, ProjectID: p.ID, UserID: actor.id(), Data: map[string]any{"fanout": f.ID, "items": len(items), "state": f.State}})
		return nil
	})
	if err != nil {
		s.abandonFanout(ctx, f.ID, items)
		if requestID != 0 {
			if _, werr := s.db.W.ExecContext(ctx, `UPDATE requests SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, RequestWithdrawn, s.nowMS(), requestID, RequestOpen); werr != nil {
				s.log.WarnContext(ctx, "withdraw a half-made fan-out's request", "request", requestID, "err", werr)
			}
		}
		return nil, err
	}
	return s.GetFanout(ctx, actor, f.ID)
}

// abandonFanout closes the tasks a fan-out that failed half-made filed, and
// leaves it stopped, so none of them is worked on by mistake.
func (s *Service) abandonFanout(ctx context.Context, id int64, made []*FanoutItem) {
	for _, it := range made {
		if it.Key == "" {
			continue
		}
		if err := s.closeAs(ctx, it.issueID, CatCanceled); err != nil {
			s.log.WarnContext(ctx, "cancel a half-made fan-out's task", "issue", it.Key, "err", err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE fanouts SET state = ?, ended_at = ? WHERE id = ?`, FanoutStopped, s.nowMS(), id); err != nil {
		s.log.WarnContext(ctx, "stop a half-made fan-out", "fanout", id, "err", err)
	}
}

// closeAs moves an issue to its project's first status of a closed
// category, as Waypoint.
func (s *Service) closeAs(ctx context.Context, issueID int64, cat string) error {
	i, err := s.issueByID(ctx, s.db.R, issueID, false)
	if err != nil {
		return err
	}
	st, err := s.firstStatusIn(ctx, s.db.R, i.ProjectID, cat)
	if err != nil {
		return err
	}
	_, err = s.UpdateIssue(ctx, SystemActor(), i.Key, IssuePatch{Status: &st.Name})
	return err
}

// fanoutAsk is the question for a plan over the line: what, why, what if
// nothing, and the click.
func (s *Service) fanoutAsk(f *Fanout, p *Project) RequestInput {
	var b strings.Builder
	fmt.Fprintf(&b, "An agent wants to run %d pieces of work in %s as separate agent sessions, %d at a time (fan-out #%d):\n\n", len(f.Items), p.Name, f.Parallel, f.ID)
	for _, it := range f.Items {
		fmt.Fprintf(&b, "- %s %s: %s, estimated $%.2f, stops at $%.2f\n", it.Key, it.Title, it.Model, it.Estimate, it.Budget)
	}
	fmt.Fprintf(&b, "\nThe estimate is $%.2f, about %.1f%% of a week's usage; anything over %.0f%% is asked about first. How it was estimated: %s.\n\n", f.Estimate, f.WeekPct, fanoutAskPct, f.Basis)
	b.WriteString("Choose Start to run them. Choose Don't start (or leave this) and nothing runs: the tasks stay in the Backlog, and the agent hears your answer.")
	return RequestInput{
		Kind:    RequestQuestion,
		Title:   fmt.Sprintf("Run %d agent sessions estimated at $%.0f (%.1f%% of a week)?", len(f.Items), math.Ceil(f.Estimate), f.WeekPct),
		Body:    b.String(),
		Project: p.Key,
		Questions: []Question{{Question: "Start these sessions?", Choices: []Choice{
			{Label: fanoutStart, Description: fmt.Sprintf("Run them, %d at a time, each stopping at its budget", f.Parallel)},
			{Label: fanoutDontStart, Description: "Nothing runs; the tasks stay in the Backlog"},
		}}},
		Followup: "never",
	}
}

const (
	fanoutStart     = "Start"
	fanoutDontStart = "Don't start"
)

const fanoutCols = `f.id, p.key, f.state, f.parallel, f.machine, f.dir, f.base, f.agent, f.estimate_usd, f.week_pct, f.basis,
	coalesce('R-' || r.number, ''), f.created_at, f.ended_at, f.user_id, coalesce(f.session_id, 0), coalesce(f.request_id, 0), f.told`

const fanoutFrom = ` FROM fanouts f JOIN projects p ON p.id = f.project_id LEFT JOIN requests r ON r.id = f.request_id`

func scanFanout(sc interface{ Scan(...any) error }) (*Fanout, error) {
	var f Fanout
	var at int64
	var ended sql.NullInt64
	if err := sc.Scan(&f.ID, &f.Project, &f.State, &f.Parallel, &f.Machine, &f.Dir, &f.Base, &f.Agent, &f.Estimate, &f.WeekPct, &f.Basis,
		&f.Request, &at, &ended, &f.userID, &f.sessionID, &f.requestID, &f.told); err != nil {
		return nil, err
	}
	f.Created = msTime(at)
	if ended.Valid {
		t := msTime(ended.Int64)
		f.Ended = &t
	}
	f.AskOver = fanoutAskPct
	return &f, nil
}

// fanoutByID loads a fan-out with its packages.
func (s *Service) fanoutByID(ctx context.Context, q queryer, id int64) (*Fanout, error) {
	f, err := scanFanout(q.QueryRowContext(ctx, `SELECT `+fanoutCols+fanoutFrom+` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("fan-out #" + itoa64(id))
	}
	if err != nil {
		return nil, err
	}
	return f, s.loadFanoutItems(ctx, q, f)
}

func (s *Service) loadFanoutItems(ctx context.Context, q queryer, f *Fanout) error {
	rows, err := q.QueryContext(ctx, `SELECT it.id, it.pos, it.issue_id, p.key || '-' || i.number, i.title, st.name, it.state, it.model, it.budget_usd, it.estimate_usd,
			it.spent_usd, it.branch, it.attach_id, it.note, it.told, it.started_at, it.ended_at,
			coalesce((SELECT l.url FROM external_links l WHERE l.issue_id = it.issue_id AND l.kind = 'pull' ORDER BY l.id DESC LIMIT 1), ''),
			coalesce((SELECT l.number FROM external_links l WHERE l.issue_id = it.issue_id AND l.kind = 'pull' ORDER BY l.id DESC LIMIT 1), 0)
		FROM fanout_items it JOIN issues i ON i.id = it.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE it.fanout_id = ? ORDER BY it.pos`, f.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	f.Items, f.Spent = nil, 0
	for rows.Next() {
		var it FanoutItem
		var started, ended sql.NullInt64
		if err := rows.Scan(&it.ID, &it.Pos, &it.issueID, &it.Key, &it.Title, &it.Status, &it.State, &it.Model, &it.Budget, &it.Estimate,
			&it.Spent, &it.Branch, &it.AttachID, &it.Note, &it.told, &started, &ended, &it.PR, &it.PRNumber); err != nil {
			return err
		}
		if started.Valid {
			t := msTime(started.Int64)
			it.Started = &t
		}
		if ended.Valid {
			t := msTime(ended.Int64)
			it.Ended = &t
		}
		f.Spent += it.Spent
		f.Items = append(f.Items, &it)
	}
	f.Spent = math.Round(f.Spent*100) / 100
	return rows.Err()
}

// ownFanout loads a fan-out the actor owns.
func (s *Service) ownFanout(ctx context.Context, q queryer, actor *Actor, id int64) (*Fanout, error) {
	f, err := s.fanoutByID(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if f.userID != actor.id() {
		return nil, notFound("fan-out #" + itoa64(id))
	}
	return f, nil
}

// GetFanout is one of the actor's fan-outs.
func (s *Service) GetFanout(ctx context.Context, actor *Actor, id int64) (*Fanout, error) {
	if err := actor.need(PermSessionsRead); err != nil {
		return nil, err
	}
	return s.ownFanout(ctx, s.db.R, actor, id)
}

// ListFanouts is the actor's latest fan-outs, newest first; live only
// unless all.
func (s *Service) ListFanouts(ctx context.Context, actor *Actor, all bool) ([]*Fanout, error) {
	if err := actor.need(PermSessionsRead); err != nil {
		return nil, err
	}
	where := ` WHERE f.user_id = ?`
	if !all {
		where += ` AND f.state IN ('asked', 'running')`
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+fanoutCols+fanoutFrom+where+` ORDER BY f.id DESC LIMIT 20`, actor.id())
	if err != nil {
		return nil, err
	}
	var out []*Fanout
	for rows.Next() {
		f, err := scanFanout(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range out {
		if err := s.loadFanoutItems(ctx, s.db.R, f); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []*Fanout{}
	}
	return out, nil
}

// fanoutOf is the live fan-out a task is a queued or starting package of,
// so the general queue leaves it alone; 0 when none.
func (s *Service) fanoutOf(ctx context.Context, issueID int64) int64 {
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT f.id FROM fanout_items it JOIN fanouts f ON f.id = it.fanout_id
		WHERE it.issue_id = ? AND it.state IN ('queued', 'starting') AND f.state IN ('asked', 'running') LIMIT 1`, issueID).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return -1
	}
	return id
}

// settleAsk applies the person's answer to an asked fan-out: Start runs it,
// anything else (Don't start, a dismissal) declines it. Returns the new
// state, or "" while the question is open.
func (s *Service) settleAsk(ctx context.Context, t *tx, f *Fanout) (string, error) {
	if f.State != FanoutAsked {
		return f.State, nil
	}
	state := FanoutDeclined
	if f.requestID != 0 {
		var rstate, payload string
		err := t.QueryRowContext(ctx, `SELECT state, payload FROM requests WHERE id = ?`, f.requestID).Scan(&rstate, &payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if err == nil {
			if rstate == RequestOpen {
				return "", nil
			}
			var pay struct {
				Answers []Answer `json:"answers"`
			}
			_ = json.Unmarshal([]byte(payload), &pay)
			if rstate == RequestAnswered && len(pay.Answers) > 0 && slices.Contains(pay.Answers[0].Choices, fanoutStart) {
				state = FanoutRunning
			}
		}
	}
	now := s.nowMS()
	if state == FanoutDeclined {
		if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = 'not started: the person said no', ended_at = ? WHERE fanout_id = ? AND state = ?`,
			ItemCancelled, now, f.ID, ItemQueued); err != nil {
			return "", err
		}
		if _, err := t.ExecContext(ctx, `UPDATE fanouts SET state = ?, ended_at = ? WHERE id = ?`, state, now, f.ID); err != nil {
			return "", err
		}
		t.emit(&Event{Kind: EvFanoutFinished, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "state": state}})
	} else if _, err := t.ExecContext(ctx, `UPDATE fanouts SET state = ? WHERE id = ?`, state, f.ID); err != nil {
		return "", err
	}
	f.State = state
	return state, nil
}

// ClaimFanout reserves the packages to start now on machine, as many as
// the fan-out's free slots, in plan order: each is leased to the fan-out's
// agent like a hand-off, for its session's wp next KEY to take. Packages
// that can't start yet (screen pending, on hold, blocked) wait; the claim
// says why when nothing starts.
func (s *Service) ClaimFanout(ctx context.Context, actor *Actor, id int64, machine string) (*FanoutClaim, error) {
	if err := actor.need(PermSessionsStart); err != nil {
		return nil, err
	}
	var claim *FanoutClaim
	var screen []int64
	err := s.write(ctx, actor, func(t *tx) error {
		claim, screen = &FanoutClaim{}, nil
		f, err := s.ownFanout(ctx, t, actor, id)
		if err != nil {
			return err
		}
		claim.Fanout = f
		if f.State == FanoutAsked {
			state, err := s.settleAsk(ctx, t, f)
			if err != nil {
				return err
			}
			if state == "" {
				claim.Waiting = fmt.Sprintf("Waiting for the person's answer to %s (over %.0f%% of a week). Waypoint tells you when they answer.", f.Request, fanoutAskPct)
				return nil
			}
		}
		if f.State != FanoutRunning {
			claim.Waiting = "Fan-out #" + itoa64(f.ID) + " is " + f.State + "; nothing more starts."
			return nil
		}
		if machine != f.Machine {
			return invalid("fan-out #%d starts its sessions on %s, where its repository is; run wp fanout start %d there", f.ID, f.Machine, f.ID)
		}
		now := s.nowMS()
		free := f.Parallel
		for _, it := range f.Items {
			if it.State == ItemStarting || it.State == ItemRunning {
				free--
			}
		}
		var why []string
		for pass := 0; pass < 2; pass++ {
			for _, it := range f.Items {
				if pass == 0 && it.State != ItemQueued || pass == 1 && it.State != ItemFailed {
					continue
				}
				if free <= 0 {
					break
				}
				if pass == 1 {
					if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, ended_at = NULL WHERE id = ?`, ItemQueued, it.ID); err != nil {
						return err
					}
					it.State = ItemQueued
				}
				wait, held, err := s.fanoutItemWait(ctx, t, f, it, now)
				if err != nil {
					return err
				}
				switch {
				case wait == ItemCancelled:
					if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = 'closed before it started', ended_at = ? WHERE id = ?`, ItemCancelled, now, it.ID); err != nil {
						return err
					}
					it.State = ItemCancelled
					continue
				case wait != "":
					if it.Note != wait {
						if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET note = ? WHERE id = ?`, wait, it.ID); err != nil {
							return err
						}
						it.Note = wait
					}
					if !held && strings.Contains(wait, "safety screen") {
						screen = append(screen, it.issueID)
					}
					why = append(why, it.Key+": "+wait)
					continue
				}
				res, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = ?, claimed_until = ?, claimed_at = ?
				WHERE id = ? AND (claimed_by = '' OR claimed_until IS NULL OR claimed_until < ? OR claimed_by = ?)`,
					f.Agent, now+HandoffReserve.Milliseconds(), now, it.issueID, now, f.Agent)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					why = append(why, it.Key+": being worked on by another agent")
					continue
				}
				if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = '', started_at = ?, attach_id = '' WHERE id = ?`, ItemStarting, now, it.ID); err != nil {
					return err
				}
				it.State, it.Note = ItemStarting, ""
				claim.Start = append(claim.Start, it)
				free--
			}
		}
		if err := s.maybeFinishFanout(ctx, t, f); err != nil {
			return err
		}
		if len(claim.Start) == 0 && claim.Waiting == "" {
			switch {
			case f.State != FanoutRunning:
				claim.Waiting = "Every package has ended."
			case free <= 0:
				claim.Waiting = fmt.Sprintf("All %d slots are running; the next package starts when one of them finishes its task.", f.Parallel)
			case len(why) > 0:
				claim.Waiting = "Nothing can start yet: " + strings.Join(why, "; ")
			default:
				claim.Waiting = "Nothing is queued."
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range screen {
		s.queueScreenIf(id, false)
	}
	return claim, nil
}

// fanoutItemWait says why a queued package can't start now ("" when it
// can, ItemCancelled when its task closed); held: the safety screen holds
// it for a person.
func (s *Service) fanoutItemWait(ctx context.Context, q queryer, f *Fanout, it *FanoutItem, now int64) (string, bool, error) {
	i, err := s.issueByID(ctx, q, it.issueID, false)
	if err != nil {
		return "", false, err
	}
	switch {
	case i.Closed():
		return ItemCancelled, false, nil
	case i.Draft:
		return "a draft: a person must publish it", false, nil
	case i.Hold.on(s.now()):
		return "on hold", false, nil
	case s.agentsPausedIn(ctx, i.ProjectID):
		return "agents are paused in " + i.ProjectKey, false, nil
	case s.isBlocked(ctx, i.ID):
		return "blocked by an open task", false, nil
	case i.ClaimedBy != "" && i.ClaimedBy != f.Agent && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now:
		return "being worked on by " + i.ClaimedBy, false, nil
	}
	if d, held := s.safetyWait(ctx, q, i); d != "" {
		if held {
			return d, true, nil
		}
		return "waiting for the safety screen", false, nil
	}
	return "", false, nil
}

// maybeFinishFanout ends a running fan-out once no package is queued,
// starting or running.
func (s *Service) maybeFinishFanout(ctx context.Context, t *tx, f *Fanout) error {
	if f.State != FanoutRunning && f.State != FanoutStopped {
		return nil
	}
	live := false
	failedToo := 0
	if f.State == FanoutRunning {
		failedToo = 1
	}
	if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fanout_items WHERE fanout_id = ? AND (state IN ('queued', 'starting', 'running') OR (state = 'failed' AND ?)))`, f.ID, failedToo).Scan(&live); err != nil {
		return err
	}
	if live {
		return nil
	}
	state := FanoutFinished
	if f.State == FanoutStopped {
		state = FanoutStopped
	}
	res, err := t.ExecContext(ctx, `UPDATE fanouts SET state = ?, ended_at = ? WHERE id = ? AND ended_at IS NULL`, state, s.nowMS(), f.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		f.State = state
		t.emit(&Event{Kind: EvFanoutFinished, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "state": state}})
	}
	return nil
}

// fanoutItem finds a package by its task's key.
func (f *Fanout) item(key string) *FanoutItem {
	for _, it := range f.Items {
		if strings.EqualFold(it.Key, key) {
			return it
		}
	}
	return nil
}

// FanoutItemStarted records that a package's session started: attach is
// its Claude Code background session id.
func (s *Service) FanoutItemStarted(ctx context.Context, actor *Actor, id int64, key, attach string) error {
	if err := actor.need(PermSessionsStart); err != nil {
		return err
	}
	if len(attach) > 100 {
		return invalid("attach id is too long")
	}
	if attach != "" && !ValidAttachID(attach) {
		return invalid("attach id is not a Claude Code background session id (6 to 36 hex digits)")
	}
	return s.write(ctx, actor, func(t *tx) error {
		f, err := s.ownFanout(ctx, t, actor, id)
		if err != nil {
			return err
		}
		it := f.item(key)
		if it == nil {
			return notFound(key + " in fan-out #" + itoa64(id))
		}
		if it.State != ItemStarting {
			return nil
		}
		_, err = t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, attach_id = ? WHERE id = ?`, ItemRunning, attach, it.ID)
		return err
	})
}

// FanoutItemFailed records that a package's session couldn't start: its
// lease goes back, and the coordinator hears why.
func (s *Service) FanoutItemFailed(ctx context.Context, actor *Actor, id int64, key, note string) error {
	if err := actor.need(PermSessionsStart); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		f, err := s.ownFanout(ctx, t, actor, id)
		if err != nil {
			return err
		}
		it := f.item(key)
		if it == nil {
			return notFound(key + " in fan-out #" + itoa64(id))
		}
		if it.State != ItemStarting {
			return nil
		}
		return s.failFanoutItem(ctx, t, f, it, excerpt(strings.TrimSpace(note), 500))
	})
}

func (s *Service) failFanoutItem(ctx context.Context, t *tx, f *Fanout, it *FanoutItem, note string) error {
	now := s.nowMS()
	if note == "" {
		note = "its session didn't start"
	}
	if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = ?, ended_at = ? WHERE id = ?`, ItemFailed, note, now, it.ID); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?
		AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = issues.id AND r.ended_at IS NULL)`, it.issueID, f.Agent); err != nil {
		return err
	}
	t.emit(&Event{Kind: EvFanoutItemEnded, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "item": it.ID, "key": it.Key, "outcome": ItemFailed, "state": ItemFailed}})
	return s.maybeFinishFanout(ctx, t, f)
}

// releaseFanoutTask lets go of a stopped package's task: it isn't retried,
// so the fan-out's hold would only keep everyone else off it until the lease
// ran out (WP-579). Its run ends as released; the stranded sweep puts it
// back in To Do once it sits idle.
func (s *Service) releaseFanoutTask(ctx context.Context, t *tx, f *Fanout, it *FanoutItem) error {
	res, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, it.issueID, f.Agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := s.endRun(ctx, t, it.issueID, f.Agent, "released", nil); err != nil {
		return err
	}
	return nil
}

// EndFanoutItem records how a package's session ended: done (wp done),
// budget (stopped at its budget) or ended (it ended without wp done), and
// what it spent. Saying it twice changes only the spend.
func (s *Service) EndFanoutItem(ctx context.Context, actor *Actor, id int64, key, outcome string, spent float64, note string) error {
	if err := actor.need(PermSessionsStart); err != nil {
		return err
	}
	if spent < 0 || spent > 100000 || math.IsNaN(spent) {
		return invalid("spent is in USD, from 0")
	}
	state := ItemStopped
	switch outcome {
	case OutcomeDone:
		state = ItemDone
		if note == "" {
			note = "done"
		}
	case OutcomeBudget:
		if note == "" {
			note = "stopped at its budget"
		}
	case OutcomeEnded:
		if note == "" {
			note = "its session ended without finishing"
		}
	default:
		return invalid("outcome is done, budget or ended")
	}
	note = excerpt(strings.TrimSpace(note), 500)
	return s.write(ctx, actor, func(t *tx) error {
		f, err := s.ownFanout(ctx, t, actor, id)
		if err != nil {
			return err
		}
		it := f.item(key)
		if it == nil {
			return notFound(key + " in fan-out #" + itoa64(id))
		}
		now := s.nowMS()
		switch it.State {
		case ItemStarting, ItemRunning:
		case ItemStopped:
			if state == ItemDone {
				if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = ?, spent_usd = max(spent_usd, ?) WHERE id = ?`, ItemDone, note, spent, it.ID); err != nil {
					return err
				}
				t.emit(&Event{Kind: EvFanoutItemEnded, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "item": it.ID, "key": it.Key, "outcome": outcome,
					"state": ItemDone, "spent": max(spent, it.Spent), "budget": it.Budget, "model": it.Model}})
				return nil
			}
			fallthrough
		default:
			_, err := t.ExecContext(ctx, `UPDATE fanout_items SET spent_usd = max(spent_usd, ?) WHERE id = ?`, spent, it.ID)
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = ?, spent_usd = max(spent_usd, ?), ended_at = ? WHERE id = ?`,
			state, note, spent, now, it.ID); err != nil {
			return err
		}
		if state == ItemStopped {
			if err := s.releaseFanoutTask(ctx, t, f, it); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvFanoutItemEnded, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "item": it.ID, "key": it.Key, "outcome": outcome,
			"state": state, "spent": max(spent, it.Spent), "budget": it.Budget, "model": it.Model}})
		return s.maybeFinishFanout(ctx, t, f)
	})
}

// StopFanout starts nothing more: queued packages are cancelled. Running
// sessions go on (the CLI stops them with --now).
func (s *Service) StopFanout(ctx context.Context, actor *Actor, id int64) (*Fanout, error) {
	if err := actor.need(PermSessionsStart); err != nil {
		return nil, err
	}
	err := s.write(ctx, actor, func(t *tx) error {
		f, err := s.ownFanout(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if f.State != FanoutAsked && f.State != FanoutRunning {
			return nil
		}
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = 'wp fanout stop', ended_at = ? WHERE fanout_id = ? AND state = ?`,
			ItemCancelled, now, f.ID, ItemQueued); err != nil {
			return err
		}
		if f.State == FanoutAsked && f.requestID != 0 {
			if _, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, RequestWithdrawn, now, f.requestID, RequestOpen); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE fanouts SET state = ? WHERE id = ?`, FanoutStopped, f.ID); err != nil {
			return err
		}
		f.State = FanoutStopped
		return s.maybeFinishFanout(ctx, t, f)
	})
	if err != nil {
		return nil, err
	}
	return s.GetFanout(ctx, actor, id)
}

// onFanoutEvent tells a fan-out's coordinator how its packages end, and
// settles an asked fan-out when the person answers.
func (s *Service) onFanoutEvent(ctx context.Context, e *Event) {
	switch e.Kind {
	case EvFanoutCreated:
		if f, err := s.fanoutByID(ctx, s.db.R, eventInt(e, "fanout")); err == nil {
			for _, it := range f.Items {
				s.queueScreenIf(it.issueID, false)
			}
		}
	case EvRequestUpdated:
		s.fanoutAnswered(ctx, e)
	case EvFanoutItemEnded:
		s.tellFanoutItem(ctx, e)
	case EvFanoutFinished:
		s.tellFanoutFinished(ctx, e)
	}
}

func eventInt(e *Event, key string) int64 {
	switch v := e.Data[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// fanoutAnswered settles the fan-out a request asked about and tells its
// coordinator what to do.
func (s *Service) fanoutAnswered(ctx context.Context, e *Event) {
	rid := eventInt(e, "request")
	if rid == 0 {
		return
	}
	var fid int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT id FROM fanouts WHERE request_id = ? AND state = ?`, rid, FanoutAsked).Scan(&fid); err != nil {
		return
	}
	var f *Fanout
	var state string
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		var err error
		if f, err = s.fanoutByID(ctx, t, fid); err != nil {
			return err
		}
		state, err = s.settleAsk(ctx, t, f)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "settle a fan-out's ask", "fanout", fid, "err", err)
		return
	}
	switch state {
	case FanoutRunning:
		s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:answer", f.ID), false,
			fmt.Sprintf("%s was answered Start: run `wp fanout start %d` to start fan-out #%d's first %d sessions.", f.Request, f.ID, f.ID, min(f.Parallel, len(f.Items))))
	case FanoutDeclined:
		s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:answer", f.ID), true,
			fmt.Sprintf("%s was answered Don't start: fan-out #%d won't run; its tasks stay in the Backlog.", f.Request, f.ID))
	}
}

// tellCoordinator queues news for the fan-out's coordinator session, once
// per key; when it has ended, active news goes to the person instead.
func (s *Service) tellCoordinator(ctx context.Context, f *Fanout, key string, passive bool, body string) {
	alive := false
	if f.sessionID != 0 {
		_ = s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, f.sessionID).Scan(&alive)
	}
	if alive {
		if _, err := s.queueMessage(ctx, f.sessionID, body, key, messageFrom{}, passive); err != nil {
			s.log.WarnContext(ctx, "tell a fan-out's coordinator", "fanout", f.ID, "err", err)
		}
		return
	}
	if passive {
		return
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE fanouts SET told = ? WHERE id = ? AND told != ?`, key, f.ID, key)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.notify(ctx, f.userID, nil, NotifyAutomated, "Fan-out #"+itoa64(f.ID), body)
	}
}

// fanoutProgress says how many packages are where: "2 of 5 done, 1 running, 2 queued".
func fanoutProgress(f *Fanout) string {
	n := f.Counts()
	parts := []string{fmt.Sprintf("%d of %d done", n[ItemDone], len(f.Items))}
	if r := n[ItemRunning] + n[ItemStarting]; r > 0 {
		parts = append(parts, fmt.Sprintf("%d running", r))
	}
	for _, st := range []string{ItemQueued, ItemStopped, ItemFailed, ItemCancelled} {
		if n[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[st], st))
		}
	}
	return strings.Join(parts, ", ")
}

// itemLine is one package in a line: key, how it went, PR, spend.
func itemLine(it *FanoutItem) string {
	line := it.Key + " " + it.State
	if it.Note != "" && it.Note != it.State {
		line += " (" + it.Note + ")"
	}
	if it.PRNumber > 0 {
		line += fmt.Sprintf(", PR #%d", it.PRNumber)
	}
	if it.State != ItemQueued && it.State != ItemCancelled {
		line += fmt.Sprintf(", %s $%.2f of $%.2f", it.Model, it.Spent, it.Budget)
	}
	return line
}

func (s *Service) tellFanoutItem(ctx context.Context, e *Event) {
	f, err := s.fanoutByID(ctx, s.db.R, eventInt(e, "fanout"))
	if err != nil {
		return
	}
	key, _ := e.Data["key"].(string)
	it := f.item(key)
	if it == nil {
		return
	}
	body := fmt.Sprintf("Fan-out #%d: %s. %s.", f.ID, itemLine(it), fanoutProgress(f))
	if it.State == ItemFailed {
		body += fmt.Sprintf(" Run `wp fanout start %d` to try again.", f.ID)
	}
	outcome, _ := e.Data["outcome"].(string)
	s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:item:%d:%s", f.ID, it.ID, outcome), true, body)
}

func (s *Service) tellFanoutFinished(ctx context.Context, e *Event) {
	f, err := s.fanoutByID(ctx, s.db.R, eventInt(e, "fanout"))
	if err != nil {
		return
	}
	if f.State == FanoutDeclined {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Fan-out #%d %s: %s; spent $%.2f of an estimated $%.2f.", f.ID, f.State, fanoutProgress(f), f.Spent, f.Estimate)
	for _, it := range f.Items {
		b.WriteString("\n- " + itemLine(it))
	}
	s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:finished", f.ID), false, b.String())
}

// CheckFanouts (every minute) gives up on packages whose sessions never
// started or ended unseen, screens packages waiting for the screen, tells
// the coordinator once about packages held for a person, and nudges it when
// nothing runs and something could start.
func (s *Service) CheckFanouts(ctx context.Context) {
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM fanouts WHERE state IN (?, ?) AND ended_at IS NULL`, FanoutRunning, FanoutStopped)
	if err != nil {
		s.log.WarnContext(ctx, "fan-out sweep", "err", err)
		return
	}
	for _, id := range ids {
		if err := s.checkFanout(ctx, id); err != nil {
			s.log.WarnContext(ctx, "fan-out sweep", "fanout", id, "err", err)
		}
	}
}

func (s *Service) checkFanout(ctx context.Context, id int64) error {
	now := s.nowMS()
	var held []*FanoutItem
	var screen []int64
	var f *Fanout
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		held, screen = nil, nil
		var err error
		if f, err = s.fanoutByID(ctx, t, id); err != nil {
			return err
		}
		for _, it := range f.Items {
			switch it.State {
			case ItemStarting:
				if it.Started != nil && now-it.Started.UnixMilli() > HandoffReserve.Milliseconds() {
					if err := s.failFanoutItem(ctx, t, f, it, "its session didn't start within 10 minutes"); err != nil {
						return err
					}
					it.State = ItemFailed
				}
			case ItemRunning:
				if gone, err := s.fanoutSessionGone(ctx, t, f, it, now); err != nil {
					return err
				} else if gone {
					if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = 'its session ended without finishing', ended_at = ? WHERE id = ?`, ItemStopped, now, it.ID); err != nil {
						return err
					}
					if err := s.releaseFanoutTask(ctx, t, f, it); err != nil {
						return err
					}
					it.State = ItemStopped
					t.emit(&Event{Kind: EvFanoutItemEnded, UserID: f.userID, Data: map[string]any{"fanout": f.ID, "item": it.ID, "key": it.Key, "outcome": OutcomeEnded, "state": ItemStopped}})
					if err := s.maybeFinishFanout(ctx, t, f); err != nil {
						return err
					}
				}
			case ItemQueued:
				wait, isHeld, err := s.fanoutItemWait(ctx, t, f, it, now)
				if err != nil {
					return err
				}
				switch {
				case wait == ItemCancelled:
					if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET state = ?, note = 'closed before it started', ended_at = ? WHERE id = ?`, ItemCancelled, now, it.ID); err != nil {
						return err
					}
					it.State = ItemCancelled
					if err := s.maybeFinishFanout(ctx, t, f); err != nil {
						return err
					}
				case isHeld && !it.told:
					if _, err := t.ExecContext(ctx, `UPDATE fanout_items SET told = 1, note = ? WHERE id = ?`, wait, it.ID); err != nil {
						return err
					}
					it.Note = wait
					held = append(held, it)
				case wait == "waiting for the safety screen" && s.screenDue(ctx, t, it.issueID, now):
					screen = append(screen, it.issueID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, it := range held {
		s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:held:%d", f.ID, it.ID), false,
			fmt.Sprintf("Fan-out #%d: %s won't start until a person reviews it. %s", f.ID, it.Key, it.Note))
	}
	if len(screen) > 0 && s.screenAllowance(ctx) > 0 {
		for _, id := range screen {
			s.queueScreenIf(id, false)
		}
	}
	return s.nudgeIdleFanout(ctx, f, now)
}

// screenDue: an issue waiting for the screen has no screen in flight or
// backing off after a failure.
func (s *Service) screenDue(ctx context.Context, q queryer, issueID, now int64) bool {
	var verdict string
	var attempts int
	var at int64
	err := q.QueryRowContext(ctx, `SELECT verdict, attempts, updated_at FROM task_safety WHERE issue_id = ?`, issueID).Scan(&verdict, &attempts, &at)
	if err != nil {
		return true
	}
	return verdict != SafetyPending || now-at > screenBackoff(max(attempts, 1)).Milliseconds()
}

// fanoutSessionGone: a running package's session ended, or Waypoint hasn't
// heard from one for fanoutLostAfter.
func (s *Service) fanoutSessionGone(ctx context.Context, q queryer, f *Fanout, it *FanoutItem, now int64) (bool, error) {
	if it.Started == nil {
		return false, nil
	}
	started := it.Started.UnixMilli()
	var live, ended bool
	err := q.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM agent_sessions WHERE user_id = ? AND kind = ? AND ended_at IS NULL AND last_seen_at > ? AND started_at >= ?
				AND (',' || task_keys || ',') LIKE ?),
			EXISTS(SELECT 1 FROM agent_sessions WHERE user_id = ? AND kind = ? AND ended_at IS NOT NULL AND started_at >= ?
				AND (',' || task_keys || ',') LIKE ?)`,
		f.userID, SessionTask, now-fanoutLostAfter.Milliseconds(), started-time.Minute.Milliseconds(), "%,"+it.Key+",%",
		f.userID, SessionTask, started-time.Minute.Milliseconds(), "%,"+it.Key+",%").Scan(&live, &ended)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	return ended || now-started > fanoutLostAfter.Milliseconds(), nil
}

// nudgeIdleFanout tells the coordinator, once per such moment, to start
// packages when none runs and one could start now (a screen cleared, a hold
// ended, a session failed).
func (s *Service) nudgeIdleFanout(ctx context.Context, f *Fanout, now int64) error {
	if f.State != FanoutRunning {
		return nil
	}
	n := f.Counts()
	if n[ItemQueued] == 0 || n[ItemRunning]+n[ItemStarting] > 0 {
		return nil
	}
	ready := 0
	for _, it := range f.Items {
		if it.State != ItemQueued {
			continue
		}
		if wait, _, err := s.fanoutItemWait(ctx, s.db.R, f, it, now); err == nil && wait == "" {
			ready++
		}
	}
	if ready == 0 {
		return nil
	}
	ended := len(f.Items) - n[ItemQueued]
	s.tellCoordinator(ctx, f, fmt.Sprintf("fanout:%d:idle:%d", f.ID, ended), false,
		fmt.Sprintf("Fan-out #%d has nothing running and %d package(s) ready: run `wp fanout start %d` on %s.", f.ID, ready, f.ID, f.Machine))
	return nil
}
