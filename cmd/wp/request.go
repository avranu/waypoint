package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
)

var requestCmd = &command{
	name: "request", aliases: []string{"requests", "req"}, group: "Tasks", args: "ask|change|run|todo|ls|show|wait|withdraw …",
	summary: "ask a person something and keep working",
	about: `Prefer a request to stopping when you need a person: a decision (ask), a
change a hook or rule blocks (change), a command you can't run (run), or a
step only they can do (todo). The answer reaches you at your next stop.

Prefer --followup never when finishing doesn't hang on the answer, and
--blocking only when your other work waits on it. --closes-when closes a
request that becomes moot.

R-12 is your request 12; another project's is KEY:R-12. Several: R-5 R-6, or R-5,6,7.`,
	examples: []string{`wp request ask "Squash the migrations?" --choice yes --choice no`, "wp request ls --answered", "wp request show R-12", "wp request wait R-12"},
}

// askCmd is `wp request ask` under a shorter name (WP-96): the SessionStart
// hook tells every agent about it.
var askCmd = &command{
	name: "ask", group: "Tasks", args: `"QUESTION" [-c A -c B] [--multi] [-d DETAILS]`,
	summary:  "ask a person a question and keep working (wp request ask)",
	examples: []string{`wp ask "Which database?" -c sqlite -c postgres`, `wp ask "Squash the migrations?" -c yes -c no -d "There are 40."`},
	run:      requestAsk,
}

// multiFlag is a repeatable string flag that keeps commas in its values.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func init() {
	requestCmd.subs = []*command{
		{name: "ask", args: `"QUESTION" [-c A -c B] [--multi] [-d DETAILS]`, summary: "ask a question: free text, or pick from choices",
			examples: []string{`wp request ask "Which database?" --choice sqlite --choice postgres`, `wp request ask "May I drop the legacy table?" -d "Nothing reads it since v2."`},
			run:      requestAsk},
		{name: "change", args: "PATH[=FILE]... [--from FILE | --patch FILE] [-m MESSAGE]", summary: "ask for file changes a hook or rule stopped you making",
			about: `Sends the new contents of each file and the git blob they were made against;
nothing is written if the files changed since. New contents come from:
  PATH --from FILE   FILE (- for stdin) is the new content of the single PATH
  PATH=FILE ...      FILE is the new content of each PATH
  --patch FILE       a unified diff applied to HEAD's files
  PATH --delete      delete it
Approved changes are applied in the person's runner worktree and committed,
or opened as a pull request.

A file outside the repo is recorded by absolute path and filed in the repo's
project (-p PROJECT to override); one above the repo root goes to the parent
project. It has no PR: the person approves with --apply runner, and the runner
writes it if the directory is allowed (wp runner --allow). One request names
files of one directory, not repo files too.`,
			examples: []string{"wp request change CLAUDE.md --from /tmp/new.md -m \"Tighten the rules\"", "wp request change --patch fix.diff -m \"Fix the parser\"", "wp request change a.go=/tmp/a.go b.go=/tmp/b.go -m \"Rename\"", "wp request change /srv/org/CLAUDE.md --from /tmp/new.md -p ORG -m \"Org rules\""},
			run:      requestChange},
		{name: "run", args: `"COMMAND" [--cmd "COMMAND"]... [--dir DIR] [-d WHY]`, summary: "ask the person to run a command and read its output",
			examples: []string{`wp request run "make migrate" --dir db -d "needs your database login"`}, run: requestRun},
		{name: "todo", args: `"WHAT A PERSON MUST DO" [--cmd "COMMAND"]... [-d DETAILS]`, summary: "ask for a step only a person can do",
			examples: []string{`wp request todo "Set the DEPLOY_SSH_KEY secret in the repo settings"`}, run: requestTodo},
		{name: "ls", aliases: []string{"list"}, args: "[--open|--answered|--all] [--mine]", summary: "list requests",
			about:    "Open requests by default; --answered is decided ones you haven't been told of. Agents see their own, people those they can decide.",
			examples: []string{"wp request ls", "wp request ls --answered", "wp request ls --all -p APP"}, run: requestLs},
		{name: "show", aliases: []string{"view", "get"}, args: "CODE... [--diff]", summary: "show requests: the ask, the answer or outcome, command output",
			examples: []string{"wp request show R-12", "wp request show R-12 R-14", "wp request show R-12 --diff"}, run: requestShow},
		{name: "wait", args: "CODE [--timeout 10m]", summary: "wait until a person decides a request",
			about:    "Prints the request once decided (an approved change, once applied); exits 1 if still open after the timeout.",
			examples: []string{"wp request wait R-12 --timeout 10m"}, run: requestWait},
		{name: "watch", args: "CODE KIND=TARGET... [--check-every 10m]", summary: "close an open request by itself when a condition holds",
			about: `Waypoint checks the conditions without a model and closes the request when
one holds (wp ask, run, todo and change take them as --closes-when):
  pr-merged=#15              the pull request merges (owner/repo#15 or URL too)
  issue-done=WP-12           the issue is done or canceled
  url-ok=https://host/health the URL answers 2xx
  cmd-ok=COMMAND             exits 0 on a person's runner (people only)`,
			examples: []string{"wp request watch R-12 pr-merged=#15", "wp request watch R-12 url-ok=https://app.example.com/healthz --check-every 1m"}, run: requestWatch},
		{name: "withdraw", args: "CODE... [-m WHY]", summary: "take back open requests you no longer need", examples: []string{"wp request withdraw R-12 -m \"main is pushed, so the repo exists now\"", "wp request withdraw R-12,13,15"}, run: requestWithdraw},
		{name: "answer", args: `CODE ["TEXT"] [--choice A]...`, summary: "(people) answer a question",
			examples: []string{`wp request answer R-12 --choice sqlite`, `wp request answer R-12 "Use sqlite for now"`}, run: requestDecide("answer")},
		{name: "approve", args: "CODE... [--apply runner|pr] [--rule RULE] [-m NOTE]", summary: "(people) approve a change or permission; a change is applied, a permission can add an allow rule",
			examples: []string{"wp request approve R-12", "wp request approve R-12 --apply pr", "wp request approve R-14 --rule 'Bash(make lint *)'"}, run: requestDecide("approve")},
		{name: "deny", args: "CODE... [-m NOTE]", summary: "(people) deny a change or permission", examples: []string{`wp request deny R-12 -m "Not in this PR"`}, run: requestDecide("deny")},
		{name: "done", args: "CODE... [-m NOTE]", summary: "(people) mark a command or action done; an agent's own request is withdrawn", examples: []string{"wp request done R-12", "wp request done R-5, 6, 7"}, run: requestDecide("done")},
		{name: "dismiss", args: "CODE... [-m NOTE]", summary: "(people) dismiss a request; the agent hears only with a -m NOTE; an agent's own is withdrawn", examples: []string{"wp request dismiss R-12"}, run: requestDecide("dismiss")},
		{name: "snooze", args: "CODE... [--for 2h]", summary: "(people) hide an open request for a while", examples: []string{"wp request snooze R-12 --for 2h"}, run: requestSnooze},
	}
}

