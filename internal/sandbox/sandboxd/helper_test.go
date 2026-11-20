//go:build linux

package sandboxd

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// fakeSys is a machine's accounts in memory.
type fakeSys struct {
	fakeUnits
	users, groups map[string]int
	sub           map[string]bool
	failAddUser   int // fail this many AddUser calls
	failDelUser   int // and DelUser calls
	adds, dels    []string
}

func newFakeSys() *fakeSys {
	return &fakeSys{
		users:  map[string]int{"root": 0, "devhost": 1000, sandbox.EgressUser: 998},
		groups: map[string]int{"root": 0, sandbox.EgressUser: 998}, sub: map[string]bool{},
		fakeUnits: fakeUnits{listeners: map[string]net.Listener{}},
	}
}

func (f *fakeSys) UserByName(n string) (int, bool, error) { id, ok := f.users[n]; return id, ok, nil }
func (f *fakeSys) GroupByName(n string) (int, bool, error) {
	id, ok := f.groups[n]
	return id, ok, nil
}
func (f *fakeSys) UserByUID(id int) (string, bool, error)  { return find(f.users, id) }
func (f *fakeSys) GroupByGID(id int) (string, bool, error) { return find(f.groups, id) }
func (f *fakeSys) AddGroup(n string, gid int) error {
	if _, ok := f.groups[n]; ok {
		return fmt.Errorf("group %s exists", n)
	}
	f.groups[n] = gid
	return nil
}
func (f *fakeSys) AddUser(n string, uid, gid int, home, gecos string) error {
	if f.failAddUser > 0 {
		f.failAddUser--
		return errors.New("useradd: killed")
	}
	if _, ok := f.users[n]; ok {
		return fmt.Errorf("user %s exists", n)
	}
	if !strings.HasSuffix(home, "/homes/"+n) || !strings.HasPrefix(gecos, "waypoint-sandbox ") {
		return fmt.Errorf("home %s gecos %s", home, gecos)
	}
	f.users[n] = uid
	f.adds = append(f.adds, n)
	return nil
}
func (f *fakeSys) DelUser(n string) error {
	if f.failDelUser > 0 {
		f.failDelUser--
		return errors.New("userdel: user is currently used by process 4242")
	}
	if _, ok := f.users[n]; !ok {
		return fmt.Errorf("user %s doesn't exist", n)
	}
	delete(f.users, n)
	delete(f.groups, n) // as USERGROUPS_ENAB does
	f.dels = append(f.dels, n)
	return nil
}
func (f *fakeSys) DelGroup(n string) error {
	if _, ok := f.groups[n]; !ok {
		return fmt.Errorf("group %s doesn't exist", n)
	}
	delete(f.groups, n)
	return nil
}
func (f *fakeSys) SubIDs() (map[string]bool, error) { return f.sub, nil }

func find(m map[string]int, id int) (string, bool, error) {
	for n, v := range m {
		if v == id {
			return n, true, nil
		}
	}
	return "", false, nil
}

type rig struct {
	t      *testing.T
	priv   ed25519.PrivateKey
	cfg    Config
	sys    *fakeSys
	chowns map[string][2]int
	now    time.Time
}

func newRig(t *testing.T) *rig {
	pub, priv, _ := ed25519.GenerateKey(nil)
	state := dirMode(t, 0o700)
	return &rig{
		t: t, priv: priv, sys: newFakeSys(), chowns: map[string][2]int{}, now: time.Unix(1_800_000_000, 0),
		cfg: Config{Version: "test", Key: pub, RunnerUID: os.Getuid(), RunnerID: 3,
			StorePath: dirMode(t, 0o755), StatePath: state, StoreOwner: os.Getuid(),
			RunPath: filepath.Join(t.TempDir(), "run"), SystemCAs: systemBundle(t)},
	}
}

// dirMode is a new directory with exactly mode, whatever the umask.
func dirMode(t *testing.T, mode os.FileMode) string {
	d := t.TempDir()
	if err := os.Chmod(d, mode); err != nil {
		t.Fatal(err)
	}
	return d
}

