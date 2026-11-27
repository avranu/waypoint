package kernel

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Handler reacts to one committed event.
type Handler func(ctx context.Context, actor *Actor, e *Event)

// BatchHandler reacts to all the visible events of one commit at once.
type BatchHandler func(ctx context.Context, actor *Actor, events []*Event)

// Sees is which events a Stage runs its subscribers for.
type Sees int

const (
	// SeesPrivate is the events with Private set, and no others.
	SeesPrivate Sees = iota
	// SeesEvery is every event that is not Private.
	SeesEvery
	// SeesVisible is the events that are not Private and not Merged, Quiet or
	// Draft.
	SeesVisible
)

// Stage is one step of the bus: an ordered list of subscribers run for each
// event it sees.
type Stage struct {
	name string
	sees Sees
	subs []stageSub
}

type stageSub struct {
	kinds []string // nil: every event the stage sees
	h     Handler
}

// Name is the name the stage was created with.
func (s *Stage) Name() string { return s.name }

// On subscribes h to every event the stage sees.
func (s *Stage) On(h Handler) { s.subs = append(s.subs, stageSub{h: h}) }

// OnKind subscribes h to the events of the given kinds the stage sees. No
// kinds panics: On is how to subscribe to every event.
func (s *Stage) OnKind(h Handler, kinds ...string) {
	if len(kinds) == 0 {
		panic(fmt.Sprintf("kernel: stage %q: OnKind with no kinds", s.name))
	}
	s.subs = append(s.subs, stageSub{kinds: slices.Clone(kinds), h: h})
}

// Stage appends a stage to the bus. Stages run in the order they were
// created; a name used twice panics. Subscribe while the owner is being built,
// before any write: the bus is not locked.
func (k *Kernel) Stage(name string, sees Sees) *Stage {
	for _, st := range k.stages {
		if st.name == name {
			panic(fmt.Sprintf("kernel: stage %q created twice", name))
		}
	}
	st := &Stage{name: name, sees: sees}
	k.stages = append(k.stages, st)
	return st
}

// AfterStages appends a batch subscriber. Batch subscribers run in the order
// they were added, after every stage, with the visible events; they are not
// called when there are none. Subscribe while the owner is being built, before
// any write: the bus is not locked.
func (k *Kernel) AfterStages(h BatchHandler) { k.batch = append(k.batch, h) }

// Publish runs events through the bus: each stage in order, events outer and
// subscribers inner, then the batch subscribers. The visible events are
// chosen when the first stage that sees them begins, so what the stages
// before it did to the flags counts, as it did when dispatch was one list.
func (k *Kernel) Publish(ctx context.Context, actor *Actor, events []*Event) {
	// Filtered once, at the first call.
	visible := sync.OnceValue(func() []*Event { return filterEvents(events, isVisible) })
	for _, st := range k.stages {
		var seen []*Event
		switch st.sees {
		case SeesPrivate:
			seen = filterEvents(events, func(e *Event) bool { return e.Private })
		case SeesEvery:
			seen = filterEvents(events, func(e *Event) bool { return !e.Private })
		case SeesVisible:
			seen = visible()
		}
		for _, e := range seen {
			for _, sub := range st.subs {
				if sub.kinds == nil || slices.Contains(sub.kinds, e.Kind) {
					sub.h(ctx, actor, e)
				}
			}
		}
	}
	batched := visible()
	if len(batched) == 0 {
		return
	}
	for _, h := range k.batch {
		h(ctx, actor, batched)
	}
}

// isVisible says whether a SeesVisible stage and the batch subscribers get e.
func isVisible(e *Event) bool { return !e.Private && !e.Merged && !e.Quiet && !e.Draft }

func filterEvents(events []*Event, keep func(*Event) bool) []*Event {
	var out []*Event
	for _, e := range events {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}
