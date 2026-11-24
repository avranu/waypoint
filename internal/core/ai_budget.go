package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// What the AI gateway asks Waypoint to wait for (WP-373, WP-412). The
// gateway enforces its own limits; Waypoint keeps no budget of its own and
// does what the gateway says:
//
//   - a daily-limit 429 ("insufficient_quota") is the key's: every caller of
//     the gateway waits until its Retry-After, or the next 00:00 UTC, where
//     each retry used to spend a request more and fail again. A parked key's
//     usage is read again now and then (GET /v1/key), so a raised limit ends
//     the wait early;
//   - any other answer with a Retry-After that the HTTP client doesn't wait
//     out itself (a pool's fair share, every candidate cooling down) parks
//     requests for the same models until then; other models' requests go on,
//     so the screen's free models running dry doesn't stop reviews.
//
// A wait is not a failed try: nothing more was sent, and callers retry after
// it.
const (
	// aiParkedCheck is how often a parked key's usage is read again, in case
	// its limit was raised.
	aiParkedCheck = 10 * time.Minute
	// maxGatewayWait is the longest Retry-After believed: a key's day ends
	// within a day, so a longer one is a fault and counts as no wait.
	maxGatewayWait = 24 * time.Hour
)

// keyUsageLLM is an LLM client that can read a gateway key's limits.
type keyUsageLLM interface {
	// KeyUsage is the key's requests-per-day limit (0 for none) and the
	// requests it has made today (UTC).
	KeyUsage(ctx context.Context, p *AIProvider) (limit, used int, err error)
}

// aiWaitError: the gateway can't be asked until `until`: the key's daily
// limit is spent ("quota"), or the gateway asked this task to wait ("busy").
// Nothing was sent, so callers don't count it as a failed try.
type aiWaitError struct {
	until time.Time
	why   string // surplus: no spare capacity for optional work
}

func (e *aiWaitError) Error() string {
	at := e.until.UTC().Format("15:04 UTC")
	switch e.why {
	case "busy":
		return "the AI gateway asked to wait; it is asked again at " + at
	case "surplus":
		return "the AI gateway has no spare capacity for optional work; it is asked again at " + at
	}
	return "the AI key's daily request limit is used up; it resets at " + at
}

// Unwrap makes it a validation error to the API: the person is told why.
func (e *aiWaitError) Unwrap() error { return &InvalidError{Msg: e.Error()} }

// aiBudget is what the Service knows of the gateway's waits.
type aiBudget struct {
	mu     sync.Mutex
	parked map[int64]aiPark
	busy   map[askPark]busyPark
}

type aiPark struct{ until, checked time.Time }

// askPark is one gateway provider's answer to a request that asked for
// names (a model, then its fallbacks): its Retry-After is theirs, and
// another request's candidates may well be free.
//
// A surplus request's wait holds surplus requests only (WP-906): the gateway
// refusing spare-capacity work says nothing about normal work. names ""
// under prioritySurplus is every surplus request to the provider: a
// refusal is about spare capacity, not about the models asked.
type askPark struct {
	pid      int64
	names    string
	priority string
}

type busyPark struct {
	until time.Time
	task  string
}

// nextUTCMidnight is when the gateway's day resets.
func nextUTCMidnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

// stockQuotaWait is how long a gateway without the key extension rests
// after a quota 429 that named no Retry-After.
const stockQuotaWait = 15 * time.Minute

// dailyQuota: the gateway says the key's daily limit is spent, which a
// retry can't help before the reset (an rpm 429 is "rate_limit").
func (e *llmError) dailyQuota() bool {
	return e.status == 429 && (strings.Contains(e.body, "insufficient_quota") || strings.Contains(e.body, `"code":"quota"`))
}

type aiTaskKey struct{}

// aiContext names the AI task ctx's requests are for.
func aiContext(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, aiTaskKey{}, task)
}

func aiTaskOf(ctx context.Context) string {
	t, _ := ctx.Value(aiTaskKey{}).(string)
	return t
}

// parkAI stops requests to the gateway until `until`.
func (s *Service) parkAI(pid int64, until time.Time) {
	b := &s.aib
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.parked == nil {
		b.parked = map[int64]aiPark{}
	}
	_, was := b.parked[pid]
	b.parked[pid] = aiPark{until: until, checked: s.now()}
	if !was {
		s.log.Warn("the AI key's daily request limit is used up; parking its requests", "until", until.UTC().Format(time.RFC3339))
	}
}

// parkAsk stops requests asking for names at priority until `until`.
func (s *Service) parkAsk(pid int64, names, priority, task string, until time.Time) {
	b := &s.aib
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy == nil {
		b.busy = map[askPark]busyPark{}
	}
	k := askPark{pid, names, priority}
	if until.After(b.busy[k].until) {
		b.busy[k] = busyPark{until: until, task: task}
	}
	s.log.Info("the AI gateway asked to wait", "task", task, "models", names, "priority", priority, "until", until.UTC().Format(time.RFC3339))
}

// noteWait parks what err says to wait for: the gateway's key when it is its
// daily-limit 429, a request for names when it carries a Retry-After the
// HTTP client didn't wait out. It returns the wait that follows from it,
// nil for any other failure.
func (s *Service) noteWait(ctx context.Context, pid int64, names string, err error) *aiWaitError {
	var le *llmError
	if !errors.As(err, &le) {
		return nil
	}
	now := s.now()
	wait := le.retryAfter
	if wait > maxGatewayWait {
		wait = 0
	}
	if le.dailyQuota() {
		until := nextUTCMidnight(now)
		if s.gatewayWith(ctx, GatewayKey) != pid {
			// No GET /key to see the limit raised, and on a stock
			// endpoint insufficient_quota is spent credit, which a top-up
			// ends: look again in a while, not at midnight.
			until = now.Add(stockQuotaWait)
		}
		if wait > 0 {
			until = now.Add(wait)
		}
		s.parkAI(pid, until)
		return &aiWaitError{until: until, why: "quota"}
	}
	if wait <= 0 || (le.status != 429 && le.status < 500) {
		return nil
	}
	until := now.Add(wait)
	s.parkAsk(pid, names, priorityOf(ctx), aiTaskOf(ctx), until)
	return &aiWaitError{until: until, why: "busy"}
}

