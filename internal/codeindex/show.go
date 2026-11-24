package codeindex

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Defaults for Show.
const (
	DefaultShowBudget    = 4000
	DefaultOutlineBudget = 2500
	smallFileLines       = 80
	smallFileBytes       = 6000
)

// ShowOptions tunes a Show.
type ShowOptions struct {
	Budget  int  // approximate tokens
	Context int  // extra lines around a symbol or range
	Outline bool // outline even a small file
	Full    bool // the whole file, within the budget
	Callers bool // also list where a symbol is used
}

// Outline is a file at a glance.
type Outline struct {
	Path       string     `json:"path"`
	Lang       string     `json:"lang"`
	Lines      int        `json:"lines"`
	Symbols    []Symbol   `json:"symbols"`
	Imports    []string   `json:"imports,omitempty"`  // files (Go: packages) in the checkout it imports
	External   []string   `json:"external,omitempty"` // other imports
	ImportedBy []string   `json:"imported_by,omitempty"`
	Signals    *Signals   `json:"signals,omitempty"`
	CoChange   []CoChange `json:"co_change,omitempty"`
}

// Block is real lines read from disk.
type Block struct {
	Path    string   `json:"path"`
	Symbol  string   `json:"symbol,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Sig     string   `json:"sig,omitempty"`
	Start   int      `json:"start"`
	End     int      `json:"end"` // the last line shown
	Total   int      `json:"total_lines"`
	Lines   []string `json:"lines"`
	Omitted int      `json:"omitted,omitempty"` // lines of the symbol cut for the budget
	Footer  string   `json:"footer,omitempty"`
}

// ShowResult is what Show found: an outline, or real lines.
type ShowResult struct {
	Kind       string      `json:"kind"` // outline, full, symbol, lines, candidates
	Outline    *Outline    `json:"outline,omitempty"`
	Blocks     []Block     `json:"blocks,omitempty"`
	Candidates []SymbolHit `json:"candidates,omitempty"` // other symbols with that name
	Callers    []FileHit   `json:"callers,omitempty"`
	Note       string      `json:"note,omitempty"`
}

var (
	lineRangeRE = lazyre.New(`^(\d+)(?:[-,:](\d+))?$`)
	grepStyleRE = lazyre.New(`^(.+?):(\d+)(?::.*)?$`)
)

// RelPath turns a path given relative to cwd (or absolute) into one
// relative to the checkout root, slash-separated. ok is false when it is
// outside the checkout.
func (ix *Index) RelPath(cwd, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(ix.Root, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolveFile finds the checkout file a path names: exactly, or by a
// unique path suffix (merge.go, core/merge.go).
func (ix *Index) resolveFile(p string) (rel string, ambiguous []string) {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	if p == "" {
		return "", nil
	}
	var id int64
	if ix.db.QueryRow(`SELECT id FROM files WHERE path = ?`, p).Scan(&id) == nil {
		return p, nil
	}
	if fi, err := os.Stat(filepath.Join(ix.Root, filepath.FromSlash(p))); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	if !strings.ContainsAny(p, "*?") {
		rows, err := ix.db.Query(`SELECT path FROM files WHERE path LIKE '%/' || ? ESCAPE '\' LIMIT 8`, likeEscape(p))
		if err == nil {
			defer rows.Close()
			var all []string
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil {
					all = append(all, s)
				}
			}
			if len(all) == 1 {
				return all[0], nil
			}
			return "", all
		}
	}
	return "", nil
}

// Show resolves ref and returns what an agent would otherwise cat or sed:
// FILE (an outline, or the whole file when small), FILE:SYMBOL (that
// symbol's current lines, read from disk), FILE:START-END, FILE:LINE (the
// symbol around that line), or SYMBOL alone (found in the index).
func (ix *Index) Show(ctx context.Context, ref string, o ShowOptions) (*ShowResult, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("nothing to show")
	}
	var file, spec string
	if rel, amb := ix.resolveFile(ref); rel != "" {
		file = rel
	} else if len(amb) == 0 {
		if i := strings.LastIndex(ref, ":"); i > 0 {
			left, right := ref[:i], ref[i+1:]
			if m := grepStyleRE.FindStringSubmatch(ref); m != nil && !lineRangeRE.MatchString(right) {
				left, right = m[1], m[2]
			}
			if rel, amb := ix.resolveFile(left); rel != "" {
				file, spec = rel, right
			} else if len(amb) > 0 {
				return nil, fmt.Errorf("%s matches several files: %s", left, strings.Join(amb, ", "))
			} else {
				file = ""
			}
		} else {
			file = ""
		}
	} else {
		return nil, fmt.Errorf("%s matches several files: %s", ref, strings.Join(amb, ", "))
	}
	if file == "" {
		return ix.showSymbol(ctx, ref, o)
	}
	switch {
	case spec == "":
		return ix.showFile(file, o)
	case lineRangeRE.MatchString(spec):
		m := lineRangeRE.FindStringSubmatch(spec)
		a, _ := strconv.Atoi(m[1])
		b := a
		if m[2] != "" {
			b, _ = strconv.Atoi(m[2])
		}
		return ix.showRange(file, a, b, m[2] == "", o)
	}
	return ix.showFileSymbol(ctx, file, spec, o)
}

