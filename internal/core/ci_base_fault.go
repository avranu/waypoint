package core

// Runner faults on a base branch (WP-551). Waypoint re-runs a babysat pull
// request's runner-fault jobs (waitForRunner), but a push run on a base
// branch that failed the same way stayed red until a person noticed, and
// until it passed its commit wasn't released or deployed: on 2026-11-04
// one repo's main was red for 8h37m and another's
// release branch for 10h22m, both because a runner's DNS failed at
// checkout. Every time the scheduler reads the CI history (ten minutes),
// the newest push runs of each attached repo's default branch and release
// branches are looked at: when they failed and every job that failed did
// so on its runner, Waypoint re-runs those jobs on a pull request head's
// schedule (ci_fault_retry.go). One that keeps coming back is tracked in the
// one runner-fault task a pull request would file (WP-589) and the re-runs go
// on; only a forge that can't be asked ends them. The count is kept in
// server_settings, so a restart doesn't start it over, and only a branch's
// newest push is ever re-run, for baseFaultWindow.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	baseFaultKey    = "base_fault:"  // server_settings, + forge id|repo|branch: a baseFault
	baseFaultWindow = 24 * time.Hour // push runs older than this are left alone
)

// baseFault is a base branch tip's runner-fault re-runs: faultRerun's
// fields, then what became of it, if anything: baseFaultAsked once the forge
// couldn't be asked and it was filed, baseFaultOther when the tip failed some
// other way (the doctor's main_red reports it; nothing is re-run), or
// baseFaultReported once it is tracked in a task (the re-runs go on).
type baseFault struct {
	faultRerun
	ended string
}

const (
	baseFaultAsked    = "asked"
	baseFaultOther    = "other"
	baseFaultReported = "reported"
)

func parseBaseFault(v string) baseFault {
	f := strings.Fields(v)
	if len(f) < 4 {
		return baseFault{}
	}
	var b baseFault
	switch f[len(f)-1] {
	case baseFaultAsked, baseFaultOther, baseFaultReported:
		b.ended = f[len(f)-1]
		f = f[:len(f)-1]
	}
	b.faultRerun = parseFaultRerun(strings.Join(f, " "))
	return b
}

func (b baseFault) String() string {
	return strings.TrimSpace(b.faultRerun.String() + " " + b.ended)
}

func baseFaultSettingKey(forgeID int64, repo, branch string) string {
	return fmt.Sprintf("%s%d|%s|%s", baseFaultKey, forgeID, strings.ToLower(repo), branch)
}

// branchDefaulter is the part of a forge client that names a repo's
// default branch.
type branchDefaulter interface {
	DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error)
}

// checkBaseFaults re-runs the runner-fault jobs of the newest push runs of
// repo's default and release branches, or files a task about them. fresh
// are the runs the forge listed just now; the kept history (ci_runs) adds
// a quiet branch's older ones.
func (s *Service) checkBaseFaults(ctx context.Context, f *Forge, repo string, fresh map[int64]ActionRun) {
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok {
		return
	}
	runs := s.basePushRuns(ctx, f.ID, repo, fresh)
	states := s.baseFaults(ctx, f.ID, repo)
	branches := slices.Sorted(maps.Keys(runs))
	for b := range states {
		if _, ok := runs[b]; !ok {
			branches = append(branches, b)
		}
	}
	def, defKnown := "", false
	for _, branch := range branches {
		key := baseFaultSettingKey(f.ID, repo, branch)
		old, had := states[branch]
		red, running := redTip(runs[branch])
		if running {
			continue // a push or a re-run under way: its result decides
		}
		if len(red) == 0 {
			if had {
				s.dropSetting(ctx, key)
			}
			continue
		}
		st := old
		if st.sha != red[0].CommitSHA {
			st = baseFault{faultRerun: faultRerun{sha: red[0].CommitSHA, first: s.nowMS()}}
		}
		if st.ended == baseFaultAsked || st.ended == baseFaultOther {
			continue
		}
		if !strings.HasPrefix(branch, "release/") {
			if !defKnown {
				defKnown = true
				def = "main"
				if dc, ok := s.Forges.(branchDefaulter); ok {
					d, err := dc.DefaultBranch(ctx, f, repo)
					switch {
					case err != nil:
						def = "" // lookup failed: only release branches this pass
					case d != "":
						def = d
					}
				}
			}
			if def == "" || branch != def {
				continue
			}
		}
		s.handleBaseFault(ctx, cl, f, repo, branch, red, &st, func() error { return s.keepSetting(ctx, key, st.String()) })
		if !had || st != old {
			s.putSetting(ctx, key, st.String())
		}
	}
}

