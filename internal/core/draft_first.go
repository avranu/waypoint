package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	wipPrefix = "WIP: "
	// A ready pull request whose full run hasn't finished this long after it
	// was marked ready is handed to a person: this is the default, for a
	// repo whose measured full runs can't say better (fullRunStallAfter,
	// WP-317).
	fullRunStall = 45 * time.Minute
	// reasonDraft is the readiness reason of a draft.
	reasonDraft = "still a draft"
)

// cleanTitle drops the forge's "WIP:" prefix: Waypoint's records keep the
// clean title, whether the pull request is a draft is its state.
func stripWIP(t string) string {
	t = strings.TrimSpace(t)
	for {
		l := strings.ToLower(t)
		switch {
		case strings.HasPrefix(l, "wip:"):
			t = strings.TrimSpace(t[4:])
		case strings.HasPrefix(l, "[wip]"):
			t = strings.TrimSpace(t[5:])
		default:
			return t
		}
	}
}

// ForgePREditor changes a pull request's title (httpForgeClient has it).
type ForgePREditor interface {
	EditPRTitle(ctx context.Context, f *Forge, repo string, number int, title string) error
	EditPRBody(ctx context.Context, f *Forge, repo string, number int, body string) error
}

func (c *httpForgeClient) EditPRTitle(ctx context.Context, f *Forge, repo string, number int, title string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), map[string]string{"title": title}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"title": title}, nil)
}

// draftFirstForge: only Forgejo's title prefix is handled.
func draftFirstForge(kind string) bool { return kind == "forgejo" }

// fullRunTally is how far a pull request marked ready is through its full
// run: the checks the edit reruns, how many of them are done, whether one
// of those really ran, and the ones whose status is still a real success
// from before the ready mark (a draft-only job the full run skipped, or a
// job whose new status hasn't come yet).
type fullRunTally struct {
	names, stale []string
	done         int
	real         bool
}

// tallyFullRun: false when pr isn't one Waypoint marked ready at this head,
// or the head had no checks to run again.
func tallyFullRun(pr *PullRequest, checks []PRCheck) (fullRunTally, bool) {
	var t fullRunTally
	if !pr.WipAuto || pr.State != "open" || pr.ReadyAt == nil || !sameSHA(pr.readySHA, pr.HeadSHA) {
		return t, false
	}
	if pr.readyStatusID == 0 && len(pr.readyChecks) == 0 {
		return t, false
	}
	have := map[string]PRCheck{}
	for _, c := range checks {
		have[c.Name] = c
	}
	for _, n := range pr.readyChecks {
		if rerunByEdit(n) {
			t.names = append(t.names, n)
		}
	}
	for _, c := range checks {
		if rerunByEdit(c.Name) && !slices.Contains(t.names, c.Name) {
			t.names = append(t.names, c.Name)
		}
	}
	if len(t.names) == 0 {
		return t, false
	}
	for _, n := range t.names {
		c, ok := have[n]
		switch {
		case !ok || c.State != CheckSuccess:
		case c.StatusID > pr.readyStatusID:
			t.done++
			t.real = t.real || !skippedPRCheck(c)
		case skippedPRCheck(c):
			t.done++
		case slices.Contains(pr.readySkipped, n):
			t.done++
		default:
			t.stale = append(t.stale, n)
		}
	}
	return t, true
}

// fullRunReasons says why a pull request marked ready doesn't have its full
// run yet: nothing when it isn't one Waypoint marked ready at this head.
func fullRunReasons(pr *PullRequest, checks []PRCheck) []string {
	t, ok := tallyFullRun(pr, checks)
	if !ok || t.done == len(t.names) && t.real {
		return nil
	}
	return []string{fmt.Sprintf("full run hasn't finished since it was marked ready (%d of %d checks)", t.done, len(t.names))}
}

// rerunByEdit: the check is a job of a workflow run for the pull request
// event, which the edit that marks it ready runs again. Others (an external
// scanner of the head, a push run) post nothing new for an edit (WP-174).
// fullRunPassed: pr, a draft now, was marked ready at its head before, and
// every check the edit reruns has passed since then, going by checks (the
// head's statuses).
func fullRunPassed(pr *PullRequest, checks []CommitCheck, now time.Time) bool {
	if pr.readyStatusID == 0 || !sameSHA(pr.readySHA, pr.HeadSHA) {
		return false
	}
	was := *pr
	was.State, was.ReadyAt = "open", &now
	pcs := make([]PRCheck, 0, len(checks))
	ran := false
	for _, c := range dropSuperseded(newestPerContext(checks)) {
		pcs = append(pcs, PRCheck{Name: c.Name, State: c.State, Description: c.Description, StatusID: c.ID})
		ran = ran || rerunByEdit(c.Name)
	}
	return ran && len(fullRunReasons(&was, pcs)) == 0
}

