package api

import (
	_ "embed"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/wql"
)

// issueLine renders an issue on one line, for agents and terminals:
//
//	WAY-12 [In Progress] high bug "Login page crashes" @robin 3pts due:2026-10-20 parent:WAY-3
//
// The priority and type are left out when they say nothing: none, task.
func issueLine(i *core.Issue) string {
	var b strings.Builder
	b.WriteString(issueHead(i))
	issueAttrs(&b, i, "")
	// Provenance (WP-D11): text an agent wrote, and whether a person has
	// reviewed it; who closed it, if an agent did.
	if i.TextAgent != "" {
		b.WriteString(" written-by:" + quoteIfSpace(i.TextAgent))
		if i.Unreviewed {
			b.WriteString(" unreviewed")
		}
	}
	if i.ResolvedAt != nil && i.ResolvedAgent != "" {
		b.WriteString(" closed-by:" + quoteIfSpace(i.ResolvedAgent))
		if i.CloseUnreviewed {
			b.WriteString(" close-unreviewed") // WP-272
		}
	}
	return b.String()
}

// issueHead is an issue's key, status, priority, type and title.
func issueHead(i *core.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s]", i.Key, i.Status.Name)
	if i.Priority != 0 {
		b.WriteString(" " + core.PriorityName(i.Priority))
	}
	if i.Type != "" && i.Type != "task" {
		b.WriteString(" " + i.Type)
	}
	fmt.Fprintf(&b, " %q", i.Title)
	if i.Draft {
		b.WriteString(" draft") // WP-42: agents can't see it until a person publishes it
	}
	return b.String()
}

// issueAttrs adds what else an issue line shows: people, points, dates,
// place, tags and the agent work on it. It leaves the delegated agent out
// when it is author, who already named it.
func issueAttrs(b *strings.Builder, i *core.Issue, author string) {
	for _, a := range i.Assignees {
		b.WriteString(" @" + a.Username)
	}
	if i.Points != nil {
		fmt.Fprintf(b, " %gpts", *i.Points)
	}
	if i.DueDate != nil {
		b.WriteString(" due:" + *i.DueDate)
	}
	if i.SprintName != "" {
		b.WriteString(" sprint:" + quoteIfSpace(i.SprintName))
	}
	if i.Parent != nil {
		b.WriteString(" parent:" + i.Parent.Key)
	}
	if i.ChildCount > 0 {
		fmt.Fprintf(b, " children:%d/%d", i.ChildDone, i.ChildCount)
	}
	for _, l := range i.Labels {
		b.WriteString(" #" + strings.ReplaceAll(l.Name, " ", "_"))
	}
	if i.Category != "" {
		b.WriteString(" category:" + i.Category)
	}
	if i.Agent != "" && i.Agent != author {
		b.WriteString(" agent:" + quoteIfSpace(i.Agent))
	}
	if i.Step != "" {
		b.WriteString(" step:" + quoteIfSpace(i.Step))
	}
	if i.ClaimedBy != "" && i.ClaimedBy != author {
		b.WriteString(" claimed:" + quoteIfSpace(i.ClaimedBy))
	}
	if h := i.Hold; h != nil {
		// WP-269: no one takes it until then.
		if h.Until != nil && h.Waits == "" {
			b.WriteString(" on-hold-until:" + h.Until.Format(time.RFC3339))
		} else {
			b.WriteString(" on-hold")
		}
	}
}

