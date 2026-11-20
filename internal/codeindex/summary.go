package codeindex

import (
	"path"
	"sort"
	"strings"
	"time"
)

// Summary is what a checkout of the default branch tells the server (ADR3
// point 5): per-file history signals, the strongest co-change pairs and
// totals, small enough to upload every day. The server keeps the newest one
// per repo and uses it where it has no checkout: the review bot's context,
// the story-point estimator.
type Summary struct {
	Commit    string         `json:"commit"`     // HEAD the index was built at
	IndexedAt time.Time      `json:"indexed_at"` // when the summary was made
	Files     int            `json:"files"`      // indexed files
	Symbols   int            `json:"symbols"`
	Commits   int            `json:"commits"` // commits of history read
	Languages map[string]int `json:"languages"`
	Churn     []FileSummary  `json:"churn"`    // files with history, most churn first (capped)
	CoChange  []PairSummary  `json:"cochange"` // strongest pairs, a < b
}

// FileSummary is one file's numbers.
type FileSummary struct {
	Path       string    `json:"path"`
	Lang       string    `json:"lang"`
	Size       int       `json:"size"`
	Lines      int       `json:"lines"`
	Symbols    int       `json:"symbols"`
	Importers  int       `json:"importers"`
	Commits    int       `json:"commits"`
	Churn      float64   `json:"churn"`
	HotPct     float64   `json:"hot_pct"`
	Hotspot    bool      `json:"hotspot"`
	Fixes      int       `json:"fixes"`
	LastFix    time.Time `json:"last_fix,omitzero"`
	LastChange time.Time `json:"last_change,omitzero"`
	LastAuthor string    `json:"last_author,omitempty"`
	Owner      string    `json:"owner,omitempty"`
	OwnerPct   float64   `json:"owner_pct,omitempty"`
}

// PairSummary is two files that change together.
type PairSummary struct {
	A       string  `json:"a"`
	B       string  `json:"b"`
	Weight  float64 `json:"weight"`
	Support int     `json:"support"`
}

const (
	summaryMaxFiles = 6000
	summaryMaxPairs = 20000
)

// HeadCommit is the commit the index was last built at.
func (ix *Index) HeadCommit() string { return ix.meta("head") }

// Summary builds the upload from the index as it is now (call Update first).
func (ix *Index) Summary() (*Summary, error) {
	st := ix.Status()
	s := &Summary{Commit: ix.meta("head"), IndexedAt: time.Now().UTC().Truncate(time.Second), Files: st.Files, Symbols: st.Symbols,
		Commits: st.Commits, Languages: st.Languages, Churn: []FileSummary{}, CoChange: []PairSummary{}}
	imp := map[string]int{}
	rows, err := ix.db.Query(`SELECT target, count(DISTINCT file_id) FROM imports WHERE target <> '' GROUP BY target`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t string
		var n int
		if rows.Scan(&t, &n) == nil {
			imp[t] = n
		}
	}
	_ = rows.Close()
	rows, err = ix.db.Query(`SELECT s.path, f.lang, f.size, f.lines, (SELECT count(*) FROM symbols y WHERE y.file_id = f.id),
		s.commits, s.churn, s.hot_pct, s.hotspot, s.fixes, s.last_fix_ts, s.last_ts, s.last_author, s.owner, s.owner_pct
		FROM file_signals s JOIN files f ON f.path = s.path ORDER BY s.churn DESC, s.path LIMIT ?`, summaryMaxFiles)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f FileSummary
		var hot int
		var fix, last int64
		if err := rows.Scan(&f.Path, &f.Lang, &f.Size, &f.Lines, &f.Symbols, &f.Commits, &f.Churn, &f.HotPct, &hot, &f.Fixes, &fix, &last, &f.LastAuthor, &f.Owner, &f.OwnerPct); err != nil {
			return nil, err
		}
		f.Hotspot = hot != 0
		if fix > 0 {
			f.LastFix = time.Unix(fix, 0).UTC()
		}
		f.LastChange = time.Unix(last, 0).UTC()
		key := f.Path
		if strings.HasSuffix(f.Path, ".go") {
			if key = path.Dir(f.Path); key == "." {
				key = ""
			}
		}
		f.Importers = imp[key]
		s.Churn = append(s.Churn, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Each file keeps its own strongest partners, so a pair can be stored
	// from one side only: collect both directions, once per pair.
	pr, err := ix.db.Query(`SELECT a, b, weight, support FROM cochange`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pr.Close() }()
	seen := map[[2]string]bool{}
	for pr.Next() {
		var p PairSummary
		if err := pr.Scan(&p.A, &p.B, &p.Weight, &p.Support); err != nil {
			return nil, err
		}
		if p.A > p.B {
			p.A, p.B = p.B, p.A
		}
		if k := [2]string{p.A, p.B}; !seen[k] {
			seen[k] = true
			s.CoChange = append(s.CoChange, p)
		}
	}
	sort.SliceStable(s.CoChange, func(i, j int) bool { return s.CoChange[i].Weight > s.CoChange[j].Weight })
	if len(s.CoChange) > summaryMaxPairs {
		s.CoChange = s.CoChange[:summaryMaxPairs]
	}
	return s, pr.Err()
}
