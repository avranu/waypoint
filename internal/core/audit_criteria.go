package core

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// What an audit reads and looks for, said more exactly than one sentence
// (WP-548): what landed on a branch over a period, the parts of the
// repository to read or leave out, the areas to look hardest at, the lowest
// severity worth reporting and how many findings are shown at a time (not a
// limit on what is kept, WP-1421). The server turns
// them into the prompt; the runner finds the period's first commit and
// limits the diff to the paths (WP-ADR40).

// AuditDefaultShown is how many findings are shown at a time when an audit
// sets no number.
const AuditDefaultShown = auditDefaultFindings

const (
	auditDefaultFindings = 15
	auditMaxPaths        = 20
	auditPathMax         = 200
	auditSinceMax        = 366 * 24 * time.Hour
	auditDefaultSince    = "7d"
)

// AuditFocus is an area an audit can be asked to look hardest at.
type AuditFocus struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Text  string `json:"text"` // what the model is told
}

var auditFocusAreas = []AuditFocus{
	{"security", "Security", "Security: injection, access checks missing or wrong, secrets or private data exposed, input trusted that shouldn't be."},
	{"data", "Data integrity", "Data integrity: data lost, corrupted or left half-written; transactions, migrations and backfills that go wrong on real data."},
	{"concurrency", "Concurrency", "Concurrency: races, deadlocks, shared state touched without its lock, work started and never stopped or waited for."},
	{"errors", "Error handling", "Error handling: errors swallowed or reported as success, wrong fallbacks, retries that never stop, cleanup skipped on failure."},
	{"logic", "Logic and edge cases", "Logic: wrong results, off-by-one, and edge cases (empty, zero, huge, duplicate, unicode, time zones) handled wrongly."},
	{"performance", "Performance", "Performance: unbounded work or memory, queries in loops, slow paths in code that runs often."},
	{"compat", "Compatibility", "Compatibility: API, CLI or file formats changed under their users, defaults changed silently, upgrades that break existing data."},
	{"tests", "Tests", "Tests: tests that can't fail, assert the wrong thing, or don't cover the change they claim to."},
}

// AuditPeriod is a period the web offers for recent changes.
type AuditPeriod struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var auditPeriods = []AuditPeriod{{"1d", "Last day"}, {"3d", "Last 3 days"}, {"7d", "Last week"}, {"14d", "Last 2 weeks"}, {"30d", "Last 30 days"}}

// auditCriteria are a request's criteria, checked.
type auditCriteria struct {
	focus, paths, exclude []string
	minSeverity           string
	maxFindings           int
}

// cleanAuditCriteria checks a request's criteria and puts them in their
// stored form.
func cleanAuditCriteria(in AuditInput) (auditCriteria, error) {
	var c auditCriteria
	for _, f := range splitList(in.Focus) {
		k := strings.ToLower(f)
		i := slices.IndexFunc(auditFocusAreas, func(a AuditFocus) bool { return a.Key == k || strings.EqualFold(a.Label, f) })
		if i < 0 {
			keys := make([]string, 0, len(auditFocusAreas))
			for _, a := range auditFocusAreas {
				keys = append(keys, a.Key)
			}
			return c, invalid("%q isn't a focus area: one of %s", f, strings.Join(keys, ", "))
		}
		if !slices.Contains(c.focus, auditFocusAreas[i].Key) {
			c.focus = append(c.focus, auditFocusAreas[i].Key)
		}
	}
	var err error
	if c.paths, err = cleanAuditPaths(in.Paths); err != nil {
		return c, err
	}
	if c.exclude, err = cleanAuditPaths(in.Exclude); err != nil {
		return c, err
	}
	switch sev := strings.ToLower(strings.TrimSpace(in.MinSeverity)); {
	case sev == "" || sev == "low":
	case slices.Contains(auditSeverities, sev):
		c.minSeverity = sev
	default:
		return c, invalid("the lowest severity to report is one of %s", strings.Join(auditSeverities, ", "))
	}
	// Waypoint's check of a round's marks is asked for a finding per mark
	// that was wrong, and per one a report left out (checkClaimsMax,
	// checkLeftOutMax), which can pass what a person may ask for.
	most := auditMaxFindings
	if in.purpose == AuditCheck {
		most = max(most, checkClaimsMax+checkLeftOutMax)
	}
	switch n := in.MaxFindings; {
	case n == 0:
	case n < 1 || n > most:
		return c, invalid("an audit shows between 1 and %d findings at a time", most)
	default:
		c.maxFindings = n
	}
	return c, nil
}

