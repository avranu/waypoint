package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LLM is what Waypoint needs from a language model provider: the models it
// offers, and one structured completion. Swappable in tests.
type LLM interface {
	Models(ctx context.Context, p *AIProvider) ([]AIModel, error)
	// Complete asks model for JSON matching schema and returns it raw, with
	// the model that answered when the provider says ("" means model): a
	// gateway answers a route like "free" with one of many.
	Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error)
}

// AIModel is one model a provider offers.
type AIModel struct {
	ID   string `json:"id"` // what to pass back as the model
	Name string `json:"name"`
	Free bool   `json:"free"`
	// ContextLength is the model's window in tokens, where the provider
	// says (OpenLLMs, OpenRouter); 0 unknown.
	ContextLength int `json:"context_length,omitempty"`
}

type httpLLM struct {
	http *http.Client
	// plainJSON remembers opencode models that can't give structured
	// answers, so they're asked for plain JSON straight away.
	plainJSON sync.Map
}

func (c *httpLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	switch p.Kind {
	case "opencode":
		return c.opencodeModels(ctx, p)
	case "ollama":
		return c.ollamaModels(ctx, p)
	}
	return c.openaiModels(ctx, p)
}

func (c *httpLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	switch p.Kind {
	case "opencode":
		raw, err := c.opencodeComplete(ctx, p, model, system, prompt, schema)
		return raw, "", err
	case "ollama":
		raw, err := c.ollamaComplete(ctx, p, model, system, prompt, schema)
		return raw, "", err
	}
	return c.openaiComplete(ctx, p, model, nil, system, prompt, schema)
}

// CompleteWithFallbacks is Complete for a gateway: fallbacks are the models
// it tries, in the same request, when model can't answer.
func (c *httpLLM) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, fallbacks []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return c.openaiComplete(ctx, p, model, fallbacks, system, prompt, schema)
}

// KeyUsage reads a gateway key's daily request limit and today's requests
// (GET /v1/key, the gateway's key extension), for the budget (ai_budget.go).
func (c *httpLLM) KeyUsage(ctx context.Context, p *AIProvider) (int, int, error) {
	var out struct {
		Policy struct {
			Limits struct {
				RPD int `json:"rpd"`
			} `json:"limits"`
		} `json:"policy"`
		Usage struct {
			DayRequests int `json:"day_requests"`
		} `json:"usage"`
	}
	if err := c.call(ctx, p, http.MethodGet, "/key", nil, &out); err != nil {
		return 0, 0, err
	}
	return out.Policy.Limits.RPD, out.Usage.DayRequests, nil
}

