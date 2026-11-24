package core

import (
	"context"
	"math"
	"sort"
	"strings"
)

// maxSimilarIssues is the most similar issues listed for proposed text.
const maxSimilarIssues = 5

// SimilarQuery is the text of an issue about to be filed and where it goes.
type SimilarQuery struct {
	Project     string `json:"project"`
	Parent      string `json:"parent"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// SimilarIssue is an open issue whose text is like the proposed one's.
type SimilarIssue struct {
	Key     string  `json:"key"`
	Title   string  `json:"title"`
	Status  string  `json:"status"`
	Project string  `json:"project"`
	Score   float64 `json:"score"`
	// Confidence is high, medium or low, the related-issues bands.
	Confidence string `json:"confidence"`
	// SameProject: in the project the new issue goes to.
	SameProject bool `json:"same_project"`
	// Sibling: under the same parent as the new issue, as the parts of
	// one plan are, which read alike without being the same work.
	Sibling bool `json:"sibling,omitempty"`
	// Same: the new issue is likely this one again: in the same project,
	// not a sibling, not named by the new text (a follow-up names what it
	// follows), and the same title or nearly the same words.
	Same      bool   `json:"same,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	Reason    string `json:"reason"`
}

// SimilarIssues lists the open issues the actor can see that read like the
// proposed text, best first: the project's issues with the same title, then
// the rest at medium similarity or more, from any project. The parent is
// left out. Nothing is filed or linked.
func (s *Service) SimilarIssues(ctx context.Context, actor *Actor, q SimilarQuery) ([]SimilarIssue, error) {
	out := []SimilarIssue{}
	if strings.TrimSpace(q.Title) == "" {
		return out, nil
	}
	var parentID int64
	project := q.Project
	if q.Parent != "" {
		id, err := s.resolveIssueID(ctx, s.db.R, q.Parent)
		if err != nil {
			return nil, err
		}
		parent, err := s.issueByID(ctx, s.db.R, id, false)
		if err != nil || !s.canSee(ctx, s.db.R, actor, id, parent.ProjectID) || (parent.Draft && actor.HidesDrafts()) {
			return nil, notFound("issue " + q.Parent)
		}
		parentID = id
		if project == "" {
			project = parent.ProjectKey
		}
	}
	if project == "" {
		return nil, invalid("project is required")
	}
	p, err := s.projectByRef(ctx, s.db.R, project)
	if err != nil {
		return nil, err
	}
	if role, err := s.projectRole(ctx, s.db.R, actor, p.ID); err != nil || role <= RoleNone {
		return nil, notFound("project " + project)
	}
	idx, err := s.similarityIndex(ctx)
	if err != nil {
		return nil, err
	}
	vec := idx.vector(q.Title, q.Description)
	named := map[string]bool{}
	for _, k := range proseKeyRE.FindAllString(q.Title+"\n"+q.Description, -1) {
		named[k] = true
	}
	seen := map[int64]bool{parentID: true}
	add := func(id int64, score float64, sameTitle bool) {
		d := idx.docs[id]
		if len(out) >= maxSimilarIssues || seen[id] || d == nil || !d.open || !s.canSee(ctx, s.db.R, actor, id, d.projectID) {
			return
		}
		seen[id] = true
		i, err := s.issueByID(ctx, s.db.R, id, false)
		if err != nil {
			return
		}
		m := SimilarIssue{Key: i.Key, Title: i.Title, Status: i.Status.Name, Project: i.ProjectKey,
			Score: math.Round(score*100) / 100, Confidence: simConfidence(score), SameProject: i.ProjectID == p.ID,
			Sibling: parentID != 0 && i.ParentID != nil && *i.ParentID == parentID, ClaimedBy: i.ClaimedBy}
		if sameTitle {
			m.Confidence, m.Reason = "high", "The same title."
		} else {
			m.Reason = "Shares " + idx.shared(vec, d.vec)
		}
		m.Same = m.SameProject && !m.Sibling && !named[i.Key] && (sameTitle || score >= simDuplicate)
		out = append(out, m)
	}
	same := idx.sameTitle(p.ID, normalTitle(q.Title))
	sort.SliceStable(same, func(a, b int) bool { return cosine(vec, idx.docs[same[a]].vec) > cosine(vec, idx.docs[same[b]].vec) })
	for _, id := range same {
		add(id, cosine(vec, idx.docs[id].vec), true)
	}
	for _, m := range idx.similar(vec, 0, simMedium) {
		add(m.id, m.score, false)
	}
	return out, nil
}
