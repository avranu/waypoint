package core

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065), and hash passwords at the
// cheapest argon2 cost: at production cost that was a quarter of the CPU the
// package's tests used (WP-1874). Hashes carry their own cost, so checking one
// made at either cost works. TestPasswordHashProductionCost covers the defaults.
func TestMain(m *testing.M) {
	argonTime, argonMemory = 1, 8
	os.Exit(testnet.Run(m))
}
