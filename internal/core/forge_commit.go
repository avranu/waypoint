package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// CommitInput is one commit of several files on a new branch.
type CommitInput struct {
	Base    string       // the branch to start from
	Branch  string       // the new branch
	Message string       // commit message
	Files   []FileChange // BaseSHA is checked by the caller (see ForgeCommitter.FileBlobSHA)
}

// ForgeCommit is what a commit made.
type ForgeCommit struct {
	SHA    string
	Branch string
}

// NewPR opens a pull request (GitLab: merge request).
type NewPR struct {
	Title string
	Body  string
	Head  string
	Base  string
	// Draft opens it as a draft: a flag on GitHub, a title prefix on Forgejo
	// ("WIP: ") and GitLab ("Draft: ").
	Draft bool
	// Labels it opens with, by name; the repo's missing ones are made.
	// Forgejo, Gitea and GitLab label it in the call that opens it, so its
	// first CI run already sees them (ci-urgent, WP-528); GitHub right after.
	Labels []string
}

// ForgeCommitter is what a forge client needs to commit files and open a
// pull request.
type ForgeCommitter interface {
	DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error)
	// FileBlobSHA is the git blob id of a file at a ref; "" if it isn't there.
	FileBlobSHA(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
	// CommitFiles creates in.Branch from in.Base with one commit.
	CommitFiles(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error)
	CreatePR(ctx context.Context, f *Forge, repo string, in NewPR) (*ForgePR, error)
}

// ChangeConflict: files changed on the forge since a change was made
// against them. Nothing was written.
type ChangeConflict struct{ Paths []string }

func (e *ChangeConflict) Error() string {
	return "changed since the agent made the change: " + strings.Join(e.Paths, ", ")
}
func (e *ChangeConflict) Unwrap() error { return ErrConflict }

func escPath(p string) string {
	return strings.Join(mapStrings(strings.Split(p, "/"), url.PathEscape), "/")
}

func (c *httpForgeClient) DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := "/repos/" + repo
	if f.Kind == "gitlab" {
		path = glProject(repo)
	}
	if err := c.do(ctx, f, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("the forge didn't say the repository's default branch")
	}
	return out.DefaultBranch, nil
}

func (c *httpForgeClient) FileBlobSHA(ctx context.Context, f *Forge, repo, path, ref string) (string, error) {
	var out struct {
		SHA    string `json:"sha"`
		BlobID string `json:"blob_id"`
		Type   string `json:"type"`
	}
	var err error
	if f.Kind == "gitlab" {
		err = c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/repository/files/%s?ref=%s", glProject(repo), url.PathEscape(path), url.QueryEscape(ref)), nil, &out)
	} else {
		err = c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escPath(path), url.QueryEscape(ref)), nil, &out)
	}
	if isStatus(err, http.StatusNotFound) {
		return "", nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "cannot unmarshal array") {
			return "", fmt.Errorf("%s is a directory", path)
		}
		return "", err
	}
	return cmp(out.BlobID, out.SHA), nil
}

func (c *httpForgeClient) CommitFiles(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error) {
	if len(in.Files) == 0 {
		return nil, errors.New("nothing to commit")
	}
	switch f.Kind {
	case "github":
		return c.ghCommit(ctx, f, repo, in)
	case "gitlab":
		return c.glCommit(ctx, f, repo, in)
	}
	return c.fjCommit(ctx, f, repo, in)
}

// fjCommit: Forgejo's multi-file commit, POST /repos/{o}/{r}/contents.
func (c *httpForgeClient) fjCommit(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error) {
	type file struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Content   string `json:"content,omitempty"`
		SHA       string `json:"sha,omitempty"`
	}
	var files []file
	for _, fc := range in.Files {
		x := file{Path: fc.Path, SHA: fc.BaseSHA}
		switch {
		case fc.Delete:
			x.Operation = "delete"
		case fc.BaseSHA == "":
			x.Operation = "create"
			x.Content = base64.StdEncoding.EncodeToString([]byte(fc.Content))
		default:
			x.Operation = "update"
			x.Content = base64.StdEncoding.EncodeToString([]byte(fc.Content))
		}
		files = append(files, x)
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	body := map[string]any{"branch": in.Base, "message": in.Message, "files": files}
	if in.Branch != "" {
		body["new_branch"] = in.Branch
	}
	err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/contents", body, &out)
	if err != nil {
		return nil, err
	}
	return &ForgeCommit{SHA: out.Commit.SHA, Branch: cmp(in.Branch, in.Base)}, nil
}

