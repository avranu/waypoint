package codeindex

import (
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// FileRisk is one changed file.
type FileRisk struct {
	Path        string    `json:"path"`
	Indexed     bool      `json:"indexed"` // false: new, untracked or skipped (no history to read)
	Lang        string    `json:"lang,omitempty"`
	Hotspot     bool      `json:"hotspot"`
	HotPct      float64   `json:"hot_pct,omitempty"`
	Commits     int       `json:"commits"`
	Fixes       int       `json:"fixes"`
	LastFix     time.Time `json:"last_fix,omitzero"`
	Escapes     int       `json:"escapes,omitempty"` // bug fixes in the escape ledger (WP-705)
	LastEscape  time.Time `json:"last_escape,omitzero"`
	AreaEscapes int       `json:"area_escapes,omitempty"` // the same, for every file in its directory
	Owner       string    `json:"owner,omitempty"`
	OwnerPct    float64   `json:"owner_pct,omitempty"`
	LastChange  time.Time `json:"last_change,omitzero"`
	LastAuthor  string    `json:"last_author,omitempty"`
	LastSubject string    `json:"last_subject,omitempty"`
	Importers   int       `json:"importers"` // files that import it (blast radius)
	ImportedBy  []string  `json:"imported_by,omitempty"`
	Tests       []string  `json:"tests,omitempty"`
	Score       float64   `json:"score"`
}

// MissingEdit is a file that changed with changed files before but is not in
// this change.
type MissingEdit struct {
	Path       string   `json:"path"`
	With       []string `json:"with"`       // the changed files it goes with
	Support    int      `json:"support"`    // commits where it changed with the most-linked one
	Confidence float64  `json:"confidence"` // support over that file's own commits
}

// RiskResult is the whole answer.
type RiskResult struct {
	Summary string        `json:"summary"`
	Files   []FileRisk    `json:"files"`
	Missing []MissingEdit `json:"missing"`
	Tests   []string      `json:"tests"`            // what to run: test files, Go packages as "go test ./dir"
	Omitted int           `json:"omitted"`          // files left out of the text for the budget
	Unknown []string      `json:"unknown"`          // named files the index doesn't know
	History string        `json:"history"`          // how much history this rests on
	Ledger  bool          `json:"ledger,omitempty"` // past fixes come from the escape ledger, not commit subjects
	Flags   RiskTotals    `json:"totals"`
}

// RiskTotals counts what the summary line says.
type RiskTotals struct {
	Files     int `json:"files"`
	Hotspots  int `json:"hotspots"`
	WithFixes int `json:"with_fixes"`
	Missing   int `json:"missing"`
	Importers int `json:"importers"`
}

const (
	missMinSupport    = 3
	missMinConfidence = 0.25
	maxMissing        = 10
	maxTestsPerFile   = 4
)

// Risk assesses changed files (paths relative to the checkout). Paths the
// index doesn't hold are listed as Unknown and still checked for tests.
func (ix *Index) Risk(changed []string) (*RiskResult, error) {
	changed = dedupe(changed)
	in := map[string]bool{}
	for _, p := range changed {
		in[p] = true
	}
	res := &RiskResult{Files: []FileRisk{}, Missing: []MissingEdit{}, Tests: []string{}, Unknown: []string{}}
	type miss struct {
		with       []string
		support    int
		confidence float64
	}
	missing := map[string]*miss{}
	tests := map[string]bool{}
	for _, p := range changed {
		fr := FileRisk{Path: p}
		known := false
		_ = ix.db.QueryRow(`SELECT lang FROM files WHERE path = ?`, p).Scan(&fr.Lang)
		known = fr.Lang != ""
		if !known {
			res.Unknown = append(res.Unknown, p)
		}
		sig, err := ix.Signals(p)
		if err != nil {
			return nil, err
		}
		if sig != nil {
			fr.Indexed = true
			fr.Hotspot, fr.HotPct, fr.Commits, fr.Fixes = sig.Hotspot, sig.HotPct, sig.Commits, sig.Fixes
			fr.LastFix, fr.Owner, fr.OwnerPct = sig.LastFix, sig.Owner, sig.OwnerPct
			fr.LastChange, fr.LastAuthor, fr.LastSubject = sig.LastChange, sig.LastAuthor, sig.LastSubject
		}
		imps, err := ix.Importers(p)
		if err != nil {
			return nil, err
		}
		fr.Importers = len(imps)
		for _, i := range imps {
			if len(fr.ImportedBy) < 3 && !isTestPath(i) {
				fr.ImportedBy = append(fr.ImportedBy, i)
			}
		}
		if known || sig != nil {
			co, err := ix.CoChanged(p, coKeep)
			if err != nil {
				return nil, err
			}
			for _, c := range co {
				if in[c.Path] || c.Support < missMinSupport || c.Confidence < missMinConfidence {
					continue
				}
				if !ix.hasFile(c.Path) {
					continue
				}
				m := missing[c.Path]
				if m == nil {
					m = &miss{}
					missing[c.Path] = m
				}
				m.with = append(m.with, p)
				if c.Support > m.support {
					m.support, m.confidence = c.Support, c.Confidence
				}
			}
		}
		fr.Tests = ix.testsFor(p, imps)
		for _, t := range fr.Tests {
			tests[t] = true
		}
		fr.Score = riskScore(fr)
		res.Files = append(res.Files, fr)
	}
	sort.SliceStable(res.Files, func(i, j int) bool { return res.Files[i].Score > res.Files[j].Score })
	for p, m := range missing {
		res.Missing = append(res.Missing, MissingEdit{Path: p, With: m.with, Support: m.support, Confidence: m.confidence})
	}
	sort.Slice(res.Missing, func(i, j int) bool {
		a, b := res.Missing[i], res.Missing[j]
		if len(a.With) != len(b.With) {
			return len(a.With) > len(b.With)
		}
		if a.Support != b.Support {
			return a.Support > b.Support
		}
		return a.Path < b.Path
	})
	if len(res.Missing) > maxMissing {
		res.Missing = res.Missing[:maxMissing]
	}
	res.Tests = testRuns(tests, in)
	for _, f := range res.Files {
		res.Flags.Files++
		res.Flags.Importers += f.Importers
		if f.Hotspot {
			res.Flags.Hotspots++
		}
		if f.Fixes > 0 {
			res.Flags.WithFixes++
		}
	}
	res.Flags.Missing = len(res.Missing)
	st := ix.Status()
	res.History = fmt.Sprintf("%d commits up to %s", st.Commits, short(st.HistoryHead))
	res.Summary = res.summary()
	return res, nil
}

// EscapeCount is one file's count in the escape ledger.
type EscapeCount struct {
	Escapes int
	LastAt  time.Time
}

// escapesEnough is how many traced fixes a repo needs before the ledger
// stands in for the keyword count: fewer say more about the ledger's age
// than about the code.
const escapesEnough = 20

// ApplyEscapes replaces fix-worded commit counts with the escape ledger's,
// for a repo whose ledger has total fixes, and scores the files again.
func (r *RiskResult) ApplyEscapes(total int, counts map[string]EscapeCount) {
	if total < escapesEnough {
		return
	}
	area := map[string]int{}
	for p, c := range counts {
		area[path.Dir(p)] += c.Escapes
	}
	r.Ledger = true
	r.Flags.WithFixes = 0
	for i := range r.Files {
		f := &r.Files[i]
		c := counts[f.Path]
		f.Fixes, f.LastFix = 0, time.Time{}
		f.Escapes, f.LastEscape, f.AreaEscapes = c.Escapes, c.LastAt, area[path.Dir(f.Path)]
		if f.Escapes > 0 {
			r.Flags.WithFixes++
		}
		f.Score = riskScore(*f)
	}
	sort.SliceStable(r.Files, func(i, j int) bool { return r.Files[i].Score > r.Files[j].Score })
	r.Summary = r.summary()
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func (ix *Index) hasFile(p string) bool {
	var n int
	_ = ix.db.QueryRow(`SELECT count(*) FROM files WHERE path = ?`, p).Scan(&n)
	return n > 0
}

// riskScore orders files: hotspots and fixed-before files first, then the
// ones many others depend on.
func riskScore(f FileRisk) float64 {
	s := 0.0
	if f.Hotspot {
		s += 3
	}
	switch {
	case f.Fixes >= 5:
		s += 3
	case f.Fixes >= 2:
		s += 2
	case f.Fixes == 1:
		s += 1
	}
	switch {
	case f.Escapes >= 3:
		s += 3
	case f.Escapes == 2:
		s += 2
	case f.Escapes == 1:
		s += 1
	}
	if f.AreaEscapes >= 5 {
		s++
	}
	switch {
	case f.Importers >= 20:
		s += 2
	case f.Importers >= 5:
		s += 1
	}
	s += f.HotPct // 0..1 breaks ties by churn
	return s
}

// testsFor finds tests for a file: the file itself when it is one, Go tests
// in its package, test files named for it, tests that import it, and tests
// that change with it.
func (ix *Index) testsFor(p string, importers []string) []string {
	if isTestPath(p) {
		return []string{p}
	}
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != p && !seen[t] && len(out) < maxTestsPerFile {
			seen[t] = true
			out = append(out, t)
		}
	}
	dir := path.Dir(p)
	stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
	if strings.HasSuffix(p, ".go") {
		// One query or the other: a second one while the first's rows are
		// open waits forever on the index's single connection.
		var rows *sql.Rows
		var err error
		if dir == "." {
			rows, err = ix.db.Query(`SELECT path FROM files WHERE path NOT LIKE '%/%' AND path LIKE '%\_test.go' ESCAPE '\' ORDER BY path`)
		} else {
			rows, err = ix.db.Query(`SELECT path FROM files WHERE path LIKE ? AND path NOT LIKE ? AND path LIKE '%\_test.go' ESCAPE '\' ORDER BY path`, likeEsc(dir)+"/%", likeEsc(dir)+"/%/%")
		}
		if err == nil {
			var named, rest []string
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil {
					if strings.HasPrefix(path.Base(t), stem+"_") || path.Base(t) == stem+"_test.go" {
						named = append(named, t)
					} else {
						rest = append(rest, t)
					}
				}
			}
			_ = rows.Close()
			for _, t := range append(named, rest...) {
				add(t)
			}
		}
	} else if len(stem) >= 3 {
		rows, err := ix.db.Query(`SELECT path FROM files WHERE path LIKE ? ESCAPE '\' ORDER BY length(path)`, "%"+likeEsc(stem)+"%")
		if err == nil {
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil && isTestPath(t) &&
					strings.Contains(strings.ToLower(path.Base(t)), strings.ToLower(stem)) {
					add(t)
				}
			}
			_ = rows.Close()
		}
	}
	for _, i := range importers {
		if isTestPath(i) {
			add(i)
		}
	}
	if co, err := ix.CoChanged(p, coKeep); err == nil {
		for _, c := range co {
			if isTestPath(c.Path) && ix.hasFile(c.Path) {
				add(c.Path)
			}
		}
	}
	return out
}

