package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// wp usage sync: the Usage tab's data (WP-D20).

var usageSyncCmd = &command{
	name: "sync", args: "[--all] [--since 30d] [--dry-run] [--dir CLAUDE_CONFIG_DIR]",
	summary: "upload what Claude Code sessions used, read from its transcripts",
	about: `Counts what each session used from Claude Code's transcripts and uploads
hourly records: tokens per model, tool, MCP, skill and command use, Bash
command heads (never arguments) and permission refusals. Risky commands
(recursive deletes, force pushes, production, disks: wp usage risky) and
refused ones go with their start, redacted. How wp itself is used goes as
counts per command: help lookups, flag names (never their values), classes
of usage error and which commands follow (wp usage cli). Prompts, replies,
file contents and tool output stay here. For each task a session takes
(wp next, wp take) it sends one row: the calls until wp ship, done or
release by what they were for (finding files, searching in one, reading,
editing), before and after the first edit, the context size at both, and
the repo-relative names of the files touched. Also reports what is configured
(MCP servers, skills, plugins, hooks by name). Only what is new is read;
wp hook claude runs it after every turn. Shown on Agents → Usage.`,
	examples: []string{"wp usage sync --all --dry-run"},
	run:      cmdUsageSync,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageSyncCmd)
}

const defaultSyncSince = 7 * 24 * time.Hour

func cmdUsageSync(c *cli, args []string) error {
	fs := c.flags()
	all := fs.Bool("all", false, "every transcript, however old")
	since := fs.String("since", "", "transcripts changed within `DURATION` (default 7d)")
	hook := fs.Bool("hook", false, "(hidden) run from a Claude Code hook: the session comes on stdin")
	dry := fs.Bool("dry-run", false, "count only; upload nothing")
	dir := fs.String("dir", "", "Claude Code config `DIR` (default CLAUDE_CONFIG_DIR, ~/.claude)")
	noInv := fs.Bool("no-inventory", false, "skip the configured MCP servers, skills…")
	noArchive := fs.Bool("no-archive", false, "skip the transcript archive upload and tell the server the archive is off, as WP_USAGE_ARCHIVE=off does")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *hook {
		usageHook(c, os.Stdin, *dir)
		return nil // a hook must never fail the session
	}
	o := usageSyncOptions{All: *all, DryRun: *dry, Dir: *dir, Inventory: !*noInv, NoArchive: *noArchive, Out: c.out}
	switch {
	case *all && *since == "":
	default:
		d, err := parseSince(*since)
		if err != nil {
			return c.usage("%v", err)
		}
		o.Since = d
		if d == 0 {
			o.Since = defaultSyncSince
		}
	}
	if c.asJSON {
		o.Out = nil
	}
	res, err := usageSync(c, o)
	if errors.Is(err, errUsageBusy) {
		c.println("Another wp usage sync is running; it will pick this up.")
		return nil
	}
	if res != nil {
		raw, _ := json.Marshal(res)
		_ = c.emit(raw, func() string { return usageSyncText(res) })
	}
	return err
}

// usageHook syncs the session a Claude Code hook names: its transcript and
// its subagents'. Silent, and never an error.
func usageHook(c *cli, stdin io.Reader, dir string) {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil || in.TranscriptPath == "" ||
		!strings.HasSuffix(in.TranscriptPath, ".jsonl") || c.server() == "" || c.token() == "" {
		return
	}
	_, _ = usageSync(c, usageSyncOptions{Hook: true, Dir: dir, Files: []string{in.TranscriptPath}})
}

