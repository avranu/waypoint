package core

import "testing"

// The policy functions that read an Actor but belong to their domains, not
// to the type (WP-1665).
func TestActorPolicies(t *testing.T) {
	admin, person := &User{ID: 1, IsAdmin: true}, &User{ID: 2}
	for _, tc := range []struct {
		name                                              string
		a                                                 *Actor
		inBrowser, closesForWork, server, readExp, runExp bool
	}{
		{"admin in the browser", &Actor{User: admin, SessionHash: "h"}, true, false, true, true, true},
		{"person in the browser", &Actor{User: person, SessionHash: "h"}, true, false, false, false, false},
		{"read-only", &Actor{User: person, SessionHash: "h", ReadOnly: true}, false, false, false, false, false},
		{"admin's token", &Actor{User: admin, TokenID: 3}, false, false, true, true, true},
		{"admin's agent", &Actor{User: admin, TokenID: 3, Agent: "x"}, false, false, true, true, false},
		{"permission token without it", &Actor{User: admin, TokenID: 3, Permissions: PermissionSet{}}, false, true, false, true, false},
		{"permission token with it", &Actor{User: admin, TokenID: 3, Permissions: PermissionSet{PermServerRead: true}}, false, true, true, true, false},
		{"runner job", &Actor{User: admin, RunnerJob: 4, Scope: map[int64]int{}}, false, true, false, false, false},
		{"forge sync", &Actor{User: person, ForgeSync: true}, false, true, false, false, false},
		{"automation", &Actor{User: person, Automation: &AutomationRef{ID: 1}}, false, true, false, false, false},
	} {
		if got := inBrowser(tc.a); got != tc.inBrowser {
			t.Errorf("%s: inBrowser = %v", tc.name, got)
		}
		if got := closesForWork(tc.a); got != tc.closesForWork {
			t.Errorf("%s: closesForWork = %v", tc.name, got)
		}
		if err := canReadServer(tc.a); (err == nil) != tc.server {
			t.Errorf("%s: canReadServer = %v", tc.name, err)
		}
		if err := canReadExperiments(tc.a); (err == nil) != tc.readExp {
			t.Errorf("%s: canReadExperiments = %v", tc.name, err)
		}
		if err := canRunExperiments(tc.a); (err == nil) != tc.runExp {
			t.Errorf("%s: canRunExperiments = %v", tc.name, err)
		}
	}
	if closesForWork(nil) {
		t.Error("nobody closes for work")
	}
}
