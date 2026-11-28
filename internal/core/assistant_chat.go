package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/hermes"
)

// assistantMethods are the gateway methods the owner's browser may call:
// chatting, its sessions and attachments, models, slash commands,
// questions, background work. Prefixes end with a dot.
var assistantMethods = []string{
	"session.", "prompt.", "image.attach", "image.attach_bytes", "image.detach", "file.attach", "pdf.attach",
	"approval.", "clarify.lock", "request.answer", "slash.exec", "command.dispatch", "command.resolve",
	"commands.catalog", "complete.", "model.options", "config.get", "config.set", "config.show",
	"message.react", "insights.get", "usage.bars", "subagent.", "process.list", "process.stop", "process.kill",
	"tools.list", "tools.show", "toolsets.list", "agents.list", "cron.manage", "skills.reload", "reload.mcp",
	"rollback.", "learning.", "ping", "delegation.status", "delegation.pause",
	"mcp.servers.list", "mcp.servers.status", "mcp.servers.test",
}

func assistantMethodAllowed(m string) bool {
	return slices.ContainsFunc(assistantMethods, func(p string) bool {
		return m == p || strings.HasSuffix(p, ".") && strings.HasPrefix(m, p)
	})
}

// assistantPaths are the parts of Hermes's REST API the owner's browser
// may reach (under /api/), as path prefixes; assistantPathsRefused are
// carved out of them. Logins, updates, plugin installs, revealing secrets,
// and anything acting on the Hermes host's own desktop stay out.
var assistantPaths = []string{
	"sessions", "profiles", "chat/workspaces", "files", "fs/", "media", "git/", "cron/", "skills", "memory",
	"mcp/", "tools/", "config", "env", "providers/custom-endpoints", "analytics/", "status", "health",
	"system/stats", "curator", "learning/", "logs", "model/", "pairing", "webhooks", "gateway/", "credentials/pool",
	"ops/", "actions/", "messaging/platforms", "dashboard/themes", "dashboard/plugins", "plugins/",
}

var assistantPathsRefused = []string{
	"env/reveal", "hermes/", "health/retirement", "health/idle", "host/", "ssh/", "ops/debug-share",
	"dashboard/agent-plugins", "dashboard/plugins/catalog", "mcp/oauth", "providers/oauth", "portal",
	"skills/hub/install", "skills/hub/update", "memory/providers/", "tools/computer-use", "cron/fire",
	"messaging/whatsapp", "messaging/telegram", "local-models", "ops/import", "gateway/migrate",
}

func assistantPathAllowed(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, r := range assistantPathsRefused {
		if strings.HasPrefix(p, r) {
			return false
		}
	}
	if strings.HasSuffix(p, "/open-terminal") {
		return false
	}
	return slices.ContainsFunc(assistantPaths, func(a string) bool {
		if strings.HasSuffix(a, "/") {
			return strings.HasPrefix(p, a)
		}
		return p == a || strings.HasPrefix(p, a+"/")
	})
}

// AssistantGateway is the assistant's chat gateway, for its owner.
func (s *Service) AssistantGateway(ctx context.Context, actor *Actor, id int64) (*hermes.Gateway, *Assistant, error) {
	c, a, err := s.UseAssistant(ctx, actor, id)
	if err != nil {
		return nil, nil, err
	}
	return c.Gateway(), a, nil
}

