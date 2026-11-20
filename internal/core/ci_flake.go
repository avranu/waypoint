package core

// Flaky checks (WP-392, WP-ADR26). On 2026-11-01 two merge-queue fronts in a row
// failed on tests their pull requests didn't touch (a data race in a test,
// an e2e check of sampled behaviour). Each failure handed out a babysit
// round, sent the pull request to the back of the queue, and the round
// fixed the flake in its own branch, so the next front failed the same way:
// one fix was pushed to three branches before any reached main.
//
// So before a round for failed checks, the babysitter re-runs the head's
// failed jobs once, keeping its place in the queue. Failing again, it gets
// its round. Passing, the failure wasn't the pull request's: Waypoint files
// a bug for the flaky test in the repo's project, with the log of the
// failure kept from before the re-run (or adds the sighting to the open one),
// so it is fixed once, on main.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// flakeRerun is a head's flake re-run: when, and the failed checks' logs
// from before it (a re-run replaces them).
type flakeRerun struct {
	SHA    string       `json:"sha"`
	At     int64        `json:"at"` // ms
	Checks []flakeCheck `json:"checks"`
	// Read: the head's checks were read again once the re-run settled.
	Read bool `json:"read,omitempty"`
	// Filed: the re-run passed and the flake was filed.
	Filed bool `json:"filed,omitempty"`
}

type flakeCheck struct {
	Name    string   `json:"name"`
	URL     string   `json:"url,omitempty"`
	Tests   []string `json:"tests,omitempty"` // the failing tests the log names
	Excerpt string   `json:"excerpt,omitempty"`
}

func parseFlakeRerun(v string) flakeRerun {
	var r flakeRerun
	if v != "" {
		_ = json.Unmarshal([]byte(v), &r)
	}
	return r
}

func (s *Service) saveFlakeRerun(ctx context.Context, b *Babysit, r flakeRerun) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.setBabysit(ctx, b, map[string]any{"flake_rerun": string(raw)}); err != nil {
		return err
	}
	b.flakeRerun = string(raw)
	return nil
}

// flakeExcerptMax bounds the log kept for each check.
const flakeExcerptMax = 3000

// rerunForFlake re-runs a head's failed checks once before their round;
// true when it did, or is waiting for that re-run's results. Not for a
// check that failed in the round before (it fails in a row: that is the
// code), nor when the forge can't re-run jobs.
func (s *Service) rerunForFlake(ctx context.Context, b *Babysit, pr *PullRequest, failed []string) bool {
	if pr.Outside {
		return false
	}
	fr := parseFlakeRerun(b.flakeRerun)
	now := s.now()
	if sameSHA(fr.SHA, pr.HeadSHA) {
		if now.Sub(msTime(fr.At)) < faultRerunSettle {
			// The forge may not have posted the re-run's pending statuses yet.
			s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("re-ran %s on %s to rule out a flaky test; waiting for the results", joinAnd(failed), short(pr.HeadSHA)))
			return true
		}
		if !fr.Read {
			// A failed head isn't polled, and the re-run's webhooks may have
			// been missed: read it once more before the round. New results
			// carry on through the events this raises.
			fr.Read = true
			if err := s.saveFlakeRerun(ctx, b, fr); err != nil {
				// The marker stays as it was; the read below still happens.
				s.log.WarnContext(ctx, "save a flake re-run", "pr", pr.ID, "err", err)
			}
			if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil && s.refreshHead(ctx, f, pr.Repo, pr.HeadSHA) == nil {
				if fresh, err := s.prByID(ctx, s.db.R, pr.ID); err == nil && (fresh.CIState != CIFailed || !sameSHA(fresh.HeadSHA, pr.HeadSHA)) {
					return true
				}
			}
		}
		return false // re-ran once and failed again: its round
	}
	if !slices.ContainsFunc(failed, func(c string) bool { return b.streaks[c] == 0 }) {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	cl, ok := s.Forges.(ForgeCIRetry)
	if err != nil || !ok || f.token == "" {
		return false
	}
	if can, err := cl.CanRetryChecks(ctx, f); err != nil || !can {
		return false
	}
	checks, err := s.flakeChecks(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read the failed checks before a flake re-run", "pr", pr.ID, "err", err)
		return false
	}
	// Save the marker first: a re-run the session can't remember would be
	// issued again, and its results would meet a round started beside it.
	marker := flakeRerun{SHA: pr.HeadSHA, At: now.UnixMilli(), Checks: checks}
	if err := s.saveFlakeRerun(ctx, b, marker); err != nil {
		s.log.WarnContext(ctx, "save a flake re-run", "pr", pr.ID, "err", err)
		return false // nothing re-run yet: its round
	}
	// The re-run replaces the failed jobs' logs, and their test results
	// with them (WP-704).
	s.keepFailedEvidence(ctx, f, pr.Repo, pr.HeadSHA)
	n, err := cl.RetryChecks(ctx, f, pr.Repo, pr.HeadSHA)
	if err != nil || n == 0 {
		if err != nil {
			s.log.WarnContext(ctx, "re-run failed checks to rule out a flake", "pr", pr.ID, "err", err)
		}
		if err := s.saveFlakeRerun(ctx, b, flakeRerun{}); err != nil {
			s.log.WarnContext(ctx, "clear a flake re-run", "pr", pr.ID, "err", err)
		}
		return false // nothing the forge can re-run
	}
	s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("%s failed on %s; Waypoint re-ran %d job%s once to rule out a flaky test before a round", joinAnd(failed), short(pr.HeadSHA), n, plural(n)))
	return true
}

