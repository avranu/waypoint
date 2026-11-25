package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// slotLongRun is the typical run time from which a run counts as long and may
// only take the lower half of the slots (WP-1604).
const slotLongRun = 5 * time.Minute

const (
	slotWaitsFile    = "waits.jsonl"
	slotWaitsMaxSize = 512 << 10
	slotWaitsKeep    = 2000
	slotClassRuns    = 5
	slotClassShort   = "short"
	slotClassLong    = "long"
)

// slotOSArgs is os.Args, overridable by tests.
var slotOSArgs = os.Args

// waitRec is one line of waits.jsonl: a completed hold.
type waitRec struct {
	Name  string `json:"name"`
	Repo  string `json:"repo"`
	Cmd   string `json:"cmd"`
	Class string `json:"class"`
	At    int64  `json:"at"`
	Wait  int64  `json:"wait"`
	Secs  int64  `json:"secs"`
	// GaveUp marks a wait that ended without a slot (stopped or failed).
	GaveUp bool `json:"gave_up,omitempty"`
	// Bound is the CPU bound the run had (slotBound*); "" for a hold that
	// ran no command of its own or from before it was kept. Kept here only:
	// the server's slot_holds has no column for it (WP-1876).
	Bound string `json:"bound,omitempty"`
	// Narrowed marks a go test narrowed to the diff (WP-1877) and All one
	// asked to run everything with --all or WP_TEST_ALL=1. Kept here only: the
	// server's decoder refuses a field it doesn't know, so sending them would
	// break clients newer than the server.
	Narrowed bool `json:"narrowed,omitempty"`
	All      bool `json:"all,omitempty"`
}

// queueRec is the content of NAME.PID.queue while a run waits.
type queueRec struct {
	Since     int64  `json:"since"`
	SinceNS   int64  `json:"since_ns,omitempty"` // Since to the nanosecond, to order waiters that came in the same second
	WD        string `json:"wd"`
	Cmd       string `json:"cmd"`
	Class     string `json:"class"`
	BehindOwn bool   `json:"behind_own"`
}

// queuedHold is a slot taken through the per-worktree queue.
type queuedHold struct {
	slot     *slotHold
	tree     *os.File
	dir      string
	rec      waitRec
	acquired time.Time
	full     *core.SlotHold
	exit     *int
}

// setExit says what the command run in the slot exited with; release
// records it.
func (h *queuedHold) setExit(code int) { h.exit = &code }

// setBound says which CPU bound the command ran under, in the record and in
// the lock file wp slot ls reads.
func (h *queuedHold) setBound(kind string) {
	if kind != h.rec.Bound {
		h.rec.Bound = kind
		h.slot.write(kind)
	}
}

func (h *queuedHold) release() {
	h.slot.release()
	now := time.Now()
	h.rec.At = now.Unix()
	h.rec.Secs = int64(now.Sub(h.acquired).Round(time.Second) / time.Second)
	if h.full != nil {
		h.full.HoldMS = now.Sub(h.acquired).Milliseconds()
		h.full.Exit = h.exit
	}
	appendWaitRecFull(h.dir, h.rec, h.full)
	_ = h.tree.Close()
}

