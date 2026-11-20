package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// movedForge knows one repo, by its new name only.
type movedForge struct{ core.ForgeClient }

func (movedForge) ListIssues(context.Context, *core.Forge, string, time.Time) ([]core.ForgeItem, error) {
	return nil, nil
}

func (movedForge) GetRepo(_ context.Context, _ *core.Forge, repo string) (*core.ForgeRepo, error) {
	if strings.EqualFold(repo, "ops/infra") {
		return &core.ForgeRepo{ID: 7, Path: "ops/infra"}, nil
	}
	return nil, errors.New("HTTP 404")
}

func (movedForge) RepoByID(context.Context, *core.Forge, int64) (*core.ForgeRepo, error) {
	return &core.ForgeRepo{ID: 7, Path: "ops/infra"}, nil
}

func TestCLIProjectRepoMove(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	e.svc.Forges = movedForge{}
	kind, base := "forgejo", "https://git.example.com"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Infra")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "acme/infra", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if out, code := e.run("project", "repo", "move", "-p", p.Key, "ops/nope"); code == 0 || !strings.Contains(out, "couldn't find ops/nope") {
		t.Errorf("move to a missing repo: %d %s", code, out)
	}
	if out, code := e.run("project", "repo", "move", "-p", p.Key); code == 0 {
		t.Errorf("move without NEW: %s", out)
	}
	out := e.wp("project", "repo", "move", "-p", p.Key, "acme/infra", "ops/infra")
	if out != "acme/infra is ops/infra now, in "+p.Key+".\n" {
		t.Errorf("move:\n%q", out)
	}
	if out := e.wp("project", "repo", "-p", p.Key); !strings.HasPrefix(out, "ops/infra on git.example.com") {
		t.Errorf("repos after the move:\n%s", out)
	}
}
