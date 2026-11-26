package depage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Sources answer how old a version is. HTTP is the real one.
type Sources interface {
	// NPMPublished is when the registry published name@version.
	NPMPublished(ctx context.Context, name, version string) (time.Time, error)
	// GoTime is the module proxy's time for a version: its commit's.
	GoTime(ctx context.Context, path, version string) (time.Time, error)
	// GoRecord is the version's record number in Go's checksum database.
	GoRecord(ctx context.Context, path, version string) (int64, error)
	// GoRecordSince is the record number of a version the proxy first saw
	// at or just after t, and when it saw it.
	GoRecordSince(ctx context.Context, t time.Time) (int64, time.Time, error)
}

// Verdict is what the check made of one version.
type Verdict int

// Verdicts.
const (
	OK Verdict = iota
	TooNew
	Unknown
)

// Result is one new version's verdict.
type Result struct {
	Dep
	Verdict   Verdict
	Published time.Time // zero when Unknown
	Detail    string    // why TooNew or Unknown
	Waiver    *Waiver   // its waiver, if it has one
	// Lookup: Unknown because a source failed to answer, so running again
	// may pass; not because the version has no publish time to read.
	Lookup bool
}

// Passes reports whether the version may land.
func (r Result) Passes() bool { return r.Verdict == OK || r.Waiver != nil }

// Check reads each dep's age and judges it against minAge before now. It
// asks the sources about several deps at once.
func Check(ctx context.Context, src Sources, deps []Dep, waivers map[string]Waiver, now time.Time, minAge time.Duration) []Result {
	cutoff := now.Add(-minAge)
	out := make([]Result, len(deps))

	var goOnce sync.Once
	var goRec int64
	var goRecAt time.Time
	var goRecErr error
	goCutoff := func() (int64, time.Time, error) {
		goOnce.Do(func() { goRec, goRecAt, goRecErr = src.GoRecordSince(ctx, cutoff) })
		return goRec, goRecAt, goRecErr
	}

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, d := range deps {
		out[i] = Result{Dep: d}
		if w, ok := waivers[d.Key()]; ok {
			out[i].Waiver = &w
		}
		wg.Add(1)
		go func(r *Result) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			judge(ctx, src, r, cutoff, minAge, goCutoff)
		}(&out[i])
	}
	wg.Wait()
	return out
}

func judge(ctx context.Context, src Sources, r *Result, cutoff time.Time, minAge time.Duration, goCutoff func() (int64, time.Time, error)) {
	unknown := func(format string, a ...any) {
		r.Verdict, r.Detail = Unknown, fmt.Sprintf(format, a...)
	}
	failed := func(format string, a ...any) {
		unknown(format, a...)
		r.Lookup = true
	}
	if rest, ok := strings.CutPrefix(r.Version, "unverifiable:"); ok {
		unknown("it comes from %s, not a registry, so it has no publish time", rest)
		return
	}
	switch r.Eco {
	case NPM:
		t, err := src.NPMPublished(ctx, r.Name, r.Version)
		if err != nil {
			failed("reading its publish time: %v", err)
			return
		}
		r.Published = t
		if t.After(cutoff) {
			r.Verdict, r.Detail = TooNew, tooNew(t, minAge)
		}
	case Go:
		t, err := src.GoTime(ctx, r.Name, r.Version)
		if err != nil {
			failed("reading its time from the module proxy: %v", err)
			return
		}
		r.Published = t
		if t.After(cutoff) {
			r.Verdict, r.Detail = TooNew, tooNew(t, minAge)
			return
		}
		limit, at, err := goCutoff()
		if err != nil {
			failed("reading Go's checksum database at the cutoff: %v", err)
			return
		}
		n, err := src.GoRecord(ctx, r.Name, r.Version)
		if err != nil {
			failed("reading its record in Go's checksum database: %v", err)
			return
		}
		if n >= limit {
			r.Verdict = TooNew
			r.Detail = fmt.Sprintf("its commit is dated %s, but Go's checksum database first recorded it after %s (record %d, the first at or after the cutoff is %d)", day(r.Published), stamp(at), n, limit)
		}
	default:
		unknown("no publish times for ecosystem %q", r.Eco)
	}
}

// tooNew says when a version was published and when it will be old enough.
func tooNew(t time.Time, minAge time.Duration) string {
	return fmt.Sprintf("published %s, old enough from %s", stamp(t), stamp(t.Add(minAge)))
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }
