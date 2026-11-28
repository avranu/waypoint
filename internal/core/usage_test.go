package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// usageEnv is a project with a glossary entry (alias, word to avoid), a
// decision page and a design note.
type usageEnv struct {
	*env
	p      *Project
	claim  *GlossaryEntry
	page   *Page
	note   *DesignNote
	agent  *Actor
	person *Actor
}

func newUsageEnv(t *testing.T) *usageEnv {
	e := newEnv(t)
	u := &usageEnv{env: e, p: e.project(e.admin, "Pilot", "")}
	term, def := "Claim", "An agent's lease on a task."
	aliases, avoid := []string{"lease"}, []string{"ticket"}
	var err error
	if u.claim, err = e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Definition: &def, Project: &u.p.Key, Aliases: &aliases, Avoid: &avoid}); err != nil {
		t.Fatal(err)
	}
	if u.page, err = e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Use SQLite"), Category: str("decision"), Project: Some(u.p.Key)}); err != nil {
		t.Fatal(err)
	}
	if u.note, err = e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Agent queue"), Kind: str("feature"), Project: &u.p.Key}); err != nil {
		t.Fatal(err)
	}
	u.agent = agentActor(e.admin, "anthropic/claude-code")
	u.person = &Actor{User: e.admin.User, SessionHash: "browser"}
	return u
}

func (u *usageEnv) usage(a *Actor, typ string, id int64) *ItemUsage {
	u.t.Helper()
	us, err := u.s.ItemUsage(u.ctx, a, typ, id)
	if err != nil {
		u.t.Fatalf("usage of %s %d: %v", typ, id, err)
	}
	return us
}

func TestKnowledgeLookups(t *testing.T) {
	u := newUsageEnv(t)
	look := func(a *Actor, term string) {
		res, err := u.s.LookupGlossary(u.ctx, a, term, u.p.Key)
		if err != nil {
			t.Fatal(err)
		}
		u.s.NoteGlossaryLookup(u.ctx, a, res)
	}
	look(u.agent, "claim")
	look(u.agent, "Claim")
	look(u.person, "claim")
	if c := u.usage(u.admin, KGlossary, u.claim.ID).Counts; c.Lookups != 1 || c.Agents != 1 {
		t.Fatalf("counts after repeats = %+v", c)
	}
	u.setClock(u.clockNow().Add(11 * time.Minute))
	look(u.agent, "lease")
	look(u.agent, "ticket")
	us := u.usage(u.admin, KGlossary, u.claim.ID)
	if us.Counts.Lookups != 2 || len(us.Uses) != 2 || us.Uses[0].Word != "lease" || us.Uses[0].Agent != "anthropic/claude-code" {
		t.Fatalf("usage = %+v", us)
	}
	u.setClock(u.clockNow().Add(11 * time.Minute))
	look(u.agent, "ticket")
	if us := u.usage(u.admin, KGlossary, u.claim.ID); us.Uses[0].Word != "ticket" || us.LastUsedAt == nil {
		t.Errorf("avoided-word lookup = %+v", us.Uses[0])
	}
	u.s.NoteLookup(u.ctx, u.agent, KPage, u.page.ID, "")
	u.s.NoteLookup(u.ctx, u.agent, KDesign, u.note.ID, "")
	u.s.NoteLookup(u.ctx, u.person, KPage, u.page.ID, "")
	if c := u.usage(u.admin, KPage, u.page.ID).Counts; c.Lookups != 1 {
		t.Errorf("page lookups = %+v", c)
	}
	if c := u.usage(u.admin, KDesign, u.note.ID).Counts; c.Lookups != 1 {
		t.Errorf("note lookups = %+v", c)
	}
	bob := u.user("bob", false)
	if _, err := u.s.ItemUsage(u.ctx, bob, KGlossary, u.claim.ID); err == nil {
		t.Error("bob read the usage of a project he isn't in")
	}
	if _, err := u.s.ItemUsage(u.ctx, u.admin, KIssue, 1); !IsInvalid(err) {
		t.Errorf("issue usage = %v", err)
	}
}

