package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var benchCmd = &command{
	name: "bench", group: "Code and CI", args: "[-n RUNS] [--base REF] [--setup SH] [--target DURATION] [--by wall|cpu] -- CMD…",
	summary: "time a command: median of several runs, against a base commit or a target",
	about: `Runs CMD several times (after one untimed warm-up) holding the "tests"
host slot (wp help slot), and prints the median wall and CPU time with the
middle half of the runs, and how busy the host was. CPU time (user + system,
CMD's children included) moves less than wall time when the machine is busy.
CMD's output is hidden; if it fails, its last lines and its exit code are
printed and wp bench exits 3: a failing command isn't timed.

--base REF checks REF out in a temporary worktree and runs CMD there too, in
the same folder relative to the repository, alternating with this tree, then
says how much faster or slower this tree is and how likely the difference is
noise (Mann-Whitney p; with 5 runs a side the smallest p is about 0.01, and
under 4 a side no difference can show as more than noise).
--setup runs a shell command once in each tree before timing (a build, say).
Build into the tree (-o ./bin), not a shared path such as /tmp: the base's
--setup runs second and would overwrite this tree's build, so both sides
would time the base.

--target DURATION makes the result a check: exit 0 when this tree's median
(by --by, wall unless given) is at or under it, 1 when not. So in a hill
climb, 1 means "not fast enough yet", 3 "broken", 2 a wrong wp bench call
(a --base that isn't a commit here included) and 130 stopped by a signal,
waiting for the slot included. Whatever CMD leaves running when it exits
is killed, so it can't slow the next run.

Each result is kept in the repository's git directory, shared by its
worktrees, against the exact tree measured (uncommitted changes included,
read once the slot is held), so the next run of the same command, from the
same folder with the same --setup, says how it compares with the last one
and the best one. If files change during the timed runs, the result says
so and wp ship doesn't send it.`,
	examples: []string{
		"wp bench -- go test -count=1 -run TestSearch ./internal/core",
		"wp bench --base origin/main --setup 'go build -o ./wpb ./cmd/wp' -- ./wpb version",
		"wp bench -n 9 --target 1s --by cpu -- ./scan testdata/big.txt",
	},
	run: cmdBench,
}

// benchTree reads the tree wp bench times; tests replace it.
var benchTree = workingTree

const (
	benchFile      = "wp-bench.jsonl"
	benchKeep      = 500
	benchMaxSize   = 512 * 1024
	benchTailLines = 20
	benchMaxRuns   = 100
	// benchFailedCode is the exit when CMD or --setup fails, kept apart from
	// 1 (target not met) because go test and most tools fail with 1.
	benchFailedCode = 3
)

// benchSample is one timed run, in seconds.
type benchSample struct{ Wall, CPU float64 }

// benchStat summarizes one measure over a side's runs: the median and the
// middle half (quartiles), in seconds.
type benchStat struct {
	Median float64 `json:"median"`
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
}

type benchSide struct {
	Label   string        `json:"label"`
	Tree    string        `json:"tree,omitempty"`
	Wall    benchStat     `json:"wall"`
	CPU     benchStat     `json:"cpu"`
	samples []benchSample `json:"-"`
}

type benchChange struct {
	Wall  float64  `json:"wall_pct"`
	CPU   float64  `json:"cpu_pct"`
	WallP *float64 `json:"wall_p,omitempty"`
	CPUP  *float64 `json:"cpu_p,omitempty"`
}

// benchRecord is one line of the kept file.
type benchRecord struct {
	At       int64   `json:"at"` // ms
	Tree     string  `json:"tree"`
	Moved    bool    `json:"moved,omitempty"` // the tree changed during the runs
	Command  string  `json:"command"`
	Dir      string  `json:"dir,omitempty"`   // the folder within the repository CMD ran in
	Setup    string  `json:"setup,omitempty"` // --setup, part of what was measured
	Runs     int     `json:"runs"`
	Wall     float64 `json:"wall"`
	CPU      float64 `json:"cpu"`
	Load     float64 `json:"load,omitempty"`
	BaseRef  string  `json:"base_ref,omitempty"`
	BaseTree string  `json:"base_tree,omitempty"`
	BaseWall float64 `json:"base_wall,omitempty"`
	BaseCPU  float64 `json:"base_cpu,omitempty"`
	// How likely the change from the base is noise (Mann-Whitney).
	WallP *float64 `json:"wall_p,omitempty"`
	CPUP  *float64 `json:"cpu_p,omitempty"`
}

