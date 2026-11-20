// Command dep-age fails when a commit adds a dependency version published in
// the last week (WP-844, internal/depage):
//
//	go run ./cmd/dep-age [-base origin/main] [-head HEAD] [-min-age 168h] [-v]
//
// It compares the go.sum, package-lock.json files and renovate.json-tracked
// pins of -head with -base's (both git revisions; run it from the repo's
// root), and asks the npm registry and Go's module proxy, checksum database
// and index how old each version head adds is. A version can land early with
// a reason in .dep-age-waivers. CI's lint job and the pre-push hook run it.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/depage"
)

func main() {
	ok, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "dep-age:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

func run(args []string) (bool, error) {
	fs := flag.NewFlagSet("dep-age", flag.ContinueOnError)
	base := fs.String("base", "origin/main", "the `revision` whose versions are already accepted")
	head := fs.String("head", defaultHead(os.Getenv), "the `revision` to check")
	minAge := fs.Duration("min-age", 7*24*time.Hour, "how long a version must have been published")
	verbose := fs.Bool("v", false, "list the versions that passed too")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	for _, rev := range []string{*base, *head} {
		if rev == "" || strings.HasPrefix(rev, "-") {
			return false, fmt.Errorf("%q isn't a revision", rev)
		}
	}
	if _, err := git("rev-parse", "--verify", "--quiet", *base+"^{commit}"); err != nil {
		return false, fmt.Errorf("no revision %s to compare with (git fetch origin main)", *base)
	}
	headTree, err := tree(*head)
	if err != nil {
		return false, err
	}
	baseTree, err := tree(*base)
	if err != nil {
		return false, err
	}
	rules, err := depage.RenovateRules(headTree, baseTree)
	if err != nil {
		return false, err
	}
	hd, err := depage.Collect(headTree, rules)
	if err != nil {
		return false, fmt.Errorf("%s: %w", *head, err)
	}
	bd, err := depage.Collect(baseTree, rules)
	if err != nil {
		return false, fmt.Errorf("%s: %w", *base, err)
	}
	added := depage.New(hd, bd)
	if len(added) == 0 {
		fmt.Printf("dep-age: no dependency versions that %s doesn't have\n", *base)
		return true, nil
	}
	waivers := map[string]depage.Waiver{}
	if b, err := headTree.Read(depage.WaiverFile); err != nil {
		return false, err
	} else if b != nil {
		if waivers, err = depage.ParseWaivers(b); err != nil {
			return false, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	results := depage.Check(ctx, depage.NewHTTP(), added, waivers, time.Now(), *minAge)
	return depage.Report(os.Stdout, results, *minAge, *verbose, os.Getenv("GITHUB_ACTIONS") == "true"), nil
}

// tree reads a revision's files from git, not the working tree, so a
// pre-push check sees what is pushed (see defaultHead).
func tree(rev string) (depage.Tree, error) {
	out, err := git("ls-tree", "-r", "-z", "--name-only", rev)
	if err != nil {
		return depage.Tree{}, err
	}
	files := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	return depage.Tree{Files: files, Read: func(path string) ([]byte, error) {
		if !have[path] {
			return nil, nil
		}
		s, err := git("cat-file", "blob", rev+":"+path)
		return []byte(s), err
	}}, nil
}

// defaultHead is the commit a pre-commit pre-push hook is pushing (one ref
// per push: pre-commit names the first), else HEAD.
func defaultHead(getenv func(string) string) string {
	if s := getenv("PRE_COMMIT_TO_REF"); s != "" {
		return s
	}
	return "HEAD"
}

func git(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("git", args...) // #nosec G204 -- git with this command's own arguments (revisions from its flags)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}