type reqView struct {
	ID       int64  `json:"id"`
	Code     string `json:"code"`
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	State    string `json:"state"`
	Agent    string `json:"agent"`
	Answer   string `json:"answer"`
	Note     string `json:"note"`
	Error    string `json:"error"`
	Via      string `json:"via"`
	New      bool   `json:"new"`
	Followup string `json:"followup"`
	Blocking bool   `json:"blocking"`
	Project  *struct {
		Key string `json:"key"`
	} `json:"project"`
	DecidedBy *struct {
		DisplayName string `json:"display_name"`
	} `json:"decided_by"`
	Session *struct {
		Machine string `json:"machine"`
		Branch  string `json:"branch"`
	} `json:"session"`
	Watches   []reqWatch `json:"watches"`
	Questions []struct {
		Question string `json:"question"`
		Multi    bool   `json:"multi"`
		Choices  []struct {
			Label string `json:"label"`
		} `json:"choices"`
	} `json:"questions"`
	Files []struct {
		Path      string `json:"path"`
		New       bool   `json:"new"`
		Delete    bool   `json:"delete"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Diff      string `json:"diff"`
	} `json:"files"`
	Cells []struct {
		N       int    `json:"n"`
		Command string `json:"command"`
		Dir     string `json:"dir"`
		Runs    []struct {
			Command  string `json:"command"`
			State    string `json:"state"`
			ExitCode *int   `json:"exit_code"`
			Output   string `json:"output"`
			Error    string `json:"error"`
			Waiting  string `json:"waiting"`
		} `json:"runs"`
	} `json:"cells"`
	Result struct {
		Commit   string `json:"commit"`
		Branch   string `json:"branch"`
		PRURL    string `json:"pr_url"`
		Reason   string `json:"reason"`
		NoChange bool   `json:"no_change"`
		Rule     string `json:"rule"`
	} `json:"result"`
	// A permission request's allow rules a person may add (WP-233).
	Rules []struct {
		Rule   string `json:"rule"`
		Covers string `json:"covers"`
	} `json:"rules"`
	RuleRepo string `json:"rule_repo"`
}

// followupPhrase says in a sentence when a decision reaches the agent.
func followupPhrase(mode string, blocking bool) string {
	switch {
	case blocking && mode == "on":
		return "within minutes if it is one you named, else with your next prompt"
	case blocking:
		return "within minutes"
	}
	switch mode {
	case "never":
		return "with your next prompt; it won't wake you"
	case "on":
		return "when you next stop if it is one you named, else with your next prompt"
	}
	return "when you next stop"
}

// reqWatch is a condition a request watches (WP-126).
type reqWatch struct {
	Summary string `json:"summary"`
	State   string `json:"state"`
	Note    string `json:"note"`
}

type reqList struct {
	Requests []reqView `json:"requests"`
	Total    int       `json:"total"`
}

// text is a request as a terminal reads it.
func (r *reqView) text(diffs bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n%s · %s", r.Ref, r.Title, r.Kind, r.State)
	if r.DecidedBy != nil {
		fmt.Fprintf(&b, " by %s", r.DecidedBy.DisplayName)
	}
	if r.Via == "terminal" {
		b.WriteString(" (in the terminal)")
	}
	if r.Session != nil {
		fmt.Fprintf(&b, " · %s on %s", r.Agent, r.Session.Machine)
	}
	b.WriteString("\n")
	if r.Body != "" {
		b.WriteString("\n" + strings.TrimSpace(r.Body) + "\n")
	}
	for i, q := range r.Questions {
		if len(r.Questions) > 1 || q.Question != r.Title {
			fmt.Fprintf(&b, "\n%d. %s\n", i+1, q.Question)
		}
		for _, c := range q.Choices {
			fmt.Fprintf(&b, "   - %s\n", c.Label)
		}
	}
	if len(r.Watches) > 0 {
		b.WriteString("\nWatching:\n" + watchLines(r.Watches))
	}
	if r.Answer != "" {
		b.WriteString("\nAnswer:\n" + r.Answer + "\n")
	}
	if r.Note != "" {
		b.WriteString("Note: " + r.Note + "\n")
	}
	for _, f := range r.Files {
		mark := "M"
		switch {
		case f.New:
			mark = "A"
		case f.Delete:
			mark = "D"
		}
		fmt.Fprintf(&b, "%s %s  +%d -%d\n", mark, f.Path, f.Additions, f.Deletions)
		if diffs {
			b.WriteString(f.Diff)
		}
	}
	for _, c := range r.Cells {
		fmt.Fprintf(&b, "\n$ %s", c.Command)
		if c.Dir != "" {
			fmt.Fprintf(&b, "   (in %s)", c.Dir)
		}
		b.WriteString("\n")
		if len(c.Runs) > 0 {
			run := c.Runs[0]
			if run.State == "queued" {
				fmt.Fprintf(&b, "  waiting: %s\n", cmpStr(run.Waiting, "for a runner to take it"))
				continue
			}
			code := "?"
			if run.ExitCode != nil {
				code = fmt.Sprint(*run.ExitCode)
			}
			fmt.Fprintf(&b, "  ran: %s, exit %s\n%s\n", run.State, code, strings.TrimRight(run.Output, "\n"))
			if run.Error != "" {
				b.WriteString("  " + run.Error + "\n")
			}
		}
	}
	if len(r.Rules) > 0 && r.State == "open" {
		fmt.Fprintf(&b, "\nApproving alone changes nothing auto mode sees. To allow it from now on, add a rule to %s's .claude/settings.json:\n", r.RuleRepo)
		for _, ru := range r.Rules {
			fmt.Fprintf(&b, "  wp request approve %s --rule %s\n    %s\n", r.Code, quoteArg(ru.Rule), ru.Covers)
		}
	}
	switch r.State {
	case "approved":
		switch {
		case r.Result.Rule != "" && r.Result.PRURL != "":
			fmt.Fprintf(&b, "Rule %s: %s\n", r.Result.Rule, r.Result.PRURL)
		case r.Result.Rule != "":
			fmt.Fprintf(&b, "Rule %s: %s\n", r.Result.Rule, r.Result.Reason)
		case r.Kind == "permission" && r.Result.Reason != "":
			fmt.Fprintf(&b, "%s\n", r.Result.Reason)
		}
	case "applied":
		switch {
		case r.Result.NoChange:
			fmt.Fprintf(&b, "\nChanged nothing: %s No commit or PR was made.\n", r.Result.Reason)
		case r.Result.PRURL != "":
			fmt.Fprintf(&b, "\nOpened %s on branch %s (not in your worktree).\n", r.Result.PRURL, r.Result.Branch)
		case r.Result.Commit != "":
			fmt.Fprintf(&b, "\nApplied in your worktree as commit %s.\n", r.Result.Commit)
		}
	case "failed", "conflict":
		fmt.Fprintf(&b, "\nNot applied: %s\n", r.Error)
	case "expired":
		fmt.Fprintf(&b, "\nExpired: %s\n", r.Result.Reason)
	}
	return b.String()
}

// requestProject is the project a request goes into when it isn't the
// session's: -p, else the git repo's project when no session knows better.
func (c *cli) requestProject(sf scopeFlags) string {
	if sf.project != nil && *sf.project != "" {
		return *sf.project
	}
	if c.hasSession() {
		return ""
	}
	sc, err := c.scopeFor(sf)
	if err != nil || sc.all() {
		return ""
	}
	if home, err := sc.home(); err == nil {
		return home
	}
	return ""
}

// hasSession: this process knows its agent session.
func (c *cli) hasSession() bool {
	if c.agentHeader() == "" {
		return false
	}
	if os.Getenv("WAYPOINT_SESSION") != "" {
		return true
	}
	cwd, _ := os.Getwd()
	return c.state.sessionIn(cwd) != ""
}

func (c *cli) fileRequest(body map[string]any) error {
	if body["kind"] == "command" {
		c.watchSession(c.sessionID()) // A command request can be approved by a person in the terminal, so the session's Bash calls are watched while it waits.
	}
	raw, err := c.call("POST", "/api/v1/requests", body)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r reqView
		_ = json.Unmarshal(raw, &r)
		if !r.New {
			return fmt.Sprintf("Already filed as %s (%s).", r.Ref, r.State)
		}
		return fmt.Sprintf("Filed %s: %s\nThe answer comes %s.", r.Ref, r.Title, followupPhrase(r.Followup, r.Blocking))
	})
}

// shortTitle is s on one line. It never cuts: a title too long for the server
// is turned into a summary there, with the whole text in the details (WP-175).
func shortTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// followupFlags are the flags that say whether an answer wakes the agent
// (WP-176).
type followupFlags struct {
	mode     *string
	on       *multiFlag
	until    *multiFlag
	every    *time.Duration
	blocking *bool
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func addFollowupFlags(fs *flag.FlagSet) followupFlags {
	f := followupFlags{mode: new(string), on: new(multiFlag), until: new(multiFlag), every: new(time.Duration), blocking: new(bool)}
	fs.Var(f.until, "closes-when", "close the request when `KIND=TARGET` holds: pr-merged=#15, issue-done=WP-12, url-ok=URL (repeatable)")
	fs.DurationVar(f.every, "check-every", 0, "how often url-ok/cmd-ok are checked (default 5m)")
	hideFlags(fs, "check-every")
	fs.StringVar(f.mode, "followup", "", "`WHEN` an answer wakes you: any (default) or never")
	fs.Var(f.on, "followup-on", "wake me only for this `CHOICE` (or approved, denied, done, dismissed; repeatable)")
	fs.BoolVar(f.blocking, "blocking", false, "my other work waits on the answer: wake me within minutes")
	return f
}

// apply puts the follow-up setting in a request body.
func (f followupFlags) apply(body map[string]any) error {
	mode := strings.ToLower(strings.TrimSpace(*f.mode))
	if mode != "" && mode != "any" && mode != "never" && mode != "on" {
		return fmt.Errorf("--followup is any, never or on, not %q", mode)
	}
	if mode == "" && len(*f.on) > 0 {
		mode = "on"
	}
	if mode == "on" && len(*f.on) == 0 {
		return errors.New("--followup on needs --followup-on CHOICE")
	}
	if (mode == "any" || mode == "never") && len(*f.on) > 0 {
		return errors.New("--followup-on goes with --followup on")
	}
	if *f.blocking && mode == "never" {
		return errors.New("--blocking is for an answer that wakes you, not --followup never")
	}
	for _, w := range *f.until {
		kind, target, ok := strings.Cut(w, "=")
		if !ok || target == "" {
			return fmt.Errorf("--closes-when is KIND=TARGET (pr-merged=#15, issue-done=WP-12, url-ok=https://…), not %q", w)
		}
		cond := map[string]any{"kind": strings.TrimSpace(kind), "target": strings.TrimSpace(target)}
		if *f.every > 0 {
			cond["every"] = int(f.every.Seconds())
		}
		body["closes_when"] = append(anyList(body["closes_when"]), cond)
	}
	if mode != "" {
		body["followup"] = mode
	}
	if len(*f.on) > 0 {
		body["followup_on"] = []string(*f.on)
	}
	if *f.blocking {
		body["blocking"] = true
	}
	return nil
}

func requestAsk(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	var choices multiFlag
	fs.Var(&choices, "choice", "an option `LABEL` (repeatable)")
	multi := fs.Bool("multi", false, "more than one choice may be picked")
	def := fs.String("default", "", "the `CHOICE` if nobody answers")
	details := fs.String("d", "", "why you ask (`MARKDOWN`)")
	title := fs.String("title", "", "a shorter `TITLE` than the question")
	issue := fs.String("issue", "", "the `ISSUE` it is about")
	fu := addFollowupFlags(fs)
	alias(fs, "details", "d")
	alias(fs, "c", "choice")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("what do you want to ask?")
	}
	question := joinArgs(pos)
	q := map[string]any{"question": question}
	if len(choices) > 0 {
		var cs []map[string]string
		for _, ch := range choices {
			cs = append(cs, map[string]string{"label": ch})
		}
		q["choices"] = cs
		q["multi"] = *multi
	}
	if *def != "" {
		q["default"] = *def
	}
	body := map[string]any{"kind": "question", "title": cmpStr(*title, shortTitle(question)), "body": *details, "questions": []any{q}}
	if p := c.requestProject(sf); p != "" {
		body["project"] = p
	}
	if *issue != "" {
		body["issue"] = *issue
	}
	if err := fu.apply(body); err != nil {
		return err
	}
	return c.fileRequest(body)
}

func requestRun(c *cli, args []string) error {
	return requestCells(c, args, "command", "run")
}

func requestTodo(c *cli, args []string) error {
	return requestCells(c, args, "action", "todo")
}

func requestCells(c *cli, args []string, kind, name string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	var cmds multiFlag
	fs.Var(&cmds, "cmd", "a `COMMAND` (repeat for several)")
	dir := fs.String("dir", "", "the `DIR` to run them in (default: the session's)")
	details := fs.String("d", "", "why (`MARKDOWN`)")
	title := fs.String("title", "", "a shorter `TITLE`")
	issue := fs.String("issue", "", "the `ISSUE` it is about")
	fu := addFollowupFlags(fs)
	alias(fs, "details", "d")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var t string
	if kind == "command" {
		if len(pos) > 0 {
			cmds = append(multiFlag{joinArgs(pos)}, cmds...)
		}
		if len(cmds) == 0 {
			return c.usage("which command?")
		}
		t = cmpStr(*title, "Run: "+shortTitle(cmds[0]))
	} else {
		if len(pos) == 0 {
			return c.usage("what must a person do?")
		}
		t = cmpStr(*title, shortTitle(joinArgs(pos)))
	}
	var cells []map[string]string
	for _, cm := range cmds {
		cells = append(cells, map[string]string{"command": cm, "dir": *dir})
	}
	body := map[string]any{"kind": kind, "title": t, "body": *details, "cells": cells}
	if p := c.requestProject(sf); p != "" {
		body["project"] = p
	}
	if *issue != "" {
		body["issue"] = *issue
	}
	_ = name
	if err := fu.apply(body); err != nil {
		return err
	}
	return c.fileRequest(body)
}

// changeFile is one file of a change request.
type changeFile struct {
	Path    string `json:"path"`
	BaseSHA string `json:"base_sha"`
	Base    string `json:"base,omitempty"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
	Outside bool   `json:"outside,omitempty"` // Path is absolute, outside any git repo
}

const (
	maxChangeFile = 512 << 10
	maxPatchFile  = 8 << 20
)

func requestChange(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	from := fs.String("from", "", "new content of the single PATH, from `FILE` (- for stdin)")
	patch := fs.String("patch", "", "a unified `DIFF` file")
	del := fs.Bool("delete", false, "delete the PATHs")
	msg := fs.String("m", "", "commit `MESSAGE` (also the title)")
	details := fs.String("d", "", "why (`MARKDOWN`)")
	title := fs.String("title", "", "a `TITLE` other than the message")
	issue := fs.String("issue", "", "the `ISSUE` it is about")
	fu := addFollowupFlags(fs)
	alias(fs, "message", "m")
	alias(fs, "details", "d")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	root, err := gitOutput("", "rev-parse", "--show-toplevel")
	inRepo := err == nil
	var files []changeFile
	var notice string
	organization := false
	switch {
	case *patch != "":
		if !inRepo {
			return errors.New("--patch applies a diff to HEAD's files; run it inside a git repo, or give PATH --from FILE for files outside one")
		}
		if files, notice, err = changesFromPatch(root, *patch); err != nil {
			return err
		}
	case len(pos) == 0:
		return c.usage("which files? Give PATH --from FILE, PATH=FILE or --patch FILE")
	default:
		for _, p := range pos {
			path, src, hasSrc := strings.Cut(p, "=")
			if hasSrc && !fileExists(src) {
				path, src, hasSrc = p, "", false
			}
			rel, outside, above, err := changePath(root, inRepo, path)
			if err != nil {
				return err
			}
			cf := changeFile{Path: rel, Delete: *del, Outside: outside}
			organization = organization || above
			if outside {
				cf.Base, cf.BaseSHA = outsideBase(rel)
			} else {
				cf.Base, cf.BaseSHA = baseOf(root, rel)
			}
			if !*del {
				switch {
				case hasSrc:
					cf.Content, err = readChangeSource(src)
				case *from != "":
					if len(pos) > 1 {
						return c.usage("--from gives the new content of one path; use PATH=FILE for several")
					}
					cf.Content, err = readChangeSource(*from)
				default:
					if outside {
						return c.usage("%s is outside a git repo, so give its new content: --from FILE or PATH=FILE", rel)
					} else {
						cf.Content, err = readChangeSource(filepath.Join(root, rel))
					}
				}
				if err != nil {
					return err
				}
			}
			files = append(files, cf)
		}
	}
	if len(files) == 0 {
		return errors.New("the change touches no files")
	}
	kept := files[:0:0]
	var same []string
	for _, f := range files {
		if !f.Delete && f.BaseSHA != "" && udiff.GitBlobSHA(f.Content) == f.BaseSHA {
			same = append(same, f.Path)
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		return errors.New("nothing to file: every file already has the content you gave, so the change alters nothing")
	}
	if len(same) > 0 {
		notice = strings.TrimSpace(notice + "\nLeft out " + strings.Join(same, ", ") + ": already has the content you gave.")
	}
	files = kept
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	message := cmpStr(*msg, "Change "+strings.Join(names, ", "))
	t := cmpStr(*title, shortTitle(strings.SplitN(message, "\n", 2)[0]))
	body := map[string]any{"kind": "change", "title": t, "body": *details, "message": message, "files": files}
	if organization {
		body["organization"] = true
	}
	if p := c.requestProject(sf); p != "" {
		body["project"] = p
	}
	if *issue != "" {
		body["issue"] = *issue
	}
	if err := fu.apply(body); err != nil {
		return err
	}
	if err := c.fileRequest(body); err != nil {
		return err
	}
	if notice != "" {
		_, _ = fmt.Fprintln(c.err, notice)
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p) // #nosec G703 -- only whether the file exists
	return err == nil && !fi.IsDir()
}

func readChangeSource(src string) (string, error) {
	var b []byte
	var err error
	if src == "-" {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, maxChangeFile+1))
	} else {
		b, err = os.ReadFile(src) // #nosec G304 -- the file the person or agent named
	}
	if err != nil {
		return "", err
	}
	if len(b) > maxChangeFile {
		return "", fmt.Errorf("%s is over %d KB; change requests carry text files up to that size", src, maxChangeFile>>10)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return "", fmt.Errorf("%s looks binary; change requests carry text files", src)
	}
	return string(b), nil
}

