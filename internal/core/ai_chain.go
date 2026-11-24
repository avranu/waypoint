package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ChainModel is one model of a chain: tried in order until one answers
// (WP-D14). Model may be a family pattern such as "opencode/muse-spark-*",
// which means the newest model of that name the provider offers, because
// free model ids change often.
//
// An entry with CLI set is a coding agent on a runner (WP-6): ProviderID is
// 0 and Model, optional, is passed to the CLI. Only the review and reply
// actions may name one (ai_runner.go).
//
// A Paid entry costs money (Haiku through the gateway, WP-243): it is asked in
// a request of its own, after the free entries before it failed, never as
// one of the gateway's fallbacks, which allow six attempts in all and would
// spend them on the free models first.
type ChainModel struct {
	ProviderID int64  `json:"provider_id"`
	Model      string `json:"model"`
	CLI        string `json:"cli,omitempty"`
	Paid       bool   `json:"paid,omitempty"`
}

// ModelAttempt is one model call in a chain, for the run's log.
type ModelAttempt struct {
	Model   string `json:"model"`
	Outcome string `json:"outcome"` // ok, rate_limit, timeout, server, auth, unavailable, no_model_at_bar, bad_output, error
	Error   string `json:"error,omitempty"`
	MS      int64  `json:"ms"`
	// RequestID is the X-Request-ID Waypoint gave a request to the gateway
	// that asked for one (gatewayRouting), for feedback on its answer.
	RequestID string `json:"request_id,omitempty"`
	// Bar is the gateway's X-OpenLLMs-Bar header on the response: the
	// minimum model it held the request to, and what it chose (WP-1440).
	// Next to RequestID, it lets a weaker model's answer be reported.
	Bar string `json:"bar,omitempty"`
}

// usageSink collects the HTTP requests one chain call took (a client retries some: a 429 with Retry-After, a schema
// a provider refused), the last one's status and the bar header of the last
// response that had one; the HTTP client finds it in the context, so the LLM
// interface stays as it is.
type usageSink struct {
	sent, status atomic.Int64
	bar          atomic.Value
}

func (u *usageSink) barSeen() string {
	b, _ := u.bar.Load().(string)
	return b
}

type usageKey struct{}

func withUsageSink(ctx context.Context) (context.Context, *usageSink) {
	u := &usageSink{}
	return context.WithValue(ctx, usageKey{}, u), u
}

func usageSinkOf(ctx context.Context) *usageSink {
	u, _ := ctx.Value(usageKey{}).(*usageSink)
	return u
}

// sentOne counts an HTTP request and keeps its status (0: no response).
func (u *usageSink) sentOne(resp *http.Response) {
	if u == nil {
		return
	}
	u.sent.Add(1)
	status := int64(0)
	if resp != nil {
		status = int64(resp.StatusCode)
		if b := resp.Header.Get(barHeader); b != "" {
			u.bar.Store(excerpt(b, 300))
		}
	}
	u.status.Store(status)
}

// errNoModel: a chain with nothing in it.
var errNoModel = errors.New("no model is set up")

// taskChain is an AI task's model followed by its fallbacks.
func (s *Service) taskChain(ctx context.Context, task string) ([]ChainModel, error) {
	var pid int64
	var model, fallbacks string
	err := s.db.R.QueryRowContext(ctx, `SELECT provider_id, model, fallbacks FROM ai_tasks WHERE task = ?`, task).Scan(&pid, &model, &fallbacks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	chain := []ChainModel{{ProviderID: pid, Model: model}}
	var more []ChainModel
	if err := json.Unmarshal([]byte(fallbacks), &more); err == nil {
		chain = append(chain, more...)
	}
	return chain, nil
}

// classifyModelError names why a model didn't answer. Every outcome but a
// cancelled run moves on to the next model, since the next may be another
// provider entirely.
func classifyModelError(err error) string {
	var le *llmError
	var me *modelError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &le):
		switch {
		case le.noModelAtBar():
			return "no_model_at_bar"
		case le.dailyQuota():
			return "quota"
		case le.status == http.StatusTooManyRequests:
			return "rate_limit"
		case le.status == http.StatusUnauthorized || le.status == http.StatusForbidden:
			return "auth"
		case le.status == http.StatusNotFound:
			return "unavailable"
		case le.status == http.StatusRequestTimeout:
			return "timeout"
		case le.status >= 500:
			return "server"
		}
	case errors.As(err, &me):
		m := strings.ToLower(me.Error())
		switch {
		case strings.Contains(m, "rate limit") || strings.Contains(m, "too many requests") || strings.Contains(m, "quota") || strings.Contains(m, "usage exceeded"):
			return "rate_limit"
		case strings.Contains(m, "not found") || strings.Contains(m, "unknown model") || strings.Contains(m, "no such model"),
			strings.Contains(m, "unavailable"), strings.Contains(m, "overloaded"), strings.Contains(m, "upstream request failed"),
			strings.Contains(m, "capacity"), strings.Contains(m, "bad gateway"):
			return "unavailable"
		}
	}
	var be *badOutputError
	if errors.As(err, &be) {
		return "bad_output"
	}
	return "error"
}

