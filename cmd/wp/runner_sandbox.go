package main

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// The runner and the machine's sandbox helper (WP-418, WP-ADR28, PL7
// §5.3). The runner tells Waypoint what the helper says, so the server
// knows whether to sign job specs for this machine, and hands the helper
// each job's spec before the job starts. Phase 1 makes the job's machine
// users and nothing more: a refusal is logged and the job runs as before.
// It also passes on the actions a person asks for in Settings → Runners →
// Sandbox (WP-448): remove a machine user, finish interrupted work.

// sandboxHelperSocket is where the host's helper listens; tests point it
// elsewhere so they never ask the real one (WP-1065).
var sandboxHelperSocket = sandbox.Socket

// sandboxCheckEvery: how often the runner re-asks the helper while idle.
const sandboxCheckEvery = 10 * time.Minute

// sandboxState is what the runner last heard from the helper.
type sandboxState struct {
	socket string // sandboxHelperSocket; tests use their own
	last   []byte // the last report, as sent
	report *sandbox.Report
	dirty  bool
	// done says how helper actions went, for the next poll (WP-448).
	done []sandboxDone
	// running are the actions being run, so a resent one isn't run twice.
	running map[int64]bool
}

// sandboxOp is a helper action a person asked for in Waypoint, with its
// signed spec (WP-448).
type sandboxOp struct {
	ID   int64           `json:"id"`
	Op   string          `json:"op"`
	Spec *sandbox.Signed `json:"spec"`
}

type sandboxDone struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

// sandboxReport asks the helper how it is. Off Linux there's no helper and
// no report.
func sandboxReport(ctx context.Context, socket string) *sandbox.Report {
	if runtime.GOOS != "linux" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := sandbox.Call(ctx, socket, sandbox.Request{Op: sandbox.OpStatus})
	switch {
	case errors.Is(err, sandbox.ErrNotInstalled):
		return &sandbox.Report{}
	case err != nil:
		return &sandbox.Report{Installed: true, Error: err.Error()}
	case resp.Status == nil:
		return &sandbox.Report{Installed: true, Error: "the helper sent no status"}
	}
	return &sandbox.Report{Installed: true, Status: resp.Status}
}

// checkSandbox refreshes the report, marking it for the next poll when it
// changed.
func (p *runnerProc) checkSandbox(ctx context.Context) {
	rep := sandboxReport(ctx, p.sandboxSocket())
	if rep == nil {
		return
	}
	b, _ := json.Marshal(rep)
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(b) == string(p.sb.last) {
		return
	}
	first := p.sb.last == nil
	p.sb.last, p.sb.report, p.sb.dirty = b, rep, true
	switch {
	case !rep.Installed:
		if first {
			p.logf("Sandbox helper: not installed (wp sandbox install)")
		}
	case rep.Error != "":
		p.logf("Sandbox helper: %s", rep.Error)
	case first || !rep.Status.Store.OK:
		store := "store ready"
		if !rep.Status.Store.OK {
			store = "store: " + rep.Status.Store.Problem
		}
		p.logf("Sandbox helper %s: key %s, %s, %d machine users", rep.Status.Version, rep.Status.Key, store, len(rep.Status.Users))
	}
}

func (p *runnerProc) sandboxSocket() string {
	if p.sb.socket != "" {
		return p.sb.socket
	}
	return sandboxHelperSocket
}

// watchSandbox re-asks the helper now and then.
func (p *runnerProc) watchSandbox(ctx context.Context) {
	t := time.NewTicker(sandboxCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.checkSandbox(ctx)
		}
	}
}

// takeSandboxReport is the report for a poll, if it has news; failed puts
// it back when the poll didn't get through.
func (p *runnerProc) takeSandboxReport() (rep *sandbox.Report, failed func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.sb.dirty {
		return nil, func() {}
	}
	p.sb.dirty = false
	return p.sb.report, func() {
		p.mu.Lock()
		p.sb.dirty = true
		p.mu.Unlock()
	}
}

// ensureSandboxUsers hands a job's spec to the helper, which makes the
// repo's machine users if they're missing.
func (p *runnerProc) ensureSandboxUsers(ctx context.Context, job *claimedJob) {
	if job.Sandbox == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := sandbox.Call(cctx, p.sandboxSocket(), sandbox.Request{Op: sandbox.OpEnsureUsers, Spec: job.Sandbox})
	if err != nil {
		p.logf("Job %d: the sandbox helper didn't make its users (the job runs unsandboxed): %v", job.ID, err)
	} else {
		names := make([]string, 0, len(resp.Users))
		for _, u := range resp.Users {
			names = append(names, u.Name)
		}
		p.logf("Job %d: sandbox users %s ready", job.ID, strings.Join(names, ", "))
	}
	p.checkSandbox(ctx) // its users changed
}

// runSandboxOps hands the helper the actions a person asked for, one at a
// time, and keeps how each went for the next poll.
func (p *runnerProc) runSandboxOps(ctx context.Context, ops []sandboxOp) {
	for _, op := range ops {
		p.mu.Lock()
		if p.sb.running == nil {
			p.sb.running = map[int64]bool{}
		}
		busy := p.sb.running[op.ID]
		p.sb.running[op.ID] = true
		p.mu.Unlock()
		if busy {
			continue
		}
		msg := ""
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		resp, err := sandbox.Call(cctx, p.sandboxSocket(), sandbox.Request{Op: op.Op, Spec: op.Spec})
		cancel()
		switch {
		case err != nil:
			msg = err.Error()
			p.logf("Sandbox %s (asked in Waypoint) failed: %v", op.Op, err)
		case op.Op == sandbox.OpRemoveUser && len(resp.Users) > 0:
			p.logf("Sandbox: removed machine user %s (asked in Waypoint)", resp.Users[0].Name)
		default:
			p.logf("Sandbox %s (asked in Waypoint) done", op.Op)
		}
		p.mu.Lock()
		p.sb.done = append(p.sb.done, sandboxDone{ID: op.ID, Error: msg})
		delete(p.sb.running, op.ID)
		p.mu.Unlock()
	}
	p.checkSandbox(ctx) // its users changed
}

// takeSandboxDone is what a poll says of finished actions; failed puts
// them back when the poll didn't get through.
func (p *runnerProc) takeSandboxDone() (done []sandboxDone, failed func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	done, p.sb.done = p.sb.done, nil
	return done, func() {
		p.mu.Lock()
		p.sb.done = append(done, p.sb.done...)
		p.mu.Unlock()
	}
}
