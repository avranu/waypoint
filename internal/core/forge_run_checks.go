package core

// Several Actions runs can share a head, so each job's check takes its state from one run per workflow and event, not the newest status per name.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// runGroup: runs of one workflow file and event replace each other; a push
// run and a pull_request run post different check names.
func runGroup(r ActionRun) string { return r.WorkflowID + "\x00" + r.Event }

// countingRuns is the run that counts of each group of a head's runs, by
// runGroup: the newest by run number whose status isn't "skipped", which
// Forgejo gives a run whose every job skipped (an event the workflow
// ignores, such as another label or an edit of a draft). When every run of
// a group skipped, the newest counts. A run in progress counts.
func countingRuns(runs []ActionRun) map[string]ActionRun {
	out := map[string]ActionRun{}
	for _, r := range runs {
		k := runGroup(r)
		if o, ok := out[k]; !ok || runOutranks(r, o) {
			out[k] = r
		}
	}
	return out
}

// runOutranks: r counts rather than o, of one group.
func runOutranks(r, o ActionRun) bool {
	if rs, os := r.Status == "skipped", o.Status == "skipped"; rs != os {
		return os
	}
	if r.Index != o.Index {
		return r.Index > o.Index
	}
	return r.ID > o.ID
}

// fgStatusJob is where an Actions status points: run number and job index
// (-1 when the URL names no job).
type fgStatusJob struct {
	run int64
	job int
}

// fgStatusRun reads an Actions status's target URL, if it is a run of repo.
func fgStatusRun(target, repo string) (fgStatusJob, bool) {
	m := fgActionsURL.FindStringSubmatch(target)
	if m == nil || !strings.EqualFold(m[1], repo) {
		return fgStatusJob{}, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return fgStatusJob{}, false
	}
	j := -1
	if m[3] != "" {
		if j, err = strconv.Atoi(m[3]); err != nil {
			j = -1
		}
	}
	return fgStatusJob{run: n, job: j}, true
}

// fgRunChecks takes each Actions job's check on a Forgejo or Gitea head from
// the run that counts. combined is the head's combined status: it names the
// checks (a job that never posted a status isn't added), and the statuses no
// Actions job of this repo posted stay as they are.
//
// A check's job is the one its newest status in that run points at or, when
// it posted none there (Forgejo posts none for some skips), the job in that
// run whose name it carries: "ci / release (pull_request)" is job "release".
// Its state is that job's: a job the run skipped is skipped, whatever an
// older run said. A check whose name has no job in that run is left out.
func (c *httpForgeClient) fgRunChecks(ctx context.Context, f *Forge, repo, sha string, combined []CommitCheck) ([]CommitCheck, error) {
	if !slices.ContainsFunc(combined, func(ch CommitCheck) bool { _, ok := fgStatusRun(ch.URL, repo); return ok }) {
		return combined, nil
	}
	runs, err := c.ActionRunsAt(ctx, f, repo, sha)
	switch {
	case errors.Is(err, ErrNotFound) || isStatus(err, http.StatusNotFound):
		return combined, nil
	case err != nil:
		return nil, err // no guessing a green from part of the data
	}
	byNumber := map[int64]ActionRun{}
	for _, r := range runs {
		byNumber[r.Index] = r
	}
	counting := countingRuns(runs)
	jobs := map[int64][]ActionJob{} // Status URLs number jobs by their id order, so jobs are kept sorted by id.
	var history []CommitCheck
	historyRead := false
	out := make([]CommitCheck, 0, len(combined))
	for _, ch := range combined {
		at, ok := fgStatusRun(ch.URL, repo)
		run, listed := byNumber[at.run]
		if !ok || !listed {
			out = append(out, ch)
			continue
		}
		cr := counting[runGroup(run)]
		st, posted := ch, run.ID == cr.ID
		if !posted {
			if !historyRead {
				if history, err = c.statusHistory(ctx, f, repo, sha); err != nil {
					return nil, err
				}
				historyRead = true
			}
			st, posted = newestStatusIn(history, ch.Name, cr.Index, repo)
		}
		js, read := jobs[cr.ID]
		if !read {
			if js, err = c.ActionJobs(ctx, f, repo, cr.ID); err != nil {
				return nil, err
			}
			slices.SortFunc(js, func(a, b ActionJob) int { return cmpInt64(a.ID, b.ID) })
			jobs[cr.ID] = js
		}
		j := -1
		if posted {
			if p, _ := fgStatusRun(st.URL, repo); p.job >= 0 && p.job < len(js) {
				j = p.job
			}
		}
		if j < 0 {
			j = slices.IndexFunc(js, func(x ActionJob) bool { return strings.HasSuffix(ch.Name, " / "+x.Name+" ("+cr.Event+")") })
		}
		if j < 0 {
			continue
		}
		state := checkState(js[j].Status)
		if !posted {
			// ID is the head's newest status of this name, so a freshness
			// check comparing status ids doesn't take the job for stale.
			st = CommitCheck{ID: ch.ID, Created: ch.Created, Name: ch.Name, Source: ch.Source, URL: fmt.Sprintf("%s/jobs/%d", strings.TrimRight(cr.HTMLURL, "/"), j)}
		}
		if !posted || st.State != state {
			st.Description = jobDescription(js[j].Status)
		}
		st.State = state
		out = append(out, st)
	}
	return out, nil
}

// newestStatusIn is the newest status of context name that run number run
// of repo posted.
func newestStatusIn(history []CommitCheck, name string, run int64, repo string) (CommitCheck, bool) {
	var best CommitCheck
	found := false
	for _, h := range history {
		if at, ok := fgStatusRun(h.URL, repo); h.Name != name || !ok || at.run != run {
			continue
		}
		if !found || h.ID > best.ID {
			best, found = h, true
		}
	}
	return best, found
}

// jobDescription says what an Actions job's status means, as Forgejo's
// statuses word it (skippedCheck and cancelledCheck read these).
func jobDescription(status string) string {
	switch status {
	case "success":
		return "Successful"
	case "failure":
		return "Failing"
	case "cancelled":
		return "Has been cancelled"
	case "skipped":
		return "Has been skipped"
	case "blocked":
		return "Blocked by required conditions"
	case "waiting":
		return "Waiting to run"
	case "running":
		return "Has started running"
	}
	return status
}
