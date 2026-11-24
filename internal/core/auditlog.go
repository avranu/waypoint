package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"slices"
	"strings"
	"time"
)

// AuditQuery narrows the log.
type AuditQuery struct {
	Project  string // a project key
	User     string // a person (id or username)
	Category string
	Agent    string // an agent spec: entries about or by agents it covers
	Days     int    // only the last n days (0: all)
	Before   int64  // an entry id: older entries only (the next page)
	Limit    int
}

// AuditProject names an entry's project.
type AuditProject struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// AuditRecord is one entry as people read it.
type AuditRecord struct {
	ID         int64         `json:"id"`
	At         time.Time     `json:"at"`
	User       *UserRef      `json:"user"`
	Actor      string        `json:"actor,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	AboutAgent string        `json:"about_agent,omitempty"`
	Action     string        `json:"action"`
	Category   string        `json:"category"`
	Project    *AuditProject `json:"project"`
	TargetType string        `json:"target_type,omitempty"`
	TargetID   *int64        `json:"target_id"`
	Target     string        `json:"target,omitempty"`
	URL        string        `json:"url,omitempty"`
	Detail     string        `json:"detail,omitempty"`
}

// AuditPage is a page of the log; NextBefore, when set, continues it.
type AuditPage struct {
	Entries    []*AuditRecord `json:"entries"`
	NextBefore int64          `json:"next_before,omitempty"`
}

// AuditCategories are the kinds of decision, in the order people read them.
var AuditCategories = []string{AuditApproval, AuditDelegation, AuditControl, AuditSettings, AuditAccess}

// auditMatches: an entry is about or by an agent the spec covers. Entries
// can name a delegation spec (opus) rather than an identity, so they also
// match when that spec reaches one of the known agents the filter covers:
// a filter of claude finds work delegated to opus. An entry about several
// agents lists them comma-separated.
func auditMatches(spec string, covered []string, r *AuditRecord) bool {
	for _, a := range append(strings.Split(r.AboutAgent, ","), r.Agent) {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if AgentMatches(spec, a) || AgentMatches(a, spec) {
			return true
		}
		for _, id := range covered {
			if AgentMatches(a, id) {
				return true
			}
		}
	}
	return false
}

// auditBatches bounds how far back an agent filter reads for one page, so
// a rare agent can't make a request scan the whole log.
const auditBatches = 20

// ListAudit reads the log, newest first. Admins see everything; others see
// entries in projects they belong to, and their own.
func (s *Service) ListAudit(ctx context.Context, actor *Actor, q AuditQuery) (*AuditPage, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	where, args := []string{}, []any{}
	if !actor.isAdmin() {
		member, margs := actor.memberProjects(RoleViewer)
		where = append(where, `((a.user_id = ? AND ?) OR a.project_id IN `+member+`)`)
		args = append(append(args, actor.id(), actor.scope == nil), margs...)
	}
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = append(where, "a.project_id = ?"), append(args, p.ID)
	}
	if q.User != "" {
		u, err := s.resolveUser(ctx, s.db.R, actor, q.User)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "a.user_id = ?"), append(args, u.ID)
	}
	if q.Category != "" {
		if !slices.Contains(AuditCategories, q.Category) {
			return nil, invalid("category must be one of %s", strings.Join(AuditCategories, ", "))
		}
		where, args = append(where, "a.category = ?"), append(args, q.Category)
	}
	if q.Days > 0 {
		where, args = append(where, "a.created_at >= ?"), append(args, s.nowMS()-int64(q.Days)*24*time.Hour.Milliseconds())
	}
	spec := strings.TrimSpace(q.Agent)
	var covered []string
	if spec != "" {
		where = append(where, "(a.about_agent != '' OR a.agent != '')")
		known, err := s.KnownAgents(ctx, actor)
		if err != nil {
			return nil, err
		}
		for _, k := range known {
			if AgentMatches(spec, k.Identity) {
				covered = append(covered, k.Identity)
			}
		}
	}
	page := &AuditPage{Entries: []*AuditRecord{}}
	users, projects := map[int64]*UserRef{}, map[int64]*AuditProject{}
	before := q.Before
	// An agent filter matches in Go (specs aren't SQL), so it reads in
	// batches until the page is full.
	size := q.Limit + 1
	if spec != "" {
		size = 500
	}
	for range auditBatches {
		w, wa := slices.Clone(where), slices.Clone(args)
		if before > 0 {
			// Entries backfilled from older records aren't in id order: time decides.
			w, wa = append(w, "(a.created_at, a.id) < (SELECT created_at, id FROM audit_log WHERE id = ?)"), append(wa, before)
		}
		cond := "1"
		if len(w) > 0 {
			cond = strings.Join(w, " AND ")
		}
		got, err := s.auditRows(ctx, cond, append(wa, size))
		if err != nil {
			return nil, err
		}
		for _, r := range got {
			if spec != "" && !auditMatches(spec, covered, &r.AuditRecord) {
				before = r.ID
				continue
			}
			if len(page.Entries) == q.Limit {
				page.NextBefore = page.Entries[len(page.Entries)-1].ID
				return page, nil
			}
			before = r.ID
			if r.userID.Valid {
				if _, ok := users[r.userID.Int64]; !ok {
					users[r.userID.Int64] = s.userRefByID(ctx, s.db.R, r.userID)
				}
				r.User = users[r.userID.Int64]
			}
			if r.projectID.Valid {
				id := r.projectID.Int64
				if _, ok := projects[id]; !ok {
					var p AuditProject
					if s.db.R.QueryRowContext(ctx, `SELECT key, name FROM projects WHERE id = ?`, id).Scan(&p.Key, &p.Name) == nil {
						projects[id] = &p
					} else {
						projects[id] = nil
					}
				}
				r.Project = projects[id]
			}
			page.Entries = append(page.Entries, &r.AuditRecord)
		}
		if len(got) < size {
			return page, nil
		}
	}
	page.NextBefore = before
	return page, nil
}

type auditRow struct {
	AuditRecord
	userID, projectID sql.NullInt64
}

func (s *Service) auditRows(ctx context.Context, cond string, args []any) ([]*auditRow, error) {
	// #nosec G202 -- cond joins fixed conditions only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.id, a.created_at, a.user_id, a.actor, a.agent, a.about_agent, a.action, a.category,
		a.project_id, a.target_type, a.target_id, a.target, a.url, a.detail
		FROM audit_log a WHERE `+cond+` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*auditRow
	for rows.Next() {
		var r auditRow
		var at int64
		var tid sql.NullInt64
		if err := rows.Scan(&r.ID, &at, &r.userID, &r.Actor, &r.Agent, &r.AboutAgent, &r.Action, &r.Category,
			&r.projectID, &r.TargetType, &tid, &r.Target, &r.URL, &r.Detail); err != nil {
			return nil, err
		}
		r.At, r.TargetID = msTime(at), ptrInt(tid)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// auditCSVMax bounds an export.
const auditCSVMax = 10_000

// AuditCSV is the log as a spreadsheet, with the same filters.
func (s *Service) AuditCSV(ctx context.Context, actor *Actor, q AuditQuery) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"time (UTC)", "person", "forge account", "agent acting", "about agent", "category", "action", "project", "target", "detail", "link"})
	q.Limit = 200
	for total := 0; total < auditCSVMax; {
		page, err := s.ListAudit(ctx, actor, q)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Entries {
			person, project := "", ""
			if r.User != nil {
				person = r.User.Username
			}
			if r.Project != nil {
				project = r.Project.Key
			}
			link := r.URL
			if strings.HasPrefix(link, "/") {
				link = s.BaseURL() + link
			}
			_ = w.Write([]string{r.At.UTC().Format(time.RFC3339), person, csvSafe(r.Actor), csvSafe(r.Agent), csvSafe(r.AboutAgent),
				r.Category, r.Action, project, csvSafe(r.Target), csvSafe(r.Detail), csvSafe(link)})
		}
		total += len(page.Entries)
		if page.NextBefore == 0 {
			break
		}
		q.Before = page.NextBefore
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// csvSafe keeps a spreadsheet from reading text as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
