package main

import (
	"regexp"
	"regexp/syntax"
	"testing"
)

// every redaction pattern alternative needs a prefilter hint; derived from the patterns, so a new alternative without one fails this test

// samples are strings that match re, at least one for each alternative, and
// one for each member of a small character class and each side of an
// optional part.
func samples(re *syntax.Regexp) []string {
	pick := func(r *syntax.Regexp) []string {
		var runes []rune
		for i := 0; i+1 < len(r.Rune); i += 2 {
			for c := r.Rune[i]; c <= r.Rune[i+1] && len(runes) <= 40; c++ {
				runes = append(runes, c)
			}
		}
		if len(runes) <= 12 {
			var out []string
			for _, c := range runes {
				out = append(out, string(c))
			}
			return out
		}
		for _, c := range "aA0_-" {
			for i := 0; i+1 < len(r.Rune); i += 2 {
				if c >= r.Rune[i] && c <= r.Rune[i+1] {
					return []string{string(c)}
				}
			}
		}
		return []string{string(runes[0])}
	}
	switch re.Op {
	case syntax.OpLiteral:
		return []string{string(re.Rune)}
	case syntax.OpCharClass:
		return pick(re)
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return []string{"a"}
	case syntax.OpCapture:
		return samples(re.Sub[0])
	case syntax.OpConcat:
		out := []string{""}
		for _, s := range re.Sub {
			var next []string
			for _, a := range out {
				for _, b := range samples(s) {
					next = append(next, a+b)
				}
			}
			out = next
		}
		return out
	case syntax.OpAlternate:
		var out []string
		for _, s := range re.Sub {
			out = append(out, samples(s)...)
		}
		return out
	case syntax.OpQuest:
		return append([]string{""}, samples(re.Sub[0])...)
	case syntax.OpStar:
		return []string{""}
	case syntax.OpPlus, syntax.OpRepeat:
		n := max(re.Min, 1)
		var out []string
		for _, s := range samples(re.Sub[0]) {
			r := ""
			for range n {
				r += s
			}
			out = append(out, r)
		}
		return out
	}
	return []string{""}
}

func TestEverySecretPatternAlternativeHasAPrefilterHint(t *testing.T) {
	for name, expr := range map[string]string{
		"secretRE":      secretRE.String(),
		"tokenLikeRE":   tokenLikeRE.String(),
		"pemKeyRE":      pemKeyRE.String(),
		"urlPasswordRE": urlPasswordRE.String(),
		"digestAuthRE":  digestAuthRE.String(),
		"schemeAuthRE":  schemeAuthRE.String(),
	} {
		parsed, err := syntax.Parse(expr, syntax.Perl)
		if err != nil {
			t.Fatal(name, err)
		}
		re := regexp.MustCompile(expr)
		got := samples(parsed)
		if len(got) < 1 {
			t.Errorf("%s: only %d samples, the generator is not reading the pattern", name, len(got))
		}
		seen := map[string]bool{}
		for _, s := range got {
			if seen[s] {
				continue
			}
			seen[s] = true
			if !re.MatchString(s) {
				t.Errorf("%s: sample %q does not match", name, s)
				continue
			}
			if !mayHoldSecret([]byte(s), nil) {
				t.Errorf("%s: %q matches but the prefilter would skip it: add a hint", name, s)
			}
		}
	}
}
