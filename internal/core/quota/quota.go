// Package quota turns the status line's usage readings into one line per quota
// window.
//
// The readings are not one rising series: the quota can be reset in the middle
// of a window (the use falls, the window's end stays), and a session that has
// been waiting repeats the use it last heard. Everything that reads
// quota_samples for a level, a rise, a rate or an even pace reads it through
// these lines (WP-1864, WP-ADR160).
package quota

import (
	"math"
	"time"
)

const (
	// Week is the length of the seven-day window.
	Week = 7 * 24 * time.Hour
	// Fresh is how old a quota reading the limits still trust.
	Fresh = 6 * time.Hour
)

const (
	// resetDrop: points under the window's level that say the quota was
	// reset, not that a reading is a little behind.
	resetDrop = 10.0
	// resetHold: how long lower readings must go on, with none at the old
	// level between them, to count as a reset when no session saw its own
	// reading fall.
	resetHold = 15 * time.Minute
	// staleWithin: this soon after the last reading, a leap back toward
	// the level before a reset is a reading from before it.
	staleWithin = time.Hour
	// fullAt: at this level nothing more can be used, so the time says
	// nothing about how fast the person uses it.
	fullAt = 99.5
)

// Reading is one stored status line sample.
type Reading struct {
	At, Reset int64
	Pct       float64
	Session   string
}

// Knot is a window's use at a time. From is when the pace it belongs to
// began: the window's start, or the last reset before it. A knot with Zeroed
// set stands for such a reset, not for a reading.
type Knot struct {
	At     int64
	Pct    float64
	From   int64
	Zeroed bool
}

// Line is one window's readings: a line rising from 0 at its start, and
// again from 0 at each reset inside it.
type Line struct {
	Start, Reset int64
	Knots        []Knot
	// SeenAt is the latest reading counted: one a session repeats from
	// before a reset, or one still in doubt, confirms nothing.
	SeenAt int64
}

// Last is the latest knot.
func (l *Line) Last() Knot { return l.Knots[len(l.Knots)-1] }

// Resets are the times the quota was reset inside the window.
func (l *Line) Resets() []int64 {
	var out []int64
	for _, k := range l.Knots {
		if k.Zeroed {
			out = append(out, k.At)
		}
	}
	return out
}

// Used is all the quota the window has used, over every reset.
func (l *Line) Used() float64 {
	total := 0.0
	for i := 1; i < len(l.Knots); i++ {
		total += l.Rise(i)
	}
	return total
}

// Rise is what was used between knot i-1 and knot i. A reset gives none back.
func (l *Line) Rise(i int) float64 {
	if l.Knots[i].Zeroed {
		return 0
	}
	return max(0, l.Knots[i].Pct-l.Knots[i-1].Pct)
}

// FromAt is when the pace at t began.
func (l *Line) FromAt(t int64) int64 {
	for i := len(l.Knots) - 1; i >= 0; i-- {
		if l.Knots[i].At <= t {
			return l.Knots[i].From
		}
	}
	return l.Start
}

// At is the window's use at t: the line between the readings around t, the
// last reading after them.
func (l *Line) At(t int64) float64 {
	k := l.Knots
	for i := 1; i < len(k); i++ {
		if t <= k[i].At {
			span := float64(k[i].At - k[i-1].At)
			if span <= 0 {
				return k[i].Pct
			}
			return k[i-1].Pct + (k[i].Pct-k[i-1].Pct)*float64(t-k[i-1].At)/span
		}
	}
	return l.Last().Pct
}

// Seen is the latest reading taken by t, as the gates had it then.
func (l *Line) Seen(t int64) (Knot, bool) {
	for i := len(l.Knots) - 1; i >= 1; i-- {
		if k := l.Knots[i]; k.At <= t && !k.Zeroed {
			return k, true
		}
	}
	return Knot{}, false
}

// Even is an even pace's share at t of a quota that was whole at from and is
// to last to the window's end.
func (l *Line) Even(from, t int64) float64 {
	if l.Reset <= from {
		return 100
	}
	return max(0, min(100, 100*float64(t-from)/float64(l.Reset-from)))
}

// builder adds readings to a window's line, telling resets from readings
// that are behind.
type builder struct {
	line *Line
	// heard is each session's latest reading and the resets seen when it came.
	heard map[string]report
	// pending are lower readings not yet taken as a reset.
	pending []Reading
	// highAt is the latest reading at the window's level and roseAt when the
	// level last rose; before is the level the last reset ended.
	highAt int64
	roseAt int64
	before float64
	resets int
}

type report struct {
	pct float64
	// since is when the session first reported pct.
	since  int64
	resets int
}

