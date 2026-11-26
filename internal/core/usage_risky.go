package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EvRiskyCommand: an upload brought high-level risky commands that ran. It
// reaches only its user.
const EvRiskyCommand = "usage.risky_command"

// NotifyRisky is the notification kind of a risky command.
const NotifyRisky = "risky"

// UsageRisky is one risky command as `wp record usage --from-transcripts` sends it.
type UsageRisky struct {
	At      int64  `json:"t"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Kind    string `json:"kind"`
	Level   string `json:"level"`
	Label   string `json:"label"`
	Command string `json:"command"`
	Segment string `json:"segment,omitempty"`
	Outcome string `json:"outcome"`
	Source  string `json:"source,omitempty"`
}

var (
	riskyKinds = map[string]bool{"delete": true, "discard": true, "force_push": true, "push_main": true, "branch_delete": true, "history": true,
		"sql": true, "cluster": true, "deploy": true, "service": true, "docker": true, "disk": true, "power": true, "permissions": true, "remote_script": true}
	riskyOutcomes = map[string]bool{"ran": true, "error": true, "refused": true, "unknown": true}
)

const (
	// riskyTaskWindow: a command this recent is filed under the task its
	// session holds now; an older one (history read again) under none, as
	// the session may have moved on since.
	riskyTaskWindow = 6 * time.Hour
	riskyNotifyMax  = 5
	// riskyCommandMax: the command is kept whole up to this many characters,
	// as wp sends it (WP-860: the first 300 often left out what was risky).
	riskyCommandMax = 16 << 10
)

// applyRisky stores a chunk's risky commands; the high ones that ran are
// added to notable for the person's notification.
func (t *tx) applyRisky(ctx context.Context, uid int64, machine string, rows []UsageRisky, maxHour, now int64, notable *[]UsageRisky) error {
	tasks := map[string]any{}
	for _, r := range rows {
		if r.At < usageMinHour || r.At > maxHour || r.Session == "" || !riskyKinds[r.Kind] || !riskyOutcomes[r.Outcome] ||
			r.Level != "high" && r.Level != "medium" {
			continue
		}
		session := clip(r.Session, 80)
		var issue any
		if r.At >= now-riskyTaskWindow.Milliseconds() {
			var ok bool
			if issue, ok = tasks[session]; !ok {
				var id int64
				if err := t.QueryRowContext(ctx, `SELECT coalesce(issue_id, 0) FROM agent_sessions WHERE user_id = ? AND machine = ? AND cli_id = ?`,
					uid, machine, session).Scan(&id); err == nil && id > 0 {
					issue = id
				}
				tasks[session] = issue
			}
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO risky_commands(user_id, at, machine, session, repo, model, issue_id, kind, level, label, command, segment, outcome, source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			uid, r.At, machine, session, clip(r.Repo, 200), clip(r.Model, 100), issue, r.Kind, r.Level, clip(r.Label, 120),
			clip(r.Command, riskyCommandMax), clip(r.Segment, riskyCommandMax), r.Outcome, clip(r.Source, 40)); err != nil {
			return err
		}
		if r.Level == "high" && r.Outcome != "refused" && r.Outcome != "unknown" && r.At >= now-riskyTaskWindow.Milliseconds() {
			*notable = append(*notable, r)
		}
	}
	return nil
}

// emitRisky tells the person, once per upload, about the high-level
// commands that just ran.
func (t *tx) emitRisky(uid int64, machine string, rows []UsageRisky) {
	if len(rows) == 0 {
		return
	}
	var b strings.Builder
	for i, r := range rows {
		if i == riskyNotifyMax {
			fmt.Fprintf(&b, "…and %d more\n", len(rows)-i)
			break
		}
		where := r.Repo
		if where == "" {
			where = machine
		}
		fmt.Fprintf(&b, "%s in %s: %s\n", r.Label, where, clip(r.Command, 120))
	}
	t.emit(&Event{Kind: EvRiskyCommand, UserID: uid, New: strings.TrimSpace(b.String()), Data: map[string]any{"count": len(rows), "machine": machine}})
}

func (s *Service) notifyRisky(ctx context.Context, e *Event) {
	n := dataInt(e, "count")
	title := "An agent ran a risky command"
	if n > 1 {
		title = fmt.Sprintf("Agents ran %d risky commands", n)
	}
	s.notifyURL(ctx, e.UserID, e, NotifyRisky, title, e.New, "/agents?tab=audit&view=risky")
}

// UsageRiskyRow is one risky command, for the Audit tab.
type UsageRiskyRow struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Level   string    `json:"level"`
	Label   string    `json:"label"`
	Command string    `json:"command"`
	Segment string    `json:"segment,omitempty"`
	Outcome string    `json:"outcome"`
	Source  string    `json:"source,omitempty"`
	Session string    `json:"session"`
	Repo    string    `json:"repo"`
	Machine string    `json:"machine"`
	Model   string    `json:"model"`
	Issue   *IssueRef `json:"issue,omitempty"` // the task the session held
}

// UsageRiskyFilter narrows the list.
type UsageRiskyFilter struct {
	Level   string // high, medium, or all
	Kind    string
	Outcome string // ran (ran or error), refused, or all
	Before  int64  // the last id seen
	Limit   int
}

// UsageRiskyPage is a page of risky commands; the first page also counts
// the period's.
type UsageRiskyPage struct {
	Rows   []UsageRiskyRow   `json:"rows"`
	Counts *UsageRiskyCounts `json:"counts,omitempty"`
}

// UsageRiskyCounts counts the period's risky commands (before the level,
// kind and outcome filters).
type UsageRiskyCounts struct {
	High    int64            `json:"high"`
	Medium  int64            `json:"medium"`
	Refused int64            `json:"refused"`
	Kinds   map[string]int64 `json:"kinds"`
}

// UsageRiskyCommands pages through risky commands, newest first, with the
// usage filters (whose, period, project, agent, machine, session).
func (s *Service) UsageRiskyCommands(ctx context.Context, actor *Actor, q UsageQuery, f UsageRiskyFilter) (*UsageRiskyPage, error) {
	sc, _, _, _, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	where, args := sc.where("at")
	page := &UsageRiskyPage{Rows: []UsageRiskyRow{}}
	if f.Before <= 0 {
		c := &UsageRiskyCounts{Kinds: map[string]int64{}}
		// #nosec G202 -- where is built from fixed conditions; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT kind, level, outcome = 'refused', count(*) FROM risky_commands WHERE `+where+` GROUP BY 1, 2, 3`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var kind, level string
			var refused bool
			var n int64
			if err := rows.Scan(&kind, &level, &refused, &n); err != nil {
				rows.Close()
				return nil, err
			}
			if level == "high" {
				c.High += n
			} else {
				c.Medium += n
			}
			if refused {
				c.Refused += n
			}
			c.Kinds[kind] += n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		page.Counts = c
	}
	if f.Level == "high" || f.Level == "medium" {
		where, args = where+" AND level = ?", append(args, f.Level)
	}
	if f.Kind != "" && f.Kind != "all" {
		if !riskyKinds[f.Kind] {
			return nil, invalid("unknown kind %q", f.Kind)
		}
		where, args = where+" AND kind = ?", append(args, f.Kind)
	}
	switch f.Outcome {
	case "ran":
		where += " AND outcome IN ('ran', 'error', 'unknown')"
	case "refused":
		where += " AND outcome = 'refused'"
	}
	if f.Before > 0 {
		where, args = where+" AND id < ?", append(args, f.Before)
	}
	limit := min(max(f.Limit, 1), 200)
	if f.Limit <= 0 {
		limit = 50
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, at, kind, level, label, command, segment, outcome, source, session, repo, machine, model, coalesce(issue_id, 0)
		FROM risky_commands WHERE `+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	var issues []int64
	issueOf := map[int]int64{}
	for rows.Next() {
		var r UsageRiskyRow
		var at, issue int64
		if err := rows.Scan(&r.ID, &at, &r.Kind, &r.Level, &r.Label, &r.Command, &r.Segment, &r.Outcome, &r.Source, &r.Session, &r.Repo, &r.Machine, &r.Model, &issue); err != nil {
			rows.Close()
			return nil, err
		}
		r.At = msTime(at)
		if sc.hideCommands {
			r.Command, r.Segment = usageCommandRedacted, ""
		}
		if issue > 0 {
			issueOf[len(page.Rows)] = issue
			issues = append(issues, issue)
		}
		page.Rows = append(page.Rows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return page, nil
	}
	refs, err := s.visibleIssueRefs(ctx, actor, issues)
	if err != nil {
		return nil, err
	}
	for i, id := range issueOf {
		page.Rows[i].Issue = refs[id]
	}
	return page, nil
}

// visibleIssueRefs names the issues the actor may see, by id.
func (s *Service) visibleIssueRefs(ctx context.Context, actor *Actor, ids []int64) (map[int64]*IssueRef, error) {
	out := map[int64]*IssueRef{}
	// #nosec G202 -- only ? placeholders are joined in
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, p.key || '-' || i.number, i.title, i.type FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.id IN (`+placeholders(len(ids))+`) AND `+draftsHidden(actor, "i"), int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	type found struct {
		ref     IssueRef
		project int64
	}
	var all []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.ref.ID, &f.project, &f.ref.Key, &f.ref.Title, &f.ref.Type); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range all {
		if r, err := s.issueRole(ctx, s.db.R, actor, &Issue{ID: f.ref.ID, ProjectID: f.project}); err != nil || r == RoleNone {
			continue
		}
		ref := f.ref
		out[ref.ID] = &ref
	}
	return out, nil
}