func rerunByEdit(name string) bool {
	return strings.HasSuffix(name, "(pull_request)") || strings.HasSuffix(name, "(pull_request_target)")
}

func skippedPRCheck(c PRCheck) bool {
	return skippedCheck(CommitCheck{State: c.State, Description: c.Description})
}

func isFullRunReason(r string) bool {
	return strings.HasPrefix(r, "full run hasn't finished")
}

// prStage: "draft", "ready (full run pending)" or "" for a pull request
// Waypoint doesn't hold as a draft.
func prStage(pr *PullRequest, checks []PRCheck) string {
	switch {
	case !pr.WipAuto:
		return ""
	case pr.State == "draft":
		return "draft"
	case pr.State == "open" && len(fullRunReasons(pr, checks)) > 0:
		return "ready (full run pending)"
	}
	return ""
}

// markReady drops "WIP: " from a draft's title on the forge and records
// when, at which head and what the head's statuses were, so the full run can
// be told from the draft tier.
func (s *Service) markReady(ctx context.Context, pr *PullRequest) error {
	return s.markReadyOn(ctx, pr, nil)
}

// markReadyOn is markReady; with a stack (WP-404) whose run passed on pr's
// head merged onto its base, that run is its full run: no checks are
// recorded as the draft tier's, so none newer have to pass, and the run for
// the edit skips its checks (the head has waypoint/stack).
func (s *Service) markReadyOn(ctx context.Context, pr *PullRequest, st *queueStack) error {
	if st != nil && pr.State == "open" {
		return s.write(ctx, SystemActor(), func(t *tx) error {
			_, err := t.ExecContext(ctx, `UPDATE pull_requests SET ready_at = coalesce(ready_at, ?), ready_sha = ?, ready_status_id = 0, ready_checks = '[]', ready_skipped = '[]', ready_cand = ? WHERE id = ? AND state = 'open'`,
				s.nowMS(), pr.HeadSHA, st.ID, pr.ID)
			return err
		})
	}
	ed, ok := s.Forges.(ForgePREditor)
	cl, ok2 := s.Forges.(ForgeChecks)
	if !ok || !ok2 {
		return fmt.Errorf("this forge client can't edit titles")
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	onEdit, err := s.ciRunsOnEdit(ctx, f, pr.Repo, pr.HeadSHA)
	if err != nil {
		return err
	}
	var all []CommitCheck
	if onEdit {
		if all, err = cl.CommitChecks(ctx, f, pr.Repo, pr.HeadSHA); err != nil {
			return err
		}
	}
	if onEdit && fullRunPassed(pr, all, s.now()) || st != nil {
		all = nil
	}
	var maxID int64
	names := []string{}
	for _, c := range dropSuperseded(newestPerContext(all)) {
		if ownCheck(c.Name) {
			continue
		}
		names = append(names, c.Name)
		maxID = max(maxID, c.ID)
	}
	if len(names) > 0 && maxID == 0 {
		return fmt.Errorf("the forge gave no status ids, so a full run can't be told from the draft tier")
	}
	nm, _ := json.Marshal(names)
	now := s.nowMS()
	var cand int64
	if st != nil {
		cand = st.ID
	}
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET ready_at = ?, ready_sha = ?, ready_status_id = ?, ready_checks = ?, ready_skipped = '[]', ready_cand = ? WHERE id = ? AND state = 'draft'`,
			now, pr.HeadSHA, maxID, string(nm), cand, pr.ID)
		return err
	}); err != nil {
		return err
	}
	if err := ed.EditPRTitle(ctx, f, pr.Repo, pr.Number, stripWIP(pr.Title)); err != nil {
		if err := s.write(ctx, SystemActor(), func(t *tx) error {
			_, err := t.ExecContext(ctx, `UPDATE pull_requests SET ready_at = NULL, ready_sha = '', ready_status_id = 0, ready_checks = '[]', ready_skipped = '[]', ready_cand = 0 WHERE id = ? AND state = 'draft'`, pr.ID)
			return err
		}); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "clear the ready mark of a pull request", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
			}
		}
		return err
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET state = 'open',
			ci_state = CASE WHEN ? THEN 'pending' ELSE ci_state END, ci_announced_sha = CASE WHEN ? THEN '' ELSE ci_announced_sha END
			WHERE id = ? AND state IN ('draft', 'open') AND ready_sha = ?`, onEdit, onEdit, pr.ID, pr.HeadSHA); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE ci_heads SET state = 'pending', settled_at = NULL, seen_at = ?, checked_at = 0 WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND sha = ? AND ?`,
			now, pr.ForgeID, pr.Repo, pr.HeadSHA, onEdit); err != nil {
			return err
		}
		pid, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		detail := "marked “" + excerpt(pr.Title, 120) + "” ready at " + short(pr.HeadSHA) + ": draft checks green, the full run starts"
		if st != nil {
			detail = "marked “" + excerpt(pr.Title, 120) + "” ready at " + short(pr.HeadSHA) + " on its passed merge queue stack (" + st.name() + "): no update or full run"
		}
		t.audit(AuditEntry{Action: "pr.ready", Category: AuditControl, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail, About: pr.Agent, Actor: "Waypoint"})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: pid, New: "ready", Data: map[string]any{"pr_id": pr.ID, "head_sha": pr.HeadSHA}})
		return nil
	})
}

func ownDraftKey(forgeID int64, repo, head string) string {
	return fmt.Sprintf("%d/%s/%s", forgeID, strings.ToLower(repo), head)
}

// expectOwnDraft notes that head is being opened as Waypoint's own draft
// (WP-158). The forge's webhook usually arrives before the call that opened
// it answers, and whichever sighting comes first fires "opened": recorded as
// a plain draft, the rules skip that event and nothing starts the first
// review later (WP-847). Call the result once the opener has recorded it.
func (s *Service) expectOwnDraft(forgeID int64, repo, head string) (done func()) {
	key := ownDraftKey(forgeID, repo, head)
	s.ownDraftsMu.Lock()
	defer s.ownDraftsMu.Unlock()
	if s.ownDrafts == nil {
		s.ownDrafts = map[string]int{}
	}
	s.ownDrafts[key]++
	return func() {
		s.ownDraftsMu.Lock()
		defer s.ownDraftsMu.Unlock()
		if s.ownDrafts[key]--; s.ownDrafts[key] <= 0 {
			delete(s.ownDrafts, key)
		}
	}
}

// ownDraftComing says whether fp is a draft being opened as Waypoint's own.
func (s *Service) ownDraftComing(forgeID int64, repo string, fp *ForgePR) bool {
	if fp.State != "draft" {
		return false
	}
	s.ownDraftsMu.Lock()
	defer s.ownDraftsMu.Unlock()
	return s.ownDrafts[ownDraftKey(forgeID, repo, fp.HeadRef)] > 0
}

// markDraft puts a pull request Waypoint marked ready back to draft: its
// full run failed, or it needs another round.
func (s *Service) markDraft(ctx context.Context, pr *PullRequest) error {
	ed, ok := s.Forges.(ForgePREditor)
	if !ok {
		return fmt.Errorf("this forge client can't edit titles")
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if err := ed.EditPRTitle(ctx, f, pr.Repo, pr.Number, wipPrefix+stripWIP(pr.Title)); err != nil {
		return err
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET state = 'draft', ready_at = NULL WHERE id = ? AND state = 'open'`, pr.ID); err != nil {
			return err
		}
		pid, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.draft", Category: AuditControl, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "put “" + excerpt(pr.Title, 120) + "” back to draft for another round", About: pr.Agent, Actor: "Waypoint"})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: pid, New: "draft", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
}

