package core

import (
	"context"
	"encoding/json"
	"strings"
)

// TaskSessionPlace is where a task session for a project can start: a
// runner of the person's and the checkout they last worked in on its
// machine.
type TaskSessionPlace struct {
	ProjectKey string `json:"project_key"`
	RunnerID   int64  `json:"runner_id"`
	Runner     string `json:"runner"`
	Online     bool   `json:"online"`
	Dir        string `json:"dir"`

	machine string
}

// TaskSessionStart is what starting a task session queued.
type TaskSessionStart struct {
	Key   string            `json:"key"`
	Job   *RunnerJob        `json:"job"`
	Place *TaskSessionPlace `json:"place"`
}

// kickoffTemplate is the template a session started on a task gets its wp
// next answer in: autopilot's wording in the project (autopilotTextFor) and
// any autopilot_phrase experiment's sentence. Autopilot and task sessions
// both use it.
func (s *Service) kickoffTemplate(ctx context.Context, projectID, issueID int64) string {
	prompt, _ := s.autopilotTextFor(ctx, s.db.R, projectID)
	return s.autopilotPhrase(ctx, issueID, prompt)
}

// TaskKickoff is the kickoff template for a session on the task key, for wp
// start and wp fanout, which launch sessions themselves.
func (s *Service) TaskKickoff(ctx context.Context, a *Actor, key string) (string, error) {
	issue, err := s.GetIssue(ctx, a, key)
	if err != nil {
		return "", err
	}
	return s.kickoffTemplate(ctx, issue.ProjectID, issue.ID), nil
}

// TaskSessionPlaces is, for each project the person has worked on in a
// checkout one of their runners can start Claude Code in, the most recent
// such place, preferring a runner that is online. Agents get none: what
// runs on a person's machine is that person's decision.
func (s *Service) TaskSessionPlaces(ctx context.Context, a *Actor) ([]*TaskSessionPlace, error) {
	if a == nil || a.User == nil || a.User.ID == 0 || a.IsAgent() || a.ReadOnly {
		return []*TaskSessionPlace{}, nil
	}
	return s.sessionPlaces(ctx, a.User.ID, jobNeeds{kind: JobSession, cli: "claude", feature: FeatureTaskKickoff})
}

// sessionPlaces is TaskSessionPlaces for a user, for jobs that need what
// need says (its directory is filled in for each checkout).
func (s *Service) sessionPlaces(ctx context.Context, userID int64, need jobNeeds) ([]*TaskSessionPlace, error) {
	out := []*TaskSessionPlace{}
	runners, err := s.ownerRunners(ctx, userID)
	if err != nil || len(runners) == 0 {
		return out, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key, x.machine, x.cwd FROM agent_sessions x
		JOIN (SELECT id AS sid, project_id AS pid FROM agent_sessions WHERE user_id = ? AND project_id IS NOT NULL
			UNION SELECT r.session_id, i.project_id FROM agent_runs r JOIN agent_sessions y ON y.id = r.session_id JOIN issues i ON i.id = r.issue_id
			WHERE y.user_id = ?) sp ON sp.sid = x.id
		JOIN projects p ON p.id = sp.pid
		WHERE x.user_id = ? AND x.worktree = 0 AND x.cwd != ''
		GROUP BY p.key, x.machine, x.cwd ORDER BY max(x.last_seen_at) DESC LIMIT 500`, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	best := map[string]*TaskSessionPlace{}
	for rows.Next() {
		var key, machine, cwd string
		if err := rows.Scan(&key, &machine, &cwd); err != nil {
			return nil, err
		}
		if b := best[key]; b != nil && b.Online {
			continue
		}
		dir, ok := cleanDir(cwd)
		if !ok {
			continue
		}
		needs := need
		needs.dir = dir
		for _, r := range runners {
			if !strings.EqualFold(r.Name, machine) || !runnerCanTake(r, needs) {
				continue
			}
			p := TaskSessionPlace{ProjectKey: key, RunnerID: r.ID, Runner: r.Name, Online: r.Online, Dir: dir, machine: machine}
			switch b := best[key]; {
			case b == nil:
				best[key] = &p
				out = append(out, &p)
			case r.Online && !b.Online:
				*b = p
			}
		}
	}
	return out, rows.Err()
}

// StartTaskSession queues a fresh Claude Code session for the task on the
// person's runner (TaskSessionPlaces), bound to the task from birth. The
// runner job's audit entry records who started it.
func (s *Service) StartTaskSession(ctx context.Context, a *Actor, key string) (*TaskSessionStart, error) {
	if err := a.need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := a.canWrite(); err != nil {
		return nil, err
	}
	if err := a.requireHuman("starting a session on a person's machine"); err != nil {
		return nil, err
	}
	issue, err := s.GetIssue(ctx, a, key)
	if err != nil {
		return nil, err
	}
	if issue.Closed() {
		return nil, invalid("%s is already closed", issue.Key)
	}
	if issue.Role != "owner" && issue.Role != "editor" {
		return nil, forbidden("you can only view %s", issue.ProjectKey)
	}
	places, err := s.TaskSessionPlaces(ctx, a)
	if err != nil {
		return nil, err
	}
	var place *TaskSessionPlace
	for _, p := range places {
		if p.ProjectKey == issue.ProjectKey {
			place = p
		}
	}
	if place == nil {
		return nil, invalid("none of your runners can start it: run wp runner (with --allow for the checkout) on a machine where you work on %s, or run wp start %s in its checkout", issue.ProjectKey, issue.Key)
	}
	raw, err := json.Marshal(SessionJob{Dir: place.Dir, Kickoff: s.kickoffTemplate(ctx, issue.ProjectID, issue.ID), Task: issue.Key,
		Title: excerpt(issue.Title, 200), Name: strings.TrimSpace(issue.Key + " " + excerpt(issue.Title, 60))})
	if err != nil {
		return nil, err
	}
	job, err := s.EnqueueRunnerJob(ctx, a, RunnerJobInput{Kind: JobSession, RunnerID: place.RunnerID, Input: raw, Source: "manual"})
	if err != nil {
		return nil, err
	}
	return &TaskSessionStart{Key: issue.Key, Job: job, Place: place}, nil
}
