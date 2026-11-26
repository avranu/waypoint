package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OTLPEvent is one log record, flattened: attributes of the resource and of
// the record together (the record's win).
type OTLPEvent struct {
	At    int64 // ms
	Name  string
	Attrs map[string]string
}

// OTLPResult says what an export did.
type OTLPResult struct {
	Rejected int64 // events that could not be attributed or are out of range
	Ignored  int64 // understood but not stored: duplicates, transcript sessions, other kinds
}

const (
	otlpMaxEvents = 50000
	otlpSeenKeep  = 14 * 24 * time.Hour
)

// otlpKeep is every attribute the receiver reads. Anything else (above all
// prompt, prompt_text, response, tool_input, tool_parameters' siblings and
// error) is dropped before hashing and mapping.
var otlpKeep = map[string]bool{
	"event.name": true, "event.timestamp": true, "event.sequence": true, "session.id": true,
	"prompt.id": true, "request_id": true, "client_request_id": true, "message.uuid": true, "tool_use_id": true,
	"model": true, "input_tokens": true, "output_tokens": true, "cache_read_tokens": true, "cache_creation_tokens": true,
	"query_source": true, "agent.name": true, "duration_ms": true,
	"tool_name": true, "success": true, "decision": true, "decision_source": true, "source": true,
	"tool_result_size_bytes": true, "tool_source": true, "mcp_server.name": true, "mcp_tool.name": true,
	"skill.name": true, "tool_parameters": true,
	"host.name": true, "vcs.owner.name": true, "vcs.repository.name": true, "vcs.repository.url.full": true,
	"workspace.host_paths": true,
}

func otlpName(e OTLPEvent) string {
	n := e.Name
	if v := e.Attrs["event.name"]; v != "" {
		n = v
	}
	return strings.TrimPrefix(n, "claude_code.")
}

func otlpInt(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

func (e OTLPEvent) hash() string {
	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		if otlpKeep[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(e.At, 10)))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(e.Attrs[k]))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// otlpRepo is the project: owner/name when Claude Code says so (it does only
// with OTEL_METRICS_INCLUDE_REPOSITORY), else the workspace directory's name,
// as the transcript path does when it has nothing better.
func otlpRepo(a map[string]string) string {
	if o, n := a["vcs.owner.name"], a["vcs.repository.name"]; o != "" && n != "" {
		return clip(o+"/"+n, 200)
	}
	if u := a["vcs.repository.url.full"]; u != "" {
		if p, err := url.Parse(u); err == nil && p.Path != "" {
			p2 := strings.TrimSuffix(strings.Trim(p.Path, "/"), ".git")
			if parts := strings.Split(p2, "/"); len(parts) >= 2 {
				return clip(strings.Join(parts[len(parts)-2:], "/"), 200)
			}
		}
	}
	if w := a["workspace.host_paths"]; w != "" {
		first, _, _ := strings.Cut(w, "\n")
		if b := path.Base(strings.ReplaceAll(first, "\\", "/")); b != "" && b != "." && b != "/" {
			return clip(b, 200)
		}
	}
	return ""
}

type otlpToolKey struct {
	kind, tool, det string
}

