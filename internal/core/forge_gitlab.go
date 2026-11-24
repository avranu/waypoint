package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// glProject is the API path for a GitLab project.
func glProject(repo string) string { return "/projects/" + url.PathEscape(repo) }

// glState maps GitLab's "opened" to the open/closed words used elsewhere.
func glState(state string) string {
	if state == "opened" {
		return "open"
	}
	return state
}

// parseGitLabHook reads a GitLab webhook. Confidential issues and notes are
// never copied: they'd become visible to everyone in the Waypoint project.
// A confidential issue still arrives, blank and flagged, so a mirror of one
// that was public can be emptied.
func parseGitLabHook(body []byte) (*forgeEvent, error) {
	var p struct {
		ObjectKind string `json:"object_kind"` // issue, merge_request, note, push
		User       struct {
			Username string `json:"username"`
		} `json:"user"`
		Project struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
			DefaultBranch     string `json:"default_branch"`
		} `json:"project"`
		ObjectAttributes struct {
			ID             int64  `json:"id"`
			IID            int    `json:"iid"`
			Title          string `json:"title"`
			Description    string `json:"description"`
			State          string `json:"state"`
			URL            string `json:"url"`
			Action         string `json:"action"`
			SourceBranch   string `json:"source_branch"`
			Draft          bool   `json:"draft"`
			WorkInProgress bool   `json:"work_in_progress"`
			Confidential   bool   `json:"confidential"`
			Note           string `json:"note"`
			NoteableType   string `json:"noteable_type"`
			System         bool   `json:"system"`
			Internal       bool   `json:"internal"` // internal notes are for project members only
			TargetBranch   string `json:"target_branch"`
			CreatedAt      glTime `json:"created_at"`
			LastCommit     struct {
				ID string `json:"id"`
			} `json:"last_commit"`
			SHA            string `json:"sha"`              // pipeline events
			MergeCommitSHA string `json:"merge_commit_sha"` // merged merge requests
		} `json:"object_attributes"`
		MergeRequest *struct {
			IID int `json:"iid"`
		} `json:"merge_request"`
		Labels []struct {
			Title string `json:"title"`
		} `json:"labels"`
		// merge_request updates: reviewers before and after.
		Changes struct {
			Reviewers *struct {
				Previous []struct {
					Username string `json:"username"`
				} `json:"previous"`
				Current []struct {
					Username string `json:"username"`
				} `json:"current"`
			} `json:"reviewers"`
		} `json:"changes"`
		Issue *struct {
			IID          int  `json:"iid"`
			Confidential bool `json:"confidential"`
		} `json:"issue"`
		Commits []struct {
			ID        string   `json:"id"`
			Message   string   `json:"message"`
			URL       string   `json:"url"`
			Timestamp string   `json:"timestamp"`
			Added     []string `json:"added"`
			Removed   []string `json:"removed"`
			Modified  []string `json:"modified"`
		} `json:"commits"`
		// push
		Ref          string `json:"ref"`
		Before       string `json:"before"`
		After        string `json:"after"`
		UserUsername string `json:"user_username"`
		SHA          string `json:"sha"` // build events
		// release
		Action string `json:"action"`
		Tag    string `json:"tag"`
		Name   string `json:"name"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	a := p.ObjectAttributes
	ev := &forgeEvent{repo: p.Project.PathWithNamespace, repoID: p.Project.ID}
	switch p.ObjectKind {
	case "issue":
		ev.kind = "issue"
		if a.Confidential {
			ev.item = &ForgeItem{Number: a.IID, State: glState(a.State), URL: a.URL, Confidential: true}
			break
		}
		ev.item = &ForgeItem{Number: a.IID, Title: a.Title, Body: a.Description, State: glState(a.State), URL: a.URL, Author: p.User.Username}
		for _, l := range p.Labels {
			ev.item.Labels = append(ev.item.Labels, l.Title)
		}
	case "merge_request":
		state := glState(a.State)
		if state == "open" && (a.Draft || a.WorkInProgress) {
			state = "draft"
		}
		ev.kind, ev.branch = "pull", a.SourceBranch
		ev.item = &ForgeItem{Number: a.IID, Title: a.Title, Body: a.Description, State: state, URL: a.URL, IsPR: true}
		ev.pr = &ForgePR{Number: a.IID, Title: a.Title, Body: a.Description, URL: a.URL, State: state,
			BaseRef: a.TargetBranch, HeadRef: a.SourceBranch, HeadSHA: a.LastCommit.ID, OpenedAt: a.CreatedAt.ptr(), actor: p.User.Username}
		for _, l := range p.Labels {
			ev.pr.Labels = append(ev.pr.Labels, l.Title)
		}
		if rv := p.Changes.Reviewers; rv != nil {
			had := map[string]bool{}
			for _, u := range rv.Previous {
				had[strings.ToLower(u.Username)] = true
			}
			for _, u := range rv.Current {
				if u.Username != "" && !had[strings.ToLower(u.Username)] {
					ev.pr.requested = append(ev.pr.requested, u.Username)
				}
			}
		}
		ev.sender = p.User.Username
		if state == "merged" {
			ev.pr.MergeSHA = a.MergeCommitSHA
		}
		switch a.Action {
		case "merge":
			ev.pr.MergedBy = p.User.Username
		case "approved", "approval":
			ev.review = &forgeReviewEvent{number: a.IID, kind: "approved", sha: a.LastCommit.ID,
				ForgeComment: ForgeComment{ID: "gl-approved:" + p.User.Username + ":" + a.LastCommit.ID, Author: p.User.Username, URL: a.URL}}
		}
	case "note":
		if a.System || a.Internal || a.Confidential || (p.Issue != nil && p.Issue.Confidential) {
			return nil, nil
		}
		number := 0
		if p.Issue != nil {
			number = p.Issue.IID
		} else if p.MergeRequest != nil {
			number = p.MergeRequest.IID
		}
		action := "created"
		if a.Action == "update" {
			action = "edited"
		}
		ev.kind = "comment"
		ev.comment = &forgeCommentEvent{action: action, number: number, onPR: a.NoteableType != "Issue",
			ForgeComment: ForgeComment{ID: strconv.FormatInt(a.ID, 10), Author: p.User.Username, Body: a.Note, URL: a.URL}}
	case "push":
		ev.kind = "push"
		ev.ref, ev.before, ev.after, ev.pusher = p.Ref, p.Before, p.After, p.UserUsername
		ev.defaultBranch = p.Project.DefaultBranch
		ev.created = p.Before != "" && strings.Trim(p.Before, "0") == ""
		ev.deleted = p.After != "" && strings.Trim(p.After, "0") == ""
		ev.sender = p.UserUsername
		for _, c := range p.Commits {
			paths := append(append(slices.Clone(c.Added), c.Modified...), c.Removed...)
			ev.commits = append(ev.commits, forgeCommit{id: c.ID, message: c.Message, url: c.URL, at: c.Timestamp, paths: paths})
		}
	case "pipeline", "build":
		ev.kind, ev.sha = "ci", cmp(a.SHA, p.SHA)
	case "release":
		if p.Action != "create" {
			return nil, nil
		}
		ev.kind = "release"
		ev.release = &forgeRelease{tag: p.Tag, name: cmp(p.Name, p.Tag), url: p.URL}
	default:
		return nil, nil
	}
	return ev, nil
}

// glTime reads GitLab webhook dates, which older servers write as
// "2013-12-03 17:23:34 UTC" rather than RFC 3339.
type glTime struct{ t time.Time }

func (g *glTime) UnmarshalJSON(b []byte) error {
	v := strings.Trim(string(b), `"`)
	if v == "" || v == "null" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05.000-07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			g.t = t
			return nil
		}
	}
	return nil
}