// splitList reads entries that may each be several, split at commas or
// line ends.
func splitList(list []string) []string {
	var out []string
	for _, v := range list {
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

// cleanAuditPaths checks paths and globs relative to the repository's root:
// git takes them as pathspecs, so none may carry pathspec magic (":") or
// climb out.
func cleanAuditPaths(list []string) ([]string, error) {
	var out []string
	for _, p := range splitList(list) {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		switch {
		case p == "" || p == ".":
			continue
		case len(p) > auditPathMax:
			return nil, invalid("a path is at most %d characters", auditPathMax)
		case strings.HasPrefix(p, ":") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "~"):
			return nil, invalid("%q: give paths relative to the repository's root, like internal/core or web/src/**/*.tsx", p)
		case slices.Contains(strings.Split(p, "/"), ".."), strings.ContainsFunc(p, func(r rune) bool { return unicode.IsControl(r) || r == '\\' }):
			return nil, invalid("%q isn't a path inside the repository", p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) > auditMaxPaths {
		return nil, invalid("name at most %d paths", auditMaxPaths)
	}
	return out, nil
}

var sinceRE = lazyre.New(`^(\d{1,4})\s*(h|hours?|d|days?|w|weeks?|mo|months?)(\s+ago)?$`)

// parseAuditSince reads when a period starts: 24h, 7d, 2w, 1mo ("7 days
// ago" too), a date (2026-10-28, midnight UTC) or a time (RFC 3339). It
// gives the start and the period as kept: 7d, 2026-10-28.
func parseAuditSince(v string, now time.Time) (time.Time, string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		v = auditDefaultSince
	}
	var at time.Time
	spec := v
	if m := sinceRE.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[byte]string{'h': "h", 'd': "d", 'w': "w", 'm': "mo"}[m[2][0]]
		span := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "mo": 30 * 24 * time.Hour}[unit]
		if n == 0 {
			return at, "", invalid("a period of 0 has nothing in it: say how long, like 7d")
		}
		if time.Duration(n) > auditSinceMax/span {
			return at, "", invalid("a period goes back at most a year; for more, audit a range or the whole repository")
		}
		at, spec = now.Add(-time.Duration(n)*span), strconv.Itoa(n)+unit
	} else if t, err := time.Parse("2006-01-02", v); err == nil {
		at = t
	} else if t, err := time.Parse(time.RFC3339, strings.ToUpper(v)); err == nil {
		at, spec = t, t.UTC().Format(time.RFC3339)
	} else {
		return at, "", invalid("%q isn't a period or a date: say 24h, 7d, 2w, 1mo or a date like 2026-10-28", v)
	}
	switch {
	case !at.Before(now):
		return at, "", invalid("%s is in the future: nothing has landed since", v)
	case now.Sub(at) > auditSinceMax:
		return at, "", invalid("a period goes back at most a year; for more, audit a range or the whole repository")
	}
	return at, spec, nil
}

// sinceWords says a period: "the last 7 days", "since 28 Oct 2026".
func sinceWords(spec string, at time.Time) string {
	if m := sinceRE.FindStringSubmatch(spec); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]string{"h": "hour", "d": "day", "w": "week", "mo": "month"}[m[2]]
		if n == 1 {
			return "the last " + unit
		}
		return fmt.Sprintf("the last %d %ss", n, unit)
	}
	if at.UTC().Hour() == 0 && at.UTC().Minute() == 0 {
		return "since " + at.UTC().Format("2 Jan 2006")
	}
	return "since " + at.UTC().Format("2 Jan 2006 15:04 UTC")
}

// auditCriteriaPrompt is what the criteria add to the prompt.
func auditCriteriaPrompt(a *CodeAudit) string {
	var b strings.Builder
	if len(a.Focus) > 0 {
		b.WriteString("Look hardest at:\n")
		for _, k := range a.Focus {
			if i := slices.IndexFunc(auditFocusAreas, func(f AuditFocus) bool { return f.Key == k }); i >= 0 {
				b.WriteString("- " + auditFocusAreas[i].Text + "\n")
			}
		}
		b.WriteString("\n")
	}
	quote := func(list []string) string {
		q := make([]string, len(list))
		for i, p := range list {
			q[i] = "`" + p + "`"
		}
		return strings.Join(q, ", ")
	}
	if len(a.Paths) > 0 {
		fmt.Fprintf(&b, "Audit only these parts of the repository: %s (paths and globs from its root). Read the rest only to follow what they call and what calls them, and report findings only in these parts.\n\n", quote(a.Paths))
	}
	if len(a.Exclude) > 0 {
		fmt.Fprintf(&b, "Leave these out: %s. Report nothing in them.\n\n", quote(a.Exclude))
	}
	return b.String()
}