// basePushRuns are repo's push runs of the last baseFaultWindow, by branch.
func (s *Service) basePushRuns(ctx context.Context, forgeID int64, repo string, fresh map[int64]ActionRun) map[string][]ActionRun {
	since := s.now().Add(-baseFaultWindow)
	byNum := map[int64]ActionRun{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT number, workflow, ref, sha, status, created_at FROM ci_runs
		WHERE forge_id = ? AND repo = ? AND event = 'push' AND created_at >= ?`, forgeID, repo, since.UnixMilli())
	if err == nil {
		for rows.Next() {
			var r ActionRun
			var created int64
			if rows.Scan(&r.Index, &r.WorkflowID, &r.PrettyRef, &r.CommitSHA, &r.Status, &created) == nil {
				r.Event, r.Created = "push", msTime(created)
				byNum[r.Index] = r
			}
		}
		rows.Close()
	}
	for _, r := range fresh {
		if r.Event == "push" && validTime(r.Created) && r.Created.After(since) {
			byNum[r.Index] = r
		}
	}
	out := map[string][]ActionRun{}
	for _, r := range byNum {
		if r.PrettyRef != "" && r.CommitSHA != "" {
			out[r.PrettyRef] = append(out[r.PrettyRef], r)
		}
	}
	return out
}

// redTip is the failed runs of a branch's newest pushed commit, each its
// workflow's newest run of it, or running when one of them hasn't ended.
// Runs of older commits are never in it: a newer push replaced them.
func redTip(runs []ActionRun) (red []ActionRun, running bool) {
	if len(runs) == 0 {
		return nil, false
	}
	runs = slices.Clone(runs)
	slices.SortFunc(runs, func(a, b ActionRun) int { return int(b.Index - a.Index) })
	tip := runs[0].CommitSHA
	seen := map[string]bool{}
	for _, r := range runs {
		if r.CommitSHA != tip || r.Status == "skipped" || seen[r.WorkflowID] {
			continue
		}
		seen[r.WorkflowID] = true
		switch {
		case ciPending(r.Status):
			return nil, true
		case r.Status == "failure" || r.Status == "cancelled":
			red = append(red, r)
		}
	}
	return red, false
}

// handleBaseFault reads the red runs' failed jobs again and, when every
// failure is a runner fault, re-runs them, waits for their host, or asks a
// person. st is updated in place; record keeps it, and must before a re-run
// is asked for.
func (s *Service) handleBaseFault(ctx context.Context, cl ForgeActionLogs, f *Forge, repo, branch string, red []ActionRun, st *baseFault, record func() error) {
	var runs []ActionRun
	faults := map[string]*RunnerFault{}
	urls := map[string]string{}
	hostFrom := st.first // the host's waits count from the earliest failure; st.first stays when the tip was first seen red
	for _, r := range red {
		cur, err := cl.ActionRunNumber(ctx, f, repo, r.Index)
		if err != nil {
			return // read again next time
		}
		if ciPending(cur.Status) || !sameSHA(cur.CommitSHA, st.sha) {
			return // a re-run is under way
		}
		if cur.Status != "failure" && cur.Status != "cancelled" {
			continue
		}
		if st.n == 0 && validTime(cur.Stopped) && cur.Stopped.UnixMilli() < hostFrom {
			hostFrom = cur.Stopped.UnixMilli()
		}
		jobs, err := cl.ActionJobs(ctx, f, repo, cur.ID)
		if err != nil {
			return
		}
		stopped := cur.Stopped
		if !validTime(stopped) {
			stopped = msTime(st.first) // no stop time: count from when the tip was first seen red
		}
		if len(jobs) == 0 && s.now().Sub(stopped) < 5*time.Minute {
			return // the forge may not list the jobs of a run that just ended yet; read again next time
		}
		hasFault := false
		for _, j := range jobs {
			if j.Status != "failure" && j.Status != "cancelled" {
				continue
			}
			if j.Name == advisoriesJob {
				// A vulnerability is the dependency's to fix, never a runner
				// fault, and it mustn't hide one in the same run. Nothing
				// waits for the job, so one task says someone must (WP-1660).
				if j.Status == "failure" {
					s.fileAdvisories(ctx, f, repo, branch, st.sha, cur)
				}
				continue
			}
			log, err := cl.ActionJobLog(ctx, f, repo, j.ID)
			if err != nil {
				return
			}
			if strings.TrimSpace(log) == "" && s.now().Sub(stopped) < 5*time.Minute {
				return // the forge may not serve the log of a job that just ended yet; read again next time
			}
			if fault := logRunnerFault(log); fault != nil {
				s.nameFaultHost(ctx, f.ID, fault)
				name := cur.WorkflowID + " " + j.Name
				faults[name], urls[name] = fault, cur.HTMLURL
				hasFault = true
				continue
			}
			if derivedFailure(log) || (j.Status == "cancelled" && !stepFailedLog(log)) {
				continue // a shard's failure, or stopped by another job's
			}
			st.ended = baseFaultOther // the code's (or a step's own) failure
			return
		}
		if hasFault {
			runs = append(runs, *cur) // a run without a runner fault (cancelled by a person) is left alone
		}
	}
	if len(faults) == 0 {
		st.ended = baseFaultOther // only cancelled, by a person or a newer run
		return
	}
	nums := make([]string, len(runs))
	for i, r := range runs {
		nums[i] = fmt.Sprintf("#%d", r.Index)
	}
	at := faultSubject{forgeID: f.ID, repo: repo, name: fmt.Sprintf("%s %s run %s", repo, branch, strings.Join(nums, ", ")), url: runs[0].HTMLURL, agent: "waypoint/ci"}
	seen := func() {
		for name, fault := range faults {
			s.runnerFaults.add(seenFault{at: s.now(), forgeID: f.ID, repo: repo, check: branch + " " + name, url: urls[name], fault: *fault})
		}
	}
	file := func(why, ended string) {
		seen()
		s.log.WarnContext(ctx, "a base branch's push run failed on its runner; tracking it in a task", "repo", repo, "branch", branch, "sha", short(st.sha), "why", why)
		if owner := s.baseFaultOwner(ctx, f.ID, repo, branch, st.sha); owner != 0 {
			if _, ok := s.reportRunnerFault(ctx, owner, at, faults, why); ok {
				st.ended = ended // else filed again next pass
			}
		}
	}
	report := func(why string) {
		if why != "" && st.ended != baseFaultReported && !timeLimitOnly(faults) {
			file(why, baseFaultReported)
		}
	}
	st.noteKinds(faultKinds(faults))
	rc, _ := s.Forges.(ForgeRunRetry)
	if why := s.rerunRefused(ctx, f, rc); why != "" {
		if timeLimitOnly(faults) {
			st.ended = baseFaultOther
			return
		}
		file(why, baseFaultAsked)
		return
	}
	now := s.now()
	if st.last > 0 && now.Sub(msTime(st.last)) < faultRerunSettle {
		return // the forge hasn't taken up the re-run yet
	}
	if timeLimitOnly(faults) && st.streak >= faultTimeLimitReruns {
		st.ended = baseFaultOther // the job's to fix; the doctor's job_time_limit and main_red say so
		return
	}
	if st.n >= faultReportAfter {
		report(faultReportWhy(st.faultRerun, now))
	}
	if now.Before(st.nextTry()) {
		return
	}
	_, wait, sickFor := s.faultHosts(ctx, f.ID, faults, now.Sub(msTime(max(hostFrom, st.last))))
	report(sickFor)
	if wait {
		return
	}
	// The attempt is recorded before the forge is asked: one whose back-off
	// went unrecorded would be asked for again every pass, the load on a sick
	// runner the back-off is for. A refused or partial one counts too: the
	// runs that did restart are followed, the rest are tried again once those
	// end, and a forge that keeps refusing backs off.
	seen()
	prev := *st
	st.n++
	st.streak++
	st.last = now.UnixMilli()
	if record() != nil {
		*st = prev // logged; tried again next pass
		return
	}
	n, ran := 0, 0
	var rerr error
	for _, r := range runs {
		m, err := rc.RetryRun(ctx, f, repo, r.ID)
		n += m
		if err != nil {
			rerr = err
			break
		}
		ran++
	}
	if rerr != nil && ran == 0 && n == 0 {
		s.log.WarnContext(ctx, "re-run a base branch's runner-fault jobs", "repo", repo, "branch", branch, "sha", short(st.sha), "err", rerr)
	}
	s.log.Info("re-ran a base branch's runner-fault jobs", "repo", repo, "branch", branch, "sha", short(st.sha), "jobs", n, "rerun", st.n, "faults", faultSummary(faults))
}

// baseFaultOwner is who is asked about a base branch's runner fault: whoever
// a babysit of the pull request that made the commit (else the branch's
// latest merge) would belong to, else an owner of the repo's project.
func (s *Service) baseFaultOwner(ctx context.Context, forgeID int64, repo, branch, sha string) int64 {
	// #nosec G202 -- a fixed column list; values are bound
	row := s.db.R.QueryRowContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.state = 'merged'
		ORDER BY (p.merge_sha = ?) DESC, p.merged_at DESC LIMIT 1`, forgeID, repo, branch, sha)
	if pr, err := scanPR(row); err == nil {
		if id := s.babysitOwnerFor(ctx, pr); id != 0 {
			return id
		}
	}
	return s.repoProjectOwner(ctx, forgeID, repo)
}

