package tracing

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065).
func TestMain(m *testing.M) { os.Exit(testnet.Run(m)) }