// IngestOTLP applies Claude Code's exported log events. Like IngestUsage it
// belongs to the authenticated person; resource attributes such as
// user.email are never consulted.
func (s *Service) IngestOTLP(ctx context.Context, actor *Actor, events []OTLPEvent) (*OTLPResult, error) {
	uid, err := usageUser(actor)
	if err != nil {
		return nil, err
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if len(events) > otlpMaxEvents {
		return nil, invalid("at most %d events in one export", otlpMaxEvents)
	}
	now := s.nowMS()
	maxAt := now + usageFutureSkew.Milliseconds()
	res := &OTLPResult{}
	err = s.write(ctx, actor, func(t *tx) error {
		hasTranscript := map[string]bool{}
		touched := map[string]bool{}
		for _, e := range events {
			name := otlpName(e)
			if name != "api_request" && name != "tool_result" && name != "tool_decision" {
				res.Ignored++
				continue
			}
			sess := clip(e.Attrs["session.id"], 80)
			if sess == "" || e.At < usageMinHour || e.At > maxAt {
				res.Rejected++
				continue
			}
			skip, ok := hasTranscript[sess]
			if !ok {
				skip, err = t.sessionHasTranscript(ctx, uid, sess)
				if err != nil {
					return err
				}
				hasTranscript[sess] = skip
			}
			if skip {
				res.Ignored++
				continue
			}
			r, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO otlp_seen(user_id, key, created_at) VALUES (?,?,?)`, uid, e.hash(), now)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				res.Ignored++
				continue
			}
			machine := clip(e.Attrs["host.name"], 100)
			if machine == "" {
				machine = "otel"
			}
			touched[machine] = true
			if err := t.applyOTLP(ctx, uid, machine, sess, name, e, res); err != nil {
				return err
			}
		}
		for m := range touched {
			if err := t.touchMachine(ctx, uid, UsageIngest{Machine: m}, now); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, `DELETE FROM otlp_seen WHERE user_id = ? AND created_at < ?`, uid, now-otlpSeenKeep.Milliseconds())
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (t *tx) sessionHasTranscript(ctx context.Context, uid int64, session string) (bool, error) {
	var one int
	for _, q := range []string{
		`SELECT 1 FROM usage_sessions WHERE user_id = ? AND session = ? LIMIT 1`,
		`SELECT 1 FROM usage_hours WHERE user_id = ? AND session = ? AND via = 'transcript' LIMIT 1`,
		`SELECT 1 FROM tool_hours WHERE user_id = ? AND session = ? AND via = 'transcript' LIMIT 1`,
	} {
		err := t.QueryRowContext(ctx, q, uid, session).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// dropOTLPFor removes the OTLP rows of the sessions a transcript chunk covers.
func (t *tx) dropOTLPFor(ctx context.Context, uid int64, c UsageChunk) error {
	sessions := map[string]bool{}
	for _, m := range c.Messages {
		sessions[clip(m.Session, 80)] = true
	}
	for _, m := range c.Tools {
		sessions[clip(m.Session, 80)] = true
	}
	for _, m := range c.Sessions {
		sessions[clip(m.Session, 80)] = true
	}
	for _, m := range c.Refusals {
		sessions[clip(m.Session, 80)] = true
	}
	for sess := range sessions {
		if sess == "" {
			continue
		}
		for _, tbl := range []string{"usage_hours", "tool_hours", "permission_events"} {
			if _, err := t.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE user_id = ? AND session = ? AND via = 'otlp'`, uid, sess); err != nil { // #nosec G202 -- fixed table names
				return err
			}
		}
	}
	return nil
}

func (t *tx) applyOTLP(ctx context.Context, uid int64, machine, session, name string, e OTLPEvent, res *OTLPResult) error {
	a := e.Attrs
	repo := otlpRepo(a)
	switch name {
	case "api_request":
		model := clip(a["model"], 100)
		if model == "" {
			res.Rejected++
			return nil
		}
		kind := "main"
		switch a["query_source"] {
		case "subagent":
			kind = clip(a["agent.name"], 80)
			if kind == "" {
				kind = "subagent"
			}
		case "auxiliary":
			kind = "auxiliary"
		}
		_, err := t.ExecContext(ctx, `INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, kind, via, messages, input, output, cache_write_5m, cache_write_1h, cache_read)
			VALUES (?,?,?,?,?,?,?,'otlp',1,?,?,?,0,?)
			ON CONFLICT(user_id, hour, machine, session, repo, model, kind, via) DO UPDATE SET
				messages = messages + 1, input = input + excluded.input, output = output + excluded.output,
				cache_write_5m = cache_write_5m + excluded.cache_write_5m, cache_read = cache_read + excluded.cache_read`,
			uid, hourStart(e.At), machine, session, repo, model, kind,
			otlpInt(a["input_tokens"]), otlpInt(a["output_tokens"]), otlpInt(a["cache_creation_tokens"]), otlpInt(a["cache_read_tokens"]))
		return err
	case "tool_result":
		tool := clip(a["tool_name"], 120)
		if tool == "" {
			res.Rejected++
			return nil
		}
		var errs, prompts int64
		if a["success"] == "false" {
			errs = 1
		}
		if ds := a["decision_source"]; ds == "user_permanent" || ds == "user_temporary" {
			prompts = 1
		}
		for _, k := range otlpToolKeys(tool, a) {
			if err := t.addOTLPTool(ctx, uid, e.At, machine, session, repo, k, 1, errs, 0, prompts, otlpInt(a["tool_result_size_bytes"])); err != nil {
				return err
			}
		}
		return nil
	case "tool_decision":
		if a["decision"] != "reject" {
			res.Ignored++
			return nil
		}
		tool := clip(a["tool_name"], 120)
		if tool == "" {
			res.Rejected++
			return nil
		}
		keys := otlpToolKeys(tool, a)
		detail := keys[len(keys)-1].det
		if parts := strings.SplitN(tool, "__", 3); len(parts) == 3 && parts[0] == "mcp" {
			detail = parts[2]
		}
		for _, k := range keys {
			if err := t.addOTLPTool(ctx, uid, e.At, machine, session, repo, k, 0, 0, 1, 0, 0); err != nil {
				return err
			}
		}
		src := "user"
		switch a["source"] {
		case "hook":
			src = "hook"
		case "config":
			src = "rule"
		}
		reason := "rejected: " + clip(a["source"], 40)
		_, err := t.ExecContext(ctx, `INSERT INTO permission_events(user_id, at, machine, session, repo, model, tool, detail, reason, source, excerpt, via)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,'otlp')`,
			uid, e.At, machine, session, repo, "", tool, clip(detail, 200), clip(reason, 200), src, otlpExcerpt(a))
		return err
	}
	return nil
}

// otlpExcerpt is what a refusal shows of the call: the command, only when
// Claude Code was told to export tool details (OTEL_LOG_TOOL_DETAILS).
func otlpExcerpt(a map[string]string) string {
	var p struct {
		Bash string `json:"bash_command"`
		Full string `json:"full_command"`
	}
	if json.Unmarshal([]byte(a["tool_parameters"]), &p) != nil {
		return ""
	}
	if p.Full != "" {
		return clip(p.Full, 300)
	}
	return clip(p.Bash, 300)
}

// otlpToolKeys names the tool_hours rows a call feeds, as classifyCall does
// for transcripts, with what the event says (no tool input unless exported).
func otlpToolKeys(tool string, a map[string]string) []otlpToolKey {
	var p struct {
		Skill    string `json:"skill_name"`
		Subagent string `json:"subagent_type"`
		MCPTool  string `json:"mcp_tool_name"`
	}
	_ = json.Unmarshal([]byte(a["tool_parameters"]), &p)
	switch {
	case strings.HasPrefix(tool, "mcp__"):
		return []otlpToolKey{{kind: "mcp", tool: tool, det: strings.SplitN(tool, "__", 3)[1]}}
	case tool == "Skill":
		k := []otlpToolKey{{kind: "builtin", tool: tool}}
		if p.Skill != "" {
			k = append(k, otlpToolKey{kind: "skill", det: p.Skill})
		}
		return k
	case tool == "Agent" || tool == "Task":
		typ := p.Subagent
		if typ == "" {
			typ = "general-purpose"
		}
		return []otlpToolKey{{kind: "builtin", tool: tool}, {kind: "subagent", det: typ}}
	}
	return []otlpToolKey{{kind: "builtin", tool: tool}}
}

func (t *tx) addOTLPTool(ctx context.Context, uid, at int64, machine, session, repo string, k otlpToolKey, calls, errs, refusals, prompts, bytes int64) error {
	_, err := t.ExecContext(ctx, `INSERT INTO tool_hours(user_id, hour, machine, session, repo, model, kind, tool, detail, via, calls, errors, refusals, prompts, result_bytes)
		VALUES (?,?,?,?,?,'',?,?,?,'otlp',?,?,?,?,?)
		ON CONFLICT(user_id, hour, machine, session, repo, model, kind, tool, detail, via) DO UPDATE SET
			calls = calls + excluded.calls, errors = errors + excluded.errors, refusals = refusals + excluded.refusals,
			prompts = prompts + excluded.prompts, result_bytes = result_bytes + excluded.result_bytes`,
		uid, hourStart(at), machine, session, repo, clip(k.kind, 20), clip(k.tool, 120), clip(k.det, 200), calls, errs, refusals, prompts, bytes)
	return err
}
