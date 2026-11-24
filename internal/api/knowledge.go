package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

func wantsText(r *http.Request) bool { return r.URL.Query().Get("format") == "text" }

func (s *Server) listGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	list, err := s.svc.ListGlossary(r.Context(), actorFrom(r), core.GlossaryQuery{
		Project: v.Get("project"), Own: queryBool(r, "own"), Text: v.Get("q"), Limit: queryInt(r, "limit", 500)})
	if err != nil {
		return nil, err
	}
	if wantsText(r) {
		return textResponse(glossaryListText(list)), nil
	}
	return list, nil
}

func (s *Server) lookupGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	res, err := s.svc.LookupGlossary(r.Context(), actorFrom(r), v.Get("term"), v.Get("project"))
	if err != nil {
		return nil, err
	}
	s.svc.NoteGlossaryLookup(r.Context(), actorFrom(r), res)
	if wantsText(r) {
		return textResponse(glossaryLookupText(res, v.Get("project"))), nil
	}
	return res, nil
}

func (s *Server) getGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	g, err := s.svc.GetGlossary(r.Context(), actorFrom(r), id)
	if err != nil {
		return nil, err
	}
	s.svc.NoteLookup(r.Context(), actorFrom(r), core.KGlossary, id, "")
	if wantsText(r) {
		return textResponse(glossaryEntryText(g, true)), nil
	}
	return g, nil
}

func (s *Server) createGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.GlossaryInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateGlossary(r.Context(), actorFrom(r), in)
}

func (s *Server) updateGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.GlossaryInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateGlossary(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteGlossary(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteGlossary(r.Context(), actorFrom(r), id)
}

// designID is the note a path names: its number, or a code (ADR7, APP-ADR7;
// ?project= says where a bare code is).
func (s *Server) designID(r *http.Request) (int64, error) {
	return s.svc.ResolveDesign(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("project"))
}

func (s *Server) listDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.DesignQuery{Project: v.Get("project"), Own: queryBool(r, "own"), Kind: v.Get("kind"), Status: v.Get("status"),
		Text: v.Get("q"), Pending: queryBool(r, "pending"), Limit: queryInt(r, "limit", 500)}
	if p := v.Get("parent"); p != "" {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: parent must be a note id (0 for top level)", errBadRequest)
		}
		q.Parent = &n
	}
	list, err := s.svc.ListDesign(r.Context(), actorFrom(r), q)
	if err != nil {
		return nil, err
	}
	if wantsText(r) {
		return textResponse(designListText(list)), nil
	}
	return list, nil
}

func (s *Server) getDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.designID(r)
	if err != nil {
		return nil, err
	}
	n, err := s.svc.GetDesign(r.Context(), actorFrom(r), id)
	if err != nil {
		return nil, err
	}
	if !queryBool(r, "listed") { // listed=1: wp brief listing it unasked, recorded as shown
		s.svc.NoteLookup(r.Context(), actorFrom(r), core.KDesign, id, "")
	}
	if wantsText(r) {
		return textResponse(designNoteText(n)), nil
	}
	return n, nil
}

func (s *Server) createDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.DesignInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateDesign(r.Context(), actorFrom(r), in)
}

func (s *Server) updateDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.designID(r)
	if err != nil {
		return nil, err
	}
	var in core.DesignInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateDesign(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.designID(r)
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteDesign(r.Context(), actorFrom(r), id)
}