// slotGit runs git in the current folder and returns its trimmed output ("" on failure).
func slotGit(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", args...).Output() // #nosec G204 -- fixed arguments
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// slotCmdOf is what follows the first " -- " in the arguments, else all of them.
func slotCmdOf(args []string) string {
	s := strings.Join(args, " ")
	if _, after, ok := strings.Cut(s, " -- "); ok {
		return after
	}
	return s
}

// slotRun is the command a hold is for: key is the local wait-history key
// (the arguments joined with spaces, as slotKey makes it), and shown is the
// command as the server is told it (WP-1646): each argument quoted and
// secret values blanked argument by argument, so a value with spaces after
// --password is blanked whole. Joined with spaces the argument boundaries
// are lost, so shown is never made from key.
type slotRun struct {
	key, shown string
	// narrowed: the run is a go test narrowed to the diff (key and shown end in
	// narrowedMark). all: it was asked to run everything though it could have
	// been narrowed. whole: it runs a whole heavy package or ./... with no -run
	// (planGoTest), which makes it long.
	narrowed, all, whole bool
}

// slotRunOf is the run of argv, whichever of wp testsum, wp ship --check or
// wp slot run asked for it.
func slotRunOf(argv []string) slotRun {
	return slotRun{key: slotKey(argv), shown: slotShown(goTestJSON(argv))}
}

// slotRunFor is the run of a hold that is several runs of argv (slotKeyFor).
func slotRunFor(kind string, argv []string) slotRun {
	return slotRun{key: slotKeyFor(kind, argv), shown: kind + ": " + slotShown(goTestJSON(argv))}
}

// slotRunOfArgs is the run of a wp command line (os.Args[1:]): what follows
// its first "--" argument, else all of it, as slotCmdOf.
func slotRunOfArgs(args []string) slotRun {
	shown := args
	if i := slices.Index(args, "--"); i >= 0 {
		shown = args[i+1:]
	}
	return slotRun{key: slotCmdOf(args), shown: slotShown(shown)}
}

// slotShown is argv shell-quoted with secrets blanked per argument, each
// value whole whatever it holds: the argument after a secret-named flag
// (--password x, -H x), the value of a secret --flag=x or NAME=x, and an
// attached header (-HAuthorization: x). Other arguments are redactCommand'd
// for secrets inside them (sh -c 'curl --token x').
func slotShown(argv []string) string {
	secret := func(name string) bool {
		return secretKeyCLI.MatchString(name) || name == "-H" || name == "--header" || name == "header"
	}
	out := make([]string, len(argv))
	hide := false
	for i, a := range argv {
		name, _, inline := strings.Cut(a, "=")
		switch {
		case hide:
			out[i], hide = "•••", false
		case strings.HasPrefix(a, "-H") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			out[i] = "-H•••"
		case inline && !strings.ContainsAny(name, " \t'\"") && secret(name):
			out[i] = name + "=•••"
		case !inline && strings.HasPrefix(a, "-") && secret(a):
			out[i], hide = a, true
		default:
			out[i] = redactCommand(a)
		}
	}
	return shellJoin(out)
}

// slotMedianSecs is the median of the values (mean of the middle two when even).
func slotMedianSecs(v []int64) float64 {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return float64(s[len(s)/2])
	}
	return float64(s[len(s)/2-1]+s[len(s)/2]) / 2
}

