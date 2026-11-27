package codeindex

import (
	"database/sql"
	"errors"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signals are the history numbers of one file (git only, no model).
type Signals struct {
	Path        string    `json:"path"`
	Commits     int       `json:"commits"`  // commits in the history window that touched it
	Churn       float64   `json:"churn"`    // decayed churn, half-life 90 days
	HotPct      float64   `json:"hot_pct"`  // churn percentile among files changed at least 3 times
	Hotspot     bool      `json:"hotspot"`  // top tenth by churn
	Fixes       int       `json:"fixes"`    // bug-fix commits that touched it
	LastFix     time.Time `json:"last_fix"` // zero when none
	LastChange  time.Time `json:"last_change"`
	LastAuthor  string    `json:"last_author"`
	LastSubject string    `json:"last_subject"`
	Owner       string    `json:"owner"`     // author with the largest decayed share
	OwnerPct    float64   `json:"owner_pct"` // 0..1
}

// Signals returns a file's history signals, nil when git has none for it
// (untracked, or older than the window).
func (ix *Index) Signals(p string) (*Signals, error) {
	var s Signals
	var fix, last int64
	var hot int
	err := ix.db.QueryRow(`SELECT path, commits, churn, hot_pct, hotspot, fixes, last_fix_ts, last_ts, last_author, last_subject, owner, owner_pct FROM file_signals WHERE path = ?`, p).
		Scan(&s.Path, &s.Commits, &s.Churn, &s.HotPct, &hot, &s.Fixes, &fix, &last, &s.LastAuthor, &s.LastSubject, &s.Owner, &s.OwnerPct)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Hotspot = hot != 0
	if fix > 0 {
		s.LastFix = time.Unix(fix, 0)
	}
	s.LastChange = time.Unix(last, 0)
	return &s, nil
}

// CoChange is a file that tends to change in the same commits.
type CoChange struct {
	Path       string  `json:"path"`
	Support    int     `json:"support"`    // commits where both changed
	Weight     float64 `json:"weight"`     // decayed, damped by commit size
	Confidence float64 `json:"confidence"` // support / the file's own commits
}

// CoChanged lists a file's strongest co-change partners (at least 3 shared
// commits), best first.
func (ix *Index) CoChanged(p string, n int) ([]CoChange, error) {
	rows, err := ix.db.Query(`SELECT c.b, c.support, c.weight, CAST(c.support AS REAL) / max(1, s.commits) FROM cochange c LEFT JOIN file_signals s ON s.path = c.a WHERE c.a = ? ORDER BY c.weight DESC LIMIT ?`, p, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoChange
	for rows.Next() {
		var c CoChange
		if err := rows.Scan(&c.Path, &c.Support, &c.Weight, &c.Confidence); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Imports lists what a file imports that resolves to a file (a directory
// for Go) in the checkout, and separately the specs that don't (external).
func (ix *Index) Imports(p string) (internal, external []string, err error) {
	rows, err := ix.db.Query(`SELECT i.spec, i.target FROM imports i JOIN files f ON f.id = i.file_id WHERE f.path = ? ORDER BY i.rowid`, p)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var spec, target string
		if err := rows.Scan(&spec, &target); err != nil {
			return nil, nil, err
		}
		if target != "" {
			internal = append(internal, target)
		} else {
			external = append(external, spec)
		}
	}
	return internal, external, rows.Err()
}

// Importers lists the files that import p (for a Go file: that import its
// package), most recently listed first by path.
func (ix *Index) Importers(p string) ([]string, error) {
	target := p
	if strings.HasSuffix(p, ".go") {
		target = path.Dir(p)
		if target == "." {
			target = ""
		}
	}
	rows, err := ix.db.Query(`SELECT DISTINCT f.path FROM imports i JOIN files f ON f.id = i.file_id WHERE i.target = ? AND f.path <> ? ORDER BY f.path`, target, p)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return !isTestPath(out[i]) && isTestPath(out[j]) })
	return out, rows.Err()
}

// Status describes the index for `wp code index --status`.
type Status struct {
	DBPath        string         `json:"db"`
	DBBytes       int64          `json:"db_bytes"`
	Schema        int            `json:"schema"`
	Head          string         `json:"head"`
	HistoryHead   string         `json:"history_head"`
	Files         int            `json:"files"`
	Symbols       int            `json:"symbols"`
	Imports       int            `json:"imports"`
	ImportsLinked int            `json:"imports_linked"`
	Commits       int            `json:"commits"`
	Skipped       int            `json:"skipped"`
	Updated       time.Time      `json:"updated"`
	LastUpdateMS  int64          `json:"last_update_ms"`
	Languages     map[string]int `json:"languages"`
}

// Status counts what the index holds, as of the last update.
func (ix *Index) Status() Status {
	st := Status{DBPath: ix.DBPath, Schema: SchemaVersion, Head: ix.meta("head"), HistoryHead: ix.meta("history_head"), Languages: map[string]int{}}
	if fi, err := os.Stat(ix.DBPath); err == nil {
		st.DBBytes = fi.Size()
		if w, err := os.Stat(ix.DBPath + "-wal"); err == nil {
			st.DBBytes += w.Size()
		}
	}
	count := func(q string, dst *int) { _ = ix.db.QueryRow(q).Scan(dst) }
	count(`SELECT count(*) FROM files`, &st.Files)
	count(`SELECT count(*) FROM symbols`, &st.Symbols)
	count(`SELECT count(*) FROM imports`, &st.Imports)
	count(`SELECT count(*) FROM imports WHERE target <> ''`, &st.ImportsLinked)
	count(`SELECT count(*) FROM commits`, &st.Commits)
	count(`SELECT count(*) FROM skipped`, &st.Skipped)
	if sec, err := strconv.ParseInt(ix.meta("updated_at"), 10, 64); err == nil {
		st.Updated = time.Unix(sec, 0)
	}
	st.LastUpdateMS, _ = strconv.ParseInt(ix.meta("last_update_ms"), 10, 64)
	if rows, err := ix.db.Query(`SELECT lang, count(*) FROM files GROUP BY lang`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var l string
			var n int
			if rows.Scan(&l, &n) == nil {
				st.Languages[l] = n
			}
		}
	}
	return st
}
