package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// git runs git for a job and returns its trimmed output. It never prompts
// (a runner has no terminal) and the error carries what git said.
func (p *runnerProc) git(ctx context.Context, dir string, args ...string) (string, error) {
	return p.gitIn(ctx, dir, nil, nil, args...)
}

// gitIn is git with input and extra environment (KEY=value).
func (p *runnerProc) gitIn(ctx context.Context, dir string, stdin []byte, extra []string, args ...string) (string, error) {
	out, err := p.gitRaw(ctx, dir, stdin, extra, args...)
	return strings.TrimSpace(out), err
}

// gitPaths runs a git command that lists paths, with -z, and splits its
// output on NUL: nothing is trimmed and nothing is quoted, so a name with
// spaces, a leading blank or non-ASCII letters comes back as it is.
func (p *runnerProc) gitPaths(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := p.gitRaw(ctx, dir, nil, nil, append(append([]string(nil), args...), "-z")...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			paths = append(paths, f)
		}
	}
	return paths, nil
}

// gitRaw is git's untrimmed output.
func (p *runnerProc) gitRaw(ctx context.Context, dir string, stdin []byte, extra []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := procCommand(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(runnerGitEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "LC_ALL=C")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	cmd.Env = append(cmd.Env, extra...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := runProc(cmd); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if r := []rune(msg); len(r) > 600 {
			msg = "…" + string(r[len(r)-600:])
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), fmt.Errorf("git %s: %s", gitVerb(args), msg)
		}
		return "", fmt.Errorf("git %s: %w", gitVerb(args), err)
	}
	return out.String(), nil
}

// gitIdentity makes commits possible where no one has told git who they
// are (the config of a person's own repo wins when there is one).
func (p *runnerProc) gitIdentity(ctx context.Context, dir string) []string {
	if email, _ := p.git(ctx, dir, "config", "user.email"); email != "" {
		return nil
	}
	return []string{"-c", "user.name=Waypoint runner", "-c", "user.email=runner@waypoint.invalid"}
}

// commit commits whatever is staged, with message, and returns the commit.
// A person's own repo keeps its hooks; a scratch checkout of someone's code
// runs none.
func (p *runnerProc) commit(ctx context.Context, dir, message string, noVerify bool) (string, error) {
	args := append(p.gitIdentity(ctx, dir), "commit", "-F", "-")
	if noVerify {
		args = append(args, "--no-verify")
	}
	if _, err := p.gitIn(ctx, dir, []byte(message), nil, args...); err != nil {
		return "", err
	}
	return p.git(ctx, dir, "rev-parse", "HEAD")
}

// gitVerb is the subcommand in args (past any -c options), for messages.
func gitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--") { // --literal-pathspecs and the like
			continue
		}
		return args[i]
	}
	return ""
}

// push sends the current branch to its upstream, or to origin when it has
// none yet.
func (p *runnerProc) push(ctx context.Context, dir string) error {
	_, err := p.git(ctx, dir, "push")
	if err != nil && strings.Contains(err.Error(), "no upstream") {
		_, err = p.git(ctx, dir, "push", "--set-upstream", "origin", "HEAD")
	}
	return err
}
