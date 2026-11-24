package api

import (
	"fmt"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

func scopeName(sc core.KScope) string {
	if sc.ProjectKey == "" {
		return "global"
	}
	return sc.ProjectKey
}

// who names the author: an agent ("agent anthropic/claude-opus-5-5"), not
// the person whose token it used (they didn't write it), or a person.
func who(u *core.UserRef, agent string) string {
	switch {
	case agent != "":
		return "agent " + agent
	case u != nil:
		return u.DisplayName
	}
	return "someone"
}

// reviewState says whether a person has reviewed what an agent wrote
// (WP-D11); "" when a person wrote it.
func reviewState(agent string, unreviewed bool, reviewer *core.UserRef) string {
	switch {
	case agent == "":
		return ""
	case unreviewed || reviewer == nil:
		return "not yet reviewed by a person"
	}
	return "reviewed by " + reviewer.DisplayName
}

// writtenBy is who wrote something and, for an agent, its review state:
// "Robin", "agent anthropic/claude-opus-5-5, not yet reviewed by a person".
func writtenBy(u *core.UserRef, agent string, unreviewed bool, reviewer *core.UserRef) string {
	w := who(u, agent)
	if r := reviewState(agent, unreviewed, reviewer); r != "" {
		w += ", " + r
	}
	return w
}

func day(t interface{ Format(string) string }) string { return t.Format("2006-01-02") }

func linkLine(l *core.Link) string {
	it := l.Item
	var name string
	switch it.Type {
	case core.KIssue:
		name = it.Key + " " + it.Title
		if it.State != "" {
			name += " (" + it.State + ")"
		}
	case core.KPage:
		name = fmt.Sprintf("page #%d %s", it.ID, it.Title)
		if it.Key != "" {
			name = fmt.Sprintf("page %s (#%d) %s", it.Key, it.ID, it.Title)
		}
	case core.KGlossary:
		name = fmt.Sprintf("term %q (#%d)", it.Title, it.ID)
	case core.KDesign:
		name = fmt.Sprintf("design %s#%d %s [%s]", codePrefix(it.Key, it.ProjectKey), it.ID, it.Title, it.Kind)
		if it.State == "proposed" {
			name += " (unapproved)"
		}
	case core.KFile:
		name = "file " + it.Ref
	case core.KURL:
		name = it.Title
		if it.Title != it.Ref {
			name += " " + it.Ref
		}
	}
	rel := strings.ReplaceAll(l.Rel, "_", " ")
	if l.Rel == "tension" {
		rel = "in tension with"
	} else if l.Direction == "in" {
		rel = map[string]string{"relates": "related to", "implements": "implemented by", "depends on": "needed by",
			"supersedes": "superseded by", "mentions": "mentioned in", "serves": "served by"}[rel]
	}
	return rel + ": " + name
}

func glossaryEntryText(g *core.GlossaryEntry, full bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  (%s glossary, term #%d)\n", g.Term, scopeName(g.KScope), g.ID)
	if d := strings.TrimSpace(g.Definition); d != "" {
		b.WriteString(d + "\n")
	}
	var meta []string
	if full && len(g.Aliases) > 0 {
		meta = append(meta, "aliases: "+strings.Join(g.Aliases, ", "))
	}
	if full && len(g.Avoid) > 0 {
		meta = append(meta, "avoid: "+strings.Join(g.Avoid, ", "))
	}
	if g.Locked {
		by := "a person"
		if g.LockedBy != nil {
			by = g.LockedBy.DisplayName
		}
		meta = append(meta, "locked by "+by+" (agents can't change it)")
	}
	if full {
		meta = append(meta, "written by "+who(g.CreatedBy, g.CreatedAgent)+" "+day(g.CreatedAt))
		if !g.UpdatedAt.Equal(g.CreatedAt) {
			meta = append(meta, "last changed by "+who(g.UpdatedBy, g.UpdatedAgent)+" "+day(g.UpdatedAt))
		}
	}
	if r := reviewState(g.UpdatedAgent, g.Unreviewed, g.ReviewedBy); r != "" {
		if !full {
			r = "by " + who(g.UpdatedBy, g.UpdatedAgent) + ", " + r
		}
		meta = append(meta, r)
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	for _, l := range g.Links {
		b.WriteString("  " + linkLine(l) + "\n")
	}
	return b.String()
}

func glossaryLookupText(res *core.GlossaryLookup, project string) string {
	if len(res.Matches) == 0 && len(res.Instead) > 0 {
		g := res.Instead[0]
		return fmt.Sprintf("%q isn't the word used here: say %q.\n", res.Term, g.Term) + glossaryEntryText(g, false)
	}
	if len(res.Matches) == 0 {
		where := "any glossary you can see"
		if project != "" {
			where = project + "'s glossary, its parents' or the global one"
		}
		s := fmt.Sprintf("No entry for %q in %s.\n", res.Term, where)
		if len(res.Suggestions) > 0 {
			var names []string
			for _, g := range res.Suggestions {
				names = append(names, g.Term)
			}
			s += "Close: " + strings.Join(names, ", ") + "\n"
		}
		return s
	}
	var b strings.Builder
	if g := res.Matches[0]; !strings.EqualFold(strings.Join(strings.Fields(res.Term), " "), g.Term) {
		fmt.Fprintf(&b, "%q is another name for %q, the term used here.\n", res.Term, g.Term)
	}
	b.WriteString(glossaryEntryText(res.Matches[0], false))
	for _, g := range res.Matches[1:] {
		fmt.Fprintf(&b, "Also in the %s glossary (overridden here): %s\n", scopeName(g.KScope), firstLine(g.Definition))
	}
	for _, g := range res.Instead {
		fmt.Fprintf(&b, "Elsewhere %q is a word to avoid: say %q.\n", res.Term, g.Term)
	}
	return b.String()
}

// codePrefix is a note's decision code (APP-ADR7: it resolves from any
// project) and a space, or nothing.
func codePrefix(code, project string) string {
	if code == "" {
		return ""
	}
	if project != "" {
		code = project + "-" + code
	}
	return code + " "
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		cut := 160
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func glossaryListText(list []*core.GlossaryEntry) string {
	if len(list) == 0 {
		return "No glossary entries.\n"
	}
	var b strings.Builder
	for _, g := range list {
		fmt.Fprintf(&b, "#%d %s (%s)", g.ID, g.Term, scopeName(g.KScope))
		if g.Locked {
			b.WriteString(" [locked]")
		}
		if g.Shadowed {
			b.WriteString(" [overridden]")
		}
		if g.Unreviewed {
			b.WriteString(" [agent-written, unreviewed]")
		}
		if d := firstLine(snippetText(cmpStr(g.Snippet, g.Definition))); d != "" {
			b.WriteString(" — " + d)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// approvalLine is a note's provenance in one place: who wrote it, whether
// a person approved it (who, when) or it's only an agent's proposal, and
// who last changed it. Agents read this to tell what people intend from
// what another agent suggested (WP-D11).
func approvalLine(n *core.DesignNote) string {
	written := "Written by " + who(n.CreatedBy, n.CreatedAgent) + " on " + day(n.CreatedAt) + "."
	if !n.Approved {
		return "PROPOSED, NOT APPROVED by a person yet. " + written
	}
	by := "a person"
	if n.ApprovedBy != nil {
		by = n.ApprovedBy.DisplayName
	}
	s := written + " Approved by " + by + " on " + day(*n.ApprovedAt) + "."
	if n.Requested != nil {
		// A request is provenance, not approval; never word it as approved.
		s = "Written by " + who(n.CreatedBy, n.CreatedAgent) + " on " + day(n.CreatedAt) + " for " + n.Requested.Issue + ", which " +
			who(n.Requested.By, "") + " filed: it counts on that request; no person approved it."
	}
	if n.UpdatedAt.After(*n.ApprovedAt) {
		s += " Last changed " + day(n.UpdatedAt) + " by " + who(n.UpdatedBy, n.UpdatedAgent) + "."
	}
	return s
}

func designHeader(n *core.DesignNote) string {
	meta := []string{n.Kind, n.Status, scopeName(n.KScope)}
	ref := fmt.Sprintf("design #%d", n.ID)
	ref = codePrefix(n.Code, n.ProjectKey) + ref
	return fmt.Sprintf("%s  [%s]  %s", n.Title, strings.Join(meta, " · "), ref)
}

// revisionWho is who wrote a revision and where it stands with a person:
// "agent X, approved by Robin 2026-10-20", "agent X, NOT APPROVED".
func revisionWho(r core.DesignRevision) string {
	head := day(r.CreatedAt) + " " + who(r.Author, r.Agent)
	reviewer := "a person"
	if r.ReviewedBy != nil {
		reviewer = r.ReviewedBy.DisplayName
	}
	when := ""
	if r.ReviewedAt != nil {
		when = " " + day(r.ReviewedAt)
	}
	switch r.State {
	case core.RevApproved:
		if r.Agent != "" {
			head += ", approved by " + reviewer + when
		} else if r.ReviewedBy != nil && r.Author != nil && r.ReviewedBy.ID != r.Author.ID {
			head += ", approved by " + reviewer + when
		}
	case core.RevMerged:
		head += ", merged into the text by " + reviewer + when
	case core.RevRejected:
		head += ", rejected by " + reviewer + when
	case core.RevPending:
		head += ", NOT APPROVED"
	case core.RevApplied:
		if r.ReviewNote != "" {
			head += ", applied: " + r.ReviewNote
		} else {
			head += ", applied while the note was unapproved"
		}
	}
	return head
}

func indent(text, pad string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n"+pad)
}

// additionsText lists the approved additions, then (apart, and marked)
// the agents' pending additions and proposed edits, which are not part of
// the text above them.
func additionsText(n *core.DesignNote, pad string) string {
	var approved, applied, pending []string
	for _, r := range n.Additions {
		line := pad + "- [r" + strconv.FormatInt(r.ID, 10) + " " + revisionWho(r) + "] " + indent(r.Body, pad+"  ")
		switch r.State {
		case core.RevApproved:
			approved = append(approved, line)
		case core.RevApplied:
			applied = append(applied, line)
		case core.RevPending:
			pending = append(pending, line)
		}
	}
	var b strings.Builder
	if len(approved) > 0 {
		b.WriteString("\n" + pad + "Additions:\n" + strings.Join(approved, "\n") + "\n")
	}
	if len(applied) > 0 {
		b.WriteString("\n" + pad + "Agent additions that count without a person's approval (nobody approved them):\n" + strings.Join(applied, "\n") + "\n")
	}
	if len(pending) > 0 {
		b.WriteString("\n" + pad + "UNAPPROVED agent additions (not accepted by a person):\n" + strings.Join(pending, "\n") + "\n")
	}
	if len(n.Proposals) > 0 {
		b.WriteString("\n" + pad + "UNAPPROVED agent edits (proposals, not applied: the text above is what a person approved):\n")
		for _, p := range n.Proposals {
			stale := ""
			if p.Stale {
				stale = ", written against an older version"
			}
			fmt.Fprintf(&b, "%s- [r%d %s%s]", pad, p.ID, revisionWho(p), stale)
			if p.Title != "" && p.Title != n.Title {
				fmt.Fprintf(&b, " new title: %s;", p.Title)
			}
			b.WriteString(" proposed text:\n" + pad + "  " + indent(p.Body, pad+"  ") + "\n")
		}
	}
	return b.String()
}

// historyText lists every revision, newest first, with who wrote it and
// who approved it.
func historyText(n *core.DesignNote) string {
	if len(n.History) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nHistory (newest first):\n")
	for i, r := range n.History {
		if i == 20 {
			fmt.Fprintf(&b, "  … %d older\n", len(n.History)-i)
			break
		}
		fmt.Fprintf(&b, "  r%d %s [%s] %s\n", r.ID, r.Kind, r.State, revisionWho(r))
	}
	return b.String()
}

func designNoteText(n *core.DesignNote) string {
	var b strings.Builder
	b.WriteString(designHeader(n) + "\n")
	if len(n.Ancestors) > 0 {
		var path []string
		for _, a := range n.Ancestors {
			path = append(path, a.Title)
		}
		b.WriteString("Part of: " + strings.Join(path, " › ") + "\n")
	}
	b.WriteString(approvalLine(n) + "\n")
	if n.Body != "" {
		b.WriteString("\n" + n.Body + "\n")
	}
	b.WriteString(additionsText(n, ""))
	b.WriteString(historyText(n))
	if len(n.Children) > 0 {
		b.WriteString("\nSub-notes:\n")
		for _, c := range n.Children {
			fmt.Fprintf(&b, "  %s#%d %s [%s]%s\n", codePrefix(c.Code, c.ProjectKey), c.ID, c.Title, c.Kind, map[bool]string{true: "", false: " (proposed, not approved)"}[c.Approved])
		}
	}
	if len(n.Links) > 0 {
		b.WriteString("\nLinks:\n")
		for _, l := range n.Links {
			b.WriteString("  " + linkLine(l) + "\n")
		}
	}
	if n.Notice != "" {
		b.WriteString("\n" + n.Notice + "\n")
	}
	return b.String()
}

func designListText(list []*core.DesignNote) string {
	if len(list) == 0 {
		return "No design notes.\n"
	}
	var b strings.Builder
	kind := ""
	for _, n := range list {
		if n.Snippet == "" && n.Kind != kind {
			kind = n.Kind
			b.WriteString(strings.ToUpper(kind[:1]) + kind[1:] + "s:\n")
		}
		fmt.Fprintf(&b, "  %s#%d %s", codePrefix(n.Code, n.ProjectKey), n.ID, n.Title)
		var tags []string
		if n.Snippet != "" {
			tags = append(tags, n.Kind)
		}
		if n.Status != "active" {
			tags = append(tags, n.Status)
		}
		if !n.Approved {
			tags = append(tags, "proposed")
		}
		if n.Pending > 0 {
			tags = append(tags, fmt.Sprintf("%d pending", n.Pending))
		}
		if n.ProjectKey == "" || n.Inherited {
			tags = append(tags, scopeName(n.KScope))
		}
		if len(tags) > 0 {
			b.WriteString(" [" + strings.Join(tags, ", ") + "]")
		}
		if n.Snippet != "" {
			b.WriteString(" — " + firstLine(snippetText(n.Snippet)))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func specText(sp *core.Spec) string {
	n := sp.Note
	var b strings.Builder
	b.WriteString("SPEC: " + designHeader(n) + "\n")
	b.WriteString(approvalLine(n) + "\n")
	if n.Status != "active" {
		fmt.Fprintf(&b, "This note is %s: it no longer describes what's intended.\n", n.Status)
	}
	if len(n.Ancestors) > 0 {
		var path []string
		for _, a := range n.Ancestors {
			path = append(path, a.Title)
		}
		b.WriteString("Part of: " + strings.Join(path, " › ") + "\n")
	}
	if n.Body != "" {
		b.WriteString("\n" + n.Body + "\n")
	}
	b.WriteString(additionsText(n, ""))
	if len(sp.Parts) > 0 {
		b.WriteString("\nParts:\n")
		for _, p := range sp.Parts {
			b.WriteString("\n## " + designHeader(p) + "\n" + approvalLine(p) + "\n")
			if p.Body != "" {
				b.WriteString(p.Body + "\n")
			}
			b.WriteString(additionsText(p, ""))
		}
	}
	if len(sp.Related) > 0 {
		b.WriteString("\nRelated:\n")
		for _, r := range sp.Related {
			l := &core.Link{Rel: r.Rel, Direction: r.Direction, Item: core.KItem{Type: core.KDesign, ID: r.Note.ID, Key: r.Note.Code, Title: r.Note.Title, Kind: r.Note.Kind,
				State: map[bool]string{true: r.Note.Status, false: "proposed"}[r.Note.Approved]}}
			b.WriteString("  " + linkLine(l))
			if r.Summary != "" {
				b.WriteString(" — " + r.Summary)
			}
			b.WriteByte('\n')
		}
	}
	if len(sp.Terms) > 0 {
		b.WriteString("\nTerms:\n")
		for _, t := range sp.Terms {
			fmt.Fprintf(&b, "  %s: %s\n", t.Term, firstLine(t.Definition))
		}
	}
	if len(sp.Issues) > 0 {
		b.WriteString("\nTasks:\n")
		for _, i := range sp.Issues {
			fmt.Fprintf(&b, "  %s %s (%s)\n", i.Key, i.Title, i.State)
		}
	}
	if len(sp.Changes) > 0 {
		b.WriteString("\nSome PRs and commits that touched it (not all):\n")
		for _, c := range sp.Changes {
			line := "  " + c.Title
			if c.State != "" {
				line += " [" + c.State + "]"
			}
			if c.Key != "" {
				line += " via " + c.Key
			}
			b.WriteString(line + " " + c.Ref + "\n")
		}
	}
	if len(sp.Files) > 0 {
		b.WriteString("\nFiles: " + strings.Join(sp.Files, ", ") + "\n")
	}
	if len(sp.Others) > 0 {
		var o []string
		for _, r := range sp.Others {
			o = append(o, fmt.Sprintf("%s#%d %s [%s]", codePrefix(r.Code, r.ProjectKey), r.ID, r.Title, r.Kind))
		}
		b.WriteString("\nOther matches: " + strings.Join(o, "; ") + "\n")
	}
	return b.String()
}

func specOverviewText(ov *core.SpecOverview) string {
	var b strings.Builder
	where := "global"
	if ov.Project != "" {
		where = ov.Project + " (with its parents' and global notes)"
	}
	b.WriteString("Design intent for " + where + "\n")
	section := func(title string, notes []*core.DesignNote) {
		if len(notes) == 0 {
			return
		}
		b.WriteString("\n" + title + ":\n")
		for _, n := range notes {
			b.WriteString("\n## " + designHeader(n) + "\n" + approvalLine(n) + "\n")
			if n.Body != "" {
				b.WriteString(n.Body + "\n")
			}
			b.WriteString(additionsText(n, ""))
		}
	}
	section("Goals", ov.Goals)
	section("Principles", ov.Principles)
	any := false
	for _, k := range core.DesignKinds {
		refs := ov.Index[k]
		if len(refs) == 0 {
			continue
		}
		if !any {
			b.WriteString("\nOther notes (wp spec <title or #id> for one):\n")
			any = true
		}
		var names []string
		for _, r := range refs {
			s := fmt.Sprintf("%s#%d %s", codePrefix(r.Code, r.ProjectKey), r.ID, r.Title)
			if !r.Approved {
				s += " (proposed)"
			}
			names = append(names, s)
		}
		b.WriteString("  " + k + "s: " + strings.Join(names, "; ") + "\n")
	}
	if len(ov.Goals)+len(ov.Principles) == 0 && !any {
		b.WriteString("\nNo design notes yet.\n")
	}
	if ov.Pending > 0 {
		fmt.Fprintf(&b, "\n%d agent change(s) wait for a person's review.\n", ov.Pending)
	}
	return b.String()
}

// snippetText turns a snippet's ⟦marks⟧ into [brackets] for terminals,
// dropping wikilink brackets first so they don't read as marks.
func snippetText(s string) string {
	s = strings.NewReplacer("[[", "", "]]", "").Replace(s)
	return strings.NewReplacer(core.MarkStart, "[", core.MarkEnd, "]").Replace(s)
}

var kindTitles = map[string]string{core.KIssue: "Issues", core.KRequest: "Requests", core.KPage: "Pages", "comment": "Comments", core.KGlossary: "Glossary", core.KDesign: "Design notes"}

// hitLine is one search hit on a line.
func hitLine(h core.SearchHit) string {
	var line string
	switch h.Kind {
	case core.KIssue:
		line = fmt.Sprintf("%s %s [%s]", h.Key, h.Title, h.Sub)
	case core.KRequest:
		line = fmt.Sprintf("request %s %s [%s, %s]", h.Key, h.Title, h.Sub, h.State)
	case core.KPage:
		line = fmt.Sprintf("page #%d %s", h.ID, h.Title)
		if h.Key != "" {
			line = fmt.Sprintf("page %s (#%d) %s", h.Key, h.ID, h.Title)
		}
	case "comment":
		line = fmt.Sprintf("%s %s (comment #%d", h.Key, h.Title, h.ID)
		if h.Sub != "" {
			line += " by " + h.Sub
		}
		line += ")"
	case core.KGlossary:
		line = fmt.Sprintf("term #%d %s", h.ID, h.Title)
		if h.State == "locked" {
			line += " [locked]"
		}
	case core.KDesign:
		line = fmt.Sprintf("design #%d %s [%s, %s]", h.ID, h.Title, h.Sub, h.State)
		if h.Key != "" {
			line = fmt.Sprintf("design %s (#%d) %s [%s, %s]", h.Key, h.ID, h.Title, h.Sub, h.State)
		}
	}
	if h.ProjectKey != "" && h.Kind != core.KIssue && h.Kind != core.KRequest && h.Kind != "comment" {
		line += " " + h.ProjectKey
	}
	switch h.Match {
	case core.MatchExact:
		line += "  <- exact"
	case core.MatchMention:
		line += "  <- mentions it"
	}
	return line
}

func searchAllText(res *core.AllResult) string {
	var b strings.Builder
	found := 0
	if len(res.Exact) > 0 {
		b.WriteString("Best match:\n")
		for _, h := range res.Exact {
			b.WriteString("  " + hitLine(h) + "\n")
		}
	}
	for _, g := range res.Groups {
		if len(g.Hits) == 0 {
			continue
		}
		found += len(g.Hits)
		fmt.Fprintf(&b, "%s (%d", kindTitles[g.Kind], len(g.Hits))
		if g.Total > len(g.Hits) {
			fmt.Fprintf(&b, " of %d", g.Total)
		}
		b.WriteString("):\n")
		for _, h := range g.Hits {
			b.WriteString("  " + hitLine(h) + "\n")
			if h.Snippet != "" {
				b.WriteString("    " + snippetText(h.Snippet) + "\n")
			}
		}
	}
	if found == 0 {
		b.WriteString("Nothing found.\n")
	}
	if !res.Archived {
		b.WriteString("(Closed issues and retired notes left out; --archived includes them.)\n")
	}
	return b.String()
}
