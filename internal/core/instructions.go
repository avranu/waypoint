package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
)

// Agent instructions (WP-1559, ADR146): text Waypoint adds to a session's
// context by what the session is (main thread or sub-agent, its model, its
// agent type) at its start, at a sub-agent's start and after a compaction.
// Each change is a revision; an agent's waits for a person, and only the
// approved revision is delivered. What each hook was answered with is
// recorded, sent or not.

const (
	InstrScopeAny      = "any"
	InstrScopeMain     = "main"
	InstrScopeSubagent = "subagent"

	InstrMomentStart   = "start"   // a start, and again after each compaction
	InstrMomentCompact = "compact" // only after a compaction

	InstrApproved = "approved"
	InstrPending  = "pending"
	InstrRejected = "rejected"

	InstrSent         = "sent"
	InstrCut          = "cut"
	InstrUnknownModel = "unknown_model"

	// EvInstructionChanged: + instruction (id), title, state.
	EvInstructionChanged = "instruction.changed"

	// instrBodyMax keeps one instruction short: it is for one kind of
	// session at one moment, not a document.
	instrBodyMax = 4000
	// What one hook's answer carries of them. Claude Code keeps 10,000
	// characters of a hook's context; a session's start shares that with the
	// wp syntax hint and the kit's notes, a sub-agent's start has it alone.
	instrBudgetMain     = 6000
	instrBudgetSubagent = 9000
)

// Instruction is one agent instruction as it stands: the approved revision's
// text and targets (or, while none is approved, its proposal's).
type Instruction struct {
	ID         int64     `json:"id"`
	Project    string    `json:"project"` // key; "" every project
	Rev        int       `json:"rev"`     // the approved revision delivered; 0 none yet
	Enabled    bool      `json:"enabled"`
	Position   int       `json:"position"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Scope      string    `json:"scope"`
	Models     []string  `json:"models"`
	AgentTypes []string  `json:"agent_types"`
	Moment     string    `json:"moment"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Pending: the newest revision waiting for a person, if any.
	Pending *InstructionRevision `json:"pending,omitempty"`
	// Only on one instruction read by id.
	Revisions []*InstructionRevision `json:"revisions,omitempty"`
	Sent      *InstructionSent       `json:"sent,omitempty"`

	projectID sql.NullInt64
}

// InstructionRevision is one change to an instruction.
type InstructionRevision struct {
	Rev        int       `json:"rev"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Scope      string    `json:"scope"`
	Models     []string  `json:"models"`
	AgentTypes []string  `json:"agent_types"`
	Moment     string    `json:"moment"`
	State      string    `json:"state"`
	By         string    `json:"by,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	At         time.Time `json:"at"`
}

// InstructionSent counts what hooks were answered with for one instruction:
// by outcome, and of those sent, to how many sessions and sub-agents.
type InstructionSent struct {
	Sent         int        `json:"sent"`
	Cut          int        `json:"cut"`
	UnknownModel int        `json:"unknown_model"`
	Sessions     int        `json:"sessions"`
	Subagents    int        `json:"subagents"`
	Compactions  int        `json:"compactions"` // sent again after one
	Last         *time.Time `json:"last,omitempty"`
}

// InstructionInput creates or changes an instruction. A field left out of a
// change keeps the newest revision's value.
type InstructionInput struct {
	Project    string    `json:"project"` // create only; "" every project (admins)
	Title      *string   `json:"title"`
	Body       *string   `json:"body"`
	Scope      *string   `json:"scope"`
	Models     *[]string `json:"models"`
	AgentTypes *[]string `json:"agent_types"`
	Moment     *string   `json:"moment"`
	Enabled    *bool     `json:"enabled"`
	Position   *int      `json:"position"`
}

// InstructionAudience is the session a hook speaks for.
type InstructionAudience struct {
	Scope     string `json:"scope"`      // main or subagent
	Model     string `json:"model"`      // "" unknown
	AgentType string `json:"agent_type"` // a sub-agent's
	Compact   bool   `json:"compact"`    // just after a compaction
}

