package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// babysitStaleAfter is how long a babysit rests in needs_person before
	// the watchdog steps in.
	babysitStaleAfter = 6 * time.Hour
	// babysitChecksStale is how long it says checks are running before they
	// are read from the forge again, at most once per period.
	babysitChecksStale = time.Hour
	// personCommentsWait is in the wait of a babysit held by review comments
	// only a person can settle.
	personCommentsWait = " for a person: "
	// personRequestKey keys the request about them:
	// auto:babysit-person:<babysit>:<comments>.
	personRequestKey = "auto:babysit-person:"
	// checksWait starts the wait of a babysit whose checks are running.
	checksWait = "checks running"
	// checksUnreadWait starts the wait of one whose checks were never read
	// ("checks haven't been read yet", "checks haven't reported yet").
	checksUnreadWait = "checks haven't"
	// readyUpdatesAtOnce is how many ready, unarmed pull requests of one base
	// run checks at once on being brought up to date: one repository's full
	// suite can take two hours, and every update starts one.
	readyUpdatesAtOnce = 2
)

// reviveStaleBabysits moves on the babysits that have rested too long.
func (s *Service) reviveStaleBabysits(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT pr_id FROM babysits WHERE ended_at IS NULL AND coalesce(job_id, 0) = 0 AND state IN (?,?)
		AND coalesce(wait_at, updated_at) <= ? ORDER BY coalesce(wait_at, updated_at) LIMIT 50`,
		BabysitWatching, BabysitNeedsPerson, s.now().Add(-babysitChecksStale).UnixMilli())
	if err != nil {
		return
	}
	var prs []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			prs = append(prs, id)
		}
	}
	rows.Close()
	for _, id := range prs {
		if ctx.Err() != nil {
			return
		}
		s.reviveStale(ctx, id)
	}
}

// reviveStale does what is due for one pull request's resting babysit.
func (s *Service) reviveStale(ctx context.Context, prID int64) {
	if s.reviveRound(ctx, prID) {
		s.refreshStaleChecks(ctx, prID)
	}
}

// refreshStaleChecks reads a pull request's checks again, once an hour.
func (s *Service) refreshStaleChecks(ctx context.Context, prID int64) {
	if at, ok := s.staleChecksRead.Load(prID); ok && s.now().Sub(at.(time.Time)) < babysitChecksStale {
		return
	}
	s.staleChecksRead.Store(prID, s.now())
	s.refreshPRChecks(ctx, prID)
}

// reviveRound hands a stale needs_person babysit to the assessor, or files
// the request a person-only wait needs. It reports whether the babysit is
// instead waiting on checks that should be read again (outside the babysit's
// lock: reading them can step it). A babysit whose lock is held has a step
// running, so it is being moved: it is passed over, and the next run of the
// job looks again.
func (s *Service) reviveRound(ctx context.Context, prID int64) (readChecks bool) {
	unlock, ok := s.babysitLocks.tryLock(prID)
	if !ok {
		return false
	}
	defer unlock()
	b, err := s.activeBabysit(ctx, s.db.R, prID)
	if err != nil || b == nil || b.JobID != 0 {
		return false
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || (pr.State != "open" && pr.State != "draft") {
		return false
	}
	idle := s.now().Sub(b.WaitSince)
	s.noticeRest(ctx, b, pr, idle)
	if b.State == BabysitWatching {
		return (strings.HasPrefix(b.Wait, checksWait) || strings.HasPrefix(b.Wait, checksUnreadWait)) && idle >= babysitChecksStale
	}
	if b.State != BabysitNeedsPerson || idle < babysitStaleAfter || parkedOn(b, pr.HeadSHA) {
		return false
	}
	refusedNotMine := s.refusedOutsideDiff(ctx, b, pr)
	if !refusedNotMine && !staleWaitRevivable(b.Wait) {
		return false
	}
	if strings.Contains(b.Wait, personCommentsWait) {
		if !s.takeUpHeldComments(ctx, b, pr, true) {
			s.askAboutPersonComments(ctx, b, pr)
		}
		return false
	}
	var findings []string
	if auditHoldReason(b.Wait) {
		for _, f := range s.auditTodo(ctx, pr.ID) {
			findings = append(findings, f.ID)
		}
	}
	why := fmt.Sprintf("it has rested for %s and nothing has moved it: %s", idle.Round(time.Hour), b.Wait)
	if refusedNotMine {
		why += ". None of those protected files is in this pull request's own diff: the refused round most likely brought them in by merging its base, " +
			"and a round may push a merge whose protected files match the base's (WP-750). Make the change the pull request needs without editing any of them"
	}
	s.log.Info("babysit watchdog", "repo", pr.Repo, "number", pr.Number, "wait", b.Wait)
	s.babysitRoundBy(ctx, b, pr, nil, false, nil, findings, &roundPlan{rung: rungAssessor, why: why})
	return false
}

const (
	// babysitRestNotice is how long a babysit rests in one wait before its
	// owner hears of it.
	babysitRestNotice = 24 * time.Hour
	// restTitle ends the notice's title; with the pull request's page it
	// keys the notice, once per wait.
	restTitle = ": nothing has moved it"
)

// noticeRest tells a babysit's owner, once per wait, that it has rested too
// long where nothing will move it (see above). Other needs-person waits were
// told to the owner when they began (babysitTell) or go to the assessor;
// waits a person already owns or was asked about (a parked question, a review
// gate, held comments, an outside pull request) aren't told again.
func (s *Service) noticeRest(ctx context.Context, b *Babysit, pr *PullRequest, idle time.Duration) {
	if b.State != BabysitWatching && b.State != BabysitNeedsPerson || parkedOn(b, pr.HeadSHA) {
		return
	}
	var detail string
	switch {
	case b.State == BabysitNeedsPerson && protectedWaitRE.MatchString(b.Wait):
		if idle < babysitStaleAfter {
			return
		}
		var moving bool
		if detail, moving = s.handoffStall(ctx, pr, b.Wait); moving {
			return
		}
	case b.State != BabysitWatching || personsWait(b.Wait) || idle < babysitRestNotice:
		return
	}
	link := "/agents?tab=pulls&pr=" + strconv.FormatInt(pr.ID, 10)
	title := fmt.Sprintf("Babysitting %s#%d%s", pr.Repo, pr.Number, restTitle)
	var told bool
	if s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM notifications WHERE user_id = ? AND url = ? AND title = ? AND created_at >= ?)`,
		b.ownerID, link, title, b.WaitSince.UnixMilli()).Scan(&told) != nil || told {
		return
	}
	body := fmt.Sprintf("%s#%d (%q) has waited %s and nothing has moved it: %s.", pr.Repo, pr.Number, pr.Title, idle.Round(time.Hour), strings.TrimSuffix(b.Wait, "."))
	if detail != "" {
		body += " " + detail
	}
	body += " It won't merge until that changes."
	s.log.Info("babysit rested", "repo", pr.Repo, "number", pr.Number, "for", idle.Round(time.Minute), "wait", b.Wait, "detail", detail)
	s.notifyURL(ctx, b.ownerID, nil, NotifyPR, title, body, link)
}

