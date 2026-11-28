package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"git.jmann.me/claude/waypoint/internal/db"
)

// loopSnapshot is a loopEnv at rest, kept so that a test whose cells all
// start from the same round (newLoopEnv, then one review) builds it once.
// Building it is most of such a test's time: each env creates a database, a project, a
// runner, a forge and issues through the Service, and every statement is
// parsed again (WP-1942 has the figures). start makes a new loopEnv from
// it: its own database, a copy of the snapshot's, its own Service, and its
// own copies of what the test keeps in memory: the actors, project, runner
// and task (deepCopy), the clock and the caught jobs, and a new fake forge,
// gateway and decision model holding what the build left in them.
//
// Of what the build left in the Service's memory, start carries the
// gateway's cached task limits, so the env asks the gateway when a build
// would. It does not carry what a restart loses and the Service makes
// again when it is needed: the similarity and glossary indexes and the
// relation graph (read from the database), that the goals were counted
// (they are counted again), the goal judge timer's generation (compared
// only with itself), the audit and branch locks (made on first use), and
// the job ledger's counts of log records (read by job health, not by a
// round). TestLoopSnapshotEnvMatchesABuild names each of those fields and
// fails when a build and a started env differ in any other field of the
// Service, either way, as far as setFields sees.
type loopSnapshot struct {
	src *loopEnv
	db  []byte
}

