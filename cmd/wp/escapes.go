package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

const traceEvery = 15 * time.Minute

var escapesCmd = &command{name: "escapes", args: "[-p KEY] [--repo R] [--days N]", group: "Code and CI",
	summary: "bugs fixed: what introduced them, what found them, how long they lived",
	about: `The escape ledger: for each merged fix linked to a bug task, the change
blamed for it, how it was found and how long it lived. Prints escapes per
week, median lifetime, the split by stage and detector, and how many fixes
a test proves (it fails without the fix). Runners fill it; wp escapes trace
and wp escapes prove run a round in this checkout.`,
	examples: []string{"wp report quality escapes", "wp report quality escapes --days 30 -p WP", "wp escapes show WP-580"},
	run:      cmdEscapes,
	subs: []*command{
		{name: "ls", args: "[-p KEY] [--repo R] [--days N] [--limit N]", summary: "the ledger's rows, newest first", run: cmdEscapesLs},
		{name: "show", args: "KEY", summary: "one bug task's fix and every candidate cause", run: cmdEscapesShow},
		{name: "trace", args: "[--limit N]", summary: "trace this checkout's untraced fixes now", run: cmdEscapesTrace},
		{name: "prove", args: "[--limit N]", summary: "run traced fixes' tests without the fix: does one fail?", run: cmdEscapesProve},
	},
}