func (ix *Index) readLines(file string) ([]string, []byte, error) {
	src, err := os.ReadFile(filepath.Join(ix.Root, filepath.FromSlash(file))) // #nosec G304 -- a path inside the checkout (RelPath/resolveFile)
	if err != nil {
		return nil, nil, err
	}
	return splitLines(src), src, nil
}

func (ix *Index) showFile(file string, o ShowOptions) (*ShowResult, error) {
	lines, src, err := ix.readLines(file)
	if err != nil {
		return nil, err
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultOutlineBudget
	}
	if o.Full || !o.Outline && len(lines) <= smallFileLines && len(src) <= smallFileBytes {
		if o.Full && o.Budget <= 0 {
			budget = DefaultShowBudget * 2
		}
		blk := ix.block(file, "", "", "", 1, len(lines), lines, budget)
		kind := "full"
		return &ShowResult{Kind: kind, Blocks: []Block{blk}}, nil
	}
	ol, err := ix.outline(file, lines, src)
	if err != nil {
		return nil, err
	}
	return &ShowResult{Kind: "outline", Outline: ol}, nil
}

// Outline returns a file's outline from the index (live scan when the file
// isn't indexed).
func (ix *Index) Outline(file string) (*Outline, error) {
	lines, src, err := ix.readLines(file)
	if err != nil {
		return nil, err
	}
	return ix.outline(file, lines, src)
}

func (ix *Index) outline(file string, lines []string, src []byte) (*Outline, error) {
	ol := &Outline{Path: file, Lang: Language(file), Lines: len(lines)}
	var id int64
	if ix.db.QueryRow(`SELECT id FROM files WHERE path = ?`, file).Scan(&id) == nil {
		syms, err := ix.fileSymbols(id)
		if err != nil {
			return nil, err
		}
		ol.Symbols = syms
		ol.Imports, ol.External, _ = ix.Imports(file)
		ol.ImportedBy, _ = ix.Importers(file)
		ol.Signals, _ = ix.Signals(file)
		ol.CoChange, _ = ix.CoChanged(file, 5)
	} else {
		ol.Symbols = Scan(ol.Lang, src).Symbols
	}
	return ol, nil
}

// block reads lines [start,end] into a Block, cutting at the budget.
func (ix *Index) block(file, sym, kind, sig string, start, end int, lines []string, budget int) Block {
	start = max(1, start)
	end = min(end, len(lines))
	b := Block{Path: file, Symbol: sym, Kind: kind, Sig: sig, Start: start, End: end, Total: len(lines)}
	used := 0
	for i := start; i <= end; i++ {
		cost := (len(lines[i-1]) + 8) / 4
		if used+cost > budget && i > start {
			b.Omitted = end - i + 1
			b.End = i - 1
			break
		}
		used += cost
		b.Lines = append(b.Lines, lines[i-1])
	}
	return b
}

func (ix *Index) showRange(file string, a, b int, single bool, o ShowOptions) (*ShowResult, error) {
	lines, src, err := ix.readLines(file)
	if err != nil {
		return nil, err
	}
	if a > len(lines) {
		return nil, fmt.Errorf("%s has %d lines", file, len(lines))
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultShowBudget
	}
	if single {
		syms := Scan(Language(file), src).Symbols
		if s, ok := Innermost(syms, a); ok && s.End-s.Start < 400 {
			blk := ix.block(file, s.Qualified(), s.Kind, s.Sig, s.Start, s.End, lines, budget)
			blk.Footer = fmt.Sprintf("line %d is in %s", a, s.Qualified())
			return &ShowResult{Kind: "symbol", Blocks: []Block{blk}}, nil
		}
		ctxN := max(o.Context, 12)
		a, b = a-ctxN, a+ctxN
	}
	if b < a {
		a, b = b, a
	}
	a, b = a-o.Context, b+o.Context
	blk := ix.block(file, "", "", "", a, b, lines, budget)
	if s, ok := Innermost(Scan(Language(file), src).Symbols, max(1, a)); ok {
		blk.Footer = fmt.Sprintf("in %s (%d-%d)", s.Qualified(), s.Start, s.End)
	}
	return &ShowResult{Kind: "lines", Blocks: []Block{blk}}, nil
}