func (g glTime) ptr() *time.Time {
	if g.t.IsZero() {
		return nil
	}
	t := g.t
	return &t
}

type glIssue struct {
	IID            int       `json:"iid"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	State          string    `json:"state"`
	WebURL         string    `json:"web_url"`
	Labels         []string  `json:"labels"`
	Draft          bool      `json:"draft"`
	WorkInProgress bool      `json:"work_in_progress"`
	Confidential   bool      `json:"confidential"`
	UpdatedAt      time.Time `json:"updated_at"`
	Author         struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (g glIssue) item(pr bool) *ForgeItem {
	if g.Confidential {
		return &ForgeItem{Number: g.IID, State: glState(g.State), URL: g.WebURL, IsPR: pr, UpdatedAt: g.UpdatedAt, Confidential: true}
	}
	it := &ForgeItem{Number: g.IID, Title: g.Title, Body: g.Description, State: glState(g.State), URL: g.WebURL,
		IsPR: pr, Labels: g.Labels, UpdatedAt: g.UpdatedAt, Author: g.Author.Username}
	if pr && it.State == "open" && (g.Draft || g.WorkInProgress) {
		it.State = "draft"
	}
	return it
}

func (c *httpForgeClient) glGetItem(ctx context.Context, f *Forge, repo string, number int, pr bool) (*ForgeItem, error) {
	kind := "issues"
	if pr {
		kind = "merge_requests"
	}
	var g glIssue
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/%s/%d", glProject(repo), kind, number), nil, &g); err != nil {
		return nil, err
	}
	return g.item(pr), nil
}

func (c *httpForgeClient) glListIssues(ctx context.Context, f *Forge, repo string, since time.Time) ([]ForgeItem, error) {
	var out []ForgeItem
	for page := 1; ; page++ {
		if page > maxForgePages {
			return nil, fmt.Errorf("%s has more than %d pages of issues; Waypoint syncs at most that many at once", repo, maxForgePages)
		}
		q := url.Values{"state": {"all"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}, "order_by": {"updated_at"}, "sort": {"asc"}}
		if !since.IsZero() {
			q.Set("updated_after", since.UTC().Format(time.RFC3339))
		}
		var list []glIssue
		if err := c.do(ctx, f, http.MethodGet, glProject(repo)+"/issues?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, g := range list {
			out = append(out, *g.item(false))
		}
		if len(list) < 100 {
			break
		}
	}
	slices.SortFunc(out, func(a, b ForgeItem) int { return a.Number - b.Number })
	return out, nil
}

func (c *httpForgeClient) glSetIssueState(ctx context.Context, f *Forge, repo string, number int, closed bool) error {
	event := "reopen"
	if closed {
		event = "close"
	}
	return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/issues/%d", glProject(repo), number), map[string]string{"state_event": event}, nil)
}

type glNote struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system"`
	// Internal (and the older Confidential) notes are hidden from all but
	// project members on GitLab; they never leave it.
	Internal     bool `json:"internal"`
	Confidential bool `json:"confidential"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c *httpForgeClient) glListComments(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error) {
	var out []ForgeComment
	for page := 1; page <= maxForgePages; page++ {
		var list []glNote
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"asc"}}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/issues/%d/notes?%s", glProject(repo), number, q.Encode()), nil, &list); err != nil {
			return nil, err
		}
		for _, n := range list {
			if !n.System && !n.Internal && !n.Confidential {
				out = append(out, ForgeComment{ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body, UpdatedAt: n.UpdatedAt})
			}
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) glPostComment(ctx context.Context, f *Forge, repo string, number int, body string) (*ForgeComment, error) {
	var n glNote
	if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/issues/%d/notes", glProject(repo), number), map[string]string{"body": body}, &n); err != nil {
		return nil, err
	}
	return &ForgeComment{ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body}, nil
}

func (c *httpForgeClient) glListRepos(ctx context.Context, f *Forge) ([]ForgeRepo, error) {
	var out []ForgeRepo
	for page := 1; page <= maxForgePages; page++ {
		var list []struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
			Namespace         struct {
				Kind     string `json:"kind"` // group or user
				FullPath string `json:"full_path"`
			} `json:"namespace"`
		}
		q := url.Values{"membership": {"true"}, "simple": {"true"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.do(ctx, f, http.MethodGet, "/projects?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, p := range list {
			out = append(out, ForgeRepo{ID: p.ID, Path: p.PathWithNamespace, Group: p.Namespace.Kind == "group"})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

// repoNamespace splits group/sub/name into the namespace and the name.
func repoNamespace(repo string) (ns, name string) {
	i := strings.LastIndexByte(repo, '/')
	if i < 0 {
		return "", repo
	}
	return repo[:i], repo[i+1:]
}
