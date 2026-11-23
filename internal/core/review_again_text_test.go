package core

import (
	"errors"
	"strings"
	"testing"
)

// The error of a round that can't be decided says what happens next for
// whoever's ship it is: a session's wp ship is asked again by the session,
// Waypoint's own ship is tried again by Waypoint.
func TestAgainReadErrorSaysWhatHappensNext(t *testing.T) {
	cause := errors.Join(errAgainRead, errors.New("database is locked"))
	mine := againReadError("WP-1", false, cause)
	if !strings.Contains(mine.Error(), "run wp ship again") || !errors.Is(mine, errAgainRead) {
		t.Errorf("session's ship: %v", mine)
	}
	ours := againReadError("WP-1", true, cause)
	if strings.Contains(ours.Error(), "run wp ship again") || !strings.Contains(ours.Error(), "Waypoint tries again") || !errors.Is(ours, errAgainRead) {
		t.Errorf("Waypoint's ship: %v", ours)
	}
}
