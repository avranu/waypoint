package core

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/semantic"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Related issues come from the issues' own words, not a model (WP-296):
// tf-idf cosine similarity over titles and descriptions, and dependencies
// the text states ("blocked by WP-12"). The similarity scorer is meant to
// be shared, by the new-issue hints (WP-104) and nightly proposals
// (WP-105) as well.

// Similarity bands, from an offline check over 1,410 issues in October
// 2026: every open pair at simHigh or more was related, most between
// simMedium and simHigh were, and many between simLow and simMedium.
const (
	simHigh      = 0.5
	simMedium    = 0.4
	simLow       = 0.3
	simDuplicate = 0.8 // in one project: the same work twice
)

// With a concept model (WP-779, WP-ADR91) issues that share fewer words
// relate when they are about the same things: the word cosine plus
// simBlendWeight times the concept cosine reaches simBlend, and they
// share some words (simFloor). On 3,115 issues, pairs in that band were
// judged related 78% of the time, and person-made relations found rose
// from 6% to 16% for about three suggestions an issue.
const (
	simBlendWeight = 0.5
	simBlend       = 0.6
	simFloor       = 0.1
)

// maxRelationSuggestions is the most suggestions given for one issue.
const maxRelationSuggestions = 8

// RelationSuggestion is a proposed link between issues.
type RelationSuggestion struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Kind       string `json:"kind"` // relates, blocks, blocked_by, duplicates
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`

	id     int64  // the other issue's
	source string // where it came from, as relation_suggestions records it
}

// Relations is a list of suggested links for an issue.
type Relations struct {
	Suggestions []RelationSuggestion `json:"suggestions"`
}

// SuggestRelations proposes links from an issue to open issues the actor
// can see: ones its text says it waits for or holds up, ones with the same
// title in its project, and ones about the same things. Nothing is linked.
func (s *Service) SuggestRelations(ctx context.Context, actor *Actor, ref string) (*Relations, error) {
	return s.suggestRelations(ctx, actor, ref, false)
}

// suggestRelations suggests links, with ownProject only to issues in the
// issue's own project.
func (s *Service) suggestRelations(ctx context.Context, actor *Actor, ref string, ownProject bool) (*Relations, error) {
	d, err := s.GetIssue(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	i := d.Issue
	skip := map[int64]bool{i.ID: true}
	named := map[int64]bool{}
	for _, r := range d.Relations {
		skip[r.Issue.ID] = true
		named[r.Issue.ID] = r.Named
	}
	if d.Parent != nil {
		skip[d.Parent.ID] = true
	}
	for _, c := range d.Children {
		skip[c.ID] = true
	}
	idx, err := s.similarityIndex(ctx)
	if err != nil {
		return nil, err
	}
	// A pair a person said isn't related stays unsuggested (WP-958).
	dismissed, err := s.dismissedRelations(ctx, i.ID)
	if err != nil {
		return nil, err
	}
	out := &Relations{Suggestions: []RelationSuggestion{}}
	usable := func(id int64) bool {
		c := idx.docs[id]
		return c != nil && !skip[id] && !dismissed[id] && c.open && (!ownProject || c.projectID == i.ProjectID)
	}
	add := func(id int64, kind, confidence, reason, source string) {
		if !usable(id) || !s.canSee(ctx, s.db.R, actor, id, idx.docs[id].projectID) {
			return
		}
		ci, err := s.issueByID(ctx, s.db.R, id, false)
		if err != nil {
			return
		}
		skip[id] = true
		out.Suggestions = append(out.Suggestions, RelationSuggestion{Key: ci.Key, Title: ci.Title, Kind: kind, Confidence: confidence,
			Reason: excerpt(reason, 300), id: id, source: source})
	}
	for _, dep := range statedDependencies(i.Title + "\n" + i.Description) {
		if id, err := s.resolveIssueID(ctx, s.db.R, dep.key); err == nil {
			if named[id] {
				delete(skip, id) // the plain link from its text can still become what the text states
			}
			confidence := "medium"
			if dep.sure {
				confidence = "high"
			}
			add(id, dep.kind, confidence, dep.sentence, sugText)
		}
	}
	for _, c := range s.forgeCoMentions(ctx, i.ID) {
		add(c.id, c.kind, c.confidence, c.reason, sugForge)
	}
	for _, id := range idx.sameTitle(i.ProjectID, normalTitle(i.Title)) {
		add(id, "duplicates", "high", "The same title.", sugTitle)
	}
	src, concept := idx.vector(i.Title, i.Description), idx.meaning(i.Title, i.Description)
	matches := idx.related(src, concept, i.ID)
	// A ranker learned from people's decisions, while it beats the blend
	// on the month it didn't see, orders the blend's best and drops what
	// it judges below its line (WP-958).
	var probs map[int64]float64
	if r := s.relationRanker(ctx); r != nil {
		var pool []simMatch
		for _, m := range matches {
			if len(pool) == rankerPool {
				break
			}
			if usable(m.id) && s.canSee(ctx, s.db.R, actor, m.id, idx.docs[m.id].projectID) {
				pool = append(pool, m)
			}
		}
		if ranked, p, err := s.rankMatches(ctx, r, idx, i, src, concept, pool); err != nil {
			s.log.WarnContext(ctx, "relation ranker", "issue", i.Key, "err", err)
		} else {
			matches, probs = ranked, p
		}
	}
	for _, m := range matches {
		if len(out.Suggestions) >= maxRelationSuggestions {
			break
		}
		c := idx.docs[m.id]
		switch {
		case c.projectID == i.ProjectID && m.score >= simDuplicate:
			add(m.id, "duplicates", "high", "Nearly the same text: "+idx.shared(src, c.vec), sugWords)
		case m.score >= simLow:
			add(m.id, "relates", simConfidence(m.score), "Shares "+idx.shared(src, c.vec), sugWords)
		default:
			add(m.id, "relates", "low", "About the same things; shares "+idx.shared(src, c.vec), sugConcepts)
		}
	}
	if len(out.Suggestions) > maxRelationSuggestions {
		out.Suggestions = out.Suggestions[:maxRelationSuggestions]
	}
	// What a person is shown is recorded, to learn from what they do
	// with it (WP-958): only a person who could add or dismiss it, so a
	// look without a choice doesn't count as ignoring it; a read-only
	// token's look writes nothing.
	if actor.actsAsPerson() && !actor.ReadOnly && len(out.Suggestions) > 0 {
		if role, err := s.issueRole(ctx, s.db.R, actor, i); err == nil && role >= RoleEditor {
			s.noteShown(ctx, actor, idx, i, src, concept, out.Suggestions, probs)
		}
	}
	return out, nil
}

func simConfidence(score float64) string {
	switch {
	case score >= simHigh:
		return "high"
	case score >= simMedium:
		return "medium"
	}
	return "low"
}

// autoRelate links a new issue to what SuggestRelations is sure about in
// its own project, when the project asks for it. It acts for no person, so
// it stays out of other projects, which the issue's author may not see.
func (s *Service) autoRelate(ctx context.Context, e *Event) {
	p, err := s.projectByID(ctx, s.db.R, e.ProjectID)
	if err != nil || !p.Settings.AutoRelate {
		return
	}
	// Bounded like estimates, so a burst of new issues can't queue up
	// unbounded index rebuilds.
	s.aiMu.Lock()
	full := s.relating >= maxQueuedEstimates
	if !full {
		s.relating++
	}
	s.aiMu.Unlock()
	if full {
		s.log.Info("auto-relate skipped: queue full", "issue", e.IssueKey)
		return
	}
	key, projectID := e.IssueKey, e.ProjectID
	s.goBG(func() {
		defer func() {
			s.aiMu.Lock()
			s.relating--
			s.aiMu.Unlock()
		}()
		by := aiActorNamed("Auto relate", projectID, "")
		res, err := s.suggestRelations(s.bgCtx, by, key, true)
		if err != nil {
			s.log.Info("auto-relate skipped", "issue", key, "err", err)
			return
		}
		for _, r := range res.Suggestions {
			if r.Confidence == "high" {
				if err := s.AddRelation(s.bgCtx, by, key, r.Kind, r.Key); err != nil {
					s.log.Info("auto-relate link failed", "issue", key, "err", err)
				}
			}
		}
	})
}

// ---- issues named in the text ----

// namedLinksMax caps the links one issue's text makes, so a pasted changelog
// doesn't relate (and notify about) everything it lists. Keys past it stay
// mentions.
const namedLinksMax = 20

// noteNamedRelations relates an issue to the issues its title or description
// newly names (WP-781), in the same write so the saved issue comes back with
// them. before is the text as it stood ("" for a new issue). A problem is
// logged, never fails the save.
func (s *Service) noteNamedRelations(ctx context.Context, t *tx, actor *Actor, issueID int64, before string) {
	if err := s.syncNamedRelations(ctx, t, actor, issueID, before); err != nil {
		s.log.WarnContext(ctx, "named relations", "issue", issueID, "err", err)
	}
}

// syncNamedRelations links only what this save newly names, so a relation
// someone removed stays gone through later saves of the same text. Links the
// text made go when neither issue's text names the other any more (a key half
// typed when the description autosaved); relations someone chose never do.
// Only text a person wrote links: agents name tasks in passing, and the
// mentions list has those (WP-ADR81).
func (s *Service) syncNamedRelations(ctx context.Context, t *tx, actor *Actor, issueID int64, before string) error {
	i, err := s.issueByID(ctx, t, issueID, true)
	if err != nil {
		return err
	}
	had := map[string]bool{}
	for _, k := range ScanMentions(before).Keys {
		had[k] = true
	}
	named := map[int64]bool{}
	var fresh []int64
	for _, k := range ScanMentions(i.Title + "\n" + i.Description).Keys {
		id, err := s.resolveIssueID(ctx, t, k)
		if err != nil || id == i.ID || named[id] {
			continue
		}
		named[id] = true
		if !had[k] {
			fresh = append(fresh, id)
		}
	}
	if err := s.dropUnnamedRelations(ctx, t, i, named); err != nil {
		return err
	}
	if !writesOwnText(actor) || len(fresh) == 0 {
		return nil
	}
	var made int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM issue_relations WHERE named_in = ?`, i.ID).Scan(&made); err != nil {
		return err
	}
	for _, id := range fresh {
		if made >= namedLinksMax {
			break
		}
		o, err := s.issueByID(ctx, t, id, false)
		if err != nil {
			return err
		}
		if !s.canSee(ctx, t, actor, o.ID, o.ProjectID) || (i.ParentID != nil && *i.ParentID == o.ID) || (o.ParentID != nil && *o.ParentID == i.ID) {
			continue
		}
		var linked bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_relations WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`,
			i.ID, o.ID, o.ID, i.ID).Scan(&linked); err != nil {
			return err
		}
		if linked {
			continue
		}
		if _, err := s.relate(ctx, t, actor, i, o, "relates", "", &i.ID); err != nil {
			return err
		}
		made++
	}
	return nil
}

// writesOwnText: the words being saved are the person's own, typed in the
// web or their terminal. A system write or a person's account carrying an
// agent's or a forge's text isn't.
func writesOwnText(a *Actor) bool {
	return a.actsAsPerson() && a.User.ID != 0 && createdByPerson(a)
}

// textNames: i's title or description names the issue id.
func (s *Service) textNames(ctx context.Context, t *tx, i *Issue, id int64) bool {
	for _, k := range ScanMentions(i.Title + "\n" + i.Description).Keys {
		if got, err := s.resolveIssueID(ctx, t, k); err == nil && got == id {
			return true
		}
	}
	return false
}

// dropUnnamedRelations removes the links i's text made to issues it no
// longer names. One the other issue's text still names is that issue's now.
func (s *Service) dropUnnamedRelations(ctx context.Context, t *tx, i *Issue, named map[int64]bool) error {
	rows, err := t.QueryContext(ctx, `SELECT id, CASE source_id WHEN ? THEN target_id ELSE source_id END FROM issue_relations WHERE named_in = ?`, i.ID, i.ID)
	if err != nil {
		return err
	}
	type link struct{ id, other int64 }
	var stale []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.id, &l.other); err != nil {
			rows.Close()
			return err
		}
		if !named[l.other] {
			stale = append(stale, l)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range stale {
		o, err := s.issueByID(ctx, t, l.other, true)
		if err != nil {
			return err
		}
		if s.textNames(ctx, t, o, i.ID) {
			if _, err := t.ExecContext(ctx, `UPDATE issue_relations SET named_in = ? WHERE id = ?`, o.ID, l.id); err != nil {
				return err
			}
			continue
		}
		holder := i
		if o.Draft && !i.Draft {
			holder = o // a draft's key stays out of the published issue's history (WP-42)
		}
		old, data, err := s.relationGone(ctx, t, holder, l.id, "automatic", "the text no longer names the other issue")
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_relations WHERE id = ?`, l.id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: holder.ProjectID, IssueID: holder.ID, IssueKey: holder.Key, Field: "relation", Old: old, Data: data})
	}
	return nil
}

