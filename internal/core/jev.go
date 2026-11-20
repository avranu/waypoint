package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Decisions (WP-298, WP-ADR15). A question with a fixed set of answers goes
// to a decision model, TypeSafe's Jev, instead of a chat model: the gateway
// serves it at /v1/systemone. It answers typed questions about a state with
// calibrated probabilities, in well under a second and for a fraction of a
// chat call, and writes no text, so there is nothing to parse or retry. A
// caller acts on an answer only when it is sure enough, and otherwise asks
// its chat model as before; any failure (no gateway, a key not allowed Jev,
// an error) does the same.

// jevModel is the decision model asked for; the gateway resolves it.
const jevModel = "jev-latest"

// jevTimeout bounds one decision; Jev answers in under a second.
const jevTimeout = 30 * time.Second

// jevQuestion is one typed question: "noul" (yes or no) with instructions,
// or "choice" among criteria (a name for each answer, and what it means).
type jevQuestion struct {
	Type         string             `json:"type"`
	Instructions string             `json:"instructions,omitempty"`
	Criteria     map[string]*string `json:"criteria,omitempty"`
}

// jevAnswer is Jev's answer to one question. Noul is the probability of yes.
type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// deciderLLM is an LLM client that can reach a gateway's decision model.
type deciderLLM interface {
	Decide(ctx context.Context, p *AIProvider, model, state string, questions map[string]jevQuestion) (map[string]jevAnswer, string, error)
}

// errNoDecider: no decision model is set up.
var errNoDecider = errors.New("no decision model")

// Decide asks the gateway's decision model questions about state, and
// returns the answers and the versioned model that gave them.
func (c *httpLLM) Decide(ctx context.Context, p *AIProvider, model, state string, questions map[string]jevQuestion) (map[string]jevAnswer, string, error) {
	var out struct {
		Model   string               `json:"model"`
		Answers map[string]jevAnswer `json:"answers"`
	}
	body := map[string]any{"model": model, "state": state, "questions": questions}
	if err := c.call(ctx, p, http.MethodPost, "/systemone", body, &out); err != nil {
		return nil, "", err
	}
	for q := range questions {
		if _, ok := out.Answers[q]; !ok {
			return nil, "", fmt.Errorf("the decision model left %q unanswered", q)
		}
	}
	if out.Model == "" {
		out.Model = model
	}
	return out.Answers, out.Model, nil
}

// decisionSuffix marks a task's decision-model requests apart from its chat
// requests, in metrics and in the gateway's waits.
const decisionSuffix = "/decision"

// decide asks the gateway's decision model questions about state for task. The
// model is named as runAI names one ("LLM gateway · jev-1.14.0"). errNoDecider
// when there is no LLM gateway.
func (s *Service) decide(ctx context.Context, task, state string, questions map[string]jevQuestion) (map[string]jevAnswer, string, error) {
	d, ok := s.LLM.(deciderLLM)
	id := s.gatewayWith(ctx, GatewayDecide)
	if !ok || id == 0 {
		return nil, "", errNoDecider
	}
	p, err := s.aiProvider(ctx, id)
	if err != nil {
		return nil, "", err
	}
	ctx = aiContext(ctx, task+decisionSuffix) // its waits are its own: another model's than the task's chat
	if w := s.aiWait(ctx, id, jevModel); w != nil {
		return nil, "", w
	}
	cctx, cancel := context.WithTimeout(ctx, jevTimeout)
	defer cancel()
	cctx, usage := withUsageSink(cctx)
	start := time.Now()
	ans, model, err := d.Decide(cctx, p, jevModel, state, questions)
	aiRequests.WithLabelValues(task+decisionSuffix, resultLabel(err)).Inc()
	// The decision model reports no tokens.
	call := aiCall{task: task + decisionSuffix, pid: id, model: model, outcome: "ok", err: err, ms: time.Since(start).Milliseconds(),
		sent: usage.sent.Load(), status: int(usage.status.Load())}
	if call.model == "" {
		call.model = jevModel
	}
	if err != nil {
		call.outcome = classifyModelError(err)
		if ctx.Err() != nil {
			call.outcome = "cancelled"
		}
	}
	s.recordAICall(ctx, call)
	if err != nil {
		if w := s.noteWait(ctx, id, jevModel, err); w != nil {
			return nil, "", w
		}
		return nil, "", err
	}
	s.log.Info("AI decision", "task", task, "model", model, "ms", time.Since(start).Milliseconds())
	return ans, p.Name + " · " + model, nil
}