func likeEsc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// testRuns turns test files into what to run: a Go package once ("go test
// ./dir"), any other test file by path. Tests that are part of the change
// come first.
func testRuns(tests, changed map[string]bool) []string {
	pk := map[string]bool{}
	var out []string
	var paths []string
	for t := range tests {
		paths = append(paths, t)
	}
	sort.Slice(paths, func(i, j int) bool {
		if changed[paths[i]] != changed[paths[j]] {
			return changed[paths[i]]
		}
		return paths[i] < paths[j]
	})
	for _, t := range paths {
		if strings.HasSuffix(t, ".go") {
			d := "./" + path.Dir(t)
			if path.Dir(t) == "." {
				d = "."
			}
			if !pk[d] {
				pk[d] = true
				out = append(out, "go test "+d)
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

func (r *RiskResult) summary() string {
	t := r.Flags
	parts := []string{fmt.Sprintf("%d file%s", t.Files, plural(t.Files))}
	parts = append(parts, fmt.Sprintf("%d hotspot%s", t.Hotspots, plural(t.Hotspots)),
		fmt.Sprintf("%d with past %s", t.WithFixes, map[bool]string{true: "escapes", false: "fixes"}[r.Ledger]),
		fmt.Sprintf("%d likely missing edit%s", t.Missing, plural(t.Missing)),
		fmt.Sprintf("%d importer%s", t.Importers, plural(t.Importers)))
	level := "low"
	switch {
	case t.Hotspots > 0 && (t.WithFixes > 0 || t.Missing > 0), t.Missing >= 3, t.Importers >= 40:
		level = "high"
	case t.Hotspots > 0, t.WithFixes > 0, t.Missing > 0, t.Importers >= 10:
		level = "medium"
	}
	s := "risk " + level + ": " + strings.Join(parts, ", ")
	if len(r.Tests) > 0 {
		s += ". Run: " + strings.Join(firstN(r.Tests, 3), "; ")
		if len(r.Tests) > 3 {
			s += fmt.Sprintf(" (+%d)", len(r.Tests)-3)
		}
	} else {
		s += ". No tests found for it"
	}
	return s
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Render is the text form, cut at about budget tokens (0: DefaultBudget).
func (r *RiskResult) Render(budget int) string {
	if budget <= 0 {
		budget = DefaultBudget
	}
	var b strings.Builder
	b.WriteString(r.Summary + "\n")
	if len(r.Missing) > 0 {
		b.WriteString("\nLikely missing edits (changed together before, not in this change):\n")
		for _, m := range r.Missing {
			fmt.Fprintf(&b, "  %s  with %s (%d commits, %.0f%%)\n", m.Path, strings.Join(firstN(m.With, 2), ", "), m.Support, m.Confidence*100)
		}
	}
	if len(r.Unknown) > 0 {
		fmt.Fprintf(&b, "\nNo history for: %s (new, untracked or not indexed)\n", strings.Join(firstN(r.Unknown, 6), ", "))
	}
	b.WriteString("\nFiles, riskiest first:\n")
	shown := 0
	for _, f := range r.Files {
		line := f.line()
		if (len(b.String())+len(line))/4 > budget && shown > 0 {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown < len(r.Files) {
		var rest []string
		for _, f := range r.Files[shown:] {
			rest = append(rest, f.Path)
		}
		fmt.Fprintf(&b, "  and %d more, quieter: %s (--budget raises the limit, --json has all)\n", len(rest), strings.Join(firstN(rest, 8), ", "))
	}
	if len(r.Tests) > 0 {
		b.WriteString("\nTests to run:\n")
		for _, t := range firstN(r.Tests, 12) {
			b.WriteString("  " + t + "\n")
		}
	}
	return b.String()
}

func (f FileRisk) line() string {
	var tags []string
	if f.Hotspot {
		tags = append(tags, fmt.Sprintf("HOTSPOT p%.0f", f.HotPct*100))
	}
	if f.Fixes > 0 {
		tags = append(tags, fmt.Sprintf("%d fix%s (last %s)", f.Fixes, map[bool]string{true: "", false: "es"}[f.Fixes == 1], ago(f.LastFix)))
	}
	if f.Escapes > 0 {
		tags = append(tags, fmt.Sprintf("%d escape%s (last %s)", f.Escapes, plural(f.Escapes), ago(f.LastEscape)))
	}
	if f.Importers > 0 {
		tags = append(tags, fmt.Sprintf("%d importer%s", f.Importers, plural(f.Importers)))
	}
	if f.Indexed {
		tags = append(tags, fmt.Sprintf("%d commits", f.Commits))
		if f.Owner != "" {
			tags = append(tags, fmt.Sprintf("owner %s %.0f%%", f.Owner, f.OwnerPct*100))
		}
		tags = append(tags, fmt.Sprintf("last %s %q", ago(f.LastChange), clipText(f.LastSubject, 60)))
	} else {
		tags = append(tags, "no history")
	}
	s := "  " + f.Path + "  " + strings.Join(tags, " · ") + "\n"
	if len(f.ImportedBy) > 0 && f.Importers >= 5 {
		s += "      used by " + strings.Join(f.ImportedBy, ", ") + "\n"
	}
	return s
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