// ---- forge co-mentions ----

type coMention struct {
	id                       int64
	kind, confidence, reason string
}

// forgeCoMentions are issues named by the same pull request, commit or
// forge issue as this one (WP-379). When the item is this issue's own work
// and its text says it depends on or blocks the other, that's a stated
// dependency (high); otherwise working on the same thing is a hint
// (medium, never linked by itself). Items naming many issues (a release,
// a sweep) say little and are left out.
func (s *Service) forgeCoMentions(ctx context.Context, issueID int64) []coMention {
	rows, err := s.db.R.QueryContext(ctx, `SELECT o.issue_id, max(me.own), group_concat(DISTINCT o.verb), coalesce(nullif(me.title, ''), me.repo), coalesce(me.number, 0), me.item_kind
		FROM forge_mentions me
		JOIN forge_mentions o ON o.item_url = me.item_url AND o.issue_id != me.issue_id AND o.gone_at IS NULL
		WHERE me.issue_id = ? AND me.gone_at IS NULL AND NOT `+unapprovedOutsidePullSQL("me.item_url")+`
		AND (SELECT count(DISTINCT x.issue_id) FROM forge_mentions x WHERE x.item_url = me.item_url AND x.gone_at IS NULL) <= ?
		GROUP BY o.issue_id, me.item_url ORDER BY max(me.updated_at) DESC LIMIT 20`, issueID, maxCoMentioned)
	if err != nil {
		s.log.Info("forge co-mentions", "err", err)
		return nil
	}
	defer rows.Close()
	var out []coMention
	for rows.Next() {
		var c coMention
		var own int
		var verbs, title, kind string
		var number int64
		if err := rows.Scan(&c.id, &own, &verbs, &title, &number, &kind); err != nil {
			return out
		}
		what := title
		if number > 0 {
			what = fmt.Sprintf("#%d %s", number, title)
		}
		noun := map[string]string{"pull": "pull request", "commit": "commit", "issue": "forge issue"}[kind]
		c.kind, c.confidence = "relates", "medium"
		c.reason = "Both named by the same " + noun + ": " + what
		if own == 1 {
			switch vs := strings.Split(verbs, ","); {
			case slices.Contains(vs, "depends_on"):
				c.kind, c.confidence, c.reason = "blocked_by", "high", "Its "+noun+" says it depends on it: "+what
			case slices.Contains(vs, "blocks"):
				c.kind, c.confidence, c.reason = "blocks", "high", "Its "+noun+" says it blocks it: "+what
			case slices.Contains(vs, "duplicates"):
				c.kind, c.confidence, c.reason = "duplicates", "medium", "Its "+noun+" says it duplicates it: "+what
			}
		}
		out = append(out, c)
	}
	return out
}

