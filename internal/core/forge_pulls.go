package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ForgePR is a pull request (GitLab: merge request) as its forge reports it.
type ForgePR struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	URL     string   `json:"url"`
	State   string   `json:"state"` // open, draft, closed, merged
	Author  string   `json:"author"`
	BaseRef string   `json:"base_ref"`
	HeadRef string   `json:"head_ref"`
	HeadSHA string   `json:"head_sha"`
	Labels  []string `json:"labels"`
	// Its life and size, when the forge says (nil: not told).
	OpenedAt     *time.Time `json:"opened_at,omitempty"`
	MergedAt     *time.Time `json:"merged_at,omitempty"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	MergedBy     string     `json:"merged_by,omitempty"`
	Additions    *int       `json:"additions,omitempty"`
	Deletions    *int       `json:"deletions,omitempty"`
	ChangedFiles *int       `json:"changed_files,omitempty"`
	Commits      *int       `json:"commits,omitempty"`
	// Mergeable: the forge says it merges cleanly (nil: not told, or still
	// working it out). HeadRepo is the repo the head branch lives in
	// (differs from the base's for a fork).
	Mergeable *bool  `json:"mergeable,omitempty"`
	HeadRepo  string `json:"head_repo,omitempty"`
	// MergeSHA is the commit a merged pull request became (WP-132).
	MergeSHA          string `json:"merge_sha,omitempty"`
	baseSHA, startSHA string
	sameRepo          bool
	actor             string
	requested         []string
	wipAuto           bool
	mergeMute         bool
}

// LineComment is one line comment of a review, on the new side of the diff.
type LineComment struct {
	Path string
	Line int
	Body string
}

// PostedReview is what a forge returned for a review it accepted.
type PostedReview struct {
	ID  string
	URL string
}

// ForgePRs is what bots need from a forge; httpForgeClient implements it
// for GitHub, Forgejo and GitLab.
type ForgePRs interface {
	GetPR(ctx context.Context, f *Forge, repo string, number int) (*ForgePR, error)
	// PRDiff is the pull request's unified diff against its base.
	PRDiff(ctx context.Context, f *Forge, repo string, pr *ForgePR) (string, error)
	// FileAt reads a file at a commit; ErrNotFound if it isn't there.
	FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
	// ReviewBodies are the pull request's reviews and conversation
	// comments (author and text), newest last, for spotting the bot's
	// markers.
	ReviewBodies(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error)
	// PostReview posts a COMMENT review at sha with line comments.
	PostReview(ctx context.Context, f *Forge, repo string, pr *ForgePR, body string, comments []LineComment) (*PostedReview, error)
	// React adds emoji (eyes, +1, -1, confused, rocket, heart) to the pull
	// request, or to one of its comments when commentID is set, and returns
	// an id to remove it by.
	React(ctx context.Context, f *Forge, repo string, number int, commentID, emoji string) (string, error)
	Unreact(ctx context.Context, f *Forge, repo string, number int, commentID, emoji, id string) error
	// PostPRComment posts a conversation comment (GitLab: a merge request note).
	PostPRComment(ctx context.Context, f *Forge, repo string, number int, body string) (*ForgeComment, error)
}

// ForgePRLister lists a repo's recently updated pull requests, so their
// records stay current when webhooks are missed (httpForgeClient has it).
type ForgePRLister interface {
	ListPRs(ctx context.Context, f *Forge, repo string) ([]*ForgePR, error)
}

// ForgeHeadPRFinder lists the open pull requests from one branch, as the
// forge filters or pages them, so a pull request isn't missed in a busy repo
// that ListPRs' one page doesn't reach (httpForgeClient has it).
type ForgeHeadPRFinder interface {
	OpenPRsFromHead(ctx context.Context, f *Forge, repo, head string) ([]*ForgePR, error)
}

// OpenPRsFromHead is the open pull requests from branch head of repo: GitLab
// and GitHub filter on the branch; Forgejo is paged through its open ones.
func (c *httpForgeClient) OpenPRsFromHead(ctx context.Context, f *Forge, repo, head string) ([]*ForgePR, error) {
	var out []*ForgePR
	switch f.Kind {
	case "gitlab":
		var list []glMR
		if err := c.do(ctx, f, http.MethodGet, glProject(repo)+"/merge_requests?state=opened&per_page=50&source_branch="+url.QueryEscape(head), nil, &list); err != nil {
			return nil, err
		}
		for _, m := range list {
			out = append(out, m.pr())
		}
		return out, nil
	case "github":
		owner, _, _ := strings.Cut(repo, "/")
		var list []apiPR
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=open&per_page=50&head=%s", repo, url.QueryEscape(owner+":"+head)), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			out = append(out, a.pr())
		}
		return out, nil
	}
	for page := 1; page <= maxForgePages; page++ {
		var list []apiPR
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=open&limit=50&page=%d", repo, page), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			if p := a.pr(); p.HeadRef == head {
				out = append(out, p)
			}
		}
		if len(list) < 50 {
			break
		}
	}
	return out, nil
}

// raw fetches a non-JSON body (a diff, a file) up to limit bytes.
func (c *httpForgeClient) raw(ctx context.Context, f *Forge, path, accept string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase(f)+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", accept)
	if f.token != "" {
		switch f.Kind {
		case "github":
			req.Header.Set("Authorization", "Bearer "+f.token)
		case "gitlab":
			req.Header.Set("PRIVATE-TOKEN", f.token)
		default:
			req.Header.Set("Authorization", "token "+f.token)
		}
	}
	resp, err := c.send(f, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &forgeAPIError{method: http.MethodGet, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", errDiffTooLarge
	}
	return string(b), nil
}

var errDiffTooLarge = errors.New("the diff is too large to review")

// maxDiffBytes bounds a diff Waypoint downloads; reviews use far less.
const maxDiffBytes = 8 << 20

type apiPR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	User    struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Mergeable      *bool  `json:"mergeable"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	CreatedAt *time.Time `json:"created_at"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedBy  *struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	} `json:"merged_by"`
	// Only GitHub's single pull request (not its list) and Forgejo say these.
	Additions    *int `json:"additions"`
	Deletions    *int `json:"deletions"`
	ChangedFiles *int `json:"changed_files"`
	Commits      *int `json:"commits"`
}

// draftTitle: Forgejo marks work in progress by title.
func draftTitle(t string) bool {
	l := strings.ToLower(strings.TrimSpace(t))
	return strings.HasPrefix(l, "wip:") || strings.HasPrefix(l, "[wip]") || strings.HasPrefix(l, "draft:")
}

func (a apiPR) pr() *ForgePR {
	p := &ForgePR{Number: a.Number, Title: a.Title, Body: a.Body, URL: a.HTMLURL, State: a.State,
		Author: cmp(a.User.Login, a.User.Username), BaseRef: a.Base.Ref, HeadRef: a.Head.Ref, HeadSHA: a.Head.SHA}
	switch {
	case a.Merged || (a.State == "closed" && a.MergedAt != nil):
		p.State = "merged"
	case a.State == "open" && (a.Draft || draftTitle(a.Title)):
		p.State = "draft"
	}
	for _, l := range a.Labels {
		p.Labels = append(p.Labels, l.Name)
	}
	p.OpenedAt, p.MergedAt, p.ClosedAt = a.CreatedAt, a.MergedAt, a.ClosedAt
	if a.MergedBy != nil {
		p.MergedBy = cmp(a.MergedBy.Login, a.MergedBy.Username)
	}
	p.Additions, p.Deletions, p.ChangedFiles, p.Commits = a.Additions, a.Deletions, a.ChangedFiles, a.Commits
	p.Mergeable = a.Mergeable
	if draftTitle(a.Title) {
		p.Mergeable, p.mergeMute = nil, true
	}
	if p.State == "merged" {
		p.MergeSHA = a.MergeCommitSHA
	}
	if a.Head.Repo != nil {
		p.HeadRepo = a.Head.Repo.FullName
	}
	return p
}

// headRepoOf is the repo fp's head lives in, ” when the forge didn't say.
func headRepoOf(fp *ForgePR, repo string) string {
	if fp.HeadRepo == "" && fp.sameRepo {
		return repo
	}
	return fp.HeadRepo
}

func (c *httpForgeClient) GetPR(ctx context.Context, f *Forge, repo string, number int) (*ForgePR, error) {
	if f.Kind == "gitlab" {
		return c.glGetPR(ctx, f, repo, number)
	}
	var a apiPR
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &a); err != nil {
		return nil, err
	}
	return a.pr(), nil
}

func (c *httpForgeClient) PRDiff(ctx context.Context, f *Forge, repo string, pr *ForgePR) (string, error) {
	switch f.Kind {
	case "github":
		return c.raw(ctx, f, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr.Number), "application/vnd.github.v3.diff", maxDiffBytes)
	case "gitlab":
		return c.glDiff(ctx, f, repo, pr.Number)
	}
	return c.raw(ctx, f, fmt.Sprintf("/repos/%s/pulls/%d.diff", repo, pr.Number), "text/plain", maxDiffBytes)
}

// maxRepoFile bounds config and instruction files read from a repo.
const maxRepoFile = 256 << 10

func (c *httpForgeClient) FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error) {
	esc := strings.Join(mapStrings(strings.Split(path, "/"), url.PathEscape), "/")
	switch f.Kind {
	case "github":
		return c.raw(ctx, f, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, esc, url.QueryEscape(ref)), "application/vnd.github.raw", maxRepoFile)
	case "gitlab":
		return c.raw(ctx, f, fmt.Sprintf("%s/repository/files/%s/raw?ref=%s", glProject(repo), url.PathEscape(path), url.QueryEscape(ref)), "text/plain", maxRepoFile)
	}
	return c.raw(ctx, f, fmt.Sprintf("/repos/%s/raw/%s?ref=%s", repo, esc, url.QueryEscape(ref)), "text/plain", maxRepoFile)
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for n, s := range in {
		out[n] = f(s)
	}
	return out
}

func (c *httpForgeClient) ReviewBodies(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error) {
	if f.Kind == "gitlab" {
		return c.glListMRNotes(ctx, f, repo, number)
	}
	var out []ForgeComment
	q := "per_page=100"
	if f.Kind != "github" {
		q = "limit=50"
	}
	var reviews []struct {
		Body  string `json:"body"`
		State string `json:"state"`
		User  struct {
			Login    string `json:"login"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews?%s", repo, number, q), nil, &reviews); err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if strings.EqualFold(r.State, "PENDING") {
			continue
		}
		out = append(out, ForgeComment{Author: cmp(r.User.Login, r.User.Username), Body: r.Body})
	}
	cs, err := c.ListComments(ctx, f, repo, number)
	if err != nil {
		return nil, err
	}
	return append(out, cs...), nil
}

