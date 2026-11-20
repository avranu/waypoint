package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestChangePRLabelsOnEachForge(t *testing.T) {
	ctx := context.Background()

	// GitHub: names; a missing label to remove is fine.
	a, f, c := newAPIFake(t, "github")
	a.reply["POST /repos/me/app/issues/5/labels"] = []any{}
	a.reply["DELETE /repos/me/app/issues/5/labels/old"] = 204
	if err := c.ChangePRLabels(ctx, f, "me/app", 5, []string{"automerge"}, []string{"old", "gone"}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/issues/5/labels").body; !slices.Equal(anyStrings(b["labels"]), []string{"automerge"}) {
		t.Errorf("github add = %+v", b)
	}
	if a.call("DELETE", "/repos/me/app/issues/5/labels/gone") == nil {
		t.Error("github didn't try to remove the second label")
	}

	// Forgejo: ids. A label the repo lacks is made first; removal looks the id up.
	a, f, c = newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/labels?limit=200"] = []map[string]any{{"id": 7, "name": "bug"}}
	a.reply["POST /repos/me/app/labels"] = map[string]any{"id": 9, "name": "automerge"}
	a.reply["POST /repos/me/app/issues/5/labels"] = []any{}
	a.reply["GET /repos/me/app/issues/5/labels"] = []map[string]any{{"id": 7, "name": "Bug"}}
	a.reply["DELETE /repos/me/app/issues/5/labels/7"] = 204
	if err := c.ChangePRLabels(ctx, f, "me/app", 5, []string{"bug", "automerge"}, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/labels").body; b["name"] != "automerge" || b["color"] == "" {
		t.Errorf("forgejo label made = %+v", b)
	}
	if ids, _ := a.call("POST", "/repos/me/app/issues/5/labels").body["labels"].([]any); len(ids) != 2 || ids[0] != 7.0 || ids[1] != 9.0 {
		t.Errorf("forgejo add ids = %v", ids)
	}
	if a.call("DELETE", "/repos/me/app/issues/5/labels/7") == nil {
		t.Error("forgejo didn't remove the label by id")
	}

	// GitLab: names in one update.
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["PUT /projects/me%2Fapp/merge_requests/5"] = map[string]any{}
	if err := c.ChangePRLabels(ctx, f, "me/app", 5, []string{"a", "b"}, []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("PUT", "/projects/me%2Fapp/merge_requests/5").body; b["add_labels"] != "a,b" || b["remove_labels"] != "c" {
		t.Errorf("gitlab labels = %+v", b)
	}
}

func TestRequestReviewersOnEachForge(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"github", "forgejo"} {
		a, f, c := newAPIFake(t, kind)
		a.reply["POST /repos/me/app/pulls/5/requested_reviewers"] = map[string]any{}
		if err := c.RequestReviewers(ctx, f, "me/app", 5, []string{"ann", "bo"}); err != nil {
			t.Fatal(err)
		}
		if b := a.call("POST", "/repos/me/app/pulls/5/requested_reviewers").body; !slices.Equal(anyStrings(b["reviewers"]), []string{"ann", "bo"}) {
			t.Errorf("%s reviewers = %+v", kind, b)
		}
	}
	// GitLab sets ids: the ones already there, plus the new.
	a, f, c := newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/merge_requests/5"] = map[string]any{"reviewers": []map[string]any{{"id": 3}}}
	a.reply["GET /users?username=ann"] = []map[string]any{{"id": 11}}
	a.reply["PUT /projects/me%2Fapp/merge_requests/5"] = map[string]any{}
	if err := c.RequestReviewers(ctx, f, "me/app", 5, []string{"ann"}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := a.call("PUT", "/projects/me%2Fapp/merge_requests/5").body["reviewer_ids"].([]any); len(ids) != 2 || ids[0] != 3.0 || ids[1] != 11.0 {
		t.Errorf("gitlab reviewer ids = %v", ids)
	}
	a.reply["GET /users?username=nobody"] = []map[string]any{}
	if err := c.RequestReviewers(ctx, f, "me/app", 5, []string{"nobody"}); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("unknown user: %v", err)
	}
}

