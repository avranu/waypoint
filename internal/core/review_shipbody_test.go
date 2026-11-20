package core

import (
	"testing"
	"time"
)

func bodyShip(in *OpenPRInput) {
	in.Body = "## Summary\n\nDid the thing.\n\n## Tested\n\nmake check\n"
	in.BodyInputs = &PRBodyInputs{Summary: "Did the thing.", Message: "did it", Commits: []string{"First\n\nDid it."}, Tested: "make check"}
}

// prBodyRows is what the model is to reword, per pull request opened.
func (e *loopEnv) prBodyRows() (n int, inputs string) {
	e.t.Helper()
	_ = e.s.db.R.QueryRow(`SELECT count(*), coalesce(max(inputs), '') FROM pr_bodies`).Scan(&n, &inputs)
	return n, inputs
}

// C: the pull request Waypoint opens after a clean review carries what the
// same ship opens directly: the body and the inputs a model rewords it from.
func TestServerOpenedPRKeepsBodyInputs(t *testing.T) {
	direct := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := direct.ship(direct.task.Key, ShipSkip, bodyShip, func(in *OpenPRInput) { in.Reason = "tiny" }); err != nil || res.Pull == nil {
		t.Fatalf("direct ship = %+v, %v", res, err)
	}
	if len(direct.g.created) != 1 {
		t.Fatalf("direct opened %d", len(direct.g.created))
	}
	dn, din := direct.prBodyRows()

	held := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := held.ship(held.task.Key, ShipAuto, bodyShip); err != nil || res.Held == "" {
		t.Fatalf("held ship = %+v, %v", res, err)
	}
	held.stopWith("Pushed; the review comes first.")
	held.tick(61 * time.Second)
	held.endAudit(held.box, "done")
	held.tick(time.Second)
	held.s.Wait()
	if len(held.g.created) != 1 {
		t.Fatalf("Waypoint opened %d pull requests", len(held.g.created))
	}
	if a, b := held.g.created[0], direct.g.created[0]; a.Title != b.Title || a.Body != b.Body {
		t.Errorf("server-opened %q / %q, direct %q / %q", a.Title, a.Body, b.Title, b.Body)
	}
	hn, hin := held.prBodyRows()
	if dn != 1 || hn != dn || hin != din {
		t.Errorf("body inputs kept for the model: direct %d %s, server-opened %d %s", dn, din, hn, hin)
	}
}
