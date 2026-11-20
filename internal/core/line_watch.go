package core

// The line watch (WP-431). On 2026-11-02 a repo's merge line stood
// empty from about 04:00 to 11:23 UTC with twelve pull requests armed:
// eight babysits had given up at the round limit and four waited on a
// person, all over faults in Waypoint itself (WP-428, WP-430), while CI sat
// a third busy. The fixes shipped and nothing picked the pull requests up
// again, and the only word of it was one notification per pull request, in
// the babysitter's terms. So:
//   - when Waypoint starts (a deploy, usually of a fix), a babysit that gave
//     up in the last day, or waits on a person for a reason Waypoint came to
//     itself, on an armed open pull request starts again, at most once in
//     resumeEvery for each pull request; review comments a person must
//     decide still wait for the person, at no round's cost;
//   - a line with armed pull requests and none of them moving tries to free
//     itself after lineSelfHealAfter: a pull request nobody babysits gets a
//     babysit, one given up on starts again (WP-784);
//   - only a line still stuck after lineStalledAfter files one request, in
//     plain words, for the person who armed them, saying what each waits
//     for. It expires when the line moves again. How long a line has been
//     quiet is read from what the database records, so a deploy doesn't
//     start the count again.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	resumeGaveUpWithin = 24 * time.Hour
	resumeEvery        = 6 * time.Hour
	lineSelfHealAfter  = 30 * time.Minute
	lineStalledAfter   = 3 * time.Hour
	lineStalledKey     = "auto:line-stalled:"
	viaResume          = "resume"
	lineMovingAgain    = "The merge line is moving again."
	resumeEnded        = "started again after Waypoint restarted"
)

// ResumeBabysits starts again, once Waypoint has started, the babysits of
// armed open pull requests that gave up in the last day or wait on a person
// for Waypoint's own reasons: the Waypoint that judged them may have been
// the fault.
func (s *Service) ResumeBabysits(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT b.pr_id FROM babysits b JOIN pull_requests p ON p.id = b.pr_id
		WHERE p.armed_at IS NOT NULL AND p.state IN ('open','draft')
		  AND b.id = (SELECT max(id) FROM babysits WHERE pr_id = b.pr_id)
		  AND ((b.state IN (?, ?) AND b.ended_at >= ?) OR (b.state = ? AND b.ended_at IS NULL))
		ORDER BY p.armed_at, p.id`, BabysitGaveUp, BabysitStopped, s.now().Add(-resumeGaveUpWithin).UnixMilli(), BabysitNeedsPerson)
	if err != nil {
		s.log.WarnContext(ctx, "resume babysits", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "resume babysits", "err", err)
		return
	}
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if s.resumeBabysit(ctx, id) {
			n++
		}
	}
	if n > 0 {
		s.log.Info("babysits started again after Waypoint started", "count", n)
	}
}

// resumeBabysit starts the pull request's babysit again when it gave up or
// waits on a person for no person's reason, and wasn't resumed lately.
func (s *Service) resumeBabysit(ctx context.Context, prID int64) bool {
	unlock := s.babysitLocks.lock(prID)
	b, pr, ok := s.resumable(ctx, prID)
	var owner int64
	if ok {
		// Everything a start can refuse is checked before the waiting
		// babysit ends, so a refusal leaves it waiting as it was.
		if owner = b.ownerID; owner == 0 {
			owner = s.babysitOwnerFor(ctx, pr)
		}
		ok = owner != 0 && (b.Model == "" || modelRE.MatchString(b.Model))
	}
	if ok && b.Active {
		ok = s.endBabysit(ctx, b, pr, BabysitStopped, resumeEnded) == nil
	}
	unlock()
	if !ok {
		return false
	}
	model := b.Model
	if planningModel(model) { // stored before startBabysit refused these: resume on the default
		model = ""
	}
	if _, err := s.startBabysit(ctx, SystemActor(), pr, owner, model, viaResume, b.Notify, false); err != nil {
		s.log.WarnContext(ctx, "resume a babysit", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	return true
}

func (s *Service) resumable(ctx context.Context, prID int64) (*Babysit, *PullRequest, bool) {
	b, err := s.latestBabysit(ctx, s.db.R, prID)
	if err != nil || b == nil {
		return nil, nil, false
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || pr.ArmedAt == nil || (pr.State != "open" && pr.State != "draft") {
		return nil, nil, false
	}
	switch b.State {
	case BabysitGaveUp:
	case BabysitStopped:
		if b.EndReason != resumeEnded {
			return nil, nil, false // stopped by a person, or by Waypoint for its reason
		}
		// ended for a resume whose start then failed: try again
	case BabysitNeedsPerson:
		if n := len(b.Rounds); n > 0 && b.Rounds[n-1].To == "session" {
			return nil, nil, false // handed to a session: its push carries it on
		}
		if parkedOn(b, pr.HeadSHA) {
			return nil, nil, false // parked for a person's answer: that, or a push, carries it on (WP-439)
		}
		if n := len(b.Rounds); n > 0 && sameSHA(b.Rounds[n-1].SHA, pr.HeadSHA) {
			if _, refused := protectedRefusal(b.Rounds[n-1]); refused {
				return nil, nil, false // a fresh round is refused the same way; a session's push carries it on
			}
		}
		if person, err := s.personComments(ctx, prID); err != nil || person > 0 {
			return nil, nil, false // a person's call: starting again changes nothing
		}
	default:
		return nil, nil, false
	}
	var last int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(created_at), 0) FROM babysits WHERE pr_id = ? AND started_via = ?`, prID, viaResume).Scan(&last); err != nil ||
		s.nowMS()-last < resumeEvery.Milliseconds() {
		return nil, nil, false
	}
	return b, pr, true
}