func quoteIfSpace(s string) string {
	if strings.ContainsAny(s, " \t") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func issuesText(res *core.SearchResult) string {
	var b strings.Builder
	for _, i := range res.Issues {
		b.WriteString(issueLine(i))
		b.WriteByte('\n')
	}
	shown := res.Offset + len(res.Issues)
	if shown < res.Total {
		fmt.Fprintf(&b, "… %d of %d shown; use offset=%d for more\n", shown, res.Total, shown)
	} else if res.Total == 0 {
		b.WriteString("No issues match.\n")
	}
	return b.String()
}

func issueText(d *core.IssueDetail, comments []core.Comment) string {
	return issueTextHint(d, comments, false)
}

// issueTextHint is issueText; withHint adds a line saying the issue has
// files and that wp files saves them (for the CLI's wp show).
func issueTextHint(d *core.IssueDetail, comments []core.Comment, withHint bool) string {
	var b strings.Builder
	// WP-224: one line says what it is and who wrote it, each name once.
	b.WriteString(issueHead(d.Issue))
	issueAttrs(&b, d.Issue, d.ReporterAgent)
	b.WriteString(" " + provenance(d, true) + "\n")
	b.WriteString(writtenLine(d, false, time.Now()))
	b.WriteString(safetyLine(d))
	if d.Tested != nil {
		b.WriteString(d.Tested.Line + " (" + d.Tested.SHA[:min(7, len(d.Tested.SHA))] + ").\n")
		for _, l := range d.Tested.BenchLines {
			b.WriteString(l + ".\n")
		}
	}
	b.WriteString(core.ObjectiveText(d.Objective, false, time.Now()))
	b.WriteString(lineageLine(d))
	b.WriteString(riskLine(d, false))
	b.WriteString(goalLine(d))
	b.WriteString(crossLine(d))
	b.WriteString(holdLine(d.Issue))
	if len(d.Ancestors) > 0 {
		var path []string
		for _, a := range d.Ancestors {
			path = append(path, a.Key+" "+a.Title)
		}
		b.WriteString("Under: " + strings.Join(path, " › ") + "\n")
	}
	if d.TimeSpent > 0 {
		fmt.Fprintf(&b, "Time tracked: %dh%02dm\n", d.TimeSpent/3600, d.TimeSpent%3600/60)
	}
	if n := fileCount(d, comments); withHint && n > 0 {
		fmt.Fprintf(&b, "Attachments: %d — wp files %s saves them to view\n", n, d.Key)
	}
	// WP-1196: what it's for, in the words of the person who asked (an
	// agent's suggestion says so, since it isn't the person's intent).
	if d.Intent != "" {
		by := ""
		if d.IntentAgent != "" {
			by = " (suggested by " + d.IntentAgent + ", not yet kept by a person)"
		}
		b.WriteString("Intent" + by + ": " + d.Intent + "\n")
	}
	if d.Description != "" {
		b.WriteString("\n" + d.Description + "\n")
	}
	// WP-449: comments right after the description, ahead of the lists
	// that can run long, so a reader who stops early still meets them.
	if len(comments) > 0 {
		b.WriteString("\nComments:\n")
		for _, c := range comments {
			b.WriteString(core.CommentLine(c, ""))
		}
	}
	if len(d.Children) > 0 {
		b.WriteString("\nChildren:\n")
		for _, c := range d.Children {
			b.WriteString("  " + issueLine(c) + "\n")
		}
	}
	if len(d.Relations) > 0 {
		b.WriteString("\nRelations:\n")
		for _, r := range d.Relations {
			fmt.Fprintf(&b, "  %s %s %q (%s)\n", relationWords(r), r.Issue.Key, r.Issue.Title, r.State)
			if r.Basis == "inferred" && r.Note != "" {
				fmt.Fprintf(&b, "    inferred by Waypoint: a change %s\n", r.Note)
			}
		}
	}
	if len(d.Links) > 0 {
		b.WriteString("\nLinks:\n")
		for _, l := range d.Links {
			fmt.Fprintf(&b, "  %s %s %s %s\n", l.Kind, l.State, l.URL, l.Title)
		}
	}
	b.WriteString(forgeMentionLines(d.ForgeMentions))
	return b.String()
}

// forgeMentionLines lists the forge items that name the issue without being
// its work (WP-379): a pull request saying "follow-up to", a commit body
// listing it. Five at most; the API has them all.
func forgeMentionLines(ms []core.ForgeMention) string {
	if len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nMentioned on the forge (not linked):\n")
	for i, m := range ms {
		if i == 5 {
			fmt.Fprintf(&b, "  … %d more\n", len(ms)-i)
			break
		}
		how := strings.Join(m.Places, ", ")
		if m.Verb != "" {
			how = strings.ReplaceAll(m.Verb, "_", " ") + " in " + how
		}
		fmt.Fprintf(&b, "  %s %s %s (%s) %s\n", m.Kind, m.State, m.URL, how, m.Title)
	}
	return b.String()
}

// holdLine says what holds the issue back from agents (WP-269), or "".
func holdLine(i *core.Issue) string {
	h := i.Hold
	if h == nil {
		return ""
	}
	line := "On hold"
	switch {
	case h.Until != nil && h.Waits != "":
		line += " until " + h.Until.Format("2006-01-02 15:04 MST") + " and " + h.Waits
	case h.Until != nil:
		line += " until " + h.Until.Format("2006-01-02 15:04 MST")
	case h.Waits != "":
		line += " until " + h.Waits
	default:
		line += " until someone lifts it"
	}
	who := h.Agent
	if who == "" {
		who = h.By
	}
	if who != "" {
		line += ", by " + who
	}
	if h.Note != "" {
		line += ": " + h.Note
	}
	return line + fmt.Sprintf(". wp hold %s --off lifts it.\n", i.Key)
}

// fileCount is how many files an issue and its comments hold.
func fileCount(d *core.IssueDetail, comments []core.Comment) int {
	n := len(d.Files)
	for _, c := range comments {
		n += len(c.Files)
	}
	return n
}

// provenance says who created an issue, who wrote its text and who closed
// it (WP-D11), each once: "by opus 5.5, not yet reviewed by a person",
// "by Robin on 2026-10-27 22:45, text by opus 5.5 (reviewed by Robin)".
// dated adds when it was created, and updated if that was later.
func provenance(d *core.IssueDetail, dated bool) string {
	var parts []string
	by := "by " + byName(d.Reporter, d.ReporterAgent)
	if dated {
		const at = "2006-01-02 15:04"
		by += " on " + d.CreatedAt.Format(at)
		if u := d.UpdatedAt.Format(at); u != d.CreatedAt.Format(at) {
			by += ", updated " + u
		}
	}
	parts = append(parts, by)
	switch {
	case d.TextAgent != "" && d.TextAgent == d.ReporterAgent:
		if r := reviewState(d.TextAgent, d.Unreviewed, d.ReviewedBy); r != "" {
			parts = append(parts, r)
		}
	case d.TextAgent != "":
		parts = append(parts, "text by "+byName(nil, d.TextAgent)+" ("+reviewState(d.TextAgent, d.Unreviewed, d.ReviewedBy)+")")
	case d.TextBy != nil && (d.Reporter == nil || d.TextBy.ID != d.Reporter.ID || d.ReporterAgent != ""):
		parts = append(parts, "text edited by "+byName(d.TextBy, ""))
	}
	if d.ResolvedAt != nil && (d.ResolvedBy != nil || d.ResolvedAgent != "") {
		closed := d.Status.Name + " by " + byName(d.ResolvedBy, d.ResolvedAgent)
		// An agent's close, and whether a person has reviewed it (WP-272).
		switch {
		case d.CloseUnreviewed:
			closed += " (close not yet reviewed by a person)"
		case d.ResolvedAgent != "" && d.CloseReviewedBy != nil:
			closed += " (close reviewed by " + d.CloseReviewedBy.DisplayName + ")"
		}
		parts = append(parts, closed)
	}
	return strings.Join(parts, ", ")
}

// writtenLine says, on its own line, what was live when the issue was
// written and what is now (WP-460), so a reader can judge whether the task
// is stale: "Written when production ran v0.52.0 (02ad6d4); it runs v0.58.6
// (39468e2) now, 41 releases later." dated starts with how long ago it was
// written, for views whose head line has no date (wp next).
func writtenLine(d *core.IssueDetail, dated bool, now time.Time) string {
	line := "Written"
	if dated {
		line += " " + age(now.Sub(d.CreatedAt)) + " ago"
	}
	w := d.Written
	switch {
	case w == nil && !dated:
		return ""
	case w == nil:
	case w.Environment == "":
		line += writtenWhen(dated) + releaseLabel(w.Version, w.Commit) + " was live"
	default:
		line += writtenWhen(dated) + w.Environment + " ran " + releaseLabel(w.Version, w.Commit)
		live := releaseLabel(w.LiveVersion, w.LiveCommit)
		switch {
		case live == releaseLabel(w.Version, w.Commit):
			line += "; it still does"
		case live != "":
			line += "; it runs " + live + " now"
			switch {
			case w.Releases == 1:
				line += ", 1 release later"
			case w.Releases > 1:
				line += fmt.Sprintf(", %d releases later", w.Releases)
			}
		}
	}
	return line + ".\n"
}

// writtenWhen joins writtenLine's date to what was live.
func writtenWhen(dated bool) string {
	if dated {
		return ", when "
	}
	return " when "
}

// releaseLabel is "v0.52.0 (02ad6d4)", or whichever of the two there is.
func releaseLabel(version, commit string) string {
	c := commit
	if len(c) > 7 {
		c = c[:7]
	}
	switch {
	case version != "" && c != "":
		return version + " (" + c + ")"
	case version != "":
		return version
	}
	return c
}

// relatedText is the tasks around this one in a single list, linked ones
// first and then the ones its text names: enough to tell what each is
// without a wp show per key (WP-804). "" with none.
func relatedText(rels []core.Relation, named []*core.Issue, now time.Time) string {
	if len(rels)+len(named) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nRelated:\n")
	for _, r := range rels {
		fmt.Fprintf(&b, " %s %q [%s]%s\n", r.Issue.Key, clipTitle(r.Issue.Title), statusAge(r.Status, r.ResolvedAt, now), relationNote(r))
	}
	for _, i := range named {
		fmt.Fprintf(&b, " %s %q [%s]\n", i.Key, clipTitle(i.Title), statusAge(i.Status.Name, i.ResolvedAt, now))
	}
	b.WriteString("`wp show KEY1 KEY2...` for more information.\n")
	return b.String()
}

// statusAge is a status, with how long ago for a finished task: Done 3d ago.
func statusAge(status string, resolved *time.Time, now time.Time) string {
	if resolved == nil {
		return status
	}
	return status + " " + age(now.Sub(*resolved)) + " ago"
}

// relationWords is a relation as wp show lists it: "follows (fix)".
func relationWords(r core.Relation) string {
	w := strings.ReplaceAll(r.Kind, "_", " ")
	if r.Subkind != "" {
		w += " (" + r.Subkind + ")"
	}
	return w
}

// relationNote says how a linked task bears on this one, "" for a plain
// relates, which adds nothing to being listed.
func relationNote(r core.Relation) string {
	note := ""
	switch r.Kind {
	case "relates":
		return ""
	case "blocks":
		note = "waits on this"
	case "blocked_by":
		note = "blocks this"
	case "duplicates":
		note = "this duplicates it"
	case "duplicated_by":
		note = "duplicates this"
	case "follows":
		note = "this follows up on it"
	case "followed_by":
		note = "a follow-up of this"
	case "supersedes":
		note = "this replaces it: undoing its work is intended"
	case "superseded_by":
		note = "replaces this"
	case "tension":
		note = "in tension with this: weigh both"
	default:
		note = strings.ReplaceAll(r.Kind, "_", " ")
	}
	if r.Subkind != "" {
		note += ", " + r.Subkind
	}
	return " (" + note + ")"
}

const (
	titleLong = 120 // longer titles stop at their first sentence
	titleMax  = 160 // and no title runs past this, in bytes
	// titleSentenceMin is the shortest first sentence that stands for a title:
	// a stop before it is a list number or "Dr.", or says too little to keep.
	titleSentenceMin = 40
)

// clipTitle shortens a title that holds a whole request to its first
// sentence, and one with no early sentence end to whole words.
func clipTitle(title string) string {
	if len(title) <= titleLong {
		return title
	}
	s := title
	for i := titleSentenceMin; i+2 < len(s); i++ {
		if sentenceEnds(s, i) {
			s = strings.TrimRight(s[:i+1], ".")
			break
		}
	}
	if len(s) > titleMax {
		s = core.ClipText(s, titleMax, "")
		// Only a space in the second half: backing up to an earlier one would
		// lose most of a long URL or path that fit.
		if i := strings.LastIndexByte(s, ' '); i > len(s)/2 {
			s = strings.TrimRight(s[:i], " ,;:")
		}
	}
	if s == title {
		return title
	}
	return s + "..."
}

// sentenceEnds says whether the punctuation at s[i] closes a sentence: a space
// follows, and the next word doesn't carry on in lower case. A sentence end it
// misses only runs on to the next one, or to titleMax.
func sentenceEnds(s string, i int) bool {
	if strings.IndexByte(".?!", s[i]) < 0 || s[i+1] != ' ' {
		return false
	}
	if next, _ := utf8.DecodeRuneInString(s[i+2:]); unicode.IsLower(next) {
		return false
	}
	return !endsAbbreviation(s[:i+1])
}

// endsAbbreviation says whether a full stop closes an abbreviation or an
// ellipsis rather than a sentence: a lone letter (U.S., e.g.), dots in a row,
// or vs.
func endsAbbreviation(s string) bool {
	n := len(s)
	if s[n-1] != '.' {
		return false
	}
	if n < 3 || s[n-3] == '.' || s[n-3] == ' ' {
		return true
	}
	return strings.HasSuffix(strings.ToLower(s), " vs.")
}

// age is a rough duration: 40m, 5h, 3d.
func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d.Minutes()), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// safetyLine says, on its own line, why agents wait on an agent-written task
// the safety screen hasn't cleared (WP-ADR10); "" once it has.
func safetyLine(d *core.IssueDetail) string {
	if d.Safety == nil || d.Safety.Verdict == core.SafetyClear {
		return ""
	}
	detail := "an agent wrote it, no person has reviewed it, and the screen hasn't read it yet"
	if d.Safety.Verdict == core.SafetyHold {
		detail = "held for a person: " + strings.TrimSuffix(d.Safety.Reason, ".") + ". A person decides"
	}
	return "Safety screen: " + detail + "\n"
}

