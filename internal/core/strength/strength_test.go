package strength

import "testing"

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in, effort string
		want       string
	}{
		{"claude-sonnet-5-5", "", "sonnet 5.5"},
		{"anthropic/claude-code sonnet 5.5", "high", "sonnet 5.5 (high)"},
		{"sonnet", "", "sonnet"},
		{"claude/haiku", "", "haiku"},
		{"claude-fable-5-2", "xhigh", "fable 5.2 (xhigh)"},
		{"claude-sonnet-5", "", "sonnet 5.0"},
		{"claude-sonnet-4-20250514", "", "sonnet 4.0"},
		{"claude-opus-5-5[1m]", "", "opus 5.5"},
		{"claude-3-5-sonnet-20241022", "", "sonnet 3.5"},
		{"cursor/grok-4.7-high", "", "cursor/grok-4.7-high"},
		{"claude-haiku-5-5", "bogus", "haiku 5.5"},
		{"", "", "unknown"},
	} {
		if got := Parse(c.in, c.effort).String(); got != c.want {
			t.Errorf("Parse(%q, %q) = %q, want %q", c.in, c.effort, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		name             string
		rejecter, finder Model
		want             Verdict
	}{
		{"haiku can't reject sonnet", Parse("haiku", ""), Parse("claude-sonnet-5-5", ""), Weaker},
		{"low sonnet can't reject high sonnet", Parse("claude-sonnet-5-5", "low"), Parse("claude-sonnet-5-5", "high"), Weaker},
		{"sonnet 5 can't reject sonnet 5.5", Parse("claude-sonnet-5", ""), Parse("claude-sonnet-5-5", ""), Weaker},
		{"same model", Parse("claude-sonnet-5-5", "high"), Parse("claude-sonnet-5-5", "high"), AtLeast},
		{"opus over sonnet", Parse("claude-opus-5-5", "low"), Parse("claude-sonnet-5-5", "max"), AtLeast},
		{"newer sonnet over older at lower effort", Parse("claude-sonnet-5-5", "low"), Parse("claude-sonnet-5", "high"), AtLeast},
		{"family outranks version", Parse("claude-sonnet-9", ""), Parse("claude-opus-4", ""), Weaker},
		{"fable equals mythos", Parse("claude-fable-5-2", ""), Parse("claude-mythos-5", ""), AtLeast},
		{"older mythos can't reject newer fable", Parse("claude-mythos-5", ""), Parse("claude-fable-5-2", ""), Weaker},
		{"dated id keeps its version", Parse("claude-3-opus-20240229", ""), Parse("claude-opus-5-5", ""), Weaker},
		{"unknown effort counts as equal", Parse("claude-sonnet-5-5", ""), Parse("claude-sonnet-5-5", "high"), AtLeast},
		{"unknown finder effort counts as equal", Parse("claude-sonnet-5-5", "low"), Parse("claude-sonnet-5-5", ""), AtLeast},
		{"unknown version counts as equal", Parse("sonnet", "high"), Parse("claude-sonnet-5-5", "high"), AtLeast},
		{"unknown finder version counts as equal", Parse("claude-sonnet-5", "high"), Parse("sonnet", "high"), AtLeast},
		{"xhigh can't reject max", Parse("claude-opus-5-5", "xhigh"), Parse("claude-opus-5-5", "max"), Weaker},
		{"max over xhigh", Parse("claude-opus-5-5", "max"), Parse("claude-opus-5-5", "xhigh"), AtLeast},
		{"high can't reject xhigh", Parse("claude-opus-5-5", "high"), Parse("claude-opus-5-5", "xhigh"), Weaker},
		{"version before effort", Parse("claude-sonnet-5", "max"), Parse("claude-sonnet-5-5", "low"), Weaker},
		{"haiku can't reject a finder of another family", Parse("haiku", ""), Parse("cursor/grok-4.7-high", ""), Weaker},
		{"opus can't reject an unknown finder", Parse("claude-opus-5-5", "max"), Parse("", ""), Weaker},
		{"fable may reject a finder of another family", Parse("claude-fable-5-2", ""), Parse("cursor/grok-4.7-high", ""), AtLeast},
		{"mythos may reject an unknown finder", Parse("claude-mythos-5", ""), Parse("", ""), AtLeast},
		{"rejecter unknown", Parse("", ""), Parse("haiku", ""), Weaker},
		{"rejecter of another family", Parse("deepseek-4.1", ""), Parse("haiku", ""), Weaker},
		{"both unknown families", Parse("", ""), Parse("muse-spark", ""), Weaker},
	} {
		got, reason := Compare(c.rejecter, c.finder)
		if got != c.want {
			t.Errorf("%s: %v (%s), want %v", c.name, got, reason, c.want)
		}
		if reason == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
}

func TestCompareReason(t *testing.T) {
	if _, r := Compare(Parse("", ""), Parse("haiku", "")); r != "Waypoint doesn't know which model this session runs" {
		t.Errorf("reason %q", r)
	}
	if _, r := Compare(Parse("deepseek-4.1", ""), Parse("haiku", "")); r != "deepseek-4.1 isn't a model Waypoint ranks" {
		t.Errorf("reason %q", r)
	}
}