// call does one JSON request against a provider.
func (c *httpLLM) call(ctx context.Context, p *AIProvider, method, path string, body, out any) error {
	resp, err := c.send(ctx, p, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// maxAnswer bounds what Waypoint reads of one provider response.
const maxAnswer = 8 << 20

// llmHeaderTimeout is how long a provider has to start its response. The
// rest is the call's context's to bound (EgressPolicy.streamClient), so
// every call to a provider has a deadline.
const llmHeaderTimeout = 6 * time.Minute

// Waiting out rate limits: a 429 with Retry-After is retried after that
// long, a few times, while the wait fits in the call's deadline; otherwise
// it is the caller's to handle (a chain moves on to its next model).
const (
	maxRateRetries = 3
	maxRetryAfter  = 2 * time.Minute
)

// send does one request against a provider and returns a response with a
// success status, its body unread; any other status is an *llmError.
func (c *httpLLM) send(ctx context.Context, p *AIProvider, method, path string, body any, accept string) (*http.Response, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	for try := 0; ; try++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.BaseURL, "/")+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		if a := gatewayAskOf(ctx); a != nil && a.requestID != "" {
			req.Header.Set("X-Request-ID", a.requestID) // the id a feedback report names (WP-514)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		switch {
		case p.apiKey == "":
		case p.Kind == "opencode":
			req.SetBasicAuth("opencode", p.apiKey) // OPENCODE_SERVER_PASSWORD
		default:
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		resp, err := c.http.Do(req)
		usageSinkOf(ctx).sentOne(resp) // for ai_calls (WP-375)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		le := &llmError{status: resp.StatusCode, body: excerpt(string(eb), 300)}
		wait, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if ok {
			le.retryAfter = wait
		}
		if resp.StatusCode != http.StatusTooManyRequests || !ok || try == maxRateRetries || wait > maxRetryAfter {
			return nil, le
		}
		if d, has := ctx.Deadline(); has && time.Until(d) < wait {
			return nil, le // it would be cancelled before the retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(h); err == nil {
		// Bounded before it becomes a Duration, which a huge one overflows.
		return time.Duration(min(max(n, 0), 366*24*3600)) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// modelError is a failure the model (or its provider) reported in its own
// words, safe to show: "APIError: rate limited", "free tier …".
type modelError struct{ name, msg string }

func (e *modelError) Error() string { return e.name + ": " + e.msg }

type llmError struct {
	status     int
	body       string
	retryAfter time.Duration // the Retry-After of a 429; 0 when it gave none
}

func (e *llmError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

// ---- opencode (`opencode serve`) ----
//
// opencode runs a full coding agent, tools included. Its free models only
// work with the agent's normal tool set, so instead of removing tools the
// session asks permission for everything and Waypoint refuses every
// request: issue text can't make it run a command.

func (c *httpLLM) opencodeModels(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	var res struct {
		All []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Models map[string]struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Cost *struct {
					Input  float64 `json:"input"`
					Output float64 `json:"output"`
				} `json:"cost"`
			} `json:"models"`
		} `json:"all"`
		Connected []string `json:"connected"`
	}
	if err := c.call(ctx, p, http.MethodGet, "/provider", nil, &res); err != nil {
		return nil, err
	}
	var out []AIModel
	for _, prov := range res.All {
		if !slices.Contains(res.Connected, prov.ID) {
			continue
		}
		for id, m := range prov.Models {
			name := m.Name
			if name == "" {
				name = id
			}
			free := m.Cost != nil && m.Cost.Input == 0 && m.Cost.Output == 0
			out = append(out, AIModel{ID: prov.ID + "/" + id, Name: prov.Name + ": " + name, Free: free})
		}
	}
	sortModels(out)
	return out, nil
}

func (c *httpLLM) opencodeComplete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, error) {
	if _, plain := c.plainJSON.Load(model); !plain {
		raw, err := c.opencodeAsk(ctx, p, model, system, prompt, schema, true)
		if !rejectsStructured(err) {
			return raw, err
		}
		// Some models (Muse Spark) refuse the forced tool call opencode's
		// structured output uses; they answer plain JSON well enough.
		c.plainJSON.Store(model, true)
	}
	return c.opencodeAsk(ctx, p, model, system, prompt, schema, false)
}

// rejectsStructured: the model can't give opencode's structured answer.
func rejectsStructured(err error) bool {
	var me *modelError
	if !errors.As(err, &me) {
		return false
	}
	return strings.Contains(me.msg, "tool_choice") || me.name == "StructuredOutputError"
}

// opencodeAsk runs one prompt in a fresh session. structured asks opencode
// for an answer matching schema; otherwise the schema goes in the system
// prompt and the JSON is taken from the reply's text.
func (c *httpLLM) opencodeAsk(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any, structured bool) (json.RawMessage, error) {
	providerID, modelID, ok := strings.Cut(model, "/")
	if !ok {
		return nil, invalid("opencode models look like provider/model")
	}
	var sess struct {
		ID         string `json:"id"`
		Permission []struct {
			Permission, Pattern, Action string
		} `json:"permission"`
	}
	if err := c.call(ctx, p, http.MethodPost, "/session", map[string]any{
		"title":      "Waypoint",
		"permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "ask"}},
	}, &sess); err != nil {
		return nil, err
	}
	// Refuse to run unless the server took the ask-for-everything rule: an
	// opencode that ignores it would let the agent use its tools.
	asks := slices.ContainsFunc(sess.Permission, func(r struct{ Permission, Pattern, Action string }) bool {
		return r.Permission == "*" && r.Pattern == "*" && r.Action == "ask"
	})
	if !asks {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = c.call(cctx, p, http.MethodDelete, "/session/"+sess.ID, nil, nil)
		cancel()
		return nil, &modelError{name: "Unsupported opencode", msg: "this server doesn't accept session permission rules; upgrade opencode"}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = c.call(cctx, p, http.MethodDelete, "/session/"+sess.ID, nil, nil)
	}()
	stop := make(chan struct{})
	defer close(stop)
	go c.opencodeRefuseTools(ctx, p, sess.ID, stop)

	body := map[string]any{
		"model":  map[string]string{"providerID": providerID, "modelID": modelID},
		"system": system,
		"parts":  []map[string]string{{"type": "text", "text": prompt}},
	}
	if structured {
		// Free models sometimes skip the structured answer; opencode can ask again.
		body["format"] = map[string]any{"type": "json_schema", "schema": schema, "retryCount": 2}
	} else {
		sb, _ := json.Marshal(schema)
		body["system"] = system + "\nReply with only a JSON object matching this schema, no prose and no code fence: " + string(sb)
	}
	var res struct {
		Info struct {
			Structured json.RawMessage `json:"structured"`
			Error      *struct {
				Name string `json:"name"`
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := c.call(ctx, p, http.MethodPost, "/session/"+sess.ID+"/message", body, &res); err != nil {
		return nil, err
	}
	if res.Info.Error != nil {
		return nil, &modelError{name: res.Info.Error.Name, msg: res.Info.Error.Data.Message}
	}
	if !structured {
		text := ""
		for _, part := range res.Parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				text = part.Text // the last step's answer
			}
		}
		if text == "" {
			return nil, errors.New("the model gave no answer")
		}
		return extractJSON(text)
	}
	if len(res.Info.Structured) == 0 || string(res.Info.Structured) == "null" {
		return nil, errors.New("the model gave no structured answer")
	}
	return res.Info.Structured, nil
}

// opencodeRefuseTools rejects every permission request from the session
// until stop closes.
func (c *httpLLM) opencodeRefuseTools(ctx context.Context, p *AIProvider, sessionID string, stop <-chan struct{}) {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var pending []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if err := c.call(ctx, p, http.MethodGet, "/permission", nil, &pending); err != nil {
			continue
		}
		for _, r := range pending {
			if r.SessionID != sessionID {
				continue
			}
			_ = c.call(ctx, p, http.MethodPost, "/permission/"+r.ID+"/reply", map[string]string{
				"reply":   "reject",
				"message": "No tools are available. Answer from the text you were given.",
			}, nil)
		}
	}
}

// ---- OpenAI-compatible (OpenLLMs, OpenRouter, LM Studio, xAI, Ollama's /v1, …) ----

func (c *httpLLM) openaiModels(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	var res struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Free    bool   `json:"free"` // where the gateway says
			Context int    `json:"context_length"`
			Pricing *struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"` // OpenRouter
		} `json:"data"`
	}
	if err := c.call(ctx, p, http.MethodGet, "/models", nil, &res); err != nil {
		return nil, err
	}
	out := make([]AIModel, 0, len(res.Data))
	for _, m := range res.Data {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		free := m.Free || strings.HasSuffix(m.ID, ":free") || (m.Pricing != nil && m.Pricing.Prompt == "0" && m.Pricing.Completion == "0")
		out = append(out, AIModel{ID: m.ID, Name: name, Free: free, ContextLength: m.Context})
	}
	sortModels(out)
	return out, nil
}