// personComments counts the review comments on a pull request that wait for
// a person's decision.
func (s *Service) personComments(ctx context.Context, prID int64) (int, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM review_comments WHERE pr_id = ? AND verdict = ? AND gone_at IS NULL AND resolved = 0`,
		prID, VerdictNeedsPerson).Scan(&n)
	return n, err
}

// ---- a stalled line ----

// lineRef is one merge line: a base of a repo on a forge.
type lineRef struct {
	forge      int64
	repo, base string
}

func (l lineRef) key() string {
	return fmt.Sprintf("%s%d:%s:%s", lineStalledKey, l.forge, strings.ToLower(l.repo), l.base)
}

// watchLines frees the merge lines that have armed pull requests and none of
// them moving, files a request for each still stuck after lineStalledAfter,
// and expires the requests of lines that move again (from CheckBabysits).
func (s *Service) watchLines(ctx context.Context) {
	now := s.now()
	// A line went quiet at the latest of its last merge, an arming, and a
	// babysit changing what it does or ending.
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.forge_id, p.repo, p.base_ref,
		  max(EXISTS (SELECT 1 FROM babysits b WHERE b.pr_id = p.id AND b.ended_at IS NULL AND b.state != ?)),
		  max(
		    coalesce((SELECT max(m.merged_at) FROM pull_requests m WHERE m.forge_id = p.forge_id AND m.repo = p.repo COLLATE NOCASE AND m.base_ref = p.base_ref AND m.state = 'merged'), 0),
		    max(p.armed_at),
		    coalesce(max((SELECT max(max(coalesce(b.wait_at, b.created_at), coalesce(b.ended_at, 0))) FROM babysits b WHERE b.pr_id = p.id)), 0))
		FROM pull_requests p WHERE p.armed_at IS NOT NULL AND p.state IN ('open','draft')
		GROUP BY p.forge_id, lower(p.repo), p.base_ref`, BabysitNeedsPerson)
	if err != nil {
		return
	}
	type line struct {
		ref    lineRef
		moving bool
		quiet  int64
	}
	var lines []line
	for rows.Next() {
		var l line
		if rows.Scan(&l.ref.forge, &l.ref.repo, &l.ref.base, &l.moving, &l.quiet) == nil {
			lines = append(lines, l)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "watch merge lines", "err", err)
		return // a partial view could expire a stalled line's request
	}
	stalled := map[string]bool{}
	for _, l := range lines {
		since := time.UnixMilli(l.quiet)
		if l.moving || now.Sub(since) < lineSelfHealAfter {
			continue
		}
		if s.unstickLine(ctx, l.ref) || now.Sub(since) < lineStalledAfter {
			continue // what it started moves the line; only then ask a person
		}
		stalled[l.ref.key()] = true
		s.askAboutLine(ctx, l.ref, since)
	}
	s.expireMovingLines(ctx, stalled)
}

