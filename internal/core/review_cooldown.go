package core

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

// modelCooldown is how long a review leaves out a model of the gateway's
// review route after one of its requests stalled on it (WP-1768). Measured
// after WP-1709: 6 of 15 runs held a 600 s timed-out attempt, took 954-1881 s
// against a 57 s median for the others, and each chunk of each run paid the
// 10 minutes again, since nothing remembered which model had stalled.
const modelCooldown = 30 * time.Minute

// coolKey is one model of one route of one gateway provider.
type coolKey struct {
	pid          int64
	route, model string
}

// coolings are the models a route's asks leave out until a time. It lives
// in memory: a restart asks the model again and, if it still stalls, learns.
type coolings struct {
	mu    sync.Mutex
	until map[coolKey]time.Time
}

// coolModel leaves model out of asks to route until modelCooldown from now;
// another stall re-arms it.
func (s *Service) coolModel(ctx context.Context, pid int64, route, model string) {
	if pid == 0 {
		return
	}
	until := s.now().Add(modelCooldown)
	c := &s.cool
	c.mu.Lock()
	if c.until == nil {
		c.until = map[coolKey]time.Time{}
	}
	c.until[coolKey{pid, route, model}] = until
	c.mu.Unlock()
	s.log.WarnContext(ctx, "a review's model stalled; leaving it out of reviews for a while", "model", model, "route", route, "until", until.UTC().Format(time.RFC3339))
}

// coolingModels are the models route's asks leave out now, expired ones
// forgotten, sorted for a stable request.
func (s *Service) coolingModels(pid int64, route string) (models []string, until time.Time) {
	now := s.now()
	c := &s.cool
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, u := range c.until {
		if !now.Before(u) {
			delete(c.until, k)
			continue
		}
		if k.pid == pid && k.route == route {
			models = append(models, k.model)
			if u.After(until) {
				until = u
			}
		}
	}
	slices.Sort(models)
	return models, until
}

// withCooling is ctx asking the gateway to leave out the models cooling on
// route as well as what ctx already excludes, and the models it added.
func (s *Service) withCooling(ctx context.Context, pid int64, route string) (context.Context, []string) {
	if pid == 0 {
		return ctx, nil
	}
	models, until := s.coolingModels(pid, route)
	if len(models) == 0 {
		return ctx, nil
	}
	s.log.InfoContext(ctx, "asking a review route without the models that stalled", "route", route, "models", models, "until", until.UTC().Format(time.RFC3339))
	return withGatewayExclude(ctx, append(slices.Clone(gatewayExcludeOf(ctx)), models...)), models
}

// asksReviewRoute is whether chain asks the gateway that takes exclude for
// its review route: a chain that names a model of its own (a named reviewer,
// a pinned model) is not held off the models that stalled on the route.
func (s *Service) asksReviewRoute(ctx context.Context, chain []ChainModel) bool {
	id := s.gatewayWith(ctx, GatewayExclude)
	return id != 0 && slices.ContainsFunc(chain, func(c ChainModel) bool {
		return c.CLI == "" && c.ProviderID == id && c.Model == gatewayReviewRoute
	})
}

// coolStalls remembers every model of the gateway pid that a request in
// attempts timed out on, the last or an earlier one (the chain goes on after a
// timeout): only a stall is remembered, not an empty, too long or unreadable
// answer. A model the chain names itself, and one another provider served
// (a family pattern resolved to its id), is not the route's choice.
func (s *Service) coolStalls(ctx context.Context, pid int64, route string, chain []ChainModel, attempts []ModelAttempt) {
	if pid == 0 {
		return
	}
	for _, a := range attempts {
		if a.Outcome != "timeout" || a.Model == "" || a.providerID != pid {
			continue
		}
		if slices.ContainsFunc(chain, func(c ChainModel) bool { return c.Model == a.Model }) {
			continue
		}
		s.coolModel(ctx, pid, route, a.Model)
	}
}

// leftNoModel is whether a request of attempts failed without any model of
// the route having served, as the gateway answers when the asked-for models are
// left out: a 400 ("exclude removes every model", whatever its wording) when
// the exclusions took every model of the route, or no_model_at_bar when they
// left only models below the bar. Any 400 counts, as the attempt does not say
// why; it returns at once, so the worst case is one extra quick request. A 404,
// 502, 504 or a bare 503 (the gateway never answers exhaustion with one) is an
// ordinary failure, not asked again. Any attempt counts: the chain goes on to
// other providers after the gateway's answer.
func leftNoModel(chain []ChainModel, attempts []ModelAttempt) bool {
	return slices.ContainsFunc(attempts, func(a ModelAttempt) bool {
		none := a.Outcome == "no_model_at_bar" || a.status == http.StatusBadRequest
		return none && (a.Model == "" || slices.ContainsFunc(chain, func(c ChainModel) bool { return c.Model == a.Model }))
	})
}

// reaskKey marks a context whose asks are a plain re-ask after a narrowed one
// was left without a model.
type reaskKey struct{}

// asReask is ctx for a plain re-ask that follows leftNoModel. The narrowed
// ask may have parked the names it shares with this one (a no_model_at_bar
// that carries Retry-After), though this ask includes models that one never
// tried; runChain does not stop it for that park. The key's daily-limit
// park and surplus work's waits still apply.
func asReask(ctx context.Context) context.Context { return context.WithValue(ctx, reaskKey{}, true) }

func isReask(ctx context.Context) bool { v, _ := ctx.Value(reaskKey{}).(bool); return v }