// maxCoMentioned: an item naming more issues than this doesn't relate them.
const maxCoMentioned = 6

// ---- stated dependencies ----

// statedDep is a dependency the text states outright. sure is false where
// the wording often means something else ("after WP-12 shipped we saw…") or
// the kind needs judgement (tension, supersedes): those are only suggested,
// never linked by autoRelate.
type statedDep struct {
	key, kind, sentence string
	sure                bool
}

var (
	depKey = `(?:\[\[)?([A-Z][A-Z0-9]{1,9}-\d{1,9})\b`
	// waitsForRE: the issue waits for the key.
	waitsForRE = lazyre.New(`(?i:\b(?:blocked by|depends on|dependent on|after|needs|requires|waits? (?:for|on)|waiting (?:for|on))\s+)` + depKey)
	// keyFirstRE: "WP-12 first", "WP-12 must land first".
	keyFirstRE = lazyre.New(depKey + `(?:\]\])?(?i:\s+(?:(?:must|has to|should) (?:land|ship|be done|merge) )?first)\b`)
	// holdsUpRE: the key waits for the issue.
	holdsUpRE = lazyre.New(`(?i:\b(?:blocks|unblocks|before|needed by|required by)\s+)` + depKey)
	// tensionRE: the two pull against each other (WP-1036).
	tensionRE = lazyre.New(`(?i:\b(?:in tension with|at odds with|conflicts? with|contradicts|works? against|pulls? against)\s+)` + depKey)
	// supersededByRE: the key replaces the issue; supersedesRE: the issue
	// replaces the key.
	supersededByRE = lazyre.New(`(?i:\b(?:superseded by|replaced by|obsoleted by)\s+)` + depKey)
	supersedesRE   = lazyre.New(`(?i:\b(?:supersedes|replaces|obsoletes)\s+)` + depKey)
	// negatedRE: words earlier in the sentence deny it ("not blocked by",
	// "I don't think it's at odds with").
	negatedRE = lazyre.New(`(?i)\b(?:not|no longer|never|isn'?t|aren'?t|doesn'?t|don'?t|wasn'?t|won'?t)\b`)
	// relativeRE: a clause about something else ("WP-3, which replaces WP-4").
	relativeRE = lazyre.New(`(?i)\b(?:which|that|who)\s+$`)
	// looseRE: dependency wording that is often only about time or need.
	looseRE = lazyre.New(`(?i)^(?:after|needs|requires|before)\b`)
)

