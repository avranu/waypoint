//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// ownedByMe: the file belongs to the user running wp, not one planted in a
// shared temp directory by someone else.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// pathPrivate: no one but the user and root can change what p, a link-free
// path to fi, names until Claude Code's Read opens it. Every directory above
// it is the user's or root's, and no one else may write in it, unless it is
// sticky (a shared temp directory) and what p goes through in it is the
// user's or root's, so it can't be renamed or replaced. Otherwise someone
// else could swap the checked picture for a link to another file between the
// hook's answer and the Read. A directory group-writable by the user's own
// primary group counts as theirs (a private group, umask 002).
func pathPrivate(p string, fi os.FileInfo) bool {
	child := fi
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		di, err := os.Lstat(d)
		if err != nil || !di.IsDir() {
			return false
		}
		st, ok := di.Sys().(*syscall.Stat_t)
		if !ok || !mineOrRoot(st) {
			return false
		}
		m := di.Mode().Perm()
		othersWrite := m&0o002 != 0 || m&0o020 != 0 && int(st.Gid) != os.Getgid()
		if othersWrite {
			cs, ok := child.Sys().(*syscall.Stat_t)
			if di.Mode()&os.ModeSticky == 0 || !ok || !mineOrRoot(cs) {
				return false
			}
		}
		if d == filepath.Dir(d) {
			return true
		}
		child = di
	}
}

func mineOrRoot(st *syscall.Stat_t) bool { return st.Uid == 0 || int(st.Uid) == os.Getuid() }
