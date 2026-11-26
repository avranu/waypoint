package core

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Round classes.
const (
	// RoundNew: a finding on code no earlier round was told about.
	RoundNew = "new"
	// RoundRepeat: the same concern again after a round fixed it, or the same
	// text under a new comment id, or a check failing again after its fix.
	RoundRepeat = "repeat"
	// RoundCaused: a finding on code the previous round changed, or a check
	// whose log names a file it changed.
	RoundCaused = "caused"
	// RoundCIConflict: a failed check or a conflict with the base.
	RoundCIConflict = "ci_or_conflict"
	// RoundBase: a check that fails on the base branch too.
	RoundBase = "base"
	// RoundNothingPushed: its agent ended without pushing anything. The
	// class of what it was given stays in WorkClass.
	RoundNothingPushed = "nothing_pushed"
)

// RoundClasses lists the classes in the order they are reported.
var RoundClasses = []string{RoundNew, RoundRepeat, RoundCaused, RoundCIConflict, RoundBase, RoundNothingPushed}

// roundClassRank: a round with items of several classes takes the
// weightiest. A round made worse by its predecessor says the most, then a
// fix that didn't hold.
var roundClassRank = map[string]int{RoundCaused: 5, RoundRepeat: 4, RoundNew: 3, RoundCIConflict: 2, RoundBase: 1}

// LedgerEntry is one item a round was given, as the ledger saw it.
type LedgerEntry struct {
	ID     string `json:"id"`   // c12, an audit finding's 12.3, a check's name, or "conflict"
	Kind   string `json:"kind"` // comment, finding, check, conflict
	Path   string `json:"path,omitempty"`
	Symbol string `json:"symbol,omitempty"` // the function or type its line is in
	Gist   string `json:"gist,omitempty"`   // the words that matter, for matching later ones
	Class  string `json:"class"`
	Of     string `json:"of,omitempty"`    // repeat: the earlier comment, finding or check it makes again; a covered comment: the one whose fix covers it
	Round  int    `json:"round,omitempty"` // repeat: the round that fixed it; covered: the round whose fix covers it; caused: the round whose change it is on
	Why    string `json:"why,omitempty"`
	// Covered: a comment written about a head before the fix of round Round,
	// which covers it: the commit of that fix (WP-1853). It is no repeat.
	Covered string `json:"covered,omitempty"`
}

// ledgerItem is a comment or an audit finding, now or earlier.
type ledgerItem struct {
	ID, Kind, Path string
	Line           int
	SHA            string // the head it was made on ("": the pull request's head)
	Body           string
	At             time.Time
	// Earlier ones: how they ended.
	Fixed  bool   // fixed, verified or being fixed
	Dealt  bool   // decided one way or another
	FixSHA string // the commit that fixed it, when recorded
}

// ledgerCheck is a failed check with its log.
type ledgerCheck struct{ Name, Log string }

// ledgerInput is what classifying a round reads.
type ledgerInput struct {
	At       time.Time
	Head     string
	Items    []ledgerItem  // the comments and audit findings it is given
	Checks   []ledgerCheck // the failed checks it is given
	Conflict bool
	Earlier  []ledgerItem   // every comment and audit finding on the pull request
	Rounds   []BabysitRound // the rounds before it
	BaseRed  map[string]bool
	// File reads a file at a commit; "" when it can't be read.
	File func(path, sha string) string
}

// Two gists on the same file and symbol make the same point when they
// share at least gistShared words and that is at least gistOverlap of the
// shorter one's. Set on a week of rounds (WP-438): the pairs a person
// called the same concern shared 25-55% of their words, the others 3-20%.
const (
	gistShared  = 3
	gistOverlap = 0.25
)

