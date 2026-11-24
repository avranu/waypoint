package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) (any, error) {
	o, err := s.svc.ListGoals(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(goalsText(o)), nil
}

// suggestGoals asks the model for goals and rewordings now; a person waits
// for the answer (the weekly check runs it too).
func (s *Server) suggestGoals(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SuggestGoals(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

// goalWork lists the open work likeliest to serve a goal, to link from the
// goal (WP-679).
func (s *Server) goalWork(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	v := r.URL.Query()
	return s.svc.GoalWork(r.Context(), actorFrom(r), id, v.Get("project"), v.Get("q"))
}

// suggestTaskGoal asks the model which goal a task serves; nothing is
// saved until the person clicks (WP-679).
func (s *Server) suggestTaskGoal(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SuggestTaskGoal(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) goalGaps(w http.ResponseWriter, r *http.Request) (any, error) {
	o, err := s.svc.ListGoals(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(goalGapsText(o)), nil
}

func (s *Server) trace(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.TraceQuery{Ref: v.Get("ref"), Project: v.Get("project"), Repo: v.Get("repo"), Commit: v.Get("commit")}
	if p := v.Get("pull"); p != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(p, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: pull is a pull request number", errBadRequest)
		}
		q.Pull = n
	}
	for _, k := range strings.Split(v.Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			q.Keys = append(q.Keys, k)
		}
	}
	tr, err := s.svc.Trace(r.Context(), actorFrom(r), q)
	if err != nil || !wantsText(r) {
		return tr, err
	}
	return textResponse(traceText(tr, v.Get("line"))), nil
}

func (s *Server) confirmGoalLink(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ConfirmGoalLink(r.Context(), actorFrom(r), id)
}

func (s *Server) dismissGoalLink(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DismissGoalLink(r.Context(), actorFrom(r), id)
}

// answerDriftFlag records a person's answer to a drift flag (WP-559).
func (s *Server) answerDriftFlag(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Answer string `json:"answer"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.AnswerDriftFlag(r.Context(), actorFrom(r), id, in.Answer)
}

func (s *Server) dismissGoalMatch(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Item core.LinkEnd `json:"item"`
		Goal int64        `json:"goal"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.DismissGoalMatch(r.Context(), actorFrom(r), in.Item, in.Goal)
}

// acknowledgeGoalLinks stops asking about waiting links without
// confirming them ("Stop asking" on the Goals view).
func (s *Server) acknowledgeGoalLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	n, err := s.svc.AcknowledgeGoalLinks(r.Context(), actorFrom(r), in.IDs)
	if err != nil {
		return nil, err
	}
	return map[string]int{"acknowledged": n}, nil
}

// itemName names an item in a line: "WP-3 Title", "goal #4 Title".
func itemName(it core.KItem) string {
	switch it.Type {
	case core.KIssue:
		if it.Key == "" {
			return it.Title
		}
		return it.Key + " " + it.Title
	case core.KPage:
		if it.Key != "" {
			return fmt.Sprintf("page %s %s", it.Key, it.Title)
		}
		return fmt.Sprintf("page #%d %s", it.ID, it.Title)
	case core.KDesign:
		if it.ID == 0 {
			return it.Title
		}
		kind := cmpStr(it.Kind, "note")
		s := fmt.Sprintf("%s %s#%d %q", kind, codePrefix(it.Key, it.ProjectKey), it.ID, it.Title)
		if it.State == "proposed" {
			s += " (not approved)"
		}
		return s
	}
	return it.Title
}

// basisWords says who stands behind a link.
func basisWords(basis, agent string, by *core.UserRef, score *float64) string {
	switch basis {
	case "person":
		if by != nil {
			return "by " + by.DisplayName
		}
		return "by a person"
	case "inferred":
		if score != nil {
			return fmt.Sprintf("Waypoint's guess, %.2f", *score)
		}
		return "Waypoint's guess"
	case "text":
		return "a [[link]] in the text"
	}
	if agent != "" {
		return "by agent " + agent
	}
	return "by an agent"
}

var groundingShort = map[string]string{
	core.GroundPersonGoal:    "by people",
	core.GroundAgentGoal:     "through an agent's link",
	core.GroundInferredGoal:  "only by Waypoint's guess",
	core.GroundProposedGoal:  "to an unapproved goal",
	core.GroundPersonRequest: "a person asked, no goal",
	core.GroundNone:          "no goal, no person",
}

func groundingCounts(m map[string]int) string {
	var parts []string
	for _, g := range core.Groundings {
		if n := m[g]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, groundingShort[g]))
		}
	}
	return strings.Join(parts, ", ")
}

