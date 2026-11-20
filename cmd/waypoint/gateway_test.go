package main

import (
	"slices"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestGatewayFromEnv(t *testing.T) {
	read := func(env map[string]string) (*core.GatewaySetup, []string, error) {
		return gatewayFromEnv(func(k string) string { return env[k] })
	}

	// No key: no gateway, whatever else is set.
	if g, n, err := read(map[string]string{"LLM_GATEWAY_URL": "https://llm.example"}); g != nil || n != nil || err != nil {
		t.Fatalf("no key = %+v %v %v", g, n, err)
	}

	// The new names: extensions only as declared, no notices.
	g, n, err := read(map[string]string{
		"LLM_GATEWAY_URL": " https://llm.example ", "LLM_GATEWAY_API_KEY": "k1", "LLM_GATEWAY_MODEL": "my-model",
		"LLM_GATEWAY_EXTENSIONS": "fallbacks, exclude",
	})
	if err != nil || n != nil || g.BaseURL != "https://llm.example" || g.APIKey != "k1" || g.Model != "my-model" ||
		!slices.Equal(g.Extensions, []string{"fallbacks", "exclude"}) {
		t.Fatalf("new names = %+v %v %v", g, n, err)
	}
	if g, _, _ := read(map[string]string{"LLM_GATEWAY_API_KEY": "k1"}); g == nil || len(g.Extensions) != 0 || g.Legacy {
		t.Fatalf("stock = %+v", g)
	}

	// The old names: read with a notice each. They keep the extensions
	// saved (a migrated deploy has them all), and a first setup on them
	// declares every one, as it did.
	g, n, err = read(map[string]string{"OPENLLMS_BASE_URL": "https://old.example", "OPENLLMS_API_KEY": "k0"})
	if err != nil || g.BaseURL != "https://old.example" || g.APIKey != "k0" || g.Extensions != nil || !g.Legacy {
		t.Fatalf("old names = %+v %v", g, err)
	}
	if len(n) != 2 || !strings.Contains(n[0], "OPENLLMS_API_KEY is read as LLM_GATEWAY_API_KEY") {
		t.Errorf("notices = %q", n)
	}
	// A new name wins over its old one; declared extensions are taken.
	g, n, _ = read(map[string]string{"LLM_GATEWAY_API_KEY": "k1", "OPENLLMS_API_KEY": "k0", "OPENLLMS_BASE_URL": "https://old.example", "LLM_GATEWAY_EXTENSIONS": "key"})
	if g.APIKey != "k1" || g.BaseURL != "https://old.example" || !slices.Equal(g.Extensions, []string{"key"}) || len(n) != 1 {
		t.Errorf("mixed = %+v %q", g, n)
	}

	// An unknown extension is an error naming the setting.
	if _, _, err := read(map[string]string{"LLM_GATEWAY_API_KEY": "k1", "LLM_GATEWAY_EXTENSIONS": "warp"}); err == nil || !strings.Contains(err.Error(), "LLM_GATEWAY_EXTENSIONS") {
		t.Errorf("unknown = %v", err)
	}
}
