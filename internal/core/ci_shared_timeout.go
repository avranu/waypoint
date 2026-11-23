package core

// A shared timeout holds no merge line (WP-923, WP-ADR from WP-923). When
// go test's -timeout stops a package that every pull request runs, the
// front of the merge queue fails after the whole run, gets a flake re-run
// (another half hour) and then an agent round about code that isn't its
// own, while the pull request that would speed the suite up waits behind
// it: on 2026-11-11 shard 1 timed out on #366, #389 and #392 in turn. So a
// failure that is only go test timeouts, on jobs that timed out on other
// commits lately too, is the suite's, not the change's: no re-run, no
// round. An armed pull request gives up its turn at once and waits out of
// the line; the slow suite goes to the repo's CI limits task (WP-922) for
// an agent. When its base moves on, as a fix merging moves it, it takes its
// place in the line again and is tried on the new base when its turn
// comes, at most sharedTimeoutRetries times: after that its timeouts go the
// usual way, in case they are its own after all.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	// sharedTimeoutWindow is how recent the other timeouts must be.
	sharedTimeoutWindow = 6 * time.Hour
	// sharedTimeoutOthers is how many other commits must have timed out the
	// same jobs.
	sharedTimeoutOthers = 2
	// sharedTimeoutRetries: a pull request is tried on a moved base this
	// many times before its timeouts go the usual way.
	sharedTimeoutRetries = 3
)

// sharedTimeoutEvidence is why a head's timeouts are taken as the suite's.
type sharedTimeoutEvidence struct {
	checks []string // the head's checks go test timed out in
	others []string // the other commits that timed out the same jobs: "#366", "main d145047"
}

// sharedTimeoutOf reads whether every failed check of pr's head is a go
// test timeout, on jobs that timed out on at least sharedTimeoutOthers other
// commits in the last sharedTimeoutWindow; nil when not.
func (s *Service) sharedTimeoutOf(ctx context.Context, pr *PullRequest) *sharedTimeoutEvidence {
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? AND c.state = 'failed'`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil
	}
	ev := &sharedTimeoutEvidence{}
	kinds := map[string]bool{}
	for rows.Next() {
		var name, log string
		if err := rows.Scan(&name, &log); err != nil {
			rows.Close()
			return nil
		}
		if !goTimeoutRE.MatchString(log) {
			if derivedFailure(log) {
				continue // it failed because a shard did
			}
			rows.Close()
			return nil
		}
		if !onlyGoTimeouts(cleanLog(log)) {
			rows.Close()
			return nil // a test failed too, or a build: that is the change's
		}
		ev.checks = append(ev.checks, name)
		kinds[evidenceKind(checkJobName(name))] = true
	}
	rows.Close()
	if rows.Err() != nil || len(ev.checks) == 0 {
		return nil
	}
	slices.Sort(ev.checks)
	since := s.now().Add(-sharedTimeoutWindow).UnixMilli()
	seen := map[string]bool{}
	note := func(sha, label string) {
		if sha == "" || sameSHA(sha, pr.HeadSHA) || seen[short(sha)] {
			return
		}
		seen[short(sha)] = true
		ev.others = append(ev.others, label)
	}
	// Other open pull requests' heads, as their checks' kept logs say.
	other, err := s.db.R.QueryContext(ctx, `SELECT p.number, h.sha, c.name FROM pull_requests p
		JOIN ci_heads h ON h.forge_id = p.forge_id AND h.repo = p.repo COLLATE NOCASE AND h.sha = p.head_sha
		JOIN ci_checks c ON c.head_id = h.id
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.id != ? AND p.state IN ('open','draft')
		  AND c.state = 'failed' AND c.updated_at >= ? AND c.log LIKE '%panic: test timed out after%'`,
		pr.ForgeID, pr.Repo, pr.ID, since)
	if err == nil {
		for other.Next() {
			var n int
			var sha, name string
			if other.Scan(&n, &sha, &name) == nil && kinds[evidenceKind(checkJobName(name))] {
				note(sha, fmt.Sprintf("#%d", n))
			}
		}
		other.Close()
	}
	// Any commit's jobs, as their logs' package times say (WP-922): the
	// base's push runs and pull requests' earlier heads.
	jobs, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT j.sha, j.event, j.ref, j.name FROM ci_job_packages p
		JOIN ci_jobs j ON j.forge_id = p.forge_id AND j.repo = p.repo AND j.task_id = p.task_id
		WHERE p.forge_id = ? AND p.repo = ? AND p.outcome = 'timeout' AND p.at >= ? ORDER BY p.at DESC`,
		pr.ForgeID, pr.Repo, since)
	if err == nil {
		own := fmt.Sprintf("#%d", pr.Number)
		for jobs.Next() {
			var sha, event, ref, name string
			if jobs.Scan(&sha, &event, &ref, &name) != nil || !kinds[evidenceKind(name)] {
				continue
			}
			if ref == own || strings.Contains(ref, fmt.Sprintf("pull/%d/", pr.Number)) {
				continue // its own earlier heads say nothing about the others
			}
			if strings.Contains(ref, stackPrefix) {
				continue // a merge-queue stack may hold this very change
			}
			label := short(sha)
			if ref != "" {
				label = ref + " " + short(sha)
			}
			note(sha, label)
		}
		jobs.Close()
	}
	if len(ev.others) < sharedTimeoutOthers {
		return nil
	}
	return ev
}

