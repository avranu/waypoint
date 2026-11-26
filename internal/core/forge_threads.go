package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported: the forge has no API for this (Forgejo can't resolve a
// thread).
var ErrUnsupported = errors.New("not supported by this forge")

// ThreadComment is one comment in a review thread.
type ThreadComment struct {
	ID     string    `json:"id"`
	Author string    `json:"author"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
	SHA    string    `json:"sha,omitempty"` // the commit it was made on
}

// ReviewThread is a conversation on a line of a pull request's diff.
type ReviewThread struct {
	// ID is the forge's id for the thread: GitHub's node id, GitLab's
	// discussion id; Forgejo has none, so it is the first comment's id.
	ID         string          `json:"id"`
	Path       string          `json:"path"`
	Line       int             `json:"line"` // on the new side; 0 when the forge no longer places it
	Resolved   bool            `json:"resolved"`
	ResolvedBy string          `json:"resolved_by,omitempty"`
	Outdated   bool            `json:"outdated,omitempty"`
	Comments   []ThreadComment `json:"comments"` // oldest first
}

// root is the thread's first comment.
func (t *ReviewThread) root() ThreadComment {
	if len(t.Comments) == 0 {
		return ThreadComment{}
	}
	return t.Comments[0]
}

// ForgeThreads is the optional part of a ForgeClient that reads and answers
// review threads (httpForgeClient implements it for GitHub, Forgejo and
// GitLab).
type ForgeThreads interface {
	// ReviewThreads lists the pull request's inline threads with their
	// comments and whether each is resolved.
	ReviewThreads(ctx context.Context, f *Forge, repo string, number int) ([]ReviewThread, error)
	// ReplyToThread adds a comment to a thread and returns its id.
	ReplyToThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread, body string) (string, error)
	// ResolveThread marks a thread resolved; ErrUnsupported where the
	// forge has no API for it.
	ResolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error
	// UnresolveThread reopens a resolved thread; ErrUnsupported likewise.
	UnresolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error
	// CommentReactions lists the emoji on a review comment.
	CommentReactions(ctx context.Context, f *Forge, repo string, number int, commentID string) ([]Reaction, error)
}

// CommentReactions lists the emoji people put on a review comment (to read
// a 👎 on a finding).
func (c *httpForgeClient) CommentReactions(ctx context.Context, f *Forge, repo string, number int, commentID string) ([]Reaction, error) {
	var out []Reaction
	if f.Kind == "gitlab" {
		var list []struct {
			Name string `json:"name"`
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/notes/%s/award_emoji?per_page=100", glProject(repo), number, url.PathEscape(commentID)), nil, &list); err != nil {
			return nil, err
		}
		for _, r := range list {
			emoji := cmp(map[string]string{"thumbsdown": "-1", "thumbsup": "+1"}[r.Name], r.Name)
			out = append(out, Reaction{Emoji: emoji, User: r.User.Username})
		}
		return out, nil
	}
	path := fmt.Sprintf("/repos/%s/issues/comments/%s/reactions", repo, url.PathEscape(commentID))
	if f.Kind == "github" {
		path = fmt.Sprintf("/repos/%s/pulls/comments/%s/reactions?per_page=100", repo, url.PathEscape(commentID))
	}
	var list []struct {
		Content string   `json:"content"`
		User    hookUser `json:"user"`
	}
	if err := c.do(ctx, f, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	for _, r := range list {
		out = append(out, Reaction{Emoji: r.Content, User: r.User.name()})
	}
	return out, nil
}

func (c *httpForgeClient) ReviewThreads(ctx context.Context, f *Forge, repo string, number int) ([]ReviewThread, error) {
	switch f.Kind {
	case "github":
		return c.ghThreads(ctx, f, repo, number)
	case "gitlab":
		return c.glThreads(ctx, f, repo, number)
	}
	return c.fgThreads(ctx, f, repo, number)
}

func (c *httpForgeClient) ReplyToThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread, body string) (string, error) {
	switch f.Kind {
	case "github":
		var res struct {
			ID int64 `json:"id"`
		}
		err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/comments/%s/replies", repo, pr.Number, url.PathEscape(th.root().ID)), map[string]string{"body": body}, &res)
		return strconv.FormatInt(res.ID, 10), err
	case "gitlab":
		var n glNote
		err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/discussions/%s/notes", glProject(repo), pr.Number, url.PathEscape(th.ID)), map[string]string{"body": body}, &n)
		return strconv.FormatInt(n.ID, 10), err
	}
	if th.Path == "" || th.Line <= 0 {
		return "", ErrUnsupported
	}
	var res struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr.Number), map[string]any{
		"commit_id": cmp(th.root().SHA, pr.HeadSHA), "event": "COMMENT",
		"comments": []map[string]any{{"path": th.Path, "new_position": th.Line, "old_position": 0, "body": body}},
	}, &res)
	if err != nil {
		return "", err
	}
	cs, err := c.fgReviewComments(ctx, f, repo, pr.Number, res.ID)
	if err == nil && len(cs) > 0 {
		return strconv.FormatInt(cs[len(cs)-1].ID, 10), nil
	}
	return "", nil
}

func (c *httpForgeClient) ResolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error {
	switch f.Kind {
	case "github":
		return c.graphql(ctx, f, `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`, map[string]any{"id": th.ID}, nil)
	case "gitlab":
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d/discussions/%s?resolved=true", glProject(repo), pr.Number, url.PathEscape(th.ID)), nil, nil)
	}
	return ErrUnsupported
}

// UnresolveThread reopens a thread. Forgejo 16 (Gitea 1.22) has no API for
// resolving or reopening a conversation (only the web page's form does), so
// there Waypoint keeps the resolution itself (WP-130).
func (c *httpForgeClient) UnresolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error {
	switch f.Kind {
	case "github":
		return c.graphql(ctx, f, `mutation($id:ID!){unresolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`, map[string]any{"id": th.ID}, nil)
	case "gitlab":
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d/discussions/%s?resolved=false", glProject(repo), pr.Number, url.PathEscape(th.ID)), nil, nil)
	}
	return ErrUnsupported
}

type fgReviewComment struct {
	ID         int64     `json:"id"`
	Body       string    `json:"body"`
	Path       string    `json:"path"`
	Position   int       `json:"position"` // the line on the new side; 0 once outdated
	OrigPos    int       `json:"original_position"`
	CommitID   string    `json:"commit_id"`
	OrigCommit string    `json:"original_commit_id"`
	CreatedAt  time.Time `json:"created_at"`
	User       hookUser  `json:"user"`
	Resolver   *hookUser `json:"resolver"`
}

func (c *httpForgeClient) fgReviewComments(ctx context.Context, f *Forge, repo string, number int, reviewID int64) ([]fgReviewComment, error) {
	var out []fgReviewComment
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/comments", repo, number, reviewID), nil, &out)
	return out, err
}

// fgThreads groups a Forgejo pull request's review comments into threads:
// comments on the same line of the same file are one conversation.
func (c *httpForgeClient) fgThreads(ctx context.Context, f *Forge, repo string, number int) ([]ReviewThread, error) {
	var comments []fgReviewComment
	for page := 1; page <= maxForgePages; page++ {
		var reviews []struct {
			ID            int64  `json:"id"`
			State         string `json:"state"`
			CommentsCount int    `json:"comments_count"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews?limit=50&page=%d", repo, number, page), nil, &reviews); err != nil {
			return nil, err
		}
		for _, r := range reviews {
			if r.CommentsCount == 0 || strings.EqualFold(r.State, "PENDING") {
				continue
			}
			cs, err := c.fgReviewComments(ctx, f, repo, number, r.ID)
			if err != nil {
				return nil, err
			}
			comments = append(comments, cs...)
		}
		if len(reviews) < 50 {
			break
		}
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if !comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].CreatedAt.Before(comments[j].CreatedAt)
		}
		return comments[i].ID < comments[j].ID
	})
	type key struct {
		path string
		line int
	}
	index := map[key]int{}
	var out []ReviewThread
	for _, cm := range comments {
		line := cm.Position
		if line == 0 {
			line = cm.OrigPos
		}
		k := key{cm.Path, line}
		n, ok := index[k]
		if !ok {
			n = len(out)
			index[k] = n
			out = append(out, ReviewThread{ID: strconv.FormatInt(cm.ID, 10), Path: cm.Path, Line: cm.Position, Outdated: cm.Position == 0})
		}
		t := &out[n]
		if cm.Resolver != nil {
			t.Resolved, t.ResolvedBy = true, cm.Resolver.name()
		}
		t.Comments = append(t.Comments, ThreadComment{ID: strconv.FormatInt(cm.ID, 10), Author: cm.User.name(), Body: cm.Body, At: cm.CreatedAt,
			SHA: cmp(cm.OrigCommit, cm.CommitID)})
	}
	return out, nil
}

