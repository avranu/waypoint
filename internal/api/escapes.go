package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

const maxFixTraceBody = 1 << 20

func (s *Server) escapesDue(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.EscapesDue(r.Context(), actorFrom(r), q.Get("machine"), q.Get("repo"), queryInt(r, "limit", 0))
}

func (s *Server) escapeTrace(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.FixTraceInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFixTraceBody)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return map[string]bool{"ok": true}, s.svc.RecordFixTrace(r.Context(), actorFrom(r), in)
}

// Proving fixes (WP-709): runners ask which traced fixes to prove and
// upload whether their tests fail without them.
func (s *Server) escapeProofsDue(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.EscapeProofsDue(r.Context(), actorFrom(r), q.Get("machine"), q.Get("repo"), queryInt(r, "limit", 0))
}

func (s *Server) escapeProof(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.FixProofInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFixTraceBody)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return map[string]bool{"ok": true}, s.svc.RecordFixProof(r.Context(), actorFrom(r), in)
}

func escapeQuery(r *http.Request) core.EscapeQuery {
	q := r.URL.Query()
	return core.EscapeQuery{Project: q.Get("project"), Repo: q.Get("repo"), Key: q.Get("key"),
		Days: queryInt(r, "days", 0), Limit: queryInt(r, "limit", 0)}
}

func (s *Server) escapeStats(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.svc.EscapeSummary(r.Context(), actorFrom(r), escapeQuery(r))
	if err != nil || !wantsText(r) {
		return st, err
	}
	return textResponse(escapeStatsText(st)), nil
}

func (s *Server) escapeList(w http.ResponseWriter, r *http.Request) (any, error) {
	list, err := s.svc.EscapeList(r.Context(), actorFrom(r), escapeQuery(r))
	if err != nil || !wantsText(r) {
		return list, err
	}
	return textResponse(escapeListText(list)), nil
}

func (s *Server) escapeFiles(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.EscapeFilesFor(r.Context(), actorFrom(r), r.URL.Query().Get("repo"))
}

func escapeGroupsText(o *strings.Builder, title string, gs []core.EscapeGroup) {
	if len(gs) == 0 {
		return
	}
	parts := make([]string, 0, len(gs))
	for _, g := range gs {
		parts = append(parts, fmt.Sprintf("%s %d (%s)", g.Name, g.Count, core.EscapeShortDays(g.Median)))
	}
	fmt.Fprintf(o, "%s: %s\n", title, strings.Join(parts, ", "))
}

func escapeStatsText(st *core.EscapeStats) string {
	var o strings.Builder
	fmt.Fprintf(&o, "Bug fixes, last %d days: %d, %d traced to a cause, %d with confidence 0.5 or more", st.Days, st.Total, st.Traced, st.Confident)
	if st.Pending > 0 {
		fmt.Fprintf(&o, "; %d more waiting for a trace", st.Pending)
	}
	o.WriteString("\n")
	if st.Total == 0 {
		return o.String()
	}
	fmt.Fprintf(&o, "Median lifetime: %s (confident), %s (all traced)\n", core.EscapeShortDays(st.MedianDays), core.EscapeShortDays(st.MedianAllDays))
	escapeGroupsText(&o, "Found at (median life)", st.Stages)
	escapeGroupsText(&o, "Found by", st.Detectors)
	escapeGroupsText(&o, "Class", st.Tags)
	fmt.Fprintf(&o, "Reopened after the fix: %d. Fixes of fixes: %d.\n", st.Reopens, st.FixOfFix)
	for _, c := range st.Chains {
		fmt.Fprintf(&o, "  %s\n", strings.Join(c, " -> "))
	}
	proofsText(&o, st)
	o.WriteString("Per week:\n")
	for _, w := range st.Weeks {
		fmt.Fprintf(&o, "  %s  %3d  median %s\n", w.Start, w.Count, core.EscapeShortDays(w.Median))
	}
	return o.String()
}

