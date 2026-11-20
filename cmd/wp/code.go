package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

// wp code: the per-checkout code index (ADR3). Everything here runs
// locally against .git/waypoint/code.db; no server, no login.
var codeCmd = &command{
	name: "code", group: "Knowledge", args: "find|show|risk|why|index …",
	summary: "search and read this checkout's code (local index)",
	about: `Use instead of a grep, sed -n and cat chain. The local index updates itself
before each command. Answers are short (about 2500 tokens; --budget raises
it) and say what was left out.`,
	examples: []string{"wp code find MergeReady", "wp code show internal/core/merge.go", "wp code show merge.go:MergeReady --callers"},
	subs: []*command{
		{name: "find", args: "QUERY… [--mode symbol|path|text] [--budget N]", summary: "find code by identifier, path or words",
			about: `QUERY is an identifier (MergeReady, Service.Merge: definitions, then
users), a path or glob (cmd/wp/), or words. Prints a digest per file, best
first, with matching lines and their symbols. HOTSPOT marks files that
change a lot; "N fixes" counts past bug-fix commits.`,
			examples: []string{"wp code find MergeReady", "wp code find 'retry backoff'", "wp code find 'cmd/wp/pr_*.go'"},
			run:      cmdCodeFind},
		{name: "show", args: "FILE | FILE:SYMBOL | FILE:START-END | FILE:LINE | SYMBOL", summary: "read a file outline, symbol or line range",
			about: `  FILE             outline with history, imports and co-changing files (a small
                   file prints in full)
  FILE:SYMBOL      the symbol's current lines, numbered (Name or Type.Name)
  FILE:START-END   those lines
  FILE:LINE        the symbol around that line
  SYMBOL           a symbol found anywhere

FILE may be a unique path suffix (merge.go).`,
			examples: []string{"wp code show internal/core/merge.go", "wp code show merge.go:MergeReady --callers", "wp code show cmd/wp/main.go:140-200"},
			run:      cmdCodeShow},
		{name: "index", args: "[--status|--rebuild|--upload]", summary: "update, inspect or rebuild the code index",
			about: `Stored in <git-dir>/waypoint/code.db, one per worktree. --upload sends the
per-file hotspot, fix and co-change summary to Waypoint; a clean default-
branch checkout does it daily by itself (WP_CODE_UPLOAD=off stops that).`,
			examples: []string{"wp code index --status", "wp code index --rebuild"},
			run:      cmdCodeIndex},
	},
}

// codeIndex opens the checkout's index and brings it up to date.
func (c *cli) codeIndex(dbPath string, update bool) (*codeindex.Index, codeindex.UpdateStats, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, codeindex.UpdateStats{}, err
	}
	ix, err := codeindex.Open(cwd, dbPath)
	if err != nil {
		return nil, codeindex.UpdateStats{}, err
	}
	if !update {
		return ix, codeindex.UpdateStats{}, nil
	}
	if ix.Status().Files == 0 {
		_, _ = fmt.Fprintln(c.err, "wp: building the code index for this checkout (first use; later runs take a moment)")
	}
	st, err := ix.Update(context.Background())
	if err != nil {
		_ = ix.Close()
		return nil, st, err
	}
	c.maybeUpload(ix) // a day-old summary of the default branch goes to the server (WP-182)
	return ix, st, nil
}

func (c *cli) codeFlags() (fs *flag.FlagSet, db *string) {
	fs = c.flags()
	db = fs.String("db", "", "index `FILE` (default <git-dir>/waypoint/code.db)")
	hideFlags(fs, "db")
	return fs, db
}

func cmdCodeFind(c *cli, args []string) error {
	fs, db := c.codeFlags()
	mode := fs.String("mode", "auto", "read the query as `MODE`: auto, symbol, path, text")
	budget := fs.Int("budget", codeindex.DefaultBudget, "answer size in `TOKENS`")
	perFile := fs.Int("per-file", 3, "lines shown per file")
	files := fs.Int("files", 12, "files considered")
	hideFlags(fs, "per-file", "files")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("what to find? e.g. wp code find MergeReady")
	}
	ix, _, err := c.codeIndex(*db, true)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	res, err := ix.Find(context.Background(), strings.Join(pos, " "), codeindex.FindOptions{Mode: *mode, Budget: *budget, PerFile: *perFile, Files: *files})
	if err != nil {
		return err
	}
	if c.asJSON {
		return c.codeJSON(res)
	}
	c.print(res.Render())
	return nil
}