type benchResult struct {
	Command  string       `json:"command"`
	Runs     int          `json:"runs"`
	By       string       `json:"by"`
	Cores    int          `json:"cores"`
	Load     []float64    `json:"load,omitempty"` // before and after, when the host tells
	Head     benchSide    `json:"head"`
	Base     *benchSide   `json:"base,omitempty"`
	Change   *benchChange `json:"change,omitempty"`
	Target   *float64     `json:"target,omitempty"`
	Met      *bool        `json:"met,omitempty"`
	Previous *benchRecord `json:"previous,omitempty"`
	Best     *benchRecord `json:"best,omitempty"`
	Moved    bool         `json:"moved,omitempty"` // files changed while it ran
}

func cmdBench(c *cli, args []string) error {
	before, argv, found := splitDashes(args)
	fs := c.flags()
	runs := fs.Int("n", 5, "timed `RUNS` per tree")
	base := fs.String("base", "", "also time `REF` (a commit, branch or tag) in a temporary worktree")
	setup := fs.String("setup", "", "shell `COMMAND` run once in each tree before timing (not timed)")
	target := fs.Duration("target", 0, "exit 1 unless the median is at most `DURATION` (500ms, 1s, 2m)")
	by := fs.String("by", "wall", "the `MEASURE` --target and the best run use: wall or cpu")
	pos, err := c.parse(fs, before)
	if err != nil {
		return err
	}
	switch {
	case !found || len(argv) == 0:
		return c.usage("give the command after --: wp bench -- CMD…")
	case len(pos) > 0:
		return c.usage("unexpected %q: the command goes after --", pos[0])
	case *runs < 1 || *runs > benchMaxRuns:
		return c.usage("-n is 1 to %d", benchMaxRuns)
	case *by != "wall" && *by != "cpu":
		return c.usage("--by is wall or cpu")
	}
	targetSet := false
	fs.Visit(func(f *flag.Flag) { targetSet = targetSet || f.Name == "target" })
	if targetSet && *target <= 0 {
		return c.usage("--target must be more than 0 (500ms, 1s, 2m)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	headDir, err := os.Getwd()
	if err != nil {
		return err
	}
	res := &benchResult{Command: shellJoin(argv), Runs: *runs, By: *by, Cores: runtime.NumCPU()}
	res.Head = benchSide{Label: "this tree"}

	var baseDir string
	if *base != "" {
		dir, cleanup, sha, err := baseWorktree(*base)
		var bad baseRefError
		switch {
		case errors.As(err, &bad):
			return c.usage("%s", bad)
		case err != nil:
			_, _ = fmt.Fprintf(c.err, "wp bench: --base %s: %v\n", *base, err)
			return exitCode(benchFailedCode)
		}
		defer cleanup()
		baseDir = dir
		res.Base = &benchSide{Label: *base + " (" + shortSHA(sha) + ")"}
		if t, err := localGit("rev-parse", sha+"^{tree}"); err == nil {
			res.Base.Tree = t
		}
	}

	env, release, err := c.takeSlotFor(ctx, "tests", defaultSlots(), slotRunFor("wp bench", argv))
	if err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(c.err, "wp bench: stopped while waiting for the slot; nothing was kept")
			return exitCode(130)
		}
		return err
	}
	defer release()
	var warmTree string
	if l, ok := hostLoad(); ok {
		res.Load = append(res.Load, l)
	}

	if *setup != "" {
		for _, d := range []string{headDir, baseDir} {
			if d == "" {
				continue
			}
			if code, tail, err := benchExec(ctx, d, env, []string{"sh", "-c", *setup}, nil); err != nil || code != 0 {
				return c.benchFailed(ctx, "--setup", d, code, tail, err)
			}
		}
	}

	sides := []*benchSide{&res.Head}
	dirs := []string{headDir}
	if res.Base != nil {
		sides, dirs = append(sides, res.Base), append(dirs, baseDir)
	}
	for round := -1; round < *runs; round++ {
		if round == 0 {
			warmTree = benchTree()
		}
		for k := range sides {
			i := k
			if (round+1)%2 == 0 && len(sides) == 2 {
				i = 1 - k
			}
			var s benchSample
			code, tail, err := benchExec(ctx, dirs[i], env, argv, &s)
			if err != nil || code != 0 {
				return c.benchFailed(ctx, sides[i].Label, dirs[i], code, tail, err)
			}
			if round >= 0 {
				sides[i].samples = append(sides[i].samples, s)
			}
		}
	}
	if l, ok := hostLoad(); ok {
		res.Load = append(res.Load, l)
	}
	if end := benchTree(); warmTree != "" && end != "" {
		res.Head.Tree, res.Moved = warmTree, end != warmTree
	}
	for _, s := range sides {
		s.Wall, s.CPU = benchStats(s.samples, func(x benchSample) float64 { return x.Wall }), benchStats(s.samples, func(x benchSample) float64 { return x.CPU })
	}
	if res.Base != nil {
		res.Change = &benchChange{
			Wall:  pctChange(res.Base.Wall.Median, res.Head.Wall.Median),
			CPU:   pctChange(res.Base.CPU.Median, res.Head.CPU.Median),
			WallP: mannWhitneyP(benchValues(res.Head.samples, false), benchValues(res.Base.samples, false)),
			CPUP:  mannWhitneyP(benchValues(res.Head.samples, true), benchValues(res.Base.samples, true)),
		}
	}
	if *target > 0 {
		t := target.Seconds()
		met := res.Head.measure(*by) <= t
		res.Target, res.Met = &t, &met
	}

	rec := benchRecord{At: time.Now().UnixMilli(), Tree: res.Head.Tree, Moved: res.Moved, Command: res.Command, Setup: *setup, Runs: *runs,
		Wall: res.Head.Wall.Median, CPU: res.Head.CPU.Median}
	if prefix, err := localGit("rev-parse", "--show-prefix"); err == nil {
		rec.Dir = strings.TrimSuffix(prefix, "/")
	}
	if len(res.Load) > 0 {
		rec.Load = slices.Max(res.Load)
	}
	if res.Base != nil {
		rec.BaseRef, rec.BaseTree, rec.BaseWall, rec.BaseCPU = *base, res.Base.Tree, res.Base.Wall.Median, res.Base.CPU.Median
		rec.WallP, rec.CPUP = res.Change.WallP, res.Change.CPUP
	}
	path := benchPath()
	res.Previous, res.Best = benchHistory(readBenchRecords(path), rec, *by)
	if rec.Tree != "" {
		appendBenchRecord(path, rec)
	}

	if c.asJSON {
		if err := c.emitValue(res); err != nil {
			return err
		}
	} else {
		c.println(benchText(res))
	}
	if res.Met != nil && !*res.Met {
		return exitCode(1)
	}
	return nil
}

