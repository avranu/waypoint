package core

// Draft-first needs a repo whose CI runs again when a pull request is edited
// (WP-173). Dropping "WIP: " is an `edited` event, and only a workflow that
// lists it under pull_request types starts the full run then; Forgejo's
// default types (opened, synchronize, reopened) don't. In a repo whose CI
// doesn't, the draft's checks were already the whole run, so an agent's pull
// request opens ready there, and one marked ready doesn't wait for a run
// that never comes.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ForgeDirLister lists a directory of a repo at a ref and reads its files.
type ForgeDirLister interface {
	// DirAt returns the paths of the files in the directory, nil when there
	// is no such directory.
	DirAt(ctx context.Context, f *Forge, repo, path, ref string) ([]string, error)
	FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
}

func (c *httpForgeClient) DirAt(ctx context.Context, f *Forge, repo, path, ref string) ([]string, error) {
	if f.Kind == "gitlab" {
		return nil, fmt.Errorf("listing a directory isn't done on GitLab")
	}
	var out []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escPath(path), url.QueryEscape(ref)), nil, &out)
	if isStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range out {
		if e.Type == "file" {
			files = append(files, e.Path)
		}
	}
	return files, nil
}

// workflowDirs: where Forgejo looks for workflows, the first that exists.
var workflowDirs = []string{".forgejo/workflows", ".gitea/workflows", ".github/workflows"}

// ciRunsOnEdit: a workflow of the repo at ref runs on a pull request's
// `edited` event.
func (s *Service) ciRunsOnEdit(ctx context.Context, f *Forge, repo, ref string) (bool, error) {
	return s.anyWorkflow(ctx, f, repo, ref, workflowRunsOnEdit)
}

// anyWorkflow: a workflow of the repo at ref (in the first workflow
// directory that exists) is one pred says yes to.
func (s *Service) anyWorkflow(ctx context.Context, f *Forge, repo, ref string, pred func(body string) bool) (bool, error) {
	cl, ok := s.Forges.(ForgeDirLister)
	if !ok {
		return false, fmt.Errorf("this forge client can't read workflows")
	}
	for _, dir := range workflowDirs {
		files, err := cl.DirAt(ctx, f, repo, dir, ref)
		if err != nil {
			return false, err
		}
		if files == nil {
			continue
		}
		for _, p := range files {
			if !strings.HasSuffix(p, ".yml") && !strings.HasSuffix(p, ".yaml") {
				continue
			}
			body, err := cl.FileAt(ctx, f, repo, p, ref)
			if err != nil {
				return false, err
			}
			if pred(body) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

// workflowRunsOnEdit: the workflow's pull_request (or pull_request_target)
// trigger lists `edited` among its types.
func workflowRunsOnEdit(body string) bool {
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if yaml.Unmarshal([]byte(body), &wf) != nil || wf.On.Kind != yaml.MappingNode {
		return false // "on: pull_request" or a list: the default types
	}
	for i := 0; i+1 < len(wf.On.Content); i += 2 {
		if k := wf.On.Content[i].Value; k != "pull_request" && k != "pull_request_target" {
			continue
		}
		ev := wf.On.Content[i+1]
		if ev.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(ev.Content); j += 2 {
			if ev.Content[j].Value != "types" {
				continue
			}
			types := ev.Content[j+1]
			if types.Value == "edited" {
				return true
			}
			for _, t := range types.Content {
				if t.Value == "edited" {
					return true
				}
			}
		}
	}
	return false
}

// draftFirstRepo: an agent's pull request to repo opens as a draft Waypoint
// holds. When the workflows can't be read it doesn't: a draft whose full run
// never starts stalls, a ready one only runs everything on each push.
func (s *Service) draftFirstRepo(ctx context.Context, f *Forge, repo, base string) bool {
	if !draftFirstForge(f.Kind) {
		return false
	}
	ok, err := s.ciRunsOnEdit(ctx, f, repo, base)
	if err != nil {
		s.log.WarnContext(ctx, "read the workflows for draft-first", "repo", repo, "err", err)
	}
	return ok
}