// badOutputError: the model answered, but not in the shape asked for.
type badOutputError struct{ err error }

func (e *badOutputError) Error() string { return "the answer didn't fit: " + e.err.Error() }

// checkAnswer holds an answer to its schema's shape: the types, and the
// required properties, of objects and arrays all the way down. Free models
// drop fields more often than they break JSON. Enums aren't checked: the
// callers map a near miss (4 points, an unknown link kind) to the nearest
// allowed value.
func checkAnswer(schema map[string]any, raw json.RawMessage) error {
	var sch, v any
	b, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &sch); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	return checkValue(sch, v, "the answer")
}

// answerMayOmit names properties a model may leave out of an answer
// although the schema lists them as required: strict structured output
// needs every property in required, but a model on plain JSON mode that
// drops one of these should not lose its whole answer. The caller reads a
// missing one as empty. evidence: the review's quoted line (WP-363); a
// finding without one is judged as unquoted.
var answerMayOmit = map[string]bool{"evidence": true}

// nullable reports whether the object schema lets property name be null: an
// optional property in strict mode is required but nullable, and a model on
// plain JSON mode may leave it out.
func nullable(sch map[string]any, name string) bool {
	props, _ := sch["properties"].(map[string]any)
	ps, _ := props[name].(map[string]any)
	ts, _ := ps["type"].([]any)
	for _, t := range ts {
		if t == "null" {
			return true
		}
	}
	return false
}