func (s *benchSide) measure(by string) float64 {
	if by == "cpu" {
		return s.CPU.Median
	}
	return s.Wall.Median
}

func (r benchRecord) measure(by string) float64 {
	if by == "cpu" {
		return r.CPU
	}
	return r.Wall
}

// benchFailed reports a failed run: the command's last lines and its code.
func (c *cli) benchFailed(ctx context.Context, what, dir string, code int, tail []byte, err error) error {
	switch {
	case ctx.Err() != nil:
		_, _ = fmt.Fprintln(c.err, "wp bench: stopped; nothing was kept")
		return exitCode(130)
	case err != nil:
		_, _ = fmt.Fprintf(c.err, "wp bench: %s in %s: %v\n", what, dir, err)
		return exitCode(benchFailedCode)
	}
	_, _ = fmt.Fprintf(c.err, "wp bench: %s failed (exit %d) in %s; a failing command isn't timed. Its last lines:\n%s\n", what, code, dir, lastLines(tail, benchTailLines))
	return exitCode(benchFailedCode)
}

// benchExec runs argv in dir and, when s isn't nil, times it. It returns
// the exit code and the end of the command's output.
func benchExec(ctx context.Context, dir string, env, argv []string, s *benchSample) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 G702 -- running the command is the point
	setProcGroup(cmd)
	killOnCancel(cmd)
	cmd.Dir, cmd.Env = dir, env
	var tail tailBuffer
	cmd.Stdout, cmd.Stderr = &tail, &tail
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	procExited(cmd)
	killGroup(cmd)
	if ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return 1, tail.b, fmt.Errorf("%s exited but left a process holding its output for %s", argv[0], cmd.WaitDelay)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		return max(ee.ExitCode(), 1), tail.b, nil
	default:
		return 127, nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	if s != nil {
		s.Wall = wall.Seconds()
		if ps := cmd.ProcessState; ps != nil {
			s.CPU = (ps.UserTime() + ps.SystemTime()).Seconds()
		}
	}
	return 0, tail.b, nil
}