// snapshotLoopEnv takes src's database as it is, once its background work
// has ended. start reads src's in-memory state each time, so src must not
// run or change after the snapshot.
func snapshotLoopEnv(t *testing.T, src *loopEnv) *loopSnapshot {
	t.Helper()
	src.s.Wait()
	src.s.quiesceGoals(t)
	out := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := src.s.db.W.Exec(`VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) // #nosec G304 -- our own temp file
	if err != nil {
		t.Fatal(err)
	}
	return &loopSnapshot{src: src, db: data}
}

// start is a new loopEnv in the snapshot's state. Its job catcher is the
// one in force from here on (catchJobs), as a newly built env's is.
func (sn *loopSnapshot) start(t *testing.T) *loopEnv {
	t.Helper()
	src := sn.src
	path := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(path, sn.db, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// newEnv: a Service on the database, the test's clock where it stood.
	e := envOn(t, d, src.clockNow())
	s := e.s
	src.s.gatewayLimits.Range(func(k, v any) bool {
		s.gatewayLimits.Store(k, v)
		return true
	})
	e.admin = deepCopy(src.admin)

	// newTaskEnv: the agent's actor, in the session the hook opened.
	te := &taskEnv{env: e, ag: deepCopy(src.ag), cli: src.cli, session: src.session}

	// newAPEnv: the project, and the jobs caught so far with their results.
	a := &apEnv{taskEnv: te, p: deepCopy(src.p), jobs: deepCopy(src.jobs), res: deepCopy(src.res)}
	a.catchJobs()

	// taskGateway, firstEnv, newLoopEnv: the fake gateway, the fake forge
	// that has seen the round's head, and the decision model's script.
	sl := src.s.LLM.(*gatewayScoringLLM)
	g := newShipForge()
	g.heads = maps.Clone(src.g.heads)
	s.Forges = g
	tl := &taskLLM{LLM: s.LLM, tasks: map[string]*gatewayTask{}, limitsCalls: sl.limitsCalls}
	j := &jevScript{choice: maps.Clone(src.j.choice), calls: src.j.calls, states: slices.Clone(src.j.states)}
	s.LLM = &gatewayScoringLLM{taskLLM: tl, jevScript: j}
	return &loopEnv{apEnv: a, box: deepCopy(src.box), g: g, j: j, task: deepCopy(src.task)}
}

// isCorePkg reports whether pkg is internal/core or a package split from it
// (internal/core/kernel, internal/core/users, ...).
func isCorePkg(pkg string) bool {
	return strings.HasSuffix(pkg, "/internal/core") || strings.Contains(pkg, "/internal/core/")
}

// deepCopy is x with nothing shared: every pointer, slice, map and
// interface value reached through x and core packages' structs is new,
// unexported fields included. A pointer met twice is copied once. Other
// packages' structs (a time, a mutex), funcs and channels are assigned.
func deepCopy[T any](x T) T {
	var out T
	seen := map[unsafe.Pointer]reflect.Value{}
	var cp func(dst, src reflect.Value)
	// held is a value of v's type holding v, addressable so cp can fill a copy of it.
	held := func(v reflect.Value) reflect.Value {
		h := reflect.New(v.Type()).Elem()
		h.Set(v)
		return h
	}
	cp = func(dst, src reflect.Value) {
		switch src.Kind() {
		case reflect.Pointer:
			if src.IsNil() {
				return
			}
			if n, ok := seen[src.UnsafePointer()]; ok {
				dst.Set(n)
				return
			}
			n := reflect.New(src.Type().Elem())
			seen[src.UnsafePointer()] = n
			cp(n.Elem(), src.Elem())
			dst.Set(n)
		case reflect.Struct:
			if !isCorePkg(src.Type().PkgPath()) {
				dst.Set(src)
				return
			}
			for i := range src.NumField() {
				// #nosec G103 -- a test copies unexported fields in place
				cp(reflect.NewAt(dst.Field(i).Type(), unsafe.Pointer(dst.Field(i).UnsafeAddr())).Elem(),
					reflect.NewAt(src.Field(i).Type(), unsafe.Pointer(src.Field(i).UnsafeAddr())).Elem())
			}
		case reflect.Array:
			for i := range src.Len() {
				cp(dst.Index(i), src.Index(i))
			}
		case reflect.Slice:
			if src.IsNil() {
				return
			}
			n := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
			for i := range src.Len() {
				cp(n.Index(i), src.Index(i))
			}
			dst.Set(n)
		case reflect.Map:
			if src.IsNil() {
				return
			}
			n := reflect.MakeMapWithSize(src.Type(), src.Len())
			for it := src.MapRange(); it.Next(); {
				k, v := reflect.New(src.Type().Key()).Elem(), reflect.New(src.Type().Elem()).Elem()
				cp(k, held(it.Key()))
				cp(v, held(it.Value()))
				n.SetMapIndex(k, v)
			}
			dst.Set(n)
		case reflect.Interface:
			if src.IsNil() {
				return
			}
			n := reflect.New(src.Elem().Type()).Elem()
			cp(n, held(src.Elem()))
			dst.Set(n)
		default:
			dst.Set(src)
		}
	}
	cp(reflect.ValueOf(&out).Elem(), reflect.ValueOf(&x).Elem())
	return out
}

// refs is where each pointer, map and slice reached through x and this
// package's structs points, with the path of the field that holds it:
// what two values must not have in common to share nothing.
func refs(x any) map[unsafe.Pointer]string {
	out := map[unsafe.Pointer]string{}
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return
			}
			if v.Kind() == reflect.Pointer {
				if _, ok := out[v.UnsafePointer()]; ok {
					return
				}
				out[v.UnsafePointer()] = path
			}
			walk(v.Elem(), path)
		case reflect.Struct:
			if !isCorePkg(v.Type().PkgPath()) {
				return
			}
			for i := range v.NumField() {
				walk(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		case reflect.Map:
			if v.IsNil() {
				return
			}
			out[v.UnsafePointer()] = path
			for it := v.MapRange(); it.Next(); {
				walk(it.Value(), path+"[]")
			}
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice {
				if v.Cap() == 0 {
					return
				}
				out[v.UnsafePointer()] = path
			}
			for i := range v.Len() {
				walk(v.Index(i), path+"[]")
			}
		}
	}
	walk(reflect.ValueOf(x), "")
	return out
}

// sharedRefs is the fields of a that point where something in b does.
func sharedRefs(a, b any) []string {
	var out []string
	rb := refs(b)
	for p, path := range refs(a) {
		if _, ok := rb[p]; ok {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// fieldDiffs is the fields two setFields readings differ in, either way,
// leaving out the ones named in but, where a name ending in a dot is every
// field under it.
func fieldDiffs(built, started map[string]string, but []string) []string {
	var out []string
	keys := maps.Clone(built)
	maps.Copy(keys, started)
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if built[k] != started[k] && !slices.ContainsFunc(but, func(b string) bool { return b == k || strings.HasSuffix(b, ".") && strings.HasPrefix(k, b) }) {
			out = append(out, fmt.Sprintf("%s: a build holds %q, a started env %q", k, built[k], started[k]))
		}
	}
	return out
}

// doneRound makes the round e.review ran an ended one (the session's turn is
// over, its report read): the shape the scenarios start from.
func (e *loopEnv) doneRound(round int64) {
	e.t.Helper()
	e.exec(`UPDATE work_log SET state = 'done', ended_at = ?, read_state = 'done' WHERE id = ?`, e.clockNow().UnixMilli(), round)
}

// readNothing makes the round's reviewer audit one that read nothing.
func (e *loopEnv) readNothing(round int64) {
	e.t.Helper()
	_, au := e.roundAuditRow(round)
	e.exec(`UPDATE code_audits SET attempts = '[{"model":"x","outcome":"nothing"}]' WHERE id = ?`, au.ID)
	e.exec(`DELETE FROM code_audit_findings WHERE audit_id = ?`, au.ID)
}

// ownReport records a session's own report audit for the round: clean, or
// with one finding still open.
func (e *loopEnv) ownReport(round int64, open bool) {
	e.t.Helper()
	e.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, status, request_key)
		SELECT project_id, forge_id, repo, target, owner_id, created_at, 'done', 'review-round:' || ? || ':own' FROM code_audits WHERE id = (SELECT audit_id FROM work_log WHERE id = ?)`, round, round)
	if open {
		e.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, severity, problem, state)
			SELECT id, 1, 'Merge loses an update', 'high', 'Two saves at once.', 'open' FROM code_audits WHERE request_key = 'review-round:' || ? || ':own'`, round)
	}
}

func (e *loopEnv) bpOf(round int64) *beforePR { return roundBeforePR(e.ctx, e.s.db.R, round) }

// A: the reviewer's audit is done and read nothing, and the session reported
// nothing. The server's ship used to hold in reviewAgain and count the hold as
// a success, for ever and in silence.
func TestRoundWithNoReviewIsNotShippedBySilence(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.doneRound(r1)
	for i := 0; i < 3; i++ {
		e.tick(2 * time.Minute)
		out, err := e.s.advanceRound(e.ctx, r1)
		if err != nil || out.Do != doTell || out.Need != needReport {
			t.Fatalf("advanceRound #%d = %+v, %v: want the session asked for its report", i, out, err)
		}
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("opened %d pull requests with no review", n)
	}
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	for _, h := range []time.Duration{3 * time.Hour, 30 * time.Hour, 72 * time.Hour} {
		e.setClock(e.clockNow().Add(h))
		_, _ = e.s.advanceRound(e.ctx, r1)
		e.tickAll(time.Minute)
		e.s.Wait()
		if n := len(e.g.created); n != 0 {
			t.Fatalf("at +%v a pull request opened with no review (%d)", h, n)
		}
	}
	if bp := e.bpOf(r1); bp == nil || bp.ShipStop == "" {
		t.Fatalf("the round was not ended: %+v", bp)
	}
	if n := e.notified("No review of this round was recorded"); n != 1 {
		t.Fatalf("the person was told %d times", n)
	}
}

// B: the session's own review is the round and no report is recorded: the
// session's ship is told to report, and the server doesn't open the pull
// request without it either.
func TestRoundOfTheSessionsOwnReviewNeedsItsReport(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.exec(`UPDATE work_log SET audit_id = NULL, audit_note = ? WHERE id = ?`, ownReviewNote, r1)
	e.doneRound(r1)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	e.tick(2 * time.Minute)
	out, err := e.s.advanceRound(e.ctx, r1)
	if err != nil || out.Do != doTell {
		t.Fatalf("advanceRound = %+v, %v", out, err)
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("the server opened %d pull requests with no review", n)
	}
}

// C: the session's own report has a finding still open: it holds the session's
// ship, and the server's decision sees it too.
func TestOwnReportFindingsAreFindingsToDecide(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.ownReport(r1, true)
	e.doneRound(r1)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || in.Undecided != 1 || !in.Own {
		t.Fatalf("facts = %+v, %v", in, err)
	}
	if out := decideRound(in); out.Do != doTell {
		t.Fatalf("decideRound = %+v: want the session asked to decide the finding", out)
	}
	in.ShipOpens = true
	if out := decideRound(in); out.Do != doTell || out.Need != needMarks {
		t.Fatalf("decideRound for a ship = %+v: want the session asked to mark the finding", out)
	}
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "still to decide") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	e.tick(2 * time.Minute)
	if out, err := e.s.advanceRound(e.ctx, r1); err != nil || out.Do != doTell {
		t.Fatalf("advanceRound = %+v, %v", out, err)
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("opened %d pull requests over an open finding", n)
	}
}

// The outcome of a round's ship, by the session's wp ship and by the server,
// for every combination of what is recorded of its review: the same class, and
// no pull request without a review.
func TestSessionAndServerShipsReachTheSameOutcome(t *testing.T) {
	const (
		opens   = "opens"
		session = "held for the session"
		wait    = "waits on the server"
		ended   = "ended and told"
	)
	type cell struct {
		audit   string
		own     string
		handed  bool
		live    bool
		decided bool
	}
	reviewed := func(c cell) bool {
		return c.own != "none" || c.handed || c.decided || c.audit == "read" || c.audit == "found"
	}
	expect := func(c cell, serverOnly bool) string {
		switch {
		case reviewed(c):
			if c.decided {
				return opens
			}
			open := c.own == "open" || (c.own == "none" && !c.handed && c.audit == "found")
			if c.own == "none" && c.handed {
				open = false
			}
			if open && c.live {
				return session
			}
			return opens
		case c.audit == "queued" || c.audit == "running":
			return wait
		case c.live:
			return session
		}
		return ended
	}
	var cells []cell
	for _, a := range []string{"none", "queued", "running", "read", "found", "nothing", "failed", "cancelled", "limited"} {
		for _, o := range []string{"none", "clean", "open"} {
			for _, h := range []bool{false, true} {
				for _, l := range []bool{true, false} {
					cells = append(cells, cell{a, o, h, l, false})
				}
			}
		}
	}
	for _, a := range []string{"none", "nothing", "failed"} {
		cells = append(cells, cell{a, "none", false, true, true})
	}
	src := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := src.review("Merge loses an update")
	snap := snapshotLoopEnv(t, src)
	build := func(t *testing.T, c cell) (*loopEnv, int64) {
		e := snap.start(t)
		_, au := e.roundAuditRow(r)
		if c.own != "none" {
			e.ownReport(r, c.own == "open")
		}
		switch c.audit {
		case "none":
			e.exec(`UPDATE work_log SET audit_id = NULL WHERE id = ?`, r)
		case "queued", "running", "failed", "cancelled", "limited":
			e.exec(`UPDATE code_audits SET status = ? WHERE id = ?`, c.audit, au.ID)
		case "nothing":
			e.readNothing(r)
		case "read":
			e.exec(`DELETE FROM code_audit_findings WHERE audit_id = ?`, au.ID)
		}
		if c.handed {
			e.exec(`UPDATE work_log SET audit_note = ? WHERE id = ?`, stakesNoClaude, r)
		}
		if c.decided {
			e.exec(`INSERT INTO review_again_decisions(round_id, issue_id, audit_id, arm, created_at, round_index)
				SELECT id, issue_id, ?, 'again', ?, 1 FROM work_log WHERE id = ?`, au.ID, e.s.nowMS(), r)
		}
		e.doneRound(r)
		if !c.live {
			e.endSession()
		}
		return e, r
	}
	byServer := func(t *testing.T, c cell) string {
		e, r := build(t, c)
		e.tick(2 * time.Minute)
		if !c.live && c.audit != "queued" && c.audit != "running" {
			e.tick(3 * time.Hour)
		}
		_, err := e.s.advanceRound(e.ctx, r)
		if err != nil {
			t.Logf("advanceRound error: %v", err)
		}
		e.s.Wait()
		if len(e.g.created) > 0 {
			if !reviewed(c) {
				t.Errorf("the server opened a pull request with no review")
			}
			return opens
		}
		if bp := e.bpOf(r); bp != nil && bp.ShipStop != "" {
			if e.notified("No review of this round was recorded") != 1 {
				t.Errorf("an end that told the person %d times", e.notified("No review of this round was recorded"))
			}
			return ended
		}
		in, ierr := e.s.roundInOf(e.ctx, r)
		if ierr != nil {
			t.Fatal(ierr)
		}
		switch out := decideRound(in); out.Do {
		case doTell:
			return session
		case doWait:
			return wait
		default:
			return fmt.Sprintf("other (%+v)", out)
		}
	}
	bySession := func(t *testing.T, c cell) string {
		e, _ := build(t, c)
		res, err := e.ship(e.task.Key, ShipAuto)
		if err != nil {
			return "error " + err.Error()
		}
		switch {
		case res.Pull != nil && res.Held == "":
			if !reviewed(c) {
				t.Errorf("the session's ship opened a pull request with no review")
			}
			return opens
		case strings.Contains(res.Held, "No pull request yet"), strings.Contains(res.Held, "still to decide"):
			return session
		case strings.Contains(res.Held, "still going"):
			return wait
		}
		return "other: " + clipText(res.Held, 80)
	}
	var table []string
	for _, c := range cells {
		c := c
		t.Run(fmt.Sprintf("audit=%s own=%s handed=%v live=%v decided=%v", c.audit, c.own, c.handed, c.live, c.decided), func(t *testing.T) {
			want := expect(c, false)
			got := byServer(t, c)
			if got != want {
				t.Errorf("the server: %s, want %s", got, want)
			}
			line := fmt.Sprintf("audit=%-9s own=%-5s handed=%-5v live=%-5v decided=%-5v server=%s", c.audit, c.own, c.handed, c.live, c.decided, got)
			if c.live {
				gs := bySession(t, c)
				if gs != want {
					t.Errorf("the session's ship: %s, want %s", gs, want)
				}
				line += " session=" + gs
			}
			table = append(table, line)
		})
	}
	t.Log("\n" + strings.Join(table, "\n"))
}

// R5, the first case: a reviewed ship that is held because another review
// round was scheduled ends cleanly as superseded: no error, no try counted,
// nobody told, and decideRound's answer for the round is the wait on the
// round its decision scheduled.
func TestHeldShipOfASupersededRoundIsNotAFailure(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"fixed"})
	if _, _, _, _, next := e.decision(r1); next == 0 {
		t.Fatal("no round was scheduled")
	}
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || !in.Superseded {
		t.Fatalf("facts = %+v, %v", in, err)
	}
	if out := decideRound(in); out.Do != doWait || out.Until <= 0 {
		t.Fatalf("decideRound = %+v: want a wait with time left on the scheduled round", out)
	}
	if bp := e.bpOf(r1); bp == nil || bp.ShipErr != "" || bp.ShipStop != "" || bp.ShipTries != 0 {
		t.Fatalf("the held ship counted as a failed try: %+v", bp)
	}
	if n := e.notified("pull request not opened"); n != 0 {
		t.Fatalf("the person was told %d times of a ship that only waits", n)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests", len(e.g.created))
	}
}

// R5, the second case: any other hold of a ship is a failed try with its text
// as the error, never a logged success.
func TestHeldShipThatWaitsOnNothingIsAFailedTry(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.doneRound(r1)
	e.exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r1)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || decideRound(in).Do != doShip {
		t.Fatalf("facts = %+v, %v: want a round that ships", in, err)
	}
	if err := e.s.heldOutcome(e.ctx, r1, true, "held for a reason that waits on nothing"); err == nil || !strings.Contains(err.Error(), "waits on nothing") {
		t.Fatalf("a reviewed hold that waits on nothing = %v", err)
	}
	if err := e.s.heldOutcome(e.ctx, r1, false, "held, unreviewed"); err == nil {
		t.Fatal("an unreviewed hold was a success")
	}
	e.exec(`INSERT INTO review_again_decisions(round_id, issue_id, audit_id, arm, next_round_id, created_at, round_index)
		SELECT id, issue_id, audit_id, 'again', id, ?, 1 FROM work_log WHERE id = ?`, e.s.nowMS(), r1)
	if err := e.s.heldOutcome(e.ctx, r1, true, "waits on the scheduled round"); err != nil {
		t.Fatalf("a hold on a scheduled round = %v", err)
	}
}

// R6: each text reviewAgain returns that promises Waypoint will act, or asks
// the session to, corresponds to an answer of decideRound that has a deadline.
func TestEveryHoldThatPromisesActionIsADeadline(t *testing.T) {
	type hold struct {
		name    string
		make    func(e *loopEnv, r int64)
		text    string
		answers int
	}
	for _, h := range []hold{
		{"the reviewer's audit is still going", func(e *loopEnv, r int64) {
			_, au := e.roundAuditRow(r)
			e.exec(`UPDATE code_audits SET status = 'running' WHERE id = ?`, au.ID)
		}, "Waypoint carries the round on", doWait},
		{"no review was recorded", func(e *loopEnv, r int64) { e.readNothing(r) }, "wp audit report", doTell},
		{"findings still to decide", func(e *loopEnv, r int64) {}, "still to decide", doTell},
		{"the check of the marks", func(e *loopEnv, r int64) { e.mark(r, "fixed") }, "checking your marks", doWait},
	} {
		t.Run(h.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r := e.review("Merge loses an update")
			h.make(e, r)
			if h.answers != doWait || h.name != "the check of the marks" {
				e.doneRound(r)
			}
			res, err := e.ship(e.task.Key, ShipAuto)
			if err != nil || !strings.Contains(res.Held, h.text) {
				t.Fatalf("held = %q, %v: want it to say %q", res.Held, err, h.text)
			}
			in, err := e.s.roundInOf(e.ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			out := decideRound(in)
			if out.Do != h.answers || out.For == "" || out.Until <= 0 {
				t.Fatalf("decideRound = %+v: want %d with something awaited and time left", out, h.answers)
			}
		})
	}
}

// The not-verified item: a round whose decision scheduled another is shipped
// at its limit with what it found carried, and only once; it was reviewed,
// so that is not a pull request opened with no review.
func TestSupersededRoundShipsCarriedOnce(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"fixed"})
	for _, h := range []time.Duration{time.Hour, 25 * time.Hour, 6 * time.Hour} {
		e.setClock(e.clockNow().Add(h))
		_, _ = e.s.advanceRound(e.ctx, r1)
		e.tickAll(time.Minute)
		e.s.Wait()
	}
	if n := len(e.g.created); n != 1 {
		t.Fatalf("opened %d pull requests, want the superseded round's one", n)
	}
	t.Logf("round %s, round 2 %s", e.round(r1).state, e.round(r1+1).state)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || in.Decisions == 0 {
		t.Fatalf("facts = %+v, %v: the round had no decision", in, err)
	}
	if n := e.notified("No review of this round was recorded"); n != 0 {
		t.Fatalf("a reviewed round was told as unreviewed (%d)", n)
	}
}

// The not-verified item: a round of an earlier run, taken up again by a
// resumed run (roundBeforeResume), with no review recorded is held like any,
// and nothing opens.
func TestResumedRoundWithNoReviewOpensNothing(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.stopWith("Reviewed; shipping later.")
	if r := e.round(r1); r.state != RoundDone {
		t.Fatalf("round 1 after its Stop: %+v", r)
	}
	if _, err := e.s.AgentPause(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.lapse(ClaimLease + time.Hour)
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.gateOpen(e.task.Key)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || res.Pull != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("the resumed run's ship = %+v, %v", res, err)
	}
	e.tickAll(2 * time.Minute)
	if _, err := e.s.advanceRound(e.ctx, r1); err != nil {
		t.Logf("advanceRound: %v", err)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests with no review", len(e.g.created))
	}
}

// A ship's question and the sweep give the same answer for the same facts: a
// round with findings still undecided is told to the session within the wait,
// and past it (or with no session) is released. A ship's facts always include
// a session (whoever ships is present), so reviewAgain asks with Session set.
func TestUndecidedAnswerIsTheSameForAShipAndTheSweep(t *testing.T) {
	for _, session := range []bool{true, false} {
		for _, since := range []time.Duration{0, heldScreenWait - time.Minute, heldScreenWait, heldScreenWait + time.Hour} {
			in := roundIn{State: RoundDone, Audit: AuditDone, Undecided: 2, Session: session, RunOpen: true, SinceChange: since}
			sweep := decideRound(in)
			in.ShipOpens = true
			ship := decideRound(in)
			wantTell := session && since < heldScreenWait
			want := doRelease
			if wantTell {
				want = doTell
			}
			if sweep.Do != want || ship.Do != want {
				t.Errorf("session %v, %v since the change: sweep %d, ship %d, want %d", session, since, sweep.Do, ship.Do, want)
			}
			if wantTell && (ship.Need != needMarks || ship.For == "" || ship.Until <= 0) {
				t.Errorf("a ship's tell: %+v", ship)
			}
			if !wantTell && (ship.Why == "" || ship.Why != sweep.Why) {
				t.Errorf("the release says %q (ship) and %q (sweep)", ship.Why, sweep.Why)
			}
		}
	}
}

// A ship is someone present: the round's own session being gone (roundInOf
// says Session false) doesn't release a ship inside the wait. reviewAgain asks
// as the session, so the answer is to show the findings, and the sweep's own
// answer for the same round stays a release.
func TestShipInsideTheWaitAsksEvenWithTheRoundsSessionGone(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := e.review("Merge loses an update")
	e.doneRound(r)
	e.endSession()
	e.setClock(e.clockNow().Add(time.Minute))
	in, err := e.s.roundInOf(e.ctx, r)
	if err != nil || in.Session || in.Undecided == 0 {
		t.Fatalf("facts = %+v, %v: want undecided findings and no session", in, err)
	}
	if out := decideRound(in); out.Do != doRelease {
		t.Fatalf("the sweep's answer = %+v: want a release", out)
	}
	in.ShipOpens, in.Session = true, true
	if out := decideRound(in); out.Do != doTell || out.Need != needMarks {
		t.Fatalf("the ship's answer = %+v: want the findings shown", out)
	}
}

// Past heldScreenWait the ship is not held for findings nobody decided: it
// opens the pull request as the sweep's release would, the findings are
// carried on the pull request and hold its merge, and the round's record says
// why it was released.
func TestShipPastTheWaitIsNotHeldForUndecidedFindings(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := e.review("Merge loses an update")
	e.stopWith("Reviewed; shipping later.")
	if _, err := e.s.AgentPause(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.lapse(heldScreenWait + time.Hour)
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.gateOpen(e.task.Key)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || strings.Contains(res.Held, "still to decide") || res.Pull == nil {
		t.Fatalf("ship = %+v, %v: want the pull request opened", res, err)
	}
	_, au := e.roundAuditRow(r)
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT coalesce(pr_id, 0) FROM code_audits WHERE id = ?`, au.ID).Scan(&prID); err != nil || prID != res.Pull.ID {
		t.Fatalf("the audit's pull request = %d (%v), want %d: the findings weren't carried", prID, err, res.Pull.ID)
	}
	reasons, _, err := e.s.auditHolds(e.ctx, res.Pull.ID, 0)
	if err != nil || !strings.Contains(strings.Join(reasons, "\n"), fmt.Sprintf("audit #%d", au.ID)) {
		t.Fatalf("the carried critical finding doesn't hold the merge: %v, %v", reasons, err)
	}
	var reason string
	if err := e.s.db.R.QueryRow(`SELECT reason FROM work_log WHERE id = ?`, r).Scan(&reason); err != nil || !strings.Contains(reason, "weren't decided") {
		t.Fatalf("the round's record says %q (%v), want why it was released", reason, err)
	}
}