// Innermost is the smallest symbol, other than a field, whose lines hold
// line: the function or type a line is in.
func Innermost(syms []Symbol, line int) (Symbol, bool) {
	best := -1
	for i, s := range syms {
		if s.Kind == "field" || s.Start > line || s.End < line {
			continue
		}
		if best < 0 || s.End-s.Start <= syms[best].End-syms[best].Start {
			best = i
		}
	}
	if best < 0 {
		return Symbol{}, false
	}
	return syms[best], true
}

// matchSymbols picks the symbols of a live scan that a reference names:
// Name or Parent.Name, case-sensitive first, then not.
func matchSymbols(syms []Symbol, spec string) []Symbol {
	parent, name := splitQualified(spec)
	var exact, folded []Symbol
	for _, s := range syms {
		if parent != "" && !strings.EqualFold(lastSegment(s.Parent), lastSegment(parent)) {
			continue
		}
		switch {
		case s.Name == name:
			exact = append(exact, s)
		case strings.EqualFold(s.Name, name) || normName(s.Name) == normName(name):
			folded = append(folded, s)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return folded
}

func (ix *Index) showFileSymbol(ctx context.Context, file, spec string, o ShowOptions) (*ShowResult, error) {
	lines, src, err := ix.readLines(file)
	if err != nil {
		return nil, err
	}
	lang := Language(file)
	parsed := Scan(lang, src)
	found := matchSymbols(parsed.Symbols, spec)
	if len(found) == 0 {
		var names []string
		_, name := splitQualified(spec)
		for _, s := range parsed.Symbols {
			if s.Kind != "field" && (strings.Contains(strings.ToLower(s.Name), strings.ToLower(name))) {
				names = append(names, s.Qualified())
			}
		}
		if len(names) > 8 {
			names = names[:8]
		}
		msg := fmt.Sprintf("no symbol %q in %s", spec, file)
		if len(names) > 0 {
			msg += "; similar: " + strings.Join(names, ", ")
		} else {
			msg += "; wp code show " + file + " lists its symbols"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultShowBudget
	}
	res := &ShowResult{Kind: "symbol"}
	per := budget
	if len(found) > 1 {
		per = max(budget/len(found), 400)
	}
	for _, s := range found {
		blk := ix.block(file, s.Qualified(), s.Kind, s.Sig, s.Start-o.Context, s.End+o.Context, lines, per)
		blk.Footer = ix.memberFooter(file, lang, s, parsed.Symbols)
		res.Blocks = append(res.Blocks, blk)
		if o.Callers {
			res.Callers = append(res.Callers, ix.callers(ctx, file, s, 3)...)
		}
	}
	return res, nil
}

// memberFooter lists a type's methods or a class's members, which sit
// elsewhere in the file (or package, for Go).
func (ix *Index) memberFooter(file, lang string, s Symbol, all []Symbol) string {
	switch s.Kind {
	case "struct", "interface", "type", "class", "trait", "impl", "enum":
	default:
		return ""
	}
	type mem struct {
		file, name string
		line       int
	}
	var ms []mem
	if lang == "go" {
		dir := path.Dir(file)
		rows, err := ix.db.Query(`SELECT f.path, s.name, s.decl FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.parent = ? AND s.kind = 'method' AND f.path LIKE ? ESCAPE '\' ORDER BY f.path, s.start`, s.Name, likeEscape(dir)+"/%")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m mem
				if rows.Scan(&m.file, &m.name, &m.line) == nil && path.Dir(m.file) == dir {
					ms = append(ms, m)
				}
			}
		}
	} else {
		for _, o := range all {
			if o.Parent == s.Name && o.Kind != "field" {
				ms = append(ms, mem{file, o.Name, o.Decl})
			}
		}
	}
	if len(ms) == 0 {
		return ""
	}
	var parts []string
	for i, m := range ms {
		if i >= 24 {
			parts = append(parts, fmt.Sprintf("+%d more", len(ms)-i))
			break
		}
		if m.file == file {
			parts = append(parts, fmt.Sprintf("%s:%d", m.name, m.line))
		} else {
			parts = append(parts, fmt.Sprintf("%s (%s:%d)", m.name, path.Base(m.file), m.line))
		}
	}
	label := "members"
	if lang == "go" {
		label = "methods"
	}
	return label + ": " + strings.Join(parts, ", ")
}

// showSymbol finds a symbol by name anywhere and shows its body.
func (ix *Index) showSymbol(ctx context.Context, name string, o ShowOptions) (*ShowResult, error) {
	hits, err := ix.LookupSymbols(ctx, name, 30)
	if err != nil {
		return nil, err
	}
	var best []SymbolHit
	for _, h := range hits {
		if h.Score >= 70 && h.Symbol.Kind != "field" || len(best) == 0 && h.Score >= 70 {
			best = append(best, h)
		}
	}
	if len(best) == 0 {
		if _, n := splitQualified(name); len(hits) > 0 {
			var names []string
			for _, h := range hits[:min(len(hits), 6)] {
				names = append(names, fmt.Sprintf("%s:%s", h.Path, h.Symbol.Qualified()))
			}
			return nil, fmt.Errorf("no file or symbol %q; similar: %s", name, strings.Join(names, ", "))
		} else {
			_ = n
		}
		return nil, fmt.Errorf("no file or symbol %q (wp code find %s searches text too)", name, name)
	}
	top := best[0]
	res, err := ix.showFileSymbol(ctx, top.Path, top.Symbol.Qualified(), o)
	if err != nil {
		// the index is a moment behind the file: fall back to the plain name
		if res, err = ix.showFileSymbol(ctx, top.Path, top.Symbol.Name, o); err != nil {
			return nil, err
		}
	}
	if len(best) > 1 {
		res.Candidates = best[1:min(len(best), 9)]
		res.Note = fmt.Sprintf("%d symbols named %s; showing the first (%s). Others: wp code show FILE:%s", len(best), name, top.Path, name)
	}
	return res, nil
}

// callers finds lines that use a symbol, outside its own definition. For
// Go it looks only in the same package and in packages that import it.
func (ix *Index) callers(ctx context.Context, file string, s Symbol, perFile int) []FileHit {
	name := s.Name
	if len(name) < 2 {
		return nil
	}
	cands, err := ix.ftsFiles(ctx, []string{strings.ToLower(name)}, name, 120)
	if err != nil {
		return nil
	}
	allowed := map[string]bool{}
	narrow := strings.HasSuffix(file, ".go")
	if narrow {
		dir := path.Dir(file)
		imp, _ := ix.Importers(file)
		for _, p := range imp {
			allowed[p] = true
		}
		for _, c := range cands {
			if path.Dir(c.path) == dir {
				allowed[c.path] = true
			}
		}
	}
	var out []FileHit
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	for _, c := range cands {
		if narrow && !allowed[c.path] || !strings.HasSuffix(c.path, path.Ext(file)) && narrow {
			continue
		}
		skip := map[int]bool{}
		if c.path == file {
			for l := s.Start; l <= s.End; l++ {
				skip[l] = true
			}
		}
		fh, err := ix.fileHit(c, []string{strings.ToLower(name)}, name, skip, perFile)
		if err != nil || len(fh.Hits) == 0 {
			continue
		}
		if s.Parent != "" && narrow {
			kept := fh.Hits[:0]
			for _, h := range fh.Hits {
				if strings.Contains(h.Text, "."+name) {
					kept = append(kept, h)
				}
			}
			if fh.Hits = kept; len(kept) == 0 {
				continue
			}
		}
		out = append(out, fh)
		if len(out) >= 15 {
			break
		}
	}
	return out
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", max(1, int(d.Minutes())))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 90*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
}

// Render is the plain-text form of a Show: numbered lines, or an outline.
func (r *ShowResult) Render(budget int) string {
	var b strings.Builder
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	if r.Outline != nil {
		b.WriteString(r.Outline.Render(budget))
	}
	for i, blk := range r.Blocks {
		if i > 0 {
			b.WriteByte('\n')
		}
		blk.render(&b)
	}
	if len(r.Callers) > 0 {
		b.WriteString("\nused by:\n")
		for _, f := range r.Callers {
			fmt.Fprintf(&b, "  %s\n", f.Path)
			for _, h := range f.Hits {
				fmt.Fprintf(&b, "    %d  %s", h.Line, h.Text)
				if h.In != "" {
					fmt.Fprintf(&b, "   [in %s]", h.In)
				}
				b.WriteByte('\n')
			}
		}
	}
	if len(r.Candidates) > 0 {
		b.WriteString("\nalso named that:\n")
		for _, c := range r.Candidates {
			fmt.Fprintf(&b, "  %s:%d  %s %s\n", c.Path, c.Symbol.Decl, c.Symbol.Kind, c.Symbol.Qualified())
		}
	}
	return b.String()
}

func (blk Block) render(b *strings.Builder) {
	hdr := fmt.Sprintf("%s:%d-%d", blk.Path, blk.Start, blk.End)
	if blk.Symbol != "" {
		hdr += "  " + blk.Kind + " " + blk.Symbol
	}
	if blk.Start == 1 && blk.End == blk.Total && blk.Symbol == "" {
		hdr = fmt.Sprintf("%s  (%d lines)", blk.Path, blk.Total)
	}
	b.WriteString(hdr + "\n")
	width := len(strconv.Itoa(blk.End))
	for i, l := range blk.Lines {
		fmt.Fprintf(b, "%*d  %s\n", width, blk.Start+i, l)
	}
	if blk.Omitted > 0 {
		fmt.Fprintf(b, "… %d more lines (wp code show %s:%d-%d)\n", blk.Omitted, blk.Path, blk.End+1, blk.End+blk.Omitted)
	}
	if blk.Footer != "" {
		b.WriteString(blk.Footer + "\n")
	}
}

// Render is the outline text: a header with signals, then the symbols.
func (o *Outline) Render(budget int) string {
	if budget <= 0 {
		budget = DefaultOutlineBudget
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s, %d lines", o.Path, o.Lang, o.Lines)
	if s := o.Signals; s != nil {
		if s.Hotspot {
			b.WriteString("  HOTSPOT")
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "history: %d commits, last %s by %s (%q); owner %s %d%%", s.Commits, ago(s.LastChange), s.LastAuthor, collapse(s.LastSubject, 70), s.Owner, int(s.OwnerPct*100+0.5))
		if s.Fixes > 0 {
			fmt.Fprintf(&b, "; %d bug fix%s, last %s", s.Fixes, map[bool]string{true: "es", false: ""}[s.Fixes != 1], ago(s.LastFix))
		}
		b.WriteByte('\n')
	} else {
		b.WriteByte('\n')
	}
	if len(o.CoChange) > 0 {
		var parts []string
		for _, c := range o.CoChange {
			parts = append(parts, fmt.Sprintf("%s (%d)", c.Path, c.Support))
		}
		b.WriteString("changes with: " + strings.Join(parts, ", ") + "\n")
	}
	if len(o.Imports) > 0 {
		b.WriteString("imports: " + joinCap(o.Imports, 8) + "\n")
	}
	if len(o.ImportedBy) > 0 {
		fmt.Fprintf(&b, "imported by %d: %s\n", len(o.ImportedBy), joinCap(o.ImportedBy, 6))
	}
	b.WriteString("symbols:\n")
	used := Tokens(b.String())
	shown, skipped := 0, 0
	syms := make([]Symbol, 0, len(o.Symbols))
	for _, s := range o.Symbols {
		if s.Kind != "field" && s.Kind != "var" || len(o.Symbols) <= 40 && s.Kind == "var" {
			syms = append(syms, s)
		}
	}
	for _, s := range syms {
		line := symbolLine(s)
		if used+Tokens(line) > budget {
			skipped = len(syms) - shown
			break
		}
		used += Tokens(line)
		shown++
		b.WriteString(line)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "  … %d more symbols (wp code show %s:START-END, or FILE:NAME)\n", skipped, o.Path)
	}
	if len(syms) == 0 {
		b.WriteString("  (none found; wp code show " + o.Path + " --full reads it)\n")
	}
	return b.String()
}

func joinCap(s []string, n int) string {
	if len(s) <= n {
		return strings.Join(s, ", ")
	}
	return strings.Join(s[:n], ", ") + fmt.Sprintf(", +%d", len(s)-n)
}

func symbolLine(s Symbol) string {
	indent := "  "
	switch {
	case s.Kind == "heading":
		indent += strings.Repeat("  ", max(0, strings.Count(strings.SplitN(s.Sig, " ", 2)[0], "#")-1))
	case s.Parent != "" && s.Kind != "method" && s.Kind != "func" || s.Parent != "" && !strings.Contains(s.Sig, "("):
		indent += "  "
	case s.Parent != "" && (s.Kind == "method" || s.Kind == "func") && strings.HasPrefix(s.Sig, "func ("):
	case s.Parent != "":
		indent += "  "
	}
	text := s.Sig
	if text == "" {
		text = s.Kind + " " + s.Name
	}
	text = collapse(text, 100)
	rng := fmt.Sprintf("%d", s.Decl)
	if s.End > s.Decl {
		rng = fmt.Sprintf("%d-%d", s.Decl, s.End)
	}
	out := fmt.Sprintf("%s%-9s %s", indent, rng, text)
	if s.Doc != "" && s.Kind != "heading" {
		out += "  // " + collapse(s.Doc, 70)
	}
	return out + "\n"
}