// graphql runs a GraphQL query on GitHub (reviewThreads and resolving them
// have no REST API).
func (c *httpForgeClient) graphql(ctx context.Context, f *Forge, query string, vars map[string]any, out any) error {
	endpoint := "https://api.github.com/graphql"
	if strings.TrimRight(f.BaseURL, "/") != "https://github.com" {
		endpoint = strings.TrimRight(f.BaseURL, "/") + "/api/graphql"
	}
	b, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := c.send(f, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &forgeAPIError{method: http.MethodPost, path: "/graphql", status: resp.StatusCode, body: strings.TrimSpace(string(eb))}
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL: %s", res.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Data, out)
}

const ghThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){
reviewThreads(first:100,after:$after){pageInfo{hasNextPage endCursor}nodes{id isResolved isOutdated path line originalLine resolvedBy{login}
comments(first:50){nodes{databaseId author{login} body createdAt commit{oid}}}}}}}}`

func (c *httpForgeClient) ghThreads(ctx context.Context, f *Forge, repo string, number int) ([]ReviewThread, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var out []ReviewThread
	after := ""
	for range maxForgePages {
		vars := map[string]any{"owner": owner, "name": name, "number": number}
		if after != "" {
			vars["after"] = after
		}
		var res struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							IsOutdated bool   `json:"isOutdated"`
							Path       string `json:"path"`
							Line       *int   `json:"line"`
							OrigLine   *int   `json:"originalLine"`
							ResolvedBy *struct {
								Login string `json:"login"`
							} `json:"resolvedBy"`
							Comments struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
									Author     *struct {
										Login string `json:"login"`
									} `json:"author"`
									Body      string    `json:"body"`
									CreatedAt time.Time `json:"createdAt"`
									Commit    *struct {
										OID string `json:"oid"`
									} `json:"commit"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, f, ghThreadsQuery, vars, &res); err != nil {
			return nil, err
		}
		rt := res.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			t := ReviewThread{ID: n.ID, Path: n.Path, Resolved: n.IsResolved, Outdated: n.IsOutdated}
			if n.Line != nil {
				t.Line = *n.Line
			} else if n.OrigLine != nil {
				t.Line = *n.OrigLine
			}
			if n.ResolvedBy != nil {
				t.ResolvedBy = n.ResolvedBy.Login
			}
			for _, cm := range n.Comments.Nodes {
				tc := ThreadComment{ID: strconv.FormatInt(cm.DatabaseID, 10), Body: cm.Body, At: cm.CreatedAt}
				if cm.Author != nil {
					tc.Author = cm.Author.Login
				}
				if cm.Commit != nil {
					tc.SHA = cm.Commit.OID
				}
				t.Comments = append(t.Comments, tc)
			}
			out = append(out, t)
		}
		if !rt.PageInfo.HasNextPage || rt.PageInfo.EndCursor == "" {
			break
		}
		after = rt.PageInfo.EndCursor
	}
	return out, nil
}