// lineageLine names, on its own line, the tasks an agent wrote the issue's
// text while working on, nearest first, and the depth that makes (WP-275);
// "" when a person wrote or reviewed it, or no task was behind it.
func lineageLine(d *core.IssueDetail) string {
	l := d.Lineage
	if l == nil || len(l.Chain) == 0 {
		return ""
	}
	parts := make([]string, 0, len(l.Chain))
	for _, c := range l.Chain {
		switch {
		case c.Gone:
			parts = append(parts, fmt.Sprintf("a deleted task (depth %d)", c.Depth))
			continue
		case c.Hidden:
			parts = append(parts, fmt.Sprintf("a task you can't open (depth %d)", c.Depth))
			continue
		default:
			parts = append(parts, c.Key)
		}
		switch {
		case c.Agent != "":
			parts[len(parts)-1] += " (" + agentShort(c.Agent) + "'s, unreviewed)"
		case c.Reviewed:
			parts[len(parts)-1] += " (an agent's, reviewed)"
		default:
			parts[len(parts)-1] += " (a person's)"
		}
	}
	return fmt.Sprintf("Written while working on %s: depth %d\n", strings.Join(parts, " ← "), l.Depth)
}

// riskLine gives, on its own line, the issue's provenance risk and the
// factors behind it (WP-554), "" when it's low; wp risk KEY says why in
// full. highOnly leaves out medium, which most agent-filed tasks are until
// a person reviews them: the agent's own task view (WP-242).
func riskLine(d *core.IssueDetail, highOnly bool) string {
	r := d.Risk
	if r == nil || r.Level == core.RiskLow || (highOnly && r.Level != core.RiskHigh) {
		return ""
	}
	var fs []string
	for _, x := range r.Reasons {
		f := strings.ReplaceAll(x.Factor, "_", " ")
		if x.Ref != "" {
			f += " " + x.Ref
		}
		if !slices.Contains(fs, f) {
			fs = append(fs, f)
		}
	}
	return "Risk: " + r.Level + " (" + strings.Join(fs, ", ") + ")\n"
}

