package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// CI polling and states.
const (
	ciPollEvery      = time.Minute
	ciPollWindow     = 2 * time.Hour
	ciPollSlowEvery  = 10 * time.Minute
	ciPollSlowWindow = 24 * time.Hour
	// Past a day a live pull request's pending head is still read, hourly,
	// until it settles or the pull request closes: a status webhook the forge
	// never sent otherwise left a finished run "pending" for good, and its
	// babysit watching it (one pull request waited 30h).
	ciPollStaleEvery = time.Hour
	ciNoChecksWait   = 5 * time.Minute

	CIPending = "pending"
	CIPassed  = "passed"
	CIFailed  = "failed"
	CINone    = "none"
)

// PRCheck is one status or check on a pull request's head.
type PRCheck struct {
	Name        string `json:"name"`
	State       string `json:"state"` // pending, success, failed
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	HasLog      bool   `json:"has_log,omitempty"` // the tail of its log is kept
	StatusID    int64  `json:"-"`                 // the forge's id of its newest status (WP-158)
}

// ownCheck: Waypoint's own statuses don't count toward CI.
func ownCheck(name string) bool { return strings.HasPrefix(name, "waypoint/") }

// trackHead makes sure a commit's CI is followed.
func (s *Service) trackHead(ctx context.Context, forgeID int64, repo, sha string) (int64, error) {
	if sha == "" {
		return 0, errors.New("no commit")
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO ci_heads(forge_id, repo, sha, seen_at) VALUES (?,?,?,?)`, forgeID, repo, sha, s.nowMS()); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM ci_heads WHERE forge_id = ? AND repo = ? AND sha = ?`, forgeID, repo, sha).Scan(&id)
	return id, err
}