// statedDependencies finds the relations prose states outright, once per
// key, with the sentence that says it: "blocked by WP-12", "after WP-12",
// "needs WP-12", "WP-12 first" (blocked_by); "blocks WP-12", "before WP-12"
// (blocks); "in tension with WP-12", "conflicts with WP-12" (tension);
// "replaces WP-12" (supersedes), "replaced by WP-12" (superseded_by). A
// sentence that asks, or denies earlier on ("not blocked by", "I don't think
// it's at odds with"), states nothing, nor does a clause about another
// issue ("WP-3, which replaces WP-4"). These are word patterns, not a reading
// of the sentence: what they find is a suggestion a person confirms, and
// autoRelate links only the unambiguous dependency wording (sure).
func statedDependencies(text string) []statedDep {
	text = proseOnly(text)
	var out []statedDep
	seen := map[string]bool{}
	find := func(re *lazyre.Regexp, kind string) {
		for _, m := range re.FindAllStringSubmatchIndex(text, 50) {
			key := text[m[2]:m[3]]
			start := strings.LastIndexAny(text[:m[0]], ".!?\n") + 1
			sentence := sentenceAt(text, m[0], m[1])
			if seen[key] || negatedRE.MatchString(text[start:m[0]]) || relativeRE.MatchString(text[start:m[0]]) || strings.HasSuffix(sentence, "?") {
				continue
			}
			seen[key] = true
			match := text[m[0]:m[1]]
			sure := false
			switch kind {
			case "blocked_by", "blocks":
				lower := strings.ToLower(match)
				sure = !looseRE.MatchString(match) && (!strings.HasSuffix(lower, "first") || strings.Contains(lower, " must ") || strings.Contains(lower, " has to ") || strings.Contains(lower, " should "))
			}
			out = append(out, statedDep{key: key, kind: kind, sentence: sentence, sure: sure})
		}
	}
	find(tensionRE, "tension")
	find(supersededByRE, "superseded_by")
	find(supersedesRE, "supersedes")
	find(waitsForRE, "blocked_by")
	find(keyFirstRE, "blocked_by")
	find(holdsUpRE, "blocks")
	return out
}

