package hermestest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FakeLLM is an OpenAI-compatible chat endpoint with scripted answers, so a
// real Hermes can be driven without a model: the last user message picks
// the script.
//
//   - "run a command": a terminal tool call (echo), then a summary
//   - "ask me": a clarify tool call with choices
//   - "dangerous": a terminal call Hermes asks approval for (rm -rf)
//   - "write a todo": a todo tool call
//   - "slow": a long answer streamed slowly (to test Stop)
//   - anything else: reasoning, then a markdown answer quoting the message
func FakeLLM() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "fake-model", "object": "model", "context_length": 200000},
		}})
	})
	mux.HandleFunc("POST /v1/chat/completions", fakeCompletion)
	return mux
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func fakeCompletion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []chatMsg `json:"messages"`
		Stream   bool      `json:"stream"`
		Tools    []any     `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	lastUser, toolResult := "", ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == "tool" && toolResult == "" && lastUser == "" {
			toolResult = text(m.Content)
		}
		if m.Role == "user" {
			lastUser = text(m.Content)
			break
		}
	}
	low := strings.ToLower(lastUser)
	var reasoning, answer string
	var call map[string]any
	delay := 15 * time.Millisecond
	switch {
	case toolResult != "":
		answer = "Done. The tool said:\n\n```\n" + excerpt(toolResult, 400) + "\n```"
	case strings.Contains(low, "run a command"):
		call = map[string]any{"name": "terminal", "arguments": `{"command":"echo hello from the sandbox"}`}
		reasoning = "The user wants a command run; I'll use the terminal."
	case strings.Contains(low, "dangerous"):
		call = map[string]any{"name": "terminal", "arguments": `{"command":"rm -rf /opt/data/workspace/scratch"}`}
	case strings.Contains(low, "ask me"):
		call = map[string]any{"name": "clarify", "arguments": `{"questions":[{"question":"Which day suits you?","choices":["Monday","Tuesday","Friday"]}]}`}
	case strings.Contains(low, "write a todo"):
		call = map[string]any{"name": "todo", "arguments": `{"todos":[{"id":"1","content":"Draft the email","status":"in_progress"},{"id":"2","content":"Send it","status":"pending"}]}`}
	case strings.Contains(low, "slow"):
		delay = 250 * time.Millisecond
		answer = strings.Repeat("This is a slow answer, one word at a time. ", 30)
	default:
		reasoning = "Thinking about: " + excerpt(lastUser, 80)
		answer = "## Here's what you said\n\n> " + excerpt(lastUser, 300) + "\n\n- a **list** item\n- `code`\n\n```go\nfmt.Println(\"hi\")\n```\n\nAnything else?"
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": answer}
		finish := "stop"
		if call != nil {
			msg["content"] = nil
			msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": call}}
			finish = "tool_calls"
		}
		writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	send := func(delta map[string]any, finish any) {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"role": "assistant"}, nil)
	for _, word := range strings.SplitAfter(reasoning, " ") {
		if word != "" {
			send(map[string]any{"reasoning_content": word}, nil)
			time.Sleep(delay)
		}
	}
	if call != nil {
		send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": call}}}, nil)
		send(map[string]any{}, "tool_calls")
	} else {
		for _, word := range strings.SplitAfter(answer, " ") {
			if r.Context().Err() != nil {
				return
			}
			send(map[string]any{"content": word}, nil)
			time.Sleep(delay)
		}
		send(map[string]any{}, "stop")
	}
	b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "model": "fake-model", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
