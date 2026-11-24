// Package leftover is a sandboxed job's last step (WP-471, PL7 §8.1): it
// runs inside the sandbox as the repo user, commits what the agent left
// uncommitted, and writes the job's commits as a git bundle to standard
// output. The runner reads that stream (a descriptor it handed the
// sandbox, never a path in the sandbox's tree) and imports it on the host
// with pushgate.Gateway.Import, so the host never runs git in a gitdir a
// sandbox could write.
//
// Nothing here is trusted by the host: the sandbox could send any bytes
// instead. It exists so an honest session's work comes back.
package leftover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Options say what to bundle.
type Options struct {
	// Dir is the job's work tree.
	Dir string
	// Base is the commit the job started from: the bundle holds the
	// commits after it.
	Base string
	// Message is the leftover commit's message; empty is "Agent changes".
	Message string
	// Git is the git binary; empty finds it on PATH.
	Git string
}

// Result says what the step did.
type Result struct {
	// Committed: there were uncommitted changes, now a commit.
	Committed bool
	// Commits counts the commits after Base; zero writes no bundle.
	Commits int
}

// DefaultMessage is the leftover commit's message when the job gives none.
const DefaultMessage = "Agent changes"

var oidRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Bundle commits the work tree's leftovers and writes the commits after
// Base to out as a bundle whose one ref is HEAD. With no commits after
// Base it writes nothing: an empty stream is "no work".
func Bundle(ctx context.Context, o Options, out io.Writer) (*Result, error) {
	if !oidRE.MatchString(o.Base) {
		return nil, fmt.Errorf("the base %q isn't a full commit id", o.Base)
	}
	bin := o.Git
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("git"); err != nil {
			return nil, err
		}
	}
	git := func(stdin []byte, w io.Writer, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...) // #nosec G204 -- fixed binary; args are ours or a checked commit id
		cmd.Dir = o.Dir
		var buf, errb bytes.Buffer
		cmd.Stdout = &buf
		if w != nil {
			cmd.Stdout = w
		}
		cmd.Stderr = &errb
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i]
			}
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, msg)
		}
		return strings.TrimSpace(buf.String()), nil
	}

	res := &Result{}
	status, err := git(nil, nil, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if status != "" {
		if _, err := git(nil, nil, "add", "-A"); err != nil {
			return nil, err
		}
		msg := strings.TrimSpace(o.Message)
		if msg == "" {
			msg = DefaultMessage
		}
		args := []string{"commit", "--quiet", "--no-verify", "-F", "-"}
		if email, _ := git(nil, nil, "config", "user.email"); email == "" {
			args = append([]string{"-c", "user.name=Waypoint runner", "-c", "user.email=runner@waypoint.invalid"}, args...)
		}
		if _, err := git([]byte(msg), nil, args...); err != nil {
			return nil, err
		}
		res.Committed = true
	}
	if _, err := git(nil, nil, "merge-base", "--is-ancestor", o.Base, "HEAD"); err != nil {
		return nil, fmt.Errorf("HEAD doesn't build on the base %.12s: %w", o.Base, err)
	}
	n, err := git(nil, nil, "rev-list", "--count", o.Base+"..HEAD")
	if err != nil {
		return nil, err
	}
	if res.Commits, err = strconv.Atoi(n); err != nil {
		return nil, errors.New("git rev-list --count: " + n)
	}
	if res.Commits == 0 {
		return res, nil
	}
	if _, err := git(nil, out, "bundle", "create", "-q", "-", "HEAD", "--not", o.Base); err != nil {
		return nil, err
	}
	return res, nil
}
