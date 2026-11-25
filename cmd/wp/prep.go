package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

var prepCmd = &command{
	name: "prep", group: "Knowledge", args: "KEY… | show KEY…",
	summary: "prepare a queued task: a free model points at where to look",
	about: `Sends what the task points at in this checkout to Waypoint, where a free
model picks files, functions, tests, earlier tasks and open questions. It
only points; no plan or advice. wp next, show and brief print the result.
Runners do this for you every 15 minutes (wp runner --no-prep stops it).`,
	examples: []string{"wp prep WP-212", "wp prep show WP-212"},
	run:      cmdPrep,
	subs: []*command{
		{name: "show", args: "KEY...", summary: "print a task's prep",
			examples: []string{"wp prep show WP-212"}, run: cmdPrepShow},
	},
}

// prepEvery is how often a runner asks which tasks to prepare.
const prepEvery = 15 * time.Minute

// Mirrors of internal/core's PrepBundle and TaskPrep (cmd/wp talks JSON).
type prepBundle struct {
	Repo    string       `json:"repo"`
	Commit  string       `json:"commit"`
	Machine string       `json:"machine"`
	Files   []prepFile   `json:"files"`
	Bodies  []prepBody   `json:"bodies,omitempty"`
	Tests   []string     `json:"tests,omitempty"`
	Docs    []string     `json:"docs,omitempty"`
	Words   []string     `json:"words,omitempty"`
	History []prepCommit `json:"history,omitempty"`
	Index   *prepSourceN `json:"index,omitempty"`
}

type prepCommit struct {
	Subject string   `json:"subject"`
	Keys    []string `json:"keys"`
	Files   []string `json:"files"`
	At      int64    `json:"at,omitempty"`
}

type prepSourceN struct {
	Matched int `json:"matched"`
}

type prepFile struct {
	Path    string       `json:"path"`
	Lines   int          `json:"lines"`
	Why     []string     `json:"why,omitempty"`
	Symbols []prepSymbol `json:"symbols,omitempty"`
}

type prepSymbol struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type prepBody struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Start int    `json:"start"`
	Text  string `json:"text"`
}

type prepPick struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Why  string `json:"why"`
}

