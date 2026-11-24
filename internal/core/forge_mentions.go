package core

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Forge mentions (WP-379, WP-ADR24): a pull request, commit or forge issue
// that names an issue key is evidence of a relationship, and every such
// place is recorded (forge_mentions), with what the text says the
// relationship is. Only an item that is the issue's own work becomes a link
// (external_links), because a link has effects that belong to that work: a
// pull request link puts an in-progress task In Review and out of the
// agents' queue, makes wp done wait for it, closes the issue's open
// requests when it merges, and drives the "PR opened/merged" automations.
// Own work is named in the branch or the title (or a commit's subject),
// or anywhere with a closing verb ("Fixes WP-12", the "Issue: WP-12" line
// wp ship writes). A key in a body or comment otherwise ("follow-up to
// WP-12", a commit body listing related tasks) is a mention only.

// keyRef is one issue key found in a piece of forge text.
type keyRef struct {
	Key   string
	Place string // title, branch, body, subject, message, comment, review
	Verb  string // the relationship the text states ("" none), see refVerbs
	Line  string // the line naming it, trimmed (the branch, for a branch)
	Own   bool   // it makes the item the issue's own work
}

// refVerbs are the relationships a few words before a key can state, in
// the order they are tried. The forms GitHub and GitLab close issues with
// (fixes, closes, resolves) and "implements" mark the item as the issue's
// own work; "issue" is a line labelled "Issue:" or "Task:", as wp ship's
// pull request body writes it.
var refVerbs = []struct{ verb, re string }{
	{"fixes", `fix(?:e[sd]|ing)?`},
	{"closes", `clos(?:e[sd]?|ing)`},
	{"resolves", `resolv(?:e[sd]?|ing)`},
	{"implements", `implement(?:s|ed|ing)?`},
	{"reverts", `revert(?:s|ed|ing)?|undo(?:es|ing)?|roll(?:s|ed|ing)? back`},
	{"supersedes", `supersed(?:e[sd]?|ing)|replac(?:e[sd]?|ing)|instead of`},
	{"duplicates", `dup(?:e|licate)? of|duplicates?|same as`},
	{"depends_on", `blocked (?:by|on)|depends? on|depending on|needs|requires|waits? (?:on|for)|after`},
	{"blocks", `(?:un)?block(?:s|ing)?`},
	{"part_of", `parts? of|towards?|subtask of|child of|step(?: \d+)? (?:of|for|in|toward|towards)`},
	{"follows", `follow[- ]?ups? (?:to|of|for|on|from)|follow[- ]?ups?|follows(?: up)?(?: on)?|continu(?:es|ing|ation of)|builds? on|split (?:out )?(?:from|of)`},
	{"relates", `see(?: also)?|re|refs?|referenc(?:e[sd]?|ing)|related(?: to)?|relates? to|cf\.?|mentioned (?:in|by)|like|per|from|via|similar to|as in|also`},
	{"not", `not|except|unlike|without|isn'?t`},
}

// ownVerbs mark the item as the issue's own work wherever they are.
var ownVerbs = map[string]bool{"fixes": true, "closes": true, "resolves": true, "implements": true, "issue": true}

var (
	// refVerbRE matches a verb right before a key: the end of the text
	// before it on its line, allowing a colon and words like "the issue".
	refVerbRE = func() *lazyre.Regexp {
		var alts []string
		for _, v := range refVerbs {
			alts = append(alts, "("+v.re+")")
		}
		return lazyre.New(`(?i)(?:^|[^a-z'])(?:` + strings.Join(alts, "|") + `)\s*[:\-–]?\s*(?:(?:the|this|that|issue|task|ticket|story|bug|epic|in|on|to|with)\s+)*[("'\x60\[]*$`)
	}()
	// refLabelRE is a line labelled as the work's issue: "Issue: WP-12".
	refLabelRE = lazyre.New(`(?i)^\s*(?:[-*>]\s*)?(?:\*\*)?(?:issues?|tasks?|tickets?)(?:\*\*)?\s*:\s*(?:\*\*)?\s*$`)
	// refListRE is what joins keys in a list ("WP-1, WP-2 and WP-3"): the
	// later ones share the first one's verb.
	refListRE = lazyre.New(`(?i)^[\s,;/&+]*(?:(?:and|or)\s+)?[\s,;/&+]*$`)
)

