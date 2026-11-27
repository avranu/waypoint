package core

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestBoilerplateStrip(t *testing.T) {
	bc := boilerCounter{}
	for n := 0; n < boilerMinDocs; n++ {
		t := newSimText(fmt.Sprintf("Check audit #%d: free review found points", n), fmt.Sprintf("topic%c words here", 'a'+n%26))
		bc.add(t.sh[0], t.sh[1])
	}
	odd := newSimText("Check audit #99: something else entirely", "")
	bc.add(odd.sh[0], odd.sh[1])
	strip := func(bp boilerplate, text string) string {
		toks := simTokens(text)
		return strings.Join(bp.strip(toks, shingles(toks)), " ")
	}
	bp := bc.boilerplate()
	if got := strip(bp, "Check audit #120: free review found points in the sanborn harvest"); got != "in the sanborn harvest" {
		t.Errorf("stripped = %q", got)
	}
	if got := strip(bp, "words here and there"); got != "words here and there" {
		t.Errorf("rare run stripped: %q", got)
	}
	bc.remove(odd.sh[0], odd.sh[1])
	if len(bc.boilerplate()) != len(bp) {
		t.Error("removing a document under the line changed the boilerplate")
	}
	first := newSimText("Check audit #0: free review found points", "")
	bc.remove(first.sh[0])
	if got := strip(bc.boilerplate(), "Check audit #120: free review found points"); got == "" {
		t.Error("the template still counts with one document too few")
	}
	var none boilerplate
	if got := none.strip([]string{"a", "b"}, nil); len(got) != 2 {
		t.Errorf("no boilerplate stripped %v", got)
	}
}

func suggestionKeys(r *Relations) []string {
	var out []string
	for _, s := range r.Suggestions {
		out = append(out, s.Key)
	}
	return out
}

func TestRelatedLeavesOutBoilerplate(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Audits", "")
	topics := []string{"login form styling", "billing invoice emails", "dark mode colours", "calendar export feed", "avatar upload sizes",
		"password reset tokens", "search ranking weights", "webhook retry policy", "runner cache keys", "markdown table rendering"}
	for n := 0; n < 40; n++ {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf("Filler %d about %s", n, topics[n%len(topics)]), Description: "Plain work on " + topics[n%len(topics)] + "."})
	}
	template := "Check audit #%d: Waypoint's own review of the pull request found these points worth a look before merging"
	for n := 0; n < boilerMinDocs; n++ {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf(template, n), Description: topics[n%len(topics)] + " follow-up."})
	}
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf(template, 101), Description: "Sanborn harvest stops at campus towns."})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf(template, 102), Description: "Payroll export rounding."})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sanborn harvest stops early", Description: "The harvest stops at campus towns."})
	r, err := e.s.SuggestRelations(e.ctx, e.admin, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got := suggestionKeys(r); !slices.Contains(got, c.Key) || slices.Contains(got, b.Key) {
		t.Errorf("suggestions for %s = %v; want %s (same harvest), not %s (only the template)", a.Key, got, c.Key, b.Key)
	}
}