// AssistantCall sends one gateway request for the owner.
func (s *Service) AssistantCall(ctx context.Context, actor *Actor, id int64, method string, params json.RawMessage) (json.RawMessage, error) {
	if !assistantMethodAllowed(method) {
		return nil, invalid("Waypoint doesn't send %q to the assistant", method)
	}
	gw, a, err := s.AssistantGateway(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if a.State.Frozen {
		return nil, fmt.Errorf("%s is frozen: %w", a.Name, ErrUnavailable)
	}
	var p any = params
	if len(params) == 0 {
		p = nil
	}
	res, err := gw.Call(ctx, method, p)
	if rerr, ok := err.(*hermes.RPCError); ok {
		return nil, invalid("%s", rerr.Message)
	}
	return res, AssistantError(err)
}

// AnswerAssistant answers one of the agent's questions (an approval or a
// clarify) for the owner.
func (s *Service) AnswerAssistant(ctx context.Context, actor *Actor, id int64, requestID string, result json.RawMessage) error {
	gw, _, err := s.AssistantGateway(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := gw.Answer(requestID, result); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// AssistantAPI sends one request to Hermes's REST API for the owner. The
// caller closes the response.
// extra carries the request headers that matter to Hermes (Range).
func (s *Service) AssistantAPI(ctx context.Context, actor *Actor, id int64, method, path string, q url.Values, body []byte, ctype string, extra http.Header) (*http.Response, error) {
	if !assistantPathAllowed(path) {
		return nil, fmt.Errorf("refused: Waypoint doesn't pass /api/%s to the assistant: %w", path, ErrForbidden)
	}
	c, a, err := s.UseAssistant(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if a.State.Frozen {
		return nil, fmt.Errorf("%s is frozen: %w", a.Name, ErrUnavailable)
	}
	do := c.Do
	if strings.HasPrefix(path, "files") || strings.HasPrefix(path, "fs/") || strings.HasSuffix(path, "/export") ||
		strings.HasSuffix(path, "/trigger") || strings.HasPrefix(path, "ops/backup/download") {
		do = c.DoLong
	}
	resp, err := do(ctx, method, "/api/"+path, q, body, ctype, extra)
	return resp, AssistantError(err)
}

// TestableAssistant is the client for testing the connection: its owner or
// an admin (who connects it for someone) may, since a test reaches only the
// gate's status and a fresh Hermes login, nothing of the agent's.
func (s *Service) TestableAssistant(ctx context.Context, actor *Actor, id int64) (*hermes.Client, error) {
	r, err := s.assistantRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.Person() || (actor.ID() != r.ownerID && !actor.User.IsAdmin) {
		return nil, ErrNotFound
	}
	return s.assistantClient(r)
}

// AssistantGate is the gate's control API for the owner: approvals, the
// audit trail, logs, connections.
func (s *Service) AssistantGate(ctx context.Context, actor *Actor, id int64) (*hermes.Client, *Assistant, error) {
	return s.UseAssistant(ctx, actor, id)
}

// PutAssistantConnection adds or changes a connection at the gate (the
// owner approves a widening on Signal) and records it.
func (s *Service) PutAssistantConnection(ctx context.Context, actor *Actor, id int64, name string, body map[string]any) (int, json.RawMessage, error) {
	c, a, err := s.UseAssistant(ctx, actor, id)
	if err != nil {
		return 0, nil, err
	}
	code, res, err := c.PutConnection(ctx, name, body)
	if err != nil {
		return 0, nil, gateError(err)
	}
	detail := "saved"
	if code == http.StatusAccepted {
		detail = "waiting for approval on Signal"
	}
	s.audit(ctx, actor, AuditEntry{Action: "assistant.connection", Category: AuditAccess, TargetType: "assistant", TargetID: id,
		Target: a.Name + " · " + name, URL: fmt.Sprintf("/assistant/%d?tab=tools", id), Detail: detail})
	s.goBG(func() { s.checkAssistant(context.Background(), id) })
	return code, res, nil
}

// DeleteAssistantConnection removes a connection at the gate, at once.
func (s *Service) DeleteAssistantConnection(ctx context.Context, actor *Actor, id int64, name string) error {
	c, a, err := s.UseAssistant(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := c.DeleteConnection(ctx, name); err != nil {
		return gateError(err)
	}
	s.audit(ctx, actor, AuditEntry{Action: "assistant.connection.remove", Category: AuditAccess, TargetType: "assistant", TargetID: id,
		Target: a.Name + " · " + name, URL: fmt.Sprintf("/assistant/%d?tab=tools", id)})
	return nil
}

// gateError turns the gate's 4xx answers into messages people can act on.
func gateError(err error) error {
	if he, ok := err.(*hermes.Error); ok {
		switch he.Status {
		case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
			return invalid("%s", he.Msg)
		case http.StatusNotFound:
			return fmt.Errorf("%s: %w", he.Msg, ErrNotFound)
		}
	}
	return AssistantError(err)
}

// GateError is gateError for the API's gate reads.
func GateError(err error) error { return gateError(err) }

// assistantHooks tells the owner, when no tab is watching, that the agent
// asked them something or finished a reply.
func (s *Service) assistantHooks(r *assistantRow, gw *hermes.Gateway) {
	id, owner, name := r.ID, r.ownerID, r.Name
	link := fmt.Sprintf("/assistant/%d", id)
	gw.OnRequest = func(method string, params json.RawMessage, watched bool) {
		if watched {
			return
		}
		var p struct {
			Command     string `json:"command"`
			Description string `json:"description"`
			Questions   []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(params, &p)
		title, body := name+" needs your approval", strings.TrimSpace(p.Description+"\n"+p.Command)
		if method == "clarify" {
			title = name + " has a question"
			qs := make([]string, 0, len(p.Questions))
			for _, q := range p.Questions {
				qs = append(qs, q.Question)
			}
			body = strings.Join(qs, "\n")
		}
		s.notifyURL(context.Background(), owner, nil, NotifyAssistant, title, body, link)
	}
	gw.OnEvent = func(typ, sid string, payload json.RawMessage) {
		if typ != "message.complete" || gw.Watched() || gw.IsQuiet(sid) {
			return
		}
		var p struct {
			Text   string `json:"text"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(payload, &p)
		title := name + " replied"
		switch p.Status {
		case "error":
			title = name + " stopped with an error"
		case "interrupted":
			return
		}
		s.notifyURL(context.Background(), owner, nil, NotifyAssistant, title, excerpt(p.Text, 300), link)
	}
}

// AssistantLink ties one of the assistant's sessions to an issue.
type AssistantLink struct {
	ID          int64     `json:"id"`
	AssistantID int64     `json:"assistant_id"`
	Assistant   string    `json:"assistant"`
	SessionID   string    `json:"session_id"`
	Title       string    `json:"title"`
	Issue       *IssueRef `json:"issue,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// LinkAssistantSession ties a session to an issue the owner can see.
func (s *Service) LinkAssistantSession(ctx context.Context, actor *Actor, id int64, sessionID, issueRef, title string) (*AssistantLink, error) {
	if _, _, err := s.UseAssistant(ctx, actor, id); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(sessionID) > 200 {
		return nil, invalid("which session?")
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, issueRef, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	var linkID int64
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO assistant_links(assistant_id, session_id, issue_id, title, created_by, created_at)
			VALUES (?,?,?,?,?,?) ON CONFLICT(assistant_id, session_id, issue_id) DO UPDATE SET title = excluded.title`,
			id, sessionID, i.ID, excerpt(strings.TrimSpace(title), 200), actor.ID(), s.nowMS()); err != nil {
			return err
		}
		return t.QueryRowContext(ctx, `SELECT id FROM assistant_links WHERE assistant_id = ? AND session_id = ? AND issue_id = ?`, id, sessionID, i.ID).Scan(&linkID)
	})
	if err != nil {
		return nil, err
	}
	links, err := s.assistantLinks(ctx, actor, `l.id = ?`, linkID)
	if err != nil || len(links) == 0 {
		return nil, err
	}
	return links[0], nil
}

// UnlinkAssistantSession removes a link.
func (s *Service) UnlinkAssistantSession(ctx context.Context, actor *Actor, id, linkID int64) error {
	if _, _, err := s.UseAssistant(ctx, actor, id); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		_, err := t.ExecContext(ctx, `DELETE FROM assistant_links WHERE id = ? AND assistant_id = ?`, linkID, id)
		return err
	})
}

// AssistantLinks lists an assistant's links, for one session if given.
func (s *Service) AssistantLinks(ctx context.Context, actor *Actor, id int64, sessionID string) ([]*AssistantLink, error) {
	if _, _, err := s.UseAssistant(ctx, actor, id); err != nil {
		return nil, err
	}
	if sessionID != "" {
		return s.assistantLinks(ctx, actor, `l.assistant_id = ? AND l.session_id = ?`, id, sessionID)
	}
	return s.assistantLinks(ctx, actor, `l.assistant_id = ?`, id)
}

// IssueAssistantLinks lists the conversations about an issue, from the
// assistants the actor owns.
func (s *Service) IssueAssistantLinks(ctx context.Context, actor *Actor, issueRef string) ([]*AssistantLink, error) {
	if !actor.Person() {
		return []*AssistantLink{}, nil
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, issueRef, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	return s.assistantLinks(ctx, actor, `l.issue_id = ? AND a.owner_id = ?`, i.ID, actor.ID())
}

func (s *Service) assistantLinks(ctx context.Context, actor *Actor, where string, args ...any) ([]*AssistantLink, error) {
	// #nosec G202 -- where is a fixed condition from this file; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT l.id, l.assistant_id, a.name, l.session_id, l.title, l.created_at,
		i.id, p.key || '-' || i.number, i.title, i.type
		FROM assistant_links l JOIN assistants a ON a.id = l.assistant_id
		LEFT JOIN issues i ON i.id = l.issue_id LEFT JOIN projects p ON p.id = i.project_id
		WHERE `+where+` ORDER BY l.created_at DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AssistantLink{}
	for rows.Next() {
		var l AssistantLink
		var created int64
		var iid *int64
		var key, title, typ *string
		if err := rows.Scan(&l.ID, &l.AssistantID, &l.Assistant, &l.SessionID, &l.Title, &created, &iid, &key, &title, &typ); err != nil {
			return nil, err
		}
		l.CreatedAt = time.UnixMilli(created).UTC()
		if iid != nil {
			l.Issue = &IssueRef{ID: *iid, Key: *key, Title: *title, Type: *typ}
		}
		out = append(out, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	visible := out[:0]
	for _, l := range out {
		if l.Issue == nil {
			visible = append(visible, l)
			continue
		}
		if _, _, err := s.getIssueFor(ctx, s.db.R, actor, l.Issue.Key, RoleViewer, false); err == nil {
			visible = append(visible, l)
		}
	}
	return visible, nil
}
