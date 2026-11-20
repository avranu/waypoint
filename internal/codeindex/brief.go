package codeindex

import (
	"context"
	"fmt"
	"math"
	"path"
	"slices"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The code half of `wp brief` (ADR3 point 3): from the text of a task or a
// question, the files, symbols, warnings, tests and doc sections an agent
// would otherwise grep and sed its way to. Everything here is the index's
// own data; the task and the records that govern it are the client's.

const (
	DefaultBriefBudget  = 3000 // tokens for the code part of a full brief
	CompactBriefBudget  = 900  // tokens for the code part of the brief wp next prints
	briefCandidateLimit = 40
)

// BriefOptions tunes a Brief. The zero value is a full brief.
type BriefOptions struct {
	Title    string // the task's title: its words come first
	Extra    string // more text whose names count, but not its words (comments)
	Key      string // the task's key: commits that name it are its own work, not precedent
	MaxFiles int    // ranked files kept (default 8, compact 5)
	Compact  bool
}

// BriefFile is one ranked file.
type BriefFile struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang"`
	Lines   int      `json:"lines"`
	Score   float64  `json:"score"`
	Why     []string `json:"why"` // named in the task, symbol X, text match, imports X, changes with X
	Signals *Signals `json:"signals,omitempty"`
	Partner []string `json:"changes_with,omitempty"` // co-change partners not in the list
	Tests   []string `json:"tests,omitempty"`
	Named   []Symbol `json:"named,omitempty"` // symbols the text names, in this file
}

// BriefDoc is a documentation heading that matches the task.
type BriefDoc struct {
	Path    string `json:"path"`
	Heading string `json:"heading"`
	Line    int    `json:"line"`
	End     int    `json:"end"`
	Score   int    `json:"score"`
}

// Brief is the code half of a brief.
type Brief struct {
	Files   []BriefFile `json:"files"`
	Matched int         `json:"matched"` // candidate files before the cut
	Bodies  []Block     `json:"bodies,omitempty"`
	Docs    []BriefDoc  `json:"docs,omitempty"`
	Tests   []string    `json:"tests,omitempty"` // test files for the listed files
	Names   []string    `json:"names,omitempty"` // identifiers and paths the text named that the index knows
	Words   []string    `json:"words,omitempty"` // the words searched for
	Compact bool        `json:"compact,omitempty"`
}

// Refs are the names a text carries.
type Refs struct {
	Paths  []string        // file paths and file names
	Idents []string        // identifiers, backticked ones first
	Quoted map[string]bool // identifiers that were in backticks or called with ()
	Words  []string        // plain words worth a full-text search
}

var (
	pathRefRE = lazyre.New("(?:^|[\\s`(\"'\\[])((?:[\\w.@-]+/)+[\\w.@-]*\\w|[\\w-]+\\.(?:go|py|pyi|tsx?|jsx?|mjs|md|sql|sh|ya?ml|json|toml|html|css|rs|java|kt))(?:$|[\\s`)\"',.:;\\]])")
	tickRE    = lazyre.New("`([^`\\n]{1,80})`")
	camelRE   = lazyre.New(`\b(?:[A-Z][a-z0-9]+(?:[A-Z][A-Za-z0-9]*)+|[a-z][a-z0-9]*[A-Z][A-Za-z0-9]*|[a-z0-9]+(?:_[a-z0-9]+)+|[A-Z][A-Za-z0-9]*\.[A-Za-z_]\w*)\b`)
	callRE    = lazyre.New(`\b([A-Za-z_]\w*)\(\)`)
	fileExtRE = lazyre.New(`\.(go|py|pyi|tsx?|jsx?|mjs|md|sql|sh|ya?ml|json|toml|html|css|rs|java|kt|txt|lock)$`)

	genericWord = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields(`add added adding make makes made fix fixes fixed new use used using should must need needs want wants like when then than also each every into onto over under from with without which while where what whose does done doing have has had been being will would could task tasks issue issues thing things code file files change changes changed update updates call calls print prints work works agent agents command commands text some more most only just about after before other another same this that these those there their they them output outputs result results include includes included including instead real part step slice per via one two three can may might not any all none http https assess adjust improve potentially possibly consider better ensure handle allow support currently rather really maybe already still even much many well good wrong right look find found keep show today because since though through between around something everything nothing anything`) {
		genericWord[w] = true
	}
}