// hostLoad is the one-minute load average, where the host reports it.
func hostLoad() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func benchValues(s []benchSample, cpu bool) []float64 {
	out := make([]float64, len(s))
	for i, x := range s {
		out[i] = x.Wall
		if cpu {
			out[i] = x.CPU
		}
	}
	return out
}

func benchStats(s []benchSample, f func(benchSample) float64) benchStat {
	v := make([]float64, len(s))
	for i, x := range s {
		v[i] = f(x)
	}
	slices.Sort(v)
	return benchStat{Median: quantile(v, 0.5), Low: quantile(v, 0.25), High: quantile(v, 0.75)}
}

// quantile of sorted v, interpolating between neighbours.
func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	pos := q * float64(len(v)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(v)-1)
	return v[lo] + (v[hi]-v[lo])*(pos-float64(lo))
}

func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

// mannWhitneyP is the two-sided p-value of the Mann-Whitney U test (normal
// approximation with tie and continuity corrections); nil when a side is
// empty or nothing varies.
func mannWhitneyP(a, b []float64) *float64 {
	n1, n2 := len(a), len(b)
	if n1 == 0 || n2 == 0 {
		return nil
	}
	all := append(append([]float64(nil), a...), b...)
	idx := make([]int, len(all))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(x, y int) int {
		switch {
		case all[x] < all[y]:
			return -1
		case all[x] > all[y]:
			return 1
		}
		return 0
	})
	rank := make([]float64, len(all))
	var ties float64
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && all[idx[j+1]] == all[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			rank[idx[k]] = r
		}
		t := float64(j - i + 1)
		ties += t*t*t - t
		i = j + 1
	}
	var r1 float64
	for i := 0; i < n1; i++ {
		r1 += rank[i]
	}
	u := r1 - float64(n1*(n1+1))/2
	n := float64(n1 + n2)
	variance := float64(n1*n2) / 12 * ((n + 1) - ties/(n*(n-1)))
	if variance <= 0 {
		return nil
	}
	z := math.Max(math.Abs(u-float64(n1*n2)/2)-0.5, 0) / math.Sqrt(variance)
	p := math.Erfc(z / math.Sqrt2)
	return &p
}

// benchPath is the kept results file, in the repository's common git
// directory so every worktree of a repository adds to one history.
func benchPath() string {
	p, err := localGit("rev-parse", "--git-common-dir")
	if err != nil || p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Join(p, benchFile)
}

func readBenchRecords(p string) []benchRecord {
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p) // #nosec G304 -- under the git dir
	if err != nil {
		return nil
	}
	var out []benchRecord
	for _, l := range strings.Split(string(data), "\n") {
		var r benchRecord
		if l != "" && json.Unmarshal([]byte(l), &r) == nil && r.Command != "" {
			out = append(out, r)
		}
	}
	return out
}

// appendBenchRecord keeps r, and only the newest benchKeep once the file
// grows past benchMaxSize. Keeping never fails a benchmark.
func appendBenchRecord(p string, r benchRecord) {
	if p == "" {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- under the git dir
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err != nil || st.Size() <= benchMaxSize {
		return
	}
	recs := readBenchRecords(p)
	if len(recs) > benchKeep {
		recs = recs[len(recs)-benchKeep:]
	}
	var b strings.Builder
	for _, x := range recs {
		if l, err := json.Marshal(x); err == nil {
			b.Write(l)
			b.WriteByte('\n')
		}
	}
	tmp := p + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o600) != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
	}
}

// benchHistory is the newest earlier result for the same command, run from
// the same folder with the same --setup, and the best one by the measure.
func benchHistory(recs []benchRecord, like benchRecord, by string) (prev, best *benchRecord) {
	for i := range recs {
		r := &recs[i]
		if r.Command != like.Command || r.Dir != like.Dir || r.Setup != like.Setup {
			continue
		}
		prev = r
		if best == nil || r.measure(by) < best.measure(by) {
			best = r
		}
	}
	return prev, best
}

