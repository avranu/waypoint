package core

// wp pr edit: change a pull request's title, description or base on the forge.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// EditPRInput is what wp pr edit changes; nil leaves a field.
type EditPRInput struct {
	Title *string
	Body  *string
	// Base retargets it: a wrong base is fixed without closing it, which
	// would lose its number and review history (WP-634).
	Base *string
}

// EditPullRequest changes a pull request's title, description or base on
// the forge. A draft keeps its "WIP: " prefix; Waypoint's record keeps the
// clean title. Whoever may merge it may edit it.
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
	if in.Title == nil && in.Body == nil && in.Base == nil {
		return invalid("say what to change: a title, a description or a base")
	}
	base := ""
	if in.Base != nil {
		if base = strings.TrimSpace(*in.Base); base == "" {
			return invalid("the base can't be empty")
		}
		if base == pr.HeadRef {
			return invalid("%s is the pull request's own branch; its base is another", base)
		}
	}
	var rb ForgePRRetargeter
	if base != "" && base != pr.BaseRef {
		var ok bool
		if rb, ok = s.Forges.(ForgePRRetargeter); !ok {
			return invalid("this forge client can't change a pull request's base")
		}
	}
	if in.Title == nil && in.Body == nil && rb == nil {
		return nil // already into that base
	}
	var ed ForgePREditor
	if in.Title != nil || in.Body != nil { // a base alone needs only ForgePRRetargeter
		var ok bool
		if ed, ok = s.Forges.(ForgePREditor); !ok {
			return invalid("this forge client can't edit pull requests")
		}
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if f.token == "" {
		return invalid("the forge has no API token; add one in Admin → Forges")
	}
	// The edits are made one by one on the forge. One that fails after an
	// earlier one succeeded still leaves the record and the audit what was
	// applied, then the error comes back (WP-1670).
	title, applied := "", []string{}
	var ferr error
	if in.Title != nil {
		if title = stripWIP(*in.Title); title == "" {
			return invalid("the title can't be empty")
		}
		forgeTitle := title
		if pr.State == "draft" {
			forgeTitle = wipPrefix + title
		}
		if ferr = ed.EditPRTitle(ctx, f, pr.Repo, pr.Number, forgeTitle); ferr == nil {
			applied = append(applied, "title")
		}
	}
	if in.Body != nil && ferr == nil {
		if ferr = ed.EditPRBody(ctx, f, pr.Repo, pr.Number, *in.Body); ferr == nil {
			applied = append(applied, "body")
		}
	}
	if rb != nil && ferr == nil {
		if ferr = rb.EditPRBase(ctx, f, pr.Repo, pr.Number, base); ferr == nil {
			applied = append(applied, "base")
		}
	}
	if len(applied) == 0 {
		return ferr
	}
	projectID, rerr := s.prAuthority(ctx, actor, pr, RoleViewer)
	if rerr != nil {
		if readFault(rerr) && !missingOrDenied(rerr) {
			s.log.WarnContext(ctx, "pr authority failed", "pr", prID, "err", rerr)
		}
	}
	werr := s.write(ctx, actor, func(t *tx) error {
		var parts []string // "retitled", "edited the description", "moved the base from X to Y"
		for _, a := range applied {
			switch a {
			case "title":
				if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET title = ? WHERE id = ?`, excerpt(title, 300), pr.ID); err != nil {
					return err
				}
				parts = append(parts, "retitled")
			case "body":
				parts = append(parts, "edited the description")
			case "base":
				if err := setPRBase(ctx, t, pr.ID, base); err != nil {
					return err
				}
				parts = append(parts, "moved the base from "+pr.BaseRef+" to "+base)
			}
		}
		// One edit reads "retitled “T”" (the description and the base add
		// "of": "moved the base from X to Y of “T”"); several, "edited “T”:
		// retitled, edited the description and moved the base from X to Y".
		name := " “" + excerpt(pr.Title, 120) + "”"
		detail := ""
		switch len(parts) {
		case 0:
		case 1:
			detail = parts[0] + name
			if applied[0] != "title" {
				detail = parts[0] + " of" + name
			}
		default:
			detail = "edited" + name + ": " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
		}
		t.audit(AuditEntry{Action: "pr.edit", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "edited", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	if ferr != nil {
		if werr != nil { // the refusal is what comes back; the record's failure isn't lost
			s.log.WarnContext(ctx, "record the edits of a pull request made before the forge refused one", "pr", prID, "err", werr)
		}
		return ferr
	}
	if werr != nil && slices.Contains(applied, "base") {
		return fmt.Errorf("the forge was changed (the base is now %s) and recording it failed: %w", base, werr)
	}
	return werr
}

// setPRBase records a pull request's new base. Each base has its own merge
// queue: its place in the old one means nothing in the new one, where it
// joins again.
func setPRBase(ctx context.Context, t *tx, prID int64, base string) error {
	_, err := t.ExecContext(ctx, `UPDATE pull_requests SET base_ref = ?, queued_at = NULL WHERE id = ?`, base, prID)
	return err
}

func (c *httpForgeClient) EditPRBody(ctx context.Context, f *Forge, repo string, number int, body string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), map[string]string{"description": body}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"body": body}, nil)
}

// ForgePRRetargeter changes the branch a pull request merges into
// (httpForgeClient has it).
type ForgePRRetargeter interface {
	EditPRBase(ctx context.Context, f *Forge, repo string, number int, base string) error
}

func (c *httpForgeClient) EditPRBase(ctx context.Context, f *Forge, repo string, number int, base string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), map[string]string{"target_branch": base}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"base": base}, nil)
}