func checkValue(schema, v any, at string) error {
	sch, _ := schema.(map[string]any)
	typ, _ := sch["type"].(string)
	ok := true
	switch typ {
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && f == float64(int64(f))
	case "boolean":
		_, ok = v.(bool)
	}
	if !ok {
		return fmt.Errorf("%s should be %s", at, article(typ))
	}
	switch val := v.(type) {
	case map[string]any:
		req, _ := sch["required"].([]any)
		for _, r := range req {
			if name, _ := r.(string); name != "" {
				if _, has := val[name]; !has && !answerMayOmit[name] && !nullable(sch, name) {
					return fmt.Errorf("%s lacks %q", at, name)
				}
			}
		}
		props, _ := sch["properties"].(map[string]any)
		for name, ps := range props {
			if pv, has := val[name]; has && pv != nil {
				if err := checkValue(ps, pv, fmt.Sprintf("%q", name)); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, item := range val {
			if err := checkValue(sch["items"], item, fmt.Sprintf("%s item %d", at, i+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

// runChain asks each model of chain in turn, each within perCall, until one
// gives an answer that decodes into out. It returns the model that answered
// (a family resolved to the model it named) and every attempt.
//
// A run of models on the LLM gateway goes in one request, the rest as
// its fallbacks, within perCall for each: the gateway skips a model that is
// resting or rate limited at once, where a request of its own would wait.
func (s *Service) runChain(ctx context.Context, chain []ChainModel, system, prompt string, schema map[string]any, out any, perCall time.Duration) (ChainModel, []ModelAttempt, error) {
	used, _, attempts, err := s.runChainAt(ctx, chain, system, prompt, schema, out, perCall)
	return used, attempts, err
}

// runChainAt is runChain, and says where in chain the models it has tried
// end: a caller that wants another answer starts there. One request to the
// gateway covers its models up to the one that answered, or all of them
// when the gateway didn't say which (a route answers as one of its own),
// so the attempts don't count the models tried.
func (s *Service) runChainAt(ctx context.Context, chain []ChainModel, system, prompt string, schema map[string]any, out any, perCall time.Duration) (ChainModel, int, []ModelAttempt, error) {
	var none ChainModel
	if len(chain) == 0 {
		return none, 0, nil, errNoModel
	}
	gwID := s.gatewayID(ctx)
	gw, _ := s.LLM.(gatewayLLM)
	var fbID, routeID, exID int64
	if gwID != 0 {
		if gw != nil {
			fbID = s.gatewayWith(ctx, GatewayFallbacks)
		}
		routeID = s.gatewayWith(ctx, GatewayRouting)
		exID = s.gatewayWith(ctx, GatewayExclude)
	}
	routing := gatewayRoutingOf(ctx)
	surplus := routing != nil && routing.priority == prioritySurplus
	var surplusID int64
	if surplus {
		surplusID = s.gatewayWith(ctx, GatewayRoutes, GatewayRouting)
	}
	var attempts []ModelAttempt
	var last error
	var spent *aiWaitError
	for i := 0; i < len(chain); i++ {
		cm := chain[i]
		if err := ctx.Err(); err != nil {
			return none, i, attempts, err
		}
		if surplus && (cm.CLI != "" || cm.Paid || surplusID == 0 || cm.ProviderID != surplusID) {
			continue
		}
		if cm.CLI != "" {
			used, a, err := s.runnerAttempt(ctx, cm, system, prompt, schema, out, perCall)
			attempts = append(attempts, a)
			if err == nil {
				return used, i + 1, attempts, nil
			}
			if ctx.Err() != nil {
				return none, i, attempts, ctx.Err()
			}
			last = err
			s.log.WarnContext(ctx, "runner entry failed; trying the next", "entry", a.Model, "outcome", a.Outcome, "err", err)
			continue
		}
		p, err := s.aiProvider(ctx, cm.ProviderID)
		if err != nil {
			attempts = append(attempts, ModelAttempt{Model: cm.Model, Outcome: "unavailable", Error: "its provider was removed"})
			last = err
			continue
		}
		if p.ID == gwID {
			if w := s.gatewayParked(ctx, p.ID); w != nil {
				attempts = append(attempts, ModelAttempt{Model: cm.Model, Outcome: "quota", Error: w.Error()})
				last, spent = w, w
				for i+1 < len(chain) && chain[i+1].CLI == "" && chain[i+1].ProviderID == p.ID {
					i++
				}
				continue
			}
		}
		model, err := s.resolveModel(ctx, p, cm.Model)
		if err != nil {
			attempts = append(attempts, ModelAttempt{Model: cm.Model, Outcome: "unavailable", Error: aiPublicError(err)})
			last = err
			continue
		}
		var fallbacks []string
		at := []int{i}
		var unresolved []ModelAttempt
		if p.ID == fbID && p.Kind == "openai" && !cm.Paid {
			for ; i+1 < len(chain) && len(fallbacks) < maxGatewayFallbacks; i++ {
				next := chain[i+1]
				if next.CLI != "" || next.ProviderID != p.ID || next.Paid {
					break
				}
				m, err := s.resolveModel(ctx, p, next.Model)
				if err != nil {
					unresolved = append(unresolved, ModelAttempt{Model: next.Model, Outcome: "unavailable", Error: aiPublicError(err)})
					continue
				}
				fallbacks, at = append(fallbacks, m), append(at, i+1)
			}
		}
		asked := append([]string{model}, fallbacks...)
		names := strings.Join(asked, ",")
		if p.ID == gwID {
			if w := s.askParked(p.ID, names, priorityOf(ctx)); w != nil {
				attempts = append(attempts, unresolved...)
				attempts = append(attempts, ModelAttempt{Model: model, Outcome: "rate_limit", Error: w.Error()})
				last, spent = w, w
				continue
			}
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, perCall*time.Duration(min(len(asked), maxCallsPerRequest)))
		cctx, usage := withUsageSink(cctx)
		var requestID string
		if routing != nil && routeID != 0 && p.ID == routeID {
			requestID = routing.newID()
			cctx = withGatewayAsk(cctx, &gatewayAsk{requestID: requestID, paid: routing.paid, priority: routing.priority, weakest: routing.weakest})
		}
		if exID == 0 || p.ID != exID {
			cctx = withoutGatewayExclude(cctx)
		}
		var raw json.RawMessage
		var served string
		if len(fallbacks) > 0 {
			raw, served, err = gw.CompleteWithFallbacks(cctx, p, model, fallbacks, system, prompt, schema)
		} else {
			raw, served, err = s.LLM.Complete(cctx, p, model, system, prompt, schema)
		}
		cancel()
		attempts = append(attempts, unresolved...)
		if err == nil {
			if verr := checkAnswer(schema, raw); verr != nil {
				err = &badOutputError{verr}
			} else if uerr := json.Unmarshal(raw, out); uerr != nil {
				err = &badOutputError{uerr}
			}
		}
		if served != "" {
			model = served
		}
		a := ModelAttempt{Model: model, Outcome: "ok", MS: time.Since(start).Milliseconds(), RequestID: requestID, Bar: usage.barSeen()}
		call := aiCall{task: aiTaskOf(ctx), pid: p.ID, model: model, fallbacks: len(fallbacks), paid: cm.Paid, outcome: "ok", err: err,
			ms: a.MS, sent: usage.sent.Load(), status: int(usage.status.Load()), bar: a.Bar}
		if err != nil {
			if ctx.Err() != nil {
				call.outcome = "cancelled"
				s.recordAICall(ctx, call)
				return none, i, append(attempts, ModelAttempt{Model: model, Outcome: "cancelled", MS: a.MS}), ctx.Err()
			}
			a.Outcome, a.Error = classifyModelError(err), aiPublicError(err)
			refused := surplus && surplusRefusal(err)
			if refused {
				a.Outcome = "refused"
			}
			call.outcome, call.priority = a.Outcome, routing.priorityOrNone()
			s.recordAICall(ctx, call)
			attempts = append(attempts, a)
			last = err
			if refused {
				w := s.noteSurplusRefusal(p.ID, err)
				s.log.Info("the gateway refused surplus work", "task", aiTaskOf(ctx), "status", call.status, "until", w.until.UTC().Format(time.RFC3339))
				return none, len(chain), attempts, w
			}
			if p.ID == gwID {
				if w := s.noteWait(ctx, p.ID, names, err); w != nil {
					last, spent = w, w
				}
			}
			s.log.WarnContext(ctx, "model failed; trying the next", "model", model, "outcome", a.Outcome, "err", err)
			var be *badOutputError
			if k := slices.Index(asked, served); errors.As(err, &be) && k >= 0 && len(fallbacks) > 0 {
				i = at[k]
				continue
			}
			if len(fallbacks) > 0 && p.ID == gwID && (a.Outcome == "quota" || a.Outcome == "rate_limit" || a.Outcome == "server" || a.Outcome == "unavailable") {
				for i+1 < len(chain) && chain[i+1].CLI == "" && chain[i+1].ProviderID == p.ID && !chain[i+1].Paid {
					i++
				}
			}
			if a.Outcome == "no_model_at_bar" && p.ID == gwID {
				for i+1 < len(chain) && chain[i+1].CLI == "" && chain[i+1].ProviderID == p.ID && !chain[i+1].Paid {
					i++
				}
			}
			continue
		}
		call.priority = routing.priorityOrNone()
		s.recordAICall(ctx, call)
		next := i + 1
		if k := slices.Index(asked, served); k >= 0 {
			next = at[k] + 1
		}
		return ChainModel{ProviderID: p.ID, Model: model, Paid: cm.Paid}, next, append(attempts, a), nil
	}
	if spent != nil {
		last = spent
	}
	if last == nil {
		return none, len(chain), attempts, errNoModel
	}
	return none, len(chain), attempts, fmt.Errorf("every model failed; the last: %w", last)
}

// maxCallsPerRequest bounds the time one chain request may take, in
// multiples of the per-call timeout, however many fallbacks it names.
const maxCallsPerRequest = 3

// attemptSummary says in a few words how a pass of models failed, e.g.
// "rate limited (3), server errors (2)". It names outcomes and counts only,
// never a provider's own text. Empty when nothing failed.
func attemptSummary(log []ModelAttempt) string {
	labels := []struct{ outcome, label string }{
		{"quota", "daily limit reached"}, {"rate_limit", "rate limited"}, {"server", "server errors"}, {"unavailable", "unavailable"},
		{"timeout", "timed out"}, {"auth", "refused (auth)"}, {"bad_output", "bad answers"}, {"refused", "no spare capacity"}, {"no_model_at_bar", "no model good enough"}, {"error", "other errors"},
	}
	counts := map[string]int{}
	for _, a := range log {
		counts[a.Outcome]++
	}
	var parts []string
	for _, l := range labels {
		if n := counts[l.outcome]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d)", l.label, n))
		}
	}
	return strings.Join(parts, ", ")
}

// modelLists caches each provider's models briefly, to resolve families.
var modelLists = struct {
	sync.Mutex
	m map[int64]cachedModels
}{m: map[int64]cachedModels{}}

type cachedModels struct {
	at     time.Time
	models []AIModel
}

// providerModels is the provider's model list, cached for ten minutes.
func (s *Service) providerModels(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	modelLists.Lock()
	c, ok := modelLists.m[p.ID]
	modelLists.Unlock()
	//wplint:ignore clock c.at is stored as real time below: a real-time cache
	if ok && time.Since(c.at) <= 10*time.Minute {
		return c.models, nil
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ms, err := s.LLM.Models(lctx, p)
	cancel()
	if err != nil {
		return nil, err
	}
	modelLists.Lock()
	//wplint:ignore clock a process-wide cache whose age providerModels measures with time.Since, in real time
	modelLists.m[p.ID] = cachedModels{at: time.Now(), models: ms}
	modelLists.Unlock()
	return ms, nil
}

// resolveModel turns a family pattern into the newest matching model the
// provider offers, preferring free ones. Plain ids pass through.
func (s *Service) resolveModel(ctx context.Context, p *AIProvider, model string) (string, error) {
	if !strings.ContainsAny(model, "*?[") {
		return model, nil
	}
	ms, err := s.providerModels(ctx, p)
	if err != nil {
		return "", err
	}
	var hits []AIModel
	for _, m := range ms {
		if ok, _ := path.Match(model, m.ID); ok {
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		return "", &modelError{name: "Unavailable", msg: "no model matching " + model + " is offered"}
	}
	slices.SortFunc(hits, func(a, b AIModel) int {
		if a.Free != b.Free {
			if a.Free {
				return -1
			}
			return 1
		}
		return strings.Compare(b.ID, a.ID)
	})
	return hits[0].ID, nil
}

// validChain checks a chain's providers exist and its models are named.
func (s *Service) validChain(ctx context.Context, chain []ChainModel) error {
	for _, cm := range chain {
		if cm.CLI != "" {
			return invalid("a coding agent on a runner can't be used here: it needs a rule with an owner, so only review and reply rules may name one")
		}
		if strings.TrimSpace(cm.Model) == "" {
			return invalid("choose a model for each entry")
		}
		if _, err := path.Match(cm.Model, ""); err != nil {
			return invalid("%q isn't a valid model pattern", cm.Model)
		}
		if _, err := s.aiProvider(ctx, cm.ProviderID); err != nil {
			return invalid("AI provider %d doesn't exist", cm.ProviderID)
		}
	}
	return nil
}

// Free models tried in order when reviewing, newest of each family:
// Muse Spark first (the owner's choice, WP-D14), then the ones that answered
// best in llm-review's measurements. Beam took Space Bunny's place when Zen
// stopped offering it (WP-495); it reviewed best of the free models since.
var defaultReviewModels = []string{
	"opencode/muse-spark-*",
	"opencode/nemotron-*-ultra-free",
	"opencode/beam-free",
	"opencode/mimo-*-flash-free",
}

// EnsureReviewModels gives the review task the default free chain on the
// first opencode provider, once, and the verifier the same chain turned by
// one, so a different model checks the findings. (An install that had a
// review chain got its verifier from migration 0030.) Once review has had a
// chain, from here or an admin, it is the admin's: turned off, it stays
// off.
func (s *Service) EnsureReviewModels(ctx context.Context) error {
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, reviewModelsKey).Scan(&v)
	if err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM ai_tasks WHERE task = 'review'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return s.markReviewModels(ctx)
	}
	var pid int64
	err = s.db.R.QueryRowContext(ctx, `SELECT id FROM ai_providers WHERE kind = 'opencode' ORDER BY id LIMIT 1`).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var fallbacks []ChainModel
	for _, m := range defaultReviewModels[1:] {
		fallbacks = append(fallbacks, ChainModel{ProviderID: pid, Model: m})
	}
	if err := s.SetAITask(ctx, SystemActor(), "review", pid, defaultReviewModels[0], fallbacks); err != nil {
		return err
	}
	turned := slices.Concat(fallbacks[1:], []ChainModel{{ProviderID: pid, Model: defaultReviewModels[0]}})
	return s.SetAITask(ctx, SystemActor(), "verify_review", pid, defaultReviewModels[1], turned)
}

// reviewModelsKey marks the review task as having had a chain, so
// EnsureReviewModels leaves it be.
const reviewModelsKey = "review_models_seeded"

func (s *Service) markReviewModels(ctx context.Context) error {
	_, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO server_settings(key, value) VALUES (?, '1')`, reviewModelsKey)
	return err
}
