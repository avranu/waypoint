package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// maxLogChecks bounds how many logs one request reads from the forge.
const maxLogChecks = 10

// CheckLog is one check's log: the failing part, or all of it.
type CheckLog struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	LogExcerpt
	Log string `json:"log,omitempty"` // with full: the whole log, cleaned
	// From: "forge" (read now), or "kept" (the end of the log Waypoint
	// kept when the check failed, when the forge can't be read now).
	From  string `json:"from,omitempty"`
	Error string `json:"error,omitempty"` // why there is no log
}

// PRLogs are the logs of a pull request's failed checks, on its head.
type PRLogs struct {
	PRID    int64      `json:"pr_id"`
	Repo    string     `json:"repo"`
	Number  int        `json:"number"`
	HeadSHA string     `json:"head_sha"`
	Checks  []CheckLog `json:"checks"`
}

// PRCheckLogs reads the logs of the failed checks on a pull request's head,
// or with check, of the checks whose name is that or has it in it (passed
// ones too). With full, each log is all there, else its failing part.
func (s *Service) PRCheckLogs(ctx context.Context, actor *Actor, prID int64, check string, full bool) (*PRLogs, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	out := &PRLogs{PRID: pr.ID, Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Checks: []CheckLog{}}
	type row struct {
		c    CommitCheck
		kept string
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.state, c.url, c.description, c.source, c.job_id, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? ORDER BY c.name`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.c.Name, &r.c.State, &r.c.URL, &r.c.Description, &r.c.Source, &r.c.JobID, &r.kept); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var pick []row
	switch check = strings.TrimSpace(check); check {
	case "":
		for _, r := range all {
			if r.c.State == CheckFailed {
				pick = append(pick, r)
			}
		}
	default:
		for _, r := range all {
			if strings.EqualFold(r.c.Name, check) {
				pick = []row{r}
				break
			}
			if strings.Contains(strings.ToLower(r.c.Name), strings.ToLower(check)) {
				pick = append(pick, r)
			}
		}
		if len(pick) == 0 {
			names := make([]string, 0, len(all))
			for _, r := range all {
				names = append(names, r.c.Name)
			}
			if len(names) == 0 {
				return nil, fmt.Errorf("no check %q: %s#%d has no checks on %s yet: %w", check, pr.Repo, pr.Number, short(pr.HeadSHA), ErrNotFound)
			}
			return nil, fmt.Errorf("no check %q on %s (its checks: %s): %w", check, short(pr.HeadSHA), strings.Join(names, ", "), ErrNotFound)
		}
	}
	if len(pick) > maxLogChecks {
		pick = pick[:maxLogChecks]
	}
	f, ferr := s.forgeByID(ctx, pr.ForgeID)
	cl, ok := s.Forges.(ForgeJobLogs)
	for _, r := range pick {
		cg := CheckLog{Name: r.c.Name, State: r.c.State, URL: r.c.URL}
		var log string
		var why error
		switch {
		case ferr != nil:
			why = ferr
		case !ok:
			why = errors.New("this forge client can't read job logs")
		case f.token == "":
			why = errors.New("the forge has no API token, so Waypoint can't read its logs")
		default:
			log, why = cl.JobLog(ctx, f, pr.Repo, pr.HeadSHA, r.c)
		}
		switch {
		case why == nil:
			cg.From = "forge"
		case r.kept != "":
			log, cg.From = r.kept, "kept"
			cg.Error = "read the end Waypoint kept: " + logError(why)
		default:
			cg.Error = logError(why)
		}
		if log != "" {
			if full {
				lines := cleanLog(log)
				cg.Log = strings.Join(lines, "\n") + "\n"
				cg.Lines, cg.Shown = len(lines), len(lines)
				cg.Fault = findRunnerFault(lines)
			} else {
				cg.LogExcerpt = failingExcerpt(log, 0)
			}
		}
		out.Checks = append(out.Checks, cg)
	}
	return out, nil
}

// logError says why a log can't be read, without the forge's internals.
func logError(err error) string {
	var fe *forgeAPIError
	switch {
	case errors.Is(err, errNoJobLog):
		return "the forge keeps no job log for this check"
	case errors.As(err, &fe):
		return fmt.Sprintf("the forge answered %d", fe.status)
	case errors.Is(err, ErrNotFound):
		return strings.TrimPrefix(err.Error(), ErrNotFound.Error()+": ")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "the forge took too long"
	}
	return err.Error()
}
