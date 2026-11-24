// Package meaning is the client for the meaning service (WP-1035, page
// WP-PL17): the embedding and entailment models behind the goal judgements,
// behind a circuit breaker (WP-1602) so a wedged service is one outage, not
// one warning per call.
package meaning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// Without the service, goal judgements use the scorers they still have; texts go only to the configured meaning URL.

// Models is the meaning service; swappable in tests.
type Models interface {
	// Embed returns one unit vector per text, and the model's id@revision.
	Embed(ctx context.Context, texts []string) (string, [][]float32, error)
	// Entail returns, per pair, the probability that the premise entails
	// the hypothesis, and the model's id@revision.
	Entail(ctx context.Context, pairs []NLIPair) (string, []float64, error)
}

// NLIPair is one premise and hypothesis.
type NLIPair struct {
	Premise    string `json:"premise"`
	Hypothesis string `json:"hypothesis"`
}

// Batch: texts or pairs in one request (the service takes 256).
const Batch = 128

var _ Models = (*Client)(nil)

// Client speaks the meaning service's API over HTTP; it is a Models.
type Client struct {
	url, token string
	http       *http.Client
	breaker    *breaker
}

// NewHTTP builds a Client for the service at url (a trailing slash is
// trimmed). Calls go through a circuit breaker that Health reports; now is
// the clock the breaker uses.
func NewHTTP(url, token string, c *http.Client, log *slog.Logger, now func() time.Time) *Client {
	return &Client{url: strings.TrimRight(strings.TrimSpace(url), "/"), token: token, http: c, breaker: newBreaker(log, now)}
}

// Health is the breaker's view of the service (breaker.go).
func (m *Client) Health() Health { return m.breaker.health() }

func (m *Client) Embed(ctx context.Context, texts []string) (string, [][]float32, error) {
	var model string
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += Batch {
		part := texts[start:min(start+Batch, len(texts))]
		var res struct {
			Model string `json:"model"`
			Data  []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := m.post(ctx, "/v1/embeddings", map[string]any{"input": part}, &res); err != nil {
			return "", nil, err
		}
		if len(res.Data) != len(part) || res.Model == "" {
			return "", nil, fmt.Errorf("the meaning service returned %d vectors for %d texts", len(res.Data), len(part))
		}
		if model != "" && res.Model != model {
			return "", nil, errors.New("the meaning service changed its embedding model mid-request")
		}
		model = res.Model
		vecs := make([][]float32, len(part))
		for _, d := range res.Data {
			if d.Index < 0 || d.Index >= len(part) || len(d.Embedding) == 0 || vecs[d.Index] != nil {
				return "", nil, errors.New("the meaning service returned a vector out of place")
			}
			vecs[d.Index] = d.Embedding
		}
		out = append(out, vecs...)
	}
	return model, out, nil
}

func (m *Client) Entail(ctx context.Context, pairs []NLIPair) (string, []float64, error) {
	var model string
	out := make([]float64, 0, len(pairs))
	for start := 0; start < len(pairs); start += Batch {
		part := pairs[start:min(start+Batch, len(pairs))]
		var res struct {
			Model  string `json:"model"`
			Scores []struct {
				Entailment float64 `json:"entailment"`
			} `json:"scores"`
		}
		if err := m.post(ctx, "/v1/nli", map[string]any{"pairs": part}, &res); err != nil {
			return "", nil, err
		}
		if len(res.Scores) != len(part) || res.Model == "" {
			return "", nil, fmt.Errorf("the meaning service returned %d scores for %d pairs", len(res.Scores), len(part))
		}
		if model != "" && res.Model != model {
			return "", nil, errors.New("the meaning service changed its NLI model mid-request")
		}
		model = res.Model
		for _, sc := range res.Scores {
			out = append(out, sc.Entailment)
		}
	}
	return model, out, nil
}

// post sends one request through the breaker (breaker.go): a
// failure to reach the service is ErrDown, and while it is down
// calls fail at once.
func (m *Client) post(ctx context.Context, path string, body, out any) error {
	probe, err := m.breaker.allow()
	if err != nil {
		return err
	}
	judged := false
	defer func() {
		if !judged {
			m.breaker.release(probe)
		}
	}()
	unanswered, err := m.send(ctx, path, body, out)
	if unanswered && errors.Is(ctx.Err(), context.Canceled) {
		// a caller's cancel says nothing about the service; only a deadline counts toward the breaker
		return err
	}
	judged = true
	m.breaker.done(ctx, probe, unanswered, err)
	if unanswered {
		return fmt.Errorf("%w: %w", ErrDown, err)
	}
	return err
}

// send makes the request; unanswered is true when the service did not
// answer it: no connection, a timeout, or a server error (5xx).
func (m *Client) send(ctx context.Context, path string, body, out any) (unanswered bool, err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url+path, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	res, err := m.http.Do(req) // #nosec G704 -- the admin's configured service
	if err != nil {
		return true, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return true, err
	}
	if res.StatusCode != http.StatusOK {
		return res.StatusCode >= 500, fmt.Errorf("the meaning service answered HTTP %d: %s", res.StatusCode, kernel.Excerpt(string(data), 200))
	}
	return false, json.Unmarshal(data, out)
}

// HealthOf is m's view of the service's health, when m keeps one (a
// Client does; a test's fake may not).
func HealthOf(m Models) (Health, bool) {
	h, ok := m.(interface{ Health() Health })
	if !ok {
		return Health{}, false
	}
	return h.Health(), true
}

// Timeout bounds one call to the meaning service: a batch of NLI
// pairs takes a few seconds on a CPU.
const Timeout = 2 * time.Minute
