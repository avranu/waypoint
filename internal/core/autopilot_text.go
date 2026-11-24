package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// DefaultAutopilotPrompt is the prompt until someone words their own. The
// runner fills in {key} (the task), {title} (its title), {done} (the task
// the session finished before, or nothing) and {next} (what wp next
// answered: the task, how to finish it and where to start). The answer
// opens with who assigned the task and ends with what to do (WP-770), so the
// default adds nothing. cmd/wp keeps a copy for servers that send none.
const DefaultAutopilotPrompt = "{next}"

// DefaultAutopilotDone is the note at the Stop that finished a task, until
// someone words their own. {key} is the task; {status} is when the next
// one starts, or what it waits for.
const DefaultAutopilotDone = "Autopilot: {key} is done. {status} Type `keep` to keep it."

const maxAutopilotText = 4000

const (
	autopilotPromptSetting = "autopilot_prompt"
	autopilotDoneSetting   = "autopilot_done"
)

// AutopilotText is autopilot's wording: for every project (Admin → AI), or
// as one project uses it. Prompt and Done are what applies; Inherited* are
// what applies without the project's own (the admins' wording, else the
// default), so a form can show what clearing its field falls back to.
type AutopilotText struct {
	Prompt        string `json:"prompt"`
	Done          string `json:"done"`
	DefaultPrompt string `json:"default_prompt"`
	DefaultDone   string `json:"default_done"`
	// CustomPrompt, CustomDone: worded where asked (the project's own for a
	// project, the admins' for every project).
	CustomPrompt bool `json:"custom_prompt"`
	CustomDone   bool `json:"custom_done"`
	// For a project only: the wording without its own.
	InheritedPrompt string `json:"inherited_prompt,omitempty"`
	InheritedDone   string `json:"inherited_done,omitempty"`
}

// AutopilotTextInput words autopilot for every project; nil leaves a part
// as it is, empty (or the default itself) goes back to the default.
type AutopilotTextInput struct {
	Prompt *string `json:"prompt"`
	Done   *string `json:"done"`
}