const instrRevCols = `r.rev, r.title, r.body, r.scope, r.models, r.agent_types, r.moment, r.state, coalesce(u.username, ''), r.agent, r.created_at`

func scanInstrRev(sc interface{ Scan(...any) error }) (*InstructionRevision, error) {
	var r InstructionRevision
	var models, types string
	var at int64
	if err := sc.Scan(&r.Rev, &r.Title, &r.Body, &r.Scope, &models, &types, &r.Moment, &r.State, &r.By, &r.Agent, &at); err != nil {
		return nil, err
	}
	r.Models, r.AgentTypes, r.At = instrList(models), instrList(types), msTime(at)
	return &r, nil
}

func instrList(raw string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// cleanInstrList trims, drops empties and repeats, and keeps at most 20.
func cleanInstrList(in []string) ([]string, error) {
	out, seen := []string{}, map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		if len(v) > 100 {
			return nil, invalid("%q is too long (100 characters at most)", v[:20]+"…")
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	if len(out) > 20 {
		return nil, invalid("at most 20 models or agent types")
	}
	return out, nil
}

// instructionRows reads instructions with the revision that stands for each:
// the approved one, else the newest. where filters agent_instructions i.
func (s *Service) instructionRows(ctx context.Context, q queryer, where string, args ...any) ([]*Instruction, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.project_id, coalesce(p.key, ''), i.rev, i.enabled, i.position, i.updated_at, `+instrRevCols+`
		FROM agent_instructions i
		LEFT JOIN projects p ON p.id = i.project_id
		JOIN agent_instruction_revisions r ON r.instruction_id = i.id AND r.rev = CASE WHEN i.rev > 0 THEN i.rev
			ELSE (SELECT max(rev) FROM agent_instruction_revisions WHERE instruction_id = i.id) END
		LEFT JOIN users u ON u.id = r.user_id
		WHERE `+where+` ORDER BY i.project_id IS NOT NULL, i.position, i.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Instruction{}
	for rows.Next() {
		var x Instruction
		var r InstructionRevision
		var models, types string
		var updated, at int64
		if err := rows.Scan(&x.ID, &x.projectID, &x.Project, &x.Rev, &x.Enabled, &x.Position, &updated,
			&r.Rev, &r.Title, &r.Body, &r.Scope, &models, &types, &r.Moment, &r.State, &r.By, &r.Agent, &at); err != nil {
			return nil, err
		}
		x.Title, x.Body, x.Scope, x.Moment = r.Title, r.Body, r.Scope, r.Moment
		x.Models, x.AgentTypes, x.UpdatedAt = instrList(models), instrList(types), msTime(updated)
		out = append(out, &x)
	}
	return out, rows.Err()
}

// withPending adds each instruction's newest pending revision.
func (s *Service) withPending(ctx context.Context, q queryer, list []*Instruction) error {
	for _, x := range list {
		r, err := scanInstrRev(q.QueryRowContext(ctx, `SELECT `+instrRevCols+` FROM agent_instruction_revisions r LEFT JOIN users u ON u.id = r.user_id
			WHERE r.instruction_id = ? AND r.state = ? AND r.rev > ? ORDER BY r.rev DESC LIMIT 1`, x.ID, InstrPending, x.Rev))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		x.Pending = r
	}
	return nil
}

// canReadInstructions: a project's are its members' to read; the ones for
// every project are anyone's who is signed in.
func (s *Service) canReadInstructions(ctx context.Context, actor *Actor, projectID sql.NullInt64) error {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return ErrUnauthorized
	}
	if !projectID.Valid {
		return nil
	}
	return s.requireProjectRole(ctx, s.db.R, actor, projectID.Int64, RoleViewer)
}

// canWriteInstructions: editors of the project; admins for every project's.
func (s *Service) canWriteInstructions(ctx context.Context, actor *Actor, projectID sql.NullInt64) error {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return ErrUnauthorized
	}
	if !projectID.Valid {
		if err := actor.canWrite(); err != nil {
			return err
		}
		if !actor.isAdmin() {
			return forbidden("instructions for every project are for admins")
		}
		return nil
	}
	return s.requireProjectRole(ctx, s.db.R, actor, projectID.Int64, RoleEditor)
}