// dropSuperseded leaves out a cancelled check whose job ran for the same
// head under another event and didn't fail: Forgejo cancels a branch's push
// run when a pull request run starts on it, and reports each cancelled job
// as a failed "ci / backend (push)" next to "ci / backend (pull_request)".
// A cancellation with no such sibling still counts as failed.
func dropSuperseded(checks []CommitCheck) []CommitCheck {
	ran := map[string]bool{}
	for _, c := range checks {
		if job, ok := jobOf(c.Name); ok && c.State != CheckFailed {
			ran[job] = true
		}
	}
	out := checks[:0:0]
	for _, c := range checks {
		if job, ok := jobOf(c.Name); ok && c.State == CheckFailed && ran[job] && cancelledCheck(c.Description) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// jobOf strips the event from an Actions check name: "ci / backend (push)"
// is job "ci / backend".
func jobOf(name string) (string, bool) {
	if !strings.HasSuffix(name, ")") {
		return "", false
	}
	i := strings.LastIndex(name, " (")
	if i <= 0 {
		return "", false
	}
	return name[:i], true
}

// ciState decides a head's state from its checks.
func ciState(checks []CommitCheck, seenAt, now int64) string {
	if len(checks) == 0 {
		if now-seenAt < ciNoChecksWait.Milliseconds() {
			return CIPending
		}
		return CINone
	}
	failed, pending := false, false
	for _, c := range checks {
		switch c.State {
		case CheckFailed:
			failed = true
		case CheckPending:
			pending = true
		}
	}
	switch {
	case pending:
		return CIPending
	case failed:
		return CIFailed
	}
	return CIPassed
}

// sameLogStale reports whether a kept log belongs to an older status than the
// one now read: a rerun may keep its URL but is posted as a new status.
func sameLogStale(kept, now int64) bool { return now != 0 && kept != now }

// refreshHead reads a commit's statuses and checks, stores them, updates
// the pull requests that have it as head, and announces what settled.
func (s *Service) refreshHead(ctx context.Context, f *Forge, repo, sha string) error {
	cl, ok := s.Forges.(ForgeChecks)
	if !ok || sha == "" || f.token == "" {
		return nil
	}
	headID, err := s.trackHead(ctx, f.ID, repo, sha)
	if err != nil {
		return err
	}
	all, err := cl.CommitChecks(ctx, f, repo, sha)
	if err != nil {
		_, _ = s.db.W.ExecContext(ctx, `UPDATE ci_heads SET checked_at = ? WHERE id = ?`, s.nowMS(), headID)
		return err
	}
	s.foreignOwnStatuses(ctx, f, repo, sha, all)
	checks := all[:0:0]
	for _, c := range all {
		if !ownCheck(c.Name) {
			checks = append(checks, c)
		}
	}
	checks = newestPerContext(checks)
	var seen int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT seen_at FROM ci_heads WHERE id = ?`, headID).Scan(&seen); err != nil {
		return err
	}
	now := s.nowMS()

	type keptLog struct {
		url, log string
		statusID int64
	}
	have := map[string]keptLog{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT name, url, log, COALESCE(status_id, 0) FROM ci_checks WHERE head_id = ? AND log != ''`, headID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n, u, l string
		var sid int64
		if rows.Scan(&n, &u, &l, &sid) == nil {
			have[n] = keptLog{u, l, sid}
		}
	}
	rows.Close()
	var need []CommitCheck
	for _, c := range checks {
		if k, kept := have[c.Name]; c.State == CheckFailed && (!kept || k.url != c.URL || sameLogStale(k.statusID, c.ID) || (cancelledCheck(c.Description) && !stepFailedLog(k.log))) {
			need = append(need, c)
		}
	}
	var logs map[string]string
	if len(need) > 0 {
		logs, err = cl.FailedLogs(ctx, f, repo, sha, need)
		if err != nil && ctx.Err() == nil {
			s.log.Debug("read failed job logs", "repo", repo, "sha", short(sha), "err", err)
		}
		for _, c := range need {
			if fault := logRunnerFault(logs[c.Name]); fault != nil {
				s.nameFaultHost(ctx, f.ID, fault)
				s.runnerFaults.add(seenFault{at: s.now(), forgeID: f.ID, repo: repo, check: c.Name, url: c.URL, fault: *fault})
				s.log.WarnContext(ctx, "a CI job failed on its runner", "repo", repo, "sha", short(sha), "check", c.Name, "runner", fault.Runner, "fault", fault.What)
			}
		}
	}
	for i, c := range checks {
		log := logs[c.Name]
		if k, kept := have[c.Name]; kept && k.url == c.URL && !sameLogStale(k.statusID, c.ID) && log == "" {
			log = k.log
		}
		if c.State == CheckFailed && cancelledCheck(c.Description) && stepFailedLog(log) {
			checks[i].Description = failedThenStopped
		}
	}
	checks = dropSuperseded(checks)
	state := ciState(checks, seen, now)
	if state == CIPassed && s.fullRunOutstanding(ctx, f, repo, sha, checks) {
		state = CIPending
	}

	type prState struct {
		id        int64
		old       string
		announced string
		number    int
		outside   bool
	}
	var prs []prState
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, c := range checks {
			if _, err := t.ExecContext(ctx, `INSERT INTO ci_checks(head_id, name, state, url, description, source, job_id, log, updated_at, status_id) VALUES (?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(head_id, name) DO UPDATE SET state = excluded.state, url = excluded.url, description = excluded.description,
					source = excluded.source, job_id = excluded.job_id, updated_at = excluded.updated_at, status_id = excluded.status_id,
					log = CASE WHEN excluded.state = 'failed' THEN CASE WHEN excluded.log != '' OR excluded.url != url OR (excluded.status_id != 0 AND excluded.status_id != COALESCE(status_id, 0)) THEN excluded.log ELSE log END ELSE '' END`,
				headID, c.Name, c.State, c.URL, excerpt(c.Description, 300), c.Source, c.JobID, logs[c.Name], now, c.ID); err != nil {
				return err
			}
		}
		names := make([]any, 0, len(checks)+1)
		names = append(names, headID)
		for _, c := range checks {
			names = append(names, c.Name)
		}
		if len(checks) == 0 {
			if _, err := t.ExecContext(ctx, `DELETE FROM ci_checks WHERE head_id = ?`, headID); err != nil {
				return err
			}
		} else if _, err := t.ExecContext(ctx, `DELETE FROM ci_checks WHERE head_id = ? AND name NOT IN (`+idList(len(checks))+`)`, names...); err != nil { // #nosec G202 -- placeholders only; values are bound
			return err
		}
		var settled any
		if state != CIPending {
			settled = now
		}
		if _, err := t.ExecContext(ctx, `UPDATE ci_heads SET state = ?, checked_at = ?, settled_at = ? WHERE id = ?`, state, now, settled, headID); err != nil {
			return err
		}
		rows, err := t.QueryContext(ctx, `SELECT id, ci_state, ci_announced_sha, number, outside FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_sha = ? AND state IN ('open','draft')`, f.ID, repo, sha)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p prState
			if err := rows.Scan(&p.id, &p.old, &p.announced, &p.number, &p.outside); err != nil {
				rows.Close()
				return err
			}
			prs = append(prs, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var failedNames []string
		var links []map[string]string
		for _, c := range checks {
			if c.State == CheckFailed {
				failedNames = append(failedNames, c.Name)
				links = append(links, map[string]string{"name": c.Name, "url": c.URL})
			}
		}
		projects, err := s.projectsOfRepo(ctx, f.ID, repo)
		if err != nil {
			return err
		}
		for _, p := range prs {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET ci_state = ?, ci_sha = ? WHERE id = ?`, state, sha, p.id); err != nil {
				return err
			}
			if (state != CIPassed && state != CIFailed) || p.announced == sha || p.outside {
				continue
			}
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET ci_announced_sha = ? WHERE id = ?`, sha, p.id); err != nil {
				return err
			}
			for _, pid := range projects {
				t.emit(&Event{Kind: EvPRCI, ProjectID: pid, New: state, Data: map[string]any{
					"pr": p.id, "pr_id": p.id, "repo": repo, "number": p.number, "sha": sha, "head_sha": sha,
					"state": state, "failed": failedNames, "links": links}})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range prs {
		if p.old != state && (state == CIPassed || state == CINone) {
			id := p.id
			s.goBG(func() { s.maybeAutoMerge(context.WithoutCancel(ctx), id) })
		}
	}
	return nil
}

// refreshPRChecks reads the checks on a pull request's head now.
func (s *Service) refreshPRChecks(ctx context.Context, prID int64) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || pr.HeadSHA == "" {
		return
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return
	}
	if err := s.refreshHead(ctx, f, pr.Repo, pr.HeadSHA); err != nil {
		s.log.Debug("read checks", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// liveHeadSQL: the ci_heads row h is the current head of an open or draft
// pull request.
const liveHeadSQL = `EXISTS (SELECT 1 FROM pull_requests p WHERE p.forge_id = h.forge_id AND p.repo = h.repo COLLATE NOCASE
	AND p.head_sha = h.sha AND p.state IN ('open','draft'))`

// PollCI reads the heads whose checks are still pending, once a minute.
func (s *Service) PollCI(ctx context.Context) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT h.forge_id, h.repo, h.sha FROM ci_heads h WHERE h.state = 'pending' AND (
			(h.seen_at > ? AND h.checked_at < ?)
			OR (h.checked_at < CASE WHEN h.seen_at > ? THEN ? ELSE ? END AND `+liveHeadSQL+`)
		) ORDER BY h.checked_at LIMIT 50`,
		now-ciPollWindow.Milliseconds(), now-ciPollEvery.Milliseconds()+5000,
		now-ciPollSlowWindow.Milliseconds(), now-ciPollSlowEvery.Milliseconds()+5000, now-ciPollStaleEvery.Milliseconds()+5000)
	if err != nil {
		return
	}
	type head struct {
		forge int64
		repo  string
		sha   string
	}
	var list []head
	for rows.Next() {
		var h head
		if rows.Scan(&h.forge, &h.repo, &h.sha) == nil {
			list = append(list, h)
		}
	}
	rows.Close()
	forges := map[int64]*Forge{}
	for _, h := range list {
		if ctx.Err() != nil {
			return
		}
		f := forges[h.forge]
		if f == nil {
			var err error
			if f, err = s.forgeByID(ctx, h.forge); err != nil {
				continue
			}
			forges[h.forge] = f
		}
		if err := s.refreshHead(ctx, f, h.repo, h.sha); err != nil {
			s.log.Debug("poll checks", "repo", h.repo, "sha", short(h.sha), "err", err)
		}
	}
}

// pruneCI forgets heads older than a month, CI hosts' job usage older
// than two months, and old CI evidence.
func (s *Service) pruneCI(ctx context.Context) {
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM ci_heads WHERE seen_at < ?`, s.nowMS()-30*24*time.Hour.Milliseconds()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM ci_job_usage WHERE started_at < ?`, s.nowMS()-ciUsageKeep.Milliseconds()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	s.pruneCIEvidence(ctx)
}

// onForgeCI handles a webhook that says checks on a commit changed (sha is
// empty when the payload doesn't say which: every pending head of the repo
// is read).
func (s *Service) onForgeCI(ctx context.Context, f *Forge, repo, sha string) {
	switch id, err := s.attachedProject(ctx, s.db.R, f.ID, repo); {
	case err != nil:
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project of a repo whose checks changed", "repo", repo, "err", err)
		}
	case id == 0:
		return
	}
	if sha != "" {
		if ref, ok := s.candLineByCommit(ctx, f.ID, repo, sha); ok {
			s.lineChangedAsync(ref)
			return
		}
		if err := s.refreshHead(ctx, f, repo, sha); err != nil {
			s.log.Debug("read checks", "repo", repo, "sha", short(sha), "err", err)
		}
		return
	}
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT h.sha FROM ci_heads h WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.state = 'pending'
		AND (h.seen_at > ? OR `+liveHeadSQL+`)`, f.ID, repo, now-ciPollWindow.Milliseconds())
	if err != nil {
		return
	}
	var shas []string
	for rows.Next() {
		var x string
		if rows.Scan(&x) == nil {
			shas = append(shas, x)
		}
	}
	rows.Close()
	for _, x := range shas {
		if err := s.refreshHead(ctx, f, repo, x); err != nil {
			s.log.Debug("read checks", "repo", repo, "sha", short(x), "err", err)
		}
	}
}

// refreshHeadLogged is refreshHead for the background: a forge that has no
// such commit or repo (404) is the documented "no", the rest is a warning.
func (s *Service) refreshHeadLogged(ctx context.Context, f *Forge, repo, sha string) {
	if err := s.refreshHead(ctx, f, repo, sha); readFault(err) && !isStatus(err, http.StatusNotFound) {
		s.log.WarnContext(ctx, "read the checks of a head", "repo", repo, "sha", short(sha), "err", err)
	}
}

// afterPRRecorded does what a pull request's new state calls for: follow
// its head's CI, re-evaluate the review gate, and merge it if it is armed
// and ready. Quiet for repos that use none of it.
func (s *Service) afterPRRecorded(ctx context.Context, f *Forge, pr *PullRequest) {
	if pr == nil || (pr.State != "open" && pr.State != "draft") {
		return
	}
	bg := context.WithoutCancel(ctx)
	if _, ok := s.Forges.(ForgeChecks); ok && pr.HeadSHA != "" && f.token != "" {
		if _, err := s.trackHead(ctx, f.ID, pr.Repo, pr.HeadSHA); err == nil {
			s.goBG(func() {
				s.refreshHeadLogged(bg, f, pr.Repo, pr.HeadSHA)
			})
		}
	}
	if pr.Outside {
		return
	}
	on, _, gerr := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if gerr != nil {
		if readFault(gerr) {
			s.log.WarnContext(ctx, "read a repo's review gate setting", "repo", pr.Repo, "number", pr.Number, "err", gerr)
		}
	}
	if on || gerr != nil || pr.GateState != "" {
		s.goBG(func() { s.checkPR(bg, pr.ID, "pull request updated") })
	}
	if s.autoArm(ctx, pr.ID) {
		s.goBG(func() { s.maybeAutoMerge(bg, pr.ID) })
	}
}

// prChecks are the checks stored for a pull request's head.
func (s *Service) prChecks(ctx context.Context, pr *PullRequest) ([]PRCheck, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.state, c.url, c.description, c.log != '', c.status_id FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? ORDER BY c.name`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PRCheck{}
	for rows.Next() {
		var c PRCheck
		if err := rows.Scan(&c.Name, &c.State, &c.URL, &c.Description, &c.HasLog, &c.StatusID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FailedCheck is a failed check with the tail of its log, for a bot that
// fixes CI.
type FailedCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Log  string `json:"log"`
}

// FailedChecks are the failed checks on a pull request's head with their
// log tails (what fix_ci reads; people see the links).
func (s *Service) FailedChecks(ctx context.Context, actor *Actor, prID int64) ([]FailedCheck, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.url, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? AND c.state = 'failed' ORDER BY c.name`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FailedCheck{}
	for rows.Next() {
		var c FailedCheck
		if err := rows.Scan(&c.Name, &c.URL, &c.Log); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ciSummary describes a pull request's CI in a few words, for readiness.
func ciSummary(state string, checks []PRCheck) (ok bool, why string) {
	switch state {
	case CIFailed:
		var names []string
		for _, c := range checks {
			if c.State == CheckFailed {
				names = append(names, c.Name)
			}
		}
		slices.Sort(names)
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
		}
		return false, "checks failed: " + strings.Join(names, ", ")
	case CIPending:
		n := 0
		for _, c := range checks {
			if c.State == CheckPending {
				n++
			}
		}
		if n > 0 {
			return false, fmt.Sprintf("%d check%s still running", n, plural(n))
		}
		return false, "checks haven't reported yet"
	case "":
		return false, "checks haven't been read yet"
	}
	return true, ""
}