// newBuilder starts the line of the window ending at reset. The window began
// length before that, or at its first reading where that is earlier (the
// clocks that name the two differ).
func newBuilder(reset, first int64, length time.Duration) *builder {
	start := min(first, reset-length.Milliseconds())
	return &builder{
		line:  &Line{Start: start, Reset: reset, Knots: []Knot{{At: start, From: start}}},
		heard: map[string]report{}, highAt: start,
	}
}

func (b *builder) add(r Reading) {
	l := b.line
	prev, known := b.heard[r.Session]
	known = known && r.Session != ""
	repeat := known && prev.pct == r.Pct
	if repeat && prev.resets < b.resets {
		return // what it heard before the reset
	}
	since := r.At
	if repeat {
		since = prev.since
	}
	b.heard[r.Session] = report{r.Pct, since, b.resets}
	level := l.Last().Pct
	if r.Pct <= level-resetDrop {
		switch {
		case known && prev.pct-r.Pct >= resetDrop:
			b.zero(r)
		case repeat && since <= b.roseAt: // heard before the level got there
		case len(b.pending) > 0 && r.At-b.pending[0].At >= resetHold.Milliseconds():
			b.zero(r)
		default:
			b.pending = append(b.pending, r)
		}
		return
	}
	if b.resets > 0 && r.Pct <= b.before && r.Pct-level >= 2*resetDrop && r.At-l.Last().At < staleWithin.Milliseconds() {
		return
	}
	b.pending = nil
	b.highAt = r.At
	b.push(r.At, r.Pct)
}

func (b *builder) push(at int64, pct float64) {
	last := b.line.Last()
	if pct > last.Pct {
		b.roseAt = at
	}
	b.line.SeenAt = max(b.line.SeenAt, at)
	b.line.Knots = append(b.line.Knots, Knot{At: at, Pct: max(last.Pct, pct), From: last.From})
}

// zero starts the line again: the quota was reset after the last reading at
// the old level, and r (after any readings pending) is what was used since.
func (b *builder) zero(r Reading) {
	b.before = b.line.Last().Pct
	b.resets++
	b.line.Knots = append(b.line.Knots, Knot{At: b.highAt, From: b.highAt, Zeroed: true})
	for _, p := range append(b.pending, r) {
		b.push(p.At, p.Pct)
		if p.Session != "" {
			b.heard[p.Session] = report{p.Pct, p.At, b.resets}
		}
	}
	b.pending = nil
	b.highAt = r.At
}

// LinesFrom splits readings, oldest first, into windows. Readings of one
// window can name its end a little apart; one more than an hour later is the
// next window, and a stale one naming an earlier window is left out, unless
// nothing has named the later one in Fresh and the earlier is still to
// come.
func LinesFrom(readings []Reading, length time.Duration) []*Line {
	var lines []*Line
	var b *builder
	hour := time.Hour.Milliseconds()
	for _, r := range readings {
		switch {
		case b != nil && r.Reset < b.line.Reset-hour && (r.Reset <= r.At || r.At-b.line.SeenAt < Fresh.Milliseconds()):
			continue
		case b == nil || r.Reset > b.line.Reset+hour || r.Reset < b.line.Reset-hour:
			b = newBuilder(r.Reset, r.At, length)
			lines = append(lines, b.line)
		}
		b.add(r)
	}
	return lines
}

// Rate is how fast quota was being used as of to, in points a
// millisecond: the use over the lines' time, each moment counting half as much
// as one halfLife later. A rise is spread over the time between its readings,
// and the time after the last reading counts as unused; time at the limit
// counts for neither. span is how long the lines had watched.
func Rate(lines []*Line, to int64, halfLife time.Duration) (rate float64, span time.Duration) {
	if len(lines) == 0 || to <= lines[0].Start {
		return 0, 0
	}
	h := float64(halfLife.Milliseconds())
	weigh := func(t int64) float64 { return math.Exp2(-float64(max(0, to-t)) / h) }
	between := func(a, b int64) float64 { return h / math.Ln2 * (weigh(b) - weigh(a)) }
	used, watched := 0.0, between(lines[0].Start, to)
	for _, l := range lines {
		for i := 1; i < len(l.Knots); i++ {
			a, b := l.Knots[i-1], l.Knots[i]
			if b.Zeroed {
				continue
			}
			from, till := min(a.At, to), min(b.At, to)
			if a.Pct >= fullAt {
				watched -= between(from, till)
				continue
			}
			if rise := l.Rise(i); b.At > a.At {
				used += rise * between(from, till) / float64(b.At-a.At)
			} else {
				used += rise * weigh(b.At)
			}
		}
	}
	span = time.Duration(to-lines[0].Start) * time.Millisecond
	if watched <= 0 {
		return 0, span
	}
	return used / watched, span
}
