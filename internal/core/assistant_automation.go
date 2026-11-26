package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/hermes"
)

const assistantMaxWait = 2 * time.Hour

func init() {
	AutomationTemplates = append(AutomationTemplates,
		AutomationTemplate{
			Name:        "Hand errands to your assistant",
			Description: "An issue created with the tag errand goes to your assistant; its reply comes back as a comment, and the conversation is linked to the issue.",
			Rule: Rule{Trigger: Trigger{Event: "created"}, If: "label:errand", Actions: []Action{
				{Type: "ask_assistant", ID: "errand", Extra: map[string]any{
					"prompt": "Please take care of this errand from Waypoint issue {{.Issue.Key}}, then say in a few lines what you did and what is left.\n\n{{untrusted .Issue.Title}}\n{{untrusted .Issue.Description}}"}},
				{Type: "comment", OnlyIf: "{{.Steps.errand.OK}}", Message: "{{.Steps.errand.Text}}"},
			}},
		},
		AutomationTemplate{
			Name:        "Let your assistant answer a webhook",
			Description: "Anything that can POST JSON (a mail forwarder, a monitor, another agent) reaches your assistant through this automation's webhook address.",
			Rule: Rule{Trigger: Trigger{Event: "webhook"}, Actions: []Action{
				{Type: "ask_assistant", Extra: map[string]any{"wait": false,
					"prompt": "Something arrived at a Waypoint webhook. Decide whether it needs action, act if it does, and tell me only if I need to know.\n\n{{untrusted (json .Trigger.Data)}}"}},
			}},
		})

	registerAction(ActionDef{Name: "ask_assistant", Label: "Ask your assistant", Group: "AI", Phrase: "ask {assistant} {prompt}", Long: true,
		Doc: "Start a conversation with an assistant you own (Hermes, through its gate) and keep its reply in Text. The conversation is linked to the trigger's issue",
		Params: []ParamDef{
			{Name: "prompt", Label: "Message", Type: PTemplate, Required: true, Help: "Wrap outside text in {{untrusted …}} so the assistant reads it as data, not instructions."},
			{Name: "assistant", Label: "Assistant", Type: PText, Placeholder: "your only one", Help: "Its name, when you have several."},
			{Name: "wait", Label: "Wait for the reply", Type: PBool, Default: true},
			{Name: "timeout", Label: "Give up after", Type: PDuration, Default: "15m", Min: f64(30), Max: f64(assistantMaxWait.Seconds()), Advanced: true},
			{Name: "notify", Label: "Tell me when it replies", Type: PBool, Default: false, Advanced: true},
		},
		Outputs: []OutputDef{{Name: "Text", Type: "text", Doc: "the reply (untrusted: the agent wrote it)"}, {Name: "Session", Type: "text", Doc: "the conversation's id"},
			{Name: "URL", Type: "text", Doc: "the conversation in Waypoint"}, {Name: "Status", Type: "text", Doc: "complete, error, interrupted, or sent when not waiting"}},
		Run: askAssistant,
		Describe: func(c *StepCtx) string {
			p, err := c.Prompt("prompt")
			if err != nil {
				return "The message has an error: " + err.Error()
			}
			return "Would ask the assistant: " + excerpt(p, 400)
		}})
}

// automationAssistant is the assistant an automation's step may use: one
// the person who last saved it owns, by name when they have several.
func (s *Service) automationAssistant(ctx context.Context, a *Automation, name string) (*assistantRow, error) {
	owner := automationRunnerOwner(a)
	if owner == 0 {
		return nil, invalid("a person must save this automation: it asks their assistant")
	}
	rows, err := s.assistantRows(ctx, `WHERE a.owner_id = ?`, owner)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	var match []*assistantRow
	for _, r := range rows {
		if name == "" || strings.EqualFold(r.Name, name) {
			match = append(match, r)
		}
	}
	switch {
	case len(match) == 1:
		return match[0], nil
	case len(rows) == 0:
		return nil, invalid("the person who saved this automation has no assistant")
	case name == "":
		return nil, invalid("name the assistant: its owner has %d", len(rows))
	}
	return nil, invalid("no assistant named %q", name)
}