func (r *rig) open() *Helper {
	r.t.Helper()
	h := New(r.cfg, r.sys, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.now = func() time.Time { return r.now }
	h.chown = func(fd, uid, gid int) error {
		p, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		r.chowns[p] = [2]int{uid, gid}
		return nil
	}
	if err := h.Open(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(h.Close)
	return h
}

func (r *rig) spec(runner int64, repos ...*sandbox.Repo) *sandbox.Signed {
	r.t.Helper()
	sp := &sandbox.Spec{Op: sandbox.OpEnsureUsers, Runner: runner}
	for _, repo := range repos {
		if repo != nil {
			sp.Repos = append(sp.Repos, *repo)
		}
	}
	return r.sign(sp)
}

func (r *rig) sign(sp *sandbox.Spec) *sandbox.Signed {
	r.t.Helper()
	sp.Nonce, sp.Issued, sp.Expiry = sandbox.NewNonce(), r.now.Unix(), r.now.Add(10*time.Minute).Unix()
	sd, err := sandbox.Sign(r.priv, sp)
	if err != nil {
		r.t.Fatal(err)
	}
	return sd
}

func (r *rig) remove(h *Helper, u sandbox.User) sandbox.Response {
	r.t.Helper()
	return h.Handle(sandbox.Request{Op: sandbox.OpRemoveUser, Spec: r.sign(&sandbox.Spec{Op: sandbox.OpRemoveUser, Runner: 3, Job: 9,
		User: &sandbox.Target{Name: u.Name, Kind: u.Kind, Key: u.Key}})})
}

var waypointRepo = &sandbox.Repo{ID: "1/42", Slug: "Robin/waypoint", Owner: "1/robin"}

func ensure(h *Helper, sd *sandbox.Signed) sandbox.Response {
	return h.Handle(sandbox.Request{Op: sandbox.OpEnsureUsers, Spec: sd})
}

func TestEnsureUsers(t *testing.T) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" || len(resp.Users) != 2 {
		t.Fatalf("%+v", resp)
	}
	repo, org := resp.Users[0], resp.Users[1]
	if !strings.HasPrefix(repo.Name, "wpr-robin-waypoint-") || repo.UID != firstUID || repo.Kind != sandbox.KindRepo || repo.Key != "1/42" {
		t.Errorf("repo user %+v", repo)
	}
	if !strings.HasPrefix(org.Name, "wpo-robin-") || org.UID != firstUID+1 || org.Key != "1/robin" {
		t.Errorf("org user %+v", org)
	}
	if r.sys.groups[repo.Name] != repo.UID || r.sys.users[org.Name] != org.UID {
		t.Errorf("accounts %+v %+v", r.sys.users, r.sys.groups)
	}
	for _, a := range storeAreas {
		p := filepath.Join(r.cfg.StorePath, a, repo.Name)
		fi, err := os.Lstat(p)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", p, fi.Mode(), err)
		}
		if r.chowns[p] != [2]int{repo.UID, repo.UID} {
			t.Errorf("%s chowned %v", p, r.chowns[p])
		}
	}

	// The same spec twice is a replay.
	sd := r.spec(3, waypointRepo)
	if resp := ensure(h, sd); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := ensure(h, sd); !strings.Contains(resp.Error, "used already") {
		t.Errorf("replay: %+v", resp)
	}

	// A renamed repo keeps its user; a new repo of the same owner gets a
	// repo user and shares the org user.
	r.now = r.now.Add(time.Hour)
	resp = ensure(h, r.spec(3, &sandbox.Repo{ID: "1/42", Slug: "Robin/waypoint2", Owner: "1/robin"}))
	if resp.Error != "" || resp.Users[0].Name != repo.Name || resp.Users[0].Slug != "Robin/waypoint2" || resp.Users[0].LastUsed != r.now.Unix() {
		t.Errorf("renamed: %+v", resp)
	}
	resp = ensure(h, r.spec(3, &sandbox.Repo{ID: "1/43", Slug: "Robin/juno", Owner: "1/robin"}))
	if resp.Error != "" || resp.Users[0].UID != firstUID+2 || resp.Users[1].Name != org.Name {
		t.Errorf("second repo: %+v", resp)
	}
	if len(r.sys.adds) != 3 {
		t.Errorf("useradd ran for %v", r.sys.adds)
	}

	// The ledger survives a restart, and a removed account comes back
	// with its uid.
	delete(r.sys.users, repo.Name)
	h.Close()
	h2 := r.open()
	st := h2.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if st == nil || len(st.Users) != 3 || st.Users[0].Present || len(st.Drift) != 1 || st.Key != sandbox.Fingerprint(r.cfg.Key) {
		t.Fatalf("status %+v", st)
	}
	if resp := ensure(h2, r.spec(3, waypointRepo)); resp.Error != "" || r.sys.users[repo.Name] != repo.UID {
		t.Errorf("re-made: %+v %v", resp, r.sys.users)
	}
}

