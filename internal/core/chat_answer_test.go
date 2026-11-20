package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An answer is read streamed or not, and a stream's usage-only last chunk
// (no choices) adds nothing to it.
func TestChatAnswerStreamedOrPlain(t *testing.T) {
	for name, body := range map[string]struct{ ctype, text string }{
		"stream": {"text/event-stream", "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"{\\\"a\\\":1}\"}}]}\n\n" +
			"data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"},
		"plain": {"application/json", `{"model":"m","choices":[{"message":{"content":"{\"a\":1}"}}],"usage":{"prompt_tokens":120,"completion_tokens":7}}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", body.ctype)
			_, _ = w.Write([]byte(body.text))
		}))
		c := gatewayClient()
		p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "k"}
		raw, served, err := c.Complete(context.Background(), p, "m", "sys", "prompt", estimateSchema)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(raw) != `{"a":1}` || served != "m" {
			t.Errorf("%s: answer %s from %q", name, raw, served)
		}
		srv.Close()
	}
}
