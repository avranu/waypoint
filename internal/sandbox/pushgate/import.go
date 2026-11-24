package pushgate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ImportRequest is what a job's runner asks of its bundle.
type ImportRequest struct {
	// Repo is "owner/name", one of the gateway's repos.
	Repo string `json:"repo"`
	// Base is the commit the job started from; Commits, Files and Diff are
	// what the bundle adds after it.
	Base string `json:"base"`
	// Branch is where the work goes if pushed. Empty only checks.
	Branch string `json:"branch,omitempty"`
	// Push sends the work to Branch on the forge with the deploy key, by
	// the same rules as a push from the session, once the checks pass.
	Push bool `json:"push,omitempty"`
}

// Imported is what a bundle held and what became of it.
type Imported struct {
	// Tip is the bundle's commit; empty when the job made none.
	Tip string `json:"tip,omitempty"`
	// Commits are Base..Tip, oldest first.
	Commits []string `json:"commits"`
	// Files are the paths Base..Tip changes, a rename as both its names.
	Files []string `json:"files"`
	// Protected are the protected files its new commits touch.
	Protected []string `json:"protected,omitempty"`
	// Diff is Base..Tip, cut at a size (DiffCut says so).
	Diff    string `json:"diff,omitempty"`
	DiffCut bool   `json:"diff_cut,omitempty"`
	// Verdict is Refused when a check refused the work (Reason says why),
	// else for a push Pushed, HeldUp (HeldID) or Failed. A check-only
	// import that passes, and an empty bundle, leave it empty.
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
	HeldID  string `json:"held_id,omitempty"`
}

// Bundle header limits.
const (
	maxBundleLine    = 1024
	maxBundlePrereqs = 1000
)

// ErrBadBundle is a stream that isn't a bundle the gateway takes.
var ErrBadBundle = errors.New("not a bundle the gateway takes")

// bundleHeader is a bundle's header: the commits it builds on and its one
// ref.
type bundleHeader struct {
	prereqs []string
	tip     string
}

// readBundleHeader reads a v2 or v3 bundle's header, up to the pack. It
// takes exactly one ref (its name isn't used: the caller says where the
// work goes) and no capabilities but SHA-1.
func readBundleHeader(r *bufio.Reader) (*bundleHeader, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBadBundle, fmt.Sprintf(format, a...))
	}
	readLine := func() (string, error) {
		b, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(b) > maxBundleLine {
			return "", bad("a header line is longer than %d bytes", maxBundleLine)
		}
		if err != nil {
			return "", bad("the header ends early")
		}
		return string(b[:len(b)-1]), nil
	}
	first, err := readLine()
	if err != nil {
		return nil, err
	}
	v3 := false
	switch first {
	case "# v2 git bundle":
	case "# v3 git bundle":
		v3 = true
	default:
		return nil, bad("it doesn't start as a v2 or v3 git bundle")
	}
	h := &bundleHeader{}
	refs := 0
	for {
		line, err := readLine()
		if err != nil {
			return nil, err
		}
		switch {
		case line == "":
			if refs == 0 {
				return nil, bad("it names no ref")
			}
			return h, nil
		case v3 && strings.HasPrefix(line, "@"):
			if line != "@object-format=sha1" {
				return nil, bad("it asks for %s", clean(line))
			}
		case strings.HasPrefix(line, "-"):
			oid, _, _ := strings.Cut(line[1:], " ")
			if !oidRE.MatchString(oid) {
				return nil, bad("a prerequisite %q isn't a commit id", clean(oid))
			}
			if len(h.prereqs) == maxBundlePrereqs {
				return nil, bad("it builds on more than %d commits", maxBundlePrereqs)
			}
			h.prereqs = append(h.prereqs, oid)
		default:
			oid, name, _ := strings.Cut(line, " ")
			if !oidRE.MatchString(oid) || name == "" {
				return nil, bad("a ref line %q isn't an id and a name", clean(line))
			}
			if refs++; refs > 1 {
				return nil, bad("it names more than one ref")
			}
			h.tip = oid
		}
	}
}