// gitOutput runs git in dir ("" = here) and returns its stdout.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 -- fixed git subcommands; paths are arguments, never a shell
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// repoRel is a path (relative to here or absolute) relative to the repo
// root, with forward slashes.
func repoRel(root, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(p) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		abs = filepath.Join(cwd, p)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	dir, base := filepath.Split(abs)
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		abs = filepath.Join(d, base)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the git repo (%s)", p, root)
	}
	return filepath.ToSlash(rel), nil
}

// changePath says how a change request names a file. Inside the repo it is
// the path relative to the root. Anywhere else (or when there is no repo
// here) it is the absolute path, outside; above is set when that file's
// directory is the repo root or one of its ancestors, which makes it belong to
// the organization above the repo (WP-115).
func changePath(root string, inRepo bool, p string) (path string, outside, above bool, err error) {
	if inRepo {
		if rel, err := repoRel(root, p); err == nil {
			return rel, false, false, nil
		}
	}
	abs := p
	if !filepath.IsAbs(p) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", false, false, err
		}
		abs = filepath.Join(cwd, p)
	}
	abs = filepath.Clean(abs)
	dir, base := filepath.Split(abs)
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		abs = filepath.Join(d, base)
	}
	abs = filepath.ToSlash(abs)
	if inRepo {
		r := filepath.ToSlash(root)
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			r = filepath.ToSlash(rr)
		}
		d := pathDir(abs)
		above = r == d || strings.HasPrefix(r, strings.TrimSuffix(d, "/")+"/")
	}
	return abs, true, above, nil
}

func pathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// outsideBase is the file's current content and its git blob id (computed,
// there is no repo to ask); empty for a file that isn't there.
func outsideBase(p string) (content, sha string) {
	b, err := os.ReadFile(p) // #nosec G304 -- the file the person or agent named
	if err != nil || len(b) > maxChangeFile || bytes.IndexByte(b, 0) >= 0 {
		return "", ""
	}
	return string(b), udiff.GitBlobSHA(string(b))
}

// baseOf is a file at HEAD and its git blob id; empty for a file that
// isn't there (a new one).
func baseOf(root, rel string) (content, sha string) {
	out, err := exec.Command("git", "-C", root, "show", "HEAD:"+rel).Output() // #nosec G204 -- rel is passed as one argument
	if err != nil {
		return "", ""
	}
	sha, err = gitOutput(root, "rev-parse", "HEAD:"+rel)
	if err != nil {
		return "", ""
	}
	return string(out), sha
}

// changesFromPatch applies a unified diff to HEAD's versions of the files
// it touches, in a scratch directory, and returns each file's base and new
// content.
//
// A mode change isn't carried: a file whose only change is its mode is
// dropped, and the notice returned names the files whose mode was left out and
// the command that sets it (WP-948).
func changesFromPatch(root, patchFile string) ([]changeFile, string, error) {
	fi, err := os.Stat(patchFile)
	if err != nil {
		return nil, "", err
	}
	if fi.Size() > maxPatchFile {
		return nil, "", fmt.Errorf("the patch is %d MB; the most wp reads is %d MB", fi.Size()>>20, maxPatchFile>>20)
	}
	abs, err := filepath.Abs(patchFile)
	if err != nil {
		return nil, "", err
	}
	paths, err := patchPaths(root, abs)
	if err != nil {
		return nil, "", err
	}
	if len(paths) == 0 {
		return nil, "", errors.New("the patch changes no files")
	}
	tmp, err := os.MkdirTemp("", "wp-patch-")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var files []changeFile
	for _, p := range paths {
		rel, err := repoRel(root, filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return nil, "", err
		}
		cf := changeFile{Path: rel}
		cf.Base, cf.BaseSHA = baseOf(root, rel)
		if cf.BaseSHA != "" {
			dst := filepath.Join(tmp, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return nil, "", err
			}
			if err := os.WriteFile(dst, []byte(cf.Base), 0o600); err != nil {
				return nil, "", err
			}
		}
		files = append(files, cf)
	}
	cmd := exec.Command("git", "apply", "--whitespace=nowarn", abs) // #nosec G204 -- fixed arguments, abs is a path
	cmd.Dir = tmp
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("the patch doesn't apply to HEAD's files: %s", strings.TrimSpace(errb.String()))
	}
	for i := range files {
		b, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(files[i].Path))) // #nosec G304 -- inside our scratch directory
		switch {
		case errors.Is(err, os.ErrNotExist):
			files[i].Delete = true
		case err != nil:
			return nil, "", err
		default:
			if len(b) > maxChangeFile || bytes.IndexByte(b, 0) >= 0 {
				return nil, "", fmt.Errorf("%s is binary or over %d KB", files[i].Path, maxChangeFile>>10)
			}
			files[i].Content = string(b)
		}
	}
	modes, err := patchModes(root, abs)
	if err != nil {
		return nil, "", err
	}
	var kept []changeFile
	var modeOnly, modeKept []string
	for _, f := range files {
		_, has := modes[f.Path]
		if has && !f.Delete && f.BaseSHA != "" && udiff.GitBlobSHA(f.Content) == f.BaseSHA {
			modeOnly = append(modeOnly, f.Path)
			continue
		}
		if has {
			modeKept = append(modeKept, f.Path)
		}
		kept = append(kept, f)
	}
	if len(modeOnly) > 0 && len(kept) == 0 {
		return nil, "", fmt.Errorf("the patch only changes file modes, and change requests carry contents, not modes. File this instead:\n  %s", chmodCommand(modes, modeOnly))
	}
	notice := ""
	if all := append(modeOnly, modeKept...); len(all) > 0 {
		notice = fmt.Sprintf("Filed the content changes only: change requests carry contents, not modes, so the mode of %s was not carried. Set it with:\n  %s", strings.Join(all, ", "), chmodCommand(modes, all))
	}
	return kept, notice, nil
}