func (c *httpForgeClient) PostReview(ctx context.Context, f *Forge, repo string, pr *ForgePR, body string, comments []LineComment) (*PostedReview, error) {
	if f.Kind == "gitlab" {
		return c.glPostReview(ctx, f, repo, pr, body, comments)
	}
	lines := make([]map[string]any, 0, len(comments))
	for _, cm := range comments {
		if f.Kind == "github" {
			lines = append(lines, map[string]any{"path": cm.Path, "line": cm.Line, "side": "RIGHT", "body": cm.Body})
		} else {
			lines = append(lines, map[string]any{"path": cm.Path, "new_position": cm.Line, "old_position": 0, "body": cm.Body})
		}
	}
	var res struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr.Number), map[string]any{
		"commit_id": pr.HeadSHA, "event": "COMMENT", "body": body, "comments": lines,
	}, &res)
	if err != nil {
		return nil, err
	}
	u := res.HTMLURL
	if u == "" {
		u = pr.URL
	}
	return &PostedReview{ID: strconv.FormatInt(res.ID, 10), URL: u}, nil
}

func (c *httpForgeClient) React(ctx context.Context, f *Forge, repo string, number int, commentID, emoji string) (string, error) {
	if f.Kind == "gitlab" {
		path := fmt.Sprintf("%s/merge_requests/%d/award_emoji", glProject(repo), number)
		if commentID != "" {
			path = fmt.Sprintf("%s/merge_requests/%d/notes/%s/award_emoji", glProject(repo), number, url.PathEscape(commentID))
		}
		var res struct {
			ID int64 `json:"id"`
		}
		if err := c.do(ctx, f, http.MethodPost, path, map[string]string{"name": glEmoji(emoji)}, &res); err != nil {
			return "", err
		}
		return strconv.FormatInt(res.ID, 10), nil
	}
	if f.Kind != "github" {
		_ = c.Unreact(ctx, f, repo, number, commentID, emoji, "")
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/reactions", repo, number)
	if commentID != "" {
		path = fmt.Sprintf("/repos/%s/issues/comments/%s/reactions", repo, url.PathEscape(commentID))
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, f, http.MethodPost, path, map[string]string{"content": emoji}, &res); err != nil {
		return "", err
	}
	return strconv.FormatInt(res.ID, 10), nil
}

func (c *httpForgeClient) Unreact(ctx context.Context, f *Forge, repo string, number int, commentID, emoji, id string) error {
	switch f.Kind {
	case "gitlab":
		path := fmt.Sprintf("%s/merge_requests/%d/award_emoji/%s", glProject(repo), number, url.PathEscape(id))
		if commentID != "" {
			path = fmt.Sprintf("%s/merge_requests/%d/notes/%s/award_emoji/%s", glProject(repo), number, url.PathEscape(commentID), url.PathEscape(id))
		}
		return c.do(ctx, f, http.MethodDelete, path, nil, nil)
	case "github":
		path := fmt.Sprintf("/repos/%s/issues/%d/reactions/%s", repo, number, url.PathEscape(id))
		if commentID != "" {
			path = fmt.Sprintf("/repos/%s/issues/comments/%s/reactions/%s", repo, url.PathEscape(commentID), url.PathEscape(id))
		}
		return c.do(ctx, f, http.MethodDelete, path, nil, nil)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/reactions", repo, number)
	if commentID != "" {
		path = fmt.Sprintf("/repos/%s/issues/comments/%s/reactions", repo, url.PathEscape(commentID))
	}
	return c.do(ctx, f, http.MethodDelete, path, map[string]string{"content": emoji}, nil)
}

func glEmoji(e string) string {
	switch e {
	case "+1":
		return "thumbsup"
	case "-1":
		return "thumbsdown"
	case "hooray":
		return "tada"
	case "rocket", "heart", "confused", "eyes":
		return e
	}
	return e
}

type glMR struct {
	// The projects the source and target branches live in (a fork's differ).
	SourceProjectID int64    `json:"source_project_id"`
	TargetProjectID int64    `json:"target_project_id"`
	IID             int      `json:"iid"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	State           string   `json:"state"`
	Draft           bool     `json:"draft"`
	WebURL          string   `json:"web_url"`
	SourceBranch    string   `json:"source_branch"`
	TargetBranch    string   `json:"target_branch"`
	SHA             string   `json:"sha"`
	Labels          []string `json:"labels"`
	Author          struct {
		Username string `json:"username"`
	} `json:"author"`
	CreatedAt *time.Time `json:"created_at"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	// merge_user replaced merged_by (GitLab 14.7); older servers send only the latter.
	MergeUser *struct {
		Username string `json:"username"`
	} `json:"merge_user"`
	MergedBy *struct {
		Username string `json:"username"`
	} `json:"merged_by"`
	MergeStatus     string `json:"merge_status"` // can_be_merged, cannot_be_merged, checking, unchecked
	MergeCommitSHA  string `json:"merge_commit_sha"`
	SquashCommitSHA string `json:"squash_commit_sha"`
	ChangesCount    string `json:"changes_count"` // "12", or "1000+"
	DiffRefs        struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (c *httpForgeClient) glGetPR(ctx context.Context, f *Forge, repo string, number int) (*ForgePR, error) {
	var m glMR
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), nil, &m); err != nil {
		return nil, err
	}
	return m.pr(), nil
}

func (m glMR) pr() *ForgePR {
	state := glState(m.State)
	if state == "open" && m.Draft {
		state = "draft"
	}
	head := cmp(m.DiffRefs.HeadSHA, m.SHA)
	p := &ForgePR{Number: m.IID, Title: m.Title, Body: m.Description, URL: m.WebURL, State: state,
		Author: m.Author.Username, BaseRef: m.TargetBranch, HeadRef: m.SourceBranch, HeadSHA: head, Labels: m.Labels,
		OpenedAt: m.CreatedAt, MergedAt: m.MergedAt, ClosedAt: m.ClosedAt,
		baseSHA: m.DiffRefs.BaseSHA, startSHA: m.DiffRefs.StartSHA,
		sameRepo: m.SourceProjectID != 0 && m.SourceProjectID == m.TargetProjectID}
	switch {
	case m.MergeUser != nil:
		p.MergedBy = m.MergeUser.Username
	case m.MergedBy != nil:
		p.MergedBy = m.MergedBy.Username
	}
	if state == "merged" {
		p.MergeSHA = cmp(m.MergeCommitSHA, m.SquashCommitSHA)
	}
	if n, err := strconv.Atoi(m.ChangesCount); err == nil {
		p.ChangedFiles = &n
	}
	switch m.MergeStatus {
	case "can_be_merged":
		p.Mergeable = ptr(true)
	case "cannot_be_merged", "cannot_be_merged_recheck":
		p.Mergeable = ptr(false)
	}
	return p
}

// ListPRs lists a repo's most recently updated pull requests, open or not,
// for keeping their records current between webhooks.
func (c *httpForgeClient) ListPRs(ctx context.Context, f *Forge, repo string) ([]*ForgePR, error) {
	var out []*ForgePR
	switch f.Kind {
	case "gitlab":
		var list []glMR
		if err := c.do(ctx, f, http.MethodGet, glProject(repo)+"/merge_requests?state=all&order_by=updated_at&sort=desc&per_page=50", nil, &list); err != nil {
			return nil, err
		}
		for _, m := range list {
			out = append(out, m.pr())
		}
		return out, nil
	case "github":
		var list []apiPR
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=all&sort=updated&direction=desc&per_page=50", repo), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			out = append(out, a.pr())
		}
		return out, nil
	}
	var list []apiPR
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=all&sort=recentupdate&limit=50", repo), nil, &list); err != nil {
		return nil, err
	}
	for _, a := range list {
		out = append(out, a.pr())
	}
	return out, nil
}