// goalLine says, on its own line, which goal the issue traces to (WP-382),
// "" when it isn't linked to one (WP-770: "none" only distracts).
func goalLine(d *core.IssueDetail) string {
	g := d.Goal
	if g == nil || g.Goal == nil {
		return ""
	}
	line := fmt.Sprintf("Goal: %s (%s; wp trace %s shows how)\n", itemName(*g.Goal), groundingShort[g.Grounding], d.Key)
	if len(g.Chain) == 0 {
		if g.Above > 0 {
			line += fmt.Sprintf("  (%s left out: wp goals shows them)\n", countWord(g.Above, "parent goal"))
		}
		return line
	}
	// The parent goals the task's arm shows (WP-1035), nearest first.
	names := make([]string, len(g.Chain))
	for n, p := range g.Chain {
		names[n] = itemName(core.KItem{Type: core.KDesign, Kind: "goal", ID: p.ID, Title: p.Title})
	}
	line += "  under " + strings.Join(names, " › ")
	if g.Above > 0 {
		line += fmt.Sprintf(" (%d more left out: wp goals shows them)", g.Above)
	}
	return line + "\n"
}

// crossLine warns the agents of this repo, on its own line, that an agent
// working in another repo filed the task and knows little of this one, and
// says how to turn it down without filing it back (WP-378). "" when it was
// filed here, a person has reviewed it, or it's closed. A decline shows
// however it was closed.
func crossLine(d *core.IssueDetail) string {
	f := d.FiledFrom
	if f == nil || f.Here {
		return ""
	}
	from := f.Repo
	switch {
	case f.Project != "" && f.Repo != "":
		from = f.Project + " (" + f.Repo + ")"
	case f.Project != "":
		from = f.Project
	case from == "":
		from = "another repo"
	}
	if f.DeclinedAt != nil {
		return "Declined: filed from " + from + "; " + byName(nil, f.DeclinedBy) + " said: " + strings.Join(strings.Fields(f.Declined), " ") + "\n"
	}
	if d.Closed() || !d.Unreviewed {
		return ""
	}
	return "Filed from " + from + " by an agent working there, which knows little of this repo. If it doesn't belong here, `wp decline " + d.Key + " -m \"why, and where it belongs\"` closes it and tells the filer.\n"
}

