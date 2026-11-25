package core

import (
	"strings"
	"testing"
)

// TestMain lowers the argon2 cost for the package's tests; this puts the
// production values back to check what a real hash encodes (WP-1874).
func TestPasswordHashProductionCost(t *testing.T) {
	cheapTime, cheapMem := argonTime, argonMemory
	argonTime, argonMemory = 2, 19*1024
	t.Cleanup(func() { argonTime, argonMemory = cheapTime, cheapMem })

	h := HashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("production hash parameters changed: %s", h)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong horse battery") {
		t.Error("a production-cost hash does not verify")
	}

	// A hash made at the test cost still verifies at the production cost.
	argonTime, argonMemory = cheapTime, cheapMem
	cheap := HashPassword("correct horse battery")
	argonTime, argonMemory = 2, 19*1024
	if !strings.Contains(cheap, "m=8,t=1,") || !CheckPassword(cheap, "correct horse battery") {
		t.Errorf("a hash carries its own cost: %s", cheap)
	}
}
