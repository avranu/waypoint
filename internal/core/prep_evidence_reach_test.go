package core

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"
)

// The notes may paraphrase any record the model was offered, so a check is
// served to nobody once one of those has left the task's project, is gone or
// is a draft (WP-ADR157).
func TestPrepEvidenceOfferedOutOfReach(t *testing.T) {
	setup := func(t *testing.T) (*evidenceEnv, *Project) {
		e := newEvidenceEnv(t)
		e.seed()
		e.decision("latest", e.p.Key, "Latest merge checks decision", "## Assumptions\n- Merge checks pass at once.")
		e.llm.evidence = e.answer()
		e.prep(e.task.Key)
		if p := e.served(e.admin, e.task.Key, false); p.Evidence == nil {
			t.Fatal("nothing served before the change")
		}
		for _, name := range []string{"linked", "ref", "note", "assumes", "latest"} {
			if !slices.ContainsFunc(e.candidates(e.task), func(c evidenceCand) bool { return c.Ref == e.code(name) }) {
				t.Fatalf("%s isn't offered", name)
			}
		}
		return e, e.project(e.admin, "Third", "")
	}
	nothing := func(t *testing.T, e *evidenceEnv, key string, people ...*Actor) {
		t.Helper()
		for _, a := range append(people, e.admin) {
			if p := e.served(a, key, false); p.State != "ready" || p.Stale || p.Evidence != nil {
				t.Errorf("a person is served %s (stale %v): %+v", p.State, p.Stale, p.Evidence)
			}
		}
		e.claim(e.ag, key)
		if p := e.served(e.ag, key, true); p.Evidence != nil {
			t.Errorf("the holder is served %+v", p.Evidence)
		}
		if got := e.units(e.task.ID); got != "0 0" {
			t.Errorf("units %s: an arm was drawn", got)
		}
		e.forceArm(e.task.ID, "shown")
		if p := e.served(e.ag, key, false); p.Evidence != nil {
			t.Errorf("the shown arm is served %+v", p.Evidence)
		}
	}
	exec := func(t *testing.T, e *evidenceEnv, q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("task moved", func(t *testing.T) {
		e, other := setup(t)
		viewer := e.user("viewer", false)
		if err := e.s.SetMember(e.ctx, e.admin, other.Key, "viewer", "viewer"); err != nil {
			t.Fatal(err)
		}
		moved, err := e.s.MoveToProject(e.ctx, e.admin, e.task.Key, other.Key)
		if err != nil {
			t.Fatal(err)
		}
		nothing(t, e, moved.Key, viewer)
	})
	t.Run("cited page a draft", func(t *testing.T) {
		e, _ := setup(t)
		exec(t, e, `UPDATE pages SET draft = 1 WHERE id = ?`, e.pages["linked"].ID)
		nothing(t, e, e.task.Key)
	})
	t.Run("uncited page deleted", func(t *testing.T) {
		e, _ := setup(t)
		if err := e.s.DeletePage(e.ctx, e.admin, e.pages["note"].ID); err != nil {
			t.Fatal(err)
		}
		nothing(t, e, e.task.Key)
	})
	t.Run("page moved", func(t *testing.T) {
		e, other := setup(t)
		if _, err := e.s.UpdatePage(e.ctx, e.admin, e.pages["note"].ID, PageInput{Project: Some(other.Key)}); err != nil {
			t.Fatal(err)
		}
		nothing(t, e, e.task.Key)
	})
	t.Run("decision moved", func(t *testing.T) {
		e, other := setup(t)
		if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["latest"].ID, DesignInput{Project: &other.Key}); err != nil {
			t.Fatal(err)
		}
		nothing(t, e, e.task.Key)
	})
	t.Run("decision deleted and its id given again", func(t *testing.T) {
		e, _ := setup(t)
		old := e.notes["latest"].ID
		if err := e.s.DeleteDesign(e.ctx, e.admin, old); err != nil {
			t.Fatal(err)
		}
		e.setClock(e.clockNow().Add(time.Minute))
		e.decision("next", e.p.Key, "Another", "## Decision\nOther.")
		if got := e.notes["next"].ID; got != old {
			t.Fatalf("id %d; want %d again", got, old)
		}
		nothing(t, e, e.task.Key)
	})
	t.Run("superseded and retired", func(t *testing.T) {
		e, _ := setup(t)
		e.page(e.admin, "newer", "experiment", e.p.Key, "Newer", "Newer.", false)
		e.supersede("newer", "linked")
		retired := "retired"
		if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["assumes"].ID, DesignInput{Status: &retired}); err != nil {
			t.Fatal(err)
		}
		p := e.served(e.admin, e.task.Key, false)
		if p.Evidence == nil {
			t.Fatal("none served")
		}
		if got, want := fmt.Sprint(refsOf(p.Evidence)), fmt.Sprint([]string{e.code("ref")}); got != want {
			t.Errorf("refs %s; want %s", got, want)
		}
		if len(p.Evidence.Claims) != 2 || p.Evidence.Claims[0].Ref != "" {
			t.Errorf("claims %+v", p.Evidence.Claims)
		}
	})
	t.Run("nothing offered", func(t *testing.T) {
		e := newEvidenceEnv(t)
		e.llm.evidence = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`
		e.prep(e.task.Key)
		other := e.project(e.admin, "Third", "")
		moved, err := e.s.MoveToProject(e.ctx, e.admin, e.task.Key, other.Key)
		if err != nil {
			t.Fatal(err)
		}
		if p := e.served(e.admin, moved.Key, false); p.Evidence == nil || len(p.Evidence.Claims) != 1 {
			t.Errorf("served %+v", p.Evidence)
		}
	})
}

// What a check offered is kept with it, and a new check starts with none.
func TestPrepEvidenceOfferedReset(t *testing.T) {
	e := newEvidenceEnv(t)
	e.seed()
	e.prep(e.task.Key)
	offered := func() string {
		return e.one(`SELECT json_array_length(offered) || ' ' || candidates FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID)
	}
	if got := offered(); got != "4 4" {
		t.Fatalf("offered and candidates: %s", got)
	}
	d, err := e.s.issueByID(e.ctx, e.s.db.R, e.task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.beginPrepEvidence(e.ctx, prepEvidenceTask{issue: d, hash: "h"}); err != nil {
		t.Fatal(err)
	}
	if got := offered(); got != "0 0" {
		t.Errorf("a new check keeps %s", got)
	}
}

