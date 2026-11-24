//go:build linux

package sandboxd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// storeAreas are the store's top directories, each with one directory per
// machine user.
var storeAreas = []string{"homes", "work", "cache"}

const xfsMagic = 0x58465342

type store struct {
	path  string
	owner int
	fd    int
	pin   pin
	chown func(fd, uid, gid int) error
}

// openStore checks the store's owner and mode and opens it.
func openStore(path string, owner int) (*store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the store %s isn't an absolute path", path)
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, fmt.Errorf("the store %s: %w", path, err)
	}
	if err := checkDir(path, &st, owner); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("the store %s: %w", path, err)
	}
	s := &store{path: path, owner: owner, fd: fd, chown: unix.Fchown}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := checkDir(path, &st, owner); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	s.pin = pin{Dev: st.Dev, Ino: st.Ino}
	for _, a := range append(append([]string{}, storeAreas...), sessionsArea) {
		if _, err := s.dir(a, owner, owner, 0o755); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

// checkDir: a real directory (not a symlink), owned by owner, that nobody
// else can write.
func checkDir(path string, st *unix.Stat_t, owner int) error {
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return fmt.Errorf("the store %s isn't a directory", path)
	case int(st.Uid) != owner:
		return fmt.Errorf("the store %s is owned by uid %d, not %d", path, st.Uid, owner)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("the store %s can be written by others (mode %o): it must be 0755", path, st.Mode&0o7777)
	}
	return nil
}

func (s *store) close() {
	if s.fd >= 0 {
		_ = unix.Close(s.fd)
		s.fd = -1
	}
}

// beneath opens rel (no "..", no symlinks on the way) below the store.
func (s *store) beneath(rel string, flags uint64) (int, error) {
	return unix.Openat2(s.fd, rel, &unix.OpenHow{
		Flags:   flags | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
}

// dir makes sure rel is a directory owned by uid:gid with mode, making it
// if it's missing. Its parent must exist; rel's last part is one name.
func (s *store) dir(rel string, uid, gid int, mode uint32) (string, error) {
	parent, name := filepath.Split(rel)
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("bad store path %q", rel)
	}
	pfd := s.fd
	if parent != "" {
		fd, err := s.beneath(strings.TrimSuffix(parent, "/"), unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return "", fmt.Errorf("store %s: %w", parent, err)
		}
		defer func() { _ = unix.Close(fd) }()
		pfd = fd
	}
	if err := unix.Mkdirat(pfd, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return "", fmt.Errorf("store %s: %w", rel, err)
	}
	fd, err := unix.Openat2(pfd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return "", fmt.Errorf("store %s: %w", rel, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		if err := s.chown(fd, uid, gid); err != nil {
			return "", fmt.Errorf("store %s: %w", rel, err)
		}
	}
	if st.Mode&0o7777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			return "", fmt.Errorf("store %s: %w", rel, err)
		}
	}
	return filepath.Join(s.path, rel), nil
}

