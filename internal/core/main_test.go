package core

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065), and hash passwords at the
// cheapest argon2 cost: at production cost that was a quarter of the CPU the
// package's tests used (WP-1874). Hashes carry their own cost, so checking one
// made at either cost works. TestMain keeps the defaults users.go declares in
// prodArgonTime and prodArgonMemory before lowering them, and
// TestPasswordHashProductionCost asserts those are the production values.
var prodArgonTime, prodArgonMemory uint32

func TestMain(m *testing.M) {
	prodArgonTime, prodArgonMemory = argonTime, argonMemory
	argonTime, argonMemory = 1, 8
	os.Exit(testnet.Run(m))
}