// armedOn is the line's armed open pull requests, first armed first.
func (s *Service) armedOn(ctx context.Context, l lineRef) ([]*PullRequest, error) {
	// #nosec G202 -- a fixed column list; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.armed_at IS NOT NULL AND p.state IN ('open','draft')
		ORDER BY p.armed_at, p.number`, l.forge, l.repo, l.base)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prs []*PullRequest
	for rows.Next() {
		if pr, err := scanPR(rows); err == nil {
			prs = append(prs, pr)
		}
	}
	return prs, rows.Err()
}

// unstickLine does what Waypoint can without a person for a line that isn't
// moving, and reports whether it started anything: a babysit for a pull
// request that never had one, whatever keeps it from merging, and again for
// one given up on in the last day (at most every resumeEvery). A wait on a
// person is left to the watchdog, which spends one assessor round per head.
func (s *Service) unstickLine(ctx context.Context, l lineRef) bool {
	prs, err := s.armedOn(ctx, l)
	if err != nil {
		return false
	}
	started := false
	for _, pr := range prs {
		b, err := s.latestBabysit(ctx, s.db.R, pr.ID)
		switch {
		case err != nil:
		case b == nil:
			started = s.babysitArmedAs(ctx, pr, s.armedOwnerFor(ctx, pr)) || started
		case b.State == BabysitGaveUp && b.EndedAt != nil && s.now().Sub(*b.EndedAt) < resumeGaveUpWithin,
			b.State == BabysitStopped && b.EndReason == resumeEnded:
			started = s.resumeBabysit(ctx, pr.ID) || started
		}
	}
	return started
}

// expireMovingLines expires the open requests about lines no longer stalled.
func (s *Service) expireMovingLines(ctx context.Context, stalled map[string]bool) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT tool_use_id FROM requests WHERE state = ? AND tool_use_id LIKE ?`, RequestOpen, lineStalledKey+"%")
	if err != nil {
		return
	}
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil && !stalled[k] {
			keys = append(keys, k)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, k := range keys {
		if err := s.write(ctx, SystemActor(), func(t *tx) error {
			_, err := s.expireRequests(ctx, t, `r.tool_use_id = ?`, []any{k}, lineMovingAgain, false, s.nowMS())
			return err
		}); err != nil {
			s.log.WarnContext(ctx, "expire a stalled line's request", "line", k, "err", err)
		}
	}
}