// An env started from a snapshot is the env a build makes: the snapshot's
// rows in every table, as many as a build's, the same state in what the
// test keeps in memory and in the fakes, nothing a build leaves in the
// Service's memory missing but what loopSnapshot names, and the same
// answer to what a cell does next. Envs started from one snapshot don't
// share state with each other or with its source.
func TestLoopSnapshotEnvMatchesABuild(t *testing.T) {
	build := func(t *testing.T) *loopEnv {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		e.review("Merge loses an update")
		e.s.Wait()
		e.s.quiesceGoals(t)
		return e
	}
	// rows is every table's rows, in an order of their own.
	rows := func(e *loopEnv) map[string][]string {
		out := map[string][]string{}
		names, err := e.s.db.R.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		for names.Next() {
			var n string
			if err := names.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[n] = nil
		}
		if err := errors.Join(names.Err(), names.Close()); err != nil {
			t.Fatal(err)
		}
		for n := range out {
			rs, err := e.s.db.R.Query(`SELECT * FROM "` + n + `"`)
			if err != nil {
				t.Fatal(err)
			}
			cols, err := rs.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rs.Next() {
				vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rs.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				out[n] = append(out[n], fmt.Sprintf("%#v", vals))
			}
			if err := errors.Join(rs.Err(), rs.Close()); err != nil {
				t.Fatal(err)
			}
			slices.Sort(out[n])
		}
		return out
	}
	counts := func(rows map[string][]string) string {
		var out []string
		for _, n := range slices.Sorted(maps.Keys(rows)) {
			out = append(out, fmt.Sprintf("%s=%d", n, len(rows[n])))
		}
		return strings.Join(out, " ")
	}
	memory := func(e *loopEnv) string {
		sl := e.s.LLM.(*gatewayScoringLLM)
		who := func(a *Actor) string {
			return fmt.Sprintf("%d/%s/admin=%v agent=%q token=%d session=%d scope=%v", a.User.ID, a.User.Username, a.User.IsAdmin, a.Agent, a.TokenID, a.Session, a.Scope)
		}
		return fmt.Sprintf("clock=%v session=%d cli=%s admin=%s ag=%s project=%+v jobs=%+v res=%+v box=%+v task=%s %q %+v\nforge %v heads=%v\ngateway %v\ndecisions %v choice=%v states=%v",
			e.clockNow(), e.session, e.cli, who(e.admin), who(e.ag), *e.p, e.jobs, e.res, *e.box, e.task.Key, e.task.Title, e.task.Status,
			setFields(e.g), e.g.heads, setFields(sl.taskLLM), setFields(e.j), e.j.choice, e.j.states)
	}
	// next is what a cell does: another ship, then the server's move.
	next := func(e *loopEnv) string {
		sl := e.s.LLM.(*gatewayScoringLLM)
		round := e.round(1)
		res, err := e.ship(e.task.Key, ShipAuto)
		e.tick(2 * time.Minute)
		out, aerr := e.s.advanceRound(e.ctx, round.id)
		e.s.Wait()
		return fmt.Sprintf("%+v %v | %+v %v | jobs=%d pulls=%d limitsCalls=%d", res, err, out, aerr, len(e.jobs), len(e.g.created), sl.limitsCalls)
	}

	// Each env is read and run while its job catcher is the one in force:
	// once a later env exists, a job an earlier env's Service enqueues lands
	// in the later env's list.
	fresh := build(t)
	freshRows, freshMemory, freshService := counts(rows(fresh)), memory(fresh), setFields(fresh.s)
	freshNext := next(fresh)

	src := build(t)
	snap := snapshotLoopEnv(t, src)
	srcRows, srcMemory := rows(src), memory(src)
	cloned := snap.start(t)
	clonedRows := rows(cloned)
	for _, n := range slices.Sorted(maps.Keys(srcRows)) {
		if !slices.Equal(srcRows[n], clonedRows[n]) {
			t.Errorf("%s's rows differ from the snapshot's source:\nsource %v\ncloned %v", n, srcRows[n], clonedRows[n])
		}
	}
	if c := counts(clonedRows); c != freshRows {
		t.Errorf("row counts differ:\nbuilt  %s\ncloned %s", freshRows, c)
	}
	clonedMemory := memory(cloned)
	if clonedMemory != freshMemory {
		t.Errorf("memory differs:\nbuilt  %s\ncloned %s", freshMemory, clonedMemory)
	}
	if !reflect.DeepEqual(cloned.task, src.task) || !reflect.DeepEqual(cloned.p, src.p) || !reflect.DeepEqual(cloned.box, src.box) ||
		!reflect.DeepEqual(cloned.admin, src.admin) || !reflect.DeepEqual(cloned.ag, src.ag) {
		t.Errorf("a started env's task, project, runner or actors aren't the source's:\nsource %+v %+v %+v %+v %+v\ncloned %+v %+v %+v %+v %+v",
			src.task, src.p, src.box, src.admin, src.ag, cloned.task, cloned.p, cloned.box, cloned.admin, cloned.ag)
	}
	held := func(e *loopEnv) []any { return []any{e.admin, e.ag, e.p, e.box, e.task, e.jobs, e.res, e.g.heads, e.j} }
	if shared := sharedRefs(held(cloned), held(src)); len(shared) > 0 {
		t.Errorf("a started env shares %v with its source", shared)
	}
	// What the Service keeps in memory, as setFields reads it: a build and
	// a started env differ only in what loopSnapshot says a started env
	// makes again. The three caches are named whole: each checks itself
	// against the database before it is used.
	remade := []string{".similar.", ".terms.", ".graph.", ".goals.checked", ".goals.judgeGen", ".auditLocks.m", ".branchLocks.m",
		".jobs.jobs", ".jobs.problems", ".jobs.perSrc", ".jobs.hours"}
	for _, d := range fieldDiffs(freshService, setFields(cloned.s), remade) {
		t.Errorf("the Service's %s: carry it in start, or name it in loopSnapshot and here", d)
	}
	if _, ok := freshService[".gatewayLimits"]; !ok {
		t.Errorf("the build no longer caches the gateway's limits, so this test doesn't show start carrying them: %v", freshService)
	}
	if c := next(cloned); c != freshNext {
		t.Errorf("the next step differs:\nbuilt  %s\ncloned %s", freshNext, c)
	}

	// Running one started env, and writing through what it holds, leaves the
	// source and the next env from the snapshot as they were.
	cloned.task.Title, cloned.box.Features[0], cloned.p.Name, cloned.ag.User.Username = "changed", "changed", "changed", "changed"
	cloned.g.heads["changed"], cloned.j.choice["changed"] = "changed", "changed"
	if m := memory(src); m != srcMemory || src.task.Title == "changed" {
		t.Errorf("a started env changed its source:\nbefore %s\nafter  %s (task %q)", srcMemory, m, src.task.Title)
	}
	again := snap.start(t)
	if m := memory(again); m != clonedMemory || again.task.Title == "changed" {
		t.Errorf("a second env from the snapshot isn't the first:\nfirst  %s\nsecond %s (task %q)", clonedMemory, m, again.task.Title)
	}
	if r := rows(again); !maps.EqualFunc(r, srcRows, slices.Equal[[]string]) {
		t.Errorf("a second env from the snapshot doesn't have its rows: %s, want %s", counts(r), counts(srcRows))
	}
}

