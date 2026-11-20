package main

import (
	"reflect"
	"testing"
)

func TestReviewTargets(t *testing.T) {
	for ref, want := range map[string][][2]string{
		"APP-12":       {{"issue", "APP-12"}},
		"close:app-12": {{"close", "APP-12"}},
		"close:app12":  {{"close", "APP12"}},
		"close:12":     nil,
		"comment:123":  {{"comment", "123"}},
		"term:45":      {{"glossary", "45"}},
	} {
		if got := reviewTargets(ref); !reflect.DeepEqual(got, want) {
			t.Errorf("reviewTargets(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestRiskTargets(t *testing.T) {
	c := &cli{}
	for ref, want := range map[string][][2]string{
		"APP-12":       {{"issue", "APP-12"}},
		"comment:123":  {{"comment", "123"}},
		"term:45":      {{"glossary", "45"}},
		"ADR7":         {{"design", "ADR7"}},
		"APP-ADR7":     {{"design", "APP-ADR7"}},
		"design:12":    {{"design", "12"}},
		"#7":           {{"page", "7"}},
		"close:APP-12": nil,
		"pr:x":         nil,
		"prompt:88":    {{"prompt", "88"}},
		"message:#88":  {{"prompt", "88"}},
		"automation:5": {{"automation", "5"}},
		"automation:x": nil,
	} {
		got, err := c.riskTargets(scopeFlags{}, ref)
		if !reflect.DeepEqual(got, want) || (want == nil) != (err != nil) {
			t.Errorf("riskTargets(%q) = %v, %v; want %v", ref, got, err, want)
		}
	}
}
