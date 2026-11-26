package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

var usageSubagentsCmd = &command{
	name: "subagents", args: "[--since 3d] [--session ID[,ID]] [--dir CLAUDE_CONFIG_DIR] [--all-projects] [--spans FILE] [-v] [--json]",
	summary: "how much subagent work is waiting and retrying",
	about: `Reads this machine's Claude Code transcripts of subagents (plain and gzipped,
nested ones included) and counts, per tool call and per minute, the work that
was waiting or retrying instead of progress:
  refused agent starts   an Agent call answered "Concurrent subagent limit reached"
  refused waits          a sleep Claude Code blocked, or a wait loop the kit hook refused
  no-op polls            a command that only prints a literal or succeeds (echo waiting, true)
  repeated tests         the same test command run again with nothing changed in between
                         (a failed edit changes nothing)
  work after a hand-back calls after the agent's first delivered report that
                         nobody asked for: not "The coordinator sent a message"
It also shows, without counting them as loss: the share re-tasked, the
hand-back reminders ([handback-send-enforce]), and the time spent inside
deliberate waits (sleep, until/while loops, wp pr wait, Monitor).
A call's minutes run to that agent's next call, capped at 30; the in-call
minutes run from the call to its result. A call counts in one pattern only
(the first in the order above). Default scope is this repository's Claude Code
project folders, its worktrees included; --all-projects reads every project.
--spans FILE writes a row per counted call so a count can be audited. -v lists
the five agents with the most calls in each pattern and the no-op commands.`,
	examples: []string{"wp report behavior subagents --since 3d", "wp report behavior subagents --session cd44a3de,af965eb1 --spans /tmp/spans.tsv -v"},
	run:      cmdUsageSubagents,
}

func cmdUsageSubagents(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "transcripts changed within `DURATION` (default: all)")
	sess := fs.String("session", "", "only sessions whose id starts with one of these `PREFIXES` (comma separated)")
	dir := fs.String("dir", "", "Claude Code config `DIR` (default CLAUDE_CONFIG_DIR, ~/.claude)")
	all := fs.Bool("all-projects", false, "every project on this machine, not only this repository")
	spans := fs.String("spans", "", "write one TSV row per counted call to `FILE`")
	verbose := fs.Bool("verbose", false, "the worst agents per pattern and the no-op commands")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	d, err := parseSince(*since)
	if err != nil {
		return c.usage("%v", err)
	}
	root := claudeDir(*dir)
	projects := filepath.Join(root, "projects")
	if _, err := os.Stat(projects); err != nil {
		return fmt.Errorf("no Claude Code transcripts at %s (use --dir if your config lives elsewhere)", projects)
	}
	var only []string
	for _, s := range strings.Split(*sess, ",") {
		if s = strings.TrimSpace(s); s != "" {
			only = append(only, s)
		}
	}
	var names []string
	if !*all {
		top, err := gitOutput("", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("run wp report behavior subagents in a git checkout of the repository, or give --all-projects: %w", err)
		}
		names = saProjectNames(top)
	}
	files := saDiscover(projects, names, only, d)
	rep, err := measureSubagents(files)
	if err != nil {
		return err
	}
	rep.Scope = "this repository's projects"
	if *all {
		rep.Scope = "all projects"
	}
	if *spans != "" {
		if err := saWriteSpans(*spans, rep); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(rep)
	return c.emit(raw, func() string { return saText(rep, *verbose) })
}

// saProjectNames are the project folders of a repository: its checkout, its
// worktrees' checkouts, and the ones Claude Code names for .claude/worktrees.
func saProjectNames(top string) []string {
	main := top
	if common, err := gitOutput(top, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && filepath.Base(common) == ".git" {
		main = filepath.Dir(common)
	}
	set := map[string]bool{projectDirName(top): true, projectDirName(main): true, projectDirName(main) + "--claude-worktrees-": true}
	if out, err := gitOutput(top, "worktree", "list", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				set[projectDirName(filepath.Clean(p))] = true
			}
		}
	}
	var names []string
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type saFile struct {
	Path, Session string
	Gz            bool
}

