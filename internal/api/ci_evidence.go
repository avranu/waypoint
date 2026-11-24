package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// What the backend test jobs' evidence says (WP-704): tests that flake, are
// slow or fail on the default branch; a commit's coverage; the #nosec and
// //nolint ledger.

// ciTests: ?repo=OWNER/NAME, ?view=flaky|slow|failing, ?days=N,
// ?package=P, ?limit=N.
func (s *Server) ciTests(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	t, err := s.svc.CITests(r.Context(), actorFrom(r), core.CITestsInput{
		Repo: q.Get("repo"), View: q.Get("view"), Days: queryInt(r, "days", 0), Package: q.Get("package"), Limit: queryInt(r, "limit", 0),
	})
	if err != nil || !wantsText(r) {
		return t, err
	}
	return textResponse(ciTestsText(t)), nil
}

// ciCoverage: ?repo=OWNER/NAME, ?sha=SHA, ?path=DIR|FILE.
func (s *Server) ciCoverage(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	c, err := s.svc.CICoverage(r.Context(), actorFrom(r), core.CICoverageInput{Repo: q.Get("repo"), SHA: q.Get("sha"), Path: q.Get("path")})
	if err != nil || !wantsText(r) {
		return c, err
	}
	return textResponse(ciCoverageText(c)), nil
}

// ciSuppressions: ?repo=OWNER/NAME, ?days=N, ?all=1.
func (s *Server) ciSuppressions(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	sp, err := s.svc.CISuppressions(r.Context(), actorFrom(r), core.CISuppressionsInput{Repo: q.Get("repo"), Days: queryInt(r, "days", 0), All: queryBool(r, "all")})
	if err != nil || !wantsText(r) {
		return sp, err
	}
	return textResponse(ciSuppressionsText(sp)), nil
}

func coverPct(covered, stmts int) string {
	if stmts == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(covered)/float64(stmts))
}

func msDur(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return core.ShortDuration(time.Duration(ms) * time.Millisecond)
}

// testName is a test as go test -run would name it, with its repo when the
// answer spans more than one.
func testName(t *core.CITests, repo, pkg, name string) string {
	s := pkg + " " + name
	if t.Repo == "" {
		s = repo + ": " + s
	}
	return s
}

// flakyRepro is the command that reproduces a flaky Go test here and tells
// whether a fix for it held, with the top flake's real names.
func flakyRepro(f core.CIFlakyTest) string {
	top, _, _ := strings.Cut(f.Name, "/")
	if !strings.HasPrefix(top, "Test") || f.Package == "" {
		return ""
	}
	pkg := f.Package
	if !strings.HasPrefix(pkg, ".") && !strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".") {
		pkg = "./" + pkg
	}
	// Both names are CI's, read from a log: quoted, so the line runs as one
	// command whatever they hold.
	return fmt.Sprintf("  Reproduce one, or check a fix held, from the repo's root: wp testsum --repeat 20 -- go test -run %s %s\n",
		core.ShellWord("^"+regexp.QuoteMeta(top)+"$"), core.ShellWord(pkg))
}

// ciTestsText is wp ci tests.
func ciTestsText(t *core.CITests) string {
	var o strings.Builder
	scope := "all repos"
	if t.Repo != "" {
		scope = t.Repo
	}
	fmt.Fprintf(&o, "Tests, %s, last %d days: %d results of %d tests from %d jobs", scope, t.Days, t.Runs, t.Tests, t.Jobs)
	if t.Since != nil {
		fmt.Fprintf(&o, " (kept since %s)", t.Since.UTC().Format("2006-01-02 15:04"))
	}
	o.WriteString("\n")
	if t.Runs == 0 {
		o.WriteString("\nNo test results kept yet: they are read from the backend test jobs' logs as the jobs finish.\n")
		return o.String()
	}
	all := t.View == ""
	if all || t.View == "flaky" {
		o.WriteString("\nFlaky (passed and failed on one commit; score = such commits / commits run on):\n")
		if len(t.Flaky) == 0 {
			o.WriteString("  none\n")
		}
		for _, f := range t.Flaky {
			fmt.Fprintf(&o, "  %4.0f%%  %d of %d commits, %d fails in %d runs, last %s  %s\n", 100*f.Score, f.Flaky, f.Commits, f.Fails, f.Runs,
				f.LastFlake.UTC().Format("Jan 02 15:04"), testName(t, f.Repo, f.Package, f.Name))
		}
		if len(t.Flaky) > 0 {
			o.WriteString(flakyRepro(t.Flaky[0]))
		}
	}
	if all || t.View == "failing" {
		o.WriteString("\nFailing on the default branch (its newest result there):\n")
		if len(t.Failing) == 0 {
			o.WriteString("  none\n")
		}
		for _, f := range t.Failing {
			last := "no pass in the window"
			if f.LastPass != nil {
				last = "last pass " + f.LastPass.UTC().Format("Jan 02 15:04")
			}
			fmt.Fprintf(&o, "  %d fails since %s (%s), newest at %s", f.Fails, f.Since.UTC().Format("Jan 02 15:04"), last, shortSHA(f.LastSHA))
			if f.OtherRefs > 0 {
				fmt.Fprintf(&o, ", %d on other refs", f.OtherRefs)
			}
			fmt.Fprintf(&o, "  %s\n", testName(t, f.Repo, f.Package, f.Name))
		}
	}
	if all || t.View == "slow" {
		o.WriteString("\nSlowest tests (passing, full runs with -race; avg / max):\n")
		if len(t.Slow) == 0 {
			o.WriteString("  none in full runs\n")
		}
		for _, x := range t.Slow {
			fmt.Fprintf(&o, "  %8s / %-8s %4d runs  %s\n", msDur(x.AvgMs), msDur(x.MaxMs), x.Runs, testName(t, x.Repo, x.Package, x.Name))
		}
		if len(t.Packages) > 0 {
			o.WriteString("\nSlowest packages (their tests' averages added up; the order shards balance by):\n")
			for _, p := range t.Packages {
				name := p.Package
				if t.Repo == "" {
					name = p.Repo + ": " + name
				}
				fmt.Fprintf(&o, "  %8s  %4d tests  %s\n", msDur(p.Ms), p.Tests, name)
			}
		}
	}
	if all {
		o.WriteString("\n--flaky, --slow or --failing for the full lists; --json for everything.\n")
	}
	return o.String()
}

