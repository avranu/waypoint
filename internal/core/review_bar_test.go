package core

import (
	"strings"
	"testing"
)

// The gateway refuses a route, purpose or provider as routing.weakest, and
// a bare model family ("sonnet", "haiku", "opus") is one of its routes:
// with "sonnet" every high-stakes audit failed with a 400 (WP-1134).
func TestTrustedAuditBarIsNoGatewayRoute(t *testing.T) {
	for _, route := range []string{"sonnet", "haiku", "opus", "claude", "anthropic", "audit"} {
		if strings.EqualFold(strings.TrimSpace(trustedAuditBar), route) {
			t.Fatalf("trustedAuditBar %q is a gateway route or provider", trustedAuditBar)
		}
	}
	if !gatewayWeakestRE.MatchString(trustedAuditBar) {
		t.Fatalf("trustedAuditBar %q isn't a name the gateway job takes", trustedAuditBar)
	}
}
