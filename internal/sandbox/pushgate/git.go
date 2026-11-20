package pushgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The gateway's git never reads a config, hook or attribute file that it
// didn't write: no system or global config, a fixed repository config that
// is rewritten on every start, no hooks, no replace refs, literal
// pathspecs, and an environment built from nothing (PL7 §7.2, §8.1).

// mirrorConfig is the whole of a mirror's config. It is written over
// whatever is there each time the gateway opens the mirror.
const mirrorConfig = `[core]
	repositoryformatversion = 0
	bare = true
	filemode = true
	hooksPath = /dev/null
	fsmonitor = false
	logAllRefUpdates = false
[gc]
	auto = 0
[transfer]
	fsckObjects = true
[fetch]
	fsckObjects = true
[receive]
	fsckObjects = true
`

// gitEnv is the environment of every git the gateway runs.
func (g *Gateway) gitEnv(extra ...string) []string {
	env := []string{
		"PATH=" + g.path,
		"HOME=" + filepath.Join(g.root, "home"),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	return append(env, extra...)
}

// git runs one git command in a mirror. args never start from text a
// sandbox chose: refs and object ids are checked before they get here.
func (g *Gateway) git(ctx context.Context, m *mirror, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, g.gitBin, append(m.protocols(), args...)...) // #nosec G204 G702 -- fixed binary, args checked
	cmd.Dir = m.dir
	cmd.Env = g.gitEnv(append([]string{"GIT_DIR=" + m.dir}, env...)...)
	cmd.Stdin = stdin
	cmd.Stdout = &out
	cmd.Stderr = &limitedBuffer{max: 64 << 10, b: &errb}
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), &gitError{args: args, err: err, stderr: errb.String()}
	}
	return out.Bytes(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	cmd := "git"
	if len(e.args) > 0 {
		cmd += " " + e.args[0]
	}
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		return fmt.Sprintf("%s: %v", cmd, e.err)
	}
	return fmt.Sprintf("%s: %v: %s", cmd, e.err, firstLine(msg))
}

func (e *gitError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

type limitedBuffer struct {
	max int
	b   *bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// mirror is the host-owned bare repository a push lands in before it goes
// on to the forge. It mirrors the forge's branches and tags; nothing else.
type mirror struct {
	repo *Repo
	dir  string
	// scheme is the forge remote's transport: the only one git may use.
	scheme string
}

func (m *mirror) protocols() []string {
	return []string{"-c", "protocol.allow=never", "-c", "protocol." + m.scheme + ".allow=always"}
}

// remoteScheme says which git transport a remote uses: ssh for the forge
// (the deploy key), file for tests.
func remoteScheme(remote string) (string, error) {
	switch {
	case strings.HasPrefix(remote, "ssh://"):
		if _, err := url.Parse(remote); err != nil {
			return "", err
		}
		return "ssh", nil
	case strings.HasPrefix(remote, "/"):
		return "file", nil
	}
	return "", fmt.Errorf("remote %q: use ssh://user@host/owner/name.git (a deploy key) or an absolute path", remote)
}

// open makes the mirror if it is missing and writes its fixed config.
func (g *Gateway) open(ctx context.Context, r *Repo) (*mirror, error) {
	scheme, err := remoteScheme(r.Remote)
	if err != nil {
		return nil, err
	}
	m := &mirror{repo: r, dir: filepath.Join(g.root, "repos", filepath.FromSlash(r.Path)+".git"), scheme: scheme}
	if _, err := os.Stat(filepath.Join(m.dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(m.dir), 0o700); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, g.gitBin, "init", "--quiet", "--bare", "--template=", m.dir) // #nosec G204 G702 -- fixed binary and our own path
		cmd.Env = g.gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git init %s: %v: %s", m.dir, err, firstLine(string(out)))
		}
	}
	for _, d := range []string{"hooks", "info"} {
		_ = os.RemoveAll(filepath.Join(m.dir, d))
	}
	if err := os.WriteFile(filepath.Join(m.dir, "config"), []byte(mirrorConfig), 0o600); err != nil {
		return nil, err
	}
	// Alternates the gateway didn't write could pull in objects from
	// anywhere; grafts could rewrite history the checks read.
	_ = os.Remove(filepath.Join(m.dir, "objects", "info", "alternates"))
	_ = os.Remove(filepath.Join(m.dir, "shallow"))
	return m, nil
}

// remoteEnv is how git reaches the forge: ssh with the repo's deploy key
// and nothing else (no agent, no ssh config, a fixed known_hosts).
func (m *mirror) remoteEnv() []string {
	if m.scheme != "ssh" {
		return nil
	}
	opts := []string{
		"ssh", "-F", "/dev/null",
		"-i", shellQuote(m.repo.KeyFile),
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + shellQuote(m.repo.KnownHosts),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
	}
	return []string{"GIT_SSH_COMMAND=" + strings.Join(opts, " "), "GIT_SSH_VARIANT=ssh", "SSH_AUTH_SOCK="}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fetch brings the mirror up to the forge's branches and tags, and learns
// the forge's default branch.
func (g *Gateway) fetch(ctx context.Context, m *mirror) (head string, err error) {
	if _, err := g.git(ctx, m, m.remoteEnv(), nil, "fetch", "--quiet", "--prune", "--no-tags",
		"--no-write-fetch-head", "--no-recurse-submodules", "--", m.repo.Remote,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", err
	}
	out, err := g.git(ctx, m, m.remoteEnv(), nil, "ls-remote", "--symref", "--", m.repo.Remote, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: "); ok {
			if ref, _, ok := strings.Cut(rest, "\t"); ok {
				return ref, nil
			}
		}
	}
	return "", nil
}

// refs reads the mirror's branches and tags.
func (g *Gateway) refs(ctx context.Context, m *mirror) (map[string]string, []string, error) {
	out, err := g.git(ctx, m, nil, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/", "refs/tags/")
	if err != nil {
		return nil, nil, err
	}
	refs := map[string]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs[name] = oid
		order = append(order, name)
	}
	return refs, order, nil
}
