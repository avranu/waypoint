package core

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PRNote is a review's body or a comment in a pull request's conversation.
type PRNote struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // review, comment
	Author string `json:"author"`
	Body   string `json:"body"`
	URL    string `json:"url,omitempty"`
	// State is a review's: approved, changes_requested, commented, pending.
	State     string    `json:"state,omitempty"`
	SHA       string    `json:"sha,omitempty"` // the commit a review was made on
	At        time.Time `json:"at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ForgeReviewNotes is the optional part of a ForgeClient that lists a pull
// request's review bodies and conversation comments, and reacts to any
// kind of comment on it (httpForgeClient: GitHub, Forgejo and GitLab).
type ForgeReviewNotes interface {
	PRNotes(ctx context.Context, f *Forge, repo string, number int) ([]PRNote, error)
	// ReactTo puts an emoji (GitHub's names: +1 -1 laugh hooray confused
	// heart rocket eyes) on a comment of a kind: inline, review or comment.
	// ErrUnsupported where the forge has no API for it (a review's body).
	ReactTo(ctx context.Context, f *Forge, repo string, number int, kind, commentID, emoji string) error
}

func (c *httpForgeClient) PRNotes(ctx context.Context, f *Forge, repo string, number int) ([]PRNote, error) {
	if f.Kind == "gitlab" {
		return c.glNotes(ctx, f, repo, number)
	}
	var out []PRNote
	for page := 1; page <= maxForgePages; page++ {
		var list []struct {
			ID          int64      `json:"id"`
			User        hookUser   `json:"user"`
			Body        string     `json:"body"`
			State       string     `json:"state"`
			HTMLURL     string     `json:"html_url"`
			CommitID    string     `json:"commit_id"`
			SubmittedAt *time.Time `json:"submitted_at"`
			UpdatedAt   *time.Time `json:"updated_at"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=50&limit=50&page=%d", repo, number, page), nil, &list); err != nil {
			return nil, err
		}
		for _, r := range list {
			n := PRNote{ID: strconv.FormatInt(r.ID, 10), Kind: "review", Author: r.User.name(), Body: r.Body, URL: r.HTMLURL,
				State: reviewState(r.State), SHA: r.CommitID}
			if r.SubmittedAt != nil {
				n.At = *r.SubmittedAt
			}
			n.UpdatedAt = n.At
			if r.UpdatedAt != nil && r.UpdatedAt.After(n.At) {
				n.UpdatedAt = *r.UpdatedAt
			}
			out = append(out, n)
		}
		if len(list) < 50 {
			break
		}
	}
	for page := 1; page <= maxForgePages; page++ {
		var list []struct {
			ID        int64     `json:"id"`
			User      hookUser  `json:"user"`
			Body      string    `json:"body"`
			HTMLURL   string    `json:"html_url"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=50&limit=50&page=%d", repo, number, page), nil, &list); err != nil {
			return nil, err
		}
		for _, cm := range list {
			out = append(out, PRNote{ID: strconv.FormatInt(cm.ID, 10), Kind: "comment", Author: cm.User.name(), Body: cm.Body, URL: cm.HTMLURL,
				At: cm.CreatedAt, UpdatedAt: cm.UpdatedAt})
		}
		if len(list) < 50 {
			break
		}
	}
	return out, nil
}

// reviewState names a review's state as Waypoint does: GitHub's APPROVED,
// CHANGES_REQUESTED, COMMENTED; Forgejo's APPROVED, REQUEST_CHANGES, COMMENT.
func reviewState(s string) string {
	switch strings.ToUpper(s) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED", "REQUEST_CHANGES":
		return "changes_requested"
	case "COMMENTED", "COMMENT":
		return "commented"
	case "PENDING", "REQUEST_REVIEW":
		return "pending"
	}
	return strings.ToLower(s)
}

// glNotes lists a merge request's conversation (its notes not on the diff;
// GitLab has no review bodies).
func (c *httpForgeClient) glNotes(ctx context.Context, f *Forge, repo string, number int) ([]PRNote, error) {
	var out []PRNote
	for page := 1; page <= maxForgePages; page++ {
		var list []struct {
			ID     int64  `json:"id"`
			Body   string `json:"body"`
			Type   string `json:"type"`
			System bool   `json:"system"`
			Author struct {
				Username string `json:"username"`
			} `json:"author"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/notes?per_page=100&page=%d", glProject(repo), number, page), nil, &list); err != nil {
			return nil, err
		}
		for _, n := range list {
			if n.System || n.Type == "DiffNote" {
				continue
			}
			out = append(out, PRNote{ID: strconv.FormatInt(n.ID, 10), Kind: "comment", Author: n.Author.Username, Body: n.Body,
				At: n.CreatedAt, UpdatedAt: n.UpdatedAt})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) ReactTo(ctx context.Context, f *Forge, repo string, number int, kind, commentID, emoji string) error {
	if f.Kind == "gitlab" {
		_, err := c.React(ctx, f, repo, number, commentID, emoji)
		return err
	}
	var path string
	switch kind {
	case "comment":
		path = fmt.Sprintf("/repos/%s/issues/comments/%s/reactions", repo, url.PathEscape(commentID))
	case "inline":
		path = fmt.Sprintf("/repos/%s/issues/comments/%s/reactions", repo, url.PathEscape(commentID))
		if f.Kind == "github" {
			path = fmt.Sprintf("/repos/%s/pulls/comments/%s/reactions", repo, url.PathEscape(commentID))
		}
	default:
		return ErrUnsupported
	}
	return c.do(ctx, f, http.MethodPost, path, map[string]string{"content": emoji}, nil)
}
