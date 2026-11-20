//go:build linux

// waypoint-sandboxd is Waypoint's root sandbox helper (WP-418, WP-ADR28,
// PL7 §5.3). wp sandbox install sets it up as a systemd service; it
// answers only the runner's user, on a unix socket, and changes the
// machine only for specs the Waypoint server signed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/broker"
	"git.jmann.me/claude/waypoint/internal/sandbox/forward"
	"git.jmann.me/claude/waypoint/internal/sandbox/leftover"
	"git.jmann.me/claude/waypoint/internal/sandbox/probe"
	"git.jmann.me/claude/waypoint/internal/sandbox/sandboxd"
)

var version = "dev"

func main() {
	err := func() error {
		// What the helper runs in sessions' units (WP-470): the same
		// binary, so a session sees nothing of the machine's but /usr.
		if len(os.Args) > 1 {
			switch os.Args[1] {
			case "broker":
				return runBroker(os.Args[2:])
			case "forward":
				return runForward(os.Args[2:])
			case "clean":
				return runClean(os.Args[2:])
			case "leftover":
				return runLeftover(os.Args[2:])
			case "probe":
				return runProbe(os.Args[2:])
			}
		}
		return run()
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waypoint-sandboxd:", err)
		os.Exit(1)
	}
}

// runProbe is the escape drill inside a Contained session (WP-473): it
// reads its targets as JSON on stdin and writes each try's case as a JSON
// line. --fork and --sleep are the resource sessions; --hold is a child
// that waits for stdin to close.
func runProbe(args []string) error {
	set := flag.NewFlagSet("probe", flag.ContinueOnError)
	fork := set.Int("fork", 0, "start this many processes")
	sleep := set.Int("sleep", 0, "sleep this many seconds")
	hold := set.Bool("hold", false, "wait for stdin to close")
	if err := set.Parse(args); err != nil {
		return err
	}
	switch {
	case *hold:
		probe.Hold(os.Stdin)
		return nil
	case *fork > 0:
		return probe.Fork(*fork, os.Stdout)
	case *sleep > 0:
		probe.Sleep(*sleep, os.Stdout)
		return nil
	}
	var t probe.Targets
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&t); err != nil {
		return fmt.Errorf("probe targets: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	probe.Run(ctx, t, os.Stdout)
	return nil
}

// runBroker is a session's broker: wp-egress, in its own unit, with the
// sandbox CA as the unit's credentials. Each request it decides is logged
// as a JSON line to the journal.
func runBroker(args []string) error {
	set := flag.NewFlagSet("broker", flag.ContinueOnError)
	policyFile := set.String("policy", "", "the session's egress policy")
	socket := set.String("socket", "", "the unix socket to listen on")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *policyFile == "" || *socket == "" {
		return errors.New("broker needs --policy and --socket")
	}
	creds := os.Getenv("CREDENTIALS_DIRECTORY")
	if creds == "" {
		return errors.New("the broker's CA comes as unit credentials (LoadCredential=ca.pem, ca-key.pem)")
	}
	certPEM, err := os.ReadFile(filepath.Join(creds, "ca.pem")) // #nosec G304 G703 -- systemd's credentials directory
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(creds, "ca-key.pem")) // #nosec G304 G703 -- systemd's credentials directory
	if err != nil {
		return err
	}
	ca, err := broker.LoadCA(certPEM, keyPEM)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*policyFile) // #nosec G304 -- the helper's policy file
	if err != nil {
		return err
	}
	policy, err := broker.ParsePolicy(raw)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	b, err := broker.New(broker.Config{Policy: policy, CA: ca, Log: func(e broker.Entry) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	}, OnLimit: func(limit string) {
		fmt.Fprintln(os.Stderr, "the session reached its", limit, "limit: refusing the rest")
	}})
	if err != nil {
		return err
	}
	if err := os.Remove(*socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(*socket, 0o660); err != nil { // #nosec G302 -- the session's group connects
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; _ = l.Close() }()
	return b.Serve(l)
}

