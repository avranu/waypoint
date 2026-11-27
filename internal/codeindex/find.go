package codeindex

import (
	"context"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Find modes. "auto" routes by the shape of the query.
const (
	ModeAuto   = "auto"
	ModeSymbol = "symbol"
	ModePath   = "path"
	ModeText   = "text"
)

// Default sizes of a find answer.
const (
	DefaultBudget  = 2500
	defaultPerFile = 3
	defaultFiles   = 12
	maxLineWidth   = 150
)

// FindOptions tunes a Find. The zero value is the default.
type FindOptions struct {
	Mode    string // auto (default), symbol, path or text
	Budget  int    // approximate tokens the rendered answer may use
	PerFile int    // lines shown per file
	Files   int    // files considered
}

// LineHit is one matching line (or definition) in a file.
type LineHit struct {
	Line int    `json:"line"`
	Text string `json:"text"`
	In   string `json:"in,omitempty"`  // the symbol containing the line
	Def  string `json:"def,omitempty"` // the kind, when the line declares a symbol that matches
}

// FileHit is one file in a Find answer.
type FileHit struct {
	Path    string    `json:"path"`
	Lang    string    `json:"lang"`
	Lines   int       `json:"lines"`
	Score   float64   `json:"score"`
	Hotspot bool      `json:"hotspot,omitempty"`
	Fixes   int       `json:"fixes,omitempty"`
	Hits    []LineHit `json:"hits,omitempty"`
	Symbols []string  `json:"symbols,omitempty"` // path hits: the file's main symbols
}

// FindResult is a ranked per-file digest of the matches.
type FindResult struct {
	Query string    `json:"query"`
	Mode  string    `json:"mode"`
	Total int       `json:"total"` // files that matched
	Files []FileHit `json:"files"`
	More  []string  `json:"more,omitempty"` // matched files left out for budget
	Note  string    `json:"note,omitempty"`
}

var (
	identRE = lazyre.New(`^[A-Za-z_$#@][\w$#]*(?:(?:\.|::|->|#)[A-Za-z_$][\w$]*)*$`)
	wordRE  = lazyre.New(`[\p{L}\p{N}_]+`)
)

var stopwords = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "is": true, "are": true, "and": true, "or": true, "for": true, "how": true, "does": true, "do": true, "where": true, "what": true, "which": true, "why": true, "it": true, "this": true, "that": true, "with": true, "on": true, "by": true, "be": true, "as": true, "at": true, "from": true, "i": true, "we": true, "can": true, "when": true}

// Classify says which mode a query is: a path, an identifier or prose.
func Classify(q string) string {
	q = strings.TrimSpace(q)
	switch {
	case q == "":
		return ModeText
	case strings.ContainsAny(q, " \t\n"):
		return ModeText
	case strings.ContainsAny(q, "*?") || strings.Contains(q, "/") || strings.HasPrefix(q, "."):
		return ModePath
	case identRE.MatchString(q) && !strings.Contains(q, "@"):
		if ext := path.Ext(q); ext != "" && Language(q) != "" && !isCommonDotted(q) {
			return ModePath
		}
		return ModeSymbol
	}
	return ModeText
}

// isCommonDotted guards Foo.Bar style names from being read as file names:
// only a real extension (.go, .py, ...) of a short suffix makes a path.
func isCommonDotted(q string) bool {
	ext := path.Ext(q)
	if len(ext) < 2 || len(ext) > 6 {
		return true
	}
	return ext[1] >= 'A' && ext[1] <= 'Z'
}

// Find answers a query with a ranked, budgeted per-file digest: the best
// lines of each file with the symbol containing them.
func (ix *Index) Find(ctx context.Context, q string, o FindOptions) (*FindResult, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.PerFile <= 0 {
		o.PerFile = defaultPerFile
	}
	if o.Files <= 0 {
		o.Files = defaultFiles
	}
	mode := o.Mode
	if mode == "" || mode == ModeAuto {
		mode = Classify(q)
	}
	var res *FindResult
	var err error
	switch mode {
	case ModePath:
		res, err = ix.findPath(q, o)
		if err == nil && res.Total == 0 {
			res, err = ix.findText(ctx, q, o)
			if res != nil {
				res.Note = "no file path matches; searched text"
			}
		}
	case ModeSymbol:
		res, err = ix.findSymbol(ctx, q, o)
	case ModeText:
		res, err = ix.findText(ctx, q, o)
	default:
		return nil, fmt.Errorf("unknown mode %q (auto, symbol, path, text)", mode)
	}
	if err != nil {
		return nil, err
	}
	res.Query = q
	fit(res, o.Budget)
	return res, nil
}

