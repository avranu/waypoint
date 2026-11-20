package core

// Branches on a forge (WP-140): list them, compare one with another, delete
// one, and finish a pull request whose work already landed elsewhere.
// httpForgeClient implements it for GitHub, Forgejo and GitLab.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ForgeBranch is a branch as its forge lists it.
type ForgeBranch struct {
	Name      string
	SHA       string
	Protected bool
}

// ForgeBranches is the optional part of a ForgeClient that reads and deletes
// branches and closes pull requests that have nothing left to merge.
type ForgeBranches interface {
	// Branches lists a repo's branches (at most maxForgeBranches).
	Branches(ctx context.Context, f *Forge, repo string) ([]ForgeBranch, error)
	// Ahead counts the commits head has that base lacks; 0 means head is
	// already contained in base. Either may be a branch name or a sha.
	Ahead(ctx context.Context, f *Forge, repo, base, head string) (int, error)
	// CommitTime is when a commit was made.
	CommitTime(ctx context.Context, f *Forge, repo, sha string) (time.Time, error)
	// DeleteBranch deletes a branch; one that is already gone is no error.
	DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error
	// MarkMerged records a pull request as merged by a commit that is
	// already on its base, without merging anything (Forgejo and Gitea's
	// "manually merged"); ErrUnsupported where the forge can't.
	MarkMerged(ctx context.Context, f *Forge, repo string, number int, sha string) error
	// ClosePR closes a pull request without merging it.
	ClosePR(ctx context.Context, f *Forge, repo string, number int) error
}

// maxForgeBranches bounds a repo's branch listing (ten pages of a hundred).
const maxForgeBranches = 1000

func (c *httpForgeClient) Branches(ctx context.Context, f *Forge, repo string) ([]ForgeBranch, error) {
	var out []ForgeBranch
	for page := 1; len(out) < maxForgeBranches; page++ {
		var list []struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				ID  string `json:"id"`
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		var path string
		switch f.Kind {
		case "gitlab":
			path = fmt.Sprintf("%s/repository/branches?per_page=100&page=%d", glProject(repo), page)
		case "github":
			path = fmt.Sprintf("/repos/%s/branches?per_page=100&page=%d", repo, page)
		default:
			path = fmt.Sprintf("/repos/%s/branches?limit=100&page=%d", repo, page)
		}
		if err := c.do(ctx, f, http.MethodGet, path, nil, &list); err != nil {
			return nil, err
		}
		for _, b := range list {
			out = append(out, ForgeBranch{Name: b.Name, SHA: cmp(b.Commit.ID, b.Commit.SHA), Protected: b.Protected})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) Ahead(ctx context.Context, f *Forge, repo, base, head string) (int, error) {
	if f.Kind == "gitlab" {
		var r struct {
			Commits []struct{ ID string } `json:"commits"`
		}
		path := fmt.Sprintf("%s/repository/compare?from=%s&to=%s", glProject(repo), url.QueryEscape(base), url.QueryEscape(head))
		if err := c.do(ctx, f, http.MethodGet, path, nil, &r); err != nil {
			return 0, err
		}
		return len(r.Commits), nil
	}
	var r struct {
		AheadBy      *int `json:"ahead_by"`      // GitHub
		TotalCommits *int `json:"total_commits"` // Forgejo and Gitea: the commits in base...head
	}
	path := fmt.Sprintf("/repos/%s/compare/%s...%s", repo, escapePath(base), escapePath(head))
	if f.Kind == "github" {
		path += "?per_page=1"
	}
	if err := c.do(ctx, f, http.MethodGet, path, nil, &r); err != nil {
		return 0, err
	}
	switch {
	case r.AheadBy != nil:
		return *r.AheadBy, nil
	case r.TotalCommits != nil:
		return *r.TotalCommits, nil
	}
	return 0, fmt.Errorf("the forge didn't say how far %s is ahead of %s", head, base)
}

func (c *httpForgeClient) CommitTime(ctx context.Context, f *Forge, repo, sha string) (time.Time, error) {
	if f.Kind == "gitlab" {
		var r struct {
			CommittedDate time.Time `json:"committed_date"`
		}
		err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/repository/commits/%s", glProject(repo), url.PathEscape(sha)), nil, &r)
		return r.CommittedDate, err
	}
	var r struct {
		Created   time.Time `json:"created"` // Forgejo and Gitea
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"` // GitHub
	}
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repo, url.PathEscape(sha)), nil, &r)
	if t := r.Committer.Date; !t.IsZero() {
		return t, err
	}
	return r.Created, err
}

func (c *httpForgeClient) DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error {
	var err error
	switch f.Kind {
	case "gitlab":
		err = c.do(ctx, f, http.MethodDelete, fmt.Sprintf("%s/repository/branches/%s", glProject(repo), url.PathEscape(branch)), nil, nil)
	case "github":
		err = c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repo, escapePath(branch)), nil, nil)
	default:
		err = c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/branches/%s", repo, escapePath(branch)), nil, nil)
	}
	if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusUnprocessableEntity) && f.Kind == "github" {
		return nil // already gone
	}
	return err
}

func (c *httpForgeClient) MarkMerged(ctx context.Context, f *Forge, repo string, number int, sha string) error {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return ErrUnsupported
	}
	body := map[string]any{"Do": "manually-merged", "merge_commit_id": sha}
	return c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number), body, nil)
}

func (c *httpForgeClient) ClosePR(ctx context.Context, f *Forge, repo string, number int) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), map[string]string{"state_event": "close"}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"state": "closed"}, nil)
}

// isRefusal: the forge understood and said no (as opposed to being down).
func isRefusal(err error) bool {
	for _, s := range []int{http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusForbidden, http.StatusNotAcceptable, http.StatusConflict, http.StatusBadRequest} {
		if isStatus(err, s) {
			return true
		}
	}
	return false
}