// classifyRound fills a round's ledger and class from what it is given.
func classifyRound(in ledgerInput) ([]LedgerEntry, string) {
	l := &ledger{in: in, syms: map[string][]codeindex.Symbol{}}
	l.last = lastPushedRound(in.Rounds)
	var out []LedgerEntry
	for _, it := range in.Items {
		out = append(out, l.item(it))
	}
	for _, c := range in.Checks {
		out = append(out, l.check(c))
	}
	if in.Conflict {
		out = append(out, LedgerEntry{ID: "conflict", Kind: "conflict", Class: RoundCIConflict})
	}
	class := ""
	for _, e := range out {
		if roundClassRank[e.Class] > roundClassRank[class] {
			class = e.Class
		}
	}
	return out, class
}

type ledger struct {
	in      ledgerInput
	last    int
	syms    map[string][]codeindex.Symbol
	touched map[string]map[string]bool
	lines   map[string][]int
}

// item classifies a comment or an audit finding.
func (l *ledger) item(it ledgerItem) LedgerEntry {
	it.Path, it.Line = itemPlace(it)
	e := LedgerEntry{ID: it.ID, Kind: it.Kind, Path: it.Path, Gist: gistOf(it.Body)}
	e.Symbol = l.symbol(it.Path, it.Line, cmp(it.SHA, l.in.Head))
	if r, ok := l.carried(it.ID); ok {
		return withPrint(r, e)
	}
	if n := l.pushedWith(it.ID); n > 0 {
		e.Class, e.Of, e.Round, e.Why = RoundRepeat, it.ID, n, fmt.Sprintf("round %d pushed a fix while it was open, and it is still open", n)
		return e
	}
	if prior, n, ok := l.repeatOf(it, e); ok {
		if fix, ok := l.coveredByFix(it, n); ok {
			// Written about a head before the fix, so nothing "came back":
			// the fix covers it (WP-1853).
			e.Class, e.Of, e.Round, e.Covered = RoundNew, prior.ID, n, fix
			e.Why = fmt.Sprintf("written before round %d's fix, which covers it as it does %s", n, prior.ID)
			return e
		}
		e.Class, e.Of, e.Round = RoundRepeat, prior.ID, n
		e.Why = "makes the point of " + prior.ID + " again"
		if n > 0 {
			e.Why += fmt.Sprintf(", which round %d fixed", n)
		}
		return e
	}
	if n, ok := l.causedBy(it, e.Symbol); ok {
		e.Class, e.Round = RoundCaused, n
		e.Why = fmt.Sprintf("on code round %d changed", n)
		return e
	}
	e.Class = RoundNew
	return e
}

// check classifies a failed check.
func (l *ledger) check(c ledgerCheck) LedgerEntry {
	e := LedgerEntry{ID: c.Name, Kind: "check"}
	if r, ok := l.carried(c.Name); ok {
		return withPrint(r, e)
	}
	if l.in.BaseRed[checkJob(c.Name)] {
		e.Class, e.Why = RoundBase, "it fails on the base branch too"
		return e
	}
	if l.last >= 0 {
		r := l.in.Rounds[l.last]
		if slices.Contains(r.Checks, c.Name) {
			e.Class, e.Of, e.Round, e.Why = RoundRepeat, c.Name, r.N, fmt.Sprintf("it still fails after round %d's fix", r.N)
			return e
		}
		if f := l.logNamesChanged(c.Log); f != "" {
			e.Class, e.Round, e.Path = RoundCaused, r.N, f
			e.Why = fmt.Sprintf("its log names %s, which round %d changed", f, r.N)
			return e
		}
	}
	e.Class = RoundCIConflict
	return e
}

// withPrint is an earlier entry's class on this round's fingerprint.
func withPrint(earlier, e LedgerEntry) LedgerEntry {
	e.Class, e.Of, e.Round, e.Why, e.Covered = earlier.Class, earlier.Of, earlier.Round, earlier.Why, earlier.Covered
	return e
}

// carried: an item an earlier round was given, with no push since. The
// same work again (a stronger model's round on the same head) keeps its
// class.
func (l *ledger) carried(id string) (LedgerEntry, bool) {
	for i := len(l.in.Rounds) - 1; i >= 0; i-- {
		r := l.in.Rounds[i]
		if r.Pushed {
			return LedgerEntry{}, false
		}
		for _, e := range r.Ledger {
			if e.ID == id {
				return e, true
			}
		}
	}
	return LedgerEntry{}, false
}

