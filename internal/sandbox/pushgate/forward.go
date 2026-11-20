package pushgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// result is what the forge said about a forwarded push.
type result struct {
	err     error
	refused map[string]string
}

// forward pushes the updates to the forge with the repo's deploy key,
// leasing each ref on the value the checks saw, all or nothing. Once the
// forge takes them, the objects move into the mirror and its refs follow.
func (g *Gateway) forward(ctx context.Context, m *mirror, in *incoming, st *repoState, cmds []Update) result {
	args := []string{"push", "--porcelain", "--atomic", "--no-verify"}
	for _, c := range cmds {
		lease := c.Old
		if c.create() {
			lease = ""
		}
		args = append(args, "--force-with-lease="+c.Ref+":"+lease)
	}
	args = append(args, "--", m.repo.Remote)
	for _, c := range cmds {
		if c.delete() {
			args = append(args, ":"+c.Ref)
		} else {
			args = append(args, c.New+":"+c.Ref)
		}
	}
	out, err := g.git(ctx, m, append(m.remoteEnv(), m.quarantineEnv(in.objects())...), nil, args...)
	if err != nil {
		return result{err: err, refused: porcelainRefusals(out)}
	}
	for _, c := range cmds {
		if c.create() && !c.delete() {
			st.owned[c.Ref] = true
		}
	}
	if err := g.migrate(m, in); err != nil {
		// The forge has the push; the mirror catches up on the next fetch.
		return result{}
	}
	var upd bytes.Buffer
	for _, c := range cmds {
		if c.delete() {
			fmt.Fprintf(&upd, "delete %s %s\n", c.Ref, c.Old)
			continue
		}
		fmt.Fprintf(&upd, "update %s %s %s\n", c.Ref, c.New, c.Old)
	}
	_, _ = g.git(ctx, m, nil, &upd, "update-ref", "--stdin")
	return result{}
}

// porcelainRefusals reads git push --porcelain's rejected refs:
// "!\t<from>:<to>\t<summary>".
func porcelainRefusals(out []byte) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 || f[0] != "!" {
			continue
		}
		_, to, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		res[to] = "the forge refused it: " + clean(f[2])
	}
	return res
}

// migrate moves a pushed pack from its quarantine into the mirror: the
// pack first, its index last, so the mirror never sees an index without
// its pack.
func (g *Gateway) migrate(m *mirror, in *incoming) error {
	src := filepath.Join(in.objects(), "pack")
	dst := filepath.Join(m.dir, "objects", "pack")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	rank := func(name string) int {
		switch filepath.Ext(name) {
		case ".pack":
			return 0
		case ".idx":
			return 2
		}
		return 1
	}
	sort.SliceStable(ents, func(i, j int) bool { return rank(ents[i].Name()) < rank(ents[j].Name()) })
	for _, e := range ents {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "pack-") {
			continue
		}
		to := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}

// Held is a push that touches protected paths, kept for a person to see.
type Held struct {
	ID        string   `json:"id"`
	Repo      string   `json:"repo"`
	Updates   []Update `json:"updates"`
	Protected []string `json:"protected"`
	// Diff is the protected files' diff, cut at 256 KB (DiffCut).
	Diff    string    `json:"diff"`
	DiffCut bool      `json:"diff_cut,omitempty"`
	Time    time.Time `json:"time"`
}

var heldIDRE = regexp.MustCompile(`^[0-9a-f]{24}$`)

func (g *Gateway) heldDir(id string) string { return filepath.Join(g.root, "held", id) }