func goalsText(o *core.GoalsOverview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goals for %s %s (its own, its parents' and global)\n", o.Project, o.ProjectName)
	if len(o.Goals) == 0 {
		b.WriteString("\nNo goals yet. A person adds one: wp goal add \"what people get, in one sentence\"\n")
	}
	for _, g := range o.Goals {
		n := g.Note
		pad := strings.Repeat("  ", g.Depth)
		state := "approved"
		switch {
		case n.Requested != nil:
			state = "written for " + n.Requested.Issue + ", not approved"
		case !n.Approved:
			state = "SUGGESTED, not approved"
		}
		where := ""
		if n.Inherited {
			where = ", from " + cmpStr(n.ProjectKey, "global")
		}
		fmt.Fprintf(&b, "\n%s- %s  [%sgoal #%d, %s%s]\n", pad, n.Title, codePrefix(n.Code, n.ProjectKey), n.ID, state, where)
		c := g.Coverage
		if c.Items == 0 {
			fmt.Fprintf(&b, "%s  nothing traces to it yet\n", pad)
		} else {
			fmt.Fprintf(&b, "%s  %s (%s open, %d finished this week): %s\n", pad, countWord(c.Items, "item"), countWord(c.Open, "task"), c.Finished, groundingCounts(c.ByGrounding))
		}
		for _, w := range g.Wording {
			fmt.Fprintf(&b, "%s  wording: %s\n", pad, w)
		}
	}
	if len(o.Totals) > 0 {
		fmt.Fprintf(&b, "\nOpen tasks: %s\n", groundingCounts(o.Totals))
	}
	if o.Drift != nil && o.Drift.Summary != "" {
		b.WriteString(core.UpperFirst(o.Drift.Summary) + ".\n")
	}
	if n := len(o.Links) + o.UntracedAll + o.UnapprovedCount; n > 0 {
		fmt.Fprintf(&b, "Gaps: %d untraced tasks, %d links no person confirmed, %d notes no person approved (wp goals gaps)\n", o.UntracedAll, len(o.Links), o.UnapprovedCount)
	}
	return b.String()
}

func goalGapsText(o *core.GoalsOverview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Gaps in %s %s\n", o.Project, o.ProjectName)
	if !o.Scoped {
		b.WriteString("\nNo approved goal covers this project, so nothing is checked against intent.\n")
	}
	if o.UntracedAll > 0 {
		fmt.Fprintf(&b, "\nOpen tasks that trace to no approved goal (%d", o.UntracedAll)
		if o.UntracedAll > len(o.Untraced) {
			fmt.Fprintf(&b, ", first %d", len(o.Untraced))
		}
		b.WriteString("):\n")
		for _, u := range o.Untraced {
			line := "  " + itemName(u.Item) + " — " + groundingShort[u.Grounding]
			if u.Agent != "" {
				line += ", written by " + u.Agent
			}
			if c := u.Candidate; c != nil {
				line += fmt.Sprintf("\n      likely serves goal #%d %q (%.2f; %s)", c.Goal.ID, c.Goal.Title, c.Score, strings.TrimSuffix(c.Reason, "."))
			}
			b.WriteString(line + "\n")
		}
	}
	if len(o.DriftFlags) > 0 {
		b.WriteString("\nDrift waiting for a person (a model's read; only a person can answer, on the Goals view):\n")
		for _, f := range o.DriftFlags {
			line := "  " + f.Headline
			if f.PR != nil {
				line = fmt.Sprintf("  %s#%d: %s", f.PR.Repo, f.PR.Number, f.Headline)
			}
			if f.Answer == core.DriftAnswerStop {
				line += " A person said to stop this."
			}
			b.WriteString(line + "\n")
		}
	}
	if len(o.Links) > 0 {
		fmt.Fprintf(&b, "\nLinks to goals no person confirmed (%d):\n", len(o.Links))
		for _, l := range o.Links {
			fmt.Fprintf(&b, "  link %d: %s → goal #%d %q (%s)\n", l.LinkID, itemName(l.Item), l.Goal.ID, l.Goal.Title, basisWords(l.Basis, l.Agent, nil, l.Score))
		}
	}
	pending := 0
	for _, g := range o.Goals {
		if !g.Note.Approved && g.Note.Requested == nil {
			pending++
		}
	}
	if pending > 0 || len(o.Unapproved) > 0 {
		b.WriteString("\nNo person approved:\n")
		for _, g := range o.Goals {
			if !g.Note.Approved && g.Note.Requested == nil {
				fmt.Fprintf(&b, "  goal #%d %q\n", g.Note.ID, g.Note.Title)
			}
		}
		for _, it := range o.Unapproved {
			b.WriteString("  " + itemName(it) + "\n")
		}
	}
	flagged := false
	for _, g := range o.Goals {
		if len(g.Wording) == 0 {
			continue
		}
		if !flagged {
			b.WriteString("\nGoals whose wording could be clearer:\n")
			flagged = true
		}
		fmt.Fprintf(&b, "  goal #%d %q %s\n", g.Note.ID, g.Note.Title, strings.Join(g.Wording, "; "))
	}
	if len(o.Ask) > 0 {
		b.WriteString("\nTo ask a person (wp ask):\n")
		for _, q := range o.Ask {
			b.WriteString("  - " + q + "\n")
		}
	}
	// Agent-made or inferred goal links are not gaps to fix, and no person vouches for them.
	if n := o.Totals[core.GroundAgentGoal] + o.Totals[core.GroundInferredGoal]; n > 0 {
		fmt.Fprintf(&b, "\nOpen tasks that reach a goal only through links no person made or confirmed: %d (%s).\n", n,
			groundingCounts(map[string]int{core.GroundAgentGoal: o.Totals[core.GroundAgentGoal], core.GroundInferredGoal: o.Totals[core.GroundInferredGoal]}))
	}
	if b.Len() == len(fmt.Sprintf("Gaps in %s %s\n", o.Project, o.ProjectName)) {
		b.WriteString("\nNone: every open task traces to an approved goal through links people made or confirmed.\n")
	}
	return b.String()
}

func hopLine(h core.TraceHop) string {
	return fmt.Sprintf("%s —%s (%s)→ %s", itemName(h.From), h.Kind, basisWords(h.Basis, h.Agent, h.By, h.Score), itemName(h.To))
}

func traceText(tr *core.ItemTrace, line string) string {
	var b strings.Builder
	if line != "" {
		b.WriteString(line + "\n")
	}
	traceBody(&b, tr, "")
	return b.String()
}

func traceBody(b *strings.Builder, tr *core.ItemTrace, pad string) {
	b.WriteString(pad + itemName(tr.Item) + "\n")
	b.WriteString(pad + "  " + tr.Words + "\n")
	if tr.Note != "" {
		b.WriteString(pad + "  " + tr.Note + "\n")
	}
	for _, r := range tr.Routes {
		fmt.Fprintf(b, "%s  route to %s\n", pad, itemName(r.Goal))
		for _, h := range r.Hops {
			b.WriteString(pad + "    " + hopLine(h) + "\n")
		}
	}
	if len(tr.Through) == 0 && tr.Item.Type != core.KURL {
		switch {
		case tr.Person:
			b.WriteString(pad + "  a person wrote or approved it")
		case tr.PersonHops != nil:
			fmt.Fprintf(b, "%s  nearest person: %s up", pad, countWord(*tr.PersonHops, "step"))
		default:
			b.WriteString(pad + "  no person wrote or approved anything it hangs from")
		}
		if tr.PersonTouches > 0 {
			fmt.Fprintf(b, "; human influences on the way: %d", tr.PersonTouches)
		}
		b.WriteString("\n")
		for _, h := range tr.PersonRoute {
			b.WriteString(pad + "    " + hopLine(h) + "\n")
		}
	}
	if c := tr.Candidate; c != nil {
		fmt.Fprintf(b, "%s  likely serves goal #%d %q (%.2f; %s); a person links it with: wp goal link %s %d\n", pad, c.Goal.ID, c.Goal.Title, c.Score,
			strings.TrimSuffix(c.Reason, "."), cmpStr(tr.Item.Key, fmt.Sprint(tr.Item.ID)), c.Goal.ID)
	}
	if !tr.Scoped && len(tr.Through) == 0 {
		b.WriteString(pad + "  (no approved goal covers its project)\n")
	}
	for _, t := range tr.Through {
		traceBody(b, t, pad+"  ")
	}
}

// countWord is "1 step" or "3 steps".
func countWord(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