// pushedWith is the last round given a comment or finding that pushed a fix
// (0: none). It is back, so that fix didn't settle it.
func (l *ledger) pushedWith(id string) int {
	for i := len(l.in.Rounds) - 1; i >= 0; i-- {
		r := l.in.Rounds[i]
		if r.Pushed && (slices.Contains(r.Comments, id) || slices.Contains(r.Findings, id)) {
			return r.N
		}
	}
	return 0
}

// repeatOf finds an earlier comment or finding this one makes again: the
// same text under a new id (a mirror's re-post), or on the same file and
// symbol, saying much the same, after it was fixed. It returns the round
// that fixed it (0 when none did).
func (l *ledger) repeatOf(it ledgerItem, e LedgerEntry) (ledgerItem, int, bool) {
	words := strings.Fields(e.Gist)
	if len(words) == 0 {
		return ledgerItem{}, 0, false
	}
	for _, p := range l.in.Earlier {
		if p.ID == it.ID || (!p.At.IsZero() && !it.At.IsZero() && p.At.After(it.At)) || l.inPacket(p.ID) {
			continue
		}
		pg := gistOf(p.Body)
		if (p.Dealt || l.pushedWith(p.ID) > 0) && len(words) >= 5 && pg == e.Gist {
			return p, l.fixedIn(p), true
		}
		if !p.Fixed && l.pushedWith(p.ID) == 0 {
			continue
		}
		pp, pl := itemPlace(p)
		if pp == "" || pp != e.Path || !gistAlike(words, strings.Fields(pg)) {
			continue
		}
		ps := l.symbol(pp, pl, cmp(p.SHA, l.in.Head))
		if ps != e.Symbol || (ps == "" && absInt(pl-it.Line) > 10) {
			continue
		}
		return p, l.fixedIn(p), true
	}
	for _, r := range l.in.Rounds {
		if !r.Pushed {
			continue
		}
		for _, pe := range r.Ledger {
			if pe.Kind != "finding" || pe.ID == it.ID || l.inPacket(pe.ID) || pe.Path != e.Path || pe.Symbol != e.Symbol || e.Symbol == "" {
				continue
			}
			if gistAlike(words, strings.Fields(pe.Gist)) {
				return ledgerItem{ID: pe.ID, Kind: pe.Kind}, r.N, true
			}
		}
	}
	return ledgerItem{}, 0, false
}

func (l *ledger) inPacket(id string) bool {
	return slices.ContainsFunc(l.in.Items, func(it ledgerItem) bool { return it.ID == id })
}

// fixedIn is the round that fixed an earlier item: the first that pushed
// while it was given it, or the one whose push is its fix commit.
func (l *ledger) fixedIn(p ledgerItem) int {
	for _, r := range l.in.Rounds {
		if r.Pushed && (slices.Contains(r.Comments, p.ID) || slices.Contains(r.Findings, p.ID)) {
			return r.N
		}
	}
	if p.FixSHA != "" {
		for i, r := range l.in.Rounds {
			if r.Pushed && sameSHA(pushedHead(l.in.Rounds, i, ""), p.FixSHA) {
				return r.N
			}
		}
	}
	return 0
}