func usageSyncText(r *usageSyncResult) string {
	var b strings.Builder
	n := func(v int64) string { return commas(v) }
	fmt.Fprintf(&b, "%s on %s: read %s of %s transcripts (%s): %s messages, %s tool calls, %s refusals, %s risky commands, %s sessions.\n",
		map[bool]string{true: "Dry run", false: "Synced"}[r.DryRun], r.Machine, n(int64(r.FilesRead)), n(int64(r.Files)), bytesText(r.Bytes),
		n(r.Messages), n(r.ToolCalls), n(r.Refusals), n(int64(r.Risky)), n(int64(r.Sessions)))
	if t := r.Tokens; t.Input+t.Output+t.CacheWrite+t.CacheRead > 0 {
		fmt.Fprintf(&b, "Tokens: %s in, %s out, %s written to and %s read from the cache.\n", n(t.Input), n(t.Output), n(t.CacheWrite), n(t.CacheRead))
	}
	if r.DryRun {
		b.WriteString("Nothing was uploaded or remembered.\n")
	} else if r.Chunks > 0 {
		fmt.Fprintf(&b, "Uploaded %d batches (%d already known to the server).\n", r.Chunks, r.Duplicate)
	} else {
		b.WriteString("Nothing new to upload.\n")
	}
	if a := r.Archive; a != nil {
		if a.Stopped == archiveBusy && a.Files == 0 {
			b.WriteString("Transcript archive: another pass is running; it uploads these too.\n")
		} else {
			fmt.Fprintf(&b, "Transcript archive: %s stored in %d pieces (%s sent compressed); %s still waiting in %d files.\n",
				bytesText(a.RawBytes), a.Pieces, bytesText(a.SentBytes), bytesText(a.PendingBytes), a.PendingFiles)
			for _, k := range slices.Sorted(maps.Keys(a.Dropped)) {
				fmt.Fprintf(&b, "  not archived, counted: %s\n", archiveDropText(k, a.Dropped[k]))
			}
			if a.Stopped != "" {
				fmt.Fprintf(&b, "  stopped early: %s\n", a.Stopped)
			}
			if a.OlderFiles > 0 {
				fmt.Fprintf(&b, "  %d older transcript%s (%s) never uploaded, outside the archive window (the last 30 days unless --since says more): wp usage sync --all archives them\n",
					a.OlderFiles, map[bool]string{true: "", false: "s"}[a.OlderFiles == 1], bytesText(a.OlderBytes))
			}
		}
	}
	if r.ArchiveError != "" {
		fmt.Fprintf(&b, "Transcript archive failed: %s\n", r.ArchiveError)
	}
	if r.Skipped > 0 {
		fmt.Fprintf(&b, "Skipped %s lines of kinds this wp doesn't read", n(r.Skipped))
		type kv struct {
			k string
			v int64
		}
		var kinds []kv
		for k, v := range r.Unknown {
			kinds = append(kinds, kv{k, v})
		}
		sort.Slice(kinds, func(i, j int) bool { return kinds[i].v > kinds[j].v })
		var parts []string
		for _, e := range kinds[:min(len(kinds), 6)] {
			parts = append(parts, fmt.Sprintf("%s ×%s", e.k, n(e.v)))
		}
		if len(parts) > 0 {
			b.WriteString(": " + strings.Join(parts, ", "))
		}
		b.WriteString(". (Claude Code's format changes; update wp if this grows.)\n")
	}
	if r.Traces > 0 || r.TracesDropped > 0 {
		fmt.Fprintf(&b, "Task traces: %s sent", n(int64(r.Traces)))
		if r.TracesDropped > 0 {
			fmt.Fprintf(&b, ", %s the server could not place (a task it doesn't have, or you can't read)", n(int64(r.TracesDropped)))
		}
		b.WriteString(".\n")
	}
	if r.SharedIDs > 0 {
		fmt.Fprintf(&b, "%s messages appear in more than one transcript.\n", n(r.SharedIDs))
	}
	if len(r.Inventory) > 0 {
		var parts []string
		for _, k := range []string{"mcp", "skill", "command", "agent", "plugin", "hook"} {
			if r.Inventory[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", r.Inventory[k], k))
			}
		}
		b.WriteString("Configured here: " + strings.Join(parts, ", ") + ".\n")
	}
	fmt.Fprintf(&b, "Took %s.\n", r.Elapsed)
	return b.String()
}

func commas(v int64) string {
	s := fmt.Sprint(v)
	for i := len(s) - 3; i > 0; i -= 3 {
		if s[i-1] == '-' {
			break
		}
		s = s[:i] + "," + s[i:]
	}
	return s
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
