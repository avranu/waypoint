package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// waits.jsonl stays local (classifier, wp slot ls); the outbox is what the server receives.

// slotPolicyVersion names the rules a hold ran under. Bump it when the class
// rule, the slot order or the worktree rule changes (WP-1646).
const slotPolicyVersion = "adr150-1"

// slotOutboxMax is the most records the outbox keeps; a variable for the tests.
var slotOutboxMax = 5000

const (
	slotOutboxFile   = "holds-outbox.jsonl"
	slotDroppedFile  = "holds-dropped"
	slotSendBatch    = 200
	slotSendBudget   = 20 * time.Second
	slotSendRequest  = 10 * time.Second
	slotSendLockFile = "holds-outbox.send.flock"
	slotSendEnv      = "WAYPOINT_SLOT_SEND"
	// slotHoldMinBytes is a lower bound on one encoded record, used only to
	// skip counting the outbox's lines while it cannot be over the cap.
	slotHoldMinBytes = 100
)

// slotSendOn says whether holds are kept for the server: one is set for this
// wp, and WAYPOINT_SLOT_SEND is not "off".
func slotSendOn(c *cli) bool {
	return c.server() != "" && !strings.EqualFold(strings.TrimSpace(os.Getenv(slotSendEnv)), "off")
}

// newSlotHold starts the record of a run that queued at start: what the class
// was decided from, the slots, and who held and awaited them then.
func (c *cli) newSlotHold(dir, name, repo, tree, cmd, class string, median float64, runs, n int, start time.Time) *core.SlotHold {
	h := &core.SlotHold{
		ID: newHoldID(), Name: name, Repo: repo, Tree: tree, Cmd: cmd, Class: class,
		ThresholdSecs: int64(slotLongRun.Seconds()), Runs: runs, Policy: slotPolicyVersion,
		Slots: n, LongSlots: len(slotOrder(slotClassLong, n)), QueuedAt: start.UnixMilli(),
	}
	if runs > 0 {
		h.MedianSecs = &median
	}
	h.Holders, h.Waiters = slotSnapshot(dir, name)
	return h
}

func newHoldID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16) + strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b)
}

// slotSnapshot reads who holds the slots of name and who waits for them, from
// the lock and queue files: a directory glob and small reads, best effort.
// A free slot is found by taking its lock for an instant, as wp slot ls does.
func slotSnapshot(dir, name string) ([]core.SlotHolder, []core.SlotWaiter) {
	holders, waiters := []core.SlotHolder{}, []core.SlotWaiter{}
	locks, _ := filepath.Glob(filepath.Join(dir, name+".*.lock"))
	for _, p := range locks {
		idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), name+"."), ".lock"))
		if err != nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0o600) // #nosec G304 -- from our own directory
		if err != nil {
			continue
		}
		if ok, err := slotTryLock(f); err != nil || ok {
			_ = f.Close()
			continue
		}
		line := slotHolderLine(f)
		_ = f.Close()
		// A held slot whose holder line is not written yet is recorded with
		// pid 0 and since 0 on purpose: the slot is held, and leaving it out
		// would undercount.
		pid, _ := strconv.Atoi(line[0])
		since, _ := strconv.ParseInt(line[1], 10, 64)
		holders = append(holders, core.SlotHolder{Slot: idx, PID: pid, Since: since})
	}
	queues, _ := filepath.Glob(filepath.Join(dir, name+".*.queue"))
	for _, p := range queues {
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), name+"."), ".queue"))
		if err != nil || pid == os.Getpid() || !slotPidAlive(pid) {
			continue
		}
		b, err := os.ReadFile(p) // #nosec G304 -- our own directory
		var q queueRec
		if err != nil || json.Unmarshal(b, &q) != nil {
			continue
		}
		waiters = append(waiters, core.SlotWaiter{PID: pid, Since: q.Since, Class: q.Class, BehindOwn: q.BehindOwn})
	}
	return holders, waiters
}

