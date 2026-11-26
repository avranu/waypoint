package core

import (
	"strings"
	"testing"
)

// TestMain lowers the argon2 cost for the package's tests, keeping the
// defaults it found; this checks they are the production values, then hashes
// at them to check what a real hash encodes (WP-1874).
func TestPasswordHashProductionCost(t *testing.T) {
	if prodArgonTime != 2 || prodArgonMemory != 19*1024 {
		t.Fatalf("argon2 defaults in users.go are time %d memory %d, want 2 and 19*1024", prodArgonTime, prodArgonMemory)
	}
	cheapTime, cheapMem := argonTime, argonMemory
	argonTime, argonMemory = prodArgonTime, prodArgonMemory
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
	argonTime, argonMemory = prodArgonTime, prodArgonMemory
	if !strings.Contains(cheap, "m=8,t=1,") || !CheckPassword(cheap, "correct horse battery") {
		t.Errorf("a hash carries its own cost: %s", cheap)
	}
}