// glDiff rebuilds a unified diff from the merge request's per-file diffs.
func (c *httpForgeClient) glDiff(ctx context.Context, f *Forge, repo string, number int) (string, error) {
	var b strings.Builder
	for page := 1; page <= maxForgePages; page++ {
		var files []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			NewFile     bool   `json:"new_file"`
			DeletedFile bool   `json:"deleted_file"`
			Diff        string `json:"diff"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/diffs?per_page=100&page=%d", glProject(repo), number, page), nil, &files); err != nil {
			return "", err
		}
		for _, d := range files {
			old, nw := "a/"+d.OldPath, "b/"+d.NewPath
			if d.NewFile {
				old = "/dev/null"
			}
			if d.DeletedFile {
				nw = "/dev/null"
			}
			fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- %s\n+++ %s\n%s", d.OldPath, d.NewPath, old, nw, d.Diff)
			if !strings.HasSuffix(d.Diff, "\n") {
				b.WriteByte('\n')
			}
			if b.Len() > maxDiffBytes {
				return "", errDiffTooLarge
			}
		}
		if len(files) < 100 {
			break
		}
	}
	return b.String(), nil
}

func (c *httpForgeClient) glListMRNotes(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error) {
	var out []ForgeComment
	for page := 1; page <= maxForgePages; page++ {
		var list []glNote
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/notes?per_page=100&page=%d", glProject(repo), number, page), nil, &list); err != nil {
			return nil, err
		}
		for _, n := range list {
			out = append(out, ForgeComment{ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

// glPostReview posts the summary as a note and each line comment as a
// discussion on the diff (GitLab has no single review object to post).
func (c *httpForgeClient) glPostReview(ctx context.Context, f *Forge, repo string, pr *ForgePR, body string, comments []LineComment) (*PostedReview, error) {
	for _, cm := range comments {
		err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/discussions", glProject(repo), pr.Number), map[string]any{
			"body": cm.Body,
			"position": map[string]any{
				"position_type": "text", "base_sha": pr.baseSHA, "start_sha": pr.startSHA, "head_sha": pr.HeadSHA,
				"new_path": cm.Path, "old_path": cm.Path, "new_line": cm.Line,
			},
		}, nil)
		if err != nil {
			return nil, err
		}
	}
	var n glNote
	if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/notes", glProject(repo), pr.Number), map[string]string{"body": body}, &n); err != nil {
		return nil, err
	}
	return &PostedReview{ID: strconv.FormatInt(n.ID, 10), URL: pr.URL + "#note_" + strconv.FormatInt(n.ID, 10)}, nil
}

func (c *httpForgeClient) PostPRComment(ctx context.Context, f *Forge, repo string, number int, body string) (*ForgeComment, error) {
	if f.Kind != "gitlab" {
		return c.PostComment(ctx, f, repo, number, body)
	}
	var n glNote
	if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/notes", glProject(repo), number), map[string]string{"body": body}, &n); err != nil {
		return nil, err
	}
	return &ForgeComment{ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body, UpdatedAt: time.Now()}, nil
}
