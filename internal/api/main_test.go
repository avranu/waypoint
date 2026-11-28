package api

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core/users"
	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065), and use the cheapest argon2
// cost (production defaults are guarded in internal/core/password_cost_test.go,
// WP-1874).
func TestMain(m *testing.M) {
	users.SetPasswordCostForTesting(1, 8)
	os.Exit(testnet.Run(m))
}