func cmdCodeShow(c *cli, args []string) error {
	fs, db := c.codeFlags()
	budget := fs.Int("budget", 0, "answer size in `TOKENS` (default 2500 outline, 4000 code)")
	ctxLines := fs.Int("context", 0, "`N` lines of context")
	short(fs, "C", "context")
	outline := fs.Bool("outline", false, "outline even for a small file")
	full := fs.Bool("full", false, "the whole file")
	callers := fs.Bool("callers", false, "also list where the symbol is used")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("show what? FILE, FILE:SYMBOL, FILE:START-END or SYMBOL")
	}
	ix, _, err := c.codeIndex(*db, true)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	cwd, _ := os.Getwd()
	var out []*codeindex.ShowResult
	for _, ref := range pos {
		r := codeRef(ix, cwd, ref)
		res, err := ix.Show(context.Background(), r, codeindex.ShowOptions{Budget: *budget, Context: *ctxLines, Outline: *outline, Full: *full, Callers: *callers})
		if err != nil {
			return err
		}
		out = append(out, res)
	}
	if c.asJSON {
		if len(out) == 1 {
			return c.codeJSON(out[0])
		}
		return c.codeJSON(out)
	}
	for i, res := range out {
		if i > 0 {
			c.println()
		}
		c.print(res.Render(*budget))
	}
	return nil
}

func cmdCodeIndex(c *cli, args []string) error {
	fs, db := c.codeFlags()
	status := fs.Bool("status", false, "show what it holds, without updating")
	rebuild := fs.Bool("rebuild", false, "discard and rebuild the index")
	upload := fs.Bool("upload", false, "send this checkout's summary to Waypoint")
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", pos[0])
	}
	ix, _, err := c.codeIndex(*db, false)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	var st codeindex.UpdateStats
	switch {
	case *status:
	case *rebuild:
		if st, err = ix.Rebuild(context.Background()); err != nil {
			return err
		}
	default:
		if st, err = ix.Update(context.Background()); err != nil {
			return err
		}
	}
	if *upload {
		if err := c.uploadSummary(ix, sf); err != nil {
			return err
		}
	}
	ss := ix.Status()
	if c.asJSON {
		return c.codeJSON(map[string]any{"status": ss, "update": st})
	}
	if !*status {
		fmt.Fprintf(c.out, "indexed in %s (read %s, write %s, history %s): %d files (%d new, %d changed, %d removed), %d new commits\n",
			st.Duration.Round(time.Millisecond), st.ScanTime.Round(time.Millisecond), st.WriteTime.Round(time.Millisecond), st.GitTime.Round(time.Millisecond), st.Files, st.Added, st.Changed, st.Removed, st.NewCommits)
	}
	fmt.Fprintf(c.out, "index   %s (%.1f MB, schema %d)\n", ss.DBPath, float64(ss.DBBytes)/1e6, ss.Schema)
	fmt.Fprintf(c.out, "files   %d, symbols %d, imports %d (%d resolved), %d unreadable or generated skipped\n", ss.Files, ss.Symbols, ss.Imports, ss.ImportsLinked, ss.Skipped)
	fmt.Fprintf(c.out, "history %d commits, up to %s\n", ss.Commits, short12(ss.HistoryHead))
	if !ss.Updated.IsZero() {
		fmt.Fprintf(c.out, "updated %s (last write took %d ms)\n", ss.Updated.Format(time.RFC3339), ss.LastUpdateMS)
	}
	return nil
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "none"
	}
	return s
}

func (c *cli) codeJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	c.println(string(b))
	return nil
}

// codeRef turns a FILE[:SPEC] argument naming a file relative to where we
// are (or absolute) into one relative to the checkout root; anything else
// (a bare symbol, a path suffix) is passed on as it is.
func codeRef(ix *codeindex.Index, cwd, ref string) string {
	file, spec, hasSpec := strings.Cut(ref, ":")
	if fi, err := os.Stat(file); err == nil && fi.Mode().IsRegular() {
		if rel, ok := ix.RelPath(cwd, file); ok {
			if hasSpec {
				return rel + ":" + spec
			}
			return rel
		}
	}
	return ref
}