// ghCommit builds the commit with GitHub's git data API: blobs, a tree on
// the base commit's tree, the commit, then the branch.
func (c *httpForgeClient) ghCommit(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+escPath(in.Base), nil, &ref); err != nil {
		return nil, err
	}
	var base struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/repos/"+repo+"/git/commits/"+ref.Object.SHA, nil, &base); err != nil {
		return nil, err
	}
	modes := map[string]string{}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/repos/"+repo+"/git/trees/"+base.Tree.SHA+"?recursive=1", nil, &tree); err == nil && !tree.Truncated {
		for _, e := range tree.Tree {
			modes[e.Path] = e.Mode
		}
	}
	type entry struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	var entries []entry
	for _, fc := range in.Files {
		mode := cmp(modes[fc.Path], "100644")
		if fc.Delete {
			entries = append(entries, entry{Path: fc.Path, Mode: mode, Type: "blob"})
			continue
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/git/blobs",
			map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(fc.Content)), "encoding": "base64"}, &blob); err != nil {
			return nil, err
		}
		sha := blob.SHA
		entries = append(entries, entry{Path: fc.Path, Mode: mode, Type: "blob", SHA: &sha})
	}
	var nt struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/git/trees", map[string]any{"base_tree": base.Tree.SHA, "tree": entries}, &nt); err != nil {
		return nil, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/git/commits",
		map[string]any{"message": in.Message, "tree": nt.SHA, "parents": []string{ref.Object.SHA}}, &commit); err != nil {
		return nil, err
	}
	if err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/git/refs", map[string]string{"ref": "refs/heads/" + in.Branch, "sha": commit.SHA}, nil); err != nil {
		return nil, err
	}
	return &ForgeCommit{SHA: commit.SHA, Branch: in.Branch}, nil
}

// glCommit: GitLab's commits API takes every file as an action.
func (c *httpForgeClient) glCommit(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error) {
	type action struct {
		Action   string `json:"action"`
		FilePath string `json:"file_path"`
		Content  string `json:"content,omitempty"`
	}
	var actions []action
	for _, fc := range in.Files {
		a := action{FilePath: fc.Path, Content: fc.Content}
		switch {
		case fc.Delete:
			a.Action, a.Content = "delete", ""
		case fc.BaseSHA == "":
			a.Action = "create"
		default:
			a.Action = "update"
		}
		actions = append(actions, a)
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, f, http.MethodPost, glProject(repo)+"/repository/commits", map[string]any{
		"branch": in.Branch, "start_branch": in.Base, "commit_message": in.Message, "actions": actions,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &ForgeCommit{SHA: out.ID, Branch: in.Branch}, nil
}

func (c *httpForgeClient) CreatePR(ctx context.Context, f *Forge, repo string, in NewPR) (*ForgePR, error) {
	title := in.Title
	if in.Draft && f.Kind != "github" && !draftTitle(title) {
		title = "WIP: " + in.Title
		if f.Kind == "gitlab" {
			title = "Draft: " + in.Title
		}
	}
	if f.Kind == "gitlab" {
		var m glMR
		body := map[string]string{"source_branch": in.Head, "target_branch": in.Base, "title": title, "description": in.Body}
		if len(in.Labels) > 0 {
			body["labels"] = strings.Join(in.Labels, ",")
		}
		if err := c.do(ctx, f, http.MethodPost, glProject(repo)+"/merge_requests", body, &m); err != nil {
			return nil, err
		}
		return m.pr(), nil
	}
	var a apiPR
	body := map[string]any{"title": title, "body": in.Body, "head": in.Head, "base": in.Base}
	if in.Draft && f.Kind == "github" {
		body["draft"] = true
	}
	if len(in.Labels) > 0 && f.Kind != "github" {
		ids, err := c.fjLabelIDs(ctx, f, repo, in.Labels)
		if err != nil {
			return nil, err
		}
		body["labels"] = ids
	}
	if err := c.do(ctx, f, http.MethodPost, "/repos/"+repo+"/pulls", body, &a); err != nil {
		return nil, err
	}
	fp := a.pr()
	labelled := true
	if len(in.Labels) > 0 && f.Kind == "github" {
		// GitHub's pull request call takes no labels: label its issue. It is
		// open either way; the caller sees what fp.Labels lacks.
		labelled = c.ChangePRLabels(ctx, f, repo, fp.Number, in.Labels, nil) == nil
	}
	for _, l := range in.Labels {
		if labelled && !slices.ContainsFunc(fp.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			fp.Labels = append(fp.Labels, l)
		}
	}
	return fp, nil
}
