package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ForgeKitReader is what reading the kit needs from a forge client.
type ForgeKitReader interface {
	DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error)
	// BranchHead is the commit a branch points at.
	BranchHead(ctx context.Context, f *Forge, repo, branch string) (string, error)
	// RepoTree lists the files (blobs) of a commit, up to maxTreePaths.
	RepoTree(ctx context.Context, f *Forge, repo, sha string) ([]string, error)
	FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
}

// maxTreePaths bounds a tree listing: a kit is small.
const maxTreePaths = 20000

func (c *httpForgeClient) BranchHead(ctx context.Context, f *Forge, repo, branch string) (string, error) {
	var out struct {
		Commit struct {
			ID  string `json:"id"`
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	p := fmt.Sprintf("/repos/%s/branches/%s", repo, escPath(branch))
	if f.Kind == "gitlab" {
		p = fmt.Sprintf("%s/repository/branches/%s", glProject(repo), url.PathEscape(branch))
	}
	if err := c.do(ctx, f, http.MethodGet, p, nil, &out); err != nil {
		return "", err
	}
	sha := cmp(out.Commit.ID, out.Commit.SHA)
	if sha == "" {
		return "", errors.New("the forge didn't say the branch's commit")
	}
	return sha, nil
}

func (c *httpForgeClient) RepoTree(ctx context.Context, f *Forge, repo, sha string) ([]string, error) {
	type entry struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	var paths []string
	add := func(es []entry) {
		for _, e := range es {
			if e.Type == "blob" {
				paths = append(paths, e.Path)
			}
		}
	}
	switch f.Kind {
	case "gitlab":
		for page := 1; len(paths) < maxTreePaths; page++ {
			var es []entry
			if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/repository/tree?recursive=true&ref=%s&per_page=100&page=%d", glProject(repo), url.QueryEscape(sha), page), nil, &es); err != nil {
				return nil, err
			}
			add(es)
			if len(es) < 100 {
				break
			}
		}
	case "github":
		var out struct {
			Tree []entry `json:"tree"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repo, url.PathEscape(sha)), nil, &out); err != nil {
			return nil, err
		}
		add(out.Tree)
	default:
		for page := 1; len(paths) < maxTreePaths; page++ {
			var out struct {
				Tree      []entry `json:"tree"`
				Truncated bool    `json:"truncated"`
			}
			if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=true&per_page=1000&page=%d", repo, url.PathEscape(sha), page), nil, &out); err != nil {
				return nil, err
			}
			add(out.Tree)
			if !out.Truncated || len(out.Tree) == 0 {
				break
			}
		}
	}
	return paths, nil
}