// flakeChecks reads the head's failed checks' kept logs: the failing part,
// and the tests it names. A check that only reports others' failure is left
// out.
func (s *Service) flakeChecks(ctx context.Context, pr *PullRequest) ([]flakeCheck, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.url, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? AND c.state = 'failed' ORDER BY c.name`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []flakeCheck
	for rows.Next() {
		var c flakeCheck
		var log string
		if err := rows.Scan(&c.Name, &c.URL, &log); err != nil {
			return nil, fmt.Errorf("scan a failed check: %w", err)
		}
		if derivedFailure(log) {
			continue
		}
		if log != "" {
			c.Tests = failingTests(cleanLog(log))
			c.Excerpt = strings.ToValidUTF8(excerptKeepLines(failingExcerpt(log, 60).Text, flakeExcerptMax), "")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// failingTestRules find the failing tests a log names: Go, the e2e harness
// ("FAIL 1280 the verifier checked …"), Playwright, Jest, pytest.
var failingTestRules = []*lazyre.Regexp{
	lazyre.New(`^--- FAIL: (\S+)`),
	lazyre.New(`^FAIL \d+ (.+)$`),
	lazyre.New(`^\s+\d+\) \[[^\]]+\] › (.+)$`),
	lazyre.New(`^\s*● (.+)$`),
	lazyre.New(`^FAILED (\S+)`),
}

// Waypoint's e2e/run.sh: "== 62-audits.mjs" starts a suite, which prints
// "FAIL <what>" for each failed check, and "FAILED: 62-audits.mjs" follows
// a suite that failed, whether or not it printed one (a throw); "-- 62-audits.mjs
// took 40s" ends it.
var (
	e2eSuiteRE  = lazyre.New(`^== (\S+\.m?js)$`)
	e2eFailRE   = lazyre.New(`^FAIL (.+)$`)
	e2eFailedRE = lazyre.New(`^FAILED: (\S+\.m?js)$`)
	e2eTookRE   = lazyre.New(`^-- (\S+\.m?js) took `)
	goPkgFailRE = lazyre.New(`^FAIL\s+\S+\s+\d[\d.]*s$`)
)

// failingTests are the tests a cleaned log says failed, in order, once each:
// top-level Go tests only (a subtest's failure fails its parent); in an e2e
// suite, "suite: what failed", or the suite alone when it names nothing.
func failingTests(lines []string) []string {
	var out []string
	add := func(name string) {
		if name = cutLine(strings.TrimSpace(name)); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	suite, named := "", false
	for _, l := range lines {
		if m := e2eSuiteRE.FindStringSubmatch(l); m != nil {
			suite, named = m[1], false
			continue
		}
		if m := e2eFailedRE.FindStringSubmatch(l); m != nil {
			if !named || m[1] != suite {
				add(m[1])
			}
			continue
		}
		if m := e2eTookRE.FindStringSubmatch(l); m != nil && m[1] == suite {
			suite = ""
			continue
		}
		// In a suite, "FAIL <what>" is its check's, whatever <what> starts
		// with; a Go package's "FAIL pkg 0.1s" is not a check.
		if m := e2eFailRE.FindStringSubmatch(l); suite != "" && m != nil && !goPkgFailRE.MatchString(l) {
			add(suite + ": " + m[1])
			named = true
			continue
		}
		for _, re := range failingTestRules {
			if m := re.FindStringSubmatch(l); m != nil {
				add(m[1])
				break
			}
		}
	}
	return out
}

// flakeTitle names the flaky tests, or else the checks: the bug's title,
// and how a sighting finds the open bug it adds to.
func flakeTitle(checks []flakeCheck) string {
	var names []string
	for _, c := range checks {
		for _, t := range c.Tests {
			if !slices.Contains(names, t) {
				names = append(names, t)
			}
		}
	}
	if len(names) == 0 {
		for _, c := range checks {
			names = append(names, checkBaseName(c.Name))
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > 3 {
		names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
	}
	return excerpt("Flaky in CI: "+joinAnd(names), 200)
}

// checkBaseName drops the event a check's name ends with: "ci / lint
// (pull_request)" is "ci / lint".
func checkBaseName(name string) string {
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}

// flakeProven: the head whose failed checks were re-run has passed, so they
// were flaky. Files the bug (or adds to the open one) once, and tells the
// babysit's session not to fix it in the pull request.
func (s *Service) flakeProven(ctx context.Context, b *Babysit, pr *PullRequest) {
	fr := parseFlakeRerun(b.flakeRerun)
	if fr.Filed || fr.SHA == "" || !sameSHA(fr.SHA, pr.HeadSHA) || pr.CIState != CIPassed || !sameSHA(pr.ciSHA, pr.HeadSHA) {
		return
	}
	key, err := s.fileFlake(ctx, b, pr, fr)
	if err != nil {
		s.log.WarnContext(ctx, "file a flaky check", "pr", pr.ID, "err", err)
		return
	}
	if key == "" {
		return // nothing filed (no project, no checks): stays unfiled
	}
	fr.Filed = true
	if err := s.saveFlakeRerun(ctx, b, fr); err != nil {
		s.log.WarnContext(ctx, "save a filed flake", "pr", pr.ID, "err", err)
	}
	var names []string
	for _, c := range fr.Checks {
		names = append(names, c.Name)
	}
	s.babysitTell(ctx, b, pr, "flake@"+pr.HeadSHA, fmt.Sprintf("%s#%d: %s failed on %s and passed when Waypoint re-ran it, so the failure wasn't this pull request's. It is filed as %s, to be fixed on main; don't fix it in this pull request.",
		pr.Repo, pr.Number, joinAnd(names), short(pr.HeadSHA), key))
}

// fileFlake files a flake in the project the repo is attached to: a new bug,
// or a comment on the open one with the same title. It returns the bug's key.
func (s *Service) fileFlake(ctx context.Context, b *Babysit, pr *PullRequest, fr flakeRerun) (string, error) {
	if len(fr.Checks) == 0 {
		return "", nil
	}
	projectID := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if projectID == 0 {
		return "", nil
	}
	owner, err := s.userByID(ctx, s.db.R, b.ownerID)
	if err != nil {
		return "", err
	}
	if owner.Disabled {
		return "", nil
	}
	writer := Actor{User: owner}
	writer.textAgent = "waypoint"
	title := flakeTitle(fr.Checks)
	seen := flakeSighting(pr, fr, s.now())
	var open string
	err = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND i.title = ? AND st.category NOT IN ('done','canceled') ORDER BY i.id DESC LIMIT 1`, projectID, title).Scan(&open)
	if err == nil {
		_, err = s.AddComment(ctx, &writer, open, "Flaky again. "+seen)
		return open, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err // a failed lookup isn't "no open issue": don't file a duplicate
	}
	in := IssueInput{Project: itoa64(projectID), Type: "bug", Agent: "claude", Priority: "high", Labels: []string{"flaky"},
		Title: title,
		Description: "A check failed and then passed when Waypoint re-ran it on the same commit, so the failure isn't the pull request's: the test is flaky. " +
			"While it is, every merge-queue front it hits loses a full run and its place in the queue.\n\n" +
			"Fix it on main, in its own pull request. A fix inside another pull request's branch reaches nobody else until that one merges, " +
			"so the next front fails the same way. Each sighting is added here as a comment.\n\n" + flakeRepro(fr.Checks) + seen}
	made, err := s.CreateIssue(ctx, &writer, in)
	if err != nil {
		return "", err
	}
	return made.Key, nil
}

