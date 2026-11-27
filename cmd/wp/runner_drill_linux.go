package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/probe"
)

// drillOutputMax bounds what a session's stdout can make the runner hold.
const drillOutputMax = 1 << 20

func (p *runnerProc) execSandboxDrill(ctx context.Context, run *jobRun, job *claimedJob) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	if len(job.Sessions) != 3 {
		return failed("Waypoint sent no start specs for the drill's sessions: the machine's sandbox helper wasn't ready, or its sandboxing is paused")
	}
	ctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()

	plant := "waypoint-drill-" + strconv.FormatInt(job.ID, 10) + "-" + randHex(6)
	hostShm := plant + "-host"
	shmPath := filepath.Join("/dev/shm", hostShm)
	if err := os.WriteFile(shmPath, []byte("waypoint drill\n"), 0o600); err != nil { // #nosec G306 -- the drill's own marker
		say("Couldn't plant %s (%v): the drill doesn't try the host's /dev/shm", shmPath, err)
		hostShm = ""
	} else {
		defer func() { _ = os.Remove(shmPath) }()
	}
	home, _ := os.UserHomeDir()
	targets := probe.Plan(probe.LocalHost(home, probe.WaypointPort, localAddrs()), plant, hostShm)
	for _, a := range serverAddrs(ctx, p.c.server()) {
		if !slices.Contains(targets.Dial, a) {
			targets.Dial = append(targets.Dial, a)
		}
	}
	input, err := json.Marshal(targets)
	if err != nil {
		return failed("drill targets: %v", err)
	}

	var cases []probe.Case
	say("Probe: %d files, %d directories, %d sockets, %d addresses, %d requests through the broker",
		len(targets.Read), len(targets.List), len(targets.Sockets), len(targets.Dial), len(targets.Egress))
	out, _, err := p.drillSession(ctx, run, job.Sessions[0], input)
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), drillOutputMax)
	for sc.Scan() {
		var c probe.Case
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.Name != "" && c.Want != "" {
			cases = append(cases, c)
		}
	}
	if serr := sc.Err(); serr != nil && err == nil {
		err = fmt.Errorf("reading the probe's output: %w", serr)
	}
	if err != nil {
		if len(cases) == 0 {
			return failed("The probe session didn't run: %v", err)
		}
		say("The probe session: %v", err)
		cases = append(cases, probe.Case{Name: "probe session", Group: probe.GroupFiles, Want: probe.Refused, Got: probe.Inconclusive, Detail: fmt.Sprint(err)})
	}
	cases = append(cases, probe.JudgePlant(plant, func(p string) bool { _, err := os.Lstat(p); return err == nil })...)
	for _, dir := range []string{"/tmp", "/dev/shm"} {
		_ = os.Remove(filepath.Join(dir, plant)) // #nosec G703 -- only if the probe got it there
	}

	say("Fork session: %d processes, TasksMax=%d", probe.ForkTries, probe.ForkResources.Tasks)
	out, errOut, err := p.drillSession(ctx, run, job.Sessions[1], nil)
	cases = append(cases, probe.JudgeFork(string(out)+"\n"+string(errOut), err))
	say("Time session: sleeps %ds, RuntimeMaxSec=%d", probe.TimeSleep, probe.TimeResources.RuntimeSec)
	began := time.Now()
	out, _, err = p.drillSession(ctx, run, job.Sessions[2], nil)
	cases = append(cases, probe.JudgeTime(string(out), time.Since(began), err))
	if ctx.Err() != nil {
		return outcome{State: "failed", Error: fmt.Sprintf("The drill timed out after %s and was stopped.", run.timeout.Round(time.Second)), Result: probe.Result{Cases: cases}}
	}

	s := probe.Summarize(cases)
	say("%d tries: %d as they should be, %d got through, %d controls refused, %d couldn't be judged", len(cases), s.Passed, s.Escaped, s.Controls, s.Inconclusive)
	for _, c := range cases {
		if !c.Passed() {
			say("  %s: %s, want %s (%s)", c.Name, c.Got, c.Want, c.Detail)
		}
	}
	return outcome{State: "done", Result: probe.Result{Cases: cases}}
}

// drillSession runs one Contained session to its end with input on its
// stdin (nothing when nil), and answers its stdout and stderr; the stderr
// also goes to the job's log.
func (p *runnerProc) drillSession(ctx context.Context, run *jobRun, sd *sandbox.Signed, input []byte) (stdout, stderr []byte, err error) {
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	inR, inW, err := pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := pipe()
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, nil, err
	}
	go func() {
		_, _ = inW.Write(input)
		_ = inW.Close()
	}()
	var out capBuffer
	out.max = drillOutputMax
	errOut := capBuffer{w: run.out, max: 64 << 10}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errOut, errR) }()

	_, err = sandbox.Start(ctx, p.sandboxSocket(), sd, inR, outW, errW)
	_ = outW.Close()
	_ = errW.Close()
	_ = inR.Close()
	deadline := time.Now().Add(10 * time.Second)
	_ = outR.SetReadDeadline(deadline)
	_ = errR.SetReadDeadline(deadline)
	wg.Wait()
	if out.cut && err == nil {
		err = fmt.Errorf("the session's output passed %d bytes and the rest was dropped", drillOutputMax)
	}
	return out.buf.Bytes(), errOut.buf.Bytes(), err
}

// capBuffer keeps up to max bytes in its buffer, copied to w when set, and
// drops the rest while still taking it, so a session never blocks on a full
// pipe.
type capBuffer struct {
	w   io.Writer
	buf bytes.Buffer
	max int
	n   int
	cut bool
}

func (c *capBuffer) Write(b []byte) (int, error) {
	if len(b) > c.max-c.n {
		c.cut = true
	}
	if keep := min(len(b), c.max-c.n); keep > 0 {
		c.n += keep
		c.buf.Write(b[:keep])
		if c.w != nil {
			_, _ = c.w.Write(b[:keep])
		}
	}
	return len(b), nil
}

// localAddrs are the machine's own addresses a session must not reach:
// every non-loopback, non-link-local one.
func localAddrs() []string {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range as {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() || n.IP.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, n.IP.String())
	}
	return out
}

// serverAddrs are the Waypoint server's addresses as this runner reaches
// it, which a session must not reach directly.
func serverAddrs(ctx context.Context, server string) []string {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[u.Scheme == "https"]
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, u.Hostname())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, port))
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