// byName is who did something, as people say it: a person's name, or the
// agent's model or tool (agentShort).
func byName(u *core.UserRef, agent string) string {
	switch {
	case agent != "":
		return agentShort(agent)
	case u != nil:
		return u.DisplayName
	}
	return "someone"
}

// agentShort is an agent identity as people say it (WP-242):
// anthropic/claude-opus-5-5 → opus 5.5, anthropic/claude-code → claude code,
// openai/gpt-5.1 → gpt-5.1.
func agentShort(identity string) string {
	// Text from a forge login agents may write as (WP-282).
	if identity == "forge" {
		return "a forge login"
	}
	if login, ok := strings.CutPrefix(identity, "forge/"); ok {
		return "forge login " + login
	}
	m := identity
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if rest, ok := strings.CutPrefix(m, "claude-"); ok && rest != "code" {
		m = rest
	}
	parts := strings.Split(m, "-")
	numeric := len(parts) > 1
	var ver []string
	for _, p := range parts[1:] {
		switch {
		case p == "" || strings.Trim(p, "0123456789") != "":
			numeric = false
		case len(p) < 6: // not a date stamp: claude-opus-4-1-20250805
			ver = append(ver, p)
		}
	}
	if numeric && len(ver) > 0 {
		return parts[0] + " " + strings.Join(ver, ".")
	}
	if len(parts) == 2 && strings.Trim(parts[1], "0123456789.") != "" {
		return parts[0] + " " + parts[1] // claude-code, gemini-cli
	}
	return m
}

