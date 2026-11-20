package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Per-job tokens (WP-418, WP-ADR28 §7.3). A runner job that runs wp gets a
// Waypoint token of its own instead of the runner's, which is a person's
// token and can do anything that person can. The server mints it when a
// runner claims the job and deletes it when the job ends; it also expires
// with the job. It is an agent-bound permission token, so every control that
// refuses agents applies to it, and its permissions leave out what a job
// should never do: manage tokens (refused to every permission token), answer
// or run requests, start sessions, run or host runner jobs, deploy, manage
// projects, repositories, forges, automations, AI settings or webhooks,
// delete things, or administer Waypoint.
//
// It reaches only the job's project and its sub-projects (jobScope), as an
// editor at most and never as an admin. A later phase has a broker hold it,
// so the sandbox only sees a placeholder.

// jobTokenPermissions is what a job's token may do: the work an agent does
// on a task, and nothing that runs code, spends credit or changes who can
// do what.
var jobTokenPermissions = []Permission{
	PermIssuesRead, PermIssuesCreate, PermIssuesEdit, PermIssuesComment, PermTimeTrack,
	PermSearch, PermPagesRead, PermPagesWrite, PermGlossaryRead, PermGlossaryWrite,
	PermDesignRead, PermDesignWrite, PermLinksWrite, PermAttachmentsRead, PermAttachmentsWrite,
	PermRequestsRead, PermRequestsCreate,
	PermTasksWork, PermAgentsRead, PermSessionsRead, PermSessionsReport,
	PermPullsRead, PermPullsWrite, PermPullsMerge,
	PermEnvironmentsRead, PermAutomationsRead, PermUsageRead,
}

// JobTokenRateLimit is how many requests a job's token may make a minute.
const JobTokenRateLimit = 600

// jobAgentVendor is the vendor part of the agent identity of a CLI's jobs.
var jobAgentVendor = map[string]string{"claude": "anthropic", "codex": "openai", "cursor": "cursor", "opencode": "opencode"}

// jobTokenAgent says whether a job gets its own token and, if so, the agent
// it acts as. Only jobs whose agent can run commands, and so run wp, get
// one: autopilot (the runner runs wp next for the new session) and agent
// jobs that may run commands. A job whose prompt carries text others wrote
// (Confine) never does: it can't read beyond its checkout today, and a
// token would hand it more than that.
func jobTokenAgent(kind string, input json.RawMessage) (string, bool) {
	switch kind {
	case JobAutopilot, JobAutopilotStart:
		var in AutopilotJob
		if json.Unmarshal(input, &in) != nil {
			return "", false
		}
		if a := strings.TrimSpace(in.Agent); a != "" {
			return excerpt(a, 100), true
		}
		return "anthropic/claude-code", true
	case JobAgent:
		var in AgentJob
		if json.Unmarshal(input, &in) != nil || in.Confine || in.ReadOnly || in.Permission != AgentEditRun {
			return "", false
		}
		return jobAgentID(in.CLI, in.Model), true
	}
	return "", false
}

// jobAgentID is the agent identity of a CLI running a model:
// anthropic/claude-opus-5-5, opencode/muse-spark-1.3 (opencode's models
// already name their provider), or the vendor and the CLI when no model is
// named.
func jobAgentID(cli, model string) string {
	vendor := jobAgentVendor[cli]
	if vendor == "" {
		vendor = "runner"
	}
	model, _, _ = strings.Cut(model, "[") // opus[1m]
	switch {
	case model == "":
		model = map[string]string{"claude": "claude-code", "cursor": "cursor-agent"}[cli]
		if model == "" {
			model = cli
		}
	case strings.Contains(model, "/"):
		return excerpt(model, 100)
	}
	return excerpt(vendor+"/"+model, 100)
}

// mintJobToken makes the token of job id, which the runner owner's runner
// has just claimed. It lives as long as the job may: its timeout plus the
// grace before an unreported job expires.
func (t *tx) mintJobToken(ctx context.Context, id, owner int64, agent string, timeout int) (string, error) {
	tok := newToken("wpt_")
	now := t.s.nowMS()
	exp := now + (time.Duration(timeout)*time.Second + runnerExpireAfter).Milliseconds()
	_, err := t.ExecContext(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, prefix, read_only, agent, permissions, created_at, expires_at, runner_job_id)
		VALUES (?,?,?,?,0,?,?,?,?,?)`, owner, fmt.Sprintf("runner job %d", id), hashToken(tok), tok[:12], agent, permissionsJSON(jobTokenPermissions), now, exp, id)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// dropJobTokens deletes the tokens of a job that has ended.
func (t *tx) dropJobTokens(ctx context.Context, id int64) error {
	_, err := t.ExecContext(ctx, `DELETE FROM api_tokens WHERE runner_job_id = ?`, id)
	return err
}

// jobScope is what a job's token reaches: the job's project and its
// sub-projects, each with the role the job's owner has there (an admin's is
// editor), at most editor, so it can't change a project's settings or
// members. Shares don't count, and a job for no project reaches none.
func (s *Service) jobScope(ctx context.Context, jobID int64, u *User) (map[int64]int, error) {
	rows, err := s.db.R.QueryContext(ctx, `WITH RECURSIVE tree(id) AS (
			SELECT project_id FROM runner_jobs WHERE id = ? AND project_id IS NOT NULL
			UNION SELECT p.id FROM projects p JOIN tree t ON p.parent_id = t.id)
		SELECT t.id, coalesce(m.role, '') FROM tree t LEFT JOIN project_members m ON m.project_id = t.id AND m.user_id = ?`, jobID, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scope := map[int64]int{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		r, _ := roleFromName(name)
		if u.IsAdmin {
			r = RoleEditor
		}
		if r = min(r, RoleEditor); r > RoleNone {
			scope[id] = r
		}
	}
	return scope, rows.Err()
}