// ListInstructions lists a project's instructions and the ones for every
// project; with no project, only the latter.
func (s *Service) ListInstructions(ctx context.Context, actor *Actor, project string) ([]*Instruction, error) {
	var pid sql.NullInt64
	if project = strings.TrimSpace(project); project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		pid = sql.NullInt64{Int64: p.ID, Valid: true}
	}
	if err := s.canReadInstructions(ctx, actor, pid); err != nil {
		return nil, err
	}
	list, err := s.instructionRows(ctx, s.db.R, `i.project_id IS NULL OR i.project_id = ?`, pid)
	if err != nil {
		return nil, err
	}
	return list, s.withPending(ctx, s.db.R, list)
}

// GetInstruction reads one, with its revisions and what was sent.
func (s *Service) GetInstruction(ctx context.Context, actor *Actor, id int64) (*Instruction, error) {
	x, err := s.instructionByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.canReadInstructions(ctx, actor, x.projectID); err != nil {
		return nil, err
	}
	if err := s.withPending(ctx, s.db.R, []*Instruction{x}); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+instrRevCols+` FROM agent_instruction_revisions r LEFT JOIN users u ON u.id = r.user_id
		WHERE r.instruction_id = ? ORDER BY r.rev DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanInstrRev(rows)
		if err != nil {
			return nil, err
		}
		x.Revisions = append(x.Revisions, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var sent InstructionSent
	var last sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT
			coalesce(sum(outcome = ?), 0), coalesce(sum(outcome = ?), 0), coalesce(sum(outcome = ?), 0),
			count(DISTINCT CASE WHEN outcome = ? AND agent_id = '' THEN session_id END),
			count(DISTINCT CASE WHEN outcome = ? AND agent_id <> '' THEN agent_id END),
			coalesce(sum(outcome = ? AND source = 'compact'), 0),
			max(CASE WHEN outcome = ? THEN created_at END)
		FROM agent_instruction_deliveries WHERE instruction_id = ?`,
		InstrSent, InstrCut, InstrUnknownModel, InstrSent, InstrSent, InstrSent, InstrSent, id).
		Scan(&sent.Sent, &sent.Cut, &sent.UnknownModel, &sent.Sessions, &sent.Subagents, &sent.Compactions, &last); err != nil {
		return nil, err
	}
	if last.Valid {
		at := msTime(last.Int64)
		sent.Last = &at
	}
	x.Sent = &sent
	return x, nil
}