// askAboutLine files the one request about a stalled line, for the person
// who armed its first pull request, unless one is open or one was filed
// since the line stalled (a person who dismissed it isn't asked again until
// the line moves and stalls anew).
func (s *Service) askAboutLine(ctx context.Context, l lineRef, since time.Time) {
	prs, err := s.armedOn(ctx, l)
	if err != nil || len(prs) == 0 {
		return
	}
	// Asking spends no runner, so it needs only someone to ask.
	owner := s.babysitOwnerFor(ctx, prs[0])
	if owner == 0 {
		owner = s.repoProjectOwner(ctx, l.forge, prs[0].Repo)
	}
	if owner == 0 {
		return
	}
	key := l.key()
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? AND user_id = ? AND (state = ? OR created_at >= ?) LIMIT 1`,
		key, owner, RequestOpen, since.UnixMilli()).Scan(&id) == nil {
		return
	}
	u, err := s.userByID(ctx, s.db.R, owner)
	if err != nil || u.Disabled {
		return
	}
	in := RequestInput{Kind: RequestAction, Title: stalledLineTitle(prs[0].Repo, l.base, len(prs)),
		Body: s.stalledLineBody(ctx, l, prs, since), ToolUseID: key, Followup: "never"}
	if p := s.attachedProject(ctx, s.db.R, l.forge, prs[0].Repo); p != 0 {
		if proj, err := s.projectByID(ctx, s.db.R, p); err == nil {
			in.Project = proj.Key
		}
	}
	if _, _, err := s.CreateRequest(ctx, &Actor{User: u, Agent: "waypoint/babysit"}, in); err != nil {
		s.log.WarnContext(ctx, "ask about a stalled merge line", "repo", prs[0].Repo, "base", l.base, "err", err)
	}
}

func stalledLineTitle(repo, base string, n int) string {
	if n == 1 {
		return fmt.Sprintf("Nothing is merging into %s %s: its one armed pull request isn't moving", repo, base)
	}
	return fmt.Sprintf("Nothing is merging into %s %s: %d armed pull requests, none moving", repo, base, n)
}

// stalledLineBody says what a person needs: since when (a time stays true
// while the request stays open, as a duration wouldn't), what each pull
// request waits for, and only the actions that apply to these pull requests.
func (s *Service) stalledLineBody(ctx context.Context, l lineRef, prs []*PullRequest, since time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "No pull request has merged into %s of %s or made progress since %s.", l.base, prs[0].Repo, since.UTC().Format("2 Jan 15:04 UTC"))
	if len(prs) == 1 {
		b.WriteString(" It has one armed pull request, and this is what it is waiting for:\n\n")
	} else {
		fmt.Fprintf(&b, " It has %d armed pull requests, and this is what each is waiting for:\n\n", len(prs))
	}
	restart, comments := false, false
	for _, pr := range prs {
		on, again := s.lineStuckOn(ctx, pr)
		restart = restart || again
		if n, err := s.personComments(ctx, pr.ID); err == nil && n > 0 {
			comments = true
		}
		fmt.Fprintf(&b, "- #%d %s: %s\n", pr.Number, pr.Title, on)
	}
	b.WriteString("\n")
	if restart {
		b.WriteString("To have Waypoint try one again now: `wp pr babysit N`. ")
	}
	if comments {
		b.WriteString("A review comment is waiting for your decision: `wp pr comments N` shows it. Accept or reject it on the pull request's page, or with `wp pr ack ID --accept|--reject -m WHY`. ")
	}
	b.WriteString("This request closes by itself once a pull request moves.")
	return b.String()
}

// lineStuckOn says, in plain words, what keeps an armed pull request from
// moving, and whether starting its babysit again could help.
func (s *Service) lineStuckOn(ctx context.Context, pr *PullRequest) (string, bool) {
	b, err := s.latestBabysit(ctx, s.db.R, pr.ID)
	switch {
	case err != nil:
		return "Waypoint couldn't read its babysitting", false
	case b == nil:
		if why := s.babysitGate(ctx, pr); why != "" {
			return "no agent may work on it: " + why, false // starting a babysit changes nothing
		}
		return "no agent is working on it, because Waypoint found no runner it may send one to", true
	case b.State == BabysitGaveUp:
		return fmt.Sprintf("Waypoint gave up on it after %d fix-up round%s (%s)", b.Round, plural(b.Round), b.EndReason), true
	case b.State == BabysitStopped:
		return "its babysitting was stopped (" + cmp(b.EndReason, "by hand") + ")", true
	case b.State == BabysitNeedsPerson && isFullRunReason(b.Wait):
		// Its checks decide this, not a fix-up round: a babysit started
		// again waits for the same run (WP-552).
		return "its CI hasn't finished the full run Waypoint waits for: " + b.Wait +
			". Starting its babysit again won't change that; the run's page on the forge says whether it is still going, and a re-run of it there starts it again", false
	case b.State == BabysitNeedsPerson && waitsForSession(b):
		return "it waits for a session to push its fix: " + b.Wait, false
	case b.State == BabysitNeedsPerson:
		return "it waits for a person: " + b.Wait, false
	}
	return "its babysit is " + strings.ReplaceAll(b.State, "_", " "), false
}

// waitsForSession: what it waits for now is a round's fix refused for
// protected files, so a task asks a session, not a person, to push it. The
// wait says so, not the last round: a later step can move the wait on
// without a round.
func waitsForSession(b *Babysit) bool {
	return protectedWaitRE.MatchString(b.Wait)
}