func TestEnsureUsersRefuses(t *testing.T) {
	r := newRig(t)
	h := r.open()
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	foreign, _ := sandbox.Sign(otherPriv, &sandbox.Spec{Op: sandbox.OpEnsureUsers, Nonce: "n", Issued: r.now.Unix(),
		Expiry: r.now.Add(time.Minute).Unix(), Runner: 3, Repos: []sandbox.Repo{*waypointRepo}})
	for name, c := range map[string]struct {
		sd   *sandbox.Signed
		want string
	}{
		"no spec":       {nil, "no spec"},
		"another key":   {foreign, "this helper pins"},
		"other runner":  {r.spec(4, waypointRepo), "runner 4"},
		"no repo":       {r.spec(3, nil), "names no repository"},
		"newline in id": {r.spec(3, &sandbox.Repo{ID: "1/42\nroot", Slug: "a/b", Owner: "1/a"}), "wrongly"},
		"colon owner":   {r.spec(3, waypointRepo, &sandbox.Repo{ID: "1/42", Slug: "a/b", Owner: "1/a:0:0"}), "wrongly"},
	} {
		if resp := ensure(h, c.sd); !strings.Contains(resp.Error, c.want) {
			t.Errorf("%s: %+v", name, resp)
		}
	}
	if resp := h.Handle(sandbox.Request{Op: "launch"}); !strings.Contains(resp.Error, "unknown operation") {
		t.Errorf("unknown op: %+v", resp)
	}

	// A user or group that exists outside the ledger isn't taken over.
	name := sandbox.RepoUser(waypointRepo.Slug, waypointRepo.ID, 0)
	r.sys.users[name] = 1234
	if resp := ensure(h, r.spec(3, waypointRepo)); !strings.Contains(resp.Error, "won't take it over") {
		t.Errorf("existing user: %+v", resp)
	}
	delete(r.sys.users, name)
	r.sys.groups[name] = 1234
	if resp := ensure(h, r.spec(3, waypointRepo)); !strings.Contains(resp.Error, "won't take it over") {
		t.Errorf("existing group: %+v", resp)
	}
	if len(r.sys.adds) != 0 {
		t.Errorf("useradd ran: %v", r.sys.adds)
	}
}

func TestUIDs(t *testing.T) {
	r := newRig(t)
	h := r.open()
	h.led.Retired = []int{firstUID}
	r.sys.users["someone"] = firstUID + 1
	r.sys.groups["somegroup"] = firstUID + 2
	resp := ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" || resp.Users[0].UID != firstUID+3 || resp.Users[1].UID != firstUID+4 {
		t.Errorf("%+v", resp)
	}
}

func TestReconcile(t *testing.T) {
	r := newRig(t)
	r.sys.failAddUser = 1
	h := r.open()
	if resp := ensure(h, r.spec(3, waypointRepo)); !strings.Contains(resp.Error, "killed") {
		t.Fatalf("%+v", resp)
	}
	// The group exists, the user doesn't, and the intent says so.
	st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if st.Pending != 1 || len(st.Users) != 0 {
		t.Fatalf("%+v", st)
	}
	// The uid stays reserved meanwhile.
	if uid, _ := h.freeUID(); uid != firstUID+1 {
		t.Errorf("free uid %d", uid)
	}
	h.Close()
	h2 := r.open()
	st = h2.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if st.Pending != 0 || len(st.Users) != 1 || !st.Users[0].Present || st.Users[0].UID != firstUID {
		t.Fatalf("after restart %+v", st)
	}
}

