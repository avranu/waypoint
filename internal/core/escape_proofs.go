package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	proofsDueMax   = 10
	proofNoteMax   = 2000
	proofTestsMax  = 240
	proofTestMax   = 300
	proofConfirmed = 1.0
)

// Proof outcomes, as internal/codeindex names them.
var proofOutcomes = []string{"proven", "passes", "no_build", "head_fails", "no_test", "unsupported", "none"}

// What the proving tests did on the blamed change.
var introChecks = []string{"", "confirmed", "passes_at_intro", "fails_before", "no_build"}

// ProofDue is a traced fix waiting for its proof.
type ProofDue struct {
	EscapeDue
	Fix   string `json:"fix"`             // the commit the trace placed the fix at
	Intro string `json:"intro,omitempty"` // the change the ledger blames
	// Dirs are the other checkouts on the machine for the fix's project. The
	// places don't say which repo they hold, so the runner picks the one whose
	// origin is the fix's repo.
	Dirs []string `json:"dirs,omitempty"`
}

// EscapeProofsDue lists traced fixes for a runner to prove, on a machine
// (each with the checkout to use) or for one repo. Newest first: a proof
// takes minutes of test time, so a round proves only a few.
func (s *Service) EscapeProofsDue(ctx context.Context, a *Actor, machine, repo string, limit int) ([]*ProofDue, error) {
	out := []*ProofDue{}
	if err := a.RequireHuman("proving fixes on a machine"); err != nil {
		return nil, err
	}
	if a.User == nil || a.User.ID == 0 || (machine == "" && repo == "") {
		return out, nil
	}
	if limit <= 0 || limit > proofsDueMax {
		limit = proofsDueMax
	}
	places, err := s.escapeDuePlaces(ctx, a, machine)
	if err != nil {
		return nil, err
	}
	seen := map[int64]*ProofDue{}
	for _, pl := range places {
		if s.requireProjectRole(ctx, s.db.R, a, pl.project, RoleEditor) != nil {
			continue
		}
		due, err := s.proofsDueIn(ctx, pl.project, repo, limit)
		if err != nil {
			return nil, err
		}
		for _, d := range due {
			if first := seen[d.PR]; first != nil {
				if pl.dir != "" && pl.dir != first.Dir && !slices.Contains(first.Dirs, pl.dir) {
					first.Dirs = append(first.Dirs, pl.dir)
				}
				continue
			}
			if len(out) >= limit {
				continue
			}
			ok, err := s.mayTraceFix(ctx, a, d.PR)
			if err != nil {
				return nil, err
			}
			if ok {
				d.Dir = pl.dir
				seen[d.PR] = d
				out = append(out, d)
			}
		}
	}
	return out, nil
}