// ExtractRefs pulls the file paths, identifiers and search words out of a
// task's text.
func ExtractRefs(title, text string) Refs {
	r := Refs{Quoted: map[string]bool{}}
	seenP, seenI := map[string]bool{}, map[string]bool{}
	addPath := func(p string) {
		p = strings.Trim(p, "./")
		if p == "" || seenP[p] || strings.Contains(p, "://") || strings.HasPrefix(p, "api/v1") {
			return
		}
		seenP[p] = true
		r.Paths = append(r.Paths, p)
	}
	addIdent := func(s string, quoted bool) {
		s = strings.TrimSuffix(strings.TrimSpace(s), "()")
		if len(s) < 3 || seenI[s] || !identRE.MatchString(s) {
			return
		}
		if quoted {
			r.Quoted[s] = true
		}
		seenI[s] = true
		r.Idents = append(r.Idents, s)
	}
	all := title + "\n" + text
	for _, m := range tickRE.FindAllStringSubmatch(all, -1) {
		in := strings.TrimSpace(m[1])
		switch {
		case strings.Contains(in, " "):
			// `wp brief --budget`: only a leading path could be a name
			if f := strings.Fields(in)[0]; strings.Contains(f, "/") || fileExtRE.MatchString(f) {
				addPath(f)
			}
		case strings.Contains(in, "/") || fileExtRE.MatchString(in):
			addPath(in)
		default:
			addIdent(in, true)
		}
	}
	for _, m := range pathRefRE.FindAllStringSubmatch(all, -1) {
		addPath(m[1])
	}
	for _, m := range callRE.FindAllStringSubmatch(all, -1) {
		addIdent(m[1], true)
	}
	for _, m := range camelRE.FindAllString(all, -1) {
		if !fileExtRE.MatchString(m) {
			addIdent(m, false)
		}
	}
	if len(r.Idents) > 24 {
		r.Idents = r.Idents[:24]
	}
	r.Words = briefWords(title, text)
	return r
}

// briefWords are the plain words worth searching for: the title's first,
// then the description's, unique, without filler.
func briefWords(title, text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string, limit int) {
		n := 0
		for _, w := range wordRE.FindAllString(s, -1) {
			lw := strings.ToLower(w)
			if seen[lw] || !worthWord(lw) {
				continue
			}
			seen[lw] = true
			out = append(out, lw)
			if n++; n >= limit {
				break
			}
		}
	}
	add(title, 8)
	add(text, 10)
	return out
}

