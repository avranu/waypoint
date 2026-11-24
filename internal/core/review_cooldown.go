package core

import (
	"context"
	"slices"
	"strings"
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

// stalledModel is failedMidStream's model when the last request timed out:
// only a stall is remembered, not an empty, too long or unreadable answer.
func stalledModel(chain []ChainModel, attempts []ModelAttempt) string {
	m := failedMidStream(chain, attempts)
	if m == "" || attempts[len(attempts)-1].Outcome != "timeout" {
		return ""
	}
	return m
}

// leftNoModel is whether the last request failed without any model of the
// route having served, as the gateway answers when every model is left out:
// no_model_at_bar, or a bare 503 (classified "server" like a 502 or 504, so
// the attempt's text tells them apart). A 404, 502, 504 or other error is an
// ordinary failure, not asked again.
func leftNoModel(chain []ChainModel, attempts []ModelAttempt) bool {
	if len(attempts) == 0 {
		return false
	}
	a := attempts[len(attempts)-1]
	none := a.Outcome == "no_model_at_bar" || (a.Outcome == "server" && strings.HasSuffix(a.Error, "HTTP 503"))
	return none && (a.Model == "" || slices.ContainsFunc(chain, func(c ChainModel) bool { return c.Model == a.Model }))
}