// userDirs makes a machine user's directories: home, work and cache,
// 0700 and theirs.
func (s *store) userDirs(name string, uid, gid int) error {
	for _, a := range storeAreas {
		if _, err := s.dir(a+"/"+name, uid, gid, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// trash holds removed users' store directories, root's alone, until a
// person deletes them: the helper never deletes a sandbox's work.
const trash = "trash"

// archive moves a user's store directories into trash/<entry>/<area>,
// each first given to root, so the retired uid's files are out of every
// sandbox's reach. It returns the archive's path, and is safe to repeat.
func (s *store) archive(name, entry string) (string, error) {
	if strings.ContainsAny(name, "/.") || strings.ContainsAny(entry, "/") || name == "" || entry == "" || entry[0] == '.' {
		return "", fmt.Errorf("bad store name %q", name)
	}
	if _, err := s.dir(trash, s.owner, s.owner, 0o700); err != nil {
		return "", err
	}
	dst := trash + "/" + entry
	if _, err := s.dir(dst, s.owner, s.owner, 0o700); err != nil {
		return "", err
	}
	dfd, err := s.beneath(dst, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return "", fmt.Errorf("store %s: %w", dst, err)
	}
	defer func() { _ = unix.Close(dfd) }()
	for _, a := range storeAreas {
		afd, err := s.beneath(a, unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return "", fmt.Errorf("store %s: %w", a, err)
		}
		err = s.moveOut(afd, a, name, dfd)
		_ = unix.Close(afd)
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(s.path, dst), nil
}

// moveOut gives area/name to root and renames it to area below dfd; a
// name already moved is skipped.
func (s *store) moveOut(afd int, area, name string, dfd int) error {
	fd, err := unix.Openat2(afd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store %s/%s: %w", area, name, err)
	}
	err = s.chown(fd, s.owner, s.owner)
	if err == nil {
		err = unix.Fchmod(fd, 0o700)
	}
	_ = unix.Close(fd)
	if err != nil {
		return fmt.Errorf("store %s/%s: %w", area, name, err)
	}
	if err := unix.Renameat2(afd, name, dfd, area, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("store %s/%s: %w", area, name, err)
	}
	return nil
}

func (s *store) home(name string) string {
	return filepath.Join(s.path, "homes", name)
}

// status says how the store is: still the pinned directory, a mount of
// its own, which filesystem and how much room.
func (s *store) status(pinned *pin) sandbox.StoreStatus {
	st := sandbox.StoreStatus{Path: s.path, OK: true}
	var now unix.Stat_t
	if err := unix.Lstat(s.path, &now); err != nil {
		st.OK, st.Problem = false, err.Error()
		return st
	}
	if pinned != nil && (now.Dev != pinned.Dev || now.Ino != pinned.Ino) {
		st.OK, st.Problem = false, "the store directory was replaced since the helper pinned it"
	} else if err := checkDir(s.path, &now, s.owner); err != nil {
		st.OK, st.Problem = false, err.Error()
	}
	var parent unix.Stat_t
	if unix.Stat(filepath.Dir(s.path), &parent) == nil {
		st.Mounted = parent.Dev != now.Dev
	}
	var fsst unix.Statfs_t
	if unix.Fstatfs(s.fd, &fsst) == nil {
		if fsst.Type == xfsMagic {
			st.FSType = "xfs"
		} else {
			st.FSType = fmt.Sprintf("0x%x", fsst.Type)
		}
		st.Free = int64(fsst.Bavail) * fsst.Bsize // #nosec G115 -- block counts fit
	}
	return st
}

func kvm() bool {
	fi, err := os.Stat("/dev/kvm")
	return err == nil && fi.Mode()&fs.ModeCharDevice != 0
}

// sessionsArea holds the Contained sessions' workspaces (WP-470):
// sessions/<id> is root's, and its ws and home are the repo user's.
const sessionsArea = "sessions"

// sessionDirs makes a session's directories; home only for an unattended
// session, whose home goes with it.
func (s *store) sessionDirs(id string, uid int, home bool) (dir string, err error) {
	if dir, err = s.dir(sessionsArea+"/"+id, s.owner, s.owner, 0o755); err != nil {
		return "", err
	}
	if _, err = s.dir(sessionsArea+"/"+id+"/ws", uid, uid, 0o700); err != nil {
		return "", err
	}
	if home {
		if _, err = s.dir(sessionsArea+"/"+id+"/home", uid, uid, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (s *store) sessionPath(id string) string { return filepath.Join(s.path, sessionsArea, id) }

// sessionIDs are the sessions with directories in the store.
func (s *store) sessionIDs() ([]string, error) {
	fd, err := s.beneath(sessionsArea, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), sessionsArea)
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// sessionOwner is the uid that owns a session's workspace (or home);
// found is false when neither is there.
func (s *store) sessionOwner(id string) (uid int, found bool, err error) {
	for _, sub := range []string{"ws", "home"} {
		fd, err := s.beneath(sessionsArea+"/"+id+"/"+sub, unix.O_PATH|unix.O_DIRECTORY)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		var st unix.Stat_t
		err = unix.Fstat(fd, &st)
		_ = unix.Close(fd)
		if err != nil {
			return 0, false, err
		}
		return int(st.Uid), true, nil
	}
	return 0, false, nil
}

// sessionLeft says which of a session's directories hold anything.
func (s *store) sessionLeft(id string) (ws, home bool) {
	empty := func(sub string) bool {
		fd, err := s.beneath(sessionsArea+"/"+id+"/"+sub, unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			return true
		}
		f := os.NewFile(uintptr(fd), sub)
		defer func() { _ = f.Close() }()
		names, _ := f.Readdirnames(1)
		return len(names) == 0
	}
	return !empty("ws"), !empty("home")
}

// removeSession takes a session's emptied directories away. It removes
// only empty directories: the user's own files go first, as that user.
func (s *store) removeSession(id string) error {
	if !sandbox.ValidSessionID(id) {
		return fmt.Errorf("bad session id %q", id)
	}
	fd, err := s.beneath(sessionsArea+"/"+id, unix.O_PATH|unix.O_DIRECTORY)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, sub := range []string{"ws", "home"} {
		if err := unix.Unlinkat(fd, sub, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			_ = unix.Close(fd)
			return fmt.Errorf("store %s/%s/%s: %w", sessionsArea, id, sub, err)
		}
	}
	_ = unix.Close(fd)
	afd, err := s.beneath(sessionsArea, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(afd) }()
	if err := unix.Unlinkat(afd, id, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("store %s/%s: %w", sessionsArea, id, err)
	}
	return nil
}