// personsWait: a wait a person owns or was already asked about.
func personsWait(wait string) bool {
	return strings.HasPrefix(wait, parkWait) || strings.HasPrefix(wait, "review gate: ") || strings.HasSuffix(wait, " asked for changes") ||
		strings.Contains(wait, personCommentsWait) || strings.HasPrefix(wait, "waiting for a person")
}

// handoffStall says why a protected-file handoff's task isn't moving the pull
// request on; moving: a session holds it now. The task is the one the
// handoff filed, found as fileProtectedPush finds it (by its title, in the
// pull request's project), not a key read from the wait: the wait carries
// file and check names others chose, and a key put there could aim the
// notice at another task.
func (s *Service) handoffStall(ctx context.Context, pr *PullRequest, wait string) (detail string, moving bool) {
	title := protectedPushTitle(pr)
	if protectedConflictRE.MatchString(wait) {
		title = protectedConflictTitle(pr)
	}
	var key string
	var id, claimedUntil int64
	var cat, claimedBy string
	projectID, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project of a stalled handoff", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
		return "Its task can't be read, so nothing is known to be doing it.", false
	}
	err = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number, i.id, st.category, i.claimed_by, coalesce(i.claimed_until, 0) FROM issues i JOIN projects p ON p.id = i.project_id
		JOIN statuses st ON st.id = i.status_id WHERE i.project_id = ? AND i.title = ? ORDER BY i.id DESC LIMIT 1`,
		projectID, title).Scan(&key, &id, &cat, &claimedBy, &claimedUntil)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "No task was filed for a session to do it (the repository is in no project, or its owner can't sign in), so nothing will pick it up: push the change from a session.", false
	case err != nil:
		return "Its task can't be read, so nothing is known to be doing it.", false
	case cat == CatDone || cat == CatCanceled:
		return fmt.Sprintf("Its task %s was closed, but the pull request hasn't moved since.", key), false
	case claimedBy != "" && claimedUntil > s.nowMS():
		return "", true
	}
	if why, held := s.safetyWait(ctx, s.db.R, &Issue{ID: id, Key: key}); held {
		return fmt.Sprintf("Its task %s waits for a person: the safety screen holds it. %s", key, why), false
	}
	return fmt.Sprintf("No session has taken its task %s: is autopilot on, with a free slot?", key), false
}

// staleWaitRevivable: a needs_person wait the watchdog may act on. A parked
// question already asks someone; a protected-file handoff already has a task
// for a session (refusedOutsideDiff makes the exception, for a refusal whose
// files the pull request doesn't change); a person's review gate or change
// request is theirs to lift.
func staleWaitRevivable(wait string) bool {
	return !strings.HasPrefix(wait, parkWait) && !strings.Contains(wait, "protected files") &&
		!strings.HasPrefix(wait, "review gate: ") && !strings.HasSuffix(wait, " asked for changes")
}

// refusedOutsideDiff: the babysit waits on a round's fix refused for
// protected files, and none of those files is one the pull request itself
// changes (WP-1043). Before WP-750 the runner refused a round that merged its
// base whenever the merge brought in CI files, though the round never edited
// them: a managed repo's #212, #216 and #217 waited a week on handoff tasks the
// safety screen held for a person. The task stays for a session, but such a
// fix needs no protected edit, so the watchdog gives it the assessor's round
// as for any other wait. assessorBlocked keeps that to one per head: a
// second refusal on it parks the babysit. Unknown files are not "not mine".
func (s *Service) refusedOutsideDiff(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	if !protectedWaitRE.MatchString(b.Wait) || protectedConflictRE.MatchString(b.Wait) {
		return false
	}
	r := lastRound(b)
	if r == nil {
		return false
	}
	files, ok := protectedRefusal(*r)
	if !ok || strings.TrimSpace(files) == "" {
		return false
	}
	s.loadPRFiles(ctx, pr)
	if !pr.filesKnown || pr.tooLarge || len(pr.files) == 0 {
		return false
	}
	for _, f := range strings.Split(files, ",") {
		if f = strings.TrimSpace(f); f == "" || slices.Contains(pr.files, f) {
			return false
		}
	}
	return true
}

// askAboutPersonComments files one request for review comments a person
// marked as needing a person: babysitTell only reaches a session, and by now
// it has usually ended. No agent may change a person's own verdict.
func (s *Service) askAboutPersonComments(ctx context.Context, b *Babysit, pr *PullRequest) {
	held := s.heldComments(ctx, b, pr)
	if len(held) == 0 {
		return
	}
	refs := make([]string, len(held))
	for i, rc := range held {
		refs[i] = rc.Ref
	}
	key := personRequestKey + strconv.FormatInt(b.ID, 10) + ":" + excerpt(strings.Join(refs, ", "), 120)
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? LIMIT 1`, key).Scan(&id) == nil {
		return
	}
	u, err := s.userByID(ctx, s.db.R, b.ownerID)
	if err != nil || u.Disabled {
		return
	}
	body := fmt.Sprintf("A person marked this on %s#%d (%q) as needing a person's call, so no agent may settle it, and the pull request can't merge until someone does.\n\n%s\n"+
		"To settle it, open %s and press Dismiss beside the comment. If you do nothing, the pull request stays as it is.",
		pr.Repo, pr.Number, pr.Title, quoteComments(held), s.BaseURL()+"/agents?tab=pulls&pr="+strconv.FormatInt(pr.ID, 10))
	in := RequestInput{Kind: RequestAction, Title: fmt.Sprintf("%s#%d needs your call on a review comment", pr.Repo, pr.Number), Body: body, ToolUseID: key, Followup: "never"}
	projectKey, err := s.attachedProjectKey(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project to ask about held review comments", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
		return
	}
	in.Project = projectKey
	if _, _, err := s.CreateRequest(ctx, &Actor{User: u, Agent: "waypoint/babysit"}, in); err != nil {
		s.log.WarnContext(ctx, "ask about review comments", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// unoffered are the late minor comments no round was given.
func unoffered(b *Babysit, late []string) []string {
	var out []string
	for _, ref := range late {
		if !slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return slices.Contains(r.Comments, ref) }) {
			out = append(out, ref)
		}
	}
	return out
}

