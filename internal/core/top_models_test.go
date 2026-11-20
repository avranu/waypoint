package core

import (
	"strings"
	"testing"
)

func TestFanoutFamiliesIncludeFableMythos(t *testing.T) {
	r := &fanoutRates{perCall: map[string]float64{}}
	for f, c := range fanoutDefaultPerCall {
		r.perCall[f] = c
	}
	for model, want := range map[string]float64{
		"claude-opus-5-5": 0.10, "claude-sonnet-5-5": 0.06, "haiku": 0.01,
		"claude-fable-5-2": 0.25, "fable": 0.25, "claude-mythos-5-2": 0.25,
		"some-future-model": 0.25,
	} {
		if got := r.perCallFor(model); got != want {
			t.Errorf("perCallFor(%q) = %v, want %v", model, got, want)
		}
	}
	if txt := fanoutDefaultsText(); !strings.Contains(txt, "fable $0.25") || !strings.Contains(txt, "mythos $0.25") || !strings.HasPrefix(txt, "opus $0.10, sonnet $0.06") {
		t.Errorf("defaults text: %q", txt)
	}
	if modelFamily("Claude-Fable-5-2") != "fable" || modelFamily("claude-mythos-5-2") != "mythos" {
		t.Error("modelFamily")
	}
}

func TestUsagePhraseNamedLimits(t *testing.T) {
	for text, want := range map[string]bool{
		"Fable limit reached": true, "Mythos limit hit": true, "Haiku limit reached": true,
		"Opus limit reached": true, "Context limit reached": false,
	} {
		if got := usagePhrase.MatchString(text); got != want {
			t.Errorf("usagePhrase(%q) = %v", text, got)
		}
		if got := limitText(text); got != want {
			t.Errorf("limitText(%q) = %v", text, got)
		}
	}
}

func TestUsageCostKeyFamilies(t *testing.T) {
	for _, c := range []struct {
		model string
		main  bool
		want  string
	}{
		{"claude-fable-5-2", true, "fable"}, {"claude-mythos-5-2", true, "mythos"},
		{"claude-mythos-5-2", false, "mythos/sub"}, {"claude-opus-5-5", false, "opus/sub"},
	} {
		if got := usageCostKey(c.model, c.main); got != c.want {
			t.Errorf("usageCostKey(%q, %v) = %q, want %q", c.model, c.main, got, c.want)
		}
	}
}
