package kernel

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestActorNeed(t *testing.T) {
	var none *Actor
	if err := none.Need(PermIssuesEdit); err != nil {
		t.Errorf("no actor: %v", err)
	}
	if err := (&Actor{}).Need(PermIssuesEdit); err != nil {
		t.Errorf("unrestricted: %v", err)
	}
	a := &Actor{Permissions: PermissionSet{PermIssuesEdit: true}}
	if err := a.Need(PermIssuesEdit); err != nil {
		t.Errorf("granted: %v", err)
	}
	err := a.Need(PermAuditsRun)
	var pe *PermissionError
	if !errors.As(err, &pe) || pe.Permission != PermAuditsRun || !errors.Is(err, ErrForbidden) {
		t.Errorf("lacking: %v", err)
	}
}

// Who counts as a person: Person is about the credentials, and
// RequirePersonDirect also about where the request came from.
func TestActorPersonAndPersonDirect(t *testing.T) {
	u := &User{ID: 7}
	for _, tc := range []struct {
		name           string
		a              *Actor
		person, direct bool
	}{
		{"browser", &Actor{User: u, SessionHash: "h"}, true, true},
		{"the system", SystemActor(), false, true},
		{"own token", &Actor{User: u, TokenID: 1}, false, true},
		{"agent", &Actor{User: u, Agent: "x"}, false, false},
		{"mcp", &Actor{User: u, MCP: true}, false, false},
		{"permission token", &Actor{User: u, TokenID: 1, Permissions: PermissionSet{}}, false, false},
		{"automation", &Actor{User: u, Automation: &AutomationRef{ID: 1}}, false, false},
		{"agent's shell", &Actor{User: u, TokenID: 1, AgentEnv: "claude"}, false, false},
		{"agent session", &Actor{User: u, TokenID: 1, Session: 3}, false, false},
	} {
		if got := tc.a.Person(); got != tc.person {
			t.Errorf("%s: Person = %v", tc.name, got)
		}
		err := tc.a.RequirePersonDirect("this")
		if (err == nil) != tc.direct || (err != nil && !errors.Is(err, ErrForbidden)) {
			t.Errorf("%s: RequirePersonDirect = %v", tc.name, err)
		}
		if got := tc.a.ActsAsPerson(); got != tc.direct {
			t.Errorf("%s: ActsAsPerson = %v", tc.name, got)
		}
	}
	var none *Actor
	if none.Person() || none.ActsAsPerson() || (&Actor{}).Person() {
		t.Error("nobody is a person")
	}
}

func TestActorMemberProjects(t *testing.T) {
	a := &Actor{User: &User{ID: 7}}
	for _, tc := range []struct {
		min  int
		role string
	}{
		{RoleViewer, ""},
		{RoleEditor, " AND role IN ('editor','owner')"},
		{RoleOwner, " AND role = 'owner'"},
	} {
		set, args := a.MemberProjects(tc.min)
		if want := "(SELECT project_id FROM project_members WHERE user_id = ?" + tc.role + ")"; set != want || len(args) != 1 || args[0] != int64(7) {
			t.Errorf("role %d: %q %v", tc.min, set, args)
		}
	}
	// A job's scoped token: its own projects with the role, not its user's.
	a.Scope = map[int64]int{5: RoleViewer, 3: RoleEditor, 4: RoleOwner}
	if set, args := a.MemberProjects(RoleEditor); set != "(?,?)" || !slices.Equal(args, []any{int64(3), int64(4)}) {
		t.Errorf("scoped: %q %v", set, args)
	}
	a.Scope = map[int64]int{}
	if set, args := a.MemberProjects(RoleViewer); set != "(NULL)" || len(args) != 0 {
		t.Errorf("empty scope: %q %v", set, args)
	}
}

func TestActorIsAdmin(t *testing.T) {
	admin := &User{ID: 1, IsAdmin: true}
	var none *Actor
	if none.IsAdmin() || (&Actor{}).IsAdmin() || (&Actor{User: &User{ID: 2}}).IsAdmin() {
		t.Error("not an admin")
	}
	if !(&Actor{User: admin}).IsAdmin() {
		t.Error("an admin")
	}
	if (&Actor{User: admin, Scope: map[int64]int{}}).IsAdmin() {
		t.Error("a job's scoped token is never an admin")
	}
}

func TestAutomationRefRunnerOwner(t *testing.T) {
	var none *AutomationRef
	if got := none.RunnerOwner(); got != 0 {
		t.Errorf("no automation: %d", got)
	}
	if got := (&AutomationRef{ID: 1}).RunnerOwner(); got != 0 {
		t.Errorf("nobody saved it: %d", got)
	}
	if got := (&AutomationRef{ID: 1, SavedBy: 9}).RunnerOwner(); got != 9 {
		t.Errorf("saved: %d", got)
	}
}

func TestSessionModelNames(t *testing.T) {
	for _, ok := range []string{"", "opus", "claude-opus-5-5[1m]", strings.Repeat("a", 80)} {
		if err := (&SessionSettings{Model: ok}).Validate("all"); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{strings.Repeat("a", 81), "-p", "--dangerously-skip-permissions", ".hidden", "[1m]", "a b"} {
		if err := (&SessionSettings{Model: bad}).Validate("all"); !IsInvalid(err) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
