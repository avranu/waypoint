package core

import (
	"strings"
	"testing"
)

// fix_ci on a pull request a rule reacts to works only on a member's, from a
// branch of the repository itself (D14); a manual run is a person's choice.
func TestFixCIGate(t *testing.T) {
	ae := newAgentEnv(t)
	pr := ae.openPR5()
	gate := func(kind, author, headRepo, agent string) string {
		fp := testPR
		fp.Author, fp.HeadRepo = author, headRepo
		ae.fg.setPR(ae.f, "me/app", fp, sampleDiff)
		cp := *pr
		cp.Agent = agent
		c := &StepCtx{Ctx: ae.ctx, S: ae.s, Automation: &Automation{ProjectID: ae.p.ID}, Event: &Event{Kind: kind}}
		return c.fixCIGate(&cp)
	}
	for _, tc := range []struct {
		kind, author, head, agent, why string
	}{
		{EvPRCI, "robin", "me/app", "", ""},
		{EvPRCI, "mallory", "me/app", "", "member"},
		{EvPRCI, "robin", "mallory/app", "", "fork"},
		{EvPRCI, "bot", "me/app", "claude", ""}, // an agent's pull request, in the repository
		{EvPRCI, "bot", "mallory/app", "claude", "fork"},
		{"manual", "mallory", "mallory/app", "", ""},
	} {
		got := gate(tc.kind, tc.author, tc.head, tc.agent)
		if (tc.why == "") != (got == "") || !strings.Contains(got, tc.why) {
			t.Errorf("%+v: gate = %q", tc, got)
		}
	}
}