// sentenceAt is the sentence (or line) around text[from:to].
func sentenceAt(text string, from, to int) string {
	start := strings.LastIndexAny(text[:from], ".!?\n") + 1
	end := len(text)
	if n := strings.IndexAny(text[to:], ".!?\n"); n >= 0 {
		end = to + n + 1
	}
	return strings.TrimSpace(text[start:end])
}

// ---- similarity ----

// simDoc is one issue in the similarity index.
type simDoc struct {
	projectID int64
	open      bool
	title     string             // normalTitle
	vec       map[string]float64 // tf-idf, unit length
	concept   []float64          // place in the concept model; nil without one
}

// simIndex holds every published issue's tf-idf vector. It is rebuilt
// when any issue changes, or a new concept model is trained, reading again
// only the issues that changed: about 0.15s at 3,000 issues.
type simIndex struct {
	key    string
	n      int
	df     map[string]int
	docs   map[int64]*simDoc
	words  map[string]string // stem → a word as written, for reasons
	boiler boilerplate       // template runs left out of counts
	model  *semantic.Model   // nil until one is trained
}

type similarCache struct {
	mu  sync.Mutex
	idx *simIndex
	// texts are issues' words, and boiler counts their shingles, so a
	// rebuild only reads again the issues whose text changed.
	texts  map[int64]*simText
	boiler boilerCounter
}

