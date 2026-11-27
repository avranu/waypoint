package codeindex

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// History signals come from `git log` alone, no model. They are folded in
// by commit: a new HEAD reads only the commits since the last one, then the
// per-file numbers are recomputed from the stored commits (cheap, in memory).
const (
	maxCommits        = 3000
	maxFilesPerCommit = 200
	coMaxFiles        = 30
	coMinSupport      = 3
	coKeep            = 12
	churnHalfLife     = 90.0
	coHalfLife        = 180.0
	fixMaxFiles       = 25
)

func decay(ageDays, halfLife float64) float64 {
	if ageDays < 0 {
		ageDays = 0
	}
	return math.Exp2(-ageDays / halfLife)
}

var (
	fixSubjectRE = lazyre.New(`(?i)\b(fix(?:e[sd]|ing)?|bug(?:fix)?|hotfix|regress(?:ion|ed)?|crash(?:es|ed)?|broken|repair(?:s|ed)?|resolve[sd]?\s+(?:a\s+)?(?:bug|issue|error))\b`)
	// subjects whose type says it isn't a bug fix, however the rest reads
	notFixRE = lazyre.New(`(?i)^(chore|docs?|style|test|tests|ci|build|refactor|perf|release|bump|merge|revert|wip)(\([^)]*\))?!?:|^(merge|revert|bump|release)\b`)
)

// isFixSubject classifies a commit subject: it says "fix"/"bug"/... and is
// not a chore, docs, test, CI or release commit.
func isFixSubject(s string) bool {
	return fixSubjectRE.MatchString(s) && !notFixRE.MatchString(strings.TrimSpace(s))
}

// IsDocPath is true for files whose changes don't count as code fixes.
func IsDocPath(p string) bool { return isDocPath(p) }

// IsTestPath is true for test files, the end-to-end suite among them.
func IsTestPath(p string) bool { return isTestPath(p) }

func isDocPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".txt", ".rst", ".lock", ".sum", ".svg", ".png", ".jpg", ".snap", ".csv", ".jsonl":
		return true
	}
	return strings.HasPrefix(p, "docs/")
}

type histCommit struct {
	sha     string
	ts      int64
	author  string
	subject string
	files   []string
}