// proofsText says how many fixes a failing test proves and how often each
// kind was fixed again (WP-709).
func proofsText(o *strings.Builder, st *core.EscapeStats) {
	fixes, proven := 0, 0
	for _, p := range st.Proofs {
		fixes += p.Fixes
		if p.Outcome == "proven" {
			proven = p.Fixes
		}
	}
	proved := fixes - st.ProofsPending
	if proved == 0 {
		fmt.Fprintf(o, "Fixes proven by a failing test: none checked yet (%d waiting for a runner).\n", st.ProofsPending)
		return
	}
	fmt.Fprintf(o, "Fixes proven by a failing test: %d of %d checked (%.0f%%)", proven, proved, 100*float64(proven)/float64(proved))
	if st.ProofsPending > 0 {
		fmt.Fprintf(o, "; %d waiting", st.ProofsPending)
	}
	parts := make([]string, 0, len(st.Proofs))
	for _, p := range st.Proofs {
		name := p.Outcome
		if name == "" {
			name = "not checked"
		}
		parts = append(parts, fmt.Sprintf("%s %d of %d", name, p.Refixed, p.Fixes))
	}
	fmt.Fprintf(o, "\nFixed again later, by proof: %s.\n", strings.Join(parts, ", "))
}

func escapeListText(list []*core.Escape) string {
	if len(list) == 0 {
		return "No escapes recorded.\n"
	}
	var o strings.Builder
	for _, e := range list {
		cause := "cause unknown"
		if e.IntroMerge != "" {
			cause = "from " + shortCommit(e.IntroMerge)
			if e.IntroNumber > 0 {
				cause = fmt.Sprintf("from #%d", e.IntroNumber)
			}
			cause += fmt.Sprintf(" (%.2f %s)", e.Confidence, e.Method)
		}
		fmt.Fprintf(&o, "%s  %s  fixed by %s#%d  %s  lived %s  found: %s by %s", e.Key, e.FixedAt.Format("2006-01-02"), e.Repo, e.Number,
			cause, core.EscapeShortDays(e.DaysLived), e.Stage, e.Detector)
		if len(e.Tags) > 0 {
			fmt.Fprintf(&o, "  [%s]", strings.Join(e.Tags, " "))
		}
		o.WriteString("\n")
		fmt.Fprintf(&o, "  <untrusted>%s</untrusted>\n", oneLine(e.Title))
		if e.Note != "" {
			fmt.Fprintf(&o, "  note: <untrusted>%s</untrusted>\n", oneLine(e.Note))
		}
		if e.Proof != "" {
			fmt.Fprintf(&o, "  %s\n", core.ProofText(e.Proof, e.ProofFailed, e.ProofIntro, e.IntroCheck, e.ProofNote))
			if e.ProofNote != "" && e.Proof != "none" {
				// Test output: the repo's code wrote it.
				fmt.Fprintf(&o, "  proof note: <untrusted>%s</untrusted>\n", oneLine(e.ProofNote))
			}
		}
		for _, it := range e.Introducers {
			pr := "no pull request"
			if it.Number > 0 {
				pr = fmt.Sprintf("#%d", it.Number)
			}
			fmt.Fprintf(&o, "  %s  %s  %d lines  share %.2f  confidence %.2f  landed %s\n", shortCommit(it.Merge), pr, it.Lines, it.Share, it.Confidence, it.At.Format("2006-01-02"))
		}
	}
	return o.String()
}

func shortCommit(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// untrustedTag is the marker in any case or spacing ("</ UNTRUSTED >").
var untrustedTag = regexp.MustCompile(`(?i)<\s*/?\s*untrusted\s*>`)

// oneLine keeps outside text on one line and unable to open or close its
// own marker, however it spells or nests it ("</untru</untrusted>sted>").
func oneLine(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	for t := untrustedTag.ReplaceAllString(s, ""); t != s; t = untrustedTag.ReplaceAllString(s, "") {
		s = t
	}
	return strings.TrimSpace(s)
}