func (s *Server) addDesignAddition(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.designID(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddDesignAddition(r.Context(), actorFrom(r), id, in.Body)
}

func (s *Server) approveDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.designID(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Version string `json:"version"` // the updated_at you saw
		Note    string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ApproveDesign(r.Context(), actorFrom(r), id, in.Version)
}

func (s *Server) reviewDesign(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "rid")
	if err != nil {
		return nil, err
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ReviewDesign(r.Context(), actorFrom(r), id, r.PathValue("action"), in.Note)
}

func (s *Server) designReview(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.DesignReview(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

func (s *Server) spec(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	topic := strings.TrimSpace(v.Get("topic"))
	if topic == "" {
		ov, err := s.svc.GetSpecOverview(r.Context(), actorFrom(r), v.Get("project"))
		if err != nil {
			return nil, err
		}
		if wantsText(r) {
			return textResponse(specOverviewText(ov)), nil
		}
		return ov, nil
	}
	sp, err := s.svc.GetSpec(r.Context(), actorFrom(r), topic, v.Get("project"))
	if err != nil {
		return nil, err
	}
	s.svc.NoteSpecLookup(r.Context(), actorFrom(r), sp)
	if wantsText(r) {
		return textResponse(specText(sp)), nil
	}
	return sp, nil
}

func linkEndFromQuery(r *http.Request) (core.LinkEnd, error) {
	v := r.URL.Query()
	e := core.LinkEnd{Type: v.Get("type"), Key: v.Get("key"), Ref: v.Get("ref")}
	if id := v.Get("id"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			if e.Type == core.KIssue {
				e.Key = id
				return e, nil
			}
			return e, fmt.Errorf("%w: id must be a number (or give key= for issues)", errBadRequest)
		}
		e.ID = n
	}
	return e, nil
}

func (s *Server) listItemLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	e, err := linkEndFromQuery(r)
	if err != nil {
		return nil, err
	}
	return s.svc.ItemLinks(r.Context(), actorFrom(r), e)
}

func (s *Server) addItemLink(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.LinkInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddItemLink(r.Context(), actorFrom(r), in)
}

func (s *Server) removeItemLink(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RemoveItemLink(r.Context(), actorFrom(r), id)
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	it, err := s.svc.Resolve(r.Context(), actorFrom(r), v.Get("ref"), v.Get("project"))
	if errors.Is(err, core.ErrNotFound) && queryBool(r, "soft") {
		return nil, nil
	}
	if err == nil {
		s.svc.NoteLookup(r.Context(), actorFrom(r), it.Type, it.ID, "")
	}
	return it, err
}

// resolveRefs answers what a text names (WP-122): the issues, requests,
// decisions, pages, pull requests and glossary terms its words are codes of.
func (s *Server) resolveRefs(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Text    string `json:"text"`
		Project string `json:"project"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	refs, err := s.svc.ResolveRefs(r.Context(), actorFrom(r), in.Text, in.Project)
	if err != nil {
		return nil, err
	}
	return map[string]any{"refs": refs}, nil
}

func (s *Server) searchAll(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.AllQuery{Text: v.Get("q"), Archived: queryBool(r, "archived"), Limit: queryInt(r, "limit", 10), Near: v.Get("near")}
	if k := v.Get("kinds"); k != "" {
		q.Kinds = []string{k}
	}
	for _, p := range strings.Split(v.Get("project"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			q.Projects = append(q.Projects, p)
		}
	}
	res, err := s.svc.SearchAll(r.Context(), actorFrom(r), q)
	if err != nil {
		return nil, err
	}
	if wantsText(r) {
		return textResponse(searchAllText(res)), nil
	}
	return res, nil
}

func (s *Server) agentTakeKey(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Agent        string `json:"agent"`
		CrossProject string `json:"cross_project"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	t, err := s.svc.AgentTake(r.Context(), actorFrom(r), r.PathValue("key"), in.Agent)
	if err != nil {
		return nil, err
	}
	s.svc.NoteCrossProject(r.Context(), actorFrom(r), in.CrossProject, t)
	warning := s.svc.TaskSessionWarning(r.Context(), actorFrom(r), t, sessionHint(r))
	if wantsJSON(r) {
		return map[string]any{"task": t.Issue, "workflow": t.Workflow, "step": t.Step, "related": t.Related, "resumed": t.Resumed, "skipped": t.Skipped, "why": t.Why, "design": t.Design, "comments": t.Comments, "warning": warning}, nil
	}
	return textResponse(agentTaskText(t, s.taskTextFor(r, t, s.onlyClaim(r, t))) + warningText(warning)), nil
}

// noteShown records what the CLI listed to an agent unasked (wp brief); the
// answer counts what was recorded and what named nothing.
func (s *Server) noteShown(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Via   string          `json:"via"`
		Items []core.ShownRef `json:"items"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	rec, unknown, err := s.svc.NoteShown(r.Context(), actorFrom(r), in.Via, in.Items)
	if err != nil {
		return nil, err
	}
	return map[string]int{"recorded": rec, "unknown": unknown}, nil
}