func (c *httpForgeClient) glThreads(ctx context.Context, f *Forge, repo string, number int) ([]ReviewThread, error) {
	var out []ReviewThread
	for page := 1; page <= maxForgePages; page++ {
		var list []struct {
			ID             string `json:"id"`
			IndividualNote bool   `json:"individual_note"`
			Notes          []struct {
				ID         int64 `json:"id"`
				Body       string
				System     bool      `json:"system"`
				Resolvable bool      `json:"resolvable"`
				Resolved   bool      `json:"resolved"`
				CreatedAt  time.Time `json:"created_at"`
				Author     struct {
					Username string `json:"username"`
				} `json:"author"`
				ResolvedBy *struct {
					Username string `json:"username"`
				} `json:"resolved_by"`
				Position *struct {
					NewPath string `json:"new_path"`
					OldPath string `json:"old_path"`
					NewLine *int   `json:"new_line"`
					OldLine *int   `json:"old_line"`
					HeadSHA string `json:"head_sha"`
				} `json:"position"`
			} `json:"notes"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/discussions?per_page=100&page=%d", glProject(repo), number, page), nil, &list); err != nil {
			return nil, err
		}
		for _, d := range list {
			if d.IndividualNote || len(d.Notes) == 0 || d.Notes[0].Position == nil {
				continue
			}
			first := d.Notes[0]
			t := ReviewThread{ID: d.ID, Path: cmp(first.Position.NewPath, first.Position.OldPath), Resolved: first.Resolvable && first.Resolved}
			if first.Position.NewLine != nil {
				t.Line = *first.Position.NewLine
			}
			if first.ResolvedBy != nil {
				t.ResolvedBy = first.ResolvedBy.Username
			}
			for _, n := range d.Notes {
				if n.System {
					continue
				}
				sha := ""
				if n.Position != nil {
					sha = n.Position.HeadSHA
				}
				t.Comments = append(t.Comments, ThreadComment{ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body, At: n.CreatedAt, SHA: sha})
			}
			if len(t.Comments) == 0 {
				continue
			}
			out = append(out, t)
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}