func askAssistant(c *StepCtx) error {
	prompt, err := c.Prompt("prompt")
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return invalid("the message came out empty")
	}
	r, err := c.S.automationAssistant(c.Ctx, c.Automation, c.Str("assistant"))
	if err != nil {
		return err
	}
	if r.State.Frozen {
		return fmt.Errorf("%s is frozen: %w", r.Name, ErrUnavailable)
	}
	client, err := c.S.assistantClient(r)
	if err != nil {
		return err
	}
	gw := client.Gateway()
	sub := gw.Listen()
	defer gw.Unsubscribe(sub)
	key := fmt.Sprintf("waypoint-run-%d-%s-%d", c.Run.ID, c.ID, c.Iteration)
	res, err := gw.Call(c.Ctx, "session.create", map[string]any{"source": "waypoint", "title": excerpt(firstLine(prompt), 80), "idempotency_key": key})
	if err != nil {
		return AssistantError(err)
	}
	var created struct {
		SessionID    string `json:"session_id"`
		Stored       string `json:"stored_session_id"`
		MessageCount int    `json:"message_count"`
	}
	if err := json.Unmarshal(res, &created); err != nil || created.SessionID == "" {
		return fmt.Errorf("the assistant didn't start a conversation")
	}
	if !c.Bool("notify") {
		gw.Quiet(created.SessionID)
	}
	link := fmt.Sprintf("/assistant/%d?s=%s", r.ID, url.QueryEscape(created.Stored))
	c.Set("Session", created.Stored)
	c.Set("URL", c.S.BaseURL()+link)
	if c.Event != nil && c.Event.IssueID != 0 {
		c.S.linkAssistantIssue(c.Ctx, r.ID, created.Stored, c.Event.IssueID, firstLine(prompt), automationRunnerOwner(c.Automation), c.Automation.ID)
	}
	if created.MessageCount == 0 {
		if _, err := gw.Call(c.Ctx, "prompt.submit", map[string]any{"session_id": created.SessionID, "text": prompt}); err != nil {
			return AssistantError(err)
		}
		c.Logf("asked %s", r.Name)
	}
	if c.Param("wait") != nil && !c.Bool("wait") {
		c.Set("Status", "sent")
		return nil
	}
	timeout, err := c.Dur("timeout", 15*time.Minute)
	if err != nil {
		return err
	}
	if created.MessageCount > 0 {
		if text, ok := c.S.lastAssistantReply(c.Ctx, client, created.Stored); ok {
			c.Set("Text", text)
			c.Set("Status", "complete")
			return nil
		}
	}
	text, status, err := waitForReply(c.Ctx, sub, created.SessionID, min(timeout, assistantMaxWait))
	if err != nil {
		return err
	}
	c.Set("Text", text)
	c.Set("Status", status)
	if status == "error" {
		return fmt.Errorf("%s stopped with an error: %s", r.Name, excerpt(text, 300))
	}
	return nil
}

// waitForReply waits for the conversation's turn to end.
func waitForReply(ctx context.Context, sub *hermes.Sub, sessionID string, timeout time.Duration) (string, string, error) {
	deadline := time.After(timeout)
	var streamed strings.Builder
	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-deadline:
			return "", "", fmt.Errorf("no reply within %s", timeout)
		case <-sub.Gone():
			return "", "", fmt.Errorf("lost the assistant's stream")
		case f := <-sub.C:
			var m struct {
				Method string `json:"method"`
				Params struct {
					Type      string          `json:"type"`
					SessionID string          `json:"session_id"`
					Payload   json.RawMessage `json:"payload"`
				} `json:"params"`
			}
			if json.Unmarshal(f.Msg, &m) != nil || m.Method != "event" || m.Params.SessionID != sessionID {
				continue
			}
			switch m.Params.Type {
			case "message.delta":
				var d struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(m.Params.Payload, &d)
				streamed.WriteString(d.Text)
			case "message.complete":
				var p struct {
					Text   any    `json:"text"`
					Status string `json:"status"`
					Error  string `json:"error"`
				}
				_ = json.Unmarshal(m.Params.Payload, &p)
				text, _ := p.Text.(string)
				if text == "" {
					text = streamed.String()
				}
				if text == "" {
					text = p.Error
				}
				if p.Status == "" {
					p.Status = "complete"
				}
				return text, p.Status, nil
			}
		}
	}
}

// lastAssistantReply is a finished conversation's last answer, from its
// stored transcript.
func (s *Service) lastAssistantReply(ctx context.Context, c *hermes.Client, stored string) (string, bool) {
	var page struct {
		Messages []struct {
			Role         string `json:"role"`
			Content      any    `json:"content"`
			FinishReason string `json:"finish_reason"`
		} `json:"messages"`
	}
	if err := c.JSON(ctx, "GET", "/api/sessions/"+url.PathEscape(stored)+"/messages", url.Values{"inline_images": {"false"}}, nil, &page); err != nil {
		return "", false
	}
	for i := len(page.Messages) - 1; i >= 0; i-- {
		m := page.Messages[i]
		if m.Role == "user" {
			return "", false
		}
		if text, ok := m.Content.(string); ok && m.Role == "assistant" && m.FinishReason == "stop" {
			return text, true
		}
	}
	return "", false
}

// linkAssistantIssue ties a conversation to an issue, for Waypoint's own
// steps (no access check: the step already acts for the owner).
func (s *Service) linkAssistantIssue(ctx context.Context, assistantID int64, session string, issueID int64, title string, by, automationID int64) {
	var auto any
	if automationID > 0 {
		auto = automationID
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO assistant_links(assistant_id, session_id, issue_id, automation_id, title, created_by, created_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(assistant_id, session_id, issue_id) DO NOTHING`,
		assistantID, session, issueID, auto, excerpt(title, 200), by, s.nowMS()); err != nil {
		s.log.WarnContext(ctx, "link assistant conversation", "err", err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
