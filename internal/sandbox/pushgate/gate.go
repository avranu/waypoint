// Package pushgate is a sandbox session's push gateway (WP-469, WP-ADR28,
// PL7 §7.2). The session pushes over HTTPS to its broker, which hands git
// receive-pack here instead of to the forge. The gateway:
//
//  1. receives the pack into a quarantine next to its own mirror of the
//     repo: a host-owned bare repository the sandbox can never write, run
//     with a git that reads no config, hooks or attributes it didn't write;
//  2. checks each ref update: branches only, never the default branch, tags
//     or release branches; force updates and deletions only of branches
//     this session created (or was given); only the session's own repos;
//  3. checks every new commit's files against the protected-path globs,
//     listed with diff-tree -r --no-renames so a file renamed out of a
//     protected path counts (WP-415), and every new object against the
//     machine's known secret values;
//  4. pushes what passes to the forge with that repo's own deploy key,
//     which only this session's broker holds, with a lease on each ref.
//
// A push that touches protected paths is held, not refused, when a Hold
// callback is set: the objects stay in the gateway's store with the diff,
// a person sees it as a request, and Forward sends it on if they approve.
// A push is all or nothing: one refused ref refuses the rest.
package pushgate

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultMaxPack is the largest pack a push may send (PL7 §7.2).
const DefaultMaxPack int64 = 2 << 30

// Limits on one push, past which it is refused rather than checked.
const (
	maxCommands   = 100
	maxNewCommits = 5000
	maxNewObjects = 500000
	maxDiff       = 256 << 10
)

// Repo is one repository the session may push to.
type Repo struct {
	// Path is "owner/name" as the session's git URL names it.
	Path string `json:"path"`
	// Remote is where the mirror fetches from and pushes to:
	// ssh://user@host[:port]/owner/name.git, with KeyFile as the identity.
	// An absolute path is a local repository (tests).
	Remote string `json:"remote"`
	// KeyFile is the repo's deploy key; KnownHosts the forge's host key.
	KeyFile    string `json:"key_file,omitempty"`
	KnownHosts string `json:"known_hosts,omitempty"`
	// DefaultBranch is the branch the session may never push to, e.g.
	// "main". The forge's HEAD, read on each fetch, is refused as well.
	DefaultBranch string `json:"default_branch"`
	// PushDefault lets the session push the default branch (the project's
	// re-auth "push to main" setting, off by default). Force updates and
	// deletions of it are still refused.
	PushDefault bool `json:"push_default,omitempty"`
	// Branches are branches the session owns from the start (a pull
	// request's head it was given): it may force-push and delete them.
	Branches []string `json:"branches,omitempty"`
}

// Config is what a gateway runs with.
type Config struct {
	// Root holds the mirrors, incoming pushes and held pushes. It must be
	// host-owned and never mounted into a sandbox.
	Root  string
	Repos []Repo
	// Protect are the protected-path globs (WP-D14): "**/" is any
	// directories, "**" anything, "*" and "?" stay within one segment.
	Protect []string
	// Secrets are this machine's credentials and tokens by name. A push
	// carrying any of their values, byte for byte, is refused. Values
	// shorter than MinSecret are ignored: they'd match by chance.
	Secrets map[string]string
	// MaxPack bounds a push's pack; zero means DefaultMaxPack.
	MaxPack int64
	// Hold is told about a push that touches protected paths, after it is
	// stored. Nil refuses such pushes outright.
	Hold func(context.Context, Held) error
	// Log receives one event per push.
	Log func(Event)
	// Git is the git binary; empty finds it on PATH.
	Git string
}

// MinSecret is the shortest secret value the scan looks for.
const MinSecret = 8

// Event is one push, as logged. It never holds a secret value or a diff.
type Event struct {
	Time    time.Time `json:"time"`
	Repo    string    `json:"repo"`
	Refs    []string  `json:"refs"`
	Verdict string    `json:"verdict"`
	Reason  string    `json:"reason,omitempty"`
	HeldID  string    `json:"held_id,omitempty"`
}

// Verdicts.
const (
	Pushed  = "pushed"
	Refused = "refused"
	HeldUp  = "held"
	Failed  = "failed"
)

// Gateway serves one session's pushes.
type Gateway struct {
	root    string
	gitBin  string
	path    string
	repos   map[string]*repoState
	protect []*regexp.Regexp
	secrets []secret
	maxPack int64
	hold    func(context.Context, Held) error
	log     func(Event)
}

type repoState struct {
	Repo
	mu        sync.Mutex
	forgeHead string
	owned     map[string]bool
}