// AuditShown is how many of an audit's findings are shown or delivered at
// a time, and the one place the rule lives (the audit's JSON carries it as
// shown). An ordinary audit shows what a person set (max_findings, which
// names the field as it was stored and as the API has always called it),
// else the default. A check of a session's marks and a review round's
// audit are delivered in full (auditGuard): the session must mark every
// finding, so none is hidden behind a number. It never limits what is asked
// of the model or what is kept: the rest are one step away (WP-1421).
func AuditShown(a *CodeAudit) int {
	if a.Purpose == AuditCheck || a.Purpose == AuditReview {
		return auditGuard(a)
	}
	if a.MaxFindings > 0 {
		return a.MaxFindings
	}
	return auditDefaultFindings
}

// auditGuard is the most findings any audit keeps: a guard against a
// runaway answer, not the limit a person asked for (WP-1320). A check of a
// session's marks may report every mark and what the session left out.
func auditGuard(a *CodeAudit) int {
	if a.Purpose == AuditCheck {
		return checkClaimsMax + checkLeftOutMax
	}
	return findingsGuard
}

// keep drops the findings an audit's criteria rule out (a model may report
// them anyway), and any past the guard (auditGuard), and says how many went.
// max_findings is how many are shown at a time (AuditShown), not how many
// are kept: it keeps them all up to the guard, worst first.
func (f *auditAnswer) keep(a *CodeAudit) int {
	before := len(f.Findings)
	if a.MinSeverity != "" {
		floor := slices.Index(auditSeverities, a.MinSeverity)
		f.Findings = slices.DeleteFunc(f.Findings, func(x CodeAuditFinding) bool { return slices.Index(auditSeverities, x.Severity) > floor })
	}
	f.floorCut = before - len(f.Findings)
	if g := auditGuard(a); len(f.Findings) > g {
		f.rememberCut(f.Findings[g:])
		f.Findings = f.Findings[:g]
	}
	f.guardCut = before - f.floorCut - len(f.Findings)
	return before - len(f.Findings)
}

// auditCutHeading heads the findings the guard cut, which are added to the
// audit's stored answer text; auditCutMax bounds them, in runes, so a
// runaway answer is still not stored without bound (WP-1421).
const (
	auditCutHeading = "Findings past the guard, as they came:"
	auditCutMax     = 400_000
)

// rememberCut keeps the findings the guard cut as JSON, to the bound
// (cutShort: it had to stop early).
func (f *auditAnswer) rememberCut(cut []CodeAuditFinding) {
	b, err := json.Marshal(cut)
	if err != nil {
		return
	}
	r := []rune(string(b))
	if len(r) > auditCutMax {
		r, f.cutShort = r[:auditCutMax], true
	}
	f.cutJSON = string(r)
}

// answerWith is the answer text with the findings the guard cut after it.
func (f *auditAnswer) answerWith(answer string) string {
	if f.cutJSON == "" {
		return answer
	}
	return answer + "\n\n" + auditCutHeading + "\n" + f.cutJSON
}

// cutWhere says where the findings the guard cut are, and how far.
func (f *auditAnswer) cutWhere() string {
	switch {
	case f.cutJSON == "":
		return "they are not kept"
	case f.cutShort:
		return "they are in the audit's answer text, cut short at its size limit"
	}
	return "they are in the audit's answer text"
}

// keepNote is what the summary says when keep left findings out: how many
// the answer held and how many are kept, so no finding goes unseen. raised is how many the answer held.
func (f *auditAnswer) keepNote(a *CodeAudit, raised int) string {
	var b strings.Builder
	if f.floorCut > 0 {
		fmt.Fprintf(&b, " (%d of the answer's %d findings were below the audit's lowest severity, %s, and are left out.)", f.floorCut, raised, a.MinSeverity)
	}
	if f.guardCut > 0 {
		fmt.Fprintf(&b, " (The answer held %d findings; %d are kept, the worst first, at the guard of %d against a runaway answer; %d were cut; %s)",
			raised, len(f.Findings), auditGuard(a), f.guardCut, f.cutWhere())
	}
	return b.String()
}
