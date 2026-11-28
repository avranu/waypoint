package users

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065), and hash passwords cheaply (WP-1874).
func TestMain(m *testing.M) {
	SetPasswordCostForTesting(1, 8)
	os.Exit(testnet.Run(m))
}