var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// New checks the configuration and makes a gateway.
func New(c Config) (*Gateway, error) {
	if c.Root == "" || !filepath.IsAbs(c.Root) {
		return nil, errors.New("push gateway: Root must be an absolute path")
	}
	bin := c.Git
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("git"); err != nil {
			return nil, fmt.Errorf("push gateway: %w", err)
		}
	}
	g := &Gateway{
		root:    c.Root,
		gitBin:  bin,
		path:    os.Getenv("PATH"),
		repos:   map[string]*repoState{},
		maxPack: c.MaxPack,
		hold:    c.Hold,
		log:     c.Log,
	}
	if g.maxPack <= 0 {
		g.maxPack = DefaultMaxPack
	}
	for _, r := range c.Repos {
		if !repoPathRE.MatchString(r.Path) || strings.Contains(r.Path, "..") || strings.HasSuffix(r.Path, ".git") {
			return nil, fmt.Errorf("push gateway: bad repo path %q", r.Path)
		}
		scheme, err := remoteScheme(r.Remote)
		if err != nil {
			return nil, fmt.Errorf("push gateway: %s: %w", r.Path, err)
		}
		if scheme == "ssh" && (r.KeyFile == "" || r.KnownHosts == "") {
			return nil, fmt.Errorf("push gateway: %s: an ssh remote needs the deploy key and the forge's known_hosts", r.Path)
		}
		if !validBranch(r.DefaultBranch) {
			return nil, fmt.Errorf("push gateway: %s: bad default branch %q", r.Path, r.DefaultBranch)
		}
		key := strings.ToLower(r.Path)
		if g.repos[key] != nil {
			return nil, fmt.Errorf("push gateway: %s is listed twice", r.Path)
		}
		st := &repoState{Repo: r, owned: map[string]bool{}}
		for _, b := range r.Branches {
			if !validBranch(b) {
				return nil, fmt.Errorf("push gateway: %s: bad branch %q", r.Path, b)
			}
			st.owned["refs/heads/"+b] = true
		}
		g.repos[key] = st
	}
	for _, p := range c.Protect {
		re, err := globRE(p)
		if err != nil {
			return nil, fmt.Errorf("push gateway: protected path %q: %w", p, err)
		}
		g.protect = append(g.protect, re)
	}
	for name, v := range c.Secrets {
		if len(v) >= MinSecret {
			g.secrets = append(g.secrets, secret{name: name, value: []byte(v)})
		}
	}
	for _, d := range []string{"home", "repos", "incoming", "held"} {
		if err := os.MkdirAll(filepath.Join(g.root, d), 0o700); err != nil {
			return nil, fmt.Errorf("push gateway: %w", err)
		}
	}
	return g, nil
}

// ServeHTTP answers git's smart-HTTP push requests:
// GET /owner/name[.git]/info/refs?service=git-receive-pack and
// POST /owner/name[.git]/git-receive-pack.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/info/refs"):
		st := g.repo(strings.TrimSuffix(p, "/info/refs"))
		if st == nil || r.URL.RawQuery != "service=git-receive-pack" {
			http.NotFound(w, r)
			return
		}
		g.advertise(w, r, st)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git-receive-pack"):
		st := g.repo(strings.TrimSuffix(p, "/git-receive-pack"))
		if st == nil {
			http.NotFound(w, r)
			return
		}
		g.receive(w, r, st)
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) repo(p string) *repoState {
	p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), ".git")
	if !repoPathRE.MatchString(p) {
		return nil
	}
	st := g.repos[strings.ToLower(p)]
	if st == nil || st.Path != p {
		return nil
	}
	return st
}

// capabilities the gateway offers. No push options, no push certificates,
// no shallow updates.
func capabilities() string {
	return "report-status delete-refs side-band-64k quiet atomic ofs-delta object-format=sha1 agent=waypoint-push-gateway"
}