// setFields is what x holds, by field path: each number, string and bool
// that isn't zero, how many entries each map, slice and sync.Map has (not
// what they are), whether each time and func is set, and the type each
// interface holds. It follows this package's structs and arrays, by value
// or by pointer, pointers to anything else it records, and atomic values.
// It doesn't see into channels, interfaces' values, atomic pointers or
// other packages' types.
func setFields(x any) map[string]string {
	out := map[string]string{}
	seen := map[uintptr]bool{}
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			walk(v.Elem(), path)
		case reflect.Struct:
			if v.Type() == reflect.TypeFor[sync.Map]() {
				n := 0
				// #nosec G103 -- a test reads an unexported sync.Map in place
				(*sync.Map)(unsafe.Pointer(v.UnsafeAddr())).Range(func(_, _ any) bool { n++; return true })
				if n > 0 {
					out[path] = strconv.Itoa(n)
				}
				return
			}
			if v.Type() == reflect.TypeFor[time.Time]() {
				if !v.IsZero() {
					out[path] = "set"
				}
				return
			}
			if pkg := v.Type().PkgPath(); !isCorePkg(pkg) && pkg != "sync/atomic" {
				return
			}
			for i := range v.NumField() {
				walk(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		case reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), path+"["+strconv.Itoa(i)+"]")
			}
		case reflect.Map, reflect.Slice:
			if v.Len() > 0 {
				out[path] = strconv.Itoa(v.Len())
			}
		case reflect.Func:
			if !v.IsNil() {
				out[path] = "set"
			}
		case reflect.Interface:
			if !v.IsNil() {
				out[path] = v.Elem().Type().String()
			}
		case reflect.Chan, reflect.UnsafePointer, reflect.Invalid:
		default: // a bool, a string or a number of any kind
			if !v.IsZero() {
				out[path] = fmt.Sprint(v)
			}
		}
	}
	walk(reflect.ValueOf(x), "")
	return out
}