// openaiComplete streams the answer: free models and a gateway's fallback
// chain can take minutes, and a stream (with the gateway's keep-alives)
// survives proxies that drop a silent connection.
func (c *httpLLM) openaiComplete(ctx context.Context, p *AIProvider, model string, fallbacks []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	// The schema goes in the prompt too: a gateway passes response_format
	// only to upstreams that take it (others get a plain transcript), and
	// they leave out fields they weren't told of.
	sb, _ := json.Marshal(schema)
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system + "\nReply with only a JSON object matching this schema: " + string(sb)},
			{"role": "user", "content": prompt},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "result", "schema": schema, "strict": true},
		},
		"stream":     true,
		"max_tokens": maxTokensOf(ctx), // a runaway stream ends here, not at the client timeout
	}
	if len(fallbacks) > 0 {
		body["fallbacks"] = fallbacks
	}
	if a := gatewayAskOf(ctx); a != nil {
		routing := map[string]any{}
		if a.paid != "" {
			routing["paid"] = a.paid // the gateway's purpose pick (WP-514)
		}
		if a.priority != "" {
			routing["priority"] = a.priority // surplus work (WP-906)
		}
		if len(routing) > 0 {
			body["routing"] = routing
		}
	}
	if ex := gatewayExcludeOf(ctx); len(ex) > 0 {
		body["exclude"] = ex // the gateway leaves these models out (WP-365)
	}
	text, served, err := c.openaiChat(ctx, p, body)
	var le *llmError
	if errors.As(err, &le) && le.status == http.StatusBadRequest {
		// Not every server knows json_schema; plain JSON mode works almost
		// everywhere.
		body["response_format"] = map[string]any{"type": "json_object"}
		text, served, err = c.openaiChat(ctx, p, body)
	}
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(text) == "" {
		return nil, served, errors.New("the model gave no answer")
	}
	raw, err := extractJSON(text)
	return raw, served, err
}

