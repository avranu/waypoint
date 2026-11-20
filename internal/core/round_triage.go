package core

// Packet triage (WP-437, WP-ADR30). A third of the babysitting rounds on 2026-11-02
// pushed nothing, and most of them could have been told in advance without
// an agent: an empty work packet, a comment already fixed on the head's
// history and listed again, and free-model findings claiming code that
// builds doesn't compile. Before a round goes out, Waypoint settles what a
// fact on hand settles, and nothing else:
//
//   - a packet with nothing in it starts no round (babysitRound);
//   - a fixed comment that an edit on the forge only reworded stays fixed
//     while its fix is on the head (syncReviewComments);
//   - a bot's claim that the code doesn't compile, a name is undefined or an
//     import is broken is rejected when this head's build passed (here).
//
// Only deterministic facts decide: the head's green checks, the head's
// history and the comment's own text. The text match is narrow on purpose
// (compile and import vocabulary in a typed source file); anything it
// doesn't match goes to the round as before. Each rejection is recorded by
// "triage", so how often one is overturned can be counted, and an agent or
// a person can still accept the comment.

import (
	"context"
	"fmt"
	"path"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// triageBy is who rejects a compile claim: the babysitter, under its own
// name so its verdicts can be told apart from a round's.
var triageBy = rcBy{kind: "babysitter", name: "triage"}

// compileClaimRE: a comment's headline saying the code doesn't build.
// Tried on the 1,066 review comments of 300 pull requests on 2026-11-02:
// 106 bot comments matched, and none of the 85 given a verdict was taken
// (84 rejected as false, one dismissed); it caught 61 of the 71 rejections
// whose note said the code builds. Runtime faults ("TypeError", "nil
// pointer", "crashes when x is undefined") stay out.
var compileClaimRE = lazyre.New(`(?i)` + strings.Join([]string{
	`\b(?:fails? to|won'?t|will not|does not|doesn'?t|cannot|can'?t) compile\b`,
	`\bcompil(?:e|ation|er)[- ](?:time )?(?:errors?|failures?|fails?)\b`,
	`\b(?:causing|cause|causes|results? in|leads? to|lead to|produces?) (?:a |an )?(?:compile|compilation|build|syntax|type ?script(?: type)?|type) errors?\b`,
	`\btype ?script (?:type |compil(?:e|ation) )?errors?\b`,
	`\b(?:breaks?|breaking|will break|would break) the build\b`,
	`\bbuild (?:will )?(?:fail|fails|break|breaks)\b`,
	`\bsyntax error\b`,
	// Compiler wording only: "err is redeclared with := so the error is
	// lost", a duplicate JSON field tag or a duplicate function body are
	// logic points that build fine (WP-579).
	`\bredeclared in (?:this|the same) (?:block|scope)\b`,
	`\bcannot redeclare\b`,
	`\bduplicate (?:import|identifier|declaration)s?\b`,
	`\b(?:missing|broken|unresolved) imports?\b`,
	`\bimports? (?:is|are) (?:broken|missing)\b`,
	`\b(?:is|are) not imported\b`,
	`\b(?:uses?|used|calls?|called|references?|referenced)\b.{0,60}?\bwithout (?:being )?import(?:ed|ing)\b`,
	`\bundefined (?:identifier|variable|function|symbol|name|type|field|method|reference to)s?\b`,
	`\b(?:references?|uses?|calls?) (?:an |the )?undefined (?:variable|function|identifier|symbol|name|type|field|method)s?\b`,
	`\b(?:used|referenced|called) but (?:is )?not (?:defined|declared)\b`,
	`\bcannot find (?:name|module|symbol|package)\b`,
	`\bundeclared name\b`,
}, "|"))

// typedSources are the files a green build vouches for: a compiler or
// type checker reads them, so a broken name or import fails the build.
var typedSources = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".rs": true,
	".java": true, ".kt": true, ".scala": true, ".swift": true, ".cs": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
}

// compileClaim: a bot's comment on a typed source file says it doesn't
// build. A person's word is never second-guessed, and a comment on the
// whole pull request (no file) isn't either.
func (rc *PRReviewComment) compileClaim() bool {
	if rc.Path == "" || !typedSources[strings.ToLower(path.Ext(rc.Path))] || !rc.botComment() {
		return false
	}
	return compileClaimRE.MatchString(headline(rc.Body))
}

// leadBadgeRE: the severity a finding opens with, "**P1** high · " or
// "[high] ".
var (
	leadBadgeRE = lazyre.New(`^(?:\*\*P[0-3]\*\* \w+ · |\[\w+\] )`)
	sentenceRE  = lazyre.New(`[.!?](?:\s|$)`)
)

// headline is a bot comment's first sentence, past a relay's header, a
// severity line and a leading badge: what the comment is about. Only it is
// tried for a compile claim, so a comment that leads with another point
// ("dead code; it also uses slices without importing it") goes to the
// round whole.
func headline(body string) string {
	for _, line := range strings.Split(stripMarkers(body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, "_"), relayedAuthorRE.MatchString(line), crSeverityRE.MatchString(line):
			continue
		case strings.HasPrefix(line, "```"), strings.HasPrefix(line, "<"):
			return ""
		}
		line = strings.ReplaceAll(leadBadgeRE.ReplaceAllString(line, ""), "**", "")
		if i := strings.Index(line, "```"); i >= 0 {
			line = line[:i]
		}
		if i := strings.Index(line, "<sub>"); i >= 0 {
			line = line[:i]
		}
		if loc := sentenceRE.FindStringIndex(line); loc != nil {
			line = line[:loc[0]+1]
		}
		return line
	}
	return ""
}

// builtChecks names the head's passing checks that ran: Waypoint's own and
// skipped ones vouch for nothing.
func builtChecks(checks []PRCheck) []string {
	var out []string
	for _, c := range checks {
		if c.State == CheckSuccess && !ownCheck(c.Name) && !strings.Contains(strings.ToLower(c.Description), "skip") {
			out = append(out, c.Name)
		}
	}
	return out
}

// triageComments rejects the compile claims a green build on this head
// answers, before they reach a round. It only acts once every check on the
// head has passed: then nothing in the build failed, whatever the claim
// names.
func (s *Service) triageComments(ctx context.Context, b *Babysit, pr *PullRequest, checks []PRCheck, comments []*PRReviewComment) {
	if pr.CIState != CIPassed || !sameSHA(pr.ciSHA, pr.HeadSHA) {
		return
	}
	built := builtChecks(checks)
	if len(built) == 0 {
		return
	}
	for _, rc := range comments {
		if rc.Dealt || rc.Gone || (rc.Verdict != VerdictNew && rc.Verdict != VerdictSeen) || !rc.compileClaim() {
			continue
		}
		if err := s.markComment(ctx, pr, rc, VerdictRejected, builtNote(built, pr.HeadSHA), "", triageBy, b.Round); err != nil {
			s.log.WarnContext(ctx, "triage a compile claim", "pr", pr.ID, "comment", rc.Ref, "err", err)
			continue
		}
		rc.Dealt = true
	}
}

// builtNote is what the rejection says: which checks passed on which head.
func builtNote(built []string, sha string) string {
	names := built
	more := ""
	if len(names) > 3 {
		names, more = names[:3], fmt.Sprintf(" and %d more", len(built)-3)
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return fmt.Sprintf("it builds: %s%s passed on %s, so the claim that it doesn't compile doesn't hold. (Checked by Waypoint before a round; accept it again if the build doesn't cover this file.)",
		joinAnd(quoted), more, short(sha))
}