// gatewayParked is the wait that applies while the gateway's key is out of
// requests, nil when it can be asked. A parked key's usage is read now and
// then, so a raised limit ends the wait early.
func (s *Service) gatewayParked(ctx context.Context, pid int64) *aiWaitError {
	now := s.now()
	b := &s.aib
	b.mu.Lock()
	pk, ok := b.parked[pid]
	if !ok {
		b.mu.Unlock()
		return nil
	}
	if !now.Before(pk.until) {
		delete(b.parked, pid)
		b.mu.Unlock()
		return nil
	}
	recheck := now.Sub(pk.checked) >= aiParkedCheck
	if recheck {
		pk.checked = now
		b.parked[pid] = pk
	}
	b.mu.Unlock()
	if recheck && s.keyHasRoom(ctx, pid) {
		b.mu.Lock()
		delete(b.parked, pid)
		b.mu.Unlock()
		return nil
	}
	return &aiWaitError{until: pk.until, why: "quota"}
}

// askParked is the wait the gateway asked of requests for names at
// priority, nil when there is none. A surplus request also waits while
// every surplus request does (a refusal).
func (s *Service) askParked(pid int64, names, priority string) *aiWaitError {
	now := s.now()
	b := &s.aib
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := []askPark{{pid, names, priority}}
	if priority == prioritySurplus {
		keys = append(keys, askPark{pid, "", prioritySurplus})
	}
	var w *aiWaitError
	for _, k := range keys {
		bp, ok := b.busy[k]
		if !ok {
			continue
		}
		if !now.Before(bp.until) {
			delete(b.busy, k)
			continue
		}
		if w == nil || bp.until.After(w.until) {
			w = &aiWaitError{until: bp.until, why: "busy"}
		}
	}
	return w
}

// normalWaits says whether the gateway has asked any normal request to
// provider pid to wait, or the key's day is spent: free capacity is short
// now, so surplus work isn't sent (WP-906), whatever the gateway would say.
func (s *Service) normalWaits(pid int64) bool {
	now := s.now()
	b := &s.aib
	b.mu.Lock()
	defer b.mu.Unlock()
	if pk, ok := b.parked[pid]; ok && now.Before(pk.until) {
		return true
	}
	for k, bp := range b.busy {
		if k.pid == pid && k.priority == "" && now.Before(bp.until) {
			return true
		}
	}
	return false
}

// aiWait is every wait that applies to a request for names: the key's day,
// then the request's own.
func (s *Service) aiWait(ctx context.Context, pid int64, names string) *aiWaitError {
	if w := s.gatewayParked(ctx, pid); w != nil {
		return w
	}
	return s.askParked(pid, names, priorityOf(ctx))
}

// keyHasRoom reads the key's limit and today's usage from the gateway, and
// says whether it may be asked again: no limit, or some left of it.
func (s *Service) keyHasRoom(ctx context.Context, pid int64) bool {
	kl, ok := s.LLM.(keyUsageLLM)
	if !ok || pid == 0 || s.gatewayWith(ctx, GatewayKey) != pid {
		return false
	}
	p, err := s.aiProvider(ctx, pid)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	limit, used, err := kl.KeyUsage(cctx, p)
	if err != nil {
		s.log.Debug("read the AI key's usage", "err", err)
		return false
	}
	return limit == 0 || used < limit
}

// screenAIWait is the wait the safety screen is under now, by what is
// already known (it sends the gateway nothing): nil when it may go. Only the
// screen's own chain counts: it waits while every provider in it is parked
// (the key's day) or asked the screen to wait, and another provider's or
// task's wait doesn't hold it. A runner entry in the chain never waits. The
// decision model's own wait isn't one: the chat model screens without it.
func (s *Service) screenAIWait(ctx context.Context) *aiWaitError {
	chain, err := s.taskChain(ctx, "task_safety")
	if err != nil || len(chain) == 0 {
		return nil
	}
	pids := map[int64]bool{}
	for _, cm := range chain {
		if cm.CLI != "" {
			return nil
		}
		pids[cm.ProviderID] = true
	}
	now := s.now()
	b := &s.aib
	b.mu.Lock()
	defer b.mu.Unlock()
	var first *aiWaitError
	for pid := range pids {
		w := &aiWaitError{why: "quota"}
		if pk, ok := b.parked[pid]; ok && now.Before(pk.until) {
			w.until = pk.until
		} else {
			w.why = "busy"
			for k, bp := range b.busy {
				if k.pid == pid && k.priority == "" && bp.task == "task_safety" && now.Before(bp.until) && bp.until.After(w.until) {
					w.until = bp.until
				}
			}
		}
		if w.until.IsZero() {
			return nil
		}
		if first == nil || w.until.Before(first.until) {
			first = w
		}
	}
	return first
}

// screenAllowance is how many screens a sweep may start now: none while the
// gateway asks the screen to wait.
func (s *Service) screenAllowance(ctx context.Context) int {
	if s.screenAIWait(ctx) != nil {
		return 0
	}
	return screenSweepBatch
}