// saDiscover lists subagent transcripts, plain or .gz, at any depth under
// <project>/<session>/subagents. A name ending "-" matches by prefix. A
// session with both forms of a file reads the plain one.
func saDiscover(projects string, names, only []string, since time.Duration) []saFile {
	cut := time.Time{}
	if since > 0 {
		cut = time.Now().Add(-since)
	}
	entries, _ := os.ReadDir(projects)
	var out []saFile
	have := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !saProjectWanted(e.Name(), names) {
			continue
		}
		sessions, _ := os.ReadDir(filepath.Join(projects, e.Name()))
		for _, s := range sessions {
			if !s.IsDir() || !saSessionWanted(s.Name(), only) {
				continue
			}
			base := filepath.Join(projects, e.Name(), s.Name(), "subagents")
			_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "agent-") {
					return nil
				}
				stem, gz := strings.CutSuffix(d.Name(), ".jsonl.gz")
				if !gz {
					var ok bool
					if stem, ok = strings.CutSuffix(d.Name(), ".jsonl"); !ok {
						return nil
					}
				}
				if fi, err := d.Info(); err != nil || !fi.Mode().IsRegular() || (!cut.IsZero() && !fi.ModTime().After(cut)) {
					return nil
				}
				key := filepath.Join(filepath.Dir(p), stem)
				if have[key] && gz {
					return nil
				}
				if !gz && have[key] {
					for i := range out {
						if filepath.Join(filepath.Dir(out[i].Path), saStem(out[i].Path)) == key {
							out[i] = saFile{p, s.Name(), false}
						}
					}
					return nil
				}
				have[key] = true
				out = append(out, saFile{p, s.Name(), gz})
				return nil
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func saStem(p string) string {
	n := filepath.Base(p)
	n = strings.TrimSuffix(n, ".gz")
	return strings.TrimSuffix(n, ".jsonl")
}

func saProjectWanted(name string, names []string) bool {
	if names == nil {
		return true
	}
	for _, n := range names {
		if name == n || strings.HasSuffix(n, "-") && strings.HasPrefix(name, n) {
			return true
		}
	}
	return false
}

func saSessionWanted(id string, only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, o := range only {
		if strings.HasPrefix(id, o) {
			return true
		}
	}
	return false
}

// saMeta reads agent-<id>.meta.json beside a transcript.
func saMeta(path string) (typ, desc string) {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), saStem(path)+".meta.json")) // #nosec G304 -- beside a transcript Claude Code wrote
	if err != nil {
		return "", ""
	}
	var m struct {
		AgentType   string `json:"agentType"`
		Description string `json:"description"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	return truncRunes(m.AgentType, 60), truncRunes(m.Description, 100)
}

type saRow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Agents      int     `json:"agents"`
	Calls       int     `json:"calls"`
	Minutes     float64 `json:"minutes"`
	InCall      float64 `json:"in_call_minutes"`
	PctCalls    float64 `json:"pct_of_calls"`
	PctMinutes  float64 `json:"pct_of_minutes"`
	agentSet    map[string]bool
	agentCounts map[string]int
}

type saSessionRow struct {
	Session  string         `json:"session"`
	Agents   int            `json:"agents"`
	Calls    int            `json:"calls"`
	Minutes  float64        `json:"minutes"`
	ByKey    map[string]int `json:"loss_calls_by_pattern"`
	Loss     int            `json:"loss_calls"`
	LossMin  float64        `json:"loss_minutes"`
	PctCalls float64        `json:"pct_of_calls"`
	PctMin   float64        `json:"pct_of_minutes"`
}

type saWorst struct {
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Type    string `json:"type,omitempty"`
	Desc    string `json:"description,omitempty"`
	Calls   int    `json:"calls"`
}

type saReport struct {
	Scope       string  `json:"scope"`
	Transcripts int     `json:"transcripts"`
	Gzipped     int     `json:"gzipped"`
	Unreadable  int     `json:"unreadable"`
	Sessions    int     `json:"sessions"`
	Agents      int     `json:"agents"`
	Calls       int     `json:"calls"`
	Minutes     float64 `json:"minutes"`
	InCall      float64 `json:"in_call_minutes"`
	From        string  `json:"from,omitempty"`
	To          string  `json:"to,omitempty"`

	Loss       saRow           `json:"loss_total"`
	Patterns   []*saRow        `json:"patterns"`
	Post       []*saRow        `json:"after_hand_back"`
	Waits      []*saRow        `json:"waiting_inside_calls"`
	PerSession []*saSessionRow `json:"per_session"`

	Nudges struct {
		Turns, Agents, Duplicates int
	} `json:"hand_back_nudges"`
	Refused struct {
		Starts, Retried60s, BeforeAnyChildFinished int
	} `json:"refused_starts"`
	Noop struct {
		Calls, ExtraBeyondEchoWaiting int
		Distinct                      []string
	} `json:"no_op_polls"`
	Worst map[string][]saWorst `json:"worst_agents"`

	spans []saSpan
}

type saSpan struct {
	Session, Agent, Pattern, Sub, Tool, Cmd, Path string
	Line                                          int
	TS                                            float64
	Mins                                          float64
}

var saLabels = map[string]string{
	patRefusedStart: "refused agent start",
	patBlockedSleep: "refused wait: sleep Claude Code blocked",
	patBlockedLoop:  "refused wait: unbounded loop the hook refused",
	patNoop:         "no-op poll",
	patRepeatTest:   "repeated test, nothing changed",
	patPostHandback: "work after hand-back, not asked for",
}

func newSaRow(key, label string) *saRow {
	return &saRow{Key: key, Label: label, agentSet: map[string]bool{}, agentCounts: map[string]int{}}
}

func (r *saRow) add(agent string, c *saCall) {
	r.agentSet[agent] = true
	r.agentCounts[agent]++
	r.Calls++
	r.Minutes += c.Mins
	r.InCall += c.InCall
}

func measureSubagents(files []saFile) (*saReport, error) {
	agents := make([]*saAgent, len(files))
	var wg sync.WaitGroup
	next := make(chan int)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f := files[i]
				a := &saAgent{Session: f.Session, Path: f.Path, Gz: f.Gz}
				a.ID = strings.TrimPrefix(saStem(f.Path), "agent-")
				a.Type, a.Desc = saMeta(f.Path)
				if saReadAgent(f.Path, a) == nil {
					agents[i] = a
				}
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()

	rep := &saReport{Worst: map[string][]saWorst{}}
	rows := map[string]*saRow{}
	for _, k := range saLossOrder {
		rows[k] = newSaRow(k, saLabels[k])
		rep.Patterns = append(rep.Patterns, rows[k])
	}
	loss := newSaRow("loss", "all of the above (each call once)")
	for _, k := range []string{resumeRetasked, resumeBackground, resumeNudged, resumeOther} {
		rows["post:"+k] = newSaRow("post:"+k, k)
	}
	postAll := newSaRow("post", "all calls after a delivered hand-back")
	rep.Post = append(rep.Post, postAll, rows["post:"+resumeRetasked], rows["post:"+resumeBackground], rows["post:"+resumeNudged], rows["post:"+resumeOther])
	wFg, wBg := newSaRow("wait-fg", "foreground wait calls"), newSaRow("wait-bg", "background wait calls")
	rep.Waits = []*saRow{wFg, wBg}
	sess := map[string]*saSessionRow{}
	sessAgents := map[string]map[string]bool{}
	nudgeAgents := map[string]bool{}
	noopSeen := map[string]bool{}
	var first, last float64
	for _, a := range agents {
		if a == nil {
			rep.Unreadable++
			continue
		}
		rep.Transcripts++
		if a.Gz {
			rep.Gzipped++
		}
		if len(a.Calls) == 0 {
			continue
		}
		key := a.Session + "/" + a.ID
		sr := sess[a.Session]
		if sr == nil {
			sr = &saSessionRow{Session: a.Session, ByKey: map[string]int{}}
			sess[a.Session] = sr
			sessAgents[a.Session] = map[string]bool{}
		}
		sessAgents[a.Session][a.ID] = true
		rep.Agents++
		if first == 0 || a.FirstTS < first {
			first = a.FirstTS
		}
		last = max(last, a.LastTS)
		if a.Nudges > 0 {
			rep.Nudges.Turns += a.Nudges
			rep.Nudges.Duplicates += a.DupNudges
			nudgeAgents[key] = true
		}
		rep.Refused.Retried60s += a.RefusedRetried60s
		rep.Refused.BeforeAnyChildFinished += a.RefusedBeforeChild
		for _, c := range a.Calls {
			rep.Calls++
			rep.Minutes += c.Mins
			rep.InCall += c.InCall
			sr.Calls++
			sr.Minutes += c.Mins
			switch c.Wait {
			case "foreground":
				wFg.add(key, c)
			case "background":
				wBg.add(key, c)
			}
			if c.Pattern == "" {
				continue
			}
			rows[c.Pattern].add(key, c)
			loss.add(key, c)
			sr.Loss++
			sr.LossMin += c.Mins
			sr.ByKey[c.Pattern]++
			if c.Pattern == patNoop {
				rep.Noop.Calls++
				if c.Extra {
					rep.Noop.ExtraBeyondEchoWaiting++
				}
				n := truncRunes(strings.Join(strings.Fields(c.Cmd), " "), 120)
				if !noopSeen[n] {
					noopSeen[n] = true
					rep.Noop.Distinct = append(rep.Noop.Distinct, n)
				}
			}
			rep.spans = append(rep.spans, saSpan{a.Session, a.ID, c.Pattern, c.Sub, c.Name, c.Cmd, a.Path, c.Line, c.TS, c.Mins})
		}
		for k, s := range a.PostHandbackByKind {
			r := rows["post:"+k]
			r.agentSet[key] = true
			r.Calls += s.Calls
			r.Minutes += s.Mins
			r.InCall += s.InCall
			postAll.agentSet[key] = true
			postAll.Calls += s.Calls
			postAll.Minutes += s.Mins
			postAll.InCall += s.InCall
		}
	}
	sort.Strings(rep.Noop.Distinct)
	rep.Nudges.Agents = len(nudgeAgents)
	rep.Refused.Starts = rows[patRefusedStart].Calls
	for _, r := range append(append(append([]*saRow{loss}, rep.Patterns...), rep.Post...), rep.Waits...) {
		r.Agents = len(r.agentSet)
	}
	rep.Loss = *loss
	for _, r := range append(append([]*saRow{&rep.Loss}, rep.Patterns...), rep.Post...) {
		r.PctCalls = pctOf(float64(r.Calls), float64(rep.Calls))
		r.PctMinutes = pctOf(r.Minutes, rep.Minutes)
	}
	for _, r := range rep.Waits {
		r.PctCalls = pctOf(float64(r.Calls), float64(rep.Calls))
		r.PctMinutes = pctOf(r.InCall, rep.InCall)
	}
	for s, sr := range sess {
		sr.Agents = len(sessAgents[s])
		sr.PctCalls = pctOf(float64(sr.Loss), float64(sr.Calls))
		sr.PctMin = pctOf(sr.LossMin, sr.Minutes)
		rep.PerSession = append(rep.PerSession, sr)
	}
	sort.Slice(rep.PerSession, func(i, j int) bool { return rep.PerSession[i].Session < rep.PerSession[j].Session })
	rep.Sessions = len(sess)
	meta := map[string]*saAgent{}
	for _, a := range agents {
		if a != nil {
			meta[a.Session+"/"+a.ID] = a
		}
	}
	for _, r := range rep.Patterns {
		type kv struct {
			k string
			n int
		}
		var l []kv
		for k, n := range r.agentCounts {
			l = append(l, kv{k, n})
		}
		sort.Slice(l, func(i, j int) bool {
			if l[i].n != l[j].n {
				return l[i].n > l[j].n
			}
			return l[i].k < l[j].k
		})
		for _, e := range l[:min(5, len(l))] {
			a := meta[e.k]
			rep.Worst[r.Key] = append(rep.Worst[r.Key], saWorst{Session: a.Session[:min(8, len(a.Session))], Agent: a.ID, Type: a.Type, Desc: a.Desc, Calls: e.n})
		}
	}
	if first > 0 {
		rep.From = time.Unix(int64(first), 0).UTC().Format("2006-01-02")
		rep.To = time.Unix(int64(last), 0).UTC().Format("2006-01-02")
	}
	return rep, nil
}

func pctOf(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * a / b
}

func saWriteSpans(path string, rep *saReport) error {
	var b strings.Builder
	b.WriteString("session\tagent\tjsonl_line\ttimestamp\tpattern\tsub_kind\tminutes\tcommand\ttool\tfile\n")
	clean := func(s string) string { return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s) }
	for _, s := range rep.spans {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%s\t%s\t%.2f\t%s\t%s\t%s\n", s.Session, s.Agent, s.Line,
			time.Unix(int64(s.TS), 0).UTC().Format(time.RFC3339), s.Pattern, clean(s.Sub), s.Mins, clean(truncRunes(s.Cmd, 200)), s.Tool, s.Path)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func saText(r *saReport, verbose bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scope: %s. Denominators: %s subagent calls by %d agents in %d sessions, %.0f minutes to the next call (%.0f minutes inside the calls), %s to %s; %d transcripts read, %d of them gzipped",
		r.Scope, commas(int64(r.Calls)), r.Agents, r.Sessions, r.Minutes, r.InCall, r.From, r.To, r.Transcripts, r.Gzipped)
	if r.Unreadable > 0 {
		fmt.Fprintf(&b, ", %d unreadable", r.Unreadable)
	}
	b.WriteString(".\n\n")
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	row := func(r *saRow) {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%.0f\t%.1f\t%.1f%%\t%.1f%%\n", r.Label, r.Agents, commas(int64(r.Calls)), r.Minutes, r.InCall, r.PctCalls, r.PctMinutes)
	}
	_, _ = fmt.Fprintln(tw, "What was lost\tagents\tcalls\tminutes\tin-call min\t% of calls\t% of minutes")
	for _, p := range r.Patterns {
		row(p)
	}
	row(&r.Loss)
	_ = tw.Flush()
	b.WriteString("\nAfter a delivered hand-back (all of it, whatever else a call was counted as):\n")
	tw = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	for _, p := range r.Post {
		row(p)
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\nHand-back reminders ([handback-send-enforce]): %d turns to %d agents; %d came after a report had already been delivered.\n", r.Nudges.Turns, r.Nudges.Agents, r.Nudges.Duplicates)
	fmt.Fprintf(&b, "Refused agent starts: %d; %d followed by another start from the same agent within 60 s; %d before any child-finished notice reached that agent.\n", r.Refused.Starts, r.Refused.Retried60s, r.Refused.BeforeAnyChildFinished)
	fmt.Fprintf(&b, "No-op polls: %d, of which %d the narrow `echo waiting` rule would not catch.\n", r.Noop.Calls, r.Noop.ExtraBeyondEchoWaiting)
	b.WriteString("\nWaiting inside a call (deliberate waits, not counted as loss; minutes are in-call):\n")
	tw = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	for _, p := range r.Waits {
		_, _ = fmt.Fprintf(tw, "%s\t%d agents\t%s calls\t%.0f in-call min\t%.1f%% of in-call minutes\n", p.Label, p.Agents, commas(int64(p.Calls)), p.InCall, p.PctMinutes)
	}
	_ = tw.Flush()
	b.WriteString("\nPer session (lost calls and minutes, each call counted once):\n")
	tw = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "session\tagents\tcalls\tminutes\trefused starts\trefused waits\tno-op polls\trepeated tests\tafter hand-back\t% of calls\t% of minutes")
	for _, s := range r.PerSession {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%.0f\t%d\t%d\t%d\t%d\t%d\t%.1f%%\t%.1f%%\n", s.Session[:min(8, len(s.Session))], s.Agents, commas(int64(s.Calls)), s.Minutes,
			s.ByKey[patRefusedStart], s.ByKey[patBlockedSleep]+s.ByKey[patBlockedLoop], s.ByKey[patNoop], s.ByKey[patRepeatTest], s.ByKey[patPostHandback], s.PctCalls, s.PctMin)
	}
	_ = tw.Flush()
	if verbose {
		for _, p := range r.Patterns {
			w := r.Worst[p.Key]
			if len(w) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\nWorst agents, %s:\n", p.Label)
			for _, x := range w {
				fmt.Fprintf(&b, "  %s %s  %d calls  %s %s\n", x.Session, x.Agent, x.Calls, x.Type, x.Desc)
			}
		}
		b.WriteString("\nDistinct no-op commands:\n")
		for _, n := range r.Noop.Distinct {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	}
	return b.String()
}
