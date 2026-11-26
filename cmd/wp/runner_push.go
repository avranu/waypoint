package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// errOnBranch is pushMoved finding nothing left to push: the moved branch
// already has the round's changes.
var errOnBranch = errors.New("the branch already has the round's changes")

// pushMoved is what a round does when the forge refuses its push because
// the branch moved while it ran (a person's session pushed, a merge of the
// base landed): the branch is fetched, the round's own commits (baseSHA..HEAD)
// are rebased onto its new tip, and the push is made once more. Dropping the
// round's work instead cost 6 of 237 rounds in #520 to #618 (WP-1914). A
// refusal for another reason, and a rebase that conflicts, are returned as
// they are. Protected files are checked again on the new range: a file the
// round never changed isn't the agent's to change.
func (p *runnerProc) pushMoved(ctx context.Context, run *jobRun, work string, env []string, in agentJob, baseSHA string, files []string, result map[string]any, pushErr error) error {
	if !strings.Contains(pushErr.Error(), "[rejected]") {
		return pushErr
	}
	ref := "refs/heads/" + in.Branch
	remote := "refs/remotes/origin/" + in.Branch
	if _, err := p.gitIn(ctx, work, nil, env, "fetch", "--quiet", "origin", "+"+ref+":"+remote); err != nil {
		return pushErr
	}
	tip, err := p.gitIn(ctx, work, nil, env, "rev-parse", "--verify", remote+"^{commit}")
	if err != nil || tip == baseSHA {
		return pushErr
	}
	fmt.Fprintf(run.out, "%s moved while the round ran; rebasing its commits onto %s\n", in.Branch, tip[:min(len(tip), 12)])
	if _, err := p.gitIn(ctx, work, nil, env, append(p.gitIdentity(ctx, work), "rebase", "--quiet", "--onto", tip, baseSHA)...); err != nil {
		_, _ = p.gitIn(ctx, work, nil, env, "rebase", "--abort")
		return fmt.Errorf("%s moved while the round ran and its commits don't rebase onto the new tip: %v", in.Branch, err)
	}
	revs, err := p.git(ctx, work, "rev-list", "--reverse", tip+"..HEAD")
	if err != nil {
		return err
	}
	commits := strings.Fields(revs)
	if len(commits) == 0 {
		// The rebase dropped every commit: the new tip has them, or the
		// same changes.
		result["commits"], result["files"], result["rebased"] = []string{}, []string{}, true
		delete(result, "commit")
		delete(result, "diff")
		return errOnBranch
	}
	names, err := p.gitPaths(ctx, work, "diff", "--name-only", "--no-renames", tip+"..HEAD")
	if err != nil {
		return err
	}
	added := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(files, n) })
	if bad := protectedFiles(in.ProtectPaths, added); len(bad) > 0 {
		return fmt.Errorf("%s moved while the round ran, and its commits now change protected files (%s), so nothing was pushed", in.Branch, strings.Join(bad, ", "))
	}
	if _, err := p.gitIn(ctx, work, nil, env, "push", "origin", ref+":"+ref); err != nil {
		return err
	}
	result["commits"], result["files"], result["commit"], result["rebased"] = commits, names, commits[len(commits)-1], true
	if diff, err := p.git(ctx, work, "diff", tip+"..HEAD"); err == nil {
		result["diff"] = capDiff(diff, maxResultDiff)
	}
	return nil
}