// fmtSec prints seconds the way people read durations.
func fmtSec(s float64) string {
	switch {
	case s < 0.001:
		return fmt.Sprintf("%.0fµs", s*1e6)
	case s < 1:
		return fmt.Sprintf("%.0fms", s*1e3)
	case s < 10:
		return fmt.Sprintf("%.2fs", s)
	case s < 120:
		return fmt.Sprintf("%.1fs", s)
	}
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}

func benchText(r *benchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d timed runs", r.Command, r.Runs)
	if r.Base != nil {
		b.WriteString(" a tree, alternating")
	}
	b.WriteString("\n")
	sides := []*benchSide{&r.Head}
	if r.Base != nil {
		sides = append(sides, r.Base)
	}
	w := 0
	for _, s := range sides {
		w = max(w, len([]rune(s.Label)))
	}
	for _, s := range sides {
		fmt.Fprintf(&b, "  %-*s  wall %s (%s–%s)  cpu %s (%s–%s)\n", w, s.Label,
			fmtSec(s.Wall.Median), fmtSec(s.Wall.Low), fmtSec(s.Wall.High),
			fmtSec(s.CPU.Median), fmtSec(s.CPU.Low), fmtSec(s.CPU.High))
	}
	if ch := r.Change; ch != nil {
		fmt.Fprintf(&b, "this tree vs %s: wall %s, cpu %s\n", r.Base.Label, changeText(ch.Wall, ch.WallP, r.Runs), changeText(ch.CPU, ch.CPUP, r.Runs))
	}
	if len(r.Load) > 0 {
		l := slices.Max(r.Load)
		fmt.Fprintf(&b, "host load %.1f on %d cores", l, r.Cores)
		if l > float64(r.Cores)/2 {
			b.WriteString(": busy, so wall times are rough; cpu moves less")
		}
		b.WriteString("\n")
	}
	if r.Previous != nil {
		fmt.Fprintf(&b, "last kept for this command: %s %s (%s, tree %s)", r.By, fmtSec(r.Previous.measure(r.By)), agoMS(r.Previous.At), shortSHA(r.Previous.Tree))
		if r.Best != nil && r.Best != r.Previous {
			fmt.Fprintf(&b, "; best %s (tree %s)", fmtSec(r.Best.measure(r.By)), shortSHA(r.Best.Tree))
		}
		b.WriteString("\n")
	}
	if r.Moved {
		b.WriteString("files in this tree changed during the timed runs (CMD or someone else wrote them), so the result is kept but wp ship won't send it\n")
	}
	if r.Target != nil {
		got := r.Head.measure(r.By)
		if *r.Met {
			fmt.Fprintf(&b, "target %s %s: met (%s)\n", r.By, fmtSec(*r.Target), fmtSec(got))
		} else {
			fmt.Fprintf(&b, "target %s %s: not met (%s, %s over)\n", r.By, fmtSec(*r.Target), fmtSec(got), fmtSec(got-*r.Target))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// benchMinRunsForP is the fewest runs a side at which a Mann-Whitney p can
// fall under 0.05 (fully separated: 3 a side give 0.08, 4 give 0.03).
const benchMinRunsForP = 4

// changeText says a change and whether it could be noise.
func changeText(pct float64, p *float64, runs int) string {
	dir := "slower"
	if pct < 0 {
		dir = "faster"
	}
	s := fmt.Sprintf("%.0f%% %s", math.Abs(pct), dir)
	switch {
	case p == nil:
		return s + " (no spread to judge)"
	case *p < 0.05:
		return s + fmt.Sprintf(" (p=%.3f)", *p)
	case runs < benchMinRunsForP:
		return s + fmt.Sprintf(" (p=%.2f; %d runs a side can't tell a change from noise, -n %d or more can)", *p, runs, benchMinRunsForP)
	}
	return s + fmt.Sprintf(" (p=%.2f, could be noise)", *p)
}

// shellJoin writes argv as a shell would need it typed, so `echo "a b"` and
// `echo a b` are kept apart.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./=:,@%+") == "" {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

func agoMS(ms int64) string {
	t := time.UnixMilli(ms)
	return ago(&t)
}