// coveredByFix says whether a comment was written about a head from before
// round n's fix, and gives that fix's commit. A comment is about the head it
// was made on (the commit the forge gives each review comment): one of the
// heads up to the start of round n's work is before the fix. Where that
// commit isn't one of them (an issue comment has none), it is when it was
// written: before round n began, the fix was not yet pushed. A comment on
// the fix's commit or any later one is the fix not holding.
func (l *ledger) coveredByFix(it ledgerItem, n int) (string, bool) {
	if it.Kind != "comment" {
		return "", false // an audit finding is made of the head it read
	}
	for i, r := range l.in.Rounds {
		if r.N != n || !r.Pushed {
			continue
		}
		fix := pushedHead(l.in.Rounds, i, l.in.Head)
		if fix == "" || sameSHA(fix, r.SHA) || (it.SHA != "" && sameSHA(it.SHA, fix)) {
			return "", false
		}
		// A fix that never touched the comment's file doesn't cover it. A
		// round whose diff wasn't kept can't say, and isn't held to this.
		if place, _ := itemPlace(it); place != "" && r.Diff != "" {
			if _, touched := diffNewLines(r.Diff)[place]; !touched {
				return "", false
			}
		}
		if it.SHA != "" {
			for k := 0; k <= i; k++ {
				if sameSHA(it.SHA, l.in.Rounds[k].SHA) || (k < i && sameSHA(it.SHA, pushedHead(l.in.Rounds, k, ""))) {
					return fix, true
				}
			}
		}
		if !it.At.IsZero() && it.At.Before(r.At) {
			return fix, true
		}
		return "", false
	}
	return "", false
}

// olderThanAFix is the cheap precondition of coveredByFix: the comment was
// written before some pushed round's fix. No forge read.
func (l *ledger) olderThanAFix(it ledgerItem) bool {
	for _, r := range l.in.Rounds {
		if r.Pushed {
			if _, ok := l.coveredByFix(it, r.N); ok {
				return true
			}
		}
	}
	return false
}

// causedBy: the item came after the last round's push and is on what that
// push changed.
func (l *ledger) causedBy(it ledgerItem, sym string) (int, bool) {
	if l.last < 0 || it.Path == "" {
		return 0, false
	}
	r := l.in.Rounds[l.last]
	if (!it.At.IsZero() && it.At.Before(r.At)) || sameSHA(it.SHA, r.SHA) {
		return 0, false
	}
	l.readTouched()
	if sym != "" {
		return r.N, l.touched[it.Path][sym]
	}
	if it.Line <= 0 {
		return 0, false
	}
	for _, n := range l.lines[it.Path] {
		if absInt(n-it.Line) <= 3 {
			return r.N, true
		}
	}
	return 0, false
}

// readTouched maps the last round's diff to the symbols it changed.
func (l *ledger) readTouched() {
	if l.touched != nil {
		return
	}
	l.touched, l.lines = map[string]map[string]bool{}, map[string][]int{}
	r := l.in.Rounds[l.last]
	head := pushedHead(l.in.Rounds, l.last, l.in.Head)
	for file, lines := range diffNewLines(r.Diff) {
		syms := l.fileSymbols(file, head)
		l.touched[file] = map[string]bool{}
		for _, n := range lines {
			if s, ok := codeindex.Innermost(syms, n); ok {
				l.touched[file][s.Qualified()] = true
			} else {
				l.lines[file] = append(l.lines[file], n)
			}
		}
	}
}

// logNameRE: a file a check's log names, with its line ("a/b.go:12").
var logNameRE = lazyre.New(`([\w./-]+\.[A-Za-z]{1,5}):\d+`)