// Brief ranks the files a text points at and gathers what an agent needs
// to start: direct mentions > symbols named > full-text matches > import
// neighbours > co-change partners.
func (ix *Index) Brief(ctx context.Context, text string, o BriefOptions) (*Brief, error) {
	if o.MaxFiles <= 0 {
		o.MaxFiles = 8
		if o.Compact {
			o.MaxFiles = 5
		}
	}
	refs := ExtractRefs(o.Title, text+"\n"+o.Extra)
	cands := map[string]*BriefFile{}
	add := func(p string, pts float64, why string) *BriefFile {
		f := cands[p]
		if f == nil {
			var lang string
			var lines int
			if ix.db.QueryRow(`SELECT lang, lines FROM files WHERE path = ?`, p).Scan(&lang, &lines) != nil {
				return nil // a directory, or a file the index doesn't hold
			}
			f = &BriefFile{Path: p, Lang: lang, Lines: lines}
			cands[p] = f
		}
		f.Score += pts
		for _, w := range f.Why {
			if w == why {
				return f
			}
		}
		f.Why = append(f.Why, why)
		return f
	}
	br := &Brief{Compact: o.Compact}

	// 1. paths the text names
	for _, p := range refs.Paths {
		rel, amb := ix.resolveFile(p)
		switch {
		case rel != "":
			if add(rel, 100, "named in the task") != nil {
				br.Names = append(br.Names, p)
			}
		case len(amb) > 0 && len(amb) <= 3:
			for _, a := range amb {
				add(a, 40, "named in the task ("+p+")")
			}
		}
	}

	// 2. symbols the text names
	for _, id := range refs.Idents {
		hits, err := ix.LookupSymbols(ctx, id, 6)
		if err != nil {
			return nil, err
		}
		floor := 85.0
		if refs.Quoted[id] {
			floor = 60
		}
		taken := 0
		for _, h := range hits {
			if h.Score < floor || taken >= 3 || taken > 0 && isTestPath(h.Path) {
				continue
			}
			pts := 45.0
			switch {
			case h.Score < 85:
				pts = 22 // only a partial name match, but it was in backticks
			case taken > 0:
				pts = 20 // the same name elsewhere
			}
			f := add(h.Path, pts, "symbol "+h.Symbol.Qualified())
			if f == nil {
				continue
			}
			if taken == 0 && h.Score >= 85 && bodyKind(h.Symbol.Kind) {
				f.Named = append(f.Named, h.Symbol)
			}
			taken++
		}
		if taken > 0 {
			br.Names = append(br.Names, id)
		}
	}

	// 3. full text: the title's words alone, then all the words
	runFTS := func(words []string, weight float64, why string) error {
		if len(words) == 0 {
			return nil
		}
		fts, err := ix.ftsFiles(ctx, words, strings.Join(words, " "), briefCandidateLimit)
		if err != nil {
			return err
		}
		var top float64
		for _, c := range fts {
			top = max(top, c.score)
		}
		for rank, c := range fts {
			if pts := weight * c.score / top; top > 0 && (pts >= weight*0.25 || rank < 3) {
				add(c.path, pts, why)
			}
		}
		return nil
	}
	titleWords, words := ix.weighWords(ctx, o.Title, text)
	br.Words = words
	if err := runFTS(titleWords, 18, "text match"); err != nil {
		return nil, err
	}
	if err := runFTS(words, 18, "text match"); err != nil {
		return nil, err
	}

	// 3b. precedent: files of earlier commits whose subjects say what the
	// task says
	for p, pts := range ix.precedent(ctx, o.Title, text, o.Key) {
		add(p, pts, "past commits on this")
	}

	// tests and documentation count for less than the code they cover,
	// unless the text named them
	for _, f := range cands {
		switch {
		case slices.ContainsFunc(f.Why, func(w string) bool { return whyRank(w) == 0 }):
		case isTestPath(f.Path):
			f.Score *= 0.55
		case f.Lang == "markdown":
			f.Score *= 0.6
		case path.Ext(f.Path) == ".json" || path.Ext(f.Path) == ".sql" || f.Lang == "text":
			f.Score *= 0.7
		}
	}
	ranked := func() []*BriefFile {
		out := make([]*BriefFile, 0, len(cands))
		for _, f := range cands {
			out = append(out, f)
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Score != out[j].Score {
				return out[i].Score > out[j].Score
			}
			return out[i].Path < out[j].Path
		})
		return out
	}

	// 4. import neighbours and 5. co-change partners of the best few
	seeds := 0
	for _, f := range ranked() {
		if seeds >= 3 {
			break
		}
		if f.Score < 20 || isTestPath(f.Path) || f.Lang == "markdown" {
			continue
		}
		seeds++
		base := path.Base(f.Path)
		// a Go package is imported by half the repository: only a few importers say anything
		if imp, err := ix.Importers(f.Path); err == nil && (!strings.HasSuffix(f.Path, ".go") || len(imp) <= 3) {
			n := 0
			for _, p := range imp {
				if !isTestPath(p) && n < 6 {
					n++
					add(p, 7, "imports "+base)
				}
			}
		}
		if in, _, err := ix.Imports(f.Path); err == nil && !strings.HasSuffix(f.Path, ".go") { // Go imports name packages, not files
			for _, p := range firstN(in, 4) {
				add(p, 6, "imported by "+base)
			}
		}
		if co, err := ix.CoChanged(f.Path, 5); err == nil {
			for _, c := range co {
				if c.Support >= 3 {
					add(c.Path, 3+8*c.Confidence, "changes with "+base)
				}
			}
		}
	}
	order := ranked()
	br.Matched = len(order)
	order = firstN2(order, o.MaxFiles)
	listed := map[string]bool{}
	for _, f := range order {
		listed[f.Path] = true
	}
	for _, f := range order {
		sort.SliceStable(f.Why, func(i, j int) bool { return whyRank(f.Why[i]) < whyRank(f.Why[j]) })
		f.Why = firstN(f.Why, 3)
		f.Signals, _ = ix.Signals(f.Path)
		if co, err := ix.CoChanged(f.Path, 6); err == nil {
			for _, c := range co {
				if c.Support >= 3 && !listed[c.Path] && len(f.Partner) < 3 {
					f.Partner = append(f.Partner, fmt.Sprintf("%s (%d)", c.Path, c.Support))
				}
			}
		}
		if !isTestPath(f.Path) {
			imps, _ := ix.Importers(f.Path)
			f.Tests = firstN(namedTests(f.Path, ix.testsFor(f.Path, imps)), 3)
		}
		br.Files = append(br.Files, *f)
	}
	seen := map[string]bool{}
	for _, f := range br.Files {
		for _, t := range f.Tests {
			if !seen[t] && !listed[t] {
				seen[t] = true
				br.Tests = append(br.Tests, t)
			}
		}
	}
	br.Docs = ix.briefDocs(ctx, words, o.Title)
	if o.Compact {
		br.Docs = firstN2(br.Docs, 2)
	}
	return br, nil
}