// store keeps a protected push: its quarantine moves under held/, with
// what it asked for and the diff a person approves.
func (g *Gateway) store(ctx context.Context, m *mirror, in *incoming, st *repoState, cmds []Update, protected []string) (*Held, error) {
	h := &Held{ID: in.id, Repo: st.Path, Updates: cmds, Protected: protected, Time: time.Now().UTC()}
	h.Diff, h.DiffCut = g.protectedDiff(ctx, m, in, st, cmds, protected)
	dir := g.heldDir(in.id)
	if err := os.Rename(in.dir, dir); err != nil {
		return nil, err
	}
	in.dir = dir
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "push.json"), b, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return h, nil
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// protectedDiff is the diff of the protected files only, per updated
// branch: against the branch's old value, or for a new branch against
// where it leaves the default branch.
func (g *Gateway) protectedDiff(ctx context.Context, m *mirror, in *incoming, st *repoState, cmds []Update, protected []string) (string, bool) {
	env := m.quarantineEnv(in.objects())
	files := protected
	if len(files) > 200 {
		files = files[:200]
	}
	var b strings.Builder
	for _, c := range cmds {
		if c.delete() {
			continue
		}
		base := c.Old
		if c.create() {
			base = emptyTree
			if mb, err := g.git(ctx, m, env, nil, "merge-base", c.New, "refs/heads/"+st.DefaultBranch); err == nil {
				base = strings.TrimSpace(string(mb))
			}
		}
		args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", base, c.New, "--"}, files...)
		out, err := g.git(ctx, m, env, nil, args...)
		if err != nil || len(out) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# %s %s..%s\n", c.Ref, short(base), short(c.New))
		b.Write(out)
		if b.Len() > maxDiff {
			break
		}
	}
	s := b.String()
	if len(s) > maxDiff {
		return s[:maxDiff], true
	}
	return s, false
}

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// ErrNoHeld is a held push that isn't (or is no longer) stored.
var ErrNoHeld = errors.New("no such held push")

func (g *Gateway) loadHeld(id string) (*Held, error) {
	if !heldIDRE.MatchString(id) {
		return nil, ErrNoHeld
	}
	b, err := os.ReadFile(filepath.Join(g.heldDir(id), "push.json")) // #nosec G304 -- id is checked hex under our root
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoHeld
	}
	if err != nil {
		return nil, err
	}
	var h Held
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	if h.ID != id {
		return nil, ErrNoHeld
	}
	return &h, nil
}

// HeldPushes lists the pushes waiting for a person, oldest first.
func (g *Gateway) HeldPushes() ([]Held, error) {
	ents, err := os.ReadDir(filepath.Join(g.root, "held"))
	if err != nil {
		return nil, err
	}
	var out []Held
	for _, e := range ents {
		if h, err := g.loadHeld(e.Name()); err == nil {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Forward sends a held push on to the forge once a person approved it.
// The ref rules are checked again, and each ref is leased on the value it
// had when the push was held: if the branch moved since, nothing is
// pushed and the push stays held.
func (g *Gateway) Forward(ctx context.Context, id string) error {
	h, err := g.loadHeld(id)
	if err != nil {
		return err
	}
	st := g.repos[strings.ToLower(h.Repo)]
	if st == nil || st.Path != h.Repo {
		return fmt.Errorf("held push %s is for %s, which this gateway doesn't serve", id, h.Repo)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// A Drop or another Forward may have run while this call waited.
	if h, err = g.loadHeld(id); err != nil {
		return err
	}
	refs := make([]string, len(h.Updates))
	for i, c := range h.Updates {
		refs[i] = c.Ref
	}
	m, err := g.open(ctx, &st.Repo)
	if err == nil {
		err = g.sync(ctx, m, st)
	}
	if err != nil {
		return err
	}
	for _, c := range h.Updates {
		if why := g.refRule(st, c); why != "" {
			return fmt.Errorf("%s: %s", c.Ref, why)
		}
	}
	in := &incoming{id: id, dir: g.heldDir(id)}
	res := g.forward(ctx, m, in, st, h.Updates)
	if res.err != nil {
		reason := clean(res.err.Error())
		for _, r := range refs {
			if why := res.refused[r]; why != "" {
				reason = r + ": " + why
				break
			}
		}
		g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Failed, Reason: "approved push: " + reason, HeldID: id})
		return fmt.Errorf("the forge didn't take the approved push: %s", reason)
	}
	_ = os.RemoveAll(in.dir)
	g.emit(Event{Repo: st.Path, Refs: refs, Verdict: Pushed, Reason: "approved", HeldID: id})
	return nil
}

// Drop deletes a held push a person turned down (or that expired).
func (g *Gateway) Drop(id string) error {
	h, err := g.loadHeld(id)
	if err != nil {
		return err
	}
	if st := g.repos[strings.ToLower(h.Repo)]; st != nil {
		// Serialize with Forward; a push it already sent is no longer held.
		st.mu.Lock()
		defer st.mu.Unlock()
		if _, err := g.loadHeld(id); err != nil {
			return err
		}
	}
	return os.RemoveAll(g.heldDir(id))
}