// logNamesChanged is a file the last round changed that a check's log
// names.
func (l *ledger) logNamesChanged(log string) string {
	if log == "" {
		return ""
	}
	changed := diffNewLines(l.in.Rounds[l.last].Diff)
	files := make([]string, 0, len(changed))
	for file := range changed {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, m := range logNameRE.FindAllStringSubmatch(log, -1) {
		named := strings.TrimPrefix(m[1], "./")
		for _, file := range files {
			if file == named || strings.HasSuffix(file, "/"+named) || strings.HasSuffix(named, "/"+file) {
				return file
			}
		}
	}
	return ""
}

// symbol is the function or type a file's line is in, at a commit.
func (l *ledger) symbol(file string, line int, sha string) string {
	if file == "" || line <= 0 {
		return ""
	}
	if s, ok := codeindex.Innermost(l.fileSymbols(file, sha), line); ok {
		return s.Qualified()
	}
	return ""
}

func (l *ledger) fileSymbols(file, sha string) []codeindex.Symbol {
	key := sha + "\x00" + file
	if syms, ok := l.syms[key]; ok {
		return syms
	}
	var syms []codeindex.Symbol
	if l.in.File != nil && sha != "" {
		if src := l.in.File(file, sha); src != "" {
			syms = codeindex.Scan(codeindex.Language(file), []byte(src)).Symbols
		}
	}
	l.syms[key] = syms
	return syms
}

// lastPushedRound is the last round that pushed a fix (not a merge of the
// base, whose diff is the base's), or -1.
func lastPushedRound(rounds []BabysitRound) int {
	for i := len(rounds) - 1; i >= 0; i-- {
		if rounds[i].Pushed {
			if rounds[i].Conflict {
				continue
			}
			return i
		}
	}
	return -1
}

// pushedHead is the head a round's push made: as recorded, or else the head
// the next round was on, or else head.
func pushedHead(rounds []BabysitRound, i int, head string) string {
	if rounds[i].PushSHA != "" {
		return rounds[i].PushSHA
	}
	if i+1 < len(rounds) && !sameSHA(rounds[i+1].SHA, rounds[i].SHA) {
		return rounds[i+1].SHA
	}
	return head
}

// diffNewLines reads a unified diff: by file, the lines of the new side it
// adds, and where it removes lines.
func diffNewLines(diff string) map[string][]int {
	out := map[string][]int{}
	file, n := "", 0
	prev := ""
	for _, line := range strings.Split(diff, "\n") {
		header := strings.HasPrefix(line, "+++ ") && strings.HasPrefix(prev, "--- ")
		prev = line
		switch {
		case header:
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "):
		case strings.HasPrefix(line, "@@"):
			n = hunkStart(line)
		case file == "" || n == 0:
		case strings.HasPrefix(line, "+"):
			out[file] = append(out[file], n)
			n++
		case strings.HasPrefix(line, "-"):
			out[file] = append(out[file], n)
		case strings.HasPrefix(line, `\`):
		default:
			n++
		}
	}
	return out
}

func hunkStart(line string) int {
	m := hunkRE.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return max(n, 1)
}

// placeRE: where a comment on the whole pull request says a finding is, as
// a mirror relays them ("on `a.go` line 3", "`a.go:3`").
var placeRE = lazyre.New("[Oo]n `([^`\\s]+\\.\\w+)` line (\\d+)|`([^`\\s]+\\.\\w+):(\\d+)`")

// itemPlace is the file and line an item is on: its own, or, for a comment
// on the whole pull request, the one place its text names.
func itemPlace(it ledgerItem) (string, int) {
	if it.Path != "" {
		return it.Path, it.Line
	}
	file, line := "", 0
	for _, m := range placeRE.FindAllStringSubmatch(it.Body, -1) {
		f, n := m[1], m[2]
		if f == "" {
			f, n = m[3], m[4]
		}
		if file != "" && f != file && path.Base(f) != path.Base(file) {
			return "", 0
		}
		if file == "" || len(f) > len(file) {
			file = f
			line, _ = strconv.Atoi(n)
		}
	}
	return file, line
}

var (
	// gistDropRE: what a comment says that isn't its point: code, collapsed
	// sections, footers, markup.
	gistDropRE = lazyre.New("(?s)```.*?```|<details>.*?</details>|<sub>.*?</sub>|<[^>]+>|https?://\\S+")
	// gistTitleRE: a bot's bold title ("**Authenticate the relay before
	// parsing the header.**"), where its point starts.
	gistTitleRE = lazyre.New(`\*\*([^*]*\s[^*]*\s[^*]*\s[^*]+)\*\*`)
	gistWordRE  = lazyre.New(`[a-z][a-z0-9_]{2,}`)
)

// gistStop are words every finding uses.
var gistStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for that this with from are was were but not can will when then than which what who its it's has have had into onto
		also only just should would could must may might does did doing been being there their they them these those here where while
		because before after about over under more most less some any all each every other such same one two use used uses using
		line lines file code comment review reviewed commented github coderabbitai bot major minor trivial critical nitpick
		high medium low quick win functional correctness maintainability quality security privacy performance supported static
		analysis script executed repository length output automated checked person verify acting suggestion potential issue
		committable fix fixes fixed prompt agents agent outside diff range comments posted actionable caution`) {
		gistStop[w] = true
	}
}

// gistOf is what a comment or finding says, as its first words that matter:
// past a relay's header, its severity and its bot's chrome, from its title
// on when it has one.
func gistOf(body string) string {
	s := stripMarkers(body)
	if m := relayedAuthorRE.FindStringIndex(s); m != nil && m[0] == 0 {
		if i := strings.Index(s[m[1]:], ":"); i >= 0 {
			s = s[m[1]+i+1:]
		}
	}
	s = gistDropRE.ReplaceAllString(s, " ")
	if m := gistTitleRE.FindStringSubmatchIndex(s); m != nil {
		s = s[m[2]:]
	}
	s = leadBadgeRE.ReplaceAllString(strings.TrimSpace(s), "")
	var words []string
	seen := map[string]bool{}
	for _, w := range gistWordRE.FindAllString(strings.ToLower(s), -1) {
		if gistStop[w] || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, w)
		if len(words) == 30 {
			break
		}
	}
	return strings.Join(words, " ")
}

// gistAlike: two gists (lists of distinct words) share enough to make the
// same point.
func gistAlike(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	in := map[string]bool{}
	for _, w := range a {
		in[w] = true
	}
	both := 0
	for _, w := range b {
		if in[w] {
			both++
		}
	}
	return both >= gistShared && float64(both) >= gistOverlap*float64(min(len(a), len(b)))
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// classifyLiveRound fills a round's ledger from its work packet, reading
// files from the forge at the commits the comments were made on.
func (s *Service) classifyLiveRound(ctx context.Context, b *Babysit, pr *PullRequest, t *PRTodo, round *BabysitRound) {
	in := ledgerInput{At: round.At, Head: pr.HeadSHA, Conflict: t.Conflict, Rounds: b.Rounds, File: s.forgeFileReader(ctx, pr)}
	for _, c := range t.Comments {
		in.Items = append(in.Items, commentItem(c.PRReviewComment))
	}
	for _, f := range t.Findings {
		in.Items = append(in.Items, s.findingItem(ctx, f.Audit, f.CodeAuditFinding))
	}
	for _, c := range t.Checks {
		in.Checks = append(in.Checks, ledgerCheck{Name: c.Name, Log: cmp(c.Log, c.Text)})
	}
	if list, err := s.reviewComments(ctx, s.db.R, pr.ID); err == nil {
		for _, rc := range list {
			in.Earlier = append(in.Earlier, commentItem(rc))
		}
	}
	in.Earlier = append(in.Earlier, s.earlierFindings(ctx, pr.ID, t.Findings)...)
	if len(t.Checks) > 0 {
		_, red, err := s.baseFailing(ctx, pr)
		if err != nil {
			s.log.WarnContext(ctx, "read what fails on the base for a round's ledger", "pr", pr.ID, "err", err)
		}
		in.BaseRed = red
	}
	round.Ledger, round.Class = classifyRound(in)
	round.WorkClass = round.Class
}

// coverMemo is what closeCovered has looked at for one pull request: on a
// head, with so many comments dealt with, the open comments it classified.
type coverMemo struct {
	head  string
	dealt int
	refs  map[string]bool
}

// coverCloses says whether closeCovered may close the comment it that the
// ledger entry e finds covered. What it repeats (one of originals) has to be
// fixed: a rejected point has no fix to cover it. A comment not written before
// the covering round began, which that round never saw, also has to say the
// very same thing: a likeness of a few words may be another point. What isn't
// closed stays for a round to judge.
func coverCloses(it ledgerItem, e LedgerEntry, originals []ledgerItem, rounds []BabysitRound) bool {
	i := slices.IndexFunc(originals, func(p ledgerItem) bool { return p.ID == e.Of })
	if i < 0 || !originals[i].Fixed {
		return false
	}
	if slices.ContainsFunc(rounds, func(r BabysitRound) bool { return r.N == e.Round && !it.At.IsZero() && it.At.Before(r.At) }) {
		return true
	}
	return e.Gist != "" && gistOf(originals[i].Body) == e.Gist
}

// closeCovered closes the open bot comments a round's fix already covers:
// ones written about a head from before that fix, which the ledger would not
// take for the fix not holding (coveredByFix, WP-1853), where coverCloses
// agrees. They are marked fixed by the fix's commit, as the duplicate that
// round was given is, so no round is spent on them and none parks on "came
// back". A person's comment is never closed this way. It changes nothing
// before a round has pushed.
func (s *Service) closeCovered(ctx context.Context, b *Babysit, pr *PullRequest, comments []*PRReviewComment) {
	if !slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return r.Pushed }) {
		return
	}
	// Classifying reads files from the forge for every item, and the sweep
	// runs every minute: each open comment is classified once per head and
	// count of comments dealt with (a closed comment can make another one a
	// duplicate), and only when some earlier comment or finding of the round
	// ledgers could be the one it repeats.
	dealt := 0
	for _, rc := range comments {
		if rc.Dealt {
			dealt++
		}
	}
	memo := coverMemo{head: strings.ToLower(pr.HeadSHA), dealt: dealt, refs: map[string]bool{}}
	if v, ok := s.coverLooked.Load(pr.ID); ok {
		if m := v.(coverMemo); m.head == memo.head && m.dealt == dealt {
			memo = m
		}
	}
	in := ledgerInput{At: s.now(), Head: pr.HeadSHA, Rounds: b.Rounds}
	for _, rc := range comments {
		in.Earlier = append(in.Earlier, commentItem(rc))
	}
	var open []*PRReviewComment
	probe := &ledger{in: in}
	for _, rc := range comments {
		if rc.Dealt || rc.Gone || (rc.Verdict != VerdictNew && rc.Verdict != VerdictSeen) || !rc.botComment() || memo.refs[rc.Ref] {
			continue
		}
		it := commentItem(rc)
		if !probe.olderThanAFix(it) || !probe.mayRepeat(it) {
			continue
		}
		open = append(open, rc)
		in.Items = append(in.Items, it)
	}
	if len(open) == 0 {
		s.coverLooked.Store(pr.ID, memo)
		return
	}
	read, failed := s.forgeFileReaderTracked(ctx, pr)
	if read == nil {
		return // no forge to read: nothing is closed on a guess
	}
	in.File = read
	entries, _ := classifyRound(in)
	if failed() {
		// A file read as empty puts two functions' comments in one place:
		// nothing is closed or remembered, and a later sweep looks again.
		return
	}
	for _, rc := range open {
		memo.refs[rc.Ref] = true
	}
	var originals []ledgerItem
	for _, e := range entries {
		if e.Covered == "" {
			continue
		}
		if originals == nil {
			originals = append(s.earlierFindings(ctx, pr.ID, nil), in.Earlier...)
		}
		for _, rc := range open {
			if rc.Ref != e.ID || !coverCloses(commentItem(rc), e, originals, b.Rounds) {
				continue
			}
			note := fmt.Sprintf("Written about %s, before round %d's fix (%s), which covers it as it does %s. (Closed by Waypoint; reopen it if the fix leaves this standing.)",
				cmp(short(rc.SHA), "an earlier head"), e.Round, short(e.Covered), e.Of)
			if err := s.markComment(ctx, pr, rc, VerdictFixed, note, e.Covered, rcBy{kind: "babysitter", name: "waypoint"}, b.Round); err != nil {
				s.log.WarnContext(ctx, "close a comment a fix covers", "pr", pr.ID, "comment", rc.Ref, "err", err)
				delete(memo.refs, rc.Ref)
				continue
			}
			rc.Dealt = true
			memo.dealt++
			s.log.InfoContext(ctx, "closed a comment a fix covers", "repo", pr.Repo, "number", pr.Number, "comment", rc.Ref, "of", e.Of, "fix", short(e.Covered))
		}
	}
	s.coverLooked.Store(pr.ID, memo)
}

// mayRepeat is the cheap local test before classifying a comment: some other
// comment that was dealt with or fixed (or given to a round that pushed)
// shares its file or its exact gist, or a round's ledger holds a finding on
// its file. When none does, the comment can repeat nothing.
func (l *ledger) mayRepeat(it ledgerItem) bool {
	place, _ := itemPlace(it)
	gist := gistOf(it.Body)
	for _, p := range l.in.Earlier {
		if p.ID == it.ID || (!p.Dealt && !p.Fixed && l.pushedWith(p.ID) <= 0) {
			continue
		}
		if pp, _ := itemPlace(p); (pp != "" && pp == place) || (gist != "" && gistOf(p.Body) == gist) {
			return true
		}
	}
	for _, r := range l.in.Rounds {
		if !r.Pushed {
			continue
		}
		for _, pe := range r.Ledger {
			if pe.Kind == "finding" && pe.Path != "" && pe.Path == place {
				return true
			}
		}
	}
	return false
}

// commentItem is a review comment as the ledger reads it.
func commentItem(rc *PRReviewComment) ledgerItem {
	return ledgerItem{ID: rc.Ref, Kind: "comment", Path: rc.Path, Line: rc.Line, SHA: rc.SHA, Body: rc.Body, At: rc.CreatedAt,
		Fixed: fixedVerdict(rc.Verdict) || rc.Verdict == VerdictFixing, Dealt: rc.Dealt, FixSHA: rc.FixSHA}
}

// findingItem is an audit finding as the ledger reads it, on the head its
// audit read.
func (s *Service) findingItem(ctx context.Context, audit int64, f CodeAuditFinding) ledgerItem {
	var sha string
	var at int64
	_ = s.db.R.QueryRowContext(ctx, `SELECT to_sha, created_at FROM code_audits WHERE id = ?`, audit).Scan(&sha, &at)
	it := ledgerItem{ID: f.ID, Kind: "finding", Path: f.Path, Line: f.Line, SHA: sha, Body: f.Title + ". " + f.Problem,
		Fixed: f.State == "fixed", Dealt: !findingToDo(f)}
	if at > 0 {
		it.At = time.UnixMilli(at)
	}
	return it
}

// earlierFindings are the findings of the pull request's audits before the
// newest one.
func (s *Service) earlierFindings(ctx context.Context, prID int64, now []TodoFinding) []ledgerItem {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM code_audits WHERE pr_id = ? AND status = ? ORDER BY id`, prID, AuditDone)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var out []ledgerItem
	for _, id := range ids {
		if len(now) > 0 && id >= now[0].Audit {
			continue
		}
		a := &CodeAudit{ID: id}
		if s.loadFindings(ctx, a) != nil {
			continue
		}
		for _, f := range a.Findings {
			out = append(out, s.findingItem(ctx, id, f))
		}
	}
	return out
}

// forgeFileReader reads a pull request's files at a commit from its forge,
// each once.
func (s *Service) forgeFileReader(ctx context.Context, pr *PullRequest) func(string, string) string {
	read, _ := s.forgeFileReaderTracked(ctx, pr)
	return read
}

// forgeFileReaderTracked is forgeFileReader, also giving a func that says
// whether any read failed (the file then reads as empty).
func (s *Service) forgeFileReaderTracked(ctx context.Context, pr *PullRequest) (func(string, string) string, func() bool) {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	prc, ok := s.Forges.(ForgePRs)
	if err != nil || !ok {
		return nil, nil
	}
	cache := map[string]string{}
	bad := false
	return func(file, sha string) string {
		key := sha + "\x00" + file
		if body, ok := cache[key]; ok {
			return body
		}
		body, err := prc.FileAt(ctx, f, pr.Repo, file, sha)
		if err != nil {
			body = ""
			bad = bad || !errors.Is(err, ErrNotFound) // a file that isn't there is an answer
		}
		cache[key] = body
		return body
	}, func() bool { return bad }
}
