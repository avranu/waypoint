package main

import (
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// gatewayEnvAliases are the names the LLM gateway's settings had before
// WP-1187, read for one release when the new name isn't set. A deploy on
// them had a gateway with every extension: its migrated setup keeps them,
// and a first setup on them declares them all (GatewaySetup.Legacy) unless
// LLM_GATEWAY_EXTENSIONS says otherwise.
var gatewayEnvAliases = map[string]string{
	"LLM_GATEWAY_URL":     "OPENLLMS_BASE_URL",
	"LLM_GATEWAY_API_KEY": "OPENLLMS_API_KEY",
}

// gatewayFromEnv reads the LLM gateway's setup: nil without a key. The
// notices say which old names it read.
func gatewayFromEnv(getenv func(string) string) (*core.GatewaySetup, []string, error) {
	var notices []string
	aliased := false
	get := func(name string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		old := gatewayEnvAliases[name]
		if old == "" {
			return ""
		}
		v := strings.TrimSpace(getenv(old))
		if v != "" {
			aliased = true
			notices = append(notices, fmt.Sprintf("%s is read as %s for this release only: rename it", old, name))
		}
		return v
	}
	key := get("LLM_GATEWAY_API_KEY")
	if key == "" {
		return nil, nil, nil
	}
	g := &core.GatewaySetup{BaseURL: get("LLM_GATEWAY_URL"), APIKey: key, Model: strings.TrimSpace(getenv("LLM_GATEWAY_MODEL")), Legacy: aliased}
	var err error
	if g.Extensions, err = core.ParseGatewayExtensions(getenv("LLM_GATEWAY_EXTENSIONS")); err != nil {
		return nil, notices, fmt.Errorf("LLM_GATEWAY_EXTENSIONS: %w", err)
	}
	return g, notices, nil
}
