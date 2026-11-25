package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// ownReviewDescribed: a subagent of another type started to review
// adversarially says so in its description ("Adversarial review of
// WP-9"); adversarial alone ("Adversarial test cases") is other work.
var ownReviewDescribed = lazyre.New(`(?i)\badversarial\b.*\b(review|audit)|\b(review|audit)\b.*\badversarial\b`)

const (
	ownReviewReportBytes = 48 << 10
	ownReviewPromptBytes = 16 << 10
)

// subagentMeta is the .meta.json Claude Code writes beside a subagent's
// transcript.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
}

// ownReview fills a SubagentStop's review fields when the subagent was an
// adversarial review; otherwise it leaves them empty and nothing is kept.
func ownReview(in hookEventInput, ev *sessionEvent, noPrompts bool) {
	transcript := subagentTranscript(in)
	var meta subagentMeta
	if transcript != "" {
		if raw, err := os.ReadFile(strings.TrimSuffix(transcript, ".jsonl") + ".meta.json"); err == nil { // #nosec G304 -- Claude Code's own file for this subagent
			_ = json.Unmarshal(raw, &meta)
		}
	}
	kind := firstNonEmpty(in.AgentType, meta.AgentType)
	if kind != "adversarial-review" && !ownReviewDescribed.MatchString(meta.Description) {
		return
	}
	var t subagentRead
	if transcript != "" {
		t = readSubagent(transcript)
	}
	report := firstNonEmpty(t.handback, in.LastMessage, t.lastText)
	if report == "" {
		return
	}
	ev.AgentType = kind
	ev.AgentModel = t.model
	ev.AgentReport = truncateUTF8(report, ownReviewReportBytes)
	if !noPrompts {
		ev.AgentDescription = meta.Description
		ev.AgentPrompt = truncateUTF8(t.prompt, ownReviewPromptBytes)
	}
}

// subagentTranscript is the subagent's transcript: the path Claude Code
// gives, else where it keeps one beside the session's.
func subagentTranscript(in hookEventInput) string {
	if p := in.AgentTranscript; filepath.IsAbs(p) {
		return p
	}
	if in.AgentID == "" || !filepath.IsAbs(in.TranscriptPath) || strings.ContainsAny(in.AgentID, `/\`) {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(in.TranscriptPath, ".jsonl"), "subagents", "agent-"+in.AgentID+".jsonl")
}

// subagentRead is what a subagent's transcript says: what it was asked,
// which model ran it and what it reported.
type subagentRead struct {
	prompt, model, handback, lastText string
}

// readSubagent reads a subagent's transcript. Its report is the message it
// handed back (SubagentHandback), else its last text; its prompt is the first
// user message that isn't a system reminder.
func readSubagent(path string) subagentRead {
	var out subagentRead
	f, err := os.Open(path) // #nosec G304 -- Claude Code's transcript of this session's subagent
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	rd := bufio.NewReader(f)
	for {
		var line struct {
			Type    string `json:"type"`
			Message struct {
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		raw, rerr := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &line) == nil {
			out = readSubagentLine(out, line.Type, line.Message.Model, line.Message.Content)
		}
		if rerr != nil {
			return out
		}
	}
}

func readSubagentLine(out subagentRead, typ, model string, content json.RawMessage) subagentRead {
	var text string
	var blocks []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name"`
		Input struct {
			Message string `json:"message"`
		} `json:"input"`
	}
	if json.Unmarshal(content, &text) != nil {
		_ = json.Unmarshal(content, &blocks)
	}
	switch typ {
	case "user":
		if out.prompt != "" {
			return out
		}
		if text == "" {
			for _, b := range blocks {
				if b.Type == "text" {
					text = b.Text
					break
				}
			}
		}
		if t := strings.TrimSpace(text); t != "" && !strings.HasPrefix(t, "<system-reminder>") {
			out.prompt = t
		}
	case "assistant":
		if strings.HasPrefix(model, "<") {
			return out
		}
		if model != "" {
			out.model = model
		}
		if t := strings.TrimSpace(text); t != "" {
			out.lastText = t
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_use" && b.Name == "SubagentHandback" && strings.TrimSpace(b.Input.Message) != "":
				out.handback = strings.TrimSpace(b.Input.Message)
			case b.Type == "text" && strings.TrimSpace(b.Text) != "":
				out.lastText = strings.TrimSpace(b.Text)
			}
		}
	}
	return out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
