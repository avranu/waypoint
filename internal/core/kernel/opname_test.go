package kernel

import "testing"

type Service struct{}

func (*Service) Probe() string { return callerName(1) }

func TestOperationName(t *testing.T) {
	const p = "git.jmann.me/claude/waypoint/internal/core"
	for full, want := range map[string]string{
		p + ".(*Service).CreateIssue":                  "CreateIssue",
		p + ".(*Service).CreateIssue.func1":            "CreateIssue",
		p + ".(*Service).CreateIssue.func1.2":          "CreateIssue",
		p + "/email.(*Service).SetEmailSettings":       "SetEmailSettings",
		p + "/email.(*Service).SetEmailSettings.func1": "SetEmailSettings",
		p + ".(*tx).emit":                              "(*tx)", // as before the generalisation
		p + ".sweep.func2":                             "sweep",
	} {
		if got := operationName(full); got != want {
			t.Errorf("operationName(%q) = %q, want %q", full, got, want)
		}
	}
	if got := (&Service{}).Probe(); got != "Probe" {
		t.Errorf("callerName from a Service method in package kernel = %q", got)
	}
}