// onlyGoTimeouts: every failure in a check's cleaned log is a package go
// test's -timeout stopped. A test that failed, a package that failed without
// running into its limit, or a build that failed is the change's, even in a
// job that also timed out.
func onlyGoTimeouts(lines []string) bool {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "--- FAIL:") || goBuildFailedRE.MatchString(t) {
			return false
		}
	}
	timedOut := false
	for _, p := range goPackages(lines, nil) {
		switch p.outcome {
		case "fail":
			return false
		case "timeout":
			timedOut = true
		}
	}
	return timedOut
}

// goBuildFailedRE is go test's line for a package that didn't build:
// "FAIL\tgit.example.com/me/app/x [build failed]".
var goBuildFailedRE = lazyre.New(`^FAIL\s+\S+\s+\[(?:build|setup) failed\]`)

// sharedTimeoutState is a babysit's waits on shared timeouts, kept as a
// server setting while it runs.
type sharedTimeoutState struct {
	SHA   string `json:"sha"`             // the head last held by one
	Tip   string `json:"tip"`             // the base tip it was held at, or last tried on
	Retry bool   `json:"retry,omitempty"` // back in the line to be tried on a moved base
	Tries int    `json:"tries,omitempty"` // updates made to try it on a moved base
}

const sharedTimeoutPrefix = "ci_shared_timeout:"

func sharedTimeoutKey(babysitID int64) string {
	return sharedTimeoutPrefix + strconv.FormatInt(babysitID, 10)
}

func (s *Service) sharedTimeoutState(ctx context.Context, b *Babysit) sharedTimeoutState {
	var st sharedTimeoutState
	if v, err := s.serverSetting(ctx, s.db.R, sharedTimeoutKey(b.ID)); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &st)
	}
	return st
}

func (s *Service) saveSharedTimeout(ctx context.Context, b *Babysit, st sharedTimeoutState) {
	if v, err := json.Marshal(st); err == nil {
		s.putSetting(ctx, sharedTimeoutKey(b.ID), string(v))
	}
}

// sharedTimeoutOver forgets a babysit's shared timeouts once its checks pass.
func (s *Service) sharedTimeoutOver(ctx context.Context, b *Babysit) {
	if st := s.sharedTimeoutState(ctx, b); st.SHA != "" || st.Tries > 0 {
		s.dropSetting(ctx, sharedTimeoutKey(b.ID))
	}
}