func (s *Service) proofsDueIn(ctx context.Context, project int64, repo string, limit int) ([]*ProofDue, error) {
	args := []any{project}
	repoSQL := ""
	if repo != "" {
		repoSQL = ` AND p.repo = ? COLLATE NOCASE`
		args = append(args, repo)
	}
	args = append(args, limit)
	// #nosec G202 -- repoSQL is fixed text; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, repo, number, merge_sha, head_sha, base_ref, commits, fix_sha, intro FROM (
		SELECT p.id, p.repo, p.number, p.merge_sha, p.head_sha, p.base_ref, COALESCE(p.commits, 0) AS commits, ft.fix_sha,
			(SELECT e.intro_merge FROM escapes e WHERE e.pr_id = ft.pr_id ORDER BY e.confidence DESC, e.id LIMIT 1) AS intro,
			ROW_NUMBER() OVER (PARTITION BY lower(p.repo) ORDER BY ft.traced_at DESC, p.id DESC) AS rn
		FROM fix_traces ft JOIN pull_requests p ON p.id = ft.pr_id
		WHERE ft.fix_sha != '' AND NOT EXISTS (SELECT 1 FROM fix_proofs fp WHERE fp.pr_id = ft.pr_id)
			AND EXISTS (SELECT 1 FROM escapes e WHERE e.pr_id = ft.pr_id AND e.project_id = ?)`+repoSQL+`)
		ORDER BY rn, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ProofDue
	for rows.Next() {
		d := &ProofDue{}
		if err := rows.Scan(&d.PR, &d.Repo, &d.Number, &d.Merge, &d.Head, &d.Base, &d.Commits, &d.Fix, &d.Intro); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FixProofInput is what a runner uploads for one fix.
type FixProofInput struct {
	PR         int64    `json:"pr_id"`
	Machine    string   `json:"machine"`
	Commit     string   `json:"commit"`
	Base       string   `json:"base"`
	Outcome    string   `json:"outcome"`
	Tests      []string `json:"tests"`
	Failed     []string `json:"failed"`
	Intro      string   `json:"intro"`
	IntroCheck string   `json:"intro_check"`
	Note       string   `json:"note"`
	Seconds    int      `json:"seconds"`
}

func (in *FixProofInput) validate() error {
	if !slices.Contains(proofOutcomes, in.Outcome) {
		return invalid("outcome must be one of %s", strings.Join(proofOutcomes, ", "))
	}
	if !slices.Contains(introChecks, in.IntroCheck) {
		return invalid("intro_check must be empty or one of %s", strings.Join(introChecks[1:], ", "))
	}
	if len(in.Note) > proofNoteMax || len(in.Machine) > 200 || len(in.Commit) > 64 || len(in.Base) > 64 || len(in.Intro) > 64 ||
		len(in.Tests) > proofTestsMax || len(in.Failed) > proofTestsMax {
		return invalid("proof is too large")
	}
	if in.Seconds < 0 {
		return invalid("seconds can't be negative")
	}
	for _, t := range append(slices.Clone(in.Tests), in.Failed...) {
		if t == "" || len(t) > proofTestMax {
			return invalid("tests has an invalid entry")
		}
	}
	for _, t := range in.Failed {
		if !slices.Contains(in.Tests, t) {
			return invalid("failed names a test not in tests")
		}
	}
	if in.Outcome == "proven" && len(in.Failed) == 0 {
		return invalid("a proven fix names the tests that failed without it")
	}
	if in.IntroCheck != "" && in.Intro == "" {
		return invalid("intro_check needs intro")
	}
	return nil
}

// RecordFixProof stores a runner's proof and redoes the fix's escapes: a
// check on the blamed change can change which cause they carry.
func (s *Service) RecordFixProof(ctx context.Context, a *Actor, in FixProofInput) error {
	if err := a.RequireHuman("recording a fix's proof"); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return err
	}
	allowed, err := s.mayTraceFix(ctx, a, in.PR)
	if err != nil {
		return err
	}
	if !allowed {
		return notFound("pull request")
	}
	tests, err := json.Marshal(nonNil(in.Tests))
	if err != nil {
		return err
	}
	failed, err := json.Marshal(nonNil(in.Failed))
	if err != nil {
		return err
	}
	return s.write(ctx, a, func(t *tx) error {
		var traced string
		err := t.QueryRowContext(ctx, `SELECT fix_sha FROM fix_traces WHERE pr_id = ?`, in.PR).Scan(&traced)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && traced == "") {
			return invalid("the fix has no trace to prove")
		} else if err != nil {
			return err
		}
		if in.Commit != traced {
			return invalid("the fix is traced to %s now, not %s", shortSHA(traced), shortSHA(in.Commit))
		}
		if in.IntroCheck != "" {
			var n int
			if err := t.QueryRowContext(ctx, `SELECT COUNT(*) FROM fix_trace_introducers WHERE pr_id = ? AND merge_sha = ?`, in.PR, in.Intro).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return invalid("the trace doesn't blame %s", shortSHA(in.Intro))
			}
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO fix_proofs(pr_id, outcome, fix_sha, base_sha, tests, failed, intro_merge, intro_check, note, machine, seconds, proved_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (pr_id) DO UPDATE SET outcome = excluded.outcome, fix_sha = excluded.fix_sha,
			base_sha = excluded.base_sha, tests = excluded.tests, failed = excluded.failed, intro_merge = excluded.intro_merge,
			intro_check = excluded.intro_check, note = excluded.note, machine = excluded.machine, seconds = excluded.seconds, proved_at = excluded.proved_at`,
			in.PR, in.Outcome, in.Commit, in.Base, string(tests), string(failed), in.Intro, in.IntroCheck, in.Note, in.Machine, in.Seconds, s.nowMS()); err != nil {
			return err
		}
		if err := applyProof(ctx, t, in.PR); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM escapes WHERE pr_id = ?`, in.PR); err != nil {
			return err
		}
		return fillEscapes(ctx, t, s.nowMS(), in.PR)
	})
}