func (c *cli) escapeFlags(args []string) (url.Values, []string, error) {
	fs := c.flags()
	project := fs.String("project", "", "only project `KEY`")
	short(fs, "p", "project")
	repo := fs.String("repo", "", "only repo `OWNER/NAME`")
	days := fs.Int("days", 0, "look back `N` days (default 90)")
	limit := fs.Int("limit", 0, "at most `N` rows")
	pos, err := c.parse(fs, args)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	if *project != "" {
		q.Set("project", *project)
	}
	if *repo != "" {
		q.Set("repo", *repo)
	}
	if *days > 0 {
		q.Set("days", strconv.Itoa(*days))
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	return q, pos, nil
}

func cmdEscapes(c *cli, args []string) error {
	q, pos, err := c.escapeFlags(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", pos[0])
	}
	return c.ciGet("/api/v1/escapes/stats", q)
}

func cmdEscapesLs(c *cli, args []string) error {
	q, pos, err := c.escapeFlags(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", pos[0])
	}
	return c.ciGet("/api/v1/escapes", q)
}

func cmdEscapesShow(c *cli, args []string) error {
	q, pos, err := c.escapeFlags(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which task")
	}
	q.Set("key", strings.ToUpper(pos[0]))
	return c.ciGet("/api/v1/escapes", q)
}

// fixTraceUpload is the body of POST /api/v1/escapes/traces.
type fixTraceUpload struct {
	PR         int64  `json:"pr_id"`
	Machine    string `json:"machine"`
	TriedMerge string `json:"tried_merge"`
	*codeindex.FixTrace
}

type escapeDue struct {
	PR      int64  `json:"pr_id"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Merge   string `json:"merge"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Commits int    `json:"commits"`
	FiledAt int64  `json:"filed_at"`
	Dir     string `json:"dir"`
}

func fetchOrigin(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// #nosec G204 -- dir is a checkout this runner is configured to use; the arguments are fixed
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--quiet", "--no-write-fetch-head", "origin").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// traceDue traces each fix in its checkout and uploads the result, returning
// how many it sent. A checkout it can't fetch or that holds another repo is
// passed over, and said: a trace from a stale tree would be wrong for good.
func traceDue(ctx context.Context, c *cli, due []escapeDue, machine string, dirOf func(escapeDue) (string, error)) (int, error) {
	fetched := map[string]error{}
	noCheckout := map[string]bool{}
	sent := 0
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, d := range due {
		dir, err := dirOf(d)
		if err != nil {
			note(err)
			continue
		}
		if !strings.EqualFold(originRepo(dir), d.Repo) {
			noCheckout[d.Repo] = true
			continue
		}
		ferr, done := fetched[dir]
		if !done {
			ferr = fetchOrigin(ctx, dir)
			fetched[dir] = ferr
		}
		if ferr != nil {
			note(ferr)
			continue
		}
		tr, err := codeindex.TraceFix(dir, codeindex.FixInput{Merge: d.Merge, Head: d.Head, Base: d.Base, Commits: d.Commits, FiledAt: time.UnixMilli(d.FiledAt)})
		if err != nil {
			note(fmt.Errorf("%s#%d: %w", d.Repo, d.Number, err))
			continue
		}
		if err := c.callInto(ctx, "POST", "/api/v1/escapes/traces", fixTraceUpload{PR: d.PR, Machine: machine, TriedMerge: d.Merge, FixTrace: tr}, nil); err != nil {
			note(fmt.Errorf("%s#%d: %w", d.Repo, d.Number, err))
			continue
		}
		sent++
	}
	if len(noCheckout) > 0 {
		note(fmt.Errorf("fixes waiting in %s, which this machine's checkout for the project doesn't hold", strings.Join(slices.Sorted(maps.Keys(noCheckout)), ", ")))
	}
	return sent, firstErr
}

func cmdEscapesTrace(c *cli, args []string) error {
	fs := c.flags()
	limit := fs.Int("limit", 0, "trace at most `N` fixes")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	repo := originRepo(".")
	if repo == "" {
		return errors.New("this isn't a checkout with an origin")
	}
	q := url.Values{"repo": {repo}}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	ctx := context.Background()
	var due []escapeDue
	if err := c.callInto(ctx, "GET", "/api/v1/escapes/due?"+q.Encode(), nil, &due); err != nil {
		return err
	}
	if len(due) == 0 {
		c.println("No fixes waiting for a trace in " + repo + ".")
		return nil
	}
	root, err := gitIn(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)
	sent, err := traceDue(ctx, c, due, "", func(escapeDue) (string, error) { return root, nil })
	c.println(fmt.Sprintf("Traced %d of %d fixes in %s.", sent, len(due), repo))
	return err
}

// traceFixes is the runner's round: trace what is due on this machine. It
// does nothing when the server has nothing for this machine, and fetches
// only when it does.
func (p *runnerProc) traceFixes(ctx context.Context) {
	lastErr := ""
	for wait := 3 * time.Minute; sleepCtx(ctx, wait); wait = traceEvery {
		var due []escapeDue
		err := p.c.callInto(ctx, "GET", "/api/v1/escapes/due?machine="+url.QueryEscape(p.opts.Name), nil, &due)
		if err != nil && isNotFound(err) {
			continue
		}
		if err == nil && len(due) > 0 {
			var sent int
			sent, err = traceDue(ctx, p.c, due, p.opts.Name, func(d escapeDue) (string, error) { return resolveDir(d.Dir, p.allowed) })
			if sent > 0 {
				p.logf("traced %d fixes for the escape ledger", sent)
			}
		}
		if err != nil && err.Error() != lastErr {
			p.logf("escape ledger: %v", err)
		}
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
}

const (
	proveEvery = 15 * time.Minute
	proveRound = 2
	// proveAsk is how many due fixes a round asks for, so that it can pass
	// over held ones and still fill the round.
	proveAsk = 10
	// proveHold is how long a fix whose proof failed (it can't be placed,
	// git fails, the server refuses the result) waits: newest first, it
	// would otherwise take every round and starve the older fixes.
	proveHold = 12 * time.Hour
)

type proofDue struct {
	escapeDue
	Fix   string   `json:"fix"`
	Intro string   `json:"intro"`
	Dirs  []string `json:"dirs"` // other checkouts the server knows of; Dir may hold another repo
}

// fixProofUpload is the body of POST /api/v1/escapes/proofs.
type fixProofUpload struct {
	PR      int64  `json:"pr_id"`
	Machine string `json:"machine"`
	*codeindex.FixProof
}

// proveDue proves each fix in its checkout and uploads the result, returning
// how many it sent and the fixes whose own proof failed (not a fetch or the
// test slot, which aren't the fix's).
func proveDue(ctx context.Context, c *cli, due []proofDue, machine string, dirOf func(escapeDue) (string, error), logf func(string, ...any)) (int, []int64, error) {
	fetched := map[string]error{}
	sent := 0
	var failed []int64
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	fail := func(pr int64, err error) {
		note(err)
		if ctx.Err() == nil {
			failed = append(failed, pr)
		}
	}
	for _, d := range due {
		dir, err := "", error(nil)
		for _, cand := range append([]string{d.Dir}, d.Dirs...) {
			ed := d.escapeDue
			ed.Dir = cand
			dir, err = dirOf(ed)
			if err == nil && strings.EqualFold(originRepo(dir), d.Repo) {
				break
			}
			if err == nil {
				dir = ""
			}
		}
		if dir == "" {
			if err == nil {
				err = fmt.Errorf("%s#%d: no checkout of %s on this machine", d.Repo, d.Number, d.Repo)
			}
			fail(d.PR, err)
			continue
		}
		ferr, done := fetched[dir]
		if !done {
			ferr = fetchOrigin(ctx, dir)
			fetched[dir] = ferr
		}
		if ferr != nil {
			note(ferr)
			continue
		}
		slot, err := acquireSlot(ctx, "tests", defaultSlots(), nil)
		if err != nil {
			note(err)
			continue
		}
		logf("proving %s#%d: running its tests with and without the fix", d.Repo, d.Number)
		pr, err := codeindex.ProveFix(ctx, dir, codeindex.ProveInput{
			Fix:   codeindex.FixInput{Merge: d.Merge, Head: d.Head, Base: d.Base, Commits: d.Commits, FiledAt: time.UnixMilli(d.FiledAt)},
			Intro: d.Intro,
		})
		slot.release()
		if err != nil {
			fail(d.PR, fmt.Errorf("%s#%d: %w", d.Repo, d.Number, err))
			continue
		}
		if err := c.callInto(ctx, "POST", "/api/v1/escapes/proofs", fixProofUpload{PR: d.PR, Machine: machine, FixProof: pr}, nil); err != nil {
			fail(d.PR, fmt.Errorf("%s#%d: %w", d.Repo, d.Number, err))
			continue
		}
		logf("%s#%d: %s", d.Repo, d.Number, pr.Outcome)
		sent++
	}
	return sent, failed, firstErr
}

// pickProofs is a round's fixes: the first n of due that aren't held, after
// dropping the holds that have run out.
func pickProofs(due []proofDue, held map[int64]time.Time, now time.Time, n int) []proofDue {
	maps.DeleteFunc(held, func(_ int64, until time.Time) bool { return !now.Before(until) })
	var out []proofDue
	for _, d := range due {
		if _, ok := held[d.PR]; ok || len(out) == n {
			continue
		}
		out = append(out, d)
	}
	return out
}

func cmdEscapesProve(c *cli, args []string) error {
	fs := c.flags()
	limit := fs.Int("limit", 1, "prove at most `N` fixes")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	repo := originRepo(".")
	if repo == "" {
		return errors.New("this isn't a checkout with an origin")
	}
	q := url.Values{"repo": {repo}, "limit": {strconv.Itoa(max(*limit, 1))}}
	ctx := context.Background()
	var due []proofDue
	if err := c.callInto(ctx, "GET", "/api/v1/escapes/proofs/due?"+q.Encode(), nil, &due); err != nil {
		return err
	}
	if len(due) == 0 {
		c.println("No traced fixes waiting for a proof in " + repo + ".")
		return nil
	}
	root, err := gitIn(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)
	sent, _, err := proveDue(ctx, c, due, "", func(escapeDue) (string, error) { return root, nil },
		func(f string, a ...any) { c.println(fmt.Sprintf(f, a...)) })
	c.println(fmt.Sprintf("Proved %d of %d fixes in %s; wp escapes ls shows them.", sent, len(due), repo))
	return err
}

// proveFixes is the runner's proving round, after the trace rounds it
// depends on. A fix whose proof failed is held for proveHold.
func (p *runnerProc) proveFixes(ctx context.Context) {
	lastErr := ""
	held := map[int64]time.Time{}
	for wait := 6 * time.Minute; sleepCtx(ctx, wait); wait = proveEvery {
		var due []proofDue
		err := p.c.callInto(ctx, "GET", "/api/v1/escapes/proofs/due?limit="+strconv.Itoa(proveAsk)+"&machine="+url.QueryEscape(p.opts.Name), nil, &due)
		if err != nil && isNotFound(err) {
			continue
		}
		due = pickProofs(due, held, time.Now(), proveRound)
		if err == nil && len(due) > 0 {
			var sent int
			var failed []int64
			sent, failed, err = proveDue(ctx, p.c, due, p.opts.Name, func(d escapeDue) (string, error) { return resolveDir(d.Dir, p.allowed) }, func(string, ...any) {})
			for _, pr := range failed {
				held[pr] = time.Now().Add(proveHold)
			}
			if sent > 0 {
				p.logf("proved %d fixes for the escape ledger", sent)
			}
		}
		if err != nil && err.Error() != lastErr {
			p.logf("fix proofs: %v", err)
		}
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
}