// readyCatchUp brings a ready pull request nobody armed up to date with its
// base (WP-750). Armed ones are updated at the front of their merge queue;
// nothing updated these, so they sat ready and days behind. A few at a time
// per base, so a run of them doesn't take the whole CI pool. It reports
// whether the step is done; false leaves it ready as it is.
func (s *Service) readyCatchUp(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	if pr.ArmedAt != nil || pr.Outside || pr.State != "open" || (pr.headRepo != "" && !strings.EqualFold(pr.headRepo, pr.Repo)) {
		return false
	}
	ctx, unlock := s.queueLockCtx(ctx, pr)
	defer unlock()
	if s.readyUpdatesGoing(ctx, pr) >= readyUpdatesAtOnce {
		return false
	}
	base := cmp(pr.BaseRef, "its base")
	switch s.updateIfBehindThen(ctx, pr, nil) {
	case upUpdated:
		s.setState(ctx, b, pr, BabysitWatching, checksWait+" after merging "+base+" into it")
		return true
	case upLanded:
		s.setState(ctx, b, pr, BabysitWatching, "its work is already on "+base+"; finishing it")
		return true
	case upConflict:
		s.babysitRound(ctx, b, pr, nil, true, nil, nil)
		return true
	}
	return false
}

// readyUpdatesGoing counts the other unarmed pull requests of pr's base
// running checks. One whose run has said nothing for a while doesn't count:
// a stuck run mustn't hold the rest back.
func (s *Service) readyUpdatesGoing(ctx context.Context, pr *PullRequest) int {
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests p JOIN babysits b ON b.pr_id = p.id AND b.ended_at IS NULL
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ? AND p.armed_at IS NULL AND p.state = 'open'
		AND b.state = ? AND b.wait LIKE ? AND coalesce(b.wait_at, b.updated_at) >= ?`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, BabysitWatching, checksWait+"%", s.now().Add(-babysitStaleAfter).UnixMilli()).Scan(&n); err != nil {
		return readyUpdatesAtOnce
	}
	return n
}