// Import reads a job's bundle into the gateway's mirror of req.Repo,
// checks it and, if asked, pushes it to req.Branch. An empty stream is a
// job that made no commits. A stream that isn't a bundle the gateway
// takes is an error (ErrBadBundle), as is one that can't be checked; a
// check that refuses the work is a verdict.
func (g *Gateway) Import(ctx context.Context, req ImportRequest, bundle io.Reader) (*Imported, error) {
	st := g.repo(req.Repo)
	switch {
	case st == nil:
		return nil, fmt.Errorf("push gateway: %s isn't one of this session's repositories", req.Repo)
	case !oidRE.MatchString(req.Base):
		return nil, fmt.Errorf("push gateway: the base %q isn't a full commit id", req.Base)
	case req.Branch != "" && !validBranch(req.Branch):
		return nil, fmt.Errorf("push gateway: bad branch %q", req.Branch)
	case req.Push && req.Branch == "":
		return nil, errors.New("push gateway: a push needs a branch")
	}
	lr := &io.LimitedReader{R: bundle, N: g.maxPack + 1}
	br := bufio.NewReaderSize(lr, 64<<10)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return &Imported{Commits: []string{}, Files: []string{}}, nil
	} else if err != nil {
		return nil, err
	}
	hdr, err := readBundleHeader(br)
	if err != nil {
		return nil, err
	}
	in, err := g.newIncoming()
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(in.dir)
		}
	}()
	pack, err := spool(filepath.Join(in.dir, "bundle.pack"), br)
	if err != nil {
		return nil, fmt.Errorf("push gateway: can't store the bundle: %w", err)
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("%w: it is larger than %d MB", ErrBadBundle, g.maxPack>>20)
	}
	defer func() { _ = pack.Close() }()

	st.mu.Lock()
	defer st.mu.Unlock()
	ref := "HEAD"
	if req.Branch != "" {
		ref = "refs/heads/" + req.Branch
	}
	refs := []string{ref}
	m, err := g.open(ctx, &st.Repo)
	if err == nil {
		err = g.sync(ctx, m, st)
	}
	if err != nil {
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: "import: fetch: " + clean(err.Error())})
		return nil, fmt.Errorf("push gateway: can't reach the forge: %w", err)
	}
	if missing, err := g.missingCommits(ctx, m, append([]string{req.Base}, hdr.prereqs...)); err != nil {
		return nil, err
	} else if len(missing) > 0 {
		return nil, fmt.Errorf("%w: it builds on %s, which the forge doesn't have", ErrBadBundle, short(missing[0]))
	}
	err = g.unpack(ctx, m, in, pack)
	_ = pack.Close()
	_ = os.Remove(pack.Name())
	if err != nil {
		return nil, fmt.Errorf("%w: the pack can't be read: %s", ErrBadBundle, clean(err.Error()))
	}
	env := m.quarantineEnv(in.objects())
	if out, err := g.git(ctx, m, env, nil, "cat-file", "-t", hdr.tip); err != nil || strings.TrimSpace(string(out)) != "commit" {
		return nil, fmt.Errorf("%w: its ref isn't a commit it carries", ErrBadBundle)
	}

	res := &Imported{Tip: hdr.tip, Commits: []string{}, Files: []string{}}
	refuse := func(why string) (*Imported, error) {
		res.Verdict, res.Reason = Refused, why
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Refused, Reason: "import: " + why})
		return res, nil
	}
	if _, err := g.git(ctx, m, env, nil, "merge-base", "--is-ancestor", req.Base, hdr.tip); err != nil {
		if exitCode(err) != 1 {
			return nil, err
		}
		return refuse("its commit doesn't build on the base " + short(req.Base))
	}
	out, err := g.git(ctx, m, env, nil, "rev-list", "--max-count="+strconv.Itoa(maxNewCommits+1), hdr.tip, "^"+req.Base)
	if err != nil {
		return nil, err
	}
	res.Commits = strings.Fields(string(out))
	slices.Reverse(res.Commits)
	if len(res.Commits) > maxNewCommits {
		res.Commits = res.Commits[:0]
		return refuse(fmt.Sprintf("more than %d new commits", maxNewCommits))
	}

	cur := zeroID
	if req.Branch != "" {
		have, _, err := g.refs(ctx, m)
		if err != nil {
			return nil, err
		}
		if oid := have[ref]; oid != "" {
			cur = oid
		}
	}
	cmds := []Update{{Old: cur, New: hdr.tip, Ref: ref}}
	var v *verdict
	if req.Push {
		v = g.check(ctx, m, in, st, cmds)
	} else {
		v = g.checkContent(ctx, m, in, cmds, &verdict{})
	}
	if v.err != nil {
		return nil, v.err
	}
	if v.secret {
		return refuse(fmt.Sprintf("it carries a known secret value (%s)", v.secretName))
	}
	if len(v.refused) > 0 {
		return refuse(v.summary())
	}
	files, err := g.git(ctx, m, env, nil, "diff", "--name-only", "--no-renames", "-z", "--no-ext-diff", "--no-textconv", req.Base, hdr.tip)
	if err != nil {
		return nil, err
	}
	for _, f := range bytes.Split(files, []byte{0}) {
		if len(f) > 0 {
			res.Files = append(res.Files, string(f))
		}
	}
	diff, err := g.git(ctx, m, env, nil, "diff", "--no-ext-diff", "--no-textconv", "--no-color", req.Base, hdr.tip)
	if err != nil {
		return nil, err
	}
	if len(diff) > maxDiff {
		diff, res.DiffCut = diff[:maxDiff], true
	}
	res.Diff = string(diff)
	res.Protected = v.protected
	if !req.Push {
		return res, nil
	}
	if len(v.protected) > 0 {
		why := "protected files: " + listShort(v.protected)
		if g.hold == nil {
			return refuse(why)
		}
		h, err := g.store(ctx, m, in, st, cmds, v.protected)
		if err != nil {
			return nil, err
		}
		keep = true
		if err := g.hold(ctx, *h); err != nil {
			_ = os.RemoveAll(g.heldDir(h.ID))
			return nil, fmt.Errorf("push gateway: couldn't ask a person to approve the push: %w", err)
		}
		res.Verdict, res.Reason, res.HeldID = HeldUp, why, h.ID
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: HeldUp, Reason: "import: " + why, HeldID: h.ID})
		return res, nil
	}
	if cur == hdr.tip {
		res.Verdict = Pushed
		return res, nil
	}
	if r := g.forward(ctx, m, in, st, cmds); r.err != nil {
		res.Verdict, res.Reason = Failed, clean(r.err.Error())
		if why := r.refused[ref]; why != "" {
			res.Reason = why
		}
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: "import: " + res.Reason})
		return res, nil
	}
	res.Verdict = Pushed
	g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Pushed, Reason: "import"})
	return res, nil
}

// spool copies r to a new file at path and returns it open at its start.
func spool(path string, r io.Reader) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path the gateway builds in its own quarantine directory, created O_EXCL
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(f, r); err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// missingCommits lists the ids the mirror has no commit for.
func (g *Gateway) missingCommits(ctx context.Context, m *mirror, oids []string) ([]string, error) {
	var in bytes.Buffer
	for _, o := range oids {
		in.WriteString(o + "\n")
	}
	out, err := g.git(ctx, m, nil, &in, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		oid, typ, _ := strings.Cut(line, " ")
		if typ != "commit" {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}