// readWaitRecs returns the records in waits.jsonl, oldest first.
func readWaitRecs(dir string) []waitRec {
	b, err := os.ReadFile(filepath.Join(dir, slotWaitsFile)) // #nosec G304 -- our own directory
	if err != nil {
		return nil
	}
	var recs []waitRec
	for _, line := range bytes.Split(b, []byte("\n")) {
		var r waitRec
		if len(line) > 0 && json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs
}

// classifyRun says long when the median of the latest 5 runs of this name,
// repo and command took slotLongRun or more.
func classifyRun(recs []waitRec, name, repo, cmd string) string {
	class, _, _ := classifyRunStats(recs, name, repo, cmd)
	return class
}

// classifyRunStats is classifyRun with what the class was decided from: the
// median of those runs and how many there were (0: no history, so short).
func classifyRunStats(recs []waitRec, name, repo, cmd string) (class string, median float64, runs int) {
	var secs []int64
	for i := len(recs) - 1; i >= 0 && len(secs) < slotClassRuns; i-- {
		if r := recs[i]; !r.GaveUp && r.Name == name && r.Repo == repo && r.Cmd == cmd {
			secs = append(secs, r.Secs)
		}
	}
	class, median = slotClassOf(secs, slotLongRun)
	return class, median, len(secs)
}

// slotClassFor is the class of a run. Inputs: the recorded runs (recs) of the
// same name, repo and command key, as classifyRunStats reads them; and
// run.whole, which planGoTest sets for a direct go test that, once the
// narrowing is decided, runs a whole heavy package or ./... with no -run in a
// repository with .waypoint/heavy-tests. That one is long on its first run,
// whatever the history says (WP-1877). Everything else, a narrowed run, a
// -run run and any command that isn't a direct go test included, follows
// its history (WP-ADR150). median and runs are the history's either way.
func slotClassFor(recs []waitRec, name, repo, cmd string, run slotRun) (class string, median float64, runs int) {
	class, median, runs = classifyRunStats(recs, name, repo, cmd)
	if run.whole {
		class = slotClassLong
	}
	return class, median, runs
}

// slotClassOf is the class a run gets from the run times (secs) of its latest
// runs: long when their median is threshold or more. With none it is short
// and the median 0.
func slotClassOf(secs []int64, threshold time.Duration) (class string, median float64) {
	if len(secs) == 0 {
		return slotClassShort, 0
	}
	median = slotMedianSecs(secs)
	if median >= threshold.Seconds() {
		return slotClassLong, median
	}
	return slotClassShort, median
}

// slotOrder is the slot indexes a run of this class tries, in order.
func slotOrder(class string, n int) []int {
	var order []int
	if class == slotClassLong {
		for i := 0; i < max(1, n/2); i++ {
			order = append(order, i)
		}
		return order
	}
	for i := n - 1; i >= 0; i-- {
		order = append(order, i)
	}
	return order
}

// appendWaitRec adds r to waits.jsonl, trimming the file when it is large.
// The whole of it runs under an exclusive flock on waits.jsonl.flock (not
// ".lock": wp slot ls reads every *.lock in the directory as a slot), so a
// record another wp appends while a trim rewrites the file isn't lost to the
// rename (WP-1604). Readers need no lock: the rename is atomic. The lock is
// best effort; holders are short-lived, and if it can't be taken the record
// is still written.
func appendWaitRec(dir string, r waitRec) { appendWaitRecFull(dir, r, nil) }

// appendWaitRecFull is appendWaitRec that also spools hold, when not nil, for
// the server (WP-1646), under the same lock.
func appendWaitRecFull(dir string, r waitRec, hold *core.SlotHold) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	p := filepath.Join(dir, slotWaitsFile)
	if lf, err := os.OpenFile(p+".flock", os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own directory
		defer func() { _ = lf.Close() }()
		if unlock, err := fileLock(lf); err == nil {
			defer unlock()
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err == nil && st.Size() > slotWaitsMaxSize {
		trimWaits(p, slotWaitsKeep)
	}
	if hold != nil {
		appendOutbox(dir, *hold)
	}
}

// trimWaits keeps the latest keep lines of the file, best effort.
func trimWaits(p string, keep int) {
	b, err := os.ReadFile(p) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	lines := bytes.SplitAfter(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	out := append(bytes.Join(lines, nil), '\n')
	f, err := os.CreateTemp(filepath.Dir(p), slotWaitsFile+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.Write(out)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil { // #nosec G703 -- our own directory
		_ = os.Remove(f.Name())
	}
}

// acquireQueued takes a slot of NAME for the run of cmd. A long run first
// takes its worktree's turn, so a worktree has one long run at a time; a
// short run goes straight to the slots it may use. The wait shows in
// NAME.PID.queue (WP-1604, WP-ADR150). A wait also says when the run is
// expected to finish if etaCmd is not "" (WP-1571).
func (c *cli) acquireQueued(ctx context.Context, name string, n int, run slotRun, etaCmd string) (qh *queuedHold, err error) {
	start := time.Now()
	dir, err := slotDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if slotSendOn(c) {
		go func() { _, _, _ = c.sendSlotHolds(context.Background()) }() // #nosec G118 -- the send has its own time budget and must not end with the wait
	}
	wd, _ := os.Getwd()
	tree := slotGit(ctx, "rev-parse", "--show-toplevel")
	if tree == "" {
		tree = wd
	}
	repo := slotGit(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if repo == "" {
		repo = wd
	}
	cmdKey := slotField(run.key)
	class, median, runs := slotClassFor(readWaitRecs(dir), name, repo, cmdKey, run)
	waited := func() int64 { return int64(time.Since(start).Round(time.Second) / time.Second) }
	var full *core.SlotHold
	if slotSendOn(c) {
		full = c.newSlotHold(dir, name, repo, slotField(tree), slotField(run.shown), class, median, runs, n, start)
	}
	var treeStart, treeEnd time.Time
	treeWaited := func() int64 {
		switch {
		case treeStart.IsZero():
			return 0
		case treeEnd.IsZero():
			return time.Since(treeStart).Milliseconds()
		}
		return treeEnd.Sub(treeStart).Milliseconds()
	}
	defer func() {
		if err != nil {
			var gave *core.SlotHold
			if full != nil {
				g := *full
				g.GaveUp, g.WaitMS, g.TreeWaitMS = true, time.Since(start).Milliseconds(), treeWaited()
				gave = &g
			}
			appendWaitRecFull(dir, waitRec{Name: name, Repo: repo, Cmd: cmdKey, Class: class, At: time.Now().Unix(), Wait: waited(), GaveUp: true, Narrowed: run.narrowed, All: run.all}, gave)
		}
	}()

	long := class == slotClassLong
	q := queueRec{Since: start.Unix(), SinceNS: start.UnixNano(), WD: slotField(wd), Cmd: cmdKey, Class: class, BehindOwn: long}
	qpath := filepath.Join(dir, fmt.Sprintf("%s.%d.queue", name, os.Getpid()))
	written := false
	writeQueue := func() {
		if b, err := json.Marshal(q); err == nil {
			written = os.WriteFile(qpath, b, 0o600) == nil || written
		}
	}
	defer func() {
		if written {
			_ = os.Remove(qpath)
		}
	}()

	var tf *os.File
	if long {
		sum := sha256.Sum256([]byte(tree))
		tf, err = os.OpenFile(filepath.Join(dir, fmt.Sprintf("tree-%s.%s.treelock", hex.EncodeToString(sum[:])[:12], name)), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- name is validated
		if err != nil {
			return nil, err
		}
		notified := false
		treeStart = time.Now()
		defer func() {
			if treeEnd.IsZero() {
				treeEnd = time.Now()
			}
		}()
		for {
			ok, lerr := slotTryLock(tf)
			if lerr != nil {
				_ = tf.Close()
				return nil, lerr
			}
			if ok {
				_ = tf.Truncate(0)
				_, _ = tf.WriteAt([]byte(fmt.Sprintf("%d\t%d\t%s\t%s\n", os.Getpid(), time.Now().Unix(), slotField(wd), slotField(strings.Join(slotOSArgs[1:], " ")))), 0)
				break
			}
			holder := slotHolderLine(tf)
			if pid, _ := strconv.Atoi(holder[0]); pid > 0 && slotIsAncestor(pid) {
				_ = tf.Close()
				tf = nil
				break
			}
			if !written {
				writeQueue()
			}
			if !notified && time.Since(start) > slotWaitNotice {
				notified = true
				c.ownRunNotice(holder)
			}
			select {
			case <-ctx.Done():
				_ = tf.Close()
				return nil, ctx.Err()
			case <-time.After(slotPoll):
			}
		}
		treeEnd = time.Now()
		q.BehindOwn = false
		if written {
			writeQueue()
		}
	}

	// A run held behind its worktree's long run came before that wait, so it
	// queues from start (WP-1876).
	actx := context.WithValue(context.WithValue(ctx, slotCtxKeyT{}, etaCmd), slotArrivalKeyT{}, start)
	hold, err := acquireSlotIn(actx, name, slotOrder(class, n), func() {
		_, _ = fmt.Fprintf(c.err, "wp slot: waiting for a free %q slot (%d on this host; wp slot ls shows who has them)\n", name, n)
		if etaCmd == "" {
			return
		}
		if note := slotQueuedNote(dir, name, etaCmd); note != "" {
			_, _ = fmt.Fprintln(c.err, note)
		}
	}, func() {
		if !written {
			writeQueue()
		}
	})
	if err != nil {
		if tf != nil {
			_ = tf.Close()
		}
		return nil, err
	}
	if full != nil {
		full.WaitMS, full.TreeWaitMS, full.Slot = time.Since(start).Milliseconds(), treeWaited(), &hold.idx
	}
	return &queuedHold{slot: hold, tree: tf, dir: dir, acquired: time.Now(), full: full, rec: waitRec{
		Name: name, Repo: repo, Cmd: cmdKey, Class: class, Wait: waited(), Bound: slotBoundOf(ctx), Narrowed: run.narrowed, All: run.all,
	}}, nil
}

// slotHolderLine reads the holder line of a lock file as four fields (pid,
// unix time, folder, args); missing ones are "".
func slotHolderLine(f *os.File) [4]string {
	b := make([]byte, 4096)
	k, _ := f.ReadAt(b, 0)
	var out [4]string
	copy(out[:], strings.SplitN(strings.TrimSpace(string(b[:k])), "\t", 4))
	return out
}

// slotIsAncestor reports whether pid is an ancestor of this process, by
// walking the parent chain in /proc (false where there is none).
func slotIsAncestor(pid int) bool {
	cur := os.Getppid()
	for i := 0; i < 64 && cur > 1; i++ {
		if cur == pid {
			return true
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(cur) + "/stat") // #nosec G304 -- a pid
		if err != nil {
			return false
		}
		s := string(b)
		j := strings.LastIndex(s, ")")
		if j < 0 {
			return false
		}
		f := strings.Fields(s[j+1:])
		if len(f) < 2 {
			return false
		}
		cur, _ = strconv.Atoi(f[1])
	}
	return false
}

// ownRunNotice says which earlier long run of this worktree holds its turn.
func (c *cli) ownRunNotice(h [4]string) {
	if h[3] == "" {
		return
	}
	mins := 0
	if ts, err := strconv.ParseInt(h[1], 10, 64); err == nil {
		mins = int(time.Since(time.Unix(ts, 0)) / time.Minute)
	}
	_, _ = fmt.Fprintf(c.err, "wp slot: waiting for this worktree's own earlier long run to finish (pid %s, running %dm: wp %s); a worktree runs one long run at a time (WP-1604)\n", h[0], mins, h[3])
}

// slotRepoLabel names a repo by its folder.
func slotRepoLabel(common string) string {
	if strings.HasSuffix(common, "/.git") {
		return filepath.Base(filepath.Dir(common))
	}
	return filepath.Base(common)
}

func slotDur(secs int64) string { return (time.Duration(secs) * time.Second).String() }

// slotQueueLs prints the live waiters and the last day's waits, and says
// whether it printed anything.
func (c *cli) slotQueueLs(dir string) bool {
	printed := false
	type waiter struct {
		name string
		pid  int
		q    queueRec
	}
	var ws []waiter
	files, _ := filepath.Glob(filepath.Join(dir, "*.queue"))
	for _, p := range files {
		base := strings.TrimSuffix(filepath.Base(p), ".queue")
		i := strings.LastIndex(base, ".")
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(base[i+1:])
		if err != nil {
			continue
		}
		if !slotPidAlive(pid) {
			_ = os.Remove(p)
			continue
		}
		b, err := os.ReadFile(p) // #nosec G304 -- our own directory
		var q queueRec
		if err != nil || json.Unmarshal(b, &q) != nil {
			continue
		}
		ws = append(ws, waiter{base[:i], pid, q})
	}
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.q.Since != b.q.Since {
			return a.q.Since < b.q.Since
		}
		return a.q.SinceNS < b.q.SinceNS || (a.q.SinceNS == b.q.SinceNS && a.pid < b.pid)
	})
	for _, w := range ws {
		what := "(" + w.q.Class + ")"
		if w.q.BehindOwn {
			what = "(behind its worktree's own run)"
		}
		_, _ = fmt.Fprintf(c.out, "%-12s waiting %s %s pid %d in %s: %s\n", w.name, time.Since(time.Unix(w.q.Since, 0)).Round(time.Second), what, w.pid, w.q.WD, w.q.Cmd)
		printed = true
	}
	type key struct{ label, class string }
	waits := map[key][]int64{}
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	unbound, runs, narrowed, all := 0, 0, 0, 0
	for _, r := range readWaitRecs(dir) {
		if r.At >= cutoff {
			k := key{slotRepoLabel(r.Repo), r.Class}
			waits[k] = append(waits[k], r.Wait)
			if !r.GaveUp {
				runs++
				if r.Bound == slotBoundNone {
					unbound++
				}
				if r.Narrowed {
					narrowed++
				}
				if r.All {
					all++
				}
			}
		}
	}
	if len(waits) == 0 {
		return printed
	}
	keys := make([]key, 0, len(waits))
	for k := range waits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].label != keys[j].label {
			return keys[i].label < keys[j].label
		}
		return keys[i].class > keys[j].class
	})
	_, _ = fmt.Fprintln(c.out, "waits over the last 24h (p50 / p95, runs):")
	for _, k := range keys {
		v := waits[k]
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		_, _ = fmt.Fprintf(c.out, "  %-11s %-5s   %s / %s (%d)\n", k.label, k.class, slotDur(slotPct(v, 0.5)), slotDur(slotPct(v, 0.95)), len(v))
	}
	_, _ = fmt.Fprintf(c.out, "runs with no CPU bound (%s=off): %d of %d\n", slotCPUEnv, unbound, runs)
	_, _ = fmt.Fprintf(c.out, "go test runs narrowed to the diff: %d; run whole with --all or %s=1: %d\n", narrowed, envTestAll, all)
	return true
}

// slotKey is the command key of a run of argv, the same whichever of wp
// testsum, wp ship --check or wp slot run asked for it (WP-1604).
func slotKey(argv []string) string { return strings.Join(goTestJSON(argv), " ") }

// slotKeyFor is the history key of a hold that is not one single run of argv
// but several (--repeat runs it N times, bench times it over many rounds,
// --base runs it again at the base). Its total would otherwise be logged as
// the command's own run time and could make later plain runs of it classify
// long, so such a hold has its own key, prefixed with what it is (WP-1604).
// The count is left out: a repeat of 3 and of 500 share a history, and a
// misjudged first run costs one run before the history classes it.
func slotKeyFor(kind string, argv []string) string { return kind + ": " + slotKey(argv) }