func (s *Service) serverSetting(ctx context.Context, q queryer, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// globalAutopilotText is the wording for every project: the admins', else
// the default.
func (s *Service) globalAutopilotText(ctx context.Context, q queryer) (prompt, done string, customPrompt, customDone bool, err error) {
	p, err := s.serverSetting(ctx, q, autopilotPromptSetting)
	if err != nil {
		return "", "", false, false, err
	}
	d, err := s.serverSetting(ctx, q, autopilotDoneSetting)
	if err != nil {
		return "", "", false, false, err
	}
	return cmp(p, DefaultAutopilotPrompt), cmp(d, DefaultAutopilotDone), p != "", d != "", nil
}

// autopilotTextFor is the wording autopilot uses in project pid: the
// project's own, else the admins', else the default. A failure to read
// either falls back to the default: autopilot never stalls on its wording.
func (s *Service) autopilotTextFor(ctx context.Context, q queryer, pid int64) (prompt, done string) {
	prompt, done, _, _, err := s.globalAutopilotText(ctx, q)
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: read its wording", "err", err)
		prompt, done = DefaultAutopilotPrompt, DefaultAutopilotDone
	}
	if pid == 0 {
		return prompt, done
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT settings FROM projects WHERE id = ?`, pid).Scan(&raw); err != nil {
		return prompt, done
	}
	var st ProjectSettings
	if json.Unmarshal([]byte(raw), &st) != nil {
		return prompt, done
	}
	return cmp(strings.TrimSpace(st.AutopilotPrompt), prompt), cmp(strings.TrimSpace(st.AutopilotDone), done)
}

// autopilotDoneNote fills in the Stop note's template.
func autopilotDoneNote(tmpl, key, status string) string {
	r := strings.NewReplacer("{key}", key, "{status}", status)
	return strings.TrimSpace(r.Replace(tmpl))
}

// GetAutopilotText is the wording for every project; any signed-in person
// may read it (it is no secret, and a project's settings show it as what
// the project's own falls back to).
func (s *Service) GetAutopilotText(ctx context.Context, actor *Actor) (*AutopilotText, error) {
	if actor == nil || actor.User == nil {
		return nil, forbidden("sign in to see autopilot's wording")
	}
	p, d, cp, cd, err := s.globalAutopilotText(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	return &AutopilotText{Prompt: p, Done: d, DefaultPrompt: DefaultAutopilotPrompt, DefaultDone: DefaultAutopilotDone, CustomPrompt: cp, CustomDone: cd}, nil
}

// ProjectAutopilotText is the wording autopilot uses in a project, with what
// it falls back to without the project's own.
func (s *Service) ProjectAutopilotText(ctx context.Context, actor *Actor, project string) (*AutopilotText, error) {
	p, err := s.GetProject(ctx, actor, project)
	if err != nil {
		return nil, err
	}
	g, err := s.GetAutopilotText(ctx, actor)
	if err != nil {
		return nil, err
	}
	own, ownDone := strings.TrimSpace(p.Settings.AutopilotPrompt), strings.TrimSpace(p.Settings.AutopilotDone)
	return &AutopilotText{
		Prompt: cmp(own, g.Prompt), Done: cmp(ownDone, g.Done),
		DefaultPrompt: DefaultAutopilotPrompt, DefaultDone: DefaultAutopilotDone,
		CustomPrompt: own != "", CustomDone: ownDone != "",
		InheritedPrompt: g.Prompt, InheritedDone: g.Done,
	}, nil
}

// cleanAutopilotText tidies one part of the wording and checks it.
func cleanAutopilotText(v, what, def string) (string, error) {
	v = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	switch {
	case len(v) > maxAutopilotText:
		return "", invalid("autopilot's %s can be at most %d characters", what, maxAutopilotText)
	case v == def:
		return "", nil
	case what == "prompt" && v != "" && !strings.Contains(v, "{key}") && !strings.Contains(v, "{next}"):
		return "", invalid("autopilot's prompt needs {key} or {next}, or the session isn't told its task")
	}
	return v, nil
}

// SetAutopilotText words autopilot for every project (admins, people only).
func (s *Service) SetAutopilotText(ctx context.Context, actor *Actor, in AutopilotTextInput) (*AutopilotText, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can word autopilot for every project")
	}
	if err := actor.requireHuman("wording autopilot"); err != nil {
		return nil, err
	}
	parts := []struct {
		in        *string
		key, what string
		def       string
	}{
		{in.Prompt, autopilotPromptSetting, "prompt", DefaultAutopilotPrompt},
		{in.Done, autopilotDoneSetting, "note when a task is done", DefaultAutopilotDone},
	}
	vals := make([]string, len(parts))
	for i, p := range parts {
		if p.in == nil {
			continue
		}
		v, err := cleanAutopilotText(*p.in, strings.Fields(p.what)[0], p.def)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	err := s.write(ctx, actor, func(t *tx) error {
		for i, p := range parts {
			if p.in == nil {
				continue
			}
			before, err := s.serverSetting(ctx, t, p.key)
			if err != nil {
				return err
			}
			if vals[i] == before {
				continue
			}
			if vals[i] == "" {
				_, err = t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = ?`, p.key)
			} else {
				_, err = t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
					ON CONFLICT(key) DO UPDATE SET value = excluded.value`, p.key, vals[i])
			}
			if err != nil {
				return err
			}
			detail := "reworded autopilot's " + p.what
			if vals[i] == "" {
				detail = "set autopilot's " + p.what + " back to the default"
			}
			t.audit(AuditEntry{Action: "settings.autopilot_text", Category: AuditSettings, TargetType: "setting",
				Target: "Autopilot wording", URL: "/admin?tab=ai", Detail: detail})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetAutopilotText(ctx, actor)
}

// autopilotDoneTemplate is the Stop note's template in project pid.
func (s *Service) autopilotDoneTemplate(ctx context.Context, q queryer, pid int64) string {
	_, done := s.autopilotTextFor(ctx, q, pid)
	return done
}