// refVerb is the relationship the text before a key on its line states.
func refVerb(before string) string {
	if refLabelRE.MatchString(before) {
		return "issue"
	}
	m := refVerbRE.FindStringSubmatch(before)
	if m == nil {
		return ""
	}
	for i, v := range refVerbs {
		if m[i+1] != "" {
			return v.verb
		}
	}
	return ""
}

// ownRef says whether a key at place with verb makes an item of kind
// (pull, commit, issue) the issue's own work.
func ownRef(kind, place, verb string) bool {
	switch place {
	case "branch":
		return true
	case "title", "subject":
		return verb == "" || ownVerbs[verb]
	case "body", "message":
		return kind != "issue" && ownVerbs[verb]
	}
	return false // comments and reviews talk about work; they aren't it
}

// scanKeyRefs finds the issue keys in one place of an item's text.
func scanKeyRefs(kind, place, text string) []keyRef {
	var out []keyRef
	for line := range strings.SplitSeq(text, "\n") {
		prevEnd, prevVerb := -1, ""
		for _, m := range issueKeyRE.FindAllStringSubmatchIndex(line, -1) {
			key := line[m[2]:m[3]] + "-" + line[m[4]:m[5]]
			var verb string
			if prevEnd >= 0 && refListRE.MatchString(line[prevEnd:m[0]]) {
				verb = prevVerb
			} else {
				verb = refVerb(line[:m[0]])
			}
			prevEnd, prevVerb = m[1], verb
			out = append(out, keyRef{Key: key, Place: place, Verb: verb, Line: around(line, m[0], 240), Own: ownRef(kind, place, verb)})
		}
	}
	return out
}

// branchRefs finds the issue keys in a branch name (any case, e.g.
// feature/way-12-login): the branch is the work's own.
func branchRefs(branch string) []keyRef {
	var out []keyRef
	for _, m := range branchKeyRE.FindAllStringSubmatch(branch, -1) {
		out = append(out, keyRef{Key: strings.ToUpper(m[1]) + "-" + m[2], Place: "branch", Line: branch, Own: true})
	}
	return out
}

