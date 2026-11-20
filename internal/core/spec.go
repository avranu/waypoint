package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// Specs (WP-D10): everything an agent needs to know about what people
// want for a feature, screen or decision, in one answer.

// Spec is a design note with what surrounds it.
type Spec struct {
	Topic string      `json:"topic"`
	Note  *DesignNote `json:"note"`
	// Parts are its sub-notes (two levels), with their bodies.
	Parts   []*DesignNote `json:"parts"`
	Related []SpecRelated `json:"related"`
	Terms   []SpecTerm    `json:"terms"`
	Issues  []KItem       `json:"issues"`
	// Changes are some PRs and commits linked to the note or its issues
	// (not every one).
	Changes []KItem     `json:"changes"`
	Files   []string    `json:"files"`
	Others  []DesignRef `json:"others"` // other notes that matched the topic
}

// SpecRelated is a note linked to the spec's note.
type SpecRelated struct {
	Rel       string    `json:"rel"`
	Direction string    `json:"direction"`
	Note      DesignRef `json:"note"`
	Summary   string    `json:"summary"`
}

// SpecTerm is a glossary term the note uses.
type SpecTerm struct {
	ID         int64  `json:"id"`
	Term       string `json:"term"`
	Definition string `json:"definition"`
	Locked     bool   `json:"locked"`
}

// SpecOverview is a project's intent at a glance: goals and principles in
// full, an index of the rest.
type SpecOverview struct {
	Project    string                 `json:"project,omitempty"`
	Goals      []*DesignNote          `json:"goals"`
	Principles []*DesignNote          `json:"principles"`
	Index      map[string][]DesignRef `json:"index"` // kind → notes
	Pending    int                    `json:"pending"`
}