// simText is an issue's title and description as simTokens, with their
// shingles.
type simText struct {
	sum         uint64 // textSum of the text they came from
	title, desc []string
	sh          [2][]uint64
}

func newSimText(title, desc string) *simText {
	t := &simText{sum: textSum(title, desc), title: simTokens(title), desc: simTokens(desc)}
	t.sh = [2][]uint64{shingles(t.title), shingles(t.desc)}
	return t
}

// textSum is FNV-1a of a title and description, to tell when they change.
func textSum(title, desc string) uint64 {
	h := uint64(14695981039346656037)
	for _, s := range [...]string{title, "\x00", desc} {
		for i := 0; i < len(s); i++ {
			h = (h ^ uint64(s[i])) * 1099511628211
		}
	}
	return h
}

// similarityIndex returns the index, rebuilding it if issues changed.
func (s *Service) similarityIndex(ctx context.Context) (*simIndex, error) {
	var n, updated, ids int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(max(updated_at), 0), coalesce(sum(id), 0) FROM issues`).Scan(&n, &updated, &ids); err != nil {
		return nil, err
	}
	corpus := fmt.Sprint(n, updated, ids)
	s.wantConcepts(n, corpus)
	model, gen := s.concepts()
	key := fmt.Sprint(corpus, " ", gen)
	s.similar.mu.Lock()
	defer s.similar.mu.Unlock()
	if s.similar.idx != nil && s.similar.idx.key == key {
		return s.similar.idx, nil
	}
	// Drafts and text held from outside (WP-279) stay out: their words
	// would show up in reasons.
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, i.title, i.description, st.category NOT IN ('done','canceled')
		FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.draft = 0 AND i.outside = 0 ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := &simIndex{key: key, df: map[string]int{}, docs: map[int64]*simDoc{}, words: map[string]string{}, model: model}
	// The cache is detached while it's updated: a failed pass leaves none,
	// and the next one starts over.
	old, bc := s.similar.texts, s.similar.boiler
	s.similar.texts, s.similar.boiler = nil, nil
	if bc == nil {
		bc = boilerCounter{}
	}
	texts := make(map[int64]*simText, len(old))
	var order []int64 // row order, so reasons pick the same words each time
	for rows.Next() {
		var id, pid int64
		var open bool
		var title, desc string
		if err := rows.Scan(&id, &pid, &title, &desc, &open); err != nil {
			return nil, err
		}
		t := old[id]
		if t == nil || t.sum != textSum(title, desc) {
			if t != nil {
				bc.remove(t.sh[0], t.sh[1])
			}
			t = newSimText(title, desc)
			bc.add(t.sh[0], t.sh[1])
		}
		texts[id] = t
		order = append(order, id)
		idx.docs[id] = &simDoc{projectID: pid, open: open, title: normalTitle(title)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, t := range old {
		if texts[id] == nil {
			bc.remove(t.sh[0], t.sh[1])
		}
	}
	s.similar.texts, s.similar.boiler = texts, bc
	idx.boiler = bc.boilerplate()
	counts := make(map[int64]map[string]int, len(texts))
	for _, id := range order {
		t := texts[id]
		c := simCountsOf(t.title, t.desc, t.sh, idx.boiler, idx.note)
		for w := range c {
			idx.df[w]++
		}
		counts[id] = c
	}
	idx.n = len(idx.docs)
	for id, c := range counts {
		d := idx.docs[id]
		d.vec = idx.weigh(c)
		d.concept = model.Project(c)
	}
	s.similar.idx = idx
	return idx, nil
}

// titleWeight counts a title word as this many description words.
const titleWeight = 3

// simCounts counts an issue's terms, title words weighted up; note, if
// given, sees each term with a word it came from.
func simCounts(title, desc string, note func(term, word string)) map[string]int {
	return simCountsWithout(title, desc, nil, note)
}

// simCountsWithout is simCounts leaving out boilerplate runs.
func simCountsWithout(title, desc string, bp boilerplate, note func(term, word string)) map[string]int {
	t, d := simTokens(title), simTokens(desc)
	var sh [2][]uint64
	if len(bp) > 0 {
		sh = [2][]uint64{shingles(t), shingles(d)}
	}
	return simCountsOf(t, d, sh, bp, note)
}

// simCountsOf counts terms from simTokens and their shingles, boilerplate
// runs left out.
func simCountsOf(title, desc []string, sh [2][]uint64, bp boilerplate, note func(term, word string)) map[string]int {
	c := map[string]int{}
	count := func(toks []string, sh []uint64, weight int) {
		for _, w := range bp.strip(toks, sh) {
			if !simKeep(w) {
				continue
			}
			t := simStem(w)
			c[t] += weight
			if note != nil {
				note(t, w)
			}
		}
	}
	count(title, sh[0], titleWeight)
	count(desc, sh[1], 1)
	return c
}

// note keeps the first word seen for each term.
func (idx *simIndex) note(term, word string) {
	if _, ok := idx.words[term]; !ok {
		idx.words[term] = word
	}
}

// weigh turns counts into a unit tf-idf vector; terms every issue has
// weigh nothing.
func (idx *simIndex) weigh(c map[string]int) map[string]float64 {
	v := make(map[string]float64, len(c))
	var norm float64
	for t, n := range c {
		df := max(idx.df[t], 1)
		w := (1 + math.Log(float64(n))) * math.Log(float64(max(idx.n, df))/float64(df))
		if w > 0 {
			v[t] = w
			norm += w * w
		}
	}
	norm = math.Sqrt(norm)
	for t := range v {
		v[t] /= norm
	}
	return v
}

// vector is the tf-idf vector of text that may not be indexed (a draft,
// or a new issue's text before it is saved).
func (idx *simIndex) vector(title, desc string) map[string]float64 {
	return idx.weigh(simCountsWithout(title, desc, idx.boiler, nil))
}

// meaning is text's place in the concept model; nil without one.
func (idx *simIndex) meaning(title, desc string) []float64 {
	if idx.model == nil {
		return nil
	}
	return idx.model.Project(simCountsWithout(title, desc, idx.boiler, nil))
}

type simMatch struct {
	id    int64
	score float64 // word cosine
	blend float64 // with the concept cosine; the word cosine without a model
}

// related lists the open issues related to an issue's vectors, best
// first: alike in words (simLow), or about the same things (simBlend)
// with some words shared.
func (idx *simIndex) related(vec map[string]float64, concept []float64, self int64) []simMatch {
	var out []simMatch
	for id, d := range idx.docs {
		if id == self || !d.open {
			continue
		}
		sc := cosine(vec, d.vec)
		if sc < simFloor {
			continue
		}
		bl := sc
		if concept != nil && d.concept != nil {
			bl += simBlendWeight * semantic.Cos(concept, d.concept)
		}
		if sc >= simLow || (concept != nil && bl >= simBlend) {
			out = append(out, simMatch{id, sc, bl})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].blend != out[b].blend {
			return out[a].blend > out[b].blend
		}
		return out[a].id < out[b].id
	})
	return out
}

// similar lists the open issues at least min alike to vec, best first,
// leaving out the issue itself.
func (idx *simIndex) similar(vec map[string]float64, self int64, min float64) []simMatch {
	var out []simMatch
	for id, d := range idx.docs {
		if id == self || !d.open {
			continue
		}
		if sc := cosine(vec, d.vec); sc >= min {
			out = append(out, simMatch{id, sc, sc})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].id < out[b].id
	})
	return out
}

// sameTitle lists a project's open issues with this normalTitle.
func (idx *simIndex) sameTitle(projectID int64, title string) []int64 {
	var out []int64
	for id, d := range idx.docs {
		if title != "" && d.open && d.projectID == projectID && d.title == title {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func cosine(a, b map[string]float64) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	var sum float64
	for t, w := range a {
		sum += w * b[t]
	}
	return sum
}

// shared names the three terms that count most toward two vectors'
// similarity, as people write them.
func (idx *simIndex) shared(a, b map[string]float64) string {
	type tw struct {
		t string
		w float64
	}
	var ts []tw
	for t, w := range a {
		if b[t] > 0 {
			ts = append(ts, tw{t, w * b[t]})
		}
	}
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].w != ts[j].w {
			return ts[i].w > ts[j].w
		}
		return ts[i].t < ts[j].t
	})
	var words []string
	for _, x := range ts[:min(3, len(ts))] {
		words = append(words, cmp(idx.words[x.t], x.t))
	}
	return strings.Join(words, ", ") + "."
}

// simWords are the words in text that say what it is about: no issue keys
// or URLs, no short or common words, no bare numbers.
func simWords(text string) []string {
	var out []string
	for _, w := range simTokens(text) {
		if simKeep(w) {
			out = append(out, w)
		}
	}
	return out
}

// simKeep: a word long enough and uncommon enough to say something.
func simKeep(w string) bool {
	return len(w) >= 3 && !simStopwords[w] && strings.Trim(w, "0123456789") != ""
}

// simStem folds plurals and common endings together, enough for
// "relations" to meet "relation" and "related".
func simStem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

// normalTitle is a title in lower case with only its words, for spotting
// the same title twice.
func normalTitle(t string) string {
	return strings.Join(wordTokens(t), " ")
}

var simStopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for you your should can could would will want need needs this that these those with from into onto
		about after before then than there here its it's are was were been being have has had not but nor yet also just only still let may might
		must shall when where what which who how why all any each one two our out use used using get set new now see via per own more most some
		such them they their over same other very does done make made way first last like isn don doesn didn won can't`) {
		simStopwords[w] = true
	}
}