// appendOutbox adds h to the outbox. The caller holds the waits flock. Over
// slotOutboxMax records the oldest are dropped and counted in holds-dropped.
func appendOutbox(dir string, h core.SlotHold) {
	b, err := json.Marshal(h)
	if err != nil {
		return
	}
	p := filepath.Join(dir, slotOutboxFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err != nil || st.Size() < int64(slotOutboxMax)*slotHoldMinBytes {
		return
	}
	all, err := os.ReadFile(p) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	lines := bytes.SplitAfter(bytes.TrimRight(all, "\n"), []byte("\n"))
	if len(lines) <= slotOutboxMax {
		return
	}
	over := len(lines) - slotOutboxMax
	if rewriteOutbox(p, append(bytes.Join(lines[over:], nil), '\n')) {
		addDropped(dir, over)
	}
}

// addDropped adds n to the counter of holds dropped from a full outbox.
func addDropped(dir string, n int) {
	p := filepath.Join(dir, slotDroppedFile)
	_ = os.WriteFile(p, []byte(strconv.FormatInt(readDropped(dir)+int64(n), 10)+"\n"), 0o600) // #nosec G703 -- our own directory
}

func readDropped(dir string) int64 {
	b, err := os.ReadFile(filepath.Join(dir, slotDroppedFile)) // #nosec G304 -- our own directory
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return max(n, 0)
}

// rewriteOutbox replaces the outbox with data through a temp file and a
// rename, as trimWaits does, and says whether it did. A variable for the
// tests.
var rewriteOutbox = rewriteOutboxFile

func rewriteOutboxFile(p string, data []byte) bool {
	f, err := os.CreateTemp(filepath.Dir(p), slotOutboxFile+".*.tmp")
	if err != nil {
		return false
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil { // #nosec G703 -- our own directory
		_ = os.Remove(f.Name())
		return false
	}
	return true
}

// readOutbox returns the records in the outbox, oldest first; a line that is
// not a record is left out.
func readOutbox(dir string) []core.SlotHold {
	b, err := os.ReadFile(filepath.Join(dir, slotOutboxFile)) // #nosec G304 -- our own directory
	if err != nil {
		return nil
	}
	var out []core.SlotHold
	for _, line := range bytes.Split(b, []byte("\n")) {
		var h core.SlotHold
		if len(line) > 0 && json.Unmarshal(line, &h) == nil && h.ID != "" {
			out = append(out, h)
		}
	}
	return out
}

// removeSent takes the records with these ids out of the outbox, under the
// waits flock so a record another wp appended meanwhile stays. A line that is
// not a record goes too. It says whether the outbox was rewritten or removed
// (true too when there was nothing to take out).
func removeSent(dir string, ids map[string]bool) bool {
	p := filepath.Join(dir, slotOutboxFile)
	if lf, err := os.OpenFile(filepath.Join(dir, slotWaitsFile+".flock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own directory
		defer func() { _ = lf.Close() }()
		if unlock, err := fileLock(lf); err == nil {
			defer unlock()
		}
	}
	b, err := os.ReadFile(p) // #nosec G304 -- our own directory
	if err != nil {
		return os.IsNotExist(err)
	}
	var keep [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		var h core.SlotHold
		if len(line) > 0 && json.Unmarshal(line, &h) == nil && h.ID != "" && !ids[h.ID] {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		return os.Remove(p) == nil
	}
	return rewriteOutbox(p, append(bytes.Join(keep, []byte("\n")), '\n'))
}

// sendSlotHolds posts the outbox to the server in batches of up to
// slotSendBatch, until it is empty, the budget (slotSendBudget) is spent or a
// request fails, and says how many it sent and how many remain. Another wp
// already sending makes it return at once. A batch is removed on a 2xx; the
// server stores an id once, so one cut off by the process ending is sent again
// safely.
func (c *cli) sendSlotHolds(ctx context.Context) (sent, remaining int, err error) {
	dir, err := slotDir()
	if err != nil {
		return 0, 0, err
	}
	if c.server() == "" || c.token() == "" {
		return 0, len(readOutbox(dir)), fmt.Errorf("no server or login set")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, 0, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, slotSendLockFile), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = lf.Close() }()
	if ok, err := slotTryLock(lf); err != nil || !ok {
		return 0, len(readOutbox(dir)), err
	}
	ctx, cancel := context.WithTimeout(ctx, slotSendBudget)
	defer cancel()
	for {
		all := readOutbox(dir)
		remaining = len(all)
		if remaining == 0 || ctx.Err() != nil {
			return sent, remaining, ctx.Err()
		}
		batch := all[:min(len(all), slotSendBatch)]
		oldest := batch[0].QueuedAt
		for _, h := range all {
			oldest = min(oldest, h.QueuedAt)
		}
		q := url.Values{"machine": {machineName()}, "pending": {strconv.Itoa(len(all))}, "oldest": {strconv.FormatInt(oldest, 10)},
			"dropped": {strconv.FormatInt(readDropped(dir), 10)}}
		rctx, rcancel := context.WithTimeout(ctx, slotSendRequest)
		_, err := c.callCtx(rctx, "POST", "/api/v1/slot-holds?"+q.Encode(), map[string]any{"holds": batch})
		rcancel()
		if err != nil {
			return sent, remaining, err
		}
		ids := make(map[string]bool, len(batch))
		for _, h := range batch {
			ids[h.ID] = true
		}
		sent += len(batch)
		if !removeSent(dir, ids) {
			// The same batch would be posted again and again.
			return sent, remaining, fmt.Errorf("the outbox could not be rewritten")
		}
	}
}

func cmdSlotSend(c *cli, args []string) error {
	fs := c.flags()
	if pos, err := c.parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return c.usage("wp slot send takes no arguments")
	}
	sent, remaining, err := c.sendSlotHolds(context.Background())
	_, _ = fmt.Fprintf(c.out, "sent %d slot holds, %d remain\n", sent, remaining)
	if err != nil {
		return fmt.Errorf("sending slot holds: %w", err)
	}
	return nil
}