// pageStoryText is a page's user story and the prompt that asked for the
// page (WP-190): whose intent the story is, or that it is no one's yet.
func pageStoryText(p *core.Page) string {
	var b strings.Builder
	if p.Story != "" {
		switch {
		case p.StoryIsPersons():
			by := "a person"
			if p.StoryBy != nil {
				by = p.StoryBy.DisplayName
			}
			kept := "written"
			if p.StoryModel != "" {
				kept = "kept"
			}
			when := ""
			if p.StoryAt != nil {
				when = " " + day(p.StoryAt)
			}
			fmt.Fprintf(&b, "\nStory (%s's intent, %s%s):\n", by, kept, when)
		default:
			fmt.Fprintf(&b, "\nStory (suggested by %s, model %s; no person has kept it, so it is no one's intent yet):\n", p.StoryAgent, cmpStr(p.StoryModel, "unknown"))
		}
		for _, l := range strings.Split(p.Story, "\n") {
			b.WriteString("  " + l + "\n")
		}
		if p.StoryStale {
			b.WriteString("  (The plan changed after this story was written or kept.)\n")
		}
	}
	if p.Prompt != nil {
		fmt.Fprintf(&b, "\nAsked for in session #%d on %s: %q\n", p.Prompt.SessionID, day(p.Prompt.At), clip(p.Prompt.Text, 600))
	} else if p.SessionID != nil {
		fmt.Fprintf(&b, "\nWritten in session #%d\n", *p.SessionID)
	}
	return b.String()
}

