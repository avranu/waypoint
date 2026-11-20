package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingForge answers a forge's API the way each kind does and records
// what was sent.
type recordingForge struct {
	t      *testing.T
	kind   string
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
	blobs  map[string]string
}

func (f *recordingForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodies == nil {
		f.bodies = map[string]map[string]any{}
	}
	prefix := map[string]string{"github": "/api/v3", "forgejo": "/api/v1", "gitlab": "/api/v4"}[f.kind]
	path := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	key := r.Method + " " + path
	f.calls = append(f.calls, key)
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.bodies[key] = body
	if r.Header.Get("Authorization") == "" && r.Header.Get("PRIVATE-TOKEN") == "" {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	out := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	switch f.kind {
	case "github":
		switch {
		case key == "GET /repos/acme/app":
			out(`{"default_branch":"trunk"}`)
		case strings.HasPrefix(key, "GET /repos/acme/app/contents/"):
			p := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/app/contents/"), "")
			if sha, ok := f.blobs[p]; ok {
				out(`{"sha":%q}`, sha)
			} else {
				w.WriteHeader(404)
			}
		case key == "GET /repos/acme/app/git/ref/heads/trunk":
			out(`{"object":{"sha":"basecommit"}}`)
		case key == "GET /repos/acme/app/git/commits/basecommit":
			out(`{"tree":{"sha":"basetree"}}`)
		case key == "GET /repos/acme/app/git/trees/basetree":
			out(`{"tree":[{"path":"run.sh","mode":"100755"},{"path":"a.txt","mode":"100644"}],"truncated":false}`)
		case key == "POST /repos/acme/app/git/blobs":
			out(`{"sha":"blob-%d"}`, len(f.calls))
		case key == "POST /repos/acme/app/git/trees":
			out(`{"sha":"newtree"}`)
		case key == "POST /repos/acme/app/git/commits":
			out(`{"sha":"newcommit"}`)
		case key == "POST /repos/acme/app/git/refs":
			w.WriteHeader(201)
			out(`{}`)
		case key == "POST /repos/acme/app/pulls":
			out(`{"number":12,"title":%q,"html_url":"https://github.example/acme/app/pull/12","state":"open"}`, body["title"])
		default:
			f.t.Errorf("unexpected %s", key)
			w.WriteHeader(500)
		}
	case "gitlab":
		switch {
		case key == "GET /projects/acme%2Fapp":
			out(`{"default_branch":"develop"}`)
		case strings.HasPrefix(key, "GET /projects/acme%2Fapp/repository/files/"):
			p := strings.TrimPrefix(path, "/projects/acme%2Fapp/repository/files/")
			p = strings.ReplaceAll(p, "%2F", "/")
			if sha, ok := f.blobs[p]; ok {
				out(`{"blob_id":%q}`, sha)
			} else {
				w.WriteHeader(404)
			}
		case key == "POST /projects/acme%2Fapp/repository/commits":
			out(`{"id":"glcommit"}`)
		case key == "POST /projects/acme%2Fapp/merge_requests":
			out(`{"iid":3,"title":%q,"web_url":"https://gl.example/acme/app/-/merge_requests/3","state":"opened"}`, body["title"])
		default:
			f.t.Errorf("unexpected %s", key)
			w.WriteHeader(500)
		}
	}
}

func newRecordingForge(t *testing.T, kind string, blobs map[string]string) (*httpForgeClient, *Forge, *recordingForge) {
	rf := &recordingForge{t: t, kind: kind, blobs: blobs}
	srv := httptest.NewServer(rf)
	t.Cleanup(srv.Close)
	return &httpForgeClient{http: srv.Client()}, &Forge{Kind: kind, BaseURL: srv.URL, token: "tok"}, rf
}

func TestGitHubCommitFiles(t *testing.T) {
	cl, f, rf := newRecordingForge(t, "github", map[string]string{"a.txt": "sha-a"})
	ctx := context.Background()
	base, err := cl.DefaultBranch(ctx, f, "acme/app")
	if err != nil || base != "trunk" {
		t.Fatalf("default branch %q %v", base, err)
	}
	if sha, err := cl.FileBlobSHA(ctx, f, "acme/app", "a.txt", base); err != nil || sha != "sha-a" {
		t.Fatalf("blob %q %v", sha, err)
	}
	if sha, err := cl.FileBlobSHA(ctx, f, "acme/app", "missing.txt", base); err != nil || sha != "" {
		t.Fatalf("missing blob %q %v", sha, err)
	}
	c, err := cl.CommitFiles(ctx, f, "acme/app", CommitInput{Base: base, Branch: "waypoint/x", Message: "msg", Files: []FileChange{
		{Path: "a.txt", BaseSHA: "sha-a", Content: "new a\n"},
		{Path: "run.sh", BaseSHA: "old", Content: "#!/bin/sh\n"},
		{Path: "gone.txt", BaseSHA: "old", Delete: true},
		{Path: "dir/new.txt", Content: "n\n"},
	}})
	if err != nil || c.SHA != "newcommit" || c.Branch != "waypoint/x" {
		t.Fatalf("commit %+v %v", c, err)
	}
	tree := rf.bodies["POST /repos/acme/app/git/trees"]
	entries := tree["tree"].([]any)
	if tree["base_tree"] != "basetree" || len(entries) != 4 {
		t.Fatalf("tree: %+v", tree)
	}
	mode := map[string]string{}
	var deleted bool
	for _, e := range entries {
		m := e.(map[string]any)
		mode[m["path"].(string)] = m["mode"].(string)
		if m["path"] == "gone.txt" {
			deleted = m["sha"] == nil
		}
	}
	if mode["run.sh"] != "100755" || mode["a.txt"] != "100644" || mode["dir/new.txt"] != "100644" || !deleted {
		t.Fatalf("modes %v deleted %v", mode, deleted)
	}
	blob := rf.bodies["POST /repos/acme/app/git/blobs"]
	if raw, _ := base64.StdEncoding.DecodeString(blob["content"].(string)); blob["encoding"] != "base64" || len(raw) == 0 {
		t.Fatalf("blob body: %+v", blob)
	}
	commit := rf.bodies["POST /repos/acme/app/git/commits"]
	if commit["tree"] != "newtree" || commit["message"] != "msg" || commit["parents"].([]any)[0] != "basecommit" {
		t.Fatalf("commit body: %+v", commit)
	}
	ref := rf.bodies["POST /repos/acme/app/git/refs"]
	if ref["ref"] != "refs/heads/waypoint/x" || ref["sha"] != "newcommit" {
		t.Fatalf("ref body: %+v", ref)
	}
	pr, err := cl.CreatePR(ctx, f, "acme/app", NewPR{Title: "T", Body: "B", Head: "waypoint/x", Base: base})
	if err != nil || pr.Number != 12 || pr.URL != "https://github.example/acme/app/pull/12" {
		t.Fatalf("pr %+v %v", pr, err)
	}
	if b := rf.bodies["POST /repos/acme/app/pulls"]; b["head"] != "waypoint/x" || b["base"] != "trunk" || b["body"] != "B" {
		t.Fatalf("pr body: %+v", b)
	}
}

func TestGitLabCommitFiles(t *testing.T) {
	cl, f, rf := newRecordingForge(t, "gitlab", map[string]string{"docs/a.md": "blob-a"})
	ctx := context.Background()
	base, err := cl.DefaultBranch(ctx, f, "acme/app")
	if err != nil || base != "develop" {
		t.Fatalf("default branch %q %v", base, err)
	}
	if sha, err := cl.FileBlobSHA(ctx, f, "acme/app", "docs/a.md", base); err != nil || sha != "blob-a" {
		t.Fatalf("blob %q %v", sha, err)
	}
	c, err := cl.CommitFiles(ctx, f, "acme/app", CommitInput{Base: base, Branch: "waypoint/y", Message: "msg", Files: []FileChange{
		{Path: "docs/a.md", BaseSHA: "blob-a", Content: "A"},
		{Path: "b.md", Content: "B"},
		{Path: "c.md", BaseSHA: "x", Delete: true},
	}})
	if err != nil || c.SHA != "glcommit" {
		t.Fatalf("commit %+v %v", c, err)
	}
	b := rf.bodies["POST /projects/acme%2Fapp/repository/commits"]
	acts := b["actions"].([]any)
	if b["branch"] != "waypoint/y" || b["start_branch"] != "develop" || b["commit_message"] != "msg" || len(acts) != 3 {
		t.Fatalf("commit body: %+v", b)
	}
	ops := []string{}
	for _, a := range acts {
		ops = append(ops, a.(map[string]any)["action"].(string))
	}
	if strings.Join(ops, ",") != "update,create,delete" {
		t.Fatalf("actions %v", ops)
	}
	pr, err := cl.CreatePR(ctx, f, "acme/app", NewPR{Title: "T", Head: "waypoint/y", Base: base})
	if err != nil || pr.Number != 3 || pr.State != "open" {
		t.Fatalf("mr %+v %v", pr, err)
	}
	if mb := rf.bodies["POST /projects/acme%2Fapp/merge_requests"]; mb["source_branch"] != "waypoint/y" || mb["target_branch"] != "develop" {
		t.Fatalf("mr body: %+v", mb)
	}
}

func TestCommitFilesNeedsFiles(t *testing.T) {
	cl, f, _ := newRecordingForge(t, "forgejo", nil)
	if _, err := cl.CommitFiles(context.Background(), f, "acme/app", CommitInput{Base: "main", Branch: "b"}); err == nil {
		t.Fatal("an empty commit was made")
	}
}

func TestChangeConflictIsAConflict(t *testing.T) {
	err := error(&ChangeConflict{Paths: []string{"a", "b"}})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("%v", err)
	}
}
