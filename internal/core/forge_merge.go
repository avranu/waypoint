package core

// Merging a pull request on its forge (WP-D21).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Merge styles.
const (
	MergeCommit = "merge"
	MergeSquash = "squash"
	MergeRebase = "rebase"
)

func validMergeStyle(s string) bool {
	return s == "" || s == MergeCommit || s == MergeSquash || s == MergeRebase
}

// MergeOptions say how a pull request is merged. HeadSHA is the head the
// merge was decided at: the forge refuses if the branch moved on.
type MergeOptions struct {
	Style        string // merge, squash, rebase; "" is the repo's default
	HeadSHA      string
	DeleteBranch bool
	Title        string
	Message      string
}

// ForgeMerger is the optional part of a ForgeClient that merges pull
// requests.
type ForgeMerger interface {
	MergePR(ctx context.Context, f *Forge, repo string, pr *ForgePR, o MergeOptions) error
}

func (c *httpForgeClient) MergePR(ctx context.Context, f *Forge, repo string, pr *ForgePR, o MergeOptions) error {
	if !validMergeStyle(o.Style) {
		return invalid("merge style must be merge, squash or rebase")
	}
	switch f.Kind {
	case "gitlab":
		if o.Style == MergeRebase {
			return fmt.Errorf("%w: GitLab rebases through the project's merge method (fast-forward); merge or squash instead", ErrUnsupported)
		}
		body := map[string]any{"sha": o.HeadSHA, "squash": o.Style == MergeSquash, "should_remove_source_branch": o.DeleteBranch}
		if o.Message != "" {
			if o.Style == MergeSquash {
				body["squash_commit_message"] = o.Message
			} else {
				body["merge_commit_message"] = o.Message
			}
		}
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d/merge", glProject(repo), pr.Number), body, nil)
	case "github":
		method := o.Style
		if method == "" {
			var r struct {
				Merge  bool `json:"allow_merge_commit"`
				Squash bool `json:"allow_squash_merge"`
				Rebase bool `json:"allow_rebase_merge"`
			}
			if err := c.do(ctx, f, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
				return err
			}
			switch {
			case r.Merge:
				method = MergeCommit
			case r.Squash:
				method = MergeSquash
			case r.Rebase:
				method = MergeRebase
			default:
				method = MergeCommit
			}
		}
		body := map[string]any{"sha": o.HeadSHA, "merge_method": method}
		if o.Title != "" {
			body["commit_title"] = o.Title
		}
		if o.Message != "" {
			body["commit_message"] = o.Message
		}
		if err := c.do(ctx, f, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, pr.Number), body, nil); err != nil {
			return err
		}
		// GitHub keeps the branch unless asked; never delete a fork's.
		if o.DeleteBranch && pr.HeadRef != "" && strings.EqualFold(pr.HeadRepo, repo) {
			if err := c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repo, escapePath(pr.HeadRef)), nil, nil); err != nil && !isStatus(err, 404) && !isStatus(err, 422) {
				return &branchNotDeleted{err}
			}
		}
		return nil
	}
	// Forgejo and Gitea.
	do := o.Style
	if do == "" {
		var r struct {
			DefaultMergeStyle string `json:"default_merge_style"`
		}
		_ = c.do(ctx, f, http.MethodGet, "/repos/"+repo, nil, &r)
		do = cmp(r.DefaultMergeStyle, MergeCommit)
	}
	body := map[string]any{"Do": do, "head_commit_id": o.HeadSHA, "delete_branch_after_merge": o.DeleteBranch}
	if o.Title != "" {
		body["MergeTitleField"] = o.Title
	}
	if o.Message != "" {
		body["MergeMessageField"] = o.Message
	}
	return c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, pr.Number), body, nil)
}

// branchNotDeleted: the pull request merged, but its branch couldn't be
// deleted.
type branchNotDeleted struct{ err error }

func (e *branchNotDeleted) Error() string {
	return "merged, but the branch was not deleted: " + e.err.Error()
}
func (e *branchNotDeleted) Unwrap() error { return e.err }

func escapePath(p string) string {
	return strings.Join(mapStrings(strings.Split(p, "/"), url.PathEscape), "/")
}

// mergeRefusal says in a sentence why a forge refused to merge.
func mergeRefusal(err error) string {
	var ae *forgeAPIError
	if !errors.As(err, &ae) {
		return forgeSaid(err)
	}
	// The forge's words are sanitised: this reaches the armed note, which
	// the babysit types into a session.
	body := forgeWords(ae.body, 300)
	switch ae.status {
	case http.StatusConflict:
		return "a newer push replaced the head that was checked"
	case http.StatusMethodNotAllowed:
		return "the forge says it can't be merged yet (conflicts, or a required check or approval is missing): " + body
	case http.StatusForbidden, http.StatusUnauthorized:
		return "the forge's token isn't allowed to merge here: " + body
	case http.StatusUnprocessableEntity, http.StatusNotAcceptable:
		return "the forge refused the merge: " + body
	}
	return forgeSaid(err)
}