// setFields reads every kind of number, arrays and pointers to values, and
// fieldDiffs reports a field only one side holds, whichever side it is.
func TestSetFieldsAndFieldDiffs(t *testing.T) {
	n := 7
	type held struct {
		i8   int8
		u16  uint16
		f32  float32
		c    complex64
		p    *int
		arr  [2]uint8
		err  error
		l    LLM
		when time.Time
		m    map[string]int
		zero int
	}
	got := setFields(&held{i8: -1, u16: 2, f32: 1.5, c: 1, p: &n, arr: [2]uint8{0, 3}, err: errors.New("x"), l: &taskLLM{}, when: time.Unix(1, 0), m: map[string]int{"a": 1}, zero: 0})
	want := map[string]string{".i8": "-1", ".u16": "2", ".f32": "1.5", ".c": "(1+0i)", ".p": "7", ".arr[1]": "3", ".err": "*errors.errorString", ".l": "*core.taskLLM", ".when": "set", ".m": "1"}
	if !maps.Equal(got, want) {
		t.Errorf("setFields = %v, want %v", got, want)
	}
	built := map[string]string{".a": "1", ".b": "2", ".same": "x", ".remade": "3", ".cache.x": "6", ".cached": "8"}
	started := map[string]string{".b": "3", ".cached": "9", ".same": "x", ".alsoRemade": "5", ".cache.y": "7", ".z": "1"}
	diffs := fieldDiffs(built, started, []string{".remade", ".alsoRemade", ".cache."})
	if len(diffs) != 4 || !strings.HasPrefix(diffs[0], ".a:") || !strings.HasPrefix(diffs[1], ".b:") || !strings.HasPrefix(diffs[2], ".cached:") || !strings.HasPrefix(diffs[3], ".z:") {
		t.Errorf("fieldDiffs = %q, want .a (only the build's), .b (differs), .cached (not under .cache.) and .z (only the started env's)", diffs)
	}
}

