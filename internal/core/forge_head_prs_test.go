package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func headPR(n int, ref string) map[string]any {
	return map[string]any{"number": n, "title": "t", "html_url": "x", "state": "open", "head": map[string]any{"ref": ref, "sha": "abc"}, "base": map[string]any{"ref": "main"}}
}

// A pull request a crashed attempt opened is found by its branch, not by being
// among the repo's fifty most recently updated (WP-1370).
func TestOpenPRsFromHeadFindsItPastTheFirstPage(t *testing.T) {
	ctx := context.Background()
	a, f, c := newAPIFake(t, "forgejo")
	var page1, page2 []map[string]any
	for i := 1; i <= 50; i++ {
		page1 = append(page1, headPR(i, fmt.Sprintf("other-%d", i)))
	}
	page2 = append(page2, headPR(77, "wp-1-first"))
	a.reply["GET /repos/me/app/pulls?state=open&limit=50&page=1"] = page1
	a.reply["GET /repos/me/app/pulls?state=open&limit=50&page=2"] = page2
	got, err := c.OpenPRsFromHead(ctx, f, "me/app", "wp-1-first")
	if err != nil || len(got) != 1 || got[0].Number != 77 {
		t.Fatalf("got %+v, %v", got, err)
	}
	a, f, c = newAPIFake(t, "github")
	a.reply["GET /repos/me/app/pulls?state=open&per_page=50&head=me%3Awp-1-first"] = []map[string]any{headPR(5, "wp-1-first")}
	if got, err := c.OpenPRsFromHead(ctx, f, "me/app", "wp-1-first"); err != nil || len(got) != 1 {
		t.Fatalf("github: %+v, %v", got, err)
	}
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/merge_requests?state=opened&per_page=50&source_branch=wp-1-first"] = []map[string]any{{"iid": 5, "state": "opened", "source_branch": "wp-1-first", "target_branch": "main"}}
	if got, err := c.OpenPRsFromHead(ctx, f, "me/app", "wp-1-first"); err != nil || len(got) != 1 {
		t.Fatalf("gitlab: %+v, %v", got, err)
	}
}

// "Already exists" from a forge that doesn't list the pull request is tried
// again; it isn't a refusal that ends the round (WP-1370).
func TestAlreadyExistsNotListedIsRetriedNotRefused(t *testing.T) {
	e, round := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	e.g.createErr = &forgeAPIError{method: "POST", path: "/repos/x/pulls", status: 422, body: "A pull request already exists for x:wp-1-first"}
	e.tick(time.Second)
	e.s.Wait()
	bp := roundBeforePR(e.ctx, e.s.db.R, round)
	if bp == nil || bp.ShipStop != "" || bp.ShipRefused {
		t.Fatalf("an 'already exists' that isn't listed ended the round: %+v", bp)
	}
	var ae *forgeAPIError
	if errors.As(errors.New(bp.ShipErr), &ae) || !strings.Contains(bp.ShipErr, "already exist") {
		t.Fatalf("ship error = %q", bp.ShipErr)
	}
}
