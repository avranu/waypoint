package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The meaning service (WP-1035, page WP-PL17): local models that read how
// two short texts relate, for the goal judgements (goal_meaning.go). An
// embedding model (e5) places texts so near meanings sit close; a natural
// language inference model (DeBERTa-v3 NLI) says how likely one text
// entails another. deploy/meaning runs them beside Waypoint; an admin can
// point WAYPOINT_AI_MEANING_URL at any server with the same API. Nothing
// is sent anywhere else, and without the service the judgements use the
// scorers they still have.

// MeaningModels is the meaning service; swappable in tests.
type MeaningModels interface {
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

// meaningBatch: texts or pairs in one request (the service takes 256).
const meaningBatch = 128

// httpMeaning speaks the meaning service's API.
type httpMeaning struct {
	url, token string
	http       *http.Client
}

func newHTTPMeaning(url, token string, c *http.Client) *httpMeaning {
	return &httpMeaning{url: strings.TrimRight(strings.TrimSpace(url), "/"), token: token, http: c}
}

func (m *httpMeaning) Embed(ctx context.Context, texts []string) (string, [][]float32, error) {
	var model string
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += meaningBatch {
		part := texts[start:min(start+meaningBatch, len(texts))]
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
			if d.Index < 0 || d.Index >= len(part) || len(d.Embedding) == 0 {
				return "", nil, errors.New("the meaning service returned a vector out of place")
			}
			vecs[d.Index] = d.Embedding
		}
		out = append(out, vecs...)
	}
	return model, out, nil
}

func (m *httpMeaning) Entail(ctx context.Context, pairs []NLIPair) (string, []float64, error) {
	var model string
	out := make([]float64, 0, len(pairs))
	for start := 0; start < len(pairs); start += meaningBatch {
		part := pairs[start:min(start+meaningBatch, len(pairs))]
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

func (m *httpMeaning) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	res, err := m.http.Do(req) // #nosec G704 -- the admin's configured service
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the meaning service answered HTTP %d: %s", res.StatusCode, excerpt(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

// meaningTimeout bounds one call to the meaning service: a batch of NLI
// pairs takes a few seconds on a CPU.
const meaningTimeout = 2 * time.Minute