// queryTerms are the lower-case words to look for.
func queryTerms(q string) []string {
	var terms []string
	seen := map[string]bool{}
	for _, w := range wordRE.FindAllString(q, -1) {
		lw := strings.ToLower(w)
		if seen[lw] || (len(lw) < 2 && len(wordRE.FindAllString(q, -1)) > 1) {
			continue
		}
		if stopwords[lw] && len(wordRE.FindAllString(q, -1)) > 1 {
			continue
		}
		seen[lw] = true
		terms = append(terms, lw)
	}
	return terms
}

func ftsQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ftsGroups builds one alternative group per term: the word itself (as a
// prefix when it is long enough) or, for an identifier, the phrase of its
// split words.
func ftsGroups(terms []string, raw string) []string {
	var groups []string
	rawWords := wordRE.FindAllString(raw, -1)
	byLower := map[string]string{}
	for _, w := range rawWords {
		byLower[strings.ToLower(w)] = w
	}
	for _, t := range terms {
		alts := []string{ftsQuote(t)}
		if len(t) >= 3 {
			alts = []string{ftsQuote(t) + "*"}
		}
		if w, ok := byLower[t]; ok {
			if sw := splitWords(w); len(sw) > 1 {
				alts = append(alts, ftsQuote(strings.Join(sw, " ")))
			}
		}
		if len(alts) == 1 {
			groups = append(groups, alts[0])
		} else {
			groups = append(groups, "("+strings.Join(alts, " OR ")+")")
		}
	}
	return groups
}

type cand struct {
	id    int64
	path  string
	lang  string
	lines int
	score float64
}

// ftsFiles runs the full-text query: all terms first, any term if that is too few.
func (ix *Index) ftsFiles(ctx context.Context, terms []string, raw string, limit int) ([]cand, error) {
	groups := ftsGroups(terms, raw)
	if len(groups) == 0 {
		return nil, nil
	}
	run := func(expr string) ([]cand, error) {
		rows, err := ix.db.QueryContext(ctx, `SELECT f.id, f.path, f.lang, f.lines, bm25(docs, 4.0, 6.0, 1.0) FROM docs JOIN files f ON f.id = docs.rowid WHERE docs MATCH ? ORDER BY bm25(docs, 4.0, 6.0, 1.0) LIMIT ?`, expr, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []cand
		for rows.Next() {
			var c cand
			var r float64
			if err := rows.Scan(&c.id, &c.path, &c.lang, &c.lines, &r); err != nil {
				return nil, err
			}
			c.score = -r
			out = append(out, c)
		}
		return out, rows.Err()
	}
	out, err := run(strings.Join(groups, " AND "))
	if err != nil {
		return nil, err
	}
	if len(out) < 3 && len(groups) > 1 {
		more, err := run(strings.Join(groups, " OR "))
		if err != nil {
			return nil, err
		}
		have := map[int64]bool{}
		for _, c := range out {
			have[c.id] = true
		}
		for _, c := range more {
			if !have[c.id] {
				c.score *= 0.5
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// adjust applies the ranking nudges: tests and docs below source.
func adjust(p string, score float64, q string) float64 {
	lq := strings.ToLower(q)
	if isTestPath(p) && !strings.Contains(lq, "test") {
		score *= 0.7
	}
	switch path.Ext(p) {
	case ".md":
		score *= 0.9
	case ".json", ".txt", ".lock", ".sql":
		score *= 0.8
	}
	return score
}

func isTestPath(p string) bool {
	b := strings.ToLower(path.Base(p))
	return strings.HasSuffix(b, "_test.go") || (strings.HasPrefix(b, "test_") && !strings.HasSuffix(b, ".go")) || strings.HasSuffix(b, "_test.py") ||
		strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") || strings.Contains(p, "/tests/") || strings.Contains(p, "/__tests__/") || strings.HasPrefix(p, "tests/") || strings.HasPrefix(p, "e2e/")
}

func (ix *Index) findText(ctx context.Context, q string, o FindOptions) (*FindResult, error) {
	terms := queryTerms(q)
	res := &FindResult{Mode: ModeText}
	cands, err := ix.ftsFiles(ctx, terms, q, 80)
	if err != nil {
		return nil, err
	}
	for i := range cands {
		cands[i].score = adjust(cands[i].path, cands[i].score, q)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	res.Total = len(cands)
	for _, c := range cands {
		if len(res.Files) >= o.Files {
			res.More = append(res.More, c.path)
			continue
		}
		fh, err := ix.fileHit(c, terms, "", nil, o.PerFile)
		if err != nil {
			return nil, err
		}
		if len(fh.Hits) == 0 {
			fh.Symbols = ix.mainSymbols(c.id, 5)
		}
		res.Files = append(res.Files, fh)
	}
	return res, nil
}

// fileHit reads the file from disk and picks its best lines for the terms.
// ident, when set, restricts lines to those naming that identifier; skip
// lists lines (1-based) not to repeat.
func (ix *Index) fileHit(c cand, terms []string, ident string, skip map[int]bool, perFile int) (FileHit, error) {
	fh := FileHit{Path: c.path, Lang: c.lang, Lines: c.lines, Score: c.score}
	sig, _ := ix.Signals(c.path)
	if sig != nil {
		fh.Hotspot, fh.Fixes = sig.Hotspot, sig.Fixes
	}
	src, err := os.ReadFile(filepath.Join(ix.Root, filepath.FromSlash(c.path))) // #nosec G304 -- an indexed file of this checkout
	if err != nil {
		return fh, nil
	}
	lines := splitLines(src)
	syms, err := ix.fileSymbols(c.id)
	if err != nil {
		return fh, err
	}
	picked := pickLines(lines, terms, ident, skip, perFile)
	for _, l := range picked {
		h := LineHit{Line: l, Text: clip(lines[l-1], terms, ident)}
		h.In = containing(syms, l, true)
		for _, s := range syms {
			if s.Decl == l && s.Kind != "field" {
				h.Def = s.Kind
				if h.In == s.Qualified() {
					h.In = ""
				}
				break
			}
		}
		fh.Hits = append(fh.Hits, h)
	}
	return fh, nil
}

// pickLines scores lines by how many of the terms they hold and returns the
// best perFile of them in file order.
func pickLines(lines, terms []string, ident string, skip map[int]bool, perFile int) []int {
	need := 1
	if len(terms) > 2 {
		need = (len(terms) + 1) / 2
	}
	if out := pickLinesNeed(lines, terms, ident, skip, perFile, need); len(out) > 0 || need == 1 {
		return out
	}
	return pickLinesNeed(lines, terms, ident, skip, perFile, 1)
}

// pickLinesNeed is pickLines for lines holding at least need of the terms.
func pickLinesNeed(lines, terms []string, ident string, skip map[int]bool, perFile, need int) []int {
	type sl struct {
		line  int
		score float64
	}
	var all []sl
	lowIdent := strings.ToLower(ident)
	for i, l := range lines {
		if len(l) > 600 || skip[i+1] {
			continue
		}
		low := strings.ToLower(l)
		if ident != "" && !strings.Contains(low, lowIdent) {
			continue
		}
		matched := 0
		score := 0.0
		for _, t := range terms {
			k := strings.Index(low, t)
			if k < 0 {
				continue
			}
			matched++
			score++
			if wordAt(low, k, len(t)) {
				score += 0.5
			}
		}
		if ident != "" {
			if matched == 0 {
				matched, score = 1, 1
			}
			if k := strings.Index(l, ident); k >= 0 && wordAt(l, k, len(ident)) {
				score += 1.5
			}
		}
		if matched < need {
			continue
		}
		t := strings.TrimSpace(l)
		if defLikeRE.MatchString(t) {
			score += 1
		}
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") {
			score -= 0.3
		}
		all = append(all, sl{i + 1, score})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > perFile {
		all = all[:perFile]
	}
	out := make([]int, len(all))
	for i, a := range all {
		out[i] = a.line
	}
	sort.Ints(out)
	return out
}

var defLikeRE = lazyre.New(`^(?:export\s+|pub\s+|public\s+|private\s+|async\s+|static\s+)*(?:func|def|class|type|struct|interface|fn|const|var|let|enum|trait|impl|object|fun)\b`)

func wordAt(s string, k, n int) bool {
	if k > 0 && isIdentByte(s[k-1]) {
		return false
	}
	return k+n >= len(s) || !isIdentByte(s[k+n])
}

// clip trims a line for display, centred on the first match when long.
func clip(l string, terms []string, ident string) string {
	t := strings.TrimSpace(strings.ReplaceAll(l, "\t", " "))
	if len(t) <= maxLineWidth {
		return t
	}
	low := strings.ToLower(t)
	k := -1
	if ident != "" {
		k = strings.Index(low, strings.ToLower(ident))
	}
	for _, term := range terms {
		if k >= 0 {
			break
		}
		k = strings.Index(low, term)
	}
	if k < 0 {
		k = 0
	}
	from := max(0, k-40)
	to := min(len(t), from+maxLineWidth)
	for from > 0 && t[from]&0xC0 == 0x80 {
		from--
	}
	for to < len(t) && t[to]&0xC0 == 0x80 {
		to++
	}
	out := t[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(t) {
		out += "…"
	}
	return out
}

// containing names the innermost symbol around a line.
func containing(syms []Symbol, line int, skipSelf bool) string {
	best := -1
	for i, s := range syms {
		if s.Kind == "field" || s.Start > line || s.End < line {
			continue
		}
		if skipSelf && s.Start == line {
			continue
		}
		if best < 0 || s.End-s.Start < syms[best].End-syms[best].Start || s.End-s.Start == syms[best].End-syms[best].Start && s.Start > syms[best].Start {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return syms[best].Qualified()
}

func (ix *Index) fileSymbols(id int64) ([]Symbol, error) {
	rows, err := ix.db.Query(`SELECT name, kind, parent, sig, doc, start, decl, end, exported FROM symbols WHERE file_id = ? ORDER BY start, end DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var s Symbol
		var ex int
		if err := rows.Scan(&s.Name, &s.Kind, &s.Parent, &s.Sig, &s.Doc, &s.Start, &s.Decl, &s.End, &ex); err != nil {
			return nil, err
		}
		s.Exported = ex != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// mainSymbols names a file's first few top-level symbols.
func (ix *Index) mainSymbols(id int64, n int) []string {
	rows, err := ix.db.Query(`SELECT kind, name, parent FROM symbols WHERE file_id = ? AND kind NOT IN ('field') AND (parent = '' OR kind IN ('method','func')) ORDER BY exported DESC, start LIMIT ?`, id, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k, nm, par string
		if rows.Scan(&k, &nm, &par) == nil {
			if par != "" {
				nm = par + "." + nm
			}
			out = append(out, shortKind(k)+" "+nm)
		}
	}
	return out
}

func shortKind(k string) string {
	switch k {
	case "struct", "interface":
		return "type"
	case "method":
		return "func"
	}
	return k
}

func (ix *Index) findPath(q string, o FindOptions) (*FindResult, error) {
	res := &FindResult{Mode: ModePath}
	needle := strings.ToLower(strings.TrimPrefix(filepath.ToSlash(q), "./"))
	rows, err := ix.db.Query(`SELECT id, path, lang, lines FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	glob := strings.ContainsAny(needle, "*?[")
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.path, &c.lang, &c.lines); err != nil {
			return nil, err
		}
		lp := strings.ToLower(c.path)
		base := path.Base(lp)
		switch {
		case lp == needle:
			c.score = 100
		case glob:
			ok, _ := path.Match(needle, lp)
			if !ok {
				ok, _ = path.Match(needle, base)
			}
			if !ok && strings.HasSuffix(needle, "/") {
				ok = strings.HasPrefix(lp, needle)
			}
			if !ok {
				continue
			}
			c.score = 50
		case base == needle:
			c.score = 85
		case strings.HasSuffix(lp, "/"+needle):
			c.score = 80
		case strings.HasSuffix(needle, "/") && strings.HasPrefix(lp, needle):
			c.score = 60
		case strings.HasPrefix(base, needle):
			c.score = 55
		case strings.Contains(base, needle):
			c.score = 45
		case strings.Contains(lp, needle):
			c.score = 30
		default:
			continue
		}
		c.score += 5 / float64(1+strings.Count(c.path, "/"))
		c.score = adjust(c.path, c.score, q)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].path < cands[j].path
	})
	res.Total = len(cands)
	limit := max(o.Files, 20)
	for _, c := range cands {
		if len(res.Files) >= limit {
			res.More = append(res.More, c.path)
			continue
		}
		fh := FileHit{Path: c.path, Lang: c.lang, Lines: c.lines, Score: c.score, Symbols: ix.mainSymbols(c.id, 6)}
		res.Files = append(res.Files, fh)
	}
	return res, nil
}

// SymbolHit is a symbol found by name.
type SymbolHit struct {
	Path   string  `json:"path"`
	Lang   string  `json:"lang"`
	Lines  int     `json:"lines"`
	Symbol Symbol  `json:"symbol"`
	Score  float64 `json:"score"`
	fileID int64
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func normName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != '_' && r != '-' && r != '$' {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// splitQualified splits Foo.Bar, Foo::bar, Foo#bar into parent and name.
func splitQualified(q string) (parent, name string) {
	q = strings.TrimPrefix(q, "@")
	for _, sep := range []string{"::", "->", "#", "."} {
		if i := strings.LastIndex(q, sep); i > 0 {
			return q[:i], q[i+len(sep):]
		}
	}
	return "", q
}

// LookupSymbols finds symbols by name, best first. A qualified name
// (Parent.Name) must match the parent too.
func (ix *Index) LookupSymbols(ctx context.Context, q string, limit int) ([]SymbolHit, error) {
	parent, name := splitQualified(strings.TrimSpace(q))
	if name == "" {
		return nil, nil
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT s.file_id, f.path, f.lang, f.lines, s.name, s.kind, s.parent, s.sig, s.doc, s.start, s.decl, s.end, s.exported
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE s.name = ?1 COLLATE NOCASE OR s.name LIKE '%' || ?2 || '%' ESCAPE '\' OR replace(s.name, '_', '') = ?3 COLLATE NOCASE
		LIMIT 3000`, name, likeEscape(name), strings.ReplaceAll(name, "_", ""))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nn := normName(name)
	ln := strings.ToLower(name)
	var out []SymbolHit
	for rows.Next() {
		var h SymbolHit
		var ex int
		s := &h.Symbol
		if err := rows.Scan(&h.fileID, &h.Path, &h.Lang, &h.Lines, &s.Name, &s.Kind, &s.Parent, &s.Sig, &s.Doc, &s.Start, &s.Decl, &s.End, &ex); err != nil {
			return nil, err
		}
		s.Exported = ex != 0
		if parent != "" && !strings.EqualFold(lastSegment(s.Parent), lastSegment(parent)) && !strings.EqualFold(path.Base(strings.TrimSuffix(h.Path, path.Ext(h.Path))), parent) {
			continue
		}
		low := strings.ToLower(s.Name)
		switch {
		case s.Name == name:
			h.Score = 100
		case low == ln:
			h.Score = 92
		case normName(s.Name) == nn:
			h.Score = 85
		case strings.HasPrefix(low, ln):
			h.Score = 60 - math.Min(20, float64(len(low)-len(ln)))
		case strings.Contains(low, ln):
			h.Score = 30 - math.Min(15, float64(len(low)-len(ln))/2)
		default:
			continue
		}
		h.Score += kindWeight(s.Kind)
		if s.Exported {
			h.Score += 3
		}
		if s.Parent == "" {
			h.Score += 2
		}
		if isTestPath(h.Path) {
			h.Score -= 15
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Symbol.Start < out[j].Symbol.Start
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func kindWeight(k string) float64 {
	switch k {
	case "struct", "interface", "class", "type", "trait", "enum", "func", "table", "view":
		return 12
	case "method", "ctor", "heading", "impl", "mod", "namespace", "macro":
		return 9
	case "const", "var", "index", "function":
		return 6
	case "key", "block", "resource":
		return 3
	case "field":
		return 1
	}
	return 4
}

func (ix *Index) findSymbol(ctx context.Context, q string, o FindOptions) (*FindResult, error) {
	res := &FindResult{Mode: ModeSymbol}
	_, name := splitQualified(q)
	hits, err := ix.LookupSymbols(ctx, q, 400)
	if err != nil {
		return nil, err
	}
	type group struct {
		cand
		syms []SymbolHit
	}
	byFile := map[string]*group{}
	var order []*group
	for _, h := range hits {
		g := byFile[h.Path]
		if g == nil {
			g = &group{cand: cand{id: h.fileID, path: h.Path, lang: h.Lang, lines: h.Lines}}
			byFile[h.Path] = g
			order = append(order, g)
		}
		g.syms = append(g.syms, h)
		if len(g.syms) == 1 {
			g.score = h.Score
		} else {
			g.score += 0.1 * h.Score
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].score > order[j].score })
	terms := queryTerms(name)
	var uses []cand
	if len(order) < o.Files*2 {
		cs, err := ix.ftsFiles(ctx, terms, name, 60)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if byFile[c.path] == nil {
				c.score = adjust(c.path, c.score, name)
				uses = append(uses, c)
			}
		}
		sort.SliceStable(uses, func(i, j int) bool { return uses[i].score > uses[j].score })
	}
	res.Total = len(order) + len(uses)
	if res.Total == 0 {
		return ix.findText(ctx, q, o)
	}
	shown := 0
	for _, g := range order {
		if shown >= o.Files {
			res.More = append(res.More, g.path)
			continue
		}
		shown++
		fh := FileHit{Path: g.path, Lang: g.lang, Lines: g.lines, Score: g.score}
		if sig, _ := ix.Signals(g.path); sig != nil {
			fh.Hotspot, fh.Fixes = sig.Hotspot, sig.Fixes
		}
		for i, h := range g.syms {
			if i >= o.PerFile+1 {
				break
			}
			text := h.Symbol.Sig
			if text == "" {
				text = h.Symbol.Name
			}
			lh := LineHit{Line: h.Symbol.Decl, Text: clip(text, nil, ""), Def: h.Symbol.Kind}
			if h.Symbol.Doc != "" {
				lh.Text = clip(text+"  // "+h.Symbol.Doc, nil, "")
			}
			fh.Hits = append(fh.Hits, lh)
		}
		sort.SliceStable(fh.Hits, func(i, j int) bool { return fh.Hits[i].Line < fh.Hits[j].Line })
		res.Files = append(res.Files, fh)
	}
	for _, c := range uses {
		if shown >= o.Files {
			res.More = append(res.More, c.path)
			continue
		}
		fh, err := ix.fileHit(c, terms, name, nil, 2)
		if err != nil {
			return nil, err
		}
		if len(fh.Hits) == 0 {
			res.Total--
			continue
		}
		shown++
		res.Files = append(res.Files, fh)
	}
	return res, nil
}

// Tokens is the rough token count of a rendered answer (4 bytes a token).
func Tokens(s string) int { return (len(s) + 3) / 4 }

// fit drops files, then lines, until the rendering fits the budget.
func fit(res *FindResult, budget int) {
	for {
		if Tokens(res.Render()) <= budget {
			return
		}
		n := len(res.Files)
		if n > 1 {
			res.More = append([]string{res.Files[n-1].Path}, res.More...)
			res.Files = res.Files[:n-1]
			continue
		}
		if n == 1 && len(res.Files[0].Hits) > 1 {
			res.Files[0].Hits = res.Files[0].Hits[:len(res.Files[0].Hits)-1]
			continue
		}
		if n == 1 && len(res.Files[0].Symbols) > 1 {
			res.Files[0].Symbols = res.Files[0].Symbols[:len(res.Files[0].Symbols)-1]
			continue
		}
		return
	}
}

// Render is the plain-text digest.
func (r *FindResult) Render() string {
	var b strings.Builder
	if len(r.Files) == 0 {
		fmt.Fprintf(&b, "No matches for %q.\n", r.Query)
		return b.String()
	}
	more := ""
	if len(r.More) > 0 {
		more = fmt.Sprintf(", showing %d", len(r.Files))
	}
	fmt.Fprintf(&b, "%s: %d file%s%s (%s)\n", r.Query, r.Total, plural(r.Total), more, r.Mode)
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	for _, f := range r.Files {
		fmt.Fprintf(&b, "%s  %s, %d lines", f.Path, f.Lang, f.Lines)
		if f.Hotspot {
			b.WriteString("  HOTSPOT")
		}
		if f.Fixes > 0 {
			fmt.Fprintf(&b, "  %d fix%s", f.Fixes, map[bool]string{true: "es", false: ""}[f.Fixes != 1])
		}
		b.WriteByte('\n')
		if len(f.Symbols) > 0 {
			fmt.Fprintf(&b, "  %s\n", strings.Join(f.Symbols, "; "))
		}
		width := 1
		for _, h := range f.Hits {
			width = max(width, len(fmt.Sprint(h.Line)))
		}
		for _, h := range f.Hits {
			fmt.Fprintf(&b, "  %*d  %s", width, h.Line, h.Text)
			if h.In != "" {
				fmt.Fprintf(&b, "   [in %s]", h.In)
			}
			b.WriteByte('\n')
		}
	}
	if len(r.More) > 0 {
		list := r.More
		suffix := ""
		if len(list) > 12 {
			list, suffix = list[:12], fmt.Sprintf(" … +%d", len(r.More)-12)
		}
		fmt.Fprintf(&b, "+%d more: %s%s  (narrow the query or raise --budget)\n", len(r.More), strings.Join(list, ", "), suffix)
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