// advertise fetches the forge's refs into the mirror and lists them, so
// the client sends a pack against what the mirror has.
func (g *Gateway) advertise(w http.ResponseWriter, r *http.Request, st *repoState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx := r.Context()
	m, err := g.open(ctx, &st.Repo)
	if err == nil {
		err = g.sync(ctx, m, st)
	}
	var refs map[string]string
	var order []string
	if err == nil {
		refs, order, err = g.refs(ctx, m)
	}
	if err != nil {
		g.emit(Event{Repo: st.Path, Verdict: Failed, Reason: "fetch: " + clean(err.Error())})
		http.Error(w, "Waypoint push gateway: can't reach the forge", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var b strings.Builder
	b.Write(pkt("# service=git-receive-pack\n"))
	b.Write(flushPkt)
	caps := "\x00" + capabilities()
	if len(order) == 0 {
		b.Write(pkt(zeroID + " capabilities^{}" + caps + "\n"))
	}
	for i, name := range order {
		line := refs[name] + " " + name
		if i == 0 {
			line += caps
		}
		b.Write(pkt(line + "\n"))
	}
	b.Write(flushPkt)
	_, _ = io.WriteString(w, b.String())
}

const zeroID = "0000000000000000000000000000000000000000"

var oidRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Update is one ref update a push asks for.
type Update struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

func (c Update) delete() bool { return c.New == zeroID }
func (c Update) create() bool { return c.Old == zeroID }

// request is a parsed receive-pack request.
type request struct {
	cmds     []Update
	sideband bool
	quiet    bool
}

// parseCommands reads the command list up to its flush; the pack follows.
func parseCommands(r *bufio.Reader) (*request, error) {
	req := &request{}
	for {
		line, err := readPkt(r)
		if errors.Is(err, errFlush) {
			break
		}
		if err != nil {
			return nil, err
		}
		s := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(s, "shallow ") {
			continue
		}
		if strings.HasPrefix(s, "push-cert") {
			return nil, errors.New("push certificates aren't accepted")
		}
		if len(req.cmds) == 0 {
			if i := strings.IndexByte(s, 0); i >= 0 {
				for _, c := range strings.Fields(s[i+1:]) {
					switch c {
					case "side-band-64k":
						req.sideband = true
					case "quiet":
						req.quiet = true
					}
				}
				s = s[:i]
			}
		}
		f := strings.Split(s, " ")
		if len(f) != 3 || !oidRE.MatchString(f[0]) || !oidRE.MatchString(f[1]) || f[2] == "" {
			return nil, fmt.Errorf("bad command %q", clean(s))
		}
		if strings.ContainsAny(f[2], "\x00\n") {
			return nil, errors.New("bad ref name")
		}
		req.cmds = append(req.cmds, Update{Old: f[0], New: f[1], Ref: f[2]})
		if len(req.cmds) > maxCommands {
			return nil, fmt.Errorf("more than %d refs in one push", maxCommands)
		}
	}
	if len(req.cmds) == 0 {
		return nil, errors.New("no commands")
	}
	return req, nil
}

// receive handles one push.
func (g *Gateway) receive(w http.ResponseWriter, r *http.Request, st *repoState) {
	ctx := r.Context()
	body := io.Reader(r.Body)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer func() { _ = zr.Close() }()
		body = zr
	}
	lr := &io.LimitedReader{R: body, N: g.maxPack + 1}
	br := bufio.NewReaderSize(lr, 64<<10)
	req, err := parseCommands(br)
	if err != nil {
		g.emit(Event{Repo: st.Path, Verdict: Refused, Reason: clean(err.Error())})
		http.Error(w, "Waypoint push gateway: "+clean(err.Error()), http.StatusBadRequest)
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	rw := &reportWriter{w: w, sideband: req.sideband, quiet: req.quiet}
	refs := make([]string, len(req.cmds))
	for i, c := range req.cmds {
		refs[i] = c.Ref
	}

	m, err := g.open(ctx, &st.Repo)
	if err == nil {
		err = g.sync(ctx, m, st)
	}
	if err != nil {
		g.failAll(rw, req, "unpack gateway error", "the gateway can't reach the forge")
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: "fetch: " + clean(err.Error())})
		return
	}
	in, err := g.newIncoming()
	if err != nil {
		g.failAll(rw, req, "unpack gateway error", "the gateway can't store the push")
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: clean(err.Error())})
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(in.dir)
		}
	}()

	if needsPack(req.cmds) {
		if err := g.unpack(ctx, m, in, br); err != nil {
			reason := "the pack can't be read"
			if lr.N <= 0 {
				reason = fmt.Sprintf("the push is larger than %d MB", g.maxPack>>20)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(br, 1<<20))
			rw.report(append([]string{"unpack " + reason}, ngAll(req.cmds, "unpacker error")...))
			g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Refused, Reason: reason + ": " + clean(err.Error())})
			return
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(br, 1<<20))
	}

	v := g.check(ctx, m, in, st, req.cmds)
	switch {
	case v.err != nil:
		g.failAll(rw, req, "unpack ok", "the gateway couldn't check the push")
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: clean(v.err.Error())})
		return
	case len(v.refused) > 0:
		lines := []string{"unpack ok"}
		for _, c := range req.cmds {
			if why := v.refused[c.Ref]; why != "" {
				lines = append(lines, "ng "+c.Ref+" "+why)
				rw.note(fmt.Sprintf("Waypoint: %s refused: %s", c.Ref, why))
			} else {
				lines = append(lines, "ng "+c.Ref+" not pushed: another ref in this push was refused")
			}
		}
		rw.report(lines)
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Refused, Reason: v.summary()})
		return
	case len(v.protected) > 0:
		why := "protected files: " + listShort(v.protected)
		if g.hold == nil {
			rw.note("Waypoint: this push changes protected files (" + listShort(v.protected) + "), which sandboxed sessions can't push.")
			rw.report(append([]string{"unpack ok"}, ngAll(req.cmds, "refused: "+why)...))
			g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Refused, Reason: why})
			return
		}
		h, err := g.store(ctx, m, in, st, req.cmds, v.protected)
		if err != nil {
			g.failAll(rw, req, "unpack ok", "the gateway couldn't hold the push for approval")
			g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: clean(err.Error())})
			return
		}
		keep = true
		if err := g.hold(ctx, *h); err != nil {
			_ = os.RemoveAll(g.heldDir(h.ID))
			g.failAll(rw, req, "unpack ok", "the gateway couldn't ask a person to approve the push")
			g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: "hold: " + clean(err.Error())})
			return
		}
		rw.note("Waypoint: this push changes protected files (" + listShort(v.protected) + "). It is held for a person to approve; nothing reached the forge yet.")
		rw.report(append([]string{"unpack ok"}, ngAll(req.cmds, "held for a person's approval ("+why+")")...))
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: HeldUp, Reason: why, HeldID: h.ID})
		return
	}

	res := g.forward(ctx, m, in, st, req.cmds)
	lines := []string{"unpack ok"}
	for _, c := range req.cmds {
		if why := res.refused[c.Ref]; why != "" || res.err != nil {
			if why == "" {
				why = "the forge refused the push"
			}
			lines = append(lines, "ng "+c.Ref+" "+why)
		} else {
			lines = append(lines, "ok "+c.Ref)
		}
	}
	rw.report(lines)
	if res.err != nil {
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: clean(res.err.Error())})
		return
	}
	g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Pushed})
}