func TestKnowledgeMentions(t *testing.T) {
	u := newUsageEnv(t)
	code := u.p.Key + "-" + u.page.Code
	i := u.issue(u.admin, IssueInput{Project: u.p.Key, Title: "Claims expire",
		Description: "The lease is per " + code + ", not a ticket.\n\n```\nticket := claim()\n```"})
	us := u.usage(u.admin, KGlossary, u.claim.ID)
	if us.Counts.Mentions != 1 || us.Counts.Avoided != 1 || us.Counts.People != 1 {
		t.Fatalf("counts = %+v", us.Counts)
	}
	if len(us.Uses) != 2 || us.Uses[0].Source == nil || us.Uses[0].Source.Key != i.Key {
		t.Fatalf("uses = %+v", us.Uses)
	}
	var words []string
	for _, x := range us.Uses {
		words = append(words, x.Kind+":"+x.Word)
	}
	if got := strings.Join(words, " "); !strings.Contains(got, "avoided:ticket") || !strings.Contains(got, "mention:") {
		t.Errorf("words = %s", got)
	}
	if c := u.usage(u.admin, KPage, u.page.ID); c.Counts.Mentions != 1 || c.Uses[0].Word != code {
		t.Errorf("page by code = %+v", c)
	}
	if _, err := u.s.UpdateIssue(u.ctx, u.admin, i.Key, IssuePatch{Description: str("The lease, again, and a ticket.")}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.s.AddComment(u.ctx, u.agent, i.Key, "Holding the [[Claim]] until done; also see [[Agent queue]]."); err != nil {
		t.Fatal(err)
	}
	us = u.usage(u.admin, KGlossary, u.claim.ID)
	if us.Counts.Mentions != 2 || us.Counts.Avoided != 1 || us.Counts.Agents != 1 {
		t.Fatalf("after edit and comment = %+v", us.Counts)
	}
	if !us.Uses[0].Comment || us.Uses[0].Agent != "anthropic/claude-code" {
		t.Errorf("comment use = %+v", us.Uses[0])
	}
	if c := u.usage(u.admin, KDesign, u.note.ID).Counts; c.Mentions != 1 {
		t.Errorf("note mentions = %+v", c)
	}
	if _, err := u.s.UpdateGlossary(u.ctx, u.admin, u.claim.ID, GlossaryInput{Definition: str("A Claim is a lease. Not a ticket.")}); err != nil {
		t.Fatal(err)
	}
	term, def := "Lease time", "How long a claim lasts."
	if _, err := u.s.CreateGlossary(u.ctx, u.admin, GlossaryInput{Term: &term, Definition: &def, Project: &u.p.Key}); err != nil {
		t.Fatal(err)
	}
	us = u.usage(u.admin, KGlossary, u.claim.ID)
	if us.Counts.Mentions != 3 || us.Counts.Avoided != 1 {
		t.Errorf("after definitions = %+v", us.Counts)
	}
	other := u.project(u.admin, "Other", "")
	oterm, odef := "Widget", "A thing."
	widget, err := u.s.CreateGlossary(u.ctx, u.admin, GlossaryInput{Term: &oterm, Definition: &odef, Project: &other.Key})
	if err != nil {
		t.Fatal(err)
	}
	u.issue(u.admin, IssueInput{Project: u.p.Key, Title: "A widget"})
	if c := u.usage(u.admin, KGlossary, widget.ID).Counts; c.Mentions != 0 {
		t.Errorf("out-of-scope widget mentions = %+v", c)
	}
	list, err := u.s.ListGlossary(u.ctx, u.admin, GlossaryQuery{Project: u.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range list {
		if g.ID == u.claim.ID && (g.Uses != 4 || g.LastUsedAt == nil) {
			t.Errorf("claim in list: uses %d, last %v", g.Uses, g.LastUsedAt)
		}
	}
}

func TestKnowledgeOverview(t *testing.T) {
	u := newUsageEnv(t)
	u.setClock(u.clockNow().Add(-60 * 24 * time.Hour))
	term, def := "Burndown", "Work left over time."
	old, err := u.s.CreateGlossary(u.ctx, u.admin, GlossaryInput{Term: &term, Definition: &def, Project: &u.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	u.setClock(u.clockNow().Add(60 * 24 * time.Hour))
	u.issue(u.agent, IssueInput{Project: u.p.Key, Title: "Close the ticket when merged"})
	u.issue(u.admin, IssueInput{Project: u.p.Key, Title: "Claim rules"})
	res, _ := u.s.LookupGlossary(u.ctx, u.agent, "claim", u.p.Key)
	u.s.NoteGlossaryLookup(u.ctx, u.agent, res)
	u.s.NoteLookup(u.ctx, u.agent, KPage, u.page.ID, "")

	ov, err := u.s.KnowledgeOverview(u.ctx, u.admin, KnowledgeQuery{Project: u.p.Key, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Lookups != 2 || ov.Totals.Mentions != 1 || ov.Totals.Avoided != 1 {
		t.Errorf("totals = %+v", ov.Totals)
	}
	if len(ov.Top) == 0 || ov.Top[0].Item.ID != u.claim.ID || ov.Top[0].Lookups != 1 || ov.Top[0].Mentions != 1 || ov.Top[0].Avoided != 1 {
		t.Errorf("top = %+v", ov.Top)
	}
	if len(ov.Avoided) != 1 || ov.Avoided[0].Word != "ticket" || ov.Avoided[0].Latest.Agent != "anthropic/claude-code" || ov.Avoided[0].Latest.Source == nil {
		t.Errorf("avoided = %+v", ov.Avoided)
	}
	if len(ov.Lookups) != 2 || ov.Lookups[0].Item == nil {
		t.Errorf("lookups = %+v", ov.Lookups)
	}
	found := false
	for _, x := range ov.Unused {
		found = found || (x.Item.Type == KGlossary && x.Item.ID == old.ID && x.LastUsedAt == nil)
	}
	if !found {
		t.Errorf("unused = %+v", ov.Unused)
	}
	ov, _ = u.s.KnowledgeOverview(u.ctx, u.admin, KnowledgeQuery{Agent: "claude", Days: 30})
	if ov.Totals.Mentions != 0 || ov.Totals.Avoided != 1 || ov.Totals.Lookups != 2 {
		t.Errorf("claude's = %+v", ov.Totals)
	}
	ov, _ = u.s.KnowledgeOverview(u.ctx, u.admin, KnowledgeQuery{Agent: "codex", Days: 30})
	if ov.Totals.ItemsUsed != 0 || len(ov.Top) != 0 {
		t.Errorf("codex's = %+v", ov.Totals)
	}
	bob := u.user("bob", false)
	ov, err = u.s.KnowledgeOverview(u.ctx, bob, KnowledgeQuery{Days: 30})
	if err != nil || ov.Totals.ItemsUsed != 0 || len(ov.Unused) != 0 {
		t.Errorf("bob's = %+v, %v", ov, err)
	}
}

func TestKnowledgeBackfillAndPRs(t *testing.T) {
	u := newUsageEnv(t)
	i := u.issue(u.admin, IssueInput{Project: u.p.Key, Title: "Expire each claim"})
	if _, err := u.s.db.W.ExecContext(u.ctx, `DELETE FROM knowledge_uses`); err != nil {
		t.Fatal(err)
	}
	u.setClock(u.clockNow().Add(time.Hour))
	u.s.BackfillKnowledgeUses(u.ctx)
	us := u.usage(u.admin, KGlossary, u.claim.ID)
	if us.Counts.Mentions != 1 || !us.Uses[0].At.Equal(i.CreatedAt) {
		t.Errorf("backfilled = %+v (issue created %v)", us.Uses, i.CreatedAt)
	}
	if _, err := u.s.db.W.ExecContext(u.ctx, `DELETE FROM knowledge_uses`); err != nil {
		t.Fatal(err)
	}
	u.s.BackfillKnowledgeUses(u.ctx)
	if c := u.usage(u.admin, KGlossary, u.claim.ID).Counts; c.Mentions != 0 {
		t.Errorf("second backfill = %+v", c)
	}
	f, err := u.s.CreateForge(u.ctx, u.admin, ForgeInput{Name: str("Forgejo"), Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.s.db.W.ExecContext(u.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'robin/pilot', 0)`, u.p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	pr, err := u.s.recordPR(u.ctx, f, "robin/pilot", &ForgePR{Number: 7, Title: "Implement " + u.p.Key + "-" + u.page.Code,
		Body: "Per the decision.", URL: "https://git.example.com/robin/pilot/pulls/7", State: "open", Author: "robin", HeadSHA: "abc1234"}, false)
	if err != nil {
		t.Fatal(err)
	}
	us = u.usage(u.admin, KPage, u.page.ID)
	if us.Counts.Mentions != 1 || us.Uses[0].Source == nil || us.Uses[0].Source.Ref != pr.URL {
		t.Errorf("pull request mention = %+v", us.Uses)
	}
}

// Recording mentions must not slow saving text down: hundreds of terms
// against a long description.
func TestMentionScanIsFast(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Fast", "")
	for n := range 400 {
		term := fmt.Sprintf("Term%03d thing", n)
		aliases, avoid := []string{fmt.Sprintf("alias%03d", n)}, []string{fmt.Sprintf("avoid%03d", n)}
		if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Project: &p.Key, Aliases: &aliases, Avoid: &avoid}); err != nil {
			t.Fatal(err)
		}
	}
	text := strings.Repeat("Some words about term017 things and alias200 with avoid399, in a long description. ", 120)
	idx, err := e.s.glossaryWords(e.ctx, e.s.db.R)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	hits := idx.find(text, []int64{p.ID}, nil)
	if len(hits) != 3 {
		t.Errorf("hits = %+v", hits)
	}
	if d := time.Since(start); !raceEnabled && d > 50*time.Millisecond {
		t.Errorf("scanning 10 KB against 1,200 words took %v", d)
	}
	start = time.Now()
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Long", Description: text})
	t.Logf("creating the issue with mentions took %v", time.Since(start))
}