// readLog parses git log --numstat output for the given range.
func (ix *Index) readLog(rangeArgs ...string) ([]histCommit, error) {
	args := []string{"log", "--no-merges", "--no-renames", "--name-only", "--format=%x01%H%x02%at%x02%an%x02%s"}
	out, err := gitOut(ix.Root, append(args, rangeArgs...)...)
	if err != nil {
		return nil, err
	}
	var commits []histCommit
	for _, block := range strings.Split(out, "\x01") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		head, rest, _ := strings.Cut(block, "\n")
		f := strings.SplitN(head, "\x02", 4)
		if len(f) < 4 {
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		c := histCommit{sha: f[0], ts: ts, author: f[2], subject: f[3]}
		for _, l := range strings.Split(rest, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				c.files = append(c.files, l)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

func (ix *Index) updateHistory(ctx context.Context, tx *sql.Tx, head string, st *UpdateStats) error {
	t0 := time.Now()
	var prev string
	_ = tx.QueryRow(`SELECT v FROM meta WHERE k = 'history_head'`).Scan(&prev)
	var commits []histCommit
	var err error
	incremental := false
	if prev != "" {
		if _, e := gitOut(ix.Root, "merge-base", "--is-ancestor", prev, head); e == nil {
			incremental = true
		}
	}
	if incremental {
		commits, err = ix.readLog(prev + ".." + head)
	} else {
		for _, q := range []string{`DELETE FROM commit_files`, `DELETE FROM commits`, `DELETE FROM file_signals`, `DELETE FROM cochange`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		commits, err = ix.readLog("-n", fmt.Sprint(maxCommits), head)
	}
	if err != nil {
		return fmt.Errorf("git log: %w", err)
	}
	st.NewCommits = len(commits)
	trace("git log read", t0)
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO commits(sha, ts, author, subject, nfiles) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ins.Close() }()
	insF, err := tx.Prepare(`INSERT INTO commit_files(commit_id, path) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insF.Close() }()
	for _, c := range commits {
		if len(c.files) == 0 || len(c.files) > maxFilesPerCommit {
			continue
		}
		res, err := ins.Exec(c.sha, c.ts, c.author, c.subject, len(c.files))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		for _, f := range c.files {
			if _, err := insF.Exec(id, f); err != nil {
				return err
			}
		}
	}
	trace("commit rows written", t0)
	if _, err := tx.Exec(`DELETE FROM commits WHERE id IN (SELECT id FROM commits ORDER BY ts DESC, id DESC LIMIT -1 OFFSET ?)`, maxCommits); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ix.computeSignals(tx); err != nil {
		return err
	}
	trace("signals computed", t0)
	return setMeta(tx, "history_head", head)
}

type sigAcc struct {
	commits     int
	churn       float64
	fixes       int
	lastFix     int64
	lastTS      int64
	lastAuthor  string
	lastSubject string
	authors     map[string]float64
}

// computeSignals rebuilds file_signals and cochange from the stored commits.
func (ix *Index) computeSignals(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM file_signals`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cochange`); err != nil {
		return err
	}
	existing := map[string]bool{}
	rows, err := tx.Query(`SELECT path FROM files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return err
		}
		existing[p] = true
	}
	_ = rows.Close()

	rows, err = tx.Query(`SELECT c.id, c.ts, c.author, c.subject, f.path FROM commits c JOIN commit_files f ON f.commit_id = c.id ORDER BY c.ts DESC, c.id DESC`)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	acc := map[string]*sigAcc{}
	pathID := map[string]int{}
	var pathName []string
	type pair struct{ a, b int }
	type pairAcc struct {
		weight  float64
		support int
	}
	pairs := map[pair]*pairAcc{}

	var cur histCommit
	var curID int64 = -1
	flush := func() {
		if curID < 0 {
			return
		}
		age := float64(now-cur.ts) / 86400
		n := len(cur.files)
		codeFiles := 0
		for _, f := range cur.files {
			if !isDocPath(f) {
				codeFiles++
			}
		}
		fix := isFixSubject(cur.subject) && n <= fixMaxFiles && codeFiles > 0
		damp := 1 / math.Max(1, math.Log2(float64(n)))
		var live []int
		for _, f := range cur.files {
			if !existing[f] {
				continue
			}
			a := acc[f]
			if a == nil {
				a = &sigAcc{authors: map[string]float64{}, lastTS: cur.ts, lastAuthor: cur.author, lastSubject: cur.subject}
				acc[f] = a
			}
			w := decay(age, churnHalfLife) * damp
			a.commits++
			a.churn += w
			a.authors[cur.author] += w
			if fix && !isDocPath(f) {
				a.fixes++
				if cur.ts > a.lastFix {
					a.lastFix = cur.ts
				}
			}
			id, ok := pathID[f]
			if !ok {
				id = len(pathName)
				pathID[f] = id
				pathName = append(pathName, f)
			}
			live = append(live, id)
		}
		if len(cur.files) >= 2 && len(cur.files) <= coMaxFiles {
			w := decay(age, coHalfLife) / math.Log2(float64(len(cur.files))+1)
			for i := range live {
				for j := i + 1; j < len(live); j++ {
					a, b := live[i], live[j]
					if a > b {
						a, b = b, a
					}
					if a == b {
						continue
					}
					p := pairs[pair{a, b}]
					if p == nil {
						p = &pairAcc{}
						pairs[pair{a, b}] = p
					}
					p.weight += w
					p.support++
				}
			}
		}
	}
	for rows.Next() {
		var id, ts int64
		var author, subject, p string
		if err := rows.Scan(&id, &ts, &author, &subject, &p); err != nil {
			_ = rows.Close()
			return err
		}
		if id != curID {
			flush()
			cur, curID = histCommit{ts: ts, author: author, subject: subject}, id
		}
		cur.files = append(cur.files, p)
	}
	_ = rows.Close()
	flush()

	var churns []float64
	for _, a := range acc {
		if a.commits >= 3 {
			churns = append(churns, a.churn)
		}
	}
	slices.Sort(churns)
	hotCut := math.Inf(1)
	if len(churns) >= 10 {
		hotCut = churns[int(float64(len(churns))*0.9)]
	}
	pct := func(c float64) float64 {
		if len(churns) == 0 {
			return 0
		}
		i, _ := slices.BinarySearch(churns, c)
		return float64(i) / float64(len(churns))
	}
	insS, err := tx.Prepare(`INSERT INTO file_signals(path, commits, churn, hot_pct, hotspot, fixes, last_fix_ts, last_ts, last_author, last_subject, owner, owner_pct) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insS.Close() }()
	for p, a := range acc {
		owner, best, total := "", 0.0, 0.0
		for au, w := range a.authors {
			total += w
			if w > best || w == best && au < owner {
				owner, best = au, w
			}
		}
		share := 0.0
		if total > 0 {
			share = best / total
		}
		hot := a.commits >= 3 && a.churn >= hotCut
		if _, err := insS.Exec(p, a.commits, a.churn, pct(a.churn), b2i(hot), a.fixes, a.lastFix, a.lastTS, a.lastAuthor, a.lastSubject, owner, share); err != nil {
			return err
		}
	}
	type partner struct {
		other   int
		weight  float64
		support int
	}
	per := map[int][]partner{}
	for k, v := range pairs {
		if v.support < coMinSupport {
			continue
		}
		per[k.a] = append(per[k.a], partner{k.b, v.weight, v.support})
		per[k.b] = append(per[k.b], partner{k.a, v.weight, v.support})
	}
	insC, err := tx.Prepare(`INSERT OR IGNORE INTO cochange(a, b, weight, support) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insC.Close() }()
	for a, ps := range per {
		slices.SortFunc(ps, func(x, y partner) int {
			if x.weight != y.weight {
				if x.weight > y.weight {
					return -1
				}
				return 1
			}
			return x.other - y.other
		})
		for _, p := range ps[:min(len(ps), coKeep)] {
			if _, err := insC.Exec(pathName[a], pathName[p.other], p.weight, p.support); err != nil {
				return err
			}
		}
	}
	return nil
}

// trace prints a phase timing to stderr when WP_CODE_TRACE is set.
func trace(what string, since time.Time) {
	if os.Getenv("WP_CODE_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "codeindex: %s at %s\n", what, time.Since(since).Round(time.Millisecond))
	}
}

// TaskCommit is an earlier commit whose subject names a task.
type TaskCommit struct {
	Subject string
	Keys    []string // the task keys it names (WP-12)
	Files   []string // which of the asked-for paths it changed
	TS      int64
}

var taskKeyRE = lazyre.New(`\b[A-Z][A-Z0-9]{1,9}-\d{1,6}\b`)

// TaskCommits lists the newest commits that changed one of paths and whose
// subjects name a task key: the earlier tasks that worked on these files.
func (ix *Index) TaskCommits(ctx context.Context, paths []string, limit int) ([]TaskCommit, error) {
	if len(paths) == 0 || limit <= 0 {
		return nil, nil
	}
	// #nosec G202 -- only ? placeholders are joined in; the paths are bound
	q := `SELECT c.id, c.subject, c.ts, cf.path FROM commits c JOIN commit_files cf ON cf.commit_id = c.id
		WHERE cf.path IN (?` + strings.Repeat(",?", len(paths)-1) + `) ORDER BY c.ts DESC, c.id DESC`
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskCommit
	at := map[int64]int{}
	for rows.Next() {
		var id, ts int64
		var subj, p string
		if err := rows.Scan(&id, &subj, &ts, &p); err != nil {
			return nil, err
		}
		if n, ok := at[id]; ok {
			out[n].Files = append(out[n].Files, p)
			continue
		}
		keys := slices.Compact(taskKeyRE.FindAllString(subj, -1))
		if len(keys) == 0 || len(out) == limit {
			continue
		}
		at[id] = len(out)
		out = append(out, TaskCommit{Subject: subj, Keys: keys, Files: []string{p}, TS: ts})
	}
	return out, rows.Err()
}
