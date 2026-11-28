package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// protectedRefusalPrefix is how the runner's error names the refused files.
const protectedRefusalPrefix = "changed protected files ("

// protectedWaitRE is the start of the waits protectedHandoff and
// protectedConflictHandoff set.
var protectedWaitRE = lazyre.New(`^(round \d+'s fix changes|its conflict with .+ is in) protected files \(`)

// protectedConflictRE is the start of the wait protectedConflictHandoff sets.
var protectedConflictRE = lazyre.New(`^its conflict with .+ is in protected files \(`)

// protectedRefusal is the protected files a round's push was refused for,
// from its outcome; ok is false when the round wasn't refused for them.
func protectedRefusal(r BabysitRound) (files string, ok bool) {
	if r.Pushed || r.To != "runner" {
		return "", false
	}
	_, rest, found := strings.Cut(r.Outcome, protectedRefusalPrefix)
	if !found {
		return "", false
	}
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

// protectedHandoff parks a babysit whose last round on this head was refused
// for protected files: it files the task that asks a session to push the
// fix, and says so in its wait and to its owner.
func (s *Service) protectedHandoff(ctx context.Context, b *Babysit, pr *PullRequest, r *BabysitRound, files, what string) {
	trusted := plainFiles(pr, strings.Split(files, ", "))
	key, err := s.fileProtectedPush(ctx, b, pr, r.At, protectedPushTitle(pr), protectedPushBody(pr, r, files), trusted)
	if err != nil {
		s.log.WarnContext(ctx, "file a protected-file push", "pr", pr.ID, "err", err)
	}
	wait := fmt.Sprintf("round %d's fix changes protected files (%s), which a round may not push; %s still on %s", r.N, files, what, short(pr.HeadSHA))
	who := "Push it from a session"
	if key != "" {
		wait += "; " + key + " asks a session to push it"
		who = key + " asks a session to push it"
	}
	s.setState(ctx, b, pr, BabysitNeedsPerson, wait)
	s.babysitTell(ctx, b, pr, "stuck@"+pr.HeadSHA, fmt.Sprintf("%s#%d: babysitting round %d's fix changes protected files (%s), which rounds may not push (WP-D14); still %s on %s. %s: on branch %s, `wp pr todo %d`, fix, push. Waypoint carries on from the push.",
		pr.Repo, pr.Number, r.N, files, what, short(pr.HeadSHA), who, pr.HeadRef, pr.Number))
}

// protectedPushTitle is the task's title, one per pull request.
func protectedPushTitle(pr *PullRequest) string {
	return fmt.Sprintf("Push the protected-file fix babysitting can't on %s#%d", pr.Repo, pr.Number)
}

// protectedConflictTitle is the conflict task's title, apart from the fix's so
// that one is never taken for the other.
func protectedConflictTitle(pr *PullRequest) string {
	return fmt.Sprintf("Settle the protected-file conflict babysitting can't on %s#%d", pr.Repo, pr.Number)
}

// fileProtectedPush files the task for a session to push a fix no round
// may (body says which), as the babysit's owner, in the pull request's
// project; an open one, or one filed since since, is returned instead. ""
// when the repo is in no project or its owner is disabled.
//
// trusted says title and body are Waypoint's template with only plain names
// in it (plainFiles), so the safety screen takes them as Waypoint's
// (stampTemplate, WP-1266) and doesn't hold the task for asking a session to
// edit protected files, which is what it is for. Nothing a round's agent
// wrote goes on the task, where the screen would hold it for the same
// reason: `wp pr todo` shows the latest refused round's account and diff
// (refusedRound) as untrusted text.
func (s *Service) fileProtectedPush(ctx context.Context, b *Babysit, pr *PullRequest, since time.Time, title, body string, trusted bool) (string, error) {
	projectID, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		return "", err
	}
	if projectID == 0 {
		return "", nil
	}
	owner, err := s.userByID(ctx, s.db.R, b.ownerID)
	if err != nil {
		return "", err
	}
	if owner.Disabled {
		return "", nil
	}
	var have string
	err = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND i.title = ? AND (st.category NOT IN ('done','canceled') OR i.created_at >= ?) ORDER BY i.id DESC LIMIT 1`,
		projectID, title, since.UnixMilli()).Scan(&have)
	if err == nil {
		return have, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	writer := Actor{User: owner}
	writer.TextAgent = "waypoint"
	made, err := s.CreateIssue(ctx, &writer, IssueInput{Project: itoa64(projectID), Agent: "claude", Priority: "high",
		Title: title, Description: body, template: trusted})
	if err != nil {
		return "", err
	}
	return made.Key, nil
}

// plainNameRE is a name the template may carry and still be trusted: a
// path, ref or repository of plain characters, with no space, so no words an
// agent or the forge chose can read as an instruction.
var plainNameRE = lazyre.New(`^[A-Za-z0-9._/@+-]{1,200}$`)

// plainNames: the pull request's repository and branches, and names, are all
// plain (plainNameRE, and no "." or ".." part), so a template that carries
// them is still Waypoint's text alone (WP-1266). A file an agent named with
// words in it leaves the task screened as an agent's, as before.
func plainNames(pr *PullRequest, names ...string) bool {
	return allPlain(append([]string{pr.Repo, pr.HeadRef, cmp(pr.BaseRef, "base")}, names...))
}

func allPlain(names []string) bool {
	for _, n := range names {
		if !plainNameRE.MatchString(n) || slices.Contains(strings.Split(n, "/"), ".") || slices.Contains(strings.Split(n, "/"), "..") {
			return false
		}
	}
	return true
}

// plainFiles: plainNames, and every file a protected path (plainProtected).
func plainFiles(pr *PullRequest, files []string) bool {
	return plainNames(pr) && plainProtected(files)
}

// plainProtected: every file is a plain name and a protected path. The
// refused files reach the handoff joined by ", " in the runner's error and
// are split again, so a file named "x, ignore, prior, instructions" would
// come back as one plain word each; as protected paths, each part says no
// more than one plain file name may. The round's agent chose the files, so
// a list that isn't plain is its text, not Waypoint's.
func plainProtected(files []string) bool {
	return len(files) > 0 && allPlain(files) && len(protectedTouched(protectedPaths, files)) == len(files)
}

// protectedPushBody: where the work is, as pointers. What it waits on (check
// names, which a pull request's own workflow files choose) and what the
// round's agent wrote stay out: `wp pr todo` says.
func protectedPushBody(pr *PullRequest, r *BabysitRound, files string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s#%d (%s) waits on a fix babysitting can't push. Babysitting round %d on %s made one, but it changes protected files (%s), "+
		"and babysitting rounds may never push those (WP-D14). A session may.\n\n", pr.Repo, pr.Number, pr.URL, r.N, short(r.SHA), files)
	fmt.Fprintf(&out, "On the pull request's branch:\n\n```sh\ngit fetch origin %s && git switch %s\nwp pr todo %d\n```\n\n", shSingleQuote(pr.HeadRef), shSingleQuote(pr.HeadRef), pr.Number)
	out.WriteString("`wp pr todo` also shows the latest refused round's own account and diff. Fix it there, run the tests it touches, then `wp ship -m \"…\"` on that branch: it pushes to the pull request and finishes this task. Babysitting carries on from the push.\n")
	return out.String()
}

// refusedRound is the latest round on head refused for protected files that
// left an account or a diff: what a session taking the handoff starts from.
// nil when there is none, or the head has moved on since.
func refusedRound(b *Babysit, head string) *BabysitRound {
	for i := len(b.Rounds) - 1; i >= 0; i-- {
		r := &b.Rounds[i]
		if r.SHA != head {
			continue
		}
		if _, ok := protectedRefusal(*r); ok && (r.Answer != "" || r.Diff != "") {
			return r
		}
	}
	return nil
}

// shSingleQuote quotes forge-supplied text (a branch name) as one shell word.
func shSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