// fullRunOutstanding: a pull request Waypoint marked ready at this head still
// lacks its full run, going by these checks. Until it has one, the head's CI
// stays pending, so it is followed and its result announced when it comes.
//
// A read that fails counts as outstanding: the head's CI isn't reported passed
// on a guess, and the next look at the forge asks again.
func (s *Service) fullRunOutstanding(ctx context.Context, f *Forge, repo, sha string, checks []CommitCheck) bool {
	outstanding, err := s.fullRunOutstandingErr(ctx, f, repo, sha, checks)
	if err != nil {
		if readFault(err) && !isStatus(err, http.StatusNotFound) {
			s.log.WarnContext(ctx, "read whether a pull request's full run is outstanding", "repo", repo, "sha", sha, "err", err)
		}
		return true
	}
	return outstanding
}

func (s *Service) fullRunOutstandingErr(ctx context.Context, f *Forge, repo, sha string, checks []CommitCheck) (bool, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_sha = ? AND state = 'open' AND wip_auto = 1 AND ready_at IS NOT NULL`, f.ID, repo, sha)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	pcs := make([]PRCheck, 0, len(checks))
	for _, c := range checks {
		pcs = append(pcs, PRCheck{Name: c.Name, State: c.State, Description: c.Description, StatusID: c.ID})
	}
	for _, id := range ids {
		pr, err := s.prByID(ctx, s.db.R, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(fullRunReasons(pr, pcs)) == 0 {
			continue
		}
		if !s.noteFullRunSkips(ctx, f, pr, pcs) || len(fullRunReasons(pr, pcs)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// noteFullRunSkips (WP-552): a job the draft tier runs and the full run
// skips (Assistant/Mail's go-unit runs on drafts only) keeps the draft
// tier's success as its status, older than the ready mark, because Forgejo
// posts no status for the skip; without more, the pull request would wait
// for its full run forever. When the checks still short of the full run
// are all such old successes, and nothing else is pending, the head's
// Actions runs (what wp ci log reads) say which of them its newest finished
// run skipped; those are kept on the pull request and count as done. It
// says whether it kept any.
func (s *Service) noteFullRunSkips(ctx context.Context, f *Forge, pr *PullRequest, checks []PRCheck) bool {
	t, ok := tallyFullRun(pr, checks)
	if !ok || pr.ReadyAt == nil || len(t.stale) == 0 || t.done+len(t.stale) < len(t.names) {
		return false
	}
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok || f == nil || f.token == "" {
		return false
	}
	skipped, err := fullRunSkipped(ctx, cl, f, pr.Repo, pr.HeadSHA, *pr.ReadyAt, t.stale)
	if err != nil {
		s.log.Debug("read a full run's jobs", "pr", pr.ID, "sha", short(pr.HeadSHA), "err", err)
	}
	if len(skipped) == 0 {
		return false
	}
	all := append(slices.Clone(pr.readySkipped), skipped...)
	nm, _ := json.Marshal(all)
	kept := false
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE pull_requests SET ready_skipped = ? WHERE id = ? AND ready_sha = ? AND ready_at = ?`, string(nm), pr.ID, pr.HeadSHA, pr.ReadyAt.UnixMilli())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		kept = n > 0
		return nil
	}); err != nil {
		s.log.WarnContext(ctx, "keep a full run's skipped checks", "pr", pr.ID, "err", err)
		return false
	}
	if !kept {
		return false
	}
	pr.readySkipped = all
	return true
}

