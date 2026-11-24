package core

// It is a prompt for the agent, not a verdict: it never says whether the session can be cleared.

import (
	"context"
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// PutOffNote is one quoted sentence and where it came from: "description",
// "comment #12" or a page's code.
type PutOffNote struct {
	From string `json:"from"`
	Text string `json:"text"`
}

// PutOffSteps is what the skim found: the first notes, how many more there
// were, and the task's subtasks that are still open (filed already, maybe
// for one of the notes).
type PutOffSteps struct {
	Notes []PutOffNote `json:"notes"`
	More  int          `json:"more,omitempty"`
	Open  []IssueRef   `json:"open,omitempty"`
}

const (
	maxPutOffNotes = 8
	putOffQuote    = 180
)

// putOffWords are the phrases that mark a step as conditional or put off.
var putOffWords = lazyre.New(`(?i)\b(only if|only when|if needed|if necessary|when needed|as needed|if it turns out|later|afterwards|follow[- ]?ups?|(?:step|phase|stage|part) [2-9]|(?:second|next) (?:step|phase|stage|pass)|defer(?:s|red)?|put off|postponed?|not yet|for now|out of scope|separate (?:task|issue|pr|pull request|change))\b`)

// PutOffSteps skims an issue the actor can see. It returns nil when nothing
// in it reads as put off and no subtask is open.
func (s *Service) PutOffSteps(ctx context.Context, actor *Actor, ref string) (*PutOffSteps, error) {
	d, err := s.GetIssue(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	var notes []PutOffNote
	add := func(from, text string) {
		for _, q := range putOffSentences(text) {
			notes = append(notes, PutOffNote{From: from, Text: q})
		}
	}
	add("description", d.Description)
	comments, err := s.ListComments(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		add(fmt.Sprintf("comment #%d", c.ID), c.Body)
	}
	for _, p := range d.Pages {
		if p.Category != "plan" {
			continue
		}
		pg, err := s.GetPage(ctx, actor, p.ID)
		if err != nil {
			continue
		}
		add(cmp(pg.Code, pg.Title), pg.Body)
	}
	notes = dedupeNotes(notes)
	out := &PutOffSteps{Notes: notes}
	if len(notes) > maxPutOffNotes {
		out.Notes, out.More = notes[:maxPutOffNotes], len(notes)-maxPutOffNotes
	}
	for _, c := range d.Children {
		if c.Status.Category != CatDone {
			out.Open = append(out.Open, c.Ref())
		}
	}
	if len(out.Notes) == 0 && len(out.Open) == 0 {
		return nil, nil
	}
	return out, nil
}

// putOffSentences returns the sentences of a markdown text that use a
// put-off phrase, shortened to a quote. Code blocks are skipped.
func putOffSentences(text string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || t == "" {
			continue
		}
		for _, sen := range splitSentences(trimListMark(t)) {
			sen = strings.Join(strings.Fields(sen), " ")
			if loc := putOffWords.FindStringIndex(sen); loc != nil {
				out = append(out, quoteAround(sen, loc[0]))
			}
		}
	}
	return out
}

// trimListMark drops a markdown heading, list or quote marker.
func trimListMark(t string) string {
	t = strings.TrimLeft(t, "#> ")
	for _, m := range []string{"- [ ] ", "- [x] ", "- ", "* ", "+ "} {
		if strings.HasPrefix(t, m) {
			return strings.TrimSpace(t[len(m):])
		}
	}
	if i := strings.Index(t, ". "); i > 0 && i <= 3 && strings.Trim(t[:i], "0123456789") == "" {
		return strings.TrimSpace(t[i+2:])
	}
	return t
}

// quoteAround shortens a sentence to a quote that keeps the phrase at byte
// offset at in view.
func quoteAround(sen string, at int) string {
	r := []rune(sen)
	if len(r) <= putOffQuote {
		return sen
	}
	pos := len([]rune(sen[:min(at, len(sen))]))
	from := 0
	if pos > putOffQuote/2 {
		from = min(pos-putOffQuote/3, len(r)-putOffQuote+1)
	}
	q := string(r[from:])
	if from > 0 {
		q = "…" + q
	}
	return excerpt(q, putOffQuote)
}

// dedupeNotes keeps the first of notes that quote the same text.
func dedupeNotes(notes []PutOffNote) []PutOffNote {
	seen := map[string]bool{}
	out := notes[:0]
	for _, n := range notes {
		k := strings.ToLower(n.Text)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}