// flakeRepro is the command that reproduces the flaky Go tests here, and
// tells whether a fix held, with their real names; "" when none is a Go test.
func flakeRepro(checks []flakeCheck) string {
	var names []string
	for _, c := range checks {
		for _, t := range c.Tests {
			top, _, _ := strings.Cut(t, "/")
			if q := regexp.QuoteMeta(top); strings.HasPrefix(top, "Test") && !slices.Contains(names, q) {
				names = append(names, q)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	run := names[0]
	if len(names) > 1 {
		run = "(" + strings.Join(names, "|") + ")"
	}
	// The names are read from CI logs: quoted, so the line runs as one
	// command whatever they hold.
	return fmt.Sprintf("Reproduce it, and check a fix held, with `wp testsum --repeat 30 -- go test -run %s ./...` (add -race if the log shows a data race): "+
		"it counts the failing runs, and 0 in 30 puts the failure rate under about 11%%.\n\n", ShellWord("^"+run+"$"))
}

// ShellWord quotes s as one word for sh, bash and zsh, unless it is plain
// path characters and needs none.
func ShellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./=:,@%+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// flakeSighting is one sighting of a flake: where, when, and the failing
// part of each check's log.
func flakeSighting(pr *PullRequest, fr flakeRerun, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Seen on %s#%d (%s) at %s, head %s; the re-run passed.\n", pr.Repo, pr.Number, pr.URL, now.UTC().Format("2006-01-02 15:04 UTC"), short(fr.SHA))
	for _, c := range fr.Checks {
		fmt.Fprintf(&b, "\n**%s**", c.Name)
		if c.URL != "" {
			fmt.Fprintf(&b, " (%s)", c.URL)
		}
		if len(c.Tests) > 0 {
			fmt.Fprintf(&b, ": %s", joinAnd(c.Tests))
		}
		b.WriteString("\n")
		if c.Excerpt != "" {
			fmt.Fprintf(&b, "\n````text\n%s\n````\n", strings.ReplaceAll(c.Excerpt, "````", "'''"))
		}
	}
	return b.String()
}