// ciCoverageText is wp ci coverage.
func ciCoverageText(c *core.CICoverage) string {
	var o strings.Builder
	what := c.Repo
	if c.Path != "" {
		what += " " + c.Path
	}
	fmt.Fprintf(&o, "Coverage of %s at %s", what, shortSHA(c.SHA))
	if c.Ref != "" {
		fmt.Fprintf(&o, " (%s)", c.Ref)
	}
	fmt.Fprintf(&o, ", %s: %s of %d statements\n", c.At.UTC().Format("2006-01-02 15:04"), coverPct(c.Covered, c.Stmts), c.Stmts)
	if !c.Complete {
		fmt.Fprintf(&o, "Partial: only shards %s were read, so the tests of the others are missing.\n", strings.Join(c.Shards, ", "))
	}
	if c.FilesGone {
		o.WriteString("The per-file detail is older than a month and was dropped.\n")
	}
	if len(c.Rows) > 0 {
		o.WriteString("\n")
		w := 0
		for _, r := range c.Rows {
			w = max(w, len(r.Path))
		}
		for _, r := range c.Rows {
			fmt.Fprintf(&o, "  %-*s  %7s  %6d stmts  %6d not run\n", w, r.Path, coverPct(r.Covered, r.Stmts), r.Stmts, r.Stmts-r.Covered)
		}
	} else if c.Path != "" && !c.FilesGone {
		o.WriteString("\nNo package or file by that path in this commit's coverage.\n")
	}
	if len(c.Trend) > 1 {
		o.WriteString("\nDefault branch, newest first:\n")
		for _, p := range c.Trend {
			mark := ""
			if !p.Complete {
				mark = "  (partial)"
			}
			fmt.Fprintf(&o, "  %s  %s  %7s%s\n", shortSHA(p.SHA), p.At.UTC().Format("Jan 02 15:04"), coverPct(p.Covered, p.Stmts), mark)
		}
	}
	return o.String()
}

// ciSuppressionsText is wp ci suppressions.
func ciSuppressionsText(s *core.CISuppressions) string {
	var o strings.Builder
	fmt.Fprintf(&o, "Suppressions in %s", s.Repo)
	if s.At != nil {
		fmt.Fprintf(&o, " at %s (%s)", shortSHA(s.SHA), s.At.UTC().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(&o, ": %d", s.Count)
	if s.NoReason > 0 {
		fmt.Fprintf(&o, ", %d with no reason given", s.NoReason)
	}
	o.WriteString("\n")
	if len(s.ByRule) > 0 {
		o.WriteString("\n")
		for _, r := range s.ByRule {
			rule := r.Rule
			if rule == "" {
				rule = "(every rule)"
			}
			fmt.Fprintf(&o, "  %4d  %s %s\n", r.Count, r.Kind, rule)
		}
	}
	if len(s.Scans) > 1 {
		var counts []string
		for _, sc := range s.Scans {
			counts = append(counts, fmt.Sprint(sc.Count))
		}
		fmt.Fprintf(&o, "\nLast %d scans, newest first: %s\n", len(s.Scans), strings.Join(counts, " "))
	}
	line := func(c core.CISuppressionComment) {
		rules := strings.Join(c.Rules, ",")
		if rules == "" {
			rules = "*"
		}
		reason := c.Reason
		if reason == "" {
			reason = "(no reason)"
		}
		fmt.Fprintf(&o, "  %s:%d  %s %s  %s\n", c.Path, c.Line, c.Kind, rules, reason)
	}
	fmt.Fprintf(&o, "\nAdded in the last %d days: %d\n", s.Days, len(s.Added))
	for _, c := range s.Added {
		line(c)
	}
	fmt.Fprintf(&o, "Removed in the last %d days: %d\n", s.Days, len(s.Removed))
	for _, c := range s.Removed {
		line(c)
	}
	if s.All != nil {
		fmt.Fprintf(&o, "\nAll %d:\n", len(s.All))
		for _, c := range s.All {
			line(c)
		}
	}
	return o.String()
}
