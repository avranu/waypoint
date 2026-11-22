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
	"sort"
	"strconv"
	"strings"
	"time"
)

// slotLongRun is the typical run time from which a run counts as long and may
// only take the lower half of the slots (WP-1604).
const slotLongRun = 5 * time.Minute

const (
	slotWaitsFile    = "waits.jsonl"
	slotWaitsMaxSize = 512 << 10
	slotWaitsKeep    = 2000
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
}

// queueRec is the content of NAME.PID.queue while a run waits.
type queueRec struct {
	Since     int64  `json:"since"`
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
}

func (h *queuedHold) release() {
	h.slot.release()
	h.rec.At = time.Now().Unix()
	h.rec.Secs = int64(time.Since(h.acquired).Round(time.Second) / time.Second)
	appendWaitRec(h.dir, h.rec)
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
	var secs []int64
	for i := len(recs) - 1; i >= 0 && len(secs) < 5; i-- {
		if r := recs[i]; r.Name == name && r.Repo == repo && r.Cmd == cmd {
			secs = append(secs, r.Secs)
		}
	}
	if len(secs) > 0 && slotMedianSecs(secs) >= slotLongRun.Seconds() {
		return slotClassLong
	}
	return slotClassShort
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

func appendWaitRec(dir string, r waitRec) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	p := filepath.Join(dir, slotWaitsFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err == nil && st.Size() > slotWaitsMaxSize {
		trimWaits(p, slotWaitsKeep)
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
	tmp := p + ".tmp"
	if os.WriteFile(tmp, out, 0o600) != nil || os.Rename(tmp, p) != nil { // #nosec G703 -- our own directory
		_ = os.Remove(tmp)
	}
}

// acquireQueued takes this worktree's turn for NAME, then a slot of the
// class's range, and shows the wait in NAME.PID.queue (WP-1604, WP-ADR150).
func (c *cli) acquireQueued(ctx context.Context, name string, n int) (*queuedHold, error) {
	start := time.Now()
	dir, err := slotDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
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
	cmdKey := slotField(slotCmdOf(slotOSArgs[1:]))
	class := classifyRun(readWaitRecs(dir), name, repo, cmdKey)

	q := queueRec{Since: start.Unix(), WD: slotField(wd), Cmd: cmdKey, Class: class, BehindOwn: true}
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

	// One slot per worktree: wait behind this worktree's own earlier run.
	sum := sha256.Sum256([]byte(tree))
	tf, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("tree-%s.%s.treelock", hex.EncodeToString(sum[:])[:12], name)), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- name is validated
	if err != nil {
		return nil, err
	}
	notified := false
	for {
		ok, err := slotTryLock(tf)
		if err != nil {
			_ = tf.Close()
			return nil, err
		}
		if ok {
			break
		}
		if !written {
			writeQueue()
		}
		if !notified && time.Since(start) > slotWaitNotice {
			notified = true
			c.ownRunNotice(tf)
		}
		select {
		case <-ctx.Done():
			_ = tf.Close()
			return nil, ctx.Err()
		case <-time.After(slotPoll):
		}
	}
	_ = tf.Truncate(0)
	_, _ = tf.WriteAt([]byte(fmt.Sprintf("%d\t%d\t%s\t%s\n", os.Getpid(), time.Now().Unix(), slotField(wd), slotField(strings.Join(slotOSArgs[1:], " ")))), 0)
	q.BehindOwn = false
	if written {
		writeQueue()
	}

	hold, err := acquireSlotIn(ctx, name, slotOrder(class, n), func() {
		_, _ = fmt.Fprintf(c.err, "wp slot: waiting for a free %q slot (%d on this host; wp slot ls shows who has them)\n", name, n)
	}, func() {
		if !written {
			writeQueue()
		}
	})
	if err != nil {
		_ = tf.Close()
		return nil, err
	}
	now := time.Now()
	return &queuedHold{slot: hold, tree: tf, dir: dir, acquired: now, rec: waitRec{
		Name: name, Repo: repo, Cmd: cmdKey, Class: class, Wait: int64(now.Sub(start).Round(time.Second) / time.Second),
	}}, nil
}

// ownRunNotice says which earlier run of this worktree holds its turn.
func (c *cli) ownRunNotice(tf *os.File) {
	b := make([]byte, 4096)
	k, _ := tf.ReadAt(b, 0)
	parts := strings.SplitN(strings.TrimSpace(string(b[:k])), "\t", 4)
	if len(parts) < 4 {
		return
	}
	mins := 0
	if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
		mins = int(time.Since(time.Unix(ts, 0)) / time.Minute)
	}
	_, _ = fmt.Fprintf(c.err, "wp slot: waiting for this worktree's own earlier run to finish (pid %s, running %dm: wp %s); a worktree runs one at a time (WP-1604)\n", parts[0], mins, parts[3])
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
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].q.Since < ws[j].q.Since })
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
	for _, r := range readWaitRecs(dir) {
		if r.At >= cutoff {
			k := key{slotRepoLabel(r.Repo), r.Class}
			waits[k] = append(waits[k], r.Wait)
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
		return keys[i].class > keys[j].class // short before long
	})
	_, _ = fmt.Fprintln(c.out, "waits over the last 24h (p50 / p95, runs):")
	for _, k := range keys {
		v := waits[k]
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		pct := func(p float64) int64 { return v[max(0, int(p*float64(len(v))+0.999999)-1)] }
		_, _ = fmt.Fprintf(c.out, "  %-11s %-5s   %s / %s (%d)\n", k.label, k.class, slotDur(pct(0.5)), slotDur(pct(0.95)), len(v))
	}
	return true
}