func TestCreatePRDraft(t *testing.T) {
	ctx := context.Background()
	for kind, want := range map[string]string{"github": "Fix", "forgejo": "WIP: Fix"} {
		a, f, c := newAPIFake(t, kind)
		a.reply["POST /repos/me/app/pulls"] = map[string]any{"number": 5, "title": want, "html_url": "x", "state": "open"}
		if _, err := c.CreatePR(ctx, f, "me/app", NewPR{Title: "Fix", Head: "h", Base: "main", Draft: true}); err != nil {
			t.Fatal(err)
		}
		b := a.call("POST", "/repos/me/app/pulls").body
		if b["title"] != want || (kind == "github") != (b["draft"] == true) {
			t.Errorf("%s draft = %+v", kind, b)
		}
	}
	a, f, c := newAPIFake(t, "gitlab")
	a.reply["POST /projects/me%2Fapp/merge_requests"] = map[string]any{"iid": 5, "title": "Draft: Fix", "state": "opened"}
	if _, err := c.CreatePR(ctx, f, "me/app", NewPR{Title: "Fix", Head: "h", Base: "main", Draft: true}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/projects/me%2Fapp/merge_requests").body; b["title"] != "Draft: Fix" {
		t.Errorf("gitlab draft = %+v", b)
	}
}

// An urgent pull request is opened with its label (WP-528): Forgejo by id,
// making the label in the repo's own colour when it lacks one; GitHub
// labels it just after; GitLab by name.
func TestCreatePRLabels(t *testing.T) {
	ctx := context.Background()
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/labels?limit=200"] = []map[string]any{{"id": 7, "name": "bug"}}
	a.reply["POST /repos/me/app/labels"] = map[string]any{"id": 9, "name": CIUrgentLabel}
	a.reply["POST /repos/me/app/pulls"] = map[string]any{"number": 5, "title": "Fix", "html_url": "x", "state": "open"}
	pr, err := c.CreatePR(ctx, f, "me/app", NewPR{Title: "Fix", Head: "h", Base: "main", Labels: []string{CIUrgentLabel}})
	if err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/labels").body; b["name"] != CIUrgentLabel || b["color"] != "#d93f0b" {
		t.Errorf("label made = %+v", b)
	}
	if ids, _ := a.call("POST", "/repos/me/app/pulls").body["labels"].([]any); len(ids) != 1 || ids[0] != 9.0 {
		t.Errorf("forgejo create label ids = %v", ids)
	}
	if !slices.Equal(pr.Labels, []string{CIUrgentLabel}) {
		t.Errorf("forgejo pr labels = %v", pr.Labels)
	}

	a, f, c = newAPIFake(t, "github")
	a.reply["POST /repos/me/app/pulls"] = map[string]any{"number": 5, "title": "Fix", "html_url": "x", "state": "open"}
	a.reply["POST /repos/me/app/issues/5/labels"] = []any{}
	if pr, err = c.CreatePR(ctx, f, "me/app", NewPR{Title: "Fix", Head: "h", Base: "main", Labels: []string{CIUrgentLabel}}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/issues/5/labels").body; !slices.Equal(anyStrings(b["labels"]), []string{CIUrgentLabel}) || !slices.Equal(pr.Labels, []string{CIUrgentLabel}) {
		t.Errorf("github labels = %+v, pr %v", b, pr.Labels)
	}

	a, f, c = newAPIFake(t, "gitlab")
	a.reply["POST /projects/me%2Fapp/merge_requests"] = map[string]any{"iid": 5, "title": "Fix", "state": "opened"}
	if _, err := c.CreatePR(ctx, f, "me/app", NewPR{Title: "Fix", Head: "h", Base: "main", Labels: []string{CIUrgentLabel}}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/projects/me%2Fapp/merge_requests").body; b["labels"] != CIUrgentLabel {
		t.Errorf("gitlab labels = %+v", b)
	}
}

// Forgejo answers an HTML 500 to a reaction its account has already made, so
// a second sign-off on a pull request has to clear the first (WP-847).
func TestReactTwiceOnForgejo(t *testing.T) {
	reacted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			reacted = false
		case http.MethodPost:
			if reacted {
				http.Error(w, "<html>Internal Server Error</html>", http.StatusInternalServerError)
				return
			}
			reacted = true
			_, _ = w.Write([]byte(`{"id":1,"content":"+1"}`))
		}
	}))
	defer srv.Close()
	c := &httpForgeClient{http: srv.Client()}
	f := &Forge{ID: 1, Kind: "forgejo", BaseURL: srv.URL, token: "tok"}
	for i := range 2 {
		if _, err := c.React(context.Background(), f, "me/app", 5, "", "+1"); err != nil {
			t.Fatalf("reaction %d: %v", i+1, err)
		}
	}
}

// Only Forgejo needs that: GitHub answers a repeat itself and deletes by id.
func TestReactOnGitHubDeletesNothing(t *testing.T) {
	a, f, c := newAPIFake(t, "github")
	a.reply["POST /repos/me/app/issues/5/reactions"] = map[string]any{"id": 1}
	if _, err := c.React(context.Background(), f, "me/app", 5, "", "+1"); err != nil {
		t.Fatal(err)
	}
	if call := a.call(http.MethodDelete, "/repos/me/app/issues/5/reactions"); call != nil {
		t.Errorf("deleted on GitHub: %+v", call)
	}
}