// patchModes are the files whose mode the patch sets to something other than a
// plain file's, or changes: path to the executable bit it ends with (git apply
// --summary).
func patchModes(root, patch string) (map[string]bool, error) {
	cmd := exec.Command("git", "-c", "core.quotepath=false", "apply", "--summary", patch) // #nosec G204 -- fixed arguments
	// From a subdirectory git leaves out the paths outside it.
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("can't read the patch: %s", strings.TrimSpace(errb.String()))
	}
	modes := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 6 && f[0] == "mode" && f[1] == "change" && f[3] == "=>":
			modes[strings.TrimSpace(strings.SplitN(line, f[4], 2)[1])] = f[4] == "100755"
		case len(f) >= 4 && f[0] == "create" && f[1] == "mode" && f[2] == "100755":
			modes[strings.TrimSpace(strings.SplitN(line, f[2], 2)[1])] = true
		}
	}
	return modes, nil
}

// chmodCommand is the request an agent files to set the modes of paths.
func chmodCommand(modes map[string]bool, paths []string) string {
	var cmds []string
	for _, on := range []bool{true, false} {
		var ps []string
		for _, p := range paths {
			if modes[p] == on {
				ps = append(ps, quoteArg(p))
			}
		}
		if len(ps) > 0 {
			flag := "-x"
			if on {
				flag = "+x"
			}
			cmds = append(cmds, "git update-index --chmod="+flag+" "+strings.Join(ps, " "))
		}
	}
	return "wp request run \"" + strings.Join(cmds, " && ") + " && git commit -m '…'\" -d WHY"
}