// A decision that counts on a person's request stays unreviewed through an
// approval and an agent's changes; one a person approved stays reviewed, an
// agent's change to it waiting for a person.
func TestPrepEvidenceDecisionReviewLabel(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		requested  bool
	}{
		{"requested", "agent, unreviewed", true},
		{"approved", "reviewed by a person", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceEnv(t)
			title, kind, body := "Merge checks", "decision", "## Assumptions\n- Merge checks are quick."
			in := DesignInput{Title: &title, Body: &body, Kind: &kind, Project: &e.p.Key}
			if tc.requested {
				in.For = e.task.Key
			}
			n, err := e.s.CreateDesign(e.ctx, e.ag, in)
			if err != nil {
				t.Fatal(err)
			}
			e.notes["note"] = n
			by := func(when string) {
				t.Helper()
				got := e.candidates(e.task)
				if len(got) != 1 || got[0].By != tc.want {
					t.Errorf("%s: %+v; want %q", when, got, tc.want)
				}
			}
			approve := func() {
				t.Helper()
				e.setClock(e.clockNow().Add(time.Hour))
				if _, err := e.s.ApproveDesign(e.ctx, e.admin, n.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			approve()
			by("approval")
			e.setClock(e.clockNow().Add(time.Hour))
			changed := "## Assumptions\n- Merge checks are slow."
			if _, err := e.s.UpdateDesign(e.ctx, e.ag, n.ID, DesignInput{Body: &changed}); err != nil {
				t.Fatal(err)
			}
			by("an agent's change")
			if tc.requested {
				approve()
				by("approval after the change")
				return
			}
			rev, err := strconv.ParseInt(e.one(`SELECT id FROM design_revisions WHERE note_id = ? AND state = 'pending'`, n.ID), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			e.setClock(e.clockNow().Add(time.Hour))
			if _, err := e.s.ReviewDesign(e.ctx, e.admin, rev, "approve", ""); err != nil {
				t.Fatal(err)
			}
			by("the change approved")
		})
	}
}

// Pages of categories that record no data are never offered, linked to the
// task or matching its words.
func TestPrepEvidenceCategoriesNeverOffered(t *testing.T) {
	e := newEvidenceEnv(t)
	for _, category := range []string{"plan", "idea", "feature", "bug"} {
		e.page(e.admin, category, category, e.p.Key, "Merge checks "+category, "Merge checks pass.", false, e.task.Key)
		e.page(e.admin, category+" matched", category, e.p.Key, "Merge checks "+category+" matched", "Merge checks pass.", false)
	}
	e.page(e.admin, "note", "note", e.p.Key, "Merge checks note", "Merge checks pass.", false, e.task.Key)
	var got []string
	for _, c := range e.candidates(e.task) {
		got = append(got, c.Category+" "+c.Ref)
	}
	if want := []string{"note " + e.code("note")}; !slices.Equal(got, want) {
		t.Errorf("offered %v; want %v", got, want)
	}
}