// applyProof sets the confidence of the introducer a proof checked: a test
// that fails on the change and passes on the commit before it confirms the
// blame; one that passes on it, or fails before it too, refutes it. Called
// after a proof and after a re-trace, which recomputes confidences from
// line shares.
func applyProof(ctx context.Context, t *tx, pr int64) error {
	var merge, check string
	err := t.QueryRowContext(ctx, `SELECT intro_merge, intro_check FROM fix_proofs WHERE pr_id = ?`, pr).Scan(&merge, &check)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var conf float64
	switch check {
	case "confirmed":
		conf = proofConfirmed
	case "passes_at_intro", "fails_before":
		conf = 0
	default:
		return nil
	}
	_, err = t.ExecContext(ctx, `UPDATE fix_trace_introducers SET confidence = ? WHERE pr_id = ? AND merge_sha = ?`, conf, pr, merge)
	return err
}

// EscapeProof is the proof outcomes over the ledger's fixes: how many, and
// how many of them a later bug was confidently blamed on.
type EscapeProof struct {
	Outcome string `json:"outcome"` // "" is a fix not proved yet
	Fixes   int    `json:"fixes"`
	Refixed int    `json:"refixed"`
}

// escapeProofs groups the ledger's fixes by proof outcome. A fix counts
// once however many bugs it closed; refixed means a confident escape in the
// same rows blames it.
func escapeProofs(rows []*Escape) (out []EscapeProof, pending int) {
	blamed := map[int64]bool{}
	for _, e := range rows {
		if e.introPR != 0 && e.confident() {
			blamed[e.introPR] = true
		}
	}
	by := map[string]*EscapeProof{}
	seen := map[int64]bool{}
	for _, e := range rows {
		if seen[e.pr] {
			continue
		}
		seen[e.pr] = true
		g := by[e.Proof]
		if g == nil {
			g = &EscapeProof{Outcome: e.Proof}
			by[e.Proof] = g
		}
		g.Fixes++
		if blamed[e.pr] {
			g.Refixed++
		}
		if e.Proof == "" {
			pending++
		}
	}
	out = make([]EscapeProof, 0, len(by))
	for _, g := range by {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fixes != out[j].Fixes {
			return out[i].Fixes > out[j].Fixes
		}
		return out[i].Outcome < out[j].Outcome
	})
	return out, pending
}