// sync fetches the forge into the mirror and notes its HEAD.
func (g *Gateway) sync(ctx context.Context, m *mirror, st *repoState) error {
	head, err := g.fetch(ctx, m)
	if err != nil {
		return err
	}
	st.forgeHead = head
	return nil
}

func needsPack(cmds []Update) bool {
	for _, c := range cmds {
		if !c.delete() {
			return true
		}
	}
	return false
}

func ngAll(cmds []Update, why string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = "ng " + c.Ref + " " + why
	}
	return out
}

func (g *Gateway) failAll(rw *reportWriter, req *request, unpack, why string) {
	rw.note("Waypoint: " + why)
	rw.report(append([]string{unpack}, ngAll(req.cmds, why)...))
}

func (g *Gateway) emit(e Event) {
	if g.log == nil {
		return
	}
	e.Time = time.Now()
	g.log(e)
}

// incoming is one push's quarantine: its own object directory, with the
// mirror's objects as an alternate.
type incoming struct {
	id  string
	dir string
}

func (in *incoming) objects() string { return filepath.Join(in.dir, "objects") }

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (g *Gateway) newIncoming() (*incoming, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	in := &incoming{id: id, dir: filepath.Join(g.root, "incoming", id)}
	if err := os.MkdirAll(filepath.Join(in.objects(), "pack"), 0o700); err != nil {
		return nil, err
	}
	return in, nil
}

// quarantineEnv points git at the push's objects, with the mirror's behind.
func (m *mirror) quarantineEnv(objects string) []string {
	return []string{
		"GIT_OBJECT_DIRECTORY=" + objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(m.dir, "objects"),
	}
}

// unpack indexes the pack into the quarantine, checking every object
// (--strict) and completing thin packs from the mirror.
func (g *Gateway) unpack(ctx context.Context, m *mirror, in *incoming, pack io.Reader) error {
	_, err := g.git(ctx, m, m.quarantineEnv(in.objects()), pack, "index-pack", "--stdin", "--fix-thin", "--strict")
	return err
}

// clean makes text safe to put in a report line or a log: one line,
// printable, short.
func clean(s string) string {
	s = firstLine(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
		if b.Len() >= 300 {
			b.WriteString("…")
			break
		}
	}
	return b.String()
}

func listShort(xs []string) string {
	const n = 5
	qs := make([]string, 0, n)
	for i, x := range xs {
		if i == n {
			qs = append(qs, fmt.Sprintf("and %d more", len(xs)-n))
			break
		}
		qs = append(qs, clean(x))
	}
	return strings.Join(qs, ", ")
}