// A deep copy equals what it copies and shares none of it: not a
// permission set, an automation, a budget, a scope, nor anything inside an
// issue or a project.
func TestDeepCopySharesNothing(t *testing.T) {
	budget, parent := 3, int64(9)
	a := &Actor{User: &User{ID: 1, Username: "ann"}, Permissions: PermissionSet{PermIssuesRead: true}, Automation: &AutomationRef{ID: 4},
		Scope: map[int64]int{1: 2}, Budget: &budget}
	iss := &Issue{Key: "T-1", ParentID: &parent, Labels: []Label{{Name: "bug"}}, Blocks: []string{"T-2"}}
	task := &IssueDetail{Issue: iss, Children: []*Issue{iss, {Key: "T-3"}}}
	p := &Project{Key: "T", Statuses: []Status{{Name: "To Do"}}, Sprints: []*Sprint{{ID: 5}}}
	src := []any{a, task, p}
	c := deepCopy(src)
	if !reflect.DeepEqual(c, src) {
		t.Fatalf("the copy differs:\ncopy   %+v\nsource %+v", c, src)
	}
	if shared := sharedRefs(c, src); len(shared) > 0 {
		t.Errorf("the copy shares %v with its source", shared)
	}
	if len(refs(src)) < 14 {
		t.Errorf("refs sees %d places in the source, want the 14 or more it holds: %v", len(refs(src)), refs(src))
	}
	ca, ct, cp := c[0].(*Actor), c[1].(*IssueDetail), c[2].(*Project)
	if ct.Children[0] != ct.Issue {
		t.Error("the task's own issue, held twice, was copied to two issues")
	}
	ca.Permissions[PermIssuesRead], ca.Automation.ID, ca.Scope[1], *ca.Budget, ca.User.Username = false, 0, 0, 0, "changed"
	*ct.ParentID, ct.Labels[0].Name, ct.Blocks[0], ct.Children[1].Key = 0, "changed", "changed", "changed"
	cp.Statuses[0].Name, cp.Sprints[0].ID = "changed", 0
	if !a.Permissions[PermIssuesRead] || a.Automation.ID != 4 || a.Scope[1] != 2 || budget != 3 || a.User.Username != "ann" ||
		parent != 9 || iss.Labels[0].Name != "bug" || iss.Blocks[0] != "T-2" || task.Children[1].Key != "T-3" ||
		p.Statuses[0].Name != "To Do" || p.Sprints[0].ID != 5 {
		t.Errorf("writing through the copy changed the source: %+v %+v %+v %+v", a, iss, task.Children[1], p)
	}
	if shared := sharedRefs([]any{a, a.Permissions}, a); len(shared) == 0 {
		t.Error("sharedRefs doesn't report a value's own pointers as shared")
	}
}