// pageText renders a page for agents.
func pageText(p *core.Page) string {
	var b strings.Builder
	b.WriteString(pageLine(p))
	b.WriteByte('\n')
	if len(p.OtherCodes) > 0 {
		b.WriteString("Also known as: " + strings.Join(p.OtherCodes, ", ") + "\n")
	}
	if p.Owner != nil || p.CreatedAgent != "" {
		b.WriteString("Created by: " + who(p.Owner, p.CreatedAgent) + "\n")
	}
	if p.TextAgent == "" && p.TextBy != nil && (p.Owner == nil || p.TextBy.ID != p.Owner.ID || p.CreatedAgent != "") {
		b.WriteString("Text last edited by: " + who(p.TextBy, "") + "\n")
	}
	if len(p.Ancestors) > 0 {
		var path []string
		for _, a := range p.Ancestors {
			path = append(path, pageRefText(a))
		}
		b.WriteString("Under: " + strings.Join(path, " › ") + "\n")
	}
	b.WriteString(pageStoryText(p))
	if p.Body != "" {
		if p.Story != "" {
			b.WriteString("\nPlan (" + who(p.TextBy, p.TextAgent) + "'s text; approving the page keeps only the story above as a person's intent):")
		}
		b.WriteString("\n" + p.Body + "\n")
	}
	if len(p.Children) > 0 {
		b.WriteString("\nSub-pages:\n")
		for _, c := range p.Children {
			b.WriteString("  " + pageRefText(c) + "\n")
		}
	}
	if len(p.Issues) > 0 {
		b.WriteString("\nLinked issues:")
		for _, i := range p.Issues {
			b.WriteString(" " + i.Key)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

//go:embed api_help.md
var apiHelpText string

func (s *Server) queryHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(wql.Help))
}

func (s *Server) apiHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(apiHelpText))
}
