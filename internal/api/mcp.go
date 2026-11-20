package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/wql"
)

// MCP: a stateless Model Context Protocol server over streamable HTTP.
// Each POST carries one JSON-RPC message; responses are plain JSON. Agents
// authenticate with the same bearer tokens as the REST API.

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpMaxBatch bounds the work one JSON-RPC batch request can ask for.
const mcpMaxBatch = 20

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	actor, err := s.authenticate(r)
	if err != nil || r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="waypoint"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "send Authorization: Bearer <Waypoint API token>"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.mcpBodyLimit()))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	// Only agents speak MCP: changes made here count as an agent's (WP-D10).
	actor.MCP = true
	ctx := context.WithValue(r.Context(), actorKey, actor)
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			return
		}
		if len(reqs) > mcpMaxBatch {
			writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, fmt.Sprintf("batch too large (max %d)", mcpMaxBatch)}})
			return
		}
		var out []rpcResponse
		for _, req := range reqs {
			if resp := s.mcpHandle(ctx, actor, req); resp != nil {
				out = append(out, *resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	resp := s.mcpHandle(ctx, actor, req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) mcpHandle(ctx context.Context, actor *core.Actor, req rpcRequest) *rpcResponse {
	if len(req.ID) == 0 {
		return nil // notification
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		// Malformed params fall back to the default version below.
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "waypoint", "title": "Waypoint", "version": s.cfg.Version},
			"instructions": "Waypoint is a personal issue tracker. Issues have keys like WAY-12. " +
				"Use search_issues with the query language (call query_help for the reference) to find work, " +
				"get_issue for detail, and create_issue / update_issue / transition_issue / add_comment to change it. " +
				"Refer to statuses, users, sprints and projects by name. " +
				"Before changing a feature or screen, read what people intend with get_spec; look up project terms with glossary_lookup; " +
				"search finds issues, design notes, glossary terms, pages and comments at once. Approved design notes are append-only for you (add_to_design). Record decisions as design notes (save_design, kind decision: they get an ADR code, like ADR7), not pages; plans, experiments and notes stay pages.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolsFor(actor)}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			return resp
		}
		err := s.checkTool(ctx, actor, p.Name, p.Arguments)
		var text string
		if err == nil {
			text, err = s.mcpCall(ctx, actor, p.Name, p.Arguments)
			err = s.svc.Refused(ctx, actor, "MCP tool "+p.Name, err)
		}
		if err != nil {
			msg := err.Error()
			if !core.IsInvalid(err) && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrForbidden) && !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrConflict) && !errors.Is(err, errBadRequest) {
				// Same rule as REST: unexpected errors are logged, not shown.
				s.log.ErrorContext(ctx, "mcp tool failed", "tool", p.Name, "err", err)
				msg = "internal error"
			}
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": "Error: " + msg}}, "isError": true}
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	case "resources/list":
		resp.Result = map[string]any{"resources": []any{}}
	case "prompts/list":
		resp.Result = map[string]any{"prompts": []any{}}
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func obj(required []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var readOnly = map[string]any{"readOnlyHint": true}

var issueFields = map[string]any{
	"title":       str("Title"),
	"description": str("Markdown description"),
	"type":        str("epic, story, task, bug or subtask"),
	"status":      str("Status name, or a category: backlog, todo, in_progress, done, canceled"),
	"priority":    str("none, low, medium, high or urgent"),
	"parent":      str("Parent issue key; empty string clears it on update"),
	"sprint":      str("Sprint name, 'active', 'next', or 'none'"),
	"assignees":   strList("Usernames, or 'me'; replaces existing assignees"),
	"labels":      strList("Label names; replaces existing labels (created if new)"),
	"points":      map[string]any{"type": "number", "description": "Story points"},
	"due_date":    str("YYYY-MM-DD, today, tomorrow, +3d, friday; empty clears"),
	"start_date":  str("Same formats as due_date"),
}

func withFields(extra map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range issueFields {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var mcpTools = []mcpTool{
	{Name: "search_issues", Title: "Search issues", Annotations: readOnly,
		Description: "Find issues with Waypoint's query language, e.g. 'assignee:me is:open order:-priority' or 'under:WAY-3 is:open' or free text. Returns one line per issue.",
		InputSchema: obj([]string{"query"}, map[string]any{
			"query":   str("Query; see query_help"),
			"project": str("Optional project key to limit to"),
			"limit":   map[string]any{"type": "integer", "description": "Max results (default 50)"},
			"offset":  map[string]any{"type": "integer"},
		})},
	{Name: "get_issue", Title: "Get issue", Annotations: readOnly,
		Description: "Full detail of an issue: description, children, relations, links and comments.",
		InputSchema: obj([]string{"key"}, map[string]any{"key": str("Issue key, e.g. WAY-12")})},
	{Name: "create_issue", Title: "Create issue",
		Description: "Create an issue. Give project (key) or parent (key), and a title or a template.",
		InputSchema: obj(nil, withFields(map[string]any{"project": str("Project key"), "template": str("Optional issue template name: fills in what you leave out and adds its sub-tasks")}))},
	{Name: "update_issue", Title: "Update issue",
		Description: "Change fields of an issue. Only the fields you pass change.",
		InputSchema: obj([]string{"key"}, withFields(map[string]any{
			"key":           str("Issue key"),
			"add_labels":    strList("Labels to add"),
			"remove_labels": strList("Labels to remove"),
			"add_assignees": strList("Usernames to add"),
			"comment":       str("Optional comment, saved together with the change"),
		}))},
	{Name: "transition_issue", Title: "Change status",
		Description: "Move an issue to a status (by name, or category like done / in_progress), optionally commenting.",
		InputSchema: obj([]string{"key", "status"}, map[string]any{"key": str("Issue key"), "status": str("Status name or category"), "comment": str("Optional comment")})},
	{Name: "add_comment", Title: "Comment",
		Description: "Add a markdown comment to an issue. @username mentions notify people.",
		InputSchema: obj([]string{"key", "body"}, map[string]any{"key": str("Issue key"), "body": str("Comment")})},
	{Name: "list_projects", Title: "List projects", Annotations: readOnly,
		Description: "Projects you can see, with their keys and open issue counts.",
		InputSchema: obj(nil, map[string]any{})},
	{Name: "get_project", Title: "Get project", Annotations: readOnly,
		Description: "A project's statuses, labels, members and open sprints.",
		InputSchema: obj([]string{"project"}, map[string]any{"project": str("Project key")})},
	{Name: "get_board", Title: "Get board", Annotations: readOnly,
		Description: "The project's board: issues grouped by status column.",
		InputSchema: obj([]string{"project"}, map[string]any{"project": str("Project key"), "query": str("Optional filter")})},
	{Name: "forecast", Title: "Forecast", Annotations: readOnly,
		Description: "When remaining work will likely be done, from recent throughput (50/85/95% dates).",
		InputSchema: obj([]string{"project"}, map[string]any{"project": str("Project key"), "query": str("Optional scope, e.g. under:WAY-3")})},
	{Name: "log_time", Title: "Log time",
		Description: "Record time spent on an issue, e.g. duration '1h30m'.",
		InputSchema: obj([]string{"issue", "duration"}, map[string]any{"issue": str("Issue key"), "duration": str("e.g. 45m, 1h30m, 1:30"), "note": str("Optional note")})},
	{Name: "timer", Title: "Timer",
		Description: "Start a timer on an issue, stop the running timer, or show it.",
		InputSchema: obj([]string{"action"}, map[string]any{"action": map[string]any{"type": "string", "enum": []string{"start", "stop", "status"}}, "issue": str("Issue key, for start"), "note": str("Optional note")})},
	{Name: "query_help", Title: "Query language help", Annotations: readOnly,
		Description: "Reference for the search query language.",
		InputSchema: obj(nil, map[string]any{})},
	{Name: "next_task", Title: "Take the next task",
		Description: "Claim the most urgent task for an agent like you (delegated to agents, then ones anyone may take), with its step instructions and related work. Asking again returns the task you hold.",
		InputSchema: obj(nil, agentQueryProps())},
	{Name: "batch_tasks", Title: "Take the next task and related work",
		Description: "Like next_task, plus up to 10 related tasks you may take, all claimed together.",
		InputSchema: obj(nil, agentQueryProps())},
	{Name: "finish_task", Title: "Finish your part of a task",
		Description: "Moves the task to its next workflow step (or Done), saves your summary as a comment and releases your claim.",
		InputSchema: obj([]string{"key"}, map[string]any{"key": str("Issue key"), "comment": str("Summary of what you did (markdown)"),
			"agent":        str("Your identity, e.g. anthropic/claude-opus-5-5, if your token doesn't say"),
			"input_tokens": map[string]any{"type": "integer"}, "output_tokens": map[string]any{"type": "integer"}, "cost_usd": map[string]any{"type": "number"}})},
	{Name: "release_task", Title: "Give a task back",
		Description: "Release a claimed task without finishing it; say why in the comment. Say what it waits for (until, after, requests, reviewed, person) and it stays on hold until then. With none it isn't handed out for 30 minutes, doubling each time it is given back again.",
		InputSchema: obj([]string{"key"}, holdSchema(map[string]any{"key": str("Issue key"), "comment": str("Why"), "agent": str("Your identity, if your token doesn't say"),
			"until": str("Not before: 2026-11-01, friday, +3d or an RFC 3339 time"), "note": str("Why it waits, when that isn't the comment")}))},
	{Name: "decline_task", Title: "Turn down a task another repo filed",
		Description: "Close a task an agent filed from another repo (get_issue shows filed_from) that doesn't belong in this one or doesn't fit what its people want: it's canceled with your reason, and the side that filed it hears why.",
		InputSchema: obj([]string{"key", "comment"}, map[string]any{"key": str("Issue key"), "comment": str("Why it doesn't belong here, and where it does"), "agent": str("Your identity, if your token doesn't say")})},
	{Name: "hold_task", Title: "Put a task on hold",
		Description: "Keep a task from agents (next and take) until a date, until tasks close or requests are answered, until a person reviews it, or until someone lifts the hold: work that must wait for something. All of what you give must be met. Your claim on it ends. off lifts a hold; a hold a person placed only a person can change.",
		InputSchema: obj([]string{"key"}, holdSchema(map[string]any{"key": str("Issue key"), "until": str("When it ends: 2026-11-01, friday, +3d or an RFC 3339 time; empty until someone lifts it"),
			"note": str("Why it waits"), "off": map[string]any{"type": "boolean", "description": "Lift the hold instead"}}))},
	{Name: "list_pages", Title: "List pages", Annotations: readOnly,
		Description: "Pages (plans, ideas, decisions, notes…) you can read, with their codes (PL13 = plan 13 in its project). Bodies left out; use get_page. Pages an agent wrote say so, and whether a person has reviewed them.",
		InputSchema: obj(nil, map[string]any{"project": str("Project key, or none for personal pages"), "category": str("plan, idea, experiment, reference, note, decision, problem, feature or bug"),
			"query": str("Full-text search"), "issue": str("Pages linked to this issue key")})},
	{Name: "get_page", Title: "Read a page", Annotations: readOnly,
		Description: "A page with its body, sub-pages, linked issues and who wrote it. Give its id, or its code (PL13; APP-PL13 for another project's).",
		InputSchema: obj(nil, map[string]any{"id": map[string]any{"type": "integer"}, "code": str("Page code, like PL13 or APP-PL13"),
			"project": str("Project key a bare code is in")})},
	{Name: "save_page", Title: "Create or edit a page",
		Description: "Without id, creates a page; with id, changes the fields you pass. Decisions belong in design notes (save_design, kind decision), not pages; plans, experiments and notes stay pages.",
		InputSchema: obj(nil, map[string]any{"id": map[string]any{"type": "integer"}, "title": str("Title"), "body": str("Markdown body"),
			"category": str("plan, idea, experiment, reference, note, decision, problem, feature or bug"), "project": str("Project key (create only)"),
			"code":   str("The page's code, if it has one from elsewhere (PL13 for an imported plan 13); new pages get the next one"),
			"parent": map[string]any{"type": "integer", "description": "Parent page id (create only)"}, "issues": strList("Issue keys to link (create only)")})},
}

var knowledgeTools = []mcpTool{
	{Name: "take_task", Title: "Take a named task",
		Description: "Claim a specific task a person asked you to work on, even if it wasn't delegated: assigns it to you, moves it to In Progress and starts your timer. finish_task or release_task stops it.",
		InputSchema: obj([]string{"key"}, map[string]any{"key": str("Issue key"), "agent": str("Your identity, if your token doesn't say")})},
	{Name: "search", Title: "Search everything", Annotations: readOnly,
		Description: "Search issues, design notes, glossary terms, pages and comments at once, grouped by kind with snippets. Closed issues and retired notes are left out unless archived is true.",
		InputSchema: obj([]string{"query"}, map[string]any{"query": str("Words (issues also take the query language)"),
			"kinds":    strList("Only these kinds: issues, design, glossary, pages, comments (or logs)"),
			"project":  strList("Only these project keys (glossary and design include their parents' and global items)"),
			"archived": map[string]any{"type": "boolean", "description": "Include closed issues and retired notes"},
			"limit":    map[string]any{"type": "integer", "description": "Max hits per kind (default 10)"}})},
	{Name: "glossary_lookup", Title: "Define a term", Annotations: readOnly,
		Description: "What a term means in a project: its own glossary first, then its parents', then the global one. Matches terms and aliases exactly; for a word the project avoids, says which term to use; suggests close ones otherwise.",
		InputSchema: obj([]string{"term"}, map[string]any{"term": str("The term"), "project": str("Project key")})},
	{Name: "list_glossary", Title: "List glossary", Annotations: readOnly,
		Description: "A project's full glossary (its own, its parents' and global terms), or a full-text search of it.",
		InputSchema: obj(nil, map[string]any{"project": str("Project key; empty for everything you can see"), "query": str("Optional full-text search")})},
	{Name: "save_glossary", Title: "Add or edit a glossary term",
		Description: "Without id, adds a term (to project, or global if empty; admins only); with id, changes the fields you pass. Locked terms can't be changed by agents.",
		InputSchema: obj(nil, map[string]any{"id": map[string]any{"type": "integer"}, "term": str("Term"), "definition": str("Markdown; [[Other term]] links"),
			"aliases": strList("Other names"), "avoid": strList("Other names for it this project doesn't use (a lookup of one points here)"), "project": str("Project key"), "links": strList("Create only: things to link (APP-12, page:4, a PR URL, a repo file path)")})},
	{Name: "get_spec", Title: "Get the spec for a feature, screen or decision", Annotations: readOnly,
		Description: "What people intend for something: the design note (approved or only proposed), people-approved and unapproved agent additions, its parts, related notes, terms, tasks and some PRs/commits. Without a topic: the project's goals and principles plus an index.",
		InputSchema: obj(nil, map[string]any{"topic": str("A title, words from it, a note id, or a decision code (ADR7, APP-ADR7)"), "project": str("Project key")})},
	{Name: "list_design", Title: "List design notes", Annotations: readOnly,
		Description: "Design notes (goals, principles, features, screens, decisions, requirements, notes) in a project's scope.",
		InputSchema: obj(nil, map[string]any{"project": str("Project key"), "kind": str("Only this kind"), "query": str("Optional full-text search"), "status": str("active (default), archived or all")})},
	{Name: "save_design", Title: "Propose or edit a design note",
		Description: "Without id, proposes a note (a person approves it); pass for (the task key) when you write it to carry out a task a person filed, and it counts on their request instead (nobody approved it, so don't call it approved or accepted: the link to the task shows why it counts). With id: edits your own unapproved note or one written for a person's task, or, on any other approved note, proposes an edit that waits for a person.",
		InputSchema: obj(nil, map[string]any{"id": map[string]any{"type": []string{"integer", "string"}, "description": "The note: its number or code (ADR7, APP-ADR7)"}, "title": str("Title"), "body": str("Markdown; [[links]] to terms, notes, pages, issue keys"),
			"kind": str("goal, principle, feature, screen, decision (gets an ADR code), requirement or note"), "project": str("Project key (create; also where a bare code is looked up)"),
			"parent": map[string]any{"type": "integer", "description": "Parent note id (create)"}, "links": strList("Create only: things to link"),
			"for": str("Create only: the task you are writing this for, like WP-177. If a person filed it the note counts on their request; otherwise it stays a proposal")})},
	{Name: "add_to_design", Title: "Add to a design note",
		Description: "Append to a design note (what you learned or decided while working). It shows as an unapproved agent addition until a person approves it.",
		InputSchema: obj([]string{"id", "body"}, map[string]any{"id": map[string]any{"type": []string{"integer", "string"}, "description": "The note: its number or code (ADR7, APP-ADR7)"}, "body": str("Markdown")})},
	{Name: "goals", Title: "Goals and what traces to them", Annotations: readOnly,
		Description: "A project's goals (what people want it to do, in their words), how much work traces to each and how firmly, and drift. With gaps: the open tasks that trace to no approved goal (with the goal each likely serves), links and notes no person approved, and questions worth asking a person. Read it before starting work no person asked for; change how freely, as long as the work serves an approved goal.",
		InputSchema: obj([]string{"project"}, map[string]any{"project": str("Project key"), "gaps": map[string]any{"type": "boolean", "description": "List what needs a person instead"}})},
	{Name: "trace", Title: "Trace work to its goal", Annotations: readOnly,
		Description: "Like git blame for intent: the goals an issue, page, design note, pull request or commit serves, each step and who made it (person, agent, Waypoint's guess), whether a person approved the goal, and how far it is from the nearest thing a person wrote or approved.",
		InputSchema: obj(nil, map[string]any{"ref": str("An issue key, design note code or number, or page code"), "project": str("Project key (for pull and bare codes)"),
			"repo": str("owner/name, for pull"), "pull": map[string]any{"type": "integer", "description": "A pull request number"},
			"commit": str("A commit id (7+ hex)"), "keys": strList("Task keys the commit's message names")})},
	{Name: "link", Title: "Link two things",
		Description: "Link an issue, page, glossary term or design note to another, or to a file or URL. Refs: APP-12, page:4, glossary:Term, design:12, file:path, or a URL.",
		InputSchema: obj([]string{"from", "to"}, map[string]any{"from": str("Source ref"), "to": str("Target ref"),
			"rel": str("relates (default), implements, depends_on or supersedes"), "note": str("Optional note")})},
}

func init() { mcpTools = append(mcpTools, knowledgeTools...) }

func agentQueryProps() map[string]any {
	return map[string]any{
		"agent":   str("Your identity, e.g. anthropic/claude-opus-5-5, if your token doesn't say"),
		"project": str("Only this project (key)"),
		"repo":    str("Only work for this repo (owner/name)"),
		"step":    str("Only tasks at this workflow step, e.g. review"),
		"query":   str("More filters in the query language"),
		"peek":    map[string]any{"type": "boolean", "description": "Look without claiming"},
	}
}

// mcpArgs is a loose view over tool arguments.
type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	if v, ok := a[k].(float64); ok {
		return fmt.Sprint(v)
	}
	return ""
}

// holdArgs is what a hold or a release waits for (WP-ADR75).
func holdArgs(a mcpArgs) core.HoldInput {
	return core.HoldInput{Until: a.str("until"), Note: a.str("note"), After: a.list("after"), Requests: a.list("requests"), Reviewed: a.flag("reviewed"), Person: a.flag("person")}
}

// holdSchema describes holdArgs in a tool's input schema.
func holdSchema(in map[string]any) map[string]any {
	in["after"] = map[string]any{"type": "array", "items": str("Issue key"), "description": "Tasks that must close first"}
	in["requests"] = map[string]any{"type": "array", "items": str("Request, R-12 or KEY:R-12"), "description": "Requests that must be answered first"}
	in["reviewed"] = map[string]any{"type": "boolean", "description": "Wait until a person reviews what an agent wrote on this task"}
	in["person"] = map[string]any{"type": "boolean", "description": "Wait until a person lifts it: for stale work or work that conflicts with the project's goals"}
	return in
}

// flag reads a boolean, also from a client that sends it as "true".
func (a mcpArgs) flag(k string) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	return strings.EqualFold(a.str(k), "true")
}

func (a mcpArgs) has(k string) bool { _, ok := a[k]; return ok }

// list reads an array of names, or one comma-separated string.
func (a mcpArgs) list(k string) []string {
	var parts []string
	switch v := a[k].(type) {
	case string:
		parts = strings.Split(v, ",")
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
	default:
		return nil
	}
	out := []string{}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// points reads story points: a number or numeric string; null or "" clears.
func (a mcpArgs) points() (core.Opt[float64], error) {
	if !a.has("points") {
		return core.Opt[float64]{}, nil
	}
	switch v := a["points"].(type) {
	case nil:
		return core.Null[float64](), nil
	case float64:
		return core.Some(v), nil
	case string:
		if v = strings.TrimSpace(v); v == "" {
			return core.Null[float64](), nil
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return core.Some(f), nil
		}
	}
	return core.Opt[float64]{}, fmt.Errorf("%w: points must be a number, or null to clear", errBadRequest)
}

// issuePatch builds an update from tool arguments. Agents often send
// numbers as strings and null to clear, so both are accepted where they
// make sense, and anything ambiguous is an error rather than a silent clear.
func (a mcpArgs) issuePatch() (core.IssuePatch, error) {
	var p core.IssuePatch
	bad := func(k, want string) error { return fmt.Errorf("%w: %s must be %s", errBadRequest, k, want) }
	text := func(k string) (*string, error) {
		switch v := a[k].(type) {
		case nil:
			if a.has(k) {
				return nil, bad(k, "a string (it can't be cleared)")
			}
			return nil, nil
		case string:
			return &v, nil
		default:
			return nil, bad(k, "a string")
		}
	}
	var err error
	for k, dst := range map[string]**string{"title": &p.Title, "description": &p.Description, "type": &p.Type, "status": &p.Status} {
		if *dst, err = text(k); err != nil {
			return p, err
		}
	}
	switch v := a["priority"].(type) {
	case nil:
	case string, float64:
		p.Priority = v
	default:
		return p, bad("priority", "a name or number")
	}
	for k, o := range map[string]*core.Opt[string]{"parent": &p.Parent, "sprint": &p.Sprint, "due_date": &p.DueDate, "start_date": &p.StartDate} {
		if !a.has(k) {
			continue
		}
		switch v := a[k].(type) {
		case nil:
			*o = core.Null[string]()
		case string:
			if v == "" {
				*o = core.Null[string]()
			} else {
				*o = core.Some(v)
			}
		default:
			return p, bad(k, "a string, or null to clear")
		}
	}
	if p.Points, err = a.points(); err != nil {
		return p, err
	}
	if a.has("assignees") {
		l := a.list("assignees")
		p.Assignees = &l
	}
	if a.has("labels") {
		l := a.list("labels")
		p.Labels = &l
	}
	p.AddLabels, p.RemLabels, p.AddAssign = a.list("add_labels"), a.list("remove_labels"), a.list("add_assignees")
	if c, ok := a["comment"].(string); ok {
		p.Comment = c
	}
	return p, nil
}

func (a mcpArgs) int(k string, def int) int {
	if v, ok := a[k].(float64); ok {
		return int(v)
	}
	return def
}

func (s *Server) mcpCall(ctx context.Context, actor *core.Actor, name string, raw json.RawMessage) (string, error) {
	args := mcpArgs{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("%w: arguments must be an object", errBadRequest)
		}
	}
	svc := s.svc
	switch name {
	case "search_issues":
		res, err := svc.Search(ctx, actor, core.SearchParams{Query: args.str("query"), Project: args.str("project"),
			Limit: args.int("limit", 50), Offset: args.int("offset", 0), DefaultOrder: "updated"})
		if err != nil {
			return "", err
		}
		return issuesText(res), nil
	case "get_issue":
		d, err := svc.GetIssue(ctx, actor, args.str("key"))
		if err != nil {
			return "", err
		}
		comments, _ := svc.ListComments(ctx, actor, d.Key)
		return issueText(d, comments), nil
	case "create_issue":
		in := core.IssueInput{Project: args.str("project"), Title: args.str("title"), Description: args.str("description"),
			Type: args.str("type"), Status: args.str("status"), Priority: args.str("priority"), Parent: args.str("parent"),
			Sprint: args.str("sprint"), Assignees: args.list("assignees"), Labels: args.list("labels"),
			DueDate: args.str("due_date"), StartDate: args.str("start_date"), Template: args.str("template")}
		pts, err := args.points()
		if err != nil {
			return "", err
		}
		in.Points = pts.Value
		d, err := svc.CreateIssue(ctx, actor, in)
		if err != nil {
			return "", err
		}
		return "Created " + issueLine(d.Issue), nil
	case "update_issue":
		p, err := args.issuePatch()
		if err != nil {
			return "", err
		}
		d, err := svc.UpdateIssue(ctx, actor, args.str("key"), p)
		if err != nil {
			return "", err
		}
		return "Updated " + issueLine(d.Issue), nil
	case "transition_issue":
		d, err := svc.Transition(ctx, actor, args.str("key"), args.str("status"), args.str("comment"))
		if err != nil {
			return "", err
		}
		return "Moved " + issueLine(d.Issue), nil
	case "add_comment":
		if _, err := svc.AddComment(ctx, actor, args.str("key"), args.str("body")); err != nil {
			return "", err
		}
		return "Commented on " + strings.ToUpper(args.str("key")), nil
	case "list_projects":
		ps, err := svc.ListProjects(ctx, actor, false)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, p := range ps {
			fmt.Fprintf(&b, "%s %q — %d open, %s mode, you are %s\n", p.Key, p.Name, p.OpenIssues, p.Settings.Mode, p.Role)
		}
		if len(ps) == 0 {
			b.WriteString("No projects.\n")
		}
		return b.String(), nil
	case "get_project":
		p, err := svc.GetProject(ctx, actor, args.str("project"))
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s %q (%s mode, %d open)\n", p.Key, p.Name, p.Settings.Mode, p.OpenIssues)
		if p.Description != "" {
			b.WriteString(p.Description + "\n")
		}
		b.WriteString("Statuses:")
		for _, st := range p.Statuses {
			fmt.Fprintf(&b, " %q(%s)", st.Name, st.Category)
		}
		b.WriteString("\nMembers:")
		for _, m := range p.Members {
			fmt.Fprintf(&b, " @%s(%s)", m.User.Username, m.Role)
		}
		if len(p.Labels) > 0 {
			b.WriteString("\nLabels:")
			for _, l := range p.Labels {
				b.WriteString(" " + l.Name)
			}
		}
		for _, sp := range p.Sprints {
			fmt.Fprintf(&b, "\nSprint %q %s %s..%s, %d issues, %g/%g pts", sp.Name, sp.State, deref(sp.StartDate), deref(sp.EndDate), sp.IssueCount, sp.DonePoints, sp.Points)
		}
		return b.String() + "\n", nil
	case "get_board":
		bd, err := svc.GetBoard(ctx, actor, args.str("project"), args.str("query"))
		if err != nil {
			return "", err
		}
		var b strings.Builder
		if bd.Sprint != nil {
			fmt.Fprintf(&b, "Sprint %q (%s..%s)\n", bd.Sprint.Name, deref(bd.Sprint.StartDate), deref(bd.Sprint.EndDate))
		} else if bd.Mode == "sprints" {
			b.WriteString("No active sprint.\n")
		}
		for _, c := range bd.Columns {
			fmt.Fprintf(&b, "\n## %s (%d)\n", c.Status.Name, len(c.Issues))
			for _, i := range c.Issues {
				b.WriteString(issueLine(i) + "\n")
			}
		}
		return b.String(), nil
	case "forecast":
		fc, err := svc.ForecastWork(ctx, actor, args.str("project"), args.str("query"))
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(fc, "", "  ")
		return string(b), nil
	case "log_time":
		issue, dur, note := args.str("issue"), args.str("duration"), args.str("note")
		e, err := svc.CreateTimeEntry(ctx, actor, core.TimeEntryInput{Issue: &issue, Duration: &dur, Note: &note})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Logged %dm on %s", e.Seconds/60, e.Issue.Key), nil
	case "timer":
		switch strings.ToLower(args.str("action")) {
		case "start":
			t, err := svc.StartTimer(ctx, actor, args.str("issue"), args.str("note"))
			if err != nil {
				return "", err
			}
			if t.Issue != nil {
				return "Timer running on " + t.Issue.Key, nil
			}
			return "Timer running", nil
		case "stop":
			t, err := svc.StopTimer(ctx, actor)
			if err != nil {
				return "", err
			}
			if t == nil {
				return "No timer was running", nil
			}
			return "Stopped after " + humanDuration(t.Seconds), nil
		case "status", "":
			t, err := svc.RunningTimer(ctx, actor)
			if err != nil {
				return "", err
			}
			if t == nil {
				return "No timer running", nil
			}
			on := ""
			if t.Issue != nil {
				on = " on " + t.Issue.Key
			}
			return "Running" + on + " for " + humanDuration(t.Seconds), nil
		default:
			return "", fmt.Errorf("%w: action must be start, stop or status", errBadRequest)
		}
	case "query_help":
		return wql.Help, nil
	case "next_task", "batch_tasks":
		q := core.AgentQuery{Agent: args.str("agent"), Project: args.str("project"), Repo: args.str("repo"), Step: args.str("step"), Query: args.str("query")}
		q.Peek, _ = args["peek"].(bool)
		take := svc.AgentNext
		if name == "batch_tasks" {
			take = svc.AgentBatch
		}
		t, err := take(ctx, actor, q)
		if p := (*core.ErrAgentsPaused)(nil); errors.As(err, &p) {
			return p.Error(), nil
		}
		if err != nil {
			return "", err
		}
		if t == nil {
			return "Nothing to do: no open task for an agent like you.", nil
		}
		return agentTaskText(t, taskText{}), nil
	case "finish_task":
		in := core.AgentDoneInput{Agent: args.str("agent"), Comment: args.str("comment")}
		if args.has("input_tokens") || args.has("output_tokens") || args.has("cost_usd") {
			in.Usage = &core.AgentUsage{InputTokens: int64(args.int("input_tokens", 0)), OutputTokens: int64(args.int("output_tokens", 0))}
			in.Usage.CostUSD, _ = args["cost_usd"].(float64)
		}
		res, err := svc.AgentDone(ctx, actor, args.str("key"), in)
		if err != nil {
			return "", err
		}
		return res.Message, nil
	case "release_task":
		res, err := svc.AgentRelease(ctx, actor, args.str("key"), core.ReleaseInput{Agent: args.str("agent"), Comment: args.str("comment"), HoldInput: holdArgs(args)})
		if err != nil {
			return "", err
		}
		return res.Message, nil
	case "decline_task":
		i, err := svc.DeclineIssue(ctx, actor, args.str("key"), args.str("agent"), args.str("comment"))
		if err != nil {
			return "", err
		}
		return "Declined " + i.Key + "; the repo that filed it hears why.", nil
	case "hold_task":
		var d *core.IssueDetail
		var err error
		if off, _ := args["off"].(bool); off {
			d, err = svc.LiftHold(ctx, actor, args.str("key"))
		} else {
			d, err = svc.SetHold(ctx, actor, args.str("key"), holdArgs(args))
		}
		if err != nil {
			return "", err
		}
		if d.Hold == nil {
			return d.Key + " is off hold: agents can take it again.", nil
		}
		return strings.TrimSuffix(holdLine(d.Issue), "\n"), nil
	case "list_pages":
		pages, err := svc.ListPages(ctx, actor, core.PageQuery{Project: args.str("project"), Category: args.str("category"), Text: args.str("query"), Issue: args.str("issue")})
		if err != nil {
			return "", err
		}
		return pagesText(pages), nil
	case "get_page":
		id := int64(args.int("id", 0))
		if c := args.str("code"); c != "" {
			var err error
			if id, err = svc.ResolvePage(ctx, actor, c, args.str("project")); err != nil {
				return "", err
			}
		}
		p, err := svc.GetPage(ctx, actor, id)
		if err != nil {
			return "", err
		}
		svc.NoteLookup(ctx, actor, core.KPage, id, "")
		return pageText(p), nil
	case "save_page":
		in := core.PageInput{}
		for k, dst := range map[string]**string{"title": &in.Title, "body": &in.Body, "category": &in.Category, "code": &in.Code} {
			if v, ok := args[k].(string); ok {
				*dst = &v
			}
		}
		if id := int64(args.int("id", 0)); id != 0 {
			p, err := svc.UpdatePage(ctx, actor, id, in)
			if err != nil {
				return "", err
			}
			return "Saved page " + pageRefText(p.Ref()), nil
		}
		if v := args.str("project"); v != "" {
			in.Project = core.Some(v)
		}
		if v := int64(args.int("parent", 0)); v != 0 {
			in.Parent = core.Some(v)
		}
		if l := args.list("issues"); len(l) > 0 {
			in.Issues = &l
		}
		p, err := svc.CreatePage(ctx, actor, in)
		if err != nil {
			return "", err
		}
		if p.Notice != "" {
			return "Created page " + pageRefText(p.Ref()) + ". " + p.Notice, nil
		}
		return "Created page " + pageRefText(p.Ref()), nil
	}
	if text, ok, err := s.mcpAutomations(ctx, actor, name, args); ok {
		return text, err
	}
	if text, ok, err := s.mcpKnowledge(ctx, actor, name, args); ok {
		return text, err
	}
	if text, ok, err := s.mcpRequests(ctx, actor, name, args); ok {
		return text, err
	}
	if text, ok, err := s.mcpAttach(ctx, actor, name, args); ok {
		return text, err
	}
	if text, ok, err := s.mcpEnvironments(ctx, actor, name, args); ok {
		return text, err
	}
	return "", fmt.Errorf("%w: unknown tool %q", errBadRequest, name)
}

// mcpKnowledge handles the glossary, design, spec, link and search tools.
func (s *Server) mcpKnowledge(ctx context.Context, actor *core.Actor, name string, args mcpArgs) (string, bool, error) {
	svc := s.svc
	optStr := func(k string) *string {
		if v, ok := args[k].(string); ok {
			return &v
		}
		return nil
	}
	switch name {
	case "take_task":
		t, err := svc.AgentTake(ctx, actor, args.str("key"), args.str("agent"))
		if err != nil {
			return "", true, err
		}
		return agentTaskText(t, taskText{}), true, nil
	case "search":
		q := core.AllQuery{Text: args.str("query"), Kinds: args.list("kinds"), Projects: args.list("project"), Limit: args.int("limit", 10)}
		q.Archived, _ = args["archived"].(bool)
		res, err := svc.SearchAll(ctx, actor, q)
		if err != nil {
			return "", true, err
		}
		return searchAllText(res), true, nil
	case "glossary_lookup":
		res, err := svc.LookupGlossary(ctx, actor, args.str("term"), args.str("project"))
		if err != nil {
			return "", true, err
		}
		svc.NoteGlossaryLookup(ctx, actor, res)
		return glossaryLookupText(res, args.str("project")), true, nil
	case "list_glossary":
		list, err := svc.ListGlossary(ctx, actor, core.GlossaryQuery{Project: args.str("project"), Text: args.str("query")})
		if err != nil {
			return "", true, err
		}
		return glossaryListText(list), true, nil
	case "save_glossary":
		in := core.GlossaryInput{Term: optStr("term"), Definition: optStr("definition"), Links: args.list("links")}
		if args.has("aliases") {
			l := args.list("aliases")
			in.Aliases = &l
		}
		if args.has("avoid") {
			l := args.list("avoid")
			in.Avoid = &l
		}
		if id := int64(args.int("id", 0)); id != 0 {
			if args.has("project") {
				in.Project = optStr("project")
			}
			g, err := svc.UpdateGlossary(ctx, actor, id, in)
			if err != nil {
				return "", true, err
			}
			return fmt.Sprintf("Saved term #%d %s (%s).", g.ID, g.Term, scopeName(g.KScope)), true, nil
		}
		p := args.str("project")
		in.Project = &p
		g, err := svc.CreateGlossary(ctx, actor, in)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("Added term #%d %s (%s).", g.ID, g.Term, scopeName(g.KScope)), true, nil
	case "get_spec":
		if strings.TrimSpace(args.str("topic")) == "" {
			ov, err := svc.GetSpecOverview(ctx, actor, args.str("project"))
			if err != nil {
				return "", true, err
			}
			return specOverviewText(ov), true, nil
		}
		sp, err := svc.GetSpec(ctx, actor, args.str("topic"), args.str("project"))
		if err != nil {
			return "", true, err
		}
		svc.NoteSpecLookup(ctx, actor, sp)
		return specText(sp), true, nil
	case "goals":
		o, err := svc.ListGoals(ctx, actor, args.str("project"))
		if err != nil {
			return "", true, err
		}
		if b, _ := args["gaps"].(bool); b {
			return goalGapsText(o), true, nil
		}
		return goalsText(o), true, nil
	case "trace":
		q := core.TraceQuery{Ref: args.str("ref"), Project: args.str("project"), Repo: args.str("repo"), Commit: args.str("commit")}
		q.Pull, q.Keys = args.int("pull", 0), args.list("keys")
		tr, err := svc.Trace(ctx, actor, q)
		if err != nil {
			return "", true, err
		}
		return traceText(tr, ""), true, nil
	case "list_design":
		list, err := svc.ListDesign(ctx, actor, core.DesignQuery{Project: args.str("project"), Kind: args.str("kind"), Text: args.str("query"), Status: args.str("status")})
		if err != nil {
			return "", true, err
		}
		return designListText(list), true, nil
	case "save_design":
		in := core.DesignInput{Title: optStr("title"), Kind: optStr("kind"), Body: optStr("body"), Links: args.list("links"), For: args.str("for")}
		if v := int64(args.int("parent", 0)); v != 0 {
			in.Parent = core.Some(v)
		}
		var n *core.DesignNote
		var err error
		if ref := args.str("id"); ref != "" && ref != "0" {
			id, rerr := svc.ResolveDesign(ctx, actor, ref, args.str("project"))
			if rerr != nil {
				return "", true, rerr
			}
			n, err = svc.UpdateDesign(ctx, actor, id, in)
		} else {
			p := args.str("project")
			in.Project = &p
			n, err = svc.CreateDesign(ctx, actor, in)
		}
		if err != nil {
			return "", true, err
		}
		return strings.TrimSpace(fmt.Sprintf("Saved design %s#%d %s. %s", codePrefix(n.Code, n.ProjectKey), n.ID, n.Title, n.Notice)), true, nil
	case "add_to_design":
		id, err := svc.ResolveDesign(ctx, actor, args.str("id"), args.str("project"))
		if err != nil {
			return "", true, err
		}
		n, err := svc.AddDesignAddition(ctx, actor, id, args.str("body"))
		if err != nil {
			return "", true, err
		}
		return strings.TrimSpace(fmt.Sprintf("Added to design %s#%d %s. %s", codePrefix(n.Code, n.ProjectKey), n.ID, n.Title, n.Notice)), true, nil
	case "link":
		l, err := svc.AddItemLink(ctx, actor, core.LinkInput{From: core.LinkEnd{Ref: args.str("from")}, To: core.LinkEnd{Ref: args.str("to")},
			Rel: args.str("rel"), Note: args.str("note")})
		if err != nil {
			return "", true, err
		}
		return "Linked: " + linkLine(l), true, nil
	}
	return "", false, nil
}

// humanDuration is "45s", "12m" or "1h 5m".
func humanDuration(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh %dm", sec/3600, sec%3600/60)
}

func deref(p *string) string {
	if p == nil {
		return "?"
	}
	return *p
}