// patchPaths are the files a patch touches (git apply --numstat -z).
func patchPaths(root, patch string) ([]string, error) {
	cmd := exec.Command("git", "apply", "--numstat", "-z", patch) // #nosec G204 -- fixed arguments
	// Run from the repo root: from a subdirectory, git apply --numstat leaves out paths outside it.
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("can't read the patch: %s", strings.TrimSpace(errb.String()))
	}
	var paths []string
	for _, rec := range strings.Split(out.String(), "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			return nil, errors.New("the patch renames or copies files, which change requests don't carry; send the new file and delete the old one")
		}
		paths = append(paths, parts[2])
	}
	return paths, nil
}

func requestLs(c *cli, args []string) error {
	fs := c.flags()
	sf := scopeFlags{
		project: fs.String("project", "", "work in `PROJECT` (key, name or repo; me for personal requests)"),
		all:     fs.Bool("all-projects", false, "every project, not just this one"),
	}
	short(fs, "p", "project")
	short(fs, "A", "all-projects")
	open := fs.Bool("open", false, "open requests (the default)")
	answered := fs.Bool("answered", false, "decided, and you haven't been told yet")
	all := fs.Bool("all", false, "every state")
	mine := fs.Bool("mine", false, "only those for you")
	state := fs.String("state", "", "one `STATE` (approved, applied, failed, withdrawn…)")
	_ = open
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	switch {
	case *state != "":
		q.Set("state", *state)
	case *answered:
		q.Set("state", "answered")
	case *all:
		q.Set("state", "all")
	}
	if *mine || c.agentHeader() != "" {
		q.Set("mine", "1")
	}
	// The server filters on one project, so each project needs its own call.
	var refs []string
	switch {
	case sf.project != nil && strings.EqualFold(*sf.project, "me"):
		refs = []string{"me"}
	case sf.all != nil && *sf.all:
		refs = []string{""}
	default:
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		if sc.all() {
			refs = []string{""}
			break
		}
		for _, p := range sc.projects {
			refs = append(refs, p.Key)
		}
	}
	var merged reqList
	var rawOne json.RawMessage
	for _, ref := range refs {
		q.Set("project", ref)
		if ref == "" {
			q.Del("project")
		}
		one, err := c.call("GET", "/api/v1/requests?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		rawOne = one
		var l reqList
		_ = json.Unmarshal(one, &l)
		merged.Requests = append(merged.Requests, l.Requests...)
		merged.Total += l.Total
	}
	raw := rawOne
	if len(refs) > 1 {
		raw, _ = json.Marshal(merged)
	}
	return c.emit(raw, func() string {
		var l reqList
		_ = json.Unmarshal(raw, &l)
		if len(l.Requests) == 0 {
			if *answered {
				return "No answered requests."
			}
			return "No requests."
		}
		var b strings.Builder
		for _, r := range l.Requests {
			where := ""
			if r.Project != nil && len(refs) == 1 && refs[0] == "" {
				where = "  [" + r.Project.Key + "]"
			}
			fmt.Fprintf(&b, "%-10s %-9s %-10s %s%s\n", r.Ref, r.State, r.Kind, strings.ReplaceAll(r.Title, "…?", "…"), where)
		}
		if *answered {
			b.WriteString("\nwp request show CODE reads one (and counts as being told).\n")
		}
		return b.String()
	})
}

func requestShow(c *cli, args []string) error {
	fs := c.flags()
	diff := fs.Bool("diff", false, "include the diff of a change")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected a request code, like R-12")
	}
	return c.each(requestCodes(pos), true, func(code string) error {
		raw, err := c.call("GET", "/api/v1/requests/"+esc(code), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var r reqView
			_ = json.Unmarshal(raw, &r)
			return r.text(*diff)
		})
	})
}

