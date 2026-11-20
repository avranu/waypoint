package api

import "testing"

func TestHoldArgsReadsFlagsSentAsStrings(t *testing.T) {
	in := holdArgs(mcpArgs{"reviewed": "true", "person": true})
	if !in.Reviewed || !in.Person {
		t.Errorf("hold = %+v", in)
	}
	if in := holdArgs(mcpArgs{"reviewed": "false", "person": false}); in.Reviewed || in.Person {
		t.Errorf("hold = %+v", in)
	}
}