// The index keeps template counts between rebuilds; after edits they must
// match counting from scratch.
func TestSimilarityIndexKeepsCounts(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Audits", "")
	var keys []string
	for n := 0; n < boilerMinDocs+1; n++ {
		keys = append(keys, e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf("Check audit #%d: the review found points worth a look", n), Description: fmt.Sprintf("Topic %c.", 'a'+n)}).Key)
	}
	idx, err := e.s.similarityIndex(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.boiler) == 0 {
		t.Fatal("the template isn't boilerplate")
	}
	title := "Sanborn harvest stops early"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, keys[0], IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE issues SET draft = 1, updated_at = updated_at + 1 WHERE id = (SELECT max(id) FROM issues)`); err != nil {
		t.Fatal(err)
	}
	if idx, err = e.s.similarityIndex(e.ctx); err != nil {
		t.Fatal(err)
	}
	fresh := boilerCounter{}
	for _, st := range e.s.similar.texts {
		fresh.add(st.sh[0], st.sh[1])
	}
	if len(e.s.similar.texts) != boilerMinDocs || !maps.Equal(fresh, e.s.similar.boiler) {
		t.Errorf("kept %d texts; counts kept %d, from scratch %d", len(e.s.similar.texts), len(e.s.similar.boiler), len(fresh))
	}
	if len(idx.boiler) != 0 {
		t.Errorf("%d issues on a template still make it boilerplate", boilerMinDocs-1)
	}
}

// conceptCorpus files issues in two topics, ops and billing, where
// deploy/rollout, release/version and server/host are written in the same
// contexts but rarely together. They are all done.
func conceptCorpus(e *env, project string) {
	pairs := [][2]string{{"deploy", "rollout"}, {"release", "version"}, {"server", "host"}}
	ops := []string{"staging", "production", "cluster", "pipeline", "outage"}
	billing := []string{"invoice", "payment", "customer", "receipt", "refund", "billing"}
	for n := 0; n < 40; n++ {
		var words []string
		for k, p := range pairs {
			words = append(words, p[(n>>k)&1])
		}
		words = append(words, ops[n%len(ops)], ops[(n+2)%len(ops)])
		tail := "."
		if n%3 == 0 {
			tail = " (canary)."
		}
		for _, in := range []IssueInput{
			{Project: project, Title: strings.Join(words, " "), Description: "Ops work: " + strings.Join(words, ", ") + tail},
			{Project: project, Title: billing[n%6] + " " + billing[(n+1)%6] + " " + billing[(n+3)%6], Description: "Billing work on the " + billing[(n+2)%6] + " and " + billing[(n+4)%6] + tail},
		} {
			done := CatDone
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, e.issue(e.admin, in).Key, IssuePatch{Status: &done}); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

func TestRelatedByConcept(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Ops", "")
	conceptCorpus(e, p.Key)
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Deploy the release to the server canary"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Rollout of the version to a host canary"})
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Invoice refund for the customer canary"})
	r, err := e.s.SuggestRelations(e.ctx, e.admin, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got := suggestionKeys(r); slices.Contains(got, b.Key) {
		t.Fatalf("on words alone %s already relates to %s: %v; the test needs pairs that share few words", a.Key, b.Key, r.Suggestions)
	}
	if m, _ := e.s.concepts(); m != nil {
		t.Fatal("a small corpus trained a model on its own")
	}
	if err := e.s.trainConcepts(e.ctx, "test", 10); err != nil {
		t.Fatal(err)
	}
	if m, gen := e.s.concepts(); m == nil || gen != 1 {
		t.Fatalf("model %v gen %d", m, gen)
	}
	r, err = e.s.SuggestRelations(e.ctx, e.admin, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	var got *RelationSuggestion
	for n := range r.Suggestions {
		if r.Suggestions[n].Key == b.Key {
			got = &r.Suggestions[n]
		}
		if r.Suggestions[n].Key == d.Key {
			t.Errorf("%s (billing) suggested for %s: %+v", d.Key, a.Key, r.Suggestions[n])
		}
	}
	if got == nil || got.Confidence != "low" || !strings.HasPrefix(got.Reason, "About the same things; shares canary") {
		t.Errorf("suggestion for %s = %+v in %+v", b.Key, got, r.Suggestions)
	}
}

func TestGlossarySenseGate(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Auth", "")
	browser := []string{"browser", "cookie", "login", "logout", "password", "signin"}
	agent := []string{"agent", "token", "prompt", "transcript", "model", "quota"}
	for n := 0; n < 30; n++ {
		for _, words := range [][]string{browser, agent} {
			w := []string{words[n%6], words[(n+1)%6], words[(n+3)%6]}
			if n%3 == 0 {
				w = append(w, "session")
			}
			e.issue(e.admin, IssueInput{Project: p.Key, Title: strings.Join(w, " "), Description: "Work on the " + strings.Join(w, " and ") + "."})
		}
	}
	term, def := "Session", "A person's browser sign-in, kept in a cookie after login until logout or the password changes."
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Definition: &def, Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	links := func(desc string) bool {
		for _, end := range e.s.mentionTargets(e.ctx, e.s.db.R, p.ID, 0, "", desc, "") {
			if end == (autoEnd{KGlossary, g.ID}) {
				return true
			}
		}
		return false
	}
	agentUse := "The agent session ran out of token quota while the prompt transcript grew."
	browserUse := "The browser session cookie expired after logout, so login failed."
	if !links(agentUse) || !links(browserUse) {
		t.Fatal("without a model every use should link, as before")
	}
	if err := e.s.trainConcepts(e.ctx, "test", 10); err != nil {
		t.Fatal(err)
	}
	if links(agentUse) {
		t.Error("an agent's session linked the browser sign-in entry")
	}
	if !links(browserUse) {
		t.Error("a browser session no longer links its entry")
	}
	idx, err := e.s.glossaryWords(e.ctx, e.s.db.R)
	if err != nil {
		t.Fatal(err)
	}
	if hits := idx.find(agentUse, []int64{p.ID}, e.s.senseGate(idx)); len(hits) != 0 {
		t.Errorf("find with the gate = %+v", hits)
	}
	refs := e.s.termRefs(e.ctx, e.admin, browserUse+" "+agentUse, refScope{id: p.ID})
	if len(refs) != 1 || refs[0].Match != "Session" {
		t.Errorf("term refs = %+v", refs)
	}
}