func TestStorePin(t *testing.T) {
	r := newRig(t)
	r.open().Close()
	r.cfg.StorePath = dirMode(t, 0o755)
	h := New(r.cfg, r.sys, nil)
	if err := h.Open(); err == nil || !strings.Contains(err.Error(), "--repin-store") {
		t.Fatalf("moved store: %v", err)
	}
	r.cfg.RepinStore = true
	r.open()

	// A store others can write isn't one.
	bad := t.TempDir()
	if err := os.Chmod(bad, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(bad, os.Getuid()); err == nil {
		t.Error("opened a world-writable store")
	}
	link := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(r.cfg.StorePath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(link, os.Getuid()); err == nil {
		t.Error("opened a store through a symlink")
	}
}

// A sandbox that swaps its work directory for a symlink doesn't send the
// helper anywhere.
func TestStoreSymlink(t *testing.T) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	name := resp.Users[0].Name
	work := filepath.Join(r.cfg.StorePath, "work", name)
	outside := t.TempDir()
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, work); err != nil {
		t.Fatal(err)
	}
	if resp := ensure(h, r.spec(3, waypointRepo)); resp.Error == "" {
		t.Error("followed a symlink in the store")
	}
	for p := range r.chowns {
		if strings.HasPrefix(p, outside) {
			t.Errorf("chowned %s", p)
		}
	}
}

func TestServe(t *testing.T) {
	serve := func(r *rig) string {
		h := r.open()
		sock := filepath.Join(t.TempDir(), "s.sock")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() { _ = h.Serve(l) }()
		return sock
	}
	r := newRig(t)
	sock := serve(r)
	ctx := context.Background()
	resp, err := sandbox.Call(ctx, sock, sandbox.Request{Op: sandbox.OpStatus})
	if err != nil || resp.Status == nil || resp.Status.Version != "test" {
		t.Fatalf("%+v %v", resp, err)
	}
	if _, err := sandbox.Call(ctx, sock, sandbox.Request{Op: sandbox.OpEnsureUsers, Spec: r.spec(3, waypointRepo)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Call(ctx, filepath.Join(t.TempDir(), "none"), sandbox.Request{Op: sandbox.OpStatus}); !errors.Is(err, sandbox.ErrNotInstalled) {
		t.Errorf("no socket: %v", err)
	}
	if os.Getuid() != 0 {
		other := newRig(t)
		other.cfg.RunnerUID = os.Getuid() + 1
		if _, err := sandbox.Call(ctx, serve(other), sandbox.Request{Op: sandbox.OpStatus}); err == nil || !strings.Contains(err.Error(), "may not use") {
			t.Errorf("another user: %v", err)
		}
	}
}

func TestEnsureUsersRepos(t *testing.T) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo,
		&sandbox.Repo{ID: "1/43", Slug: "Robin/juno", Owner: "1/robin"},
		&sandbox.Repo{ID: "1/7", Slug: "Ops/infrastructure", Owner: "1/ops"}))
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	var names []string
	for _, u := range resp.Users {
		names = append(names, u.Kind+":"+u.Slug)
	}
	if got := strings.Join(names, " "); got != "repo:Robin/waypoint org:Robin repo:Robin/juno repo:Ops/infrastructure org:Ops" {
		t.Errorf("users %s", got)
	}
	var many []*sandbox.Repo
	for i := range sandbox.MaxSpecRepos + 1 {
		many = append(many, &sandbox.Repo{ID: fmt.Sprintf("1/%d", 100+i), Slug: fmt.Sprintf("o/r%d", i), Owner: "1/o"})
	}
	if resp := ensure(h, r.spec(3, many...)); !strings.Contains(resp.Error, "at most") {
		t.Errorf("too many: %+v", resp)
	}
}