var (
	noBuildUndefRE      = lazyre.New(`\bundefined:\s+(?:[A-Za-z_][A-Za-z0-9_]*\.)*([A-Za-z_][A-Za-z0-9_]*)`)
	noBuildFieldUndefRE = lazyre.New(`\.([A-Za-z_][A-Za-z0-9_]*)\s+undefined\b`)
	noBuildUnknownFldRE = lazyre.New(`\bunknown field\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
)

// ClassifyNoBuildNote classifies why a fix's tests failed to compile on base
// (N25, WP-911): distinguishing tests tied to brand-new unexported helpers from
// signature changes or new exported APIs.
func ClassifyNoBuildNote(note string) string {
	if strings.TrimSpace(note) == "" {
		return "other"
	}
	var sawUnexported, sawExported bool
	checkSym := func(sym string) {
		if sym == "" {
			return
		}
		r := rune(sym[0])
		if unicode.IsLower(r) || r == '_' {
			sawUnexported = true
		} else if unicode.IsUpper(r) {
			sawExported = true
		}
	}
	for _, m := range noBuildUndefRE.FindAllStringSubmatch(note, -1) {
		checkSym(m[1])
	}
	for _, m := range noBuildFieldUndefRE.FindAllStringSubmatch(note, -1) {
		checkSym(m[1])
	}
	for _, m := range noBuildUnknownFldRE.FindAllStringSubmatch(note, -1) {
		checkSym(m[1])
	}
	if sawUnexported {
		return "unexported_symbol"
	}
	if strings.Contains(note, "too many arguments in call to") ||
		strings.Contains(note, "not enough arguments in call to") ||
		strings.Contains(note, "in argument to") ||
		strings.Contains(note, "assignment mismatch:") ||
		strings.Contains(note, "multiple-value ") ||
		strings.Contains(note, "in return statement") {
		return "signature_change"
	}
	if sawExported {
		return "exported_api"
	}
	return "other"
}

func noBuildCauseLabel(cause string) string {
	switch cause {
	case "unexported_symbol":
		return "tests new unexported symbol"
	case "signature_change":
		return "tests changed call signature"
	case "exported_api":
		return "tests new exported symbol"
	default:
		return ""
	}
}

// escapeNoBuildCauses breaks down no_build fixes by compiler failure cause.
func escapeNoBuildCauses(rows []*Escape) []EscapeProof {
	blamed := map[int64]bool{}
	for _, e := range rows {
		if e.introPR != 0 && e.confident() {
			blamed[e.introPR] = true
		}
	}
	by := map[string]*EscapeProof{}
	seen := map[int64]bool{}
	for _, e := range rows {
		if e.Proof != "no_build" || seen[e.pr] {
			continue
		}
		seen[e.pr] = true
		cause := ClassifyNoBuildNote(e.ProofNote)
		g := by[cause]
		if g == nil {
			g = &EscapeProof{Outcome: cause}
			by[cause] = g
		}
		g.Fixes++
		if blamed[e.pr] {
			g.Refixed++
		}
	}
	if len(by) == 0 {
		return nil
	}
	out := make([]EscapeProof, 0, len(by))
	for _, g := range by {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fixes != out[j].Fixes {
			return out[i].Fixes > out[j].Fixes
		}
		return out[i].Outcome < out[j].Outcome
	})
	return out
}

// fixProofLine is the one line a pull request's detail shows about its
// proof, "" when it has none.
func fixProofLine(ctx context.Context, q queryer, pr int64) (string, error) {
	var outcome, failed, intro, check, note string
	err := q.QueryRowContext(ctx, `SELECT outcome, failed, intro_merge, intro_check, note FROM fix_proofs WHERE pr_id = ?`, pr).Scan(&outcome, &failed, &intro, &check, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	var names []string
	_ = json.Unmarshal([]byte(failed), &names)
	return ProofText(outcome, names, intro, check, note), nil
}

// ProofText says what a fix's proof found, in one line.
func ProofText(outcome string, failed []string, intro, check, note string) string {
	var line string
	switch outcome {
	case "proven":
		line = "fix proven: " + listNames(failed) + " fails without the fix"
	case "passes":
		line = "not proven: its tests pass without the fix too"
	case "no_build":
		line = "not proven: its tests don't build without the fix"
		if lbl := noBuildCauseLabel(ClassifyNoBuildNote(note)); lbl != "" {
			line += " (" + lbl + ")"
		}
	case "head_fails":
		line = "not proven: its tests fail on the fix itself"
	case "no_test":
		line = "no failing test: the fix adds or changes none"
	case "unsupported":
		line = "not proven: its tests aren't Go"
	default:
		line = "nothing to prove"
	}
	short := intro
	if len(short) > 10 {
		short = short[:10]
	}
	switch check {
	case "confirmed":
		line += fmt.Sprintf("; the blame on %s is confirmed (fails on it, passes before it)", short)
	case "passes_at_intro":
		line += fmt.Sprintf("; the blamed %s is refuted (the test passes on it)", short)
	case "fails_before":
		line += fmt.Sprintf("; the blamed %s is refuted (the test fails before it too)", short)
	case "no_build":
		line += fmt.Sprintf("; the test doesn't build on the blamed %s", short)
	}
	if note != "" && outcome == "none" {
		if i := strings.IndexByte(note, '\n'); i >= 0 {
			note = note[:i]
		}
		line += " (" + note + ")"
	}
	return line
}

func listNames(v []string) string {
	if len(v) <= 3 {
		return strings.Join(v, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(v[:3], ", "), len(v)-3)
}