func firstN2[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func whyRank(w string) int {
	switch {
	case strings.HasPrefix(w, "named"):
		return 0
	case strings.HasPrefix(w, "symbol"):
		return 1
	case w == "text match":
		return 2
	case strings.HasPrefix(w, "past"):
		return 2
	case strings.HasPrefix(w, "import"):
		return 3
	}
	return 4
}

func bodyKind(k string) bool {
	switch k {
	case "func", "method", "type", "struct", "interface", "class", "function", "ctor", "trait", "enum", "impl", "table":
		return true
	}
	return false
}

// briefDocs finds documentation headings that match the words.
func (ix *Index) briefDocs(ctx context.Context, words []string, title string) []BriefDoc {
	lt := strings.ToLower(title)
	type key struct {
		path string
		line int
	}
	hits := map[key]*BriefDoc{}
	for _, w := range words {
		wt := 1
		if strings.Contains(lt, w) {
			wt = 2
		}
		rows, err := ix.db.QueryContext(ctx, `SELECT f.path, s.name, s.decl, s.end FROM symbols s JOIN files f ON f.id = s.file_id
			WHERE f.lang = 'markdown' AND s.kind = 'heading' AND s.name LIKE '%' || ? || '%' ESCAPE '\' LIMIT 200`, likeEscape(w))
		if err != nil {
			continue
		}
		for rows.Next() {
			var d BriefDoc
			if rows.Scan(&d.Path, &d.Heading, &d.Line, &d.End) != nil {
				continue
			}
			k := key{d.Path, d.Line}
			if hits[k] == nil {
				hits[k] = &d
			}
			hits[k].Score += wt
		}
		_ = rows.Close()
	}
	var out []BriefDoc
	for _, d := range hits {
		if d.Score >= 3 {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return firstN2(out, 4)
}

// RenderBrief is the brief as text within budget tokens: the ranked files,
// warnings, tests and docs first, then the exact bodies of the symbols the
// text names, then outlines of the top files with what is left. What was
// cut says how to get it. It fills b.Bodies as it goes.
func (ix *Index) RenderBrief(b *Brief, budget int) string {
	if budget <= 0 {
		budget = DefaultBriefBudget
		if b.Compact {
			budget = CompactBriefBudget
		}
	}
	if len(b.Files) == 0 {
		return ""
	}
	var out strings.Builder
	used := 0
	put := func(s string) bool {
		if used+Tokens(s) > budget {
			return false
		}
		used += Tokens(s)
		out.WriteString(s)
		return true
	}
	if b.Compact {
		put("Code, best first:\n") // its last line says how to read them
	} else {
		put("Code: files this points at, best first (wp code show FILE[:SYMBOL] reads them)\n")
	}
	for i, f := range b.Files {
		put(fileLine(i+1, f))
	}
	if b.Matched > len(b.Files) {
		put(fmt.Sprintf("  … %d more matched (wp code find \"%s\")\n", b.Matched-len(b.Files), strings.Join(firstN(b.Words, 4), " ")))
	}

	warn := warnings(b)
	if len(warn) > 0 {
		put("Watch out:\n")
		for _, w := range firstN(warn, map[bool]int{true: 3, false: 6}[b.Compact]) {
			if !put(w) {
				break
			}
		}
	}
	if tl := testLine(b); tl != "" {
		put(tl)
	}
	if len(b.Docs) > 0 {
		put("Docs:\n")
		for _, d := range b.Docs {
			if !put(fmt.Sprintf("  %s:%d  %s\n", d.Path, d.Line, collapse(d.Heading, 80))) {
				break
			}
		}
	}
	if b.Compact {
		if syms := namedLines(b); len(syms) > 0 {
			put("Named symbols:\n")
			for _, s := range syms {
				if !put(s) {
					break
				}
			}
		}
		put("More: wp brief (full), wp code show FILE[:SYMBOL], wp code find QUERY\n")
		return out.String()
	}

	// the bodies of symbols the text names: what it would sed -n for
	if left := budget - used; left > 200 {
		ix.fillBodies(b, left*55/100)
		if len(b.Bodies) > 0 {
			put("\nSymbols the text names:\n")
			for _, blk := range b.Bodies {
				var sb strings.Builder
				blk.render(&sb)
				if !put(sb.String()) {
					put(fmt.Sprintf("… %s:%d-%d does not fit (wp code show %s:%s)\n", blk.Path, blk.Start, blk.End, blk.Path, blk.Symbol))
				}
			}
		}
	}
	// outlines of the top files with the rest
	var skipped []string
	shown := 0
	for _, f := range b.Files {
		if shown >= 4 || isTestPath(f.Path) || f.Lines <= 40 {
			continue // a short file reads whole in one `wp code show`
		}
		left := budget - used
		if left < 220 {
			skipped = append(skipped, f.Path)
			continue
		}
		ol, err := ix.Outline(f.Path)
		if err != nil || len(ol.Symbols) == 0 {
			continue
		}
		if !put("\n" + ol.briefRender(min(left-20, 650))) {
			skipped = append(skipped, f.Path)
			continue
		}
		shown++
	}
	if len(skipped) > 0 {
		put(fmt.Sprintf("\nOutlines not shown (budget): %s (wp code show FILE, or --budget N)\n", strings.Join(firstN(skipped, 4), ", ")))
	}
	return out.String()
}

// fillBodies reads the bodies of the symbols the text names, sharing budget.
func (ix *Index) fillBodies(b *Brief, budget int) {
	type want struct {
		file string
		sym  Symbol
	}
	var ws []want
	for _, f := range b.Files {
		for _, s := range f.Named {
			ws = append(ws, want{f.Path, s})
		}
	}
	ws = firstN2(ws, 4)
	for _, w := range ws {
		lines, _, err := ix.readLines(w.file)
		if err != nil {
			continue
		}
		b.Bodies = append(b.Bodies, ix.block(w.file, w.sym.Qualified(), w.sym.Kind, w.sym.Sig, w.sym.Start, w.sym.End, lines, max(budget/len(ws), 120)))
	}
}

func warnings(b *Brief) []string {
	var out []string
	for _, f := range b.Files {
		var w []string
		if s := f.Signals; s != nil {
			if s.Hotspot {
				w = append(w, fmt.Sprintf("hotspot (%d commits, last %s)", s.Commits, ago(s.LastChange)))
			}
			if s.Fixes > 0 {
				w = append(w, fmt.Sprintf("%d past bug fix%s, last %s", s.Fixes, map[bool]string{true: "es", false: ""}[s.Fixes != 1], ago(s.LastFix)))
			}
		}
		if len(f.Partner) > 0 {
			w = append(w, "usually changes with "+strings.Join(f.Partner, ", "))
		}
		if len(w) > 0 {
			out = append(out, fmt.Sprintf("  %s: %s\n", f.Path, strings.Join(w, "; ")))
		}
	}
	return out
}

func fileLine(i int, f BriefFile) string {
	var flags []string
	if f.Signals != nil && f.Signals.Hotspot {
		flags = append(flags, "HOTSPOT")
	}
	if f.Signals != nil && f.Signals.Fixes > 0 {
		flags = append(flags, fmt.Sprintf("%d fixes", f.Signals.Fixes))
	}
	extra := ""
	if len(flags) > 0 {
		extra = "  " + strings.Join(flags, ", ")
	}
	return fmt.Sprintf("  %d. %s (%d lines)  %s%s\n", i, f.Path, f.Lines, strings.Join(f.Why, "; "), extra)
}

func testLine(b *Brief) string {
	var parts []string
	pkgs, seen := map[string]bool{}, map[string]bool{}
	for _, f := range b.Files {
		for _, t := range f.Tests {
			if seen[t] {
				continue
			}
			seen[t] = true
			if d := "./" + path.Dir(t) + "/"; strings.HasSuffix(t, "_test.go") && !pkgs[d] {
				pkgs[d] = true
				t += " (go test " + d + ")"
			}
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	limit := map[bool]int{true: 3, false: 6}[b.Compact]
	more := ""
	if len(parts) > limit {
		more = fmt.Sprintf(", +%d", len(parts)-limit)
		parts = parts[:limit]
	}
	return "Tests: " + strings.Join(parts, ", ") + more + "\n"
}

func namedLines(b *Brief) []string {
	var out []string
	for _, f := range b.Files {
		for _, s := range f.Named {
			out = append(out, fmt.Sprintf("  %s:%d-%d  %s\n", f.Path, s.Start, s.End, collapse(s.Sig, 90)))
		}
	}
	return firstN(out, 5)
}

// briefRender is the outline without the history lines the warnings
// already carry.
func (o *Outline) briefRender(budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s, %d lines", o.Path, o.Lang, o.Lines)
	if len(o.ImportedBy) > 0 {
		fmt.Fprintf(&b, "; imported by %d: %s", len(o.ImportedBy), joinCap(o.ImportedBy, 3))
	}
	b.WriteString("\n")
	used := Tokens(b.String())
	var syms []Symbol
	for _, s := range o.Symbols {
		if s.Kind != "field" && s.Kind != "var" {
			syms = append(syms, s)
		}
	}
	shown := 0
	for _, s := range syms {
		line := symbolLine(s)
		if used+Tokens(line) > budget {
			break
		}
		used += Tokens(line)
		shown++
		b.WriteString(line)
	}
	if shown < len(syms) {
		fmt.Fprintf(&b, "  … %d more symbols (wp code show %s)\n", len(syms)-shown, o.Path)
	}
	return b.String()
}

// precedent scores the files of earlier commits whose subjects say what the
// task says (WP-241): commit subjects are prose like the task's, where its
// code is not. Each word counts by how few subjects use it, twice in the
// title; a subject needs two shared words, or one of the title's. The best
// few commits lend their files points by how well they matched: the last
// time someone did something like this, these changed. Commits that name
// key are the task's own work, not precedent.
func (ix *Index) precedent(ctx context.Context, title, text, key string) map[string]float64 {
	rows, err := ix.db.QueryContext(ctx, `SELECT id, subject FROM commits WHERE nfiles <= 25`) // the window is 3000 commits
	if err != nil {
		return nil
	}
	type subject struct {
		id    int64
		words map[string]bool
	}
	var subjects []subject
	df := map[string]int{}
	lk := strings.ToLower(key)
	for rows.Next() {
		var id int64
		var subj string
		if rows.Scan(&id, &subj) != nil || lk != "" && strings.Contains(strings.ToLower(subj), lk) {
			continue
		}
		ws := stems(subj)
		for w := range ws {
			df[w]++
		}
		subjects = append(subjects, subject{id, ws})
	}
	_ = rows.Close()
	n := float64(len(subjects))
	weight := map[string]float64{}
	inTitle := stems(title)
	for w, tf := range stemCounts(text) {
		weight[w] = math.Sqrt(float64(min(tf, 4)))
	}
	for w := range inTitle {
		weight[w] = 2 + weight[w]
	}
	for w := range weight {
		if df[w] == 0 || df[w] > 3 && float64(df[w]) > n/8 { // a word in an eighth of them says little
			delete(weight, w)
			continue
		}
		weight[w] *= math.Log(n / float64(df[w]))
	}
	type hit struct {
		id    int64
		score float64
	}
	var hits []hit
	for _, s := range subjects {
		var score float64
		shared, titled := 0, false
		for w := range s.words {
			if wt := weight[w]; wt > 0 {
				score += wt
				shared++
				titled = titled || inTitle[w]
			}
		}
		if shared >= 2 || titled {
			hits = append(hits, hit{s.id, score})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	best := hits[0].score
	out := map[string]float64{}
	for _, h := range firstN2(hits, 8) {
		if h.score < best/3 {
			break
		}
		frows, err := ix.db.QueryContext(ctx, `SELECT path FROM commit_files WHERE commit_id = ?`, h.id)
		if err != nil {
			continue
		}
		for frows.Next() {
			var p string
			if frows.Scan(&p) == nil && Language(p) != "markdown" && !isTestPath(p) {
				out[p] = min(out[p]+14*h.score/best, 30)
			}
		}
		_ = frows.Close()
	}
	return out
}

// stems are the stems of the words worth matching in s.
func stems(s string) map[string]bool {
	out := map[string]bool{}
	for w := range stemCounts(s) {
		out[w] = true
	}
	return out
}

// stemCounts counts the stems of the words worth matching in s.
func stemCounts(s string) map[string]int {
	out := map[string]int{}
	for _, w := range wordRE.FindAllString(s, -1) {
		if lw := strings.ToLower(w); worthWord(lw) {
			out[stemWord(lw)]++
		}
	}
	return out
}

// worthWord says whether a lower-case word could say where to look: not
// short, filler or a number.
func worthWord(lw string) bool {
	return len(lw) >= 4 && !stopwords[lw] && !genericWord[lw] && strings.Trim(lw, "0123456789") != ""
}

// weighWords picks the words worth a full-text search (WP-241): the ones
// the index holds in few files, said often or in the title, each cut to its
// stem so "babysitter" finds babysit.go. A word in no file, or in a fifth of
// them, says nothing about where to look. titleWords are the title's, in
// order; words are the best of all, title's first.
func (ix *Index) weighWords(ctx context.Context, title, text string) (titleWords, words []string) {
	var total float64
	if ix.db.QueryRowContext(ctx, `SELECT count(*) FROM files`).Scan(&total) != nil || total == 0 {
		return nil, nil
	}
	type term struct {
		stem   string
		tf     int
		title  bool
		weight float64
	}
	terms := map[string]*term{}
	var order []*term
	for i, s := range []string{title, text} {
		for _, w := range wordRE.FindAllString(s, -1) {
			lw := strings.ToLower(w)
			if !worthWord(lw) {
				continue
			}
			st := stemWord(lw)
			t := terms[st]
			if t == nil {
				if len(terms) >= 120 {
					continue
				}
				t = &term{stem: st}
				terms[st] = t
				order = append(order, t)
			}
			t.tf++
			t.title = t.title || i == 0
		}
	}
	var kept []*term
	for _, t := range order {
		var df float64
		if ix.db.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE docs MATCH ?`, ftsQuote(t.stem)+"*").Scan(&df) != nil || df == 0 || df > total/5 {
			continue
		}
		t.weight = (1 + math.Log(float64(t.tf))) * math.Log(total/df)
		var named float64
		if ix.db.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE docs MATCH ?`, "{path symbols} : "+ftsQuote(t.stem)+"*").Scan(&named) == nil && named > 0 {
			t.weight *= 4 // a word the code names things with, not only says in comments
		}
		if t.title {
			t.weight *= 2
			if len(titleWords) < 8 {
				titleWords = append(titleWords, t.stem)
			}
		}
		kept = append(kept, t)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].weight > kept[j].weight })
	for _, t := range firstN2(kept, 12) {
		words = append(words, t.stem)
	}
	return titleWords, words
}

// stemWord cuts a common English ending off a word, leaving at least four
// letters: babysitter -> babysit, merged -> merg, runner -> runn. The
// full-text search takes it as a prefix.
func stemWord(w string) string {
	for _, suf := range []string{"ations", "ation", "ings", "ing", "ers", "er", "ies", "es", "ed", "s"} {
		st, ok := strings.CutSuffix(w, suf)
		if !ok || len(st) < 4 || suf == "s" && strings.HasSuffix(st, "s") {
			continue
		}
		if n := len(st); n >= 5 && st[n-1] == st[n-2] && !strings.ContainsRune("aeiouls", rune(st[n-1])) {
			st = st[:n-1] // babysitt -> babysit
		}
		return st
	}
	return w
}

// namedTests keeps, for a Go file, the tests named after it (merge.go:
// merge_test.go, merge_retry_test.go): the package's other test files are
// what a run covers, not what a reader needs (WP-271).
func namedTests(p string, tests []string) []string {
	if !strings.HasSuffix(p, ".go") {
		return tests
	}
	stem := strings.TrimSuffix(path.Base(p), ".go")
	var out []string
	for _, t := range tests {
		if b := path.Base(t); strings.HasPrefix(b, stem+"_") || path.Dir(t) != path.Dir(p) {
			out = append(out, t)
		}
	}
	return out
}
