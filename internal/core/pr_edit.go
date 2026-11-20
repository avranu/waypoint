package core

// wp pr edit: change a pull request's title or description on the forge.

import (
	"context"
	"fmt"
	"net/http"
)

// EditPRInput is what wp pr edit changes; nil leaves a field.
type EditPRInput struct {
	Title *string
	Body  *string
}

// EditPullRequest changes a pull request's title or description on the forge.
// A draft keeps its "WIP: " prefix; Waypoint's record keeps the clean title.
// Whoever may merge it may edit it.
func (s *Service) EditPullRequest(ctx context.Context, actor *Actor, prID int64, in EditPRInput) error {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return err
	}
	if err := s.mayMerge(ctx, actor, pr); err != nil {
		return err
	}
	if pr.State != "open" && pr.State != "draft" {
		return invalid("it is %s", pr.State)
	}
	if in.Title == nil && in.Body == nil {
		return invalid("say what to change: a title or a description")
	}
	ed, ok := s.Forges.(ForgePREditor)
	if !ok {
		return invalid("this forge client can't edit pull requests")
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if f.token == "" {
		return invalid("the forge has no API token; add one in Admin → Forges")
	}
	title := ""
	if in.Title != nil {
		if title = stripWIP(*in.Title); title == "" {
			return invalid("the title can't be empty")
		}
		forgeTitle := title
		if pr.State == "draft" {
			forgeTitle = wipPrefix + title
		}
		if err := ed.EditPRTitle(ctx, f, pr.Repo, pr.Number, forgeTitle); err != nil {
			return err
		}
	}
	if in.Body != nil {
		if err := ed.EditPRBody(ctx, f, pr.Repo, pr.Number, *in.Body); err != nil {
			return err
		}
	}
	projectID, _ := s.prAuthority(ctx, actor, pr, RoleViewer)
	return s.write(ctx, actor, func(t *tx) error {
		detail := "edited the description of"
		if in.Title != nil {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET title = ? WHERE id = ?`, excerpt(title, 300), pr.ID); err != nil {
				return err
			}
			detail = "retitled"
		}
		t.audit(AuditEntry{Action: "pr.edit", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail + " “" + excerpt(pr.Title, 120) + "”"})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "edited", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
}

func (c *httpForgeClient) EditPRBody(ctx context.Context, f *Forge, repo string, number int, body string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), map[string]string{"description": body}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"body": body}, nil)
}