// around is up to n runes of line with byte offset start in view.
func around(line string, start, n int) string {
	line = strings.TrimSpace(line)
	r := []rune(line)
	if len(r) <= n {
		return line
	}
	// The trim may have moved start a little: close enough for a window.
	s := len([]rune(line[:min(start, len(line))]))
	from := max(0, s-n/2)
	to := min(len(r), from+n)
	from = max(0, to-n)
	out := string(r[from:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(r) {
		out += "…"
	}
	return out
}

// forgeText is one piece of forge text to record the mentions of.
type forgeText struct {
	f    *Forge
	repo string
	kind string // pull, commit, issue: the item's
	// item is the forge item as a link shows it (no title for outside
	// text, WP-281).
	item      *ForgeItem
	sourceURL string   // the item's URL, or the comment's
	places    []string // the places this text has: rows there it no longer names are gone
	trusted   bool     // a Waypoint person (or Waypoint) wrote it: lines may be quoted
	author    string
	// fromOutside: a pull request's author isn't a Waypoint person, even
	// after a person approved its text (doubt counts as outside).
	fromOutside bool
	// link: own work becomes a link (an open pull request's text, a
	// forge issue's or a pushed commit's; not a closed pull request read
	// by a sync). takeBack: links this text made and no longer makes are
	// removed.
	link, takeBack bool
}

// mentionResult is what recording a source's mentions changed.
type mentionResult struct {
	Recorded, Gone, Linked, Unlinked int
}

// recordForgeMentions records refs (all from src) and links the issues whose
// own work src is. Keys resolve only in projects with the repo attached: a
// mention in some unrelated repo must not reach an issue.
func (s *Service) recordForgeMentions(ctx context.Context, src forgeText, refs []keyRef) (mentionResult, error) {
	var res mentionResult
	if src.item == nil || src.item.URL == "" || src.sourceURL == "" || src.item.Confidential {
		return res, nil
	}
	defer forgeLocks.lock("mentions|" + src.sourceURL)()
	// One row per issue and place: the own reference, else the first that
	// states a relationship, else the first.
	type ip struct {
		issue int64
		place string
	}
	byPlace := map[ip]*mentionHit{}
	var order []ip
	issues := map[string]*mentionHit{} // key → resolved issue (nil: not one here)
	attached := map[int64]bool{}
	for _, r := range refs {
		hit, seen := issues[r.Key]
		if !seen {
			hit = s.mentionTarget(ctx, src, r.Key, attached)
			issues[r.Key] = hit
		}
		if hit == nil {
			continue
		}
		k := ip{hit.issueID, r.Place}
		cur := byPlace[k]
		switch {
		case cur == nil:
			f := *hit
			f.ref = r
			byPlace[k] = &f
			order = append(order, k)
		case !cur.ref.Own && (r.Own || verbRank(r.Verb) > verbRank(cur.ref.Verb)):
			cur.ref = r
		}
	}
	now := s.nowMS()
	number := any(nil)
	if src.item.Number != 0 {
		number = src.item.Number
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		type old struct {
			id                     int64
			verb, excerpt, title   string
			state, author, itemURL string
			own                    bool
			gone                   bool
		}
		rows, err := t.QueryContext(ctx, `SELECT id, issue_id, place, verb, own, excerpt, title, state, author, item_url, gone_at IS NOT NULL
			FROM forge_mentions WHERE source_url = ?`, src.sourceURL)
		if err != nil {
			return err
		}
		have := map[ip]old{}
		for rows.Next() {
			var o old
			var k ip
			if err := rows.Scan(&o.id, &k.issue, &k.place, &o.verb, &o.own, &o.excerpt, &o.title, &o.state, &o.author, &o.itemURL, &o.gone); err != nil {
				rows.Close()
				return err
			}
			have[k] = o
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		type ref struct {
			project int64
			key     string
		}
		touched := map[int64]ref{} // one event per issue
		owned := map[int64]*mentionHit{}
		for _, k := range order {
			m := byPlace[k]
			excerptText := ""
			if src.trusted {
				excerptText = m.ref.Line
			}
			if m.ref.Own {
				if owned[m.issueID] == nil {
					owned[m.issueID] = m
				}
			}
			o, ok := have[k]
			switch {
			case !ok:
				if _, err := t.ExecContext(ctx, `INSERT INTO forge_mentions(issue_id, forge_id, repo, item_kind, item_url, number, source_url, place, verb, own, excerpt, title, state, author, created_at, updated_at)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.issueID, src.f.ID, src.repo, src.kind, src.item.URL, number, src.sourceURL, k.place,
					m.ref.Verb, m.ref.Own, excerptText, src.item.Title, src.item.State, src.author, now, now); err != nil {
					return err
				}
				res.Recorded++
				touched[m.issueID] = ref{m.projectID, m.key}
			case o.gone || o.verb != m.ref.Verb || o.own != m.ref.Own || o.excerpt != excerptText || o.title != src.item.Title ||
				o.state != src.item.State || o.author != src.author:
				if _, err := t.ExecContext(ctx, `UPDATE forge_mentions SET verb = ?, own = ?, excerpt = ?, title = ?, state = ?, author = ?, updated_at = ?, gone_at = NULL WHERE id = ?`,
					m.ref.Verb, m.ref.Own, excerptText, src.item.Title, src.item.State, src.author, now, o.id); err != nil {
					return err
				}
				if o.gone || o.verb != m.ref.Verb || o.own != m.ref.Own {
					res.Recorded++
					touched[m.issueID] = ref{m.projectID, m.key}
				}
			}
			delete(have, k)
		}
		// What this text no longer names.
		for k, o := range have {
			if o.gone || !slices.Contains(src.places, k.place) {
				continue
			}
			if _, err := t.ExecContext(ctx, `UPDATE forge_mentions SET gone_at = ?, updated_at = ? WHERE id = ?`, now, now, o.id); err != nil {
				return err
			}
			res.Gone++
			if pid, key, err := s.issueProject(ctx, t, k.issue); err == nil {
				touched[k.issue] = ref{pid, key}
			}
		}
		if src.link {
			n, err := s.linkOwnWork(ctx, t, src, owned)
			if err != nil {
				return err
			}
			res.Linked = n
		}
		if src.takeBack {
			n, err := s.takeBackLinks(ctx, t, src, owned)
			if err != nil {
				return err
			}
			res.Unlinked = n
		}
		for issueID, r := range touched {
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: r.project, IssueID: issueID, IssueKey: r.key})
		}
		return nil
	})
	return res, err
}

// mentionHit is an issue a key resolved to, and the reference recorded.
type mentionHit struct {
	issueID, projectID int64
	key                string // the issue's key now
	ref                keyRef
}

// mentionTarget resolves a key to an issue in a project with src's repo
// attached (nil: none).
func (s *Service) mentionTarget(ctx context.Context, src forgeText, key string, attached map[int64]bool) *mentionHit {
	id, err := s.resolveIssueID(ctx, s.db.R, key)
	if err != nil {
		return nil
	}
	pid, key, err := s.issueProject(ctx, s.db.R, id)
	if err != nil {
		return nil
	}
	ok, seen := attached[pid]
	if !seen {
		ok = s.repoAttached(ctx, pid, src.f.ID, src.repo)
		attached[pid] = ok
	}
	if !ok {
		return nil
	}
	return &mentionHit{issueID: id, projectID: pid, key: key}
}

// issueProject is an issue's project and key.
func (s *Service) issueProject(ctx context.Context, q queryer, issueID int64) (int64, string, error) {
	var pid int64
	var key string
	err := q.QueryRowContext(ctx, `SELECT i.project_id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issueID).Scan(&pid, &key)
	return pid, key, err
}

// linkOwnWork links src's item to the issues it is the own work of,
// unless someone unlinked it from one. It returns how many it linked. An
// outside author's pull request, approved or not, leaves out an issue
// still waiting as an outside draft: publishing the issue, or its author
// becoming a person, makes the link and runs it (WP-1802). A person's pull
// request links the draft at once.
func (s *Service) linkOwnWork(ctx context.Context, t *tx, src forgeText, owned map[int64]*mentionHit) (int, error) {
	var n int
	number := any(nil)
	if src.item.Number != 0 {
		number = src.item.Number
	}
	for issueID, m := range owned {
		if src.kind == "pull" && src.fromOutside {
			var outside bool
			if err := t.QueryRowContext(ctx, `SELECT outside FROM issues WHERE id = ?`, issueID).Scan(&outside); err != nil {
				return n, err
			}
			if outside {
				continue
			}
		}
		added, err := s.linkOwnItem(ctx, t, issueID, src.f.ID, src.f.Kind, src.kind, src.item.URL, src.repo, number, src.item.Title, "", m.ref.Place)
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	return n, nil
}

// linkOwnItem links an item to an issue as its own work, unless someone
// unlinked it. It says whether a link was added; one made before forge
// mentions were recorded only learns how.
func (s *Service) linkOwnItem(ctx context.Context, t *tx, issueID, forgeID int64, provider, kind, url, repo string, number any, title, state, place string) (bool, error) {
	var unlinked bool
	if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_unlinks WHERE issue_id = ? AND url = ?)`, issueID, url).Scan(&unlinked); err != nil {
		return false, err
	}
	if unlinked {
		return false, nil
	}
	res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO external_links(issue_id, provider, kind, url, repo, number, title, state, synced_at, created_at, via)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, issueID, provider, kind, url, repo, number, title, state, s.nowMS(), s.nowMS(), place)
	if err != nil {
		return false, err
	}
	if added, _ := res.RowsAffected(); added == 0 {
		// Linked before forge mentions were recorded: say how.
		_, err := t.ExecContext(ctx, `UPDATE external_links SET via = ? WHERE issue_id = ? AND url = ? AND via = ''`, place, issueID, url)
		return false, err
	}
	i, err := s.issueByID(ctx, t, issueID, false)
	if err != nil {
		return false, err
	}
	ev := &Event{Kind: EvIssueLinked, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: kind, New: url}
	if kind == "pull" && number != nil {
		ev.pullLinked = &pullRef{forgeID: forgeID, repo: repo, number: number.(int)}
	}
	t.emit(ev)
	return true, nil
}

// linkHeldOwnWork links the pull requests whose own text named issues
// that were outside drafts and are published now (WP-1802), with the
// state last seen, and sets off what opening one would have, unless a
// person hasn't approved its pull request yet.
func (s *Service) linkHeldOwnWork(ctx context.Context, t *tx, issueID int64) error {
	rows, err := t.QueryContext(ctx, `SELECT m.forge_id, f.kind, m.repo, m.item_url, m.number, m.title, m.state, m.place FROM forge_mentions m
		JOIN forges f ON f.id = m.forge_id
		WHERE m.issue_id = ? AND m.own = 1 AND m.gone_at IS NULL AND m.item_kind = 'pull' AND m.number IS NOT NULL AND m.state IN ('open', 'draft', 'merged')`, issueID)
	if err != nil {
		return err
	}
	type pull struct {
		forgeID                                  int64
		provider, repo, url, title, state, place string
		number                                   int
	}
	var list []pull
	for rows.Next() {
		var p pull
		if err := rows.Scan(&p.forgeID, &p.provider, &p.repo, &p.url, &p.number, &p.title, &p.state, &p.place); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range list {
		added, err := s.linkOwnItem(ctx, t, issueID, p.forgeID, p.provider, "pull", p.url, p.repo, p.number, p.title, p.state, p.place)
		if err != nil {
			return err
		}
		if !added || p.state != "open" {
			continue
		}
		if _, err := s.emitPullOpened(ctx, t, issueID, p.url, "", p.title, p.number, true); err != nil {
			return err
		}
	}
	return nil
}

// takeBackLinks removes the links src's text made (not added ones, nor
// those from before WP-379) to issues it no longer names as its own work,
// unless the link moved the issue (removing it then is a person's call).
func (s *Service) takeBackLinks(ctx context.Context, t *tx, src forgeText, owned map[int64]*mentionHit) (int, error) {
	rows, err := t.QueryContext(ctx, `SELECT el.id, el.issue_id, i.project_id FROM external_links el JOIN issues i ON i.id = el.issue_id
		WHERE el.url = ? AND el.kind = ? AND el.via NOT IN ('', 'added') AND el.mirror = 0 AND el.moved_to IS NULL`, src.item.URL, src.kind)
	if err != nil {
		return 0, err
	}
	type gone struct{ id, issue, project int64 }
	var list []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.issue, &g.project); err != nil {
			rows.Close()
			return 0, err
		}
		if owned[g.issue] == nil {
			list = append(list, g)
		}
	}
	rows.Close()
	for _, g := range list {
		if _, err := t.ExecContext(ctx, `DELETE FROM external_links WHERE id = ?`, g.id); err != nil {
			return 0, err
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: g.project, IssueID: g.issue})
	}
	return len(list), nil
}

// ---- the texts ----

// scanPull records a pull request's mentions: its branch, title and body.
// raw is the item as the forge sent it (whose text is scanned), shown as a
// link may show it. link: its own work becomes links (an open pull request).
func (s *Service) scanPull(ctx context.Context, f *Forge, repo string, raw, shown *ForgeItem, branch string, link bool) (mentionResult, error) {
	refs := branchRefs(branch)
	refs = append(refs, scanKeyRefs("pull", "title", stripWIP(raw.Title))...)
	refs = append(refs, scanKeyRefs("pull", "body", raw.Body)...)
	places := []string{"title", "body"}
	if branch != "" {
		places = append(places, "branch")
	}
	return s.recordForgeMentions(ctx, forgeText{f: f, repo: repo, kind: "pull", item: shown, sourceURL: raw.URL, places: places,
		trusted: shown == raw, author: raw.Author, link: link, takeBack: link,
		fromOutside: s.prVoice(ctx, f).authorOutside(ctx, s.db.R, raw.Author, effectiveAuthor(raw.Author, raw.Body))}, refs)
}

// scanForgeIssue records a forge issue's mentions: a key in its title
// links it, one in its body is a mention.
func (s *Service) scanForgeIssue(ctx context.Context, f *Forge, repo string, raw, shown *ForgeItem) (mentionResult, error) {
	refs := append(scanKeyRefs("issue", "title", raw.Title), scanKeyRefs("issue", "body", raw.Body)...)
	return s.recordForgeMentions(ctx, forgeText{f: f, repo: repo, kind: "issue", item: shown, sourceURL: raw.URL, places: []string{"title", "body"},
		trusted: shown == raw, author: raw.Author, link: true, takeBack: true}, refs)
}

// scanCommit records a pushed commit's mentions: a key in its subject
// links it; one in the rest of its message only with a closing verb.
func (s *Service) scanCommit(ctx context.Context, f *Forge, repo string, c forgeCommit) (mentionResult, error) {
	subject, body, _ := strings.Cut(c.message, "\n")
	trusted := !s.prVoice(ctx, f).outside(ctx, s.db.R, c.author)
	title := ""
	if trusted {
		title = excerpt(subject, 200) // a stranger's message, merged by a person, shows as the commit alone (WP-281)
	}
	refs := append(scanKeyRefs("commit", "subject", subject), scanKeyRefs("commit", "message", body)...)
	return s.recordForgeMentions(ctx, forgeText{f: f, repo: repo, kind: "commit", item: &ForgeItem{Title: title, URL: c.url, State: "pushed"},
		sourceURL: c.url, places: []string{"subject", "message"}, trusted: trusted, author: c.author, link: true}, refs)
}

// scanComment records the mentions in a comment (or review) on a pull
// request or forge issue: never a link, always evidence. A deleted comment
// no longer names anything.
func (s *Service) scanComment(ctx context.Context, f *Forge, repo string, onPR bool, number int, place, action string, c ForgeComment) {
	if c.URL == "" || isBotText(c.Body) {
		return
	}
	kind := "issue"
	if onPR {
		kind = "pull"
	}
	itemURL, _, _ := strings.Cut(c.URL, "#")
	item := &ForgeItem{Number: number, URL: itemURL, IsPR: onPR}
	// The item's title and state as its other mentions and links show it.
	_ = s.db.R.QueryRowContext(ctx, `SELECT title, state FROM (
			SELECT title, state, 0 AS o FROM external_links WHERE url = ?
			UNION ALL SELECT title, state, 1 FROM forge_mentions WHERE item_url = ?) ORDER BY o LIMIT 1`, itemURL, itemURL).Scan(&item.Title, &item.State)
	var refs []keyRef
	if action != "deleted" {
		refs = scanKeyRefs(kind, place, c.Body)
	}
	trusted := !s.prVoice(ctx, f).outside(ctx, s.db.R, c.Author)
	if _, err := s.recordForgeMentions(ctx, forgeText{f: f, repo: repo, kind: kind, item: item, sourceURL: c.URL, places: []string{place},
		trusted: trusted, author: c.Author}, refs); err != nil {
		s.log.WarnContext(ctx, "record forge mentions", "url", c.URL, "err", err)
	}
}

// ---- reading ----

// ForgeMention is a forge item that names an issue, with where and how.
type ForgeMention struct {
	Kind   string `json:"kind"` // pull, commit, issue
	URL    string `json:"url"`
	Repo   string `json:"repo"`
	Number *int64 `json:"number,omitempty"`
	Title  string `json:"title"`
	State  string `json:"state"`
	// Places are where it names the issue: title, branch, body, subject,
	// message, comment, review.
	Places []string `json:"places"`
	// Verb is the relationship the text states: fixes, closes, resolves,
	// implements, issue, part_of, follows, depends_on, blocks, reverts,
	// supersedes, duplicates, relates, not; "" when it just names it.
	Verb    string `json:"verb,omitempty"`
	Excerpt string `json:"excerpt,omitempty"` // the line naming it ("" for outside text)
	// SourceURL is where the text is (a comment's URL, or the item's).
	SourceURL string    `json:"source_url"`
	Linked    bool      `json:"linked"` // the item is also linked to the issue
	Own       bool      `json:"own"`    // the text names it as the item's own work
	Gone      bool      `json:"gone"`   // the text no longer names it
	At        time.Time `json:"at"`     // first seen
}

// forgeMentions lists the forge items that name an issue, one per item,
// newest first. all includes linked items and text that no longer names
// it; otherwise only what is mentioned and not linked.
func (s *Service) forgeMentions(ctx context.Context, q queryer, issueID int64, all bool) ([]ForgeMention, error) {
	rows, err := q.QueryContext(ctx, `SELECT fm.item_kind, fm.item_url, fm.repo, fm.number, fm.title, fm.state, fm.place, fm.verb, fm.excerpt, fm.source_url,
			fm.own, fm.gone_at IS NOT NULL, fm.created_at, EXISTS(SELECT 1 FROM external_links el WHERE el.issue_id = fm.issue_id AND el.url = fm.item_url)
		FROM forge_mentions fm WHERE fm.issue_id = ? ORDER BY fm.created_at DESC, fm.id DESC`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForgeMention{}
	at := map[string]int{}
	best := map[string]int{} // URL → rank of the row its verb came from
	for rows.Next() {
		var m ForgeMention
		var num sql.NullInt64
		var place string
		var created int64
		if err := rows.Scan(&m.Kind, &m.URL, &m.Repo, &num, &m.Title, &m.State, &place, &m.Verb, &m.Excerpt, &m.SourceURL, &m.Own, &m.Gone, &created, &m.Linked); err != nil {
			return nil, err
		}
		if !all && (m.Gone || m.Linked) {
			continue
		}
		m.Number, m.At = ptrInt(num), time.UnixMilli(created).UTC()
		if i, ok := at[m.URL]; ok {
			o := &out[i]
			if !slices.Contains(o.Places, place) {
				o.Places = append(o.Places, place)
			}
			o.Own = o.Own || m.Own
			// The verb, excerpt and link come from one text: the best row's. A
			// live text beats one that no longer names the issue, a trusted one
			// (it has its excerpt) beats an outside one, then the stronger verb;
			// a tie keeps the newer.
			if r := mentionRank(m); r > best[m.URL] {
				o.Verb, o.Excerpt, o.SourceURL = m.Verb, m.Excerpt, m.SourceURL
				best[m.URL] = r
			}
			o.Gone = o.Gone && m.Gone
			if m.At.Before(o.At) {
				o.At = m.At
			}
			continue
		}
		m.Places = []string{place}
		best[m.URL] = mentionRank(m)
		at[m.URL] = len(out)
		out = append(out, m)
	}
	return out, rows.Err()
}

// verbRank orders verbs by how much they claim: an own-work verb, another
// relationship, a bare "see", none.
func verbRank(verb string) int {
	switch verb {
	case "":
		return 0
	case "relates":
		return 1
	case "fixes", "closes", "resolves", "implements", "issue":
		return 3
	}
	return 2
}

// mentionRank is how well a row's text states the mention (see forgeMentions).
func mentionRank(m ForgeMention) int {
	r := verbRank(m.Verb) * 2
	if m.Excerpt != "" {
		if m.Verb == "" {
			r++ // no verb to give, but the line that names it
		} else {
			r += 20
		}
	}
	if !m.Gone {
		r += 1000
	}
	return r
}

// ForgeMentions lists the forge items naming an issue (all: also linked
// ones and text that no longer names it).
func (s *Service) ForgeMentions(ctx context.Context, actor *Actor, ref string, all bool) ([]ForgeMention, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	return s.forgeMentions(ctx, s.db.R, i.ID, all)
}

// ---- before WP-379 ----

const forgeMentionsSetting = "forge_mentions_v1"

// ForgeMentionBackfill is what BackfillForgeMentions did.
type ForgeMentionBackfill struct {
	Items    int `json:"items"`    // pull requests and forge issues read again
	Recorded int `json:"recorded"` // mentions recorded
	Kept     int `json:"kept"`     // links their text makes
	Demoted  int `json:"demoted"`  // links their text only mentions, now mentions
	Failed   int `json:"failed"`   // items the forge didn't return
}

// BackfillForgeMentions reads again the pull requests and forge issues
// that were linked by scanning their text before WP-379, records their
// mentions, and removes the links their text only mentions (a key in
// passing in a body), unless the link moved its issue. Links whose key
// the text doesn't name were added by hand and stay. It runs once, in the
// background at start; force reruns it.
func (s *Service) BackfillForgeMentions(ctx context.Context, force bool) (ForgeMentionBackfill, error) {
	var res ForgeMentionBackfill
	if !force {
		var done int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM server_settings WHERE key = ?`, forgeMentionsSetting).Scan(&done); err != nil || done > 0 {
			return res, err
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT el.kind, el.url, el.repo, el.number FROM external_links el
		WHERE el.kind IN ('pull', 'issue') AND el.via = '' AND el.created_agent = '' AND el.mirror = 0 AND el.number IS NOT NULL ORDER BY el.url`)
	if err != nil {
		return res, err
	}
	type item struct {
		kind, url, repo string
		number          int
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.kind, &it.url, &it.repo, &it.number); err != nil {
			rows.Close()
			return res, err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		f := s.forgeForLink(ctx, ExternalLink{URL: it.url})
		if f == nil || f.ID == 0 {
			continue
		}
		raw, err := s.Forges.GetItem(ctx, f, it.repo, it.number, it.kind == "pull")
		if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusGone) {
			continue // gone for good: retrying at every start would not read it
		}
		if err != nil || raw == nil || raw.URL == "" {
			res.Failed++
			continue
		}
		if raw.URL != it.url {
			c := *raw
			c.URL = it.url // as linked: the forge may spell it differently
			raw = &c
		}
		res.Items++
		shown := s.linkItem(ctx, f, raw)
		var r mentionResult
		if it.kind == "pull" {
			var branch string
			_ = s.db.R.QueryRowContext(ctx, `SELECT head_ref FROM pull_requests WHERE forge_id = ? AND url = ?`, f.ID, it.url).Scan(&branch)
			r, err = s.scanPull(ctx, f, it.repo, raw, shown, branch, false)
		} else {
			refs := append(scanKeyRefs("issue", "title", raw.Title), scanKeyRefs("issue", "body", raw.Body)...)
			r, err = s.recordForgeMentions(ctx, forgeText{f: f, repo: it.repo, kind: "issue", item: shown, sourceURL: raw.URL,
				places: []string{"title", "body"}, trusted: shown == raw, author: raw.Author}, refs)
		}
		if err != nil {
			return res, err
		}
		res.Recorded += r.Recorded
		kept, demoted, err := s.settleLegacyLinks(ctx, it.url)
		if err != nil {
			return res, err
		}
		res.Kept += kept
		res.Demoted += demoted
	}
	if res.Failed > 0 {
		// Read again at the next start: a forge that was down or rate-limited
		// must not keep its legacy links for good.
		s.log.WarnContext(ctx, "backfill forge mentions incomplete", "failed", res.Failed)
		return res, nil
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT OR REPLACE INTO server_settings(key, value) VALUES (?, '1')`, forgeMentionsSetting)
	return res, err
}

// settleLegacyLinks sorts the scan-made links to an item from before
// WP-379 by its recorded mentions: an own mention says how it was made; a
// key the text only mentions loses its link (unless the link moved the
// issue); a key it doesn't name stays (added by hand).
func (s *Service) settleLegacyLinks(ctx context.Context, url string) (kept, demoted int, err error) {
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT el.id, el.issue_id, i.project_id, el.moved_to IS NOT NULL,
				(SELECT fm.place FROM forge_mentions fm WHERE fm.issue_id = el.issue_id AND fm.item_url = el.url AND fm.gone_at IS NULL AND fm.own = 1 LIMIT 1),
				EXISTS(SELECT 1 FROM forge_mentions fm WHERE fm.issue_id = el.issue_id AND fm.item_url = el.url AND fm.source_url = el.url AND fm.gone_at IS NULL)
			FROM external_links el JOIN issues i ON i.id = el.issue_id
			WHERE el.url = ? AND el.via = '' AND el.created_agent = '' AND el.mirror = 0`, url)
		if err != nil {
			return err
		}
		type link struct {
			id, issue, project int64
			moved, named       bool
			own                sql.NullString
		}
		var list []link
		for rows.Next() {
			var l link
			if err := rows.Scan(&l.id, &l.issue, &l.project, &l.moved, &l.own, &l.named); err != nil {
				rows.Close()
				return err
			}
			list = append(list, l)
		}
		rows.Close()
		for _, l := range list {
			switch {
			case l.own.Valid:
				if _, err := t.ExecContext(ctx, `UPDATE external_links SET via = ? WHERE id = ?`, l.own.String, l.id); err != nil {
					return err
				}
				kept++
			case l.named && !l.moved:
				if _, err := t.ExecContext(ctx, `DELETE FROM external_links WHERE id = ?`, l.id); err != nil {
					return err
				}
				t.emit(&Event{Kind: EvProjectUpdated, ProjectID: l.project, IssueID: l.issue})
				demoted++
			}
		}
		return nil
	})
	return kept, demoted, err
}

// RunForgeMentionBackfill is BackfillForgeMentions for the server's start.
func (s *Service) RunForgeMentionBackfill(ctx context.Context) {
	res, err := s.BackfillForgeMentions(ctx, false)
	if err != nil {
		s.log.WarnContext(ctx, "backfill forge mentions", "err", err)
		return
	}
	if res.Items > 0 || res.Failed > 0 {
		s.log.Info("backfilled forge mentions", "items", res.Items, "recorded", res.Recorded, "kept", res.Kept,
			"demoted", res.Demoted, "failed", res.Failed)
	}
}

// String is a one-line account, for logs and the CLI.
func (r ForgeMentionBackfill) String() string {
	return fmt.Sprintf("%d items read, %d mentions recorded, %d links kept, %d demoted to mentions, %d not returned",
		r.Items, r.Recorded, r.Kept, r.Demoted, r.Failed)
}