// requestCodes reads a list of request codes (R-5 R-6, R-5,6,7, or 5 6 7):
// a bare number is R-N, the code people and agents see, not the request's
// id.
func requestCodes(pos []string) []string {
	codes := listArgs(pos)
	for i, code := range codes {
		if isDigits(code) {
			codes[i] = "R-" + code
		}
	}
	return codes
}

func requestWait(c *cli, args []string) error {
	fs := c.flags()
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("expected one request code, like R-12")
	}
	code := requestCodes(pos)[0]
	deadline := time.Now().Add(*timeout)
	for {
		secs := int(time.Until(deadline).Seconds())
		secs = min(secs, 20)
		path := "/api/v1/requests/" + esc(code)
		if secs > 0 {
			path += fmt.Sprintf("?wait=%d", secs)
		}
		raw, err := c.call("GET", path, nil)
		if err != nil {
			return err
		}
		var r reqView
		_ = json.Unmarshal(raw, &r)
		if r.State != "open" && r.State != "applying" {
			return c.emit(raw, func() string { return r.text(false) })
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s is still %s after %s", r.Ref, r.State, *timeout)
		}
		if r.State == "applying" {
			time.Sleep(2 * time.Second)
		}
	}
}

func requestWatch(c *cli, args []string) error {
	fs := c.flags()
	every := fs.Duration("check-every", 0, "how often url-ok/cmd-ok are checked (default 5m)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return c.usage("expected a request code and KIND=TARGET, like: wp request watch R-12 pr-merged=#15")
	}
	var specs []any
	for _, w := range pos[1:] {
		kind, target, ok := strings.Cut(w, "=")
		if !ok || target == "" {
			return c.usage("a condition is KIND=TARGET, not %q", w)
		}
		cond := map[string]any{"kind": kind, "target": target}
		if *every > 0 {
			cond["every"] = int(every.Seconds())
		}
		specs = append(specs, cond)
	}
	var raw []byte
	code := requestCodes(pos[:1])[0]
	for _, cond := range specs {
		if raw, err = c.call("POST", "/api/v1/requests/"+esc(code)+"/watch", cond); err != nil {
			return err
		}
	}
	return c.emit(raw, func() string {
		var r reqView
		_ = json.Unmarshal(raw, &r)
		return fmt.Sprintf("%s now watches:\n%s", r.Ref, watchLines(r.Watches))
	})
}