type prepView struct {
	Key       string     `json:"key"`
	State     string     `json:"state"`
	Model     string     `json:"model"`
	Repo      string     `json:"repo"`
	Commit    string     `json:"commit"`
	Machine   string     `json:"machine"`
	Notes     *prepNotes `json:"notes"`
	Dropped   int        `json:"dropped"`
	Error     string     `json:"error"`
	HeldOut   bool       `json:"held_out"`
	Stale     bool       `json:"stale"`
	ShownAt   *time.Time `json:"shown_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Evidence is the server's to give (WP-ADR157): only asked for here.
	Evidence *prepEvidence `json:"evidence,omitempty"`
}

// prepEvidence mirrors internal/core's PrepEvidence.
type prepEvidence struct {
	Evidence []struct {
		Ref      string `json:"ref"`
		Kind     string `json:"kind"`
		Title    string `json:"title"`
		Category string `json:"category"`
		Date     string `json:"date"`
		By       string `json:"by"`
		BearsOn  string `json:"bears_on"`
		// ChangedSince: the record was edited after it was matched.
		ChangedSince bool `json:"changed_since,omitempty"`
	} `json:"evidence"`
	Claims []struct {
		Quote        string `json:"quote"`
		Test         string `json:"test"`
		Ref          string `json:"ref,omitempty"`
		RefKind      string `json:"ref_kind,omitempty"`
		ChangedSince bool   `json:"changed_since,omitempty"`
	} `json:"claims"`
}

type prepNotes struct {
	Files   []prepPick `json:"files"`
	Symbols []prepPick `json:"symbols"`
	Tests   []string   `json:"tests"`
	Related []struct {
		Key   string   `json:"key"`
		Title string   `json:"title"`
		State string   `json:"state"`
		PRs   []int    `json:"prs"`
		Files []string `json:"files"`
		Why   string   `json:"why"`
	} `json:"related"`
	Terms []struct {
		Term       string `json:"term"`
		Definition string `json:"definition"`
		Why        string `json:"why"`
	} `json:"terms"`
	Questions []string `json:"questions"`
}

const (
	prepBundleFiles  = 15
	prepHistoryFiles = 8
	prepHistory      = 30
	prepBundleSyms   = 60
	prepBodies       = 8
)

func cmdPrep(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("prep which task? Give one or more KEYs (runners prepare the agent queue by themselves)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, k := range pos {
		key := strings.ToUpper(k)
		if !keyRE.MatchString(key) {
			return c.usage("%q isn't a task key", k)
		}
		n, err := c.prepTask(context.Background(), cwd, key)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		c.printf("%s: sent %d file%s from this checkout; the prep model reads them in the background (wp prep show %s)\n", key, n, pluralS(n), key)
	}
	return nil
}

func cmdPrepShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	keys := listArgs(pos)
	if len(keys) == 0 {
		return c.usage("show which task's prep? Give its KEY")
	}
	return c.each(keys, true, func(k string) error {
		key := normalKey(k)
		if !keyRE.MatchString(key) {
			return c.usage("%q isn't a task KEY, like WP-212", k)
		}
		return c.prepShow(key)
	})
}

func (c *cli) prepShow(key string) error {
	var p prepView
	if err := c.do("GET", "/api/v1/issues/"+esc(key)+"/prep?evidence=1", nil, &p); err != nil {
		return err
	}
	if c.asJSON {
		return c.codeJSON(p)
	}
	at := p.UpdatedAt
	c.printf("%s prep: %s, %s, from %s at %s", key, p.State, ago(&at), p.Machine, short12(p.Commit))
	if p.Model != "" {
		c.printf(", by %s", p.Model)
	}
	c.println()
	if p.HeldOut {
		c.println("Held out: agents aren't shown its notes (the comparison group on Agents → Usage).")
	}
	if p.Stale {
		c.println("The task changed since; a runner will prepare it again.")
	}
	if p.Error != "" {
		c.println("Failed: " + p.Error)
	}
	if p.Dropped > 0 {
		c.printf("The model named %d thing%s that weren't among the candidates; they were left out.\n", p.Dropped, pluralS(p.Dropped))
	}
	var notes string
	if p.Notes != nil {
		notes = renderPrepNotes(&p, nil, 4000, false)
		c.print(notes)
	}
	project := projectOfKey(key)
	evidence, listed := renderPrepEvidence(p.Evidence, project)
	c.print(evidence)
	c.noteShown("prep", append(printedTerms(&p, notes, project, false), listed...))
	return nil
}

// prepTask builds the bundle for key from the checkout at dir and sends it;
// it returns how many files it sent.
func (c *cli) prepTask(ctx context.Context, dir, key string) (int, error) {
	var iss issueForBrief
	if err := c.do("GET", "/api/v1/issues/"+esc(key), nil, &iss); err != nil {
		return 0, err
	}
	var comments []commentForBrief
	_ = c.do("GET", "/api/v1/issues/"+esc(key)+"/comments", nil, &comments)
	var extra string
	for _, ch := range iss.Children {
		extra += ch.Title + "\n"
	}
	for _, cm := range comments {
		extra += cm.Body + "\n"
	}
	ix, err := codeindex.Open(dir, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ix.Close() }()
	if _, err := ix.Update(ctx); err != nil {
		return 0, err
	}
	b, err := buildPrepBundle(ctx, ix, iss.Title, iss.Description, extra, key)
	if err != nil {
		return 0, err
	}
	b.Repo, b.Machine = originRepo(dir), machineName()
	if len(b.Files) == 0 {
		return 0, fmt.Errorf("nothing in this checkout matches the task")
	}
	return len(b.Files), c.do("POST", "/api/v1/issues/"+esc(key)+"/prep", b, nil)
}

// buildPrepBundle is the code half of a full brief with every file's outline
// and the source of the symbols the task names, all from the index.
func buildPrepBundle(ctx context.Context, ix *codeindex.Index, title, body, extra, key string) (*prepBundle, error) {
	cb, err := ix.Brief(ctx, body, codeindex.BriefOptions{Title: title, Extra: extra, Key: key, MaxFiles: prepBundleFiles})
	if err != nil {
		return nil, err
	}
	b := &prepBundle{Commit: ix.HeadCommit(), Tests: cb.Tests, Words: cb.Words, Index: &prepSourceN{Matched: cb.Matched}}
	for _, d := range cb.Docs {
		b.Docs = append(b.Docs, d.Path+": "+d.Heading)
	}
	for _, f := range cb.Files {
		pf := prepFile{Path: f.Path, Lines: f.Lines, Why: f.Why}
		if o, err := ix.Outline(f.Path); err == nil {
			for _, y := range o.Symbols {
				if len(pf.Symbols) == prepBundleSyms {
					break
				}
				pf.Symbols = append(pf.Symbols, prepSymbol{Name: y.Qualified(), Kind: y.Kind, Start: y.Start, End: y.End})
			}
		}
		b.Files = append(b.Files, pf)
		for _, y := range f.Named {
			if len(b.Bodies) == prepBodies {
				break
			}
			res, err := ix.Show(ctx, f.Path+":"+y.Qualified(), codeindex.ShowOptions{Budget: 1200})
			if err != nil || len(res.Blocks) == 0 {
				continue
			}
			blk := res.Blocks[0]
			b.Bodies = append(b.Bodies, prepBody{Path: f.Path, Name: y.Qualified(), Start: blk.Start, Text: strings.Join(blk.Lines, "\n")})
		}
	}
	var top []string
	for _, f := range b.Files[:min(len(b.Files), prepHistoryFiles)] {
		top = append(top, f.Path)
	}
	tc, err := ix.TaskCommits(ctx, top, prepHistory)
	if err != nil {
		return nil, err
	}
	for _, h := range tc {
		if !slices.Contains(h.Keys, key) {
			b.History = append(b.History, prepCommit{Subject: h.Subject, Keys: h.Keys, Files: h.Files, At: h.TS})
		}
	}
	return b, nil
}

// originRepo is owner/name of the checkout's origin, or "".
func originRepo(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output() // #nosec G204 -- fixed arguments; dir is a checkout
	if err != nil {
		return ""
	}
	r, _ := repoFromRemote(strings.TrimSpace(string(out)))
	return r
}

type prepDue struct {
	Key     string `json:"key"`
	Project string `json:"project"`
	Dir     string `json:"dir"`
}

// prepTasks prepares the tasks Waypoint says are due on this machine, a
// couple of minutes after the runner starts and every 15 minutes after.
// A failure is said once, not every round.
func (p *runnerProc) prepTasks(ctx context.Context) {
	lastErr := map[string]string{}
	for wait := 2 * time.Minute; sleepCtx(ctx, wait); wait = prepEvery {
		var due []prepDue
		if err := p.c.callInto(ctx, "GET", "/api/v1/prep/due?machine="+url.QueryEscape(p.opts.Name), nil, &due); err != nil {
			if msg := err.Error(); msg != lastErr[""] && !isNotFound(err) {
				p.logf("Task prep: %v", err)
				lastErr[""] = msg
			}
			continue
		}
		for _, d := range due {
			dir, err := resolveDir(d.Dir, p.allowed)
			if err == nil {
				_, err = p.c.prepTask(ctx, dir, d.Key)
			}
			if err != nil {
				if msg := err.Error(); msg != lastErr[d.Key] {
					p.logf("Task prep %s: %v", d.Key, err)
					lastErr[d.Key] = msg
				}
				continue
			}
			delete(lastErr, d.Key)
			p.logf("Prepared %s from %s for the prep model", d.Key, dir)
		}
	}
}

// prepSection is the prep printed in a brief: for the agent taking the task
// (shown records that it was shown), with each picked symbol resolved in ix.
// The evidence follows the notes. Both are empty when there is no ready prep,
// or neither its pointers nor evidence are for this reader.
func (c *cli) prepSection(ix *codeindex.Index, key string, budget int, compact, shown bool) (notes, evidence string) {
	path := "/api/v1/issues/" + esc(key) + "/prep?evidence=1"
	if shown {
		path += "&shown=1"
	}
	var p prepView
	if c.do("GET", path, nil, &p) != nil || p.State != "ready" || p.Notes == nil && p.Evidence == nil {
		return "", ""
	}
	return c.renderPrepShown(key, &p, prepIndex(ix, p.Repo), budget, compact)
}

// renderPrepShown prints a prep for an agent, its notes in about budget
// tokens and then its evidence: the glossary terms, pages and decisions it
// printed (not the ones the compact form or the budget left out) are told to
// the server as shown, whether or not this is the task it took (WP-ADR145).
func (c *cli) renderPrepShown(key string, p *prepView, ix *codeindex.Index, budget int, compact bool) (out, evidence string) {
	project := projectOfKey(key)
	if p.Notes != nil {
		out = renderPrepNotes(p, ix, budget, compact)
	}
	items := printedTerms(p, out, project, compact)
	evidence, listed := renderPrepEvidence(p.Evidence, project)
	if evidence != "" && out != "" && !strings.HasSuffix(out, "\n") {
		evidence = "\n" + evidence
	}
	c.noteShown("prep", append(items, listed...))
	return out, evidence
}

func projectOfKey(key string) string { return key[:max(strings.LastIndex(key, "-"), 0)] }

// printedTerms are the glossary terms in out, a prep's notes as printed: not
// the ones the compact form or the budget left out.
func printedTerms(p *prepView, out, project string, compact bool) []shownItem {
	if p.Notes == nil {
		return nil
	}
	var items []shownItem
	for i, t := range p.Notes.Terms {
		if i == prepTermLimit(compact) {
			break
		}
		if strings.Contains(out, "\n    "+t.Term+": ") {
			items = append(items, shownItem{Type: "glossary", Ref: t.Term, Project: project})
		}
	}
	return items
}

// renderPrepEvidence prints a prep's recorded data and claims to test, all of
// them in every form (the server caps how many it picks, and the experiment's
// unit counts them all), and returns the pages and decisions it named. Empty
// when there are none.
func renderPrepEvidence(e *prepEvidence, project string) (string, []shownItem) {
	if e == nil || len(e.Evidence) == 0 && len(e.Claims) == 0 {
		return "", nil
	}
	var b strings.Builder
	var items []shownItem
	edited := func(changed bool) string {
		if changed {
			return ", edited since it was matched"
		}
		return ""
	}
	// One command per kind that reads something printed here.
	read := map[string]string{}
	canRead := func(kind, ref string) {
		if read[kind] == "" {
			read[kind] = "wp " + kind + " show " + ref
		}
	}
	if len(e.Evidence) > 0 {
		b.WriteString("Recorded evidence that may bear on this task (matched by a weaker model):\n")
		for _, it := range e.Evidence {
			category := it.Category
			if it.Kind == "design" {
				category += " assumptions"
			}
			fmt.Fprintf(&b, "  %s %s [%s, %s, %s%s]", it.Ref, it.Title, category, it.Date, it.By, edited(it.ChangedSince))
			if it.BearsOn != "" {
				b.WriteString(" — bears on: " + it.BearsOn)
			}
			b.WriteByte('\n')
			items = append(items, shownItem{Type: it.Kind, Ref: it.Ref, Project: project})
			canRead(it.Kind, it.Ref)
		}
	}
	if len(e.Claims) > 0 {
		b.WriteString("Claims in the task text worth testing (picked by a weaker model):\n")
		for _, cl := range e.Claims {
			fmt.Fprintf(&b, "  - \"%s\" — test: %s", cl.Quote, cl.Test)
			if cl.Ref != "" {
				b.WriteString(" (see " + cl.Ref + edited(cl.ChangedSince) + ")")
				canRead(cl.RefKind, cl.Ref)
				if !slices.ContainsFunc(items, func(it shownItem) bool { return it.Ref == cl.Ref }) {
					items = append(items, shownItem{Type: cl.RefKind, Ref: cl.Ref, Project: project})
				}
			}
			b.WriteByte('\n')
		}
	}
	var commands []string
	for _, kind := range []string{"page", "design"} {
		if read[kind] != "" {
			commands = append(commands, read[kind])
		}
	}
	if len(commands) > 0 {
		b.WriteString("  Read one: " + strings.Join(commands, " · ") + "\n")
	}
	b.WriteString("Judge these before relying on them: could more or better data be collected, or better controls used? Check how and when each figure was measured. A claim listed here is not known to be wrong.\n")
	return b.String(), items
}

// prepTermLimit is how many of a prep's terms renderPrepNotes prints.
func prepTermLimit(compact bool) int {
	if compact {
		return 3
	}
	return 5
}

// prepIndex is ix when this directory is a checkout of the repo the prep
// was made from; in another repo every pick would look gone.
func prepIndex(ix *codeindex.Index, repo string) *codeindex.Index {
	if ix == nil || repo == "" {
		return ix
	}
	if cwd, err := os.Getwd(); err == nil {
		if r := originRepo(cwd); r != "" && !strings.EqualFold(r, repo) {
			return nil
		}
	}
	return ix
}

// showPrep is the short prep wp show prints after the comments: most
// agents open a task with wp show, not wp next. Empty when there is no ready
// prep with notes or evidence for this reader (a held-out one has no notes for
// agents). The index isn't built here for it: without one the picks print
// without line numbers.
func (c *cli) showPrep(key string) string {
	if c.asJSON || os.Getenv("WP_NO_BRIEF") != "" {
		return ""
	}
	var p prepView
	if c.do("GET", "/api/v1/issues/"+esc(key)+"/prep?evidence=1&shown=1", nil, &p) != nil || p.State != "ready" {
		return ""
	}
	var out string
	if p.Notes != nil {
		out = c.showPrepNotes(key, &p)
	}
	project := projectOfKey(key)
	evidence, listed := renderPrepEvidence(p.Evidence, project)
	c.noteShown("prep", append(printedTerms(&p, out, project, true), listed...))
	return out + evidence
}

// showPrepNotes is the notes' part of showPrep.
func (c *cli) showPrepNotes(key string, p *prepView) string {
	var ix *codeindex.Index
	if cwd, err := os.Getwd(); err == nil {
		if x, err := codeindex.Open(cwd, ""); err == nil {
			defer func() { _ = x.Close() }()
			if x.Status().Files > 0 {
				if _, err := x.Update(context.Background()); err == nil {
					ix = x
				}
			}
		}
	}
	return renderPrepNotes(p, prepIndex(ix, p.Repo), 600, true) + "  (wp brief " + key + " adds the code itself)\n"
}

// prepModelName is a prep's model without the gateway that ran it
// ("LLM gateway · ollama-cloud/beam" is ollama-cloud/beam).
func prepModelName(m string) string {
	if _, after, ok := strings.Cut(m, " · "); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(m)
}

// renderPrepNotes prints a prep's notes in about budget tokens: where to
// look first, then earlier tasks, terms and what the task leaves open. With
// an index, each symbol is printed where it is now (and, in the full form,
// the first ones' lines); a pick the index no longer has is left out.
func renderPrepNotes(p *prepView, ix *codeindex.Index, budget int, compact bool) string {
	n := p.Notes
	var b strings.Builder
	if m := prepModelName(p.Model); m != "" {
		fmt.Fprintf(&b, "A weaker model, `%s`, suggested these places to start.\n", m)
	} else {
		b.WriteString("A weaker model suggested these places to start.\n")
	}
	type resolved struct {
		pick prepPick
		res  *codeindex.ShowResult
	}
	var syms []resolved
	for _, s := range n.Symbols {
		if ix == nil {
			syms = append(syms, resolved{pick: s})
			continue
		}
		res, err := ix.Show(context.Background(), s.Path+":"+s.Name, codeindex.ShowOptions{Budget: 900})
		if err != nil || len(res.Blocks) == 0 {
			continue
		}
		syms = append(syms, resolved{s, res})
	}
	limit := func(full, short int) int {
		if compact {
			return short
		}
		return full
	}
	if len(syms) > 0 {
		for i, s := range syms {
			if i == limit(8, 6) {
				break
			}
			where := s.pick.Path + ":" + s.pick.Name
			if s.res != nil {
				blk := s.res.Blocks[0]
				where = fmt.Sprintf("%s:%d-%d %s", blk.Path, blk.Start, blk.End+blk.Omitted, s.pick.Name)
			}
			fmt.Fprintf(&b, "  %s — %s\n", where, s.pick.Why)
		}
	}
	for i, f := range n.Files {
		if i == limit(8, 5) {
			break
		}
		fmt.Fprintf(&b, "  %s — %s\n", f.Path, f.Why)
	}
	if len(n.Tests) > 0 {
		b.WriteString("  Tests: " + strings.Join(n.Tests, ", ") + "\n")
	}
	if len(n.Related) > 0 {
		b.WriteString("  Earlier tasks:\n")
		for i, r := range n.Related {
			if i == limit(5, 3) {
				break
			}
			facts := []string{r.State}
			for _, pr := range r.PRs {
				facts = append(facts, fmt.Sprintf("PR #%d", pr))
			}
			if len(r.Files) > 0 {
				facts = append(facts, "changed "+strings.Join(r.Files[:min(len(r.Files), 3)], ", "))
			}
			fmt.Fprintf(&b, "    %s %s [%s] — %s\n", r.Key, r.Title, strings.Join(facts, "; "), r.Why)
		}
	}
	if len(n.Terms) > 0 {
		b.WriteString("  Terms (wp define TERM):\n")
		for i, t := range n.Terms {
			if i == prepTermLimit(compact) {
				break
			}
			fmt.Fprintf(&b, "    %s: %s — %s\n", t.Term, t.Definition, t.Why)
		}
	}
	if len(n.Questions) > 0 {
		b.WriteString("  Open in the task text:\n")
		for i, q := range n.Questions {
			if i == limit(4, 3) {
				break
			}
			fmt.Fprintf(&b, "    - %s\n", q)
		}
	}
	out := clipTokens(b.String(), budget)
	if compact || ix == nil {
		return out
	}
	left := budget - codeindex.Tokens(out)
	for _, s := range syms {
		if s.res == nil {
			continue
		}
		r := s.res.Render(min(left, 900))
		if left -= codeindex.Tokens(r); left < 0 {
			out += "  … more: wp code show " + s.pick.Path + ":" + s.pick.Name + "\n"
			break
		}
		out += "\n" + r
	}
	return out
}