// repoProjectOwner is an owner of the repo's project (0: none).
func (s *Service) repoProjectOwner(ctx context.Context, forgeID int64, repo string) int64 {
	projectID, err := s.attachedProject(ctx, s.db.R, forgeID, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project to find its owner", "repo", repo, "err", err) // 0: nobody this pass
		}
		return 0
	}
	var id int64
	switch err := s.db.R.QueryRowContext(ctx, `SELECT u.id FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? AND m.role = 'owner' AND u.disabled = 0 ORDER BY m.user_id LIMIT 1`, projectID).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0
	case err != nil:
		if readFault(err) {
			s.log.WarnContext(ctx, "read the owner of a repo's project", "repo", repo, "err", err)
		}
		return 0
	}
	return id
}

// baseFaults are repo's kept base-branch fault states, by branch.
func (s *Service) baseFaults(ctx context.Context, forgeID int64, repo string) map[string]baseFault {
	prefix := baseFaultSettingKey(forgeID, repo, "")
	out := map[string]baseFault{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT key, value FROM server_settings WHERE key >= ? AND key < ?`, prefix, prefix+"\xff")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[strings.TrimPrefix(k, prefix)] = parseBaseFault(v)
		}
	}
	return out
}

func (s *Service) putSetting(ctx context.Context, key, value string) {
	if err := s.keepSetting(ctx, key, value); err != nil { // logged
		if readFault(err) {
			s.log.WarnContext(ctx, "keep setting failed", "err", err)
		}
	}
}

// keepSetting is putSetting for a caller that must know the value is kept.
func (s *Service) keepSetting(ctx context.Context, key, value string) error {
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		s.log.WarnContext(ctx, "keep a setting", "key", key, "err", err)
	}
	return err
}

func (s *Service) dropSetting(ctx context.Context, key string) {
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM server_settings WHERE key = ?`, key); err != nil {
		s.log.WarnContext(ctx, "drop a setting", "key", key, "err", err)
	}
}