// runForward is the first process in a session's unit: the loopback
// proxy and the session's command (internal/sandbox/forward).
func runForward(args []string) error {
	set := flag.NewFlagSet("forward", flag.ContinueOnError)
	launch := set.String("launch", forward.LaunchFile, "the session's launch file")
	if err := set.Parse(args); err != nil {
		return err
	}
	l, err := forward.Load(*launch)
	if err != nil {
		return err
	}
	code, err := forward.Run(l)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waypoint-sandboxd:", err)
	}
	os.Exit(code)
	return nil
}

// runClean empties directories as the session's user.
func runClean(dirs []string) error {
	var errs []error
	for _, d := range dirs {
		errs = append(errs, sandboxd.CleanContents(d))
	}
	return errors.Join(errs...)
}

// runLeftover is a job's last step, run in its session's unit as the repo
// user: it commits what the agent left and writes the job's commits to
// standard output as a bundle, for the runner to import on the host
// (WP-471, PL7 §8.1). No commits after the base write nothing.
func runLeftover(args []string) error {
	set := flag.NewFlagSet("leftover", flag.ContinueOnError)
	base := set.String("base", "", "the commit the job started from")
	message := set.String("message", "", "the leftover commit's message (default \""+leftover.DefaultMessage+"\")")
	dir := set.String("dir", ".", "the job's work tree")
	if err := set.Parse(args); err != nil {
		return err
	}
	res, err := leftover.Bundle(context.Background(), leftover.Options{Dir: *dir, Base: *base, Message: *message}, os.Stdout)
	if err != nil {
		return err
	}
	if res.Committed {
		fmt.Fprintln(os.Stderr, "Committed the changes the agent left")
	}
	fmt.Fprintf(os.Stderr, "%d commit(s) after %.12s\n", res.Commits, *base)
	return nil
}

// toolchains is a repeatable flag.
type toolchains []string

func (t *toolchains) String() string     { return strings.Join(*t, ",") }
func (t *toolchains) Set(v string) error { *t = append(*t, v); return nil }

func run() error {
	socket := flag.String("socket", sandbox.Socket, "the unix socket to listen on")
	store := flag.String("store", sandbox.StoreDir, "the sandbox store (root-owned, 0755)")
	state := flag.String("state", sandbox.StateDir, "the helper's state directory (root-owned, 0700)")
	keyFile := flag.String("key", sandbox.KeyFile, "the Waypoint server's pinned spec key")
	runnerUID := flag.Int("runner-uid", -1, "the uid of the user wp runner runs as: the only one (besides root) the socket answers")
	runnerID := flag.Int64("runner-id", 0, "if set, accept only specs for this Waypoint runner")
	repin := flag.Bool("repin-store", false, "accept a store directory other than the one pinned (after moving it on purpose)")
	showVersion := flag.Bool("version", false, "print the version")
	var tools toolchains
	flag.Var(&tools, "toolchain", "a directory every Contained session sees read-only, its bin/ on PATH (repeatable; root's or the runner's, not writable by others)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *runnerUID <= 0 {
		return errors.New("--runner-uid is required (and isn't root's)")
	}
	if os.Geteuid() != 0 {
		return errors.New("runs as root (it makes the machine's sandbox users)")
	}
	text, err := os.ReadFile(*keyFile) // #nosec G304 -- the operator's flag
	if err != nil {
		return fmt.Errorf("the pinned key: %w", err)
	}
	key, err := sandbox.ParseKey(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", *keyFile, err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	h := sandboxd.New(sandboxd.Config{
		Version: version, Key: key, RunnerUID: *runnerUID, RunnerID: *runnerID,
		StorePath: *store, StatePath: *state, StoreOwner: 0, RepinStore: *repin, Toolchains: tools,
	}, sandboxd.OS{}, log)
	if err := h.Open(); err != nil {
		return err
	}
	defer h.Close()

	// A socket left by a crash goes; anything else at that path stays.
	if fi, err := os.Lstat(*socket); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return fmt.Errorf("%s exists and isn't a socket", *socket)
		}
		if err := os.Remove(*socket); err != nil {
			return err
		}
	}
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", *socket)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	if err := os.Chown(*socket, *runnerUID, 0); err != nil {
		return err
	}
	if err := os.Chmod(*socket, 0o600); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; _ = l.Close() }()
	log.Info("listening", "socket", *socket, "key", sandbox.Fingerprint(key), "runner_uid", *runnerUID, "store", *store, "version", version)
	return h.Serve(l)
}