// openaiChat sends one chat completion and returns the answer's text and
// the model that gave it: the gateway's servedModelHeader, else the
// response's own model field. It reads a stream, or a plain response from a
// server that ignores "stream".
func (c *httpLLM) openaiChat(ctx context.Context, p *AIProvider, body map[string]any) (string, string, error) {
	resp, err := c.send(ctx, p, http.MethodPost, "/chat/completions", body, "text/event-stream, application/json")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	served := resp.Header.Get(servedModelHeader)
	type chatError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var res struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&res); err != nil {
			return "", "", err
		}
		if served == "" {
			served = res.Model
		}
		if len(res.Choices) == 0 {
			return "", served, errors.New("the model gave no answer")
		}
		return res.Choices[0].Message.Content, served, nil
	}
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxAnswer)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // keep-alive comments, event names, blank separators
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return text.String(), served, nil
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *chatError `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", served, fmt.Errorf("unreadable stream: %w", err)
		}
		if chunk.Error != nil {
			// Failed after the stream started: no status code to go by.
			name := chunk.Error.Type
			if name == "" {
				name = "error"
			}
			return "", served, &modelError{name: name, msg: excerpt(chunk.Error.Message, 300)}
		}
		if served == "" {
			served = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			if text.Len()+len(chunk.Choices[0].Delta.Content) > maxAnswer {
				return "", served, errors.New("the answer was too long")
			}
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return "", served, err
	}
	return text.String(), served, nil // ended without [DONE]; the JSON check decides
}

// ---- Ollama (its own API: every local model, structured output) ----
//
// Ollama also speaks the OpenAI API under /v1, but its own lists every
// pulled model with its size, takes the JSON schema as `format`, and lets
// Waypoint size the context window: Ollama's default is a few thousand
// tokens and it drops the start of a longer prompt without saying so.

func (c *httpLLM) ollamaModels(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	var res struct {
		Models []struct {
			Name    string `json:"name"`
			Details struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := c.call(ctx, p, http.MethodGet, "/api/tags", nil, &res); err != nil {
		return nil, err
	}
	out := make([]AIModel, 0, len(res.Models))
	for _, m := range res.Models {
		name := m.Name
		if m.Details.ParameterSize != "" {
			name += " (" + m.Details.ParameterSize + ")"
		}
		out = append(out, AIModel{ID: m.Name, Name: name, Free: true}) // it runs on your own hardware
	}
	sortModels(out)
	return out, nil
}

// Ollama's context window, in tokens, sized to the prompt: about three
// characters a token, room to answer, within what local models take.
const (
	ollamaMinCtx = 8192
	ollamaMaxCtx = 65536
	ollamaAnswer = 2048
)

func ollamaContext(chars int) int {
	n := chars/3 + ollamaAnswer
	n = (n + 4095) / 4096 * 4096
	return min(max(n, ollamaMinCtx), ollamaMaxCtx)
}

func (c *httpLLM) ollamaComplete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, error) {
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
		"stream":  false,
		"format":  schema,
		"options": map[string]any{"temperature": 0, "num_ctx": ollamaContext(len(system) + len(prompt))},
	}
	var res struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := c.call(ctx, p, http.MethodPost, "/api/chat", body, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, &modelError{name: "ollama", msg: excerpt(res.Error, 300)}
	}
	if strings.TrimSpace(res.Message.Content) == "" {
		return nil, errors.New("the model gave no answer")
	}
	return extractJSON(res.Message.Content)
}

// extractJSON finds the JSON object in a reply that may wrap it in prose or
// a code fence.
func extractJSON(s string) (json.RawMessage, error) {
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return nil, errors.New("the model's answer wasn't JSON")
	}
	raw := json.RawMessage(s[start : end+1])
	if !json.Valid(raw) {
		return nil, errors.New("the model's answer wasn't valid JSON")
	}
	return raw, nil
}

// sortModels puts free models first, then by name.
func sortModels(ms []AIModel) {
	slices.SortFunc(ms, func(a, b AIModel) int {
		if a.Free != b.Free {
			if a.Free {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}

// defaultMaxTokens caps an answer when its caller named no cap: every call
// here asks for a small JSON object (a title, a verdict, a few findings),
// reasoning included, so 8k is several times what a good answer takes.
const defaultMaxTokens = 8192

type maxTokensKey struct{}

// withMaxTokens sets the cap for gateway calls made with ctx, for answers
// that run longer than the default (review findings, verdicts).
func withMaxTokens(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxTokensKey{}, n)
}

func maxTokensOf(ctx context.Context) int {
	if n, _ := ctx.Value(maxTokensKey{}).(int); n > 0 {
		return n
	}
	return defaultMaxTokens
}
