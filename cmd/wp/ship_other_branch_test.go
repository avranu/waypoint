package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// On a branch named for another task, commits whose subjects all name the
// task being finished are that task's code: they ship, linked to that task.
// Commits that don't all say so stay out of a pull request, with a notice
// (WP-665).
func TestShipOnBranchNamedForAnotherTask(t *testing.T) {
	var unreadable atomic.Value
	unreadable.Store("")
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if k, _ := unreadable.Load().(string); k != "" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues/"+k {
				http.Error(w, "broken", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	me := e.login()
	ctx := context.Background()
	fg := &openForge{}
	e.svc.Forges = fg
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()

	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")

	task := func(title string) string {
		t.Helper()
		iss, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return iss.Key
	}
	other := task("The task the branch is named for")
	n := 0
	pushed := func(msg ...string) {
		t.Helper()
		n++
		name := strings.ToLower(other) + "-work-" + string(rune('a'+n))
		sh(t, dir, "checkout", "-q", "main")
		sh(t, dir, "checkout", "-q", "-b", name)
		args := []string{"commit", "-q", "--allow-empty"}
		for _, m := range msg {
			args = append(args, "-m", m)
		}
		sh(t, dir, args...)
		sh(t, dir, "push", "-q", "-u", "origin", name)
	}
	prLinks := func(out string) string {
		t.Helper()
		m := regexp.MustCompile(`waypoint#(\d+)|(?m)^PR (\d+)\b`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no pull request in:\n%s", out)
		}
		return e.wp("pr", "show", m[1]+m[2])
	}
	inReview := func(key string) bool { return strings.Contains(e.wp("show", key), "[In Review]") }

	given := task("Given by key")
	pushed("Older copies stay private (" + given + ")")
	out := e.wp("ship", given, "-m", "Done it")
	if len(fg.created) != 1 || !inReview(given) {
		t.Fatalf("ship KEY didn't ship the commit naming it (%d opened):\n%s", len(fg.created), out)
	}
	if links := prLinks(out); !strings.Contains(links, given) || strings.Contains(links, other) {
		t.Errorf("linked to the wrong task:\n%s", links)
	}

	held := task("Held here")
	pushed("Fix it (" + held + ")")
	e.wp("take", held)
	out = e.wp("ship", "-m", "Done it")
	if len(fg.created) != 2 || !inReview(held) {
		t.Fatalf("ship of a held task didn't ship the commit naming it (%d opened):\n%s", len(fg.created), out)
	}
	if links := prLinks(out); !strings.Contains(links, held) || strings.Contains(links, other) {
		t.Errorf("linked to the wrong task:\n%s", links)
	}
	if show := e.wp("show", other); strings.Contains(show, "In Review") {
		t.Errorf("the branch's task moved:\n%s", show)
	}

	review := task("A review with no code")
	pushed("Other work")
	out = e.wp("ship", review, "-m", "Reviewed")
	if len(fg.created) != 2 || !strings.Contains(out, "Notice:") || !strings.Contains(out, "wp pr open --issue "+review) {
		t.Errorf("commits naming nothing (%d opened):\n%s", len(fg.created), out)
	}
	if show := e.wp("show", review); !strings.Contains(show, "[Done]") {
		t.Errorf("the task didn't finish:\n%s", show)
	}

	mentioned := task("Only mentioned")
	pushed("Other work ("+other+")", "See also "+mentioned)
	out = e.wp("ship", mentioned, "-m", "Reviewed")
	if len(fg.created) != 2 || !strings.Contains(out, "Notice:") {
		t.Errorf("a key in a commit body shipped (%d opened):\n%s", len(fg.created), out)
	}

	declined := task("Declined")
	pushed("Other work")
	if out = e.wp("ship", declined, "-m", "Nope", "--no-ship"); strings.Contains(out, "Notice:") {
		t.Errorf("--no-ship still noticed:\n%s", out)
	}

	again := task("Finished already")
	pushed("The work (" + again + ")")
	e.wp("ship", again, "-m", "First", "--no-ship")
	if out, code := e.run("ship", again, "-m", "Again"); len(fg.created) != 2 || !strings.Contains(out, "already Done") {
		t.Errorf("a finished task's commit shipped again (%d opened, exit %d):\n%s", len(fg.created), code, out)
	}

	// A task that can't be read may be finished, so its commits are neither shipped nor left behind; --no-ship still works.
	unread := task("Can't be read")
	pushed("The work (" + unread + ")")
	unreadable.Store(unread)
	out, code := e.run("ship", unread, "-m", "Done it")
	if code == 0 || len(fg.created) != 2 || !strings.Contains(out, "can't tell whether "+unread) {
		t.Errorf("a failed lookup (%d opened, exit %d):\n%s", len(fg.created), code, out)
	}
	unreadable.Store("")
	if show := e.wp("show", unread); strings.Contains(show, "[Done]") {
		t.Errorf("finished on a failed lookup:\n%s", show)
	}
	unreadable.Store(unread)
	e.wp("ship", unread, "-m", "Not its", "--no-ship")
	unreadable.Store("")
	if show := e.wp("show", unread); !strings.Contains(show, "[Done]") {
		t.Errorf("--no-ship didn't finish it:\n%s", show)
	}

	// One matching commit must not make the branch's other commits ship under this task.
	mixed := task("One commit of several")
	pushed("Add the parser (" + other + ")")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix the redirect ("+mixed+")")
	sh(t, dir, "push", "-q")
	out = e.wp("ship", mixed, "-m", "Reviewed")
	if len(fg.created) != 2 || !strings.Contains(out, "wp pr open --issue "+mixed) {
		t.Errorf("a branch only partly this task's shipped (%d opened):\n%s", len(fg.created), out)
	}

	merged := task("Only merged in")
	pushed("Other work")
	side := strings.ToLower(merged) + "-fix"
	sh(t, dir, "checkout", "-q", "-b", side)
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Side work")
	sh(t, dir, "checkout", "-q", "-")
	sh(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch '"+side+"'", side)
	sh(t, dir, "push", "-q")
	if out = e.wp("ship", merged, "-m", "Reviewed"); len(fg.created) != 2 || !strings.Contains(out, "Notice:") {
		t.Errorf("a merge's subject shipped the branch (%d opened):\n%s", len(fg.created), out)
	}

	withMain := task("Merged main in")
	pushed("The work (" + withMain + ")")
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Main moved on")
	sh(t, dir, "push", "-q", "origin", "main")
	sh(t, dir, "checkout", "-q", "-")
	sh(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch 'main' into it", "main")
	sh(t, dir, "push", "-q")
	if out = e.wp("ship", withMain, "-m", "Done"); len(fg.created) != 3 || !inReview(withMain) {
		t.Errorf("a merge commit stopped the task's commits shipping (%d opened):\n%s", len(fg.created), out)
	}

	one, two := task("One of two"), task("Two of two")
	pushed("Fix it (" + one + ")")
	out = e.wp("done", one, two, "-m", "Reviewed")
	if len(fg.created) != 3 || !strings.Contains(out, "wp pr open --issue "+one+" or wp pr open --issue "+two) || strings.Contains(out, "--issue KEY") {
		t.Errorf("finishing several (%d opened):\n%s", len(fg.created), out)
	}
	if show := e.wp("show", one); !strings.Contains(show, "[Done]") {
		t.Errorf("the task didn't finish:\n%s", show)
	}

	quiet := task("In JSON")
	pushed("Other work")
	var stdout, stderr bytes.Buffer
	code = run([]string{"ship", quiet, "-m", "Reviewed", "--json"}, &stdout, &stderr)
	if code != 0 || strings.Contains(stdout.String(), "Notice:") || !strings.Contains(stderr.String(), "wp pr open --issue "+quiet) {
		t.Errorf("--json (exit %d):\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	autos, err := e.svc.ListAutomations(ctx, me, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	for _, a := range autos {
		if a.Rule.Trigger.Event == "pr_merged" {
			if _, err := e.svc.UpdateAutomation(ctx, me, a.ID, core.AutomationInput{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
		}
	}
	shipped := task("Shipped before")
	fg.mu.Lock()
	fg.merged = map[int]bool{9: true}
	fg.mu.Unlock()
	if _, err := e.svc.AddLink(ctx, me, shipped, "https://git.example.com/robin/waypoint/pulls/9", ""); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	pushed("More (" + shipped + ")")
	e.wp("take", shipped)
	out = e.wp("ship", "-m", "Again")
	if !strings.Contains(out, "Not linking "+shipped) || !strings.Contains(out, "already merged") || inReview(shipped) {
		t.Errorf("a held task with a merged pull request linked:\n%s", out)
	}
	if links := prLinks(out); !strings.Contains(links, "Linked issues: none") {
		t.Errorf("linked anyway:\n%s", links)
	}
}