func (s *Service) instructionByID(ctx context.Context, q queryer, id int64) (*Instruction, error) {
	list, err := s.instructionRows(ctx, q, `i.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("instruction")
	}
	return list[0], nil
}

// instrContent is a revision's text and targets, checked.
type instrContent struct {
	title, body, scope, moment string
	models, types              []string
}

// merged is base with in's fields over it, validated.
func (base instrContent) merged(in InstructionInput) (instrContent, error) {
	c := base
	if in.Title != nil {
		c.title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		c.body = strings.TrimSpace(*in.Body)
	}
	if in.Scope != nil {
		c.scope = strings.ToLower(strings.TrimSpace(*in.Scope))
	}
	if in.Moment != nil {
		c.moment = strings.ToLower(strings.TrimSpace(*in.Moment))
	}
	var err error
	if in.Models != nil {
		if c.models, err = cleanInstrList(*in.Models); err != nil {
			return c, err
		}
	}
	if in.AgentTypes != nil {
		if c.types, err = cleanInstrList(*in.AgentTypes); err != nil {
			return c, err
		}
	}
	c.scope, c.moment = cmp(c.scope, InstrScopeAny), cmp(c.moment, InstrMomentStart)
	switch {
	case c.title == "":
		return c, invalid("an instruction needs a title")
	case len(c.title) > 200:
		return c, invalid("the title is too long (200 characters at most)")
	case c.body == "":
		return c, invalid("an instruction needs its text")
	case len(c.body) > instrBodyMax:
		return c, invalid("the text is %d characters; an instruction is at most %d (split it by who it is for)", len(c.body), instrBodyMax)
	case c.scope != InstrScopeAny && c.scope != InstrScopeMain && c.scope != InstrScopeSubagent:
		return c, invalid("scope is main, subagent or any")
	case c.moment != InstrMomentStart && c.moment != InstrMomentCompact:
		return c, invalid("moment is start or compact")
	case c.scope == InstrScopeMain && len(c.types) > 0:
		return c, invalid("agent types are a sub-agent's: use scope subagent or any with them")
	case c.scope != InstrScopeMain && c.moment == InstrMomentCompact:
		return c, invalid("moment compact is the main thread's: use scope main with it")
	}
	for _, g := range c.types {
		if _, err := path.Match(strings.TrimPrefix(g, "!"), "x"); err != nil {
			return c, invalid("agent type %q is not a glob", g)
		}
	}
	return c, nil
}

// addInstrRevision writes c as the next revision of the instruction: approved
// and standing when a person wrote it, pending when an agent did.
func (s *Service) addInstrRevision(ctx context.Context, t *tx, actor *Actor, id int64, c instrContent) (state string, err error) {
	var rev int
	if err := t.QueryRowContext(ctx, `SELECT coalesce(max(rev), 0) + 1 FROM agent_instruction_revisions WHERE instruction_id = ?`, id).Scan(&rev); err != nil {
		return "", err
	}
	state = InstrApproved
	var reviewedBy, reviewedAt any
	if actor.writesAsAgent() {
		state = InstrPending
	} else {
		reviewedBy, reviewedAt = actor.id(), s.nowMS()
	}
	agent := actor.creditAgent()
	sessID, sessTask := t.creditSession(ctx, agent)
	if _, err := t.ExecContext(ctx, `INSERT INTO agent_instruction_revisions(instruction_id, rev, title, body, scope, models, agent_types, moment, state, user_id, agent, session_id, session_issue_id, created_at, reviewed_by, reviewed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, rev, c.title, c.body, c.scope, jsonList(c.models), jsonList(c.types), c.moment, state,
		actor.id(), agent, sessID, sessTask, s.nowMS(), reviewedBy, reviewedAt); err != nil {
		return "", err
	}
	if state == InstrApproved {
		if _, err := t.ExecContext(ctx, `UPDATE agent_instructions SET rev = ?, updated_at = ? WHERE id = ?`, rev, s.nowMS(), id); err != nil {
			return "", err
		}
	} else if _, err := t.ExecContext(ctx, `UPDATE agent_instructions SET updated_at = ? WHERE id = ?`, s.nowMS(), id); err != nil {
		return "", err
	}
	return state, nil
}

func instructionEvent(id int64, projectID sql.NullInt64, title, state string) *Event {
	return &Event{Kind: EvInstructionChanged, ProjectID: projectID.Int64, AdminOnly: !projectID.Valid,
		Data: map[string]any{"instruction": id, "title": title, "state": state}}
}

// CreateInstruction adds an instruction. A person's is delivered from the
// next hook; an agent's is a proposal a person approves first.
func (s *Service) CreateInstruction(ctx context.Context, actor *Actor, in InstructionInput) (*Instruction, error) {
	var pid sql.NullInt64
	if ref := strings.TrimSpace(in.Project); ref != "" {
		p, err := s.projectByRef(ctx, s.db.R, ref)
		if err != nil {
			return nil, err
		}
		pid = sql.NullInt64{Int64: p.ID, Valid: true}
	}
	if err := s.canWriteInstructions(ctx, actor, pid); err != nil {
		return nil, err
	}
	c, err := instrContent{}.merged(in)
	if err != nil {
		return nil, err
	}
	if (in.Enabled != nil || in.Position != nil) && actor.writesAsAgent() {
		return nil, forbidden("an agent proposes the text and who it is for; turning it on or off and its order are a person's")
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		enabled, position := true, 0
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		if in.Position != nil {
			position = *in.Position
		}
		res, err := t.ExecContext(ctx, `INSERT INTO agent_instructions(project_id, enabled, position, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			pid, enabled, position, actor.id(), s.nowMS(), s.nowMS())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		state, err := s.addInstrRevision(ctx, t, actor, id, c)
		if err != nil {
			return err
		}
		t.emit(instructionEvent(id, pid, c.title, state))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetInstruction(ctx, actor, id)
}

// UpdateInstruction changes an instruction: a new revision when its text or
// targets change (pending when an agent writes it), and its switch and order,
// which only a person sets.
func (s *Service) UpdateInstruction(ctx context.Context, actor *Actor, id int64, in InstructionInput) (*Instruction, error) {
	x, err := s.instructionByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.canWriteInstructions(ctx, actor, x.projectID); err != nil {
		return nil, err
	}
	if in.Enabled != nil || in.Position != nil {
		if err := actor.requirePersonDirect("turning an instruction on or off, or reordering it,"); err != nil {
			return nil, err
		}
	}
	content := in.Title != nil || in.Body != nil || in.Scope != nil || in.Models != nil || in.AgentTypes != nil || in.Moment != nil
	err = s.write(ctx, actor, func(t *tx) error {
		state := ""
		if content {
			// From the newest revision, so a second proposal builds on the first.
			var b instrContent
			var models, types string
			if err := t.QueryRowContext(ctx, `SELECT title, body, scope, models, agent_types, moment FROM agent_instruction_revisions
				WHERE instruction_id = ? AND state <> ? ORDER BY rev DESC LIMIT 1`, id, InstrRejected).Scan(&b.title, &b.body, &b.scope, &models, &types, &b.moment); err != nil {
				return err
			}
			b.models, b.types = instrList(models), instrList(types)
			c, err := b.merged(in)
			if err != nil {
				return err
			}
			if state, err = s.addInstrRevision(ctx, t, actor, id, c); err != nil {
				return err
			}
			x.Title = c.title
		}
		if in.Enabled != nil {
			if _, err := t.ExecContext(ctx, `UPDATE agent_instructions SET enabled = ?, updated_at = ? WHERE id = ?`, *in.Enabled, s.nowMS(), id); err != nil {
				return err
			}
			state = map[bool]string{true: "on", false: "off"}[*in.Enabled]
		}
		if in.Position != nil {
			if _, err := t.ExecContext(ctx, `UPDATE agent_instructions SET position = ?, updated_at = ? WHERE id = ?`, *in.Position, s.nowMS(), id); err != nil {
				return err
			}
		}
		t.emit(instructionEvent(id, x.projectID, x.Title, cmp(state, "changed")))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetInstruction(ctx, actor, id)
}

// ReviewInstruction approves or rejects a pending revision (rev 0: the
// newest pending one). Approving makes it the one delivered. People only.
func (s *Service) ReviewInstruction(ctx context.Context, actor *Actor, id int64, rev int, approve bool) (*Instruction, error) {
	x, err := s.instructionByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.canWriteInstructions(ctx, actor, x.projectID); err != nil {
		return nil, err
	}
	if err := actor.requirePersonDirect("approving or rejecting an instruction"); err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		var title string
		q, args := `SELECT rev, title FROM agent_instruction_revisions WHERE instruction_id = ? AND state = ? ORDER BY rev DESC LIMIT 1`, []any{id, InstrPending}
		if rev > 0 {
			q, args = `SELECT rev, title FROM agent_instruction_revisions WHERE instruction_id = ? AND state = ? AND rev = ?`, []any{id, InstrPending, rev}
		}
		if err := t.QueryRowContext(ctx, q, args...).Scan(&rev, &title); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("nothing pending on this instruction")
			}
			return err
		}
		state := InstrRejected
		if approve {
			state = InstrApproved
			if rev < x.Rev {
				return invalid("revision %d is older than the one delivered (%d); edit from it instead", rev, x.Rev)
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_instruction_revisions SET state = ?, reviewed_by = ?, reviewed_at = ? WHERE instruction_id = ? AND rev = ?`,
			state, actor.id(), s.nowMS(), id, rev); err != nil {
			return err
		}
		if approve {
			// Older proposals are answered by this one.
			if _, err := t.ExecContext(ctx, `UPDATE agent_instruction_revisions SET state = ?, reviewed_by = ?, reviewed_at = ? WHERE instruction_id = ? AND state = ? AND rev < ?`,
				InstrRejected, actor.id(), s.nowMS(), id, InstrPending, rev); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE agent_instructions SET rev = ?, updated_at = ? WHERE id = ?`, rev, s.nowMS(), id); err != nil {
				return err
			}
		}
		t.emit(instructionEvent(id, x.projectID, title, state))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetInstruction(ctx, actor, id)
}

// DeleteInstruction removes an instruction, its revisions and its record of
// what was sent. People only.
func (s *Service) DeleteInstruction(ctx context.Context, actor *Actor, id int64) error {
	x, err := s.instructionByID(ctx, s.db.R, id)
	if err != nil {
		return err
	}
	if err := s.canWriteInstructions(ctx, actor, x.projectID); err != nil {
		return err
	}
	if err := actor.requirePersonDirect("deleting an instruction"); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `DELETE FROM agent_instructions WHERE id = ?`, id); err != nil {
			return err
		}
		t.emit(instructionEvent(id, x.projectID, x.Title, "deleted"))
		return nil
	})
}

// ---- who an instruction is for ----

// instrFor says whether x is for the audience: no (not for it), yes, or
// unknownModel (it would be, but it names models and the model isn't known).
func instrFor(x *Instruction, a InstructionAudience) (yes, unknownModel bool) {
	if x.Scope != InstrScopeAny && x.Scope != a.Scope {
		return false, false
	}
	if x.Moment == InstrMomentCompact && !a.Compact {
		return false, false
	}
	if len(x.AgentTypes) > 0 && !instrTypeMatch(x.AgentTypes, a.AgentType) {
		return false, false
	}
	if len(x.Models) > 0 {
		model := strings.ToLower(strings.TrimSpace(a.Model))
		if model == "" {
			return false, true
		}
		hit := false
		for _, m := range x.Models {
			if strings.Contains(model, strings.ToLower(m)) {
				hit = true
				break
			}
		}
		if !hit {
			return false, false
		}
	}
	return true, false
}

// instrTypeMatch: the type matches a glob and none of the ! ones. Only !
// globs means every type but those. Case-insensitive.
func instrTypeMatch(globs []string, agentType string) bool {
	agentType = strings.ToLower(agentType)
	positive, hit := false, false
	for _, g := range globs {
		g = strings.ToLower(g)
		if rest, neg := strings.CutPrefix(g, "!"); neg {
			if ok, _ := path.Match(rest, agentType); ok {
				return false
			}
			continue
		}
		positive = true
		if ok, _ := path.Match(g, agentType); ok {
			hit = true
		}
	}
	return hit || (!positive && agentType != "")
}

// instructionsFor is what a session of the project (invalid: none) is given:
// the instructions for the audience, in order, each with its outcome. Sent
// ones fit the budget whole; one that doesn't is cut, never shortened.
func (s *Service) instructionsFor(ctx context.Context, q queryer, projectID sql.NullInt64, a InstructionAudience) (text string, picked []*Instruction, outcomes []string, err error) {
	if projectID.Valid {
		p, err := s.projectByID(ctx, q, projectID.Int64)
		if err != nil {
			return "", nil, nil, err
		}
		if p.Settings.NoAgentInstructions {
			return "", nil, nil, nil
		}
	}
	list, err := s.instructionRows(ctx, q, `i.enabled = 1 AND i.rev > 0 AND (i.project_id IS NULL OR i.project_id = ?)`, projectID)
	if err != nil {
		return "", nil, nil, err
	}
	budget := instrBudgetMain
	if a.Scope == InstrScopeSubagent {
		budget = instrBudgetSubagent
	}
	var parts []string
	used := 0
	for _, x := range list {
		yes, unknown := instrFor(x, a)
		switch {
		case unknown:
			picked, outcomes = append(picked, x), append(outcomes, InstrUnknownModel)
		case !yes:
		case used+len(x.Body)+2 > budget:
			picked, outcomes = append(picked, x), append(outcomes, InstrCut)
		default:
			used += len(x.Body) + 2
			parts = append(parts, x.Body)
			picked, outcomes = append(picked, x), append(outcomes, InstrSent)
		}
	}
	return strings.Join(parts, "\n\n"), picked, outcomes, nil
}

// deliverInstructions answers a SessionStart or SubagentStart with the
// session's instructions and records each one, sent or not (ADR145). A
// resumed session keeps its context, so it is sent only what it wasn't
// already: a new instruction, or a newer revision.
func (s *Service) deliverInstructions(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) (string, error) {
	a := InstructionAudience{Scope: InstrScopeMain, Model: cmp(strings.TrimSpace(ev.Model), x.Model), Compact: ev.Source == "compact"}
	if ev.Event == "SubagentStart" {
		a = InstructionAudience{Scope: InstrScopeSubagent, Model: strings.TrimSpace(ev.AgentModel), AgentType: ev.AgentType}
	}
	_, picked, outcomes, err := s.instructionsFor(ctx, t, x.projectID, a)
	if err != nil || len(picked) == 0 {
		return "", err
	}
	var parts []string
	for n, in := range picked {
		if outcomes[n] == InstrSent && ev.Event == "SessionStart" && ev.Source == "resume" {
			var had int
			if err := t.QueryRowContext(ctx, `SELECT count(*) FROM agent_instruction_deliveries
				WHERE session_id = ? AND instruction_id = ? AND rev = ? AND agent_id = '' AND outcome = ?`, x.ID, in.ID, in.Rev, InstrSent).Scan(&had); err != nil {
				return "", err
			}
			if had > 0 {
				continue // still in its context: neither sent nor recorded again
			}
		}
		agentID := ""
		if a.Scope == InstrScopeSubagent {
			agentID = ev.AgentID
		}
		var model any
		if a.Model != "" {
			model = a.Model
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO agent_instruction_deliveries(instruction_id, rev, session_id, project_id, agent_id, agent_type, model, event, source, chars, outcome, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, in.Rev, x.ID, x.projectID, agentID, a.AgentType, model, ev.Event, ev.Source, len(in.Body), outcomes[n], s.hookTime(ev)); err != nil {
			return "", err
		}
		if outcomes[n] == InstrSent {
			parts = append(parts, in.Body)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// PreviewInstructions is what a session of the project would be given now,
// with each instruction's outcome. Nothing is recorded.
func (s *Service) PreviewInstructions(ctx context.Context, actor *Actor, project string, a InstructionAudience) (*InstructionPreview, error) {
	var pid sql.NullInt64
	if project = strings.TrimSpace(project); project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		pid = sql.NullInt64{Int64: p.ID, Valid: true}
	}
	if err := s.canReadInstructions(ctx, actor, pid); err != nil {
		return nil, err
	}
	a.Scope = strings.ToLower(strings.TrimSpace(a.Scope))
	if a.Scope != InstrScopeMain && a.Scope != InstrScopeSubagent {
		return nil, invalid("scope is main or subagent")
	}
	text, picked, outcomes, err := s.instructionsFor(ctx, s.db.R, pid, a)
	if err != nil {
		return nil, err
	}
	res := &InstructionPreview{Text: text, Chars: len(text), Items: []InstructionPreviewItem{}}
	if pid.Valid {
		if p, err := s.projectByID(ctx, s.db.R, pid.Int64); err == nil {
			res.Off = p.Settings.NoAgentInstructions
		}
	}
	for n, x := range picked {
		res.Items = append(res.Items, InstructionPreviewItem{ID: x.ID, Title: x.Title, Outcome: outcomes[n], Chars: len(x.Body)})
	}
	return res, nil
}

// InstructionPreview is PreviewInstructions' answer.
type InstructionPreview struct {
	Text  string                   `json:"text"`
	Chars int                      `json:"chars"`
	Items []InstructionPreviewItem `json:"items"`
	// Off: the project has agent instructions turned off, so nothing is sent.
	Off bool `json:"off,omitempty"`
}

// InstructionPreviewItem is one instruction in a preview.
type InstructionPreviewItem struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Outcome string `json:"outcome"`
	Chars   int    `json:"chars"`
}