// watchLines lists a request's conditions and their last check.
func watchLines(ws []reqWatch) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "  %s", w.Summary)
		switch {
		case w.State == "satisfied":
			b.WriteString(" (satisfied)")
		case w.Note != "":
			fmt.Fprintf(&b, " (last check: %s)", w.Note)
		default:
			b.WriteString(" (not checked yet)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func requestWithdraw(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "`WHY` it is no longer needed, for the person to read")
	alias(fs, "note", "m")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected a request code, like R-12")
	}
	var body any
	if *note != "" {
		body = map[string]any{"note": *note}
	}
	return c.each(requestCodes(pos), false, func(code string) error {
		raw, err := c.call("POST", "/api/v1/requests/"+esc(code)+"/withdraw", body)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var r reqView
			_ = json.Unmarshal(raw, &r)
			return "Withdrew " + r.Ref + "."
		})
	})
}

func requestDecide(verb string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		var choices multiFlag
		if verb == "answer" {
			fs.Var(&choices, "choice", "a `CHOICE` to pick (repeat for several)")
		}
		note := fs.String("m", "", "a `NOTE` for the agent")
		alias(fs, "note", "m")
		var apply, rule *string
		if verb == "approve" {
			apply = fs.String("apply", "", "`HOW` to apply a change: runner or pr")
			rule = fs.String("rule", "", "a permission request: also add this allow `RULE` to the repo's .claude/settings.json by pull request (wp request show lists suggestions)")
		}
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("expected a request code, like R-12")
		}
		body := map[string]any{"decision": verb, "note": *note}
		codes := requestCodes(pos[:1])
		if verb == "answer" {
			text := joinArgs(pos[1:])
			body["answers"] = []map[string]any{{"choices": []string(choices), "text": text}}
			if len(choices) == 0 && text == "" {
				return c.usage("answer with text or --choice")
			}
		} else {
			codes = requestCodes(pos)
		}
		if apply != nil && *apply != "" {
			body["apply_path"] = *apply
		}
		if rule != nil && *rule != "" {
			body["rule"] = *rule
		}
		return c.each(codes, false, func(code string) error {
			raw, err := c.call("POST", "/api/v1/requests/"+esc(code)+"/decide", body)
			if err != nil {
				return err
			}
			return c.emit(raw, func() string {
				var r reqView
				_ = json.Unmarshal(raw, &r)
				if r.State == "withdrawn" && (verb == "done" || verb == "dismiss") {
					return "Withdrew " + r.Ref + " (deciding is for people; an agent's " + verb + " takes back its own request, with -m kept as the reason)."
				}
				return r.Ref + " is now " + r.State + "."
			})
		})
	}
}

func requestSnooze(c *cli, args []string) error {
	fs := c.flags()
	d := fs.Duration("for", time.Hour, "how long (0: bring it back)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected a request code, like R-12")
	}
	body := map[string]any{}
	if *d > 0 {
		body["minutes"] = int(math.Ceil(d.Minutes()))
	}
	return c.each(requestCodes(pos), false, func(code string) error {
		raw, err := c.call("POST", "/api/v1/requests/"+esc(code)+"/snooze", body)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var r reqView
			_ = json.Unmarshal(raw, &r)
			return "Snoozed " + r.Ref + "."
		})
	})
}

var _ = flag.ErrHelp