// advisoriesTaskTitle is the task about the advisories on a base branch, and
// how a later failure finds the open one.
func advisoriesTaskTitle(repo, branch string) string {
	return excerpt("Fix the advisories on "+repo+" "+branch, 200)
}

// fileAdvisories files the one urgent task about a base branch whose
// advisories job failed (WP-1660): no release, deploy or pull request waits
// for that job, so without a task nobody is asked to act. While one is open
// nothing more is filed; a later failure after it closed files the next.
func (s *Service) fileAdvisories(ctx context.Context, f *Forge, repo, branch, sha string, run *ActionRun) {
	title := advisoriesTaskTitle(repo, branch)
	var open string
	err := s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.title = ? AND st.category NOT IN ('done','canceled') LIMIT 1`, title).Scan(&open)
	if err == nil {
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.log.WarnContext(ctx, "look for the open task about a branch's advisories", "repo", repo, "branch", branch, "err", err)
		return
	}
	projectID, err := s.attachedProject(ctx, s.db.R, f.ID, repo)
	if err != nil || projectID == 0 {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project of a repo with advisories", "repo", repo, "err", err)
		}
		return
	}
	ownerID := s.baseFaultOwner(ctx, f.ID, repo, branch, sha)
	if ownerID == 0 {
		return
	}
	owner, err := s.userByID(ctx, s.db.R, ownerID)
	if err != nil || owner.Disabled {
		return
	}
	writer := Actor{User: owner}
	writer.textAgent = "waypoint"
	text := fmt.Sprintf("The `%s` job (npm audit, govulncheck) failed on %s %s at %s (%s): a dependency there has a known vulnerability. "+
		"No release, deploy or pull request waits for that job, so this task is what asks for the fix.\n\n"+
		"1. Read what it found: `wp ci log --repo %s --run %d --job %s`.\n"+
		"2. Upgrade or replace the dependency in a pull request, and check the job's commands pass on it (`deploy/advisories.sh check go main`, `npm run audit` in web/).\n"+
		"3. No fixed release exists: say so here with the evidence and ask a person (`wp request ask`). An exception to an advisory is a person's decision, never an agent's.\n\n"+
		"While this task is open Waypoint files no other for the same branch.\n",
		advisoriesJob, faultCode(repo), faultCode(branch), faultCode(short(sha)), faultCode(run.HTMLURL), repo, run.Index, advisoriesJob)
	made, err := s.CreateIssue(ctx, &writer, IssueInput{Project: itoa64(projectID), Type: "bug", Agent: "claude", Priority: "urgent", Labels: []string{"ci"}, Title: title, Description: text})
	if err != nil {
		s.log.WarnContext(ctx, "file the task about a branch's advisories", "repo", repo, "branch", branch, "err", err)
		return
	}
	s.log.Info("a base branch's advisories job failed; filed a task", "repo", repo, "branch", branch, "task", made.Key)
}
