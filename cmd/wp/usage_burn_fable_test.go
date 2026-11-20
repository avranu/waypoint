package main

import "testing"

func TestBurnPriceForTopModels(t *testing.T) {
	for _, model := range []string{"fable", "claude-fable-5-2", "claude-mythos-5-2", "some-future-model"} {
		if p := burnPriceFor(nil, model); p.Input != 10 || p.Output != 50 || p.CacheWrite5m != 12.5 || p.CacheWrite1h != 20 || p.CacheRead != 1 {
			t.Errorf("burnPriceFor(nil, %q) = %+v, want Fable's price", model, p)
		}
	}
	if p := burnPriceFor(nil, "claude-opus-5-5"); p.Output != 20 {
		t.Errorf("opus fallback: %+v", p)
	}
	if p := burnPriceFor(nil, "claude-sonnet-5-5"); p.Output != 10 {
		t.Errorf("sonnet fallback: %+v", p)
	}
	table := []burnPrice{{Model: "claude-fable-5", Input: 1, Output: 2}, {Model: "claude-fable-5-2", Input: 3, Output: 4}}
	if p := burnPriceFor(table, "claude-fable-5-2-20261101"); p.Input != 3 {
		t.Errorf("a fetched row wins by longest prefix: %+v", p)
	}
}

func TestGuardUnusualModelFable(t *testing.T) {
	usual := []string{"claude-opus-5-5"}
	if !guardUnusualModel("claude-fable-5-2", usual, nil) {
		t.Error("fable over opus-only automation should be unusual")
	}
	if guardUnusualModel("claude-sonnet-5-5", usual, nil) {
		t.Error("a cheaper model is never unusual")
	}
}

// A model of no known price is costed as the dearest, but the guard's
// unusual-model rule needs a known price to call it dearer than the usual ones.
func TestGuardUnusualModelUnknown(t *testing.T) {
	usual := []string{"claude-opus-5-5"}
	if guardUnusualModel("some-future-model", usual, nil) {
		t.Error("a model of unknown price is not known to be dearer than opus")
	}
	table := []burnPrice{{Model: "some-future", Output: 99}}
	if !guardUnusualModel("some-future-model", usual, table) {
		t.Error("a fetched price above the usual models makes it unusual")
	}
}

// The fallback rows agree with the rows the server seeds under the same id
// (migration 0026_usage.sql), so a price does not depend on the fetch.
func TestBurnFallbackMatchesSeed(t *testing.T) {
	if p := burnPriceFor(nil, "claude-fable-5"); p.CacheRead != 1 {
		t.Errorf("claude-fable-5 cache read %v, seeded at 1", p.CacheRead)
	}
}