// runCancelled: a cancelled run leaves the jobs it never reached skipped, which
// says nothing about whether the full run skips them.
func runCancelled(status string) bool {
	return status == "cancelled" || status == "canceled"
}

// fullRunSkipped: of names (checks of pull request runs, "ci / go-unit
// (pull_request)"), those whose job was skipped in the newest finished run
// of sha that has the job and ran something (a no-op run for a body edit
// skips everything, so it says nothing). Runs made before readyAt (the draft
// tier's) are not the full run and say nothing. A job the newest such run ran,
// the draft tier's run before the full run starts, say, isn't one.
func fullRunSkipped(ctx context.Context, cl ForgeActionLogs, f *Forge, repo, sha string, readyAt time.Time, names []string) ([]string, error) {
	runs, err := cl.ActionRunsAt(ctx, f, repo, sha)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(runs, func(a, b ActionRun) int { return cmpInt64(b.Index, a.Index) })
	left := slices.Clone(names)
	var out []string
	for _, r := range runs {
		if len(left) == 0 {
			break
		}
		if !sameSHA(r.CommitSHA, sha) || (!r.Created.IsZero() && r.Created.Before(readyAt)) || !rerunByEdit("("+r.Event+")") || !runDone(r.Status) || runCancelled(r.Status) {
			continue
		}
		jobs, err := cl.ActionJobs(ctx, f, repo, r.ID)
		if err != nil {
			return out, err
		}
		if !slices.ContainsFunc(jobs, func(j ActionJob) bool { return j.Status != "skipped" }) {
			continue
		}
		left = slices.DeleteFunc(left, func(n string) bool {
			i := slices.IndexFunc(jobs, func(j ActionJob) bool { return strings.HasSuffix(n, " / "+j.Name+" ("+r.Event+")") })
			if i < 0 {
				return false
			}
			if jobs[i].Status == "skipped" {
				out = append(out, n)
			}
			return true
		})
	}
	return out, nil
}