// GetSpec finds the note a topic names (an id, an exact title, then the
// best full-text match) in a project's scope chain and gathers its spec.
func (s *Service) GetSpec(ctx context.Context, actor *Actor, topic, project string) (*Spec, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, invalid("which feature, screen or decision? Give a topic (or ask for the overview)")
	}
	var note *DesignNote
	var others []DesignRef
	// A number, or a code (ADR7, APP-ADR7) that names a note.
	id := int64(0)
	if n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimPrefix(topic, "design:"), "#"), 10, 64); err == nil {
		id = n
	} else if _, ok := ParseDesignCode(strings.TrimPrefix(topic, "design:")); ok {
		if id, err = s.ResolveDesign(ctx, actor, topic, project); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if id > 0 {
		d, err := s.GetDesign(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		note = d
	} else {
		matches, err := s.ListDesign(ctx, actor, DesignQuery{Project: project, Text: topic, Status: "all", Limit: 6})
		if err != nil {
			return nil, err
		}
		// Prefer active notes; exact titles already sort first.
		best := -1
		for i, m := range matches {
			if strings.EqualFold(m.Title, topic) && !m.Archived() {
				best = i
				break
			}
		}
		if best < 0 {
			for i, m := range matches {
				if !m.Archived() {
					best = i
					break
				}
			}
		}
		if best < 0 && len(matches) > 0 {
			best = 0
		}
		if best < 0 {
			return nil, notFound("design note about " + strconv.Quote(topic) + " (wp design ls lists them)")
		}
		for i, m := range matches {
			if i != best {
				others = append(others, m.Ref())
			}
		}
		if note, err = s.GetDesign(ctx, actor, matches[best].ID); err != nil {
			return nil, err
		}
	}
	sp := &Spec{Topic: topic, Note: note, Parts: []*DesignNote{}, Related: []SpecRelated{}, Terms: []SpecTerm{},
		Issues: []KItem{}, Changes: []KItem{}, Files: []string{}, Others: others}
	if sp.Others == nil {
		sp.Others = []DesignRef{}
	}
	// Sub-notes, two levels down, with their bodies and additions.
	var walk func(refs []DesignRef, depth int) error
	walk = func(refs []DesignRef, depth int) error {
		for _, c := range refs {
			if len(sp.Parts) >= 30 {
				return nil
			}
			d, err := s.GetDesign(ctx, actor, c.ID)
			if err != nil {
				continue
			}
			if d.Archived() {
				continue
			}
			sp.Parts = append(sp.Parts, d)
			if depth < 2 {
				if err := walk(d.Children, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(note.Children, 1); err != nil {
		return nil, err
	}
	seenChange := map[string]bool{}
	addChange := func(it KItem) {
		if len(sp.Changes) < 10 && !seenChange[it.Ref] {
			seenChange[it.Ref] = true
			sp.Changes = append(sp.Changes, it)
		}
	}
	notes := append([]*DesignNote{note}, sp.Parts...)
	seenTerm, seenIssue, seenNote := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, n := range notes {
		for _, l := range n.Links {
			it := l.Item
			switch it.Type {
			case KDesign:
				if n != note || seenNote[it.ID] {
					continue
				}
				seenNote[it.ID] = true
				sum := ""
				if d, _, err := s.designFor(ctx, s.db.R, actor, it.ID, RoleViewer); err == nil {
					sum = summary(d.Body)
				}
				sp.Related = append(sp.Related, SpecRelated{Rel: l.Rel, Direction: l.Direction, Summary: sum,
					Note: DesignRef{ID: it.ID, Title: it.Title, Kind: it.Kind, Status: it.State, Approved: it.State != "proposed", ProjectKey: it.ProjectKey}})
			case KGlossary:
				if seenTerm[it.ID] {
					continue
				}
				seenTerm[it.ID] = true
				if g, _, err := s.glossaryFor(ctx, s.db.R, actor, it.ID, RoleViewer); err == nil {
					sp.Terms = append(sp.Terms, SpecTerm{ID: g.ID, Term: g.Term, Definition: g.Definition, Locked: g.Locked})
				}
			case KIssue:
				if seenIssue[it.ID] {
					continue
				}
				seenIssue[it.ID] = true
				sp.Issues = append(sp.Issues, it)
			case KURL:
				if it.Kind == "pull" || it.Kind == "commit" {
					addChange(it)
				}
			case KFile:
				sp.Files = append(sp.Files, it.Ref)
			}
		}
	}
	// PRs and commits on the linked issues.
	for _, it := range sp.Issues {
		if len(sp.Changes) >= 10 {
			break
		}
		links, err := s.externalLinks(ctx, s.db.R, it.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if l.Kind != "pull" && l.Kind != "commit" {
				continue
			}
			title := l.Title
			switch {
			case l.Kind == "pull" && l.Number != nil:
				title = l.Repo + " PR #" + itoa64(*l.Number) + cond(l.Title != "", ": "+l.Title, "")
			case l.Kind == "commit":
				title = l.Repo + " commit " + l.Title
			}
			addChange(KItem{Type: KURL, Ref: l.URL, Title: title, Kind: l.Kind, State: l.State, Repo: l.Repo, Number: l.Number, Provider: l.Provider, Key: it.Key})
		}
	}
	return sp, nil
}

func cond(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// summary is the first paragraph of markdown, shortened.
func summary(body string) string {
	body = strings.TrimSpace(body)
	for _, para := range strings.Split(body, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		p = strings.Join(strings.Fields(p), " ")
		if len(p) > 240 {
			cut := 240
			for cut > 0 && (p[cut]&0xC0) == 0x80 {
				cut--
			}
			p = p[:cut] + "…"
		}
		return p
	}
	return ""
}

// GetSpecOverview is a project's (or the global) intent at a glance.
func (s *Service) GetSpecOverview(ctx context.Context, actor *Actor, project string) (*SpecOverview, error) {
	notes, err := s.ListDesign(ctx, actor, DesignQuery{Project: project, Limit: 1000})
	if err != nil {
		return nil, err
	}
	ov := &SpecOverview{Project: project, Goals: []*DesignNote{}, Principles: []*DesignNote{}, Index: map[string][]DesignRef{}}
	for _, n := range notes {
		ov.Pending += n.Pending
		if !n.Approved {
			ov.Pending++
		}
		switch n.Kind {
		case "goal", "principle":
			full, err := s.GetDesign(ctx, actor, n.ID)
			if err != nil {
				return nil, err
			}
			full.History, full.Links = nil, nil
			if n.Kind == "goal" {
				ov.Goals = append(ov.Goals, full)
			} else {
				ov.Principles = append(ov.Principles, full)
			}
		default:
			ov.Index[n.Kind] = append(ov.Index[n.Kind], n.Ref())
		}
	}
	return ov, nil
}

// DesignNotesFor lists the design notes linked to an issue (either way)
// that the actor can read, for task briefs.
func (s *Service) DesignNotesFor(ctx context.Context, actor *Actor, issueID int64) []DesignRef {
	// A note linked only as in tension with the task is marked so: a
	// task can't be for a goal it works against.
	rows, err := s.db.R.QueryContext(ctx, `SELECT CASE WHEN src_type = 'design' THEN src_id ELSE dst_id END AS note, min(rel = 'tension') FROM knowledge_links
		WHERE (src_type = 'design' AND dst_type = 'issue' AND dst_id = ?) OR (src_type = 'issue' AND src_id = ? AND dst_type = 'design')
		GROUP BY note ORDER BY min(id)`, issueID, issueID)
	if err != nil {
		return nil
	}
	var ids []int64
	tension := map[int64]bool{}
	for rows.Next() {
		var id sql.NullInt64
		var only bool
		if rows.Scan(&id, &only) == nil && id.Valid {
			ids = append(ids, id.Int64)
			tension[id.Int64] = only
		}
	}
	rows.Close()
	var out []DesignRef
	for _, id := range ids {
		if n, _, err := s.designFor(ctx, s.db.R, actor, id, RoleViewer); err == nil {
			ref := n.Ref()
			ref.Tension = tension[id]
			out = append(out, ref)
		}
	}
	return out
}