// waitOnSharedTimeout is the step of an armed pull request whose head
// failed only on go test timeouts the suite shares (sharedTimeoutOf): out of
// the merge line while its base stays, back in it to be tried again when
// the base moves. false when its failure goes the usual way.
func (s *Service) waitOnSharedTimeout(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	if pr.ArmedAt == nil || pr.Outside {
		return false
	}
	st := s.sharedTimeoutState(ctx, b)
	if st.Tries >= sharedTimeoutRetries {
		return false
	}
	ev := s.sharedTimeoutOf(ctx, pr)
	if ev == nil {
		return false
	}
	base := cmp(pr.BaseRef, "its base")
	tip, behind, stale := s.behindBase(ctx, pr)
	what := fmt.Sprintf("go test timed out in %s, as it did lately on %s: the suite is slow, not this change", joinAnd(ev.checks), joinAnd(ev.others))
	// st.Tip is "" until its first hold: then it has not moved since one.
	if behind && !stale && tip != "" && (st.Retry || st.Tip != "" && !sameSHA(tip, st.Tip)) {
		// The base moved on since it was held (perhaps with a fix): back in
		// the line once the one in its full run (perhaps the fix) is
		// through, and at the front brought up to date and tried again.
		if !st.Retry {
			st.Retry = true
			s.saveSharedTimeout(ctx, b, st)
		}
		// Not while it merges after another: it would hold the line (WP-912).
		if waits, _ := s.mergeWaitReasons(ctx, pr); len(waits) > 0 {
			s.setState(ctx, b, pr, BabysitWatching, what+"; "+waits[0])
			return true
		}
		var unlock func()
		ctx, unlock = s.queueLockCtx(ctx, pr)
		defer unlock()
		ahead, err := s.queueAhead(ctx, pr)
		if err != nil {
			// Its place unread, it isn't the front's to take: the branch
			// stays as it is and the next look reads the line again.
			s.log.WarnContext(ctx, "read the merge line for a shared timeout's retry", "pr", pr.ID, "err", err)
			s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("%s; %s moved on, so it is tried again on it at its turn (its place in the merge line couldn't be read; looking again)", what, base))
			return true
		}
		if len(ahead) > 0 {
			s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("%s; %s moved on, so it is tried again on it at its turn, behind #%d", what, base, ahead[0].Number))
			return true
		}
		// Marked ready first, as at the front of the queue, so the update's
		// push runs the full tier once (WP-456).
		switch s.updateIfBehindThen(ctx, pr, func() { s.readyForUpdate(ctx, pr) }) {
		case upUpdated:
			st.Retry, st.Tip, st.Tries = false, tip, st.Tries+1
			s.saveSharedTimeout(ctx, b, st)
			fallthrough
		case upWaiting:
			// Its turn in the line comes from the line, not from here: a pull
			// request held out of the queue isn't "back" in it.
			if pr.QueuedAt == nil {
				s.setState(ctx, b, pr, BabysitWatching, what+"; tried again on the new "+base+": merged "+base+" into it")
				return true
			}
			s.setState(ctx, b, pr, BabysitWatching, what+"; front of the merge queue again: merged "+base+" into it to try it on the new base")
			return true
		case upToDate:
		default:
			return false // a conflict or a failed update: the usual way
		}
	}
	// Held where it is: out of the line, so the one behind (perhaps the fix)
	// gets its turn.
	// The tip it is held at: the first hold's even when behind, so only a
	// later move sends it back into the line.
	newTip := tip != "" && (st.Tip == "" || !behind && !sameSHA(st.Tip, tip))
	if !sameSHA(st.SHA, pr.HeadSHA) || st.Retry || newTip {
		st.SHA, st.Retry = pr.HeadSHA, false
		if newTip {
			st.Tip = tip
		}
		s.saveSharedTimeout(ctx, b, st)
	}
	if pr.QueuedAt != nil {
		s.leaveQueue(ctx, pr)
		pr.QueuedAt = nil
		s.log.Info("a pull request whose tests timed out like the others' gave up its turn in the merge queue", "pr", pr.ID, "head", pr.HeadSHA)
		s.nudgeLine(ctx, pr)
	}
	key := "timeout@" + pr.HeadSHA
	task := s.ciLimitsTask(ctx, pr.ForgeID, pr.Repo)
	if b.told != key {
		if err := s.setBabysit(ctx, b, map[string]any{"told": key}); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "set babysit failed", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
			}
		}
		b.told = key
		seen := fmt.Sprintf("%s held by it: %s timed out on %s (%s), as on %s; it waits out of the merge line for %s to move on.\n",
			faultCode(fmt.Sprintf("%s#%d", pr.Repo, pr.Number)), faultCode(joinAnd(ev.checks)), short(pr.HeadSHA), faultCode(pr.URL), faultCode(joinAnd(ev.others)), base)
		ls, err := s.ciLimitsForPR(ctx, pr)
		if err != nil {
			s.log.Debug("CI limits for a shared timeout", "pr", pr.ID, "err", err)
		}
		var rerr error
		task, rerr = s.fileCILimits(ctx, pr.ForgeID, pr.Repo, "problem", ls, seen)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "file cilimits failed", "forgeid", pr.ForgeID, "err", rerr)
			}
		}
		if b.sessionID != 0 {
			tracked := ""
			if task != "" {
				tracked = " " + task + " tracks the slow suite."
			}
			// The check names and job refs are the forge's text: quoted, as
			// in the task.
			text := fmt.Sprintf("%s#%d: go test timed out in %s, as it did lately on %s: the suite is slow, not this change. Waypoint starts no round for it and it waits out of the merge line until %s moves on; don't push to re-run it.%s",
				pr.Repo, pr.Number, faultCode(joinAnd(ev.checks)), faultCode(joinAnd(ev.others)), base, tracked)
			if _, err := s.queueMessage(ctx, b.sessionID, text, "babysit:"+strconv.FormatInt(b.ID, 10)+":"+key, messageFrom{}, true); err != nil {
				s.log.WarnContext(ctx, "tell a session about its babysit", "babysit", b.ID, "err", err)
			}
		}
	}
	wait := what + "; out of the merge line until " + base + " moves on"
	if task != "" {
		wait += " (" + task + " tracks the slow suite)"
	}
	s.setState(ctx, b, pr, BabysitWatching, wait)
	return true
}

// ciLimitsForPR is the limits of pr's repo, for the task's comment.
func (s *Service) ciLimitsForPR(ctx context.Context, pr *PullRequest) ([]ciLimit, error) {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return nil, err
	}
	return s.ciLimitsOf(ctx, f, pr.Repo)
}