func TestRemoveUser(t *testing.T) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	repo, org := resp.Users[0], resp.Users[1]
	if err := os.WriteFile(filepath.Join(r.cfg.StorePath, "work", repo.Name, "notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.now = r.now.Add(time.Hour)
	resp = r.remove(h, repo)
	if resp.Error != "" || len(resp.Users) != 1 || resp.Users[0].Name != repo.Name {
		t.Fatalf("remove: %+v", resp)
	}
	if _, ok := r.sys.users[repo.Name]; ok {
		t.Errorf("the account is still there")
	}
	if _, ok := r.sys.groups[repo.Name]; ok {
		t.Errorf("the group is still there")
	}
	archive := filepath.Join(r.cfg.StorePath, "trash", fmt.Sprintf("%s-%d", repo.Name, r.now.Unix()))
	for _, a := range storeAreas {
		if _, err := os.Lstat(filepath.Join(r.cfg.StorePath, a, repo.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s/%s stayed: %v", a, repo.Name, err)
		}
		p := filepath.Join(archive, a)
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("archived %s: %v", p, err)
		}
		if r.chowns[filepath.Join(r.cfg.StorePath, a, repo.Name)] != [2]int{r.cfg.StoreOwner, r.cfg.StoreOwner} {
			t.Errorf("%s wasn't given to root first: %v", a, r.chowns)
		}
	}
	if b, err := os.ReadFile(filepath.Join(archive, "work", "notes")); err != nil || string(b) != "x" {
		t.Errorf("the work wasn't kept: %v", err)
	}
	st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if len(st.Users) != 1 || st.Users[0].Name != org.Name || st.Retired != 1 || st.Pending != 0 ||
		len(st.Removed) != 1 || st.Removed[0].Name != repo.Name || st.Removed[0].Job != 9 || st.Removed[0].Archive != archive {
		t.Fatalf("status %+v", st)
	}

	// Removing it again is done already; a name that isn't the key's is refused.
	if resp := r.remove(h, repo); resp.Error != "" {
		t.Errorf("again: %+v", resp)
	}
	wrong := org
	wrong.Key = "1/someone"
	if resp := r.remove(h, wrong); !strings.Contains(resp.Error, "isn't a machine user") {
		t.Errorf("wrong key: %+v", resp)
	}
	// An ensure_users spec doesn't remove.
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpRemoveUser, Spec: r.spec(3, waypointRepo)}); !strings.Contains(resp.Error, "is for ensure_users") {
		t.Errorf("wrong op: %+v", resp)
	}

	// The repo's next job gets a fresh user with a new uid.
	resp = ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" || resp.Users[0].Name != repo.Name || resp.Users[0].UID == repo.UID || resp.Users[1].UID != org.UID {
		t.Errorf("again: %+v", resp)
	}
}

func TestRemoveUserFails(t *testing.T) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo))
	repo := resp.Users[0]
	r.sys.failDelUser = 1
	if resp := r.remove(h, repo); !strings.Contains(resp.Error, "used by process") {
		t.Fatalf("%+v", resp)
	}
	st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if st.Pending != 1 || len(st.Users) != 1 || len(st.Drift) != 1 {
		t.Fatalf("status %+v", st)
	}
	// Meanwhile the repo's jobs get another user, not the one going away.
	resp = ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" || resp.Users[0].Name == repo.Name || resp.Users[0].UID == repo.UID {
		t.Errorf("while removing: %+v", resp)
	}
	// A signed reconcile finishes it.
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpReconcile}); !strings.Contains(resp.Error, "no spec") {
		t.Errorf("unsigned reconcile: %+v", resp)
	}
	resp = h.Handle(sandbox.Request{Op: sandbox.OpReconcile, Spec: r.sign(&sandbox.Spec{Op: sandbox.OpReconcile, Runner: 3})})
	if resp.Error != "" || resp.Status == nil || resp.Status.Pending != 0 || len(resp.Status.Drift) != 0 || len(resp.Status.Removed) != 1 {
		t.Fatalf("reconcile: %+v", resp)
	}
	if _, ok := r.sys.users[repo.Name]; ok {
		t.Errorf("still there after reconcile")
	}
}
