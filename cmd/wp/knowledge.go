package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var designKinds = []string{"goal", "principle", "feature", "screen", "decision", "requirement", "note"}

// knowledgeScope is the project a glossary or design command works in:
// the scope's home project, "global" with -p global, or "" for everything
// (-A).
func (c *cli) knowledgeScope(sf scopeFlags) (string, error) {
	if sf.project != nil && strings.EqualFold(*sf.project, "global") {
		return "global", nil
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return "", err
	}
	if sc.all() {
		return "", nil
	}
	return sc.home()
}

// textCall asks for the server's text rendering unless --json.
func (c *cli) textCall(path string, q url.Values) error {
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

var searchCmd = &command{
	name: "search", group: "Knowledge", args: "WORDS... [FLAGS]",
	aliases: []string{"find", "grep"},
	summary: "search issues, requests, notes, glossary, pages, comments",
	about: `Searches the scope (see wp help scope), grouped by kind. A code (R-28, ADR7,
PL3, APP-12, page #60) in any spelling finds its item first. Issues also take
the query language (wp help query). Closed issues and retired notes are left
out unless --archived.`,
	examples: []string{
		"wp search claim lease",
		"wp search API:R-28",
		"wp search login --kind issues,pages",
	},
	run: cmdSearch,
}

func cmdSearch(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	var kinds listFlag
	fs.Var(&kinds, "kind", "only these `KINDS`: issues, requests, design, glossary, pages, comments")
	short(fs, "k", "kind")
	archived := fs.Bool("archived", false, "include closed issues and retired notes")
	alias(fs, "all-states", "archived")
	limit := fs.Int("limit", 10, "at most `N` hits per kind")
	short(fs, "n", "limit")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("search for what? Give some words")
	}
	q := url.Values{"q": {joinArgs(pos)}, "limit": {strconv.Itoa(*limit)}}
	if len(kinds) > 0 {
		q.Set("kinds", strings.Join(kinds, ","))
	}
	if *archived {
		q.Set("archived", "1")
	}
	sc, err := c.workScope(sf)
	if err != nil {
		return err
	}
	if !sc.all() {
		keys := make([]string, len(sc.projects))
		for i, p := range sc.projects {
			keys[i] = p.Key
		}
		q.Set("project", strings.Join(keys, ","))
		c.note("Searching %s.", sc)
	}
	return c.textCall("/api/v1/search/all", q)
}

var defineCmd = &command{
	name: "define", group: "Knowledge", args: "TERM[, TERM...] [FLAGS]",
	aliases: []string{"whatis", "term"},
	summary: "what a term means here (the glossary)",
	about: `Looks a term or alias up in the project's glossary, then its parents' and
the global one. Commas separate several terms.`,
	examples: []string{"wp define claim", "wp define claim, agent run, lane"},
	run:      cmdDefine,
}

func cmdDefine(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("define what? Give a term")
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return err
	}
	return c.each(termList(pos), true, func(term string) error {
		return c.lookupTerm(term, project)
	})
}

// termList is the glossary terms named: commas separate them, since a term
// may have spaces ("agent run, claim").
func termList(pos []string) []string {
	var out []string
	for _, t := range strings.Split(joinArgs(pos), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (c *cli) lookupTerm(term, project string) error {
	q := url.Values{"term": {term}}
	if project != "" {
		q.Set("project", project)
	}
	return c.textCall("/api/v1/glossary/lookup", q)
}

var glossaryCmd = &command{
	name: "glossary", group: "Knowledge", args: "SUBCOMMAND",
	aliases: []string{"gloss", "terms"},
	summary: "the domain glossary of a project (or global)",
	about: `A project's glossary adds its parents' and the global terms; a nearer
definition overrides. Entries have aliases, words to avoid (a lookup of
one names the term to use), and a markdown definition: one tight sentence
first. People can lock an entry against agents. A TERM is the term, an
alias, or #ID.`,
	examples: []string{
		`wp glossary add Share "Access to one issue and everything beneath it." --avoid "invite to issue"`,
		"wp glossary ls",
	},
}

func init() {
	glossaryCmd.subs = []*command{glossaryLsCmd, glossaryShowCmd, glossaryAddCmd, glossaryEditCmd, glossaryLinkCmd, glossaryLockCmd, glossaryUnlockCmd, glossaryRmCmd}
	glossaryCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return glossaryLs(c, args)
		}
		return c.usage("no glossary command %q (to look a term up: wp define TERM)", args[0])
	}
}

var glossaryLsCmd = &command{
	name: "ls", args: "[TEXT] [FLAGS]", aliases: []string{"list", "search"},
	summary: "the scope's full glossary, or search it",
	about: `TEXT searches terms, aliases and definitions. -p global lists the global
glossary.`,
	examples: []string{"wp glossary ls", "wp glossary ls lease", "wp glossary ls --own"},
	run:      glossaryLs,
}

func glossaryLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	own := fs.Bool("own", false, "only the project's own terms")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return err
	}
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if *own {
		q.Set("own", "1")
	}
	if len(pos) > 0 {
		q.Set("q", joinArgs(pos))
	}
	return c.textCall("/api/v1/glossary", q)
}

var glossaryShowCmd = &command{
	name: "show", args: "TERM[, TERM...]", aliases: []string{"view", "get"},
	summary:  "an entry, with its links and author",
	examples: []string{"wp glossary show claim", "wp glossary show '#4'", "wp glossary show claim, '#4'"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		refs := termList(pos)
		if len(refs) == 0 {
			return c.usage("which term?")
		}
		project, scoped := "", false
		return c.each(refs, true, func(ref string) error {
			if id, ok := numRef(ref); ok {
				return c.textCall(fmt.Sprintf("/api/v1/glossary/%d", id), url.Values{})
			}
			if !scoped {
				if project, err = c.knowledgeScope(sf); err != nil {
					return err
				}
				scoped = true
			}
			return c.lookupTerm(ref, project)
		})
	},
}

// numRef reads "12" or "#12".
func numRef(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	return n, err == nil && n > 0
}

// termID finds a glossary entry's id by #ID, term or alias, nearest scope
// first.
func (c *cli) termID(ref string, sf scopeFlags) (int64, error) {
	if id, ok := numRef(ref); ok {
		return id, nil
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return 0, err
	}
	q := url.Values{"term": {ref}}
	if project != "" {
		q.Set("project", project)
	}
	var res struct {
		Matches []struct {
			ID int64 `json:"id"`
		} `json:"matches"`
		Suggestions []struct {
			Term string `json:"term"`
		} `json:"suggestions"`
	}
	if err := c.do("GET", "/api/v1/glossary/lookup?"+q.Encode(), nil, &res); err != nil {
		return 0, err
	}
	if len(res.Matches) == 0 {
		msg := fmt.Sprintf("no glossary entry %q here", ref)
		if len(res.Suggestions) > 0 {
			var names []string
			for _, s := range res.Suggestions {
				names = append(names, s.Term)
			}
			msg += " (close: " + strings.Join(names, ", ") + ")"
		}
		return 0, fmt.Errorf("%s", msg)
	}
	return res.Matches[0].ID, nil
}

type glossaryInfo struct {
	ID         int64    `json:"id"`
	Term       string   `json:"term"`
	ProjectKey string   `json:"project_key"`
	Locked     bool     `json:"locked"`
	Aliases    []string `json:"aliases"`
}

func (g glossaryInfo) line() string {
	where := g.ProjectKey
	if where == "" {
		where = "global"
	}
	s := fmt.Sprintf("term #%d %s (%s)", g.ID, g.Term, where)
	if g.Locked {
		s += " [locked]"
	}
	return s
}

var glossaryAddCmd = &command{
	name: "add", args: "TERM [DEFINITION] [FLAGS]", aliases: []string{"new", "create"},
	summary: "add a term",
	about: `The definition is the second argument, -d, or -f FILE. -p global is for
admins.`,
	examples: []string{
		`wp glossary add Claim "An agent's 2-hour lease on a task." --alias lease --link internal/core/agentwork.go`,
	},
	run: glossaryAdd,
}

func glossaryAdd(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	def := fs.String("desc", "", "the definition (`MARKDOWN`)")
	short(fs, "d", "desc")
	alias(fs, "definition", "desc")
	file := fs.String("file", "", "read the definition from `FILE` (- for stdin)")
	short(fs, "f", "file")
	var aliases, avoid, links listFlag
	fs.Var(&aliases, "alias", "another `NAME` for it (repeatable)")
	fs.Var(&avoid, "avoid", "a `WORD` this project avoids (repeatable)")
	fs.Var(&links, "link", "link to a `REF` (see glossary link; repeatable)")
	lock := fs.Bool("lock", false, "lock against agents (people only)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("give the term")
	}
	term := pos[0]
	if len(pos) > 1 && *def == "" {
		*def = joinArgs(pos[1:])
	}
	if *file != "" {
		if *def, err = readBody(*file); err != nil {
			return err
		}
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return err
	}
	if project == "" {
		return c.usage("which glossary? Use -p PROJECT or -p global")
	}
	if project == "global" {
		project = ""
	}
	in := map[string]any{"term": term, "definition": *def, "project": project, "aliases": []string(aliases), "links": []string(links)}
	if len(avoid) > 0 {
		in["avoid"] = []string(avoid)
	}
	if *lock {
		in["locked"] = true
	}
	raw, err := c.call("POST", "/api/v1/glossary", in)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var g glossaryInfo
		_ = jsonInto(raw, &g)
		return "Added " + g.line()
	})
}

var glossaryEditCmd = &command{
	name: "edit", args: "TERM [FLAGS]", aliases: []string{"update", "set"},
	summary: "change a term",
	about: `--alias and --avoid replace the whole list; "" clears it. Agents can't edit
locked entries.`,
	examples: []string{
		`wp glossary edit claim -d "An agent's lease on a task."`,
		"wp glossary edit claim --alias lease --alias lock",
	},
	run: glossaryEdit,
}

func glossaryEdit(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	term := fs.String("term", "", "rename it to `TERM`")
	def := fs.String("desc", "", "replace the definition with this `MARKDOWN`")
	short(fs, "d", "desc")
	alias(fs, "definition", "desc")
	file := fs.String("file", "", "replace the definition from `FILE` (- for stdin)")
	short(fs, "f", "file")
	var aliases, avoid listFlag
	fs.Var(&aliases, "alias", "replace the aliases with `NAME` (repeatable)")
	fs.Var(&avoid, "avoid", "replace the words to avoid with `WORD` (repeatable)")
	move := fs.String("move", "", "move to `PROJECT`'s glossary, or global")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which term?")
	}
	id, err := c.termID(joinArgs(pos), sf)
	if err != nil {
		return err
	}
	in := map[string]any{}
	if *term != "" {
		in["term"] = *term
	}
	if *file != "" {
		if *def, err = readBody(*file); err != nil {
			return err
		}
		in["definition"] = *def
	} else if *def != "" {
		in["definition"] = *def
	}
	for key, given := range map[string]listFlag{"aliases": aliases, "avoid": avoid} {
		if len(given) > 0 {
			list := []string{}
			for _, a := range given {
				if a != "" {
					list = append(list, a)
				}
			}
			in[key] = list
		}
	}
	if *move != "" {
		if strings.EqualFold(*move, "global") {
			in["project"] = ""
		} else {
			in["project"] = *move
		}
	}
	if len(in) == 0 {
		return c.usage("nothing to change; see wp glossary edit -h")
	}
	raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/glossary/%d", id), in)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var g glossaryInfo
		_ = jsonInto(raw, &g)
		return "Saved " + g.line()
	})
}

func glossaryLockTo(locked bool) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which term?")
		}
		id, err := c.termID(joinArgs(pos), sf)
		if err != nil {
			return err
		}
		raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/glossary/%d", id), map[string]any{"locked": locked})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var g glossaryInfo
			_ = jsonInto(raw, &g)
			if locked {
				return "Locked " + g.line() + ": agents can't change or delete it."
			}
			return "Unlocked " + g.line()
		})
	}
}

var glossaryLockCmd = &command{
	name: "lock", args: "TERM",
	summary:  "stop agents editing an entry (people only)",
	examples: []string{"wp glossary lock claim"},
	run:      glossaryLockTo(true),
}

var glossaryUnlockCmd = &command{
	name: "unlock", args: "TERM",
	summary:  "let agents edit an entry again (people only)",
	examples: []string{"wp glossary unlock claim"},
	run:      glossaryLockTo(false),
}

var glossaryLinkCmd = &command{
	name: "link", args: "TERM REF... [--rel REL]",
	summary: "link an entry to issues, pages, notes, files, URLs",
	about: `A REF is an issue key, page:4, design:12, glossary:Term, a file path
(internal/core/agentwork.go:120) or a URL. See wp unlink to remove.`,
	examples: []string{"wp glossary link claim internal/core/agentwork.go APP-12"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		rel := fs.String("rel", "", "`RELATION`: relates (default), implements, depends_on, supersedes, tension")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return c.usage("give the term and at least one thing to link")
		}
		id, err := c.termID(pos[0], sf)
		if err != nil {
			return err
		}
		return c.addLinks(map[string]any{"type": "glossary", "id": id}, pos[1:], *rel)
	},
}

// addLinks links from an item to each ref and prints one line per link.
func (c *cli) addLinks(from map[string]any, refs []string, rel string) error {
	var out []string
	var raws []string
	for _, r := range refs {
		raw, err := c.call("POST", "/api/v1/links", map[string]any{"from": from, "to": map[string]any{"ref": r}, "rel": rel})
		if err != nil {
			return fmt.Errorf("linking %s: %w", r, err)
		}
		raws = append(raws, string(raw))
		var l struct {
			ID   int64  `json:"id"`
			Rel  string `json:"rel"`
			Item struct {
				Title string `json:"title"`
				Key   string `json:"key"`
				Type  string `json:"type"`
			} `json:"item"`
		}
		_ = jsonInto(raw, &l)
		name := l.Item.Title
		if l.Item.Key != "" {
			name = l.Item.Key + " " + name
		}
		out = append(out, fmt.Sprintf("Linked (%s) %s %s (link #%d)", strings.ReplaceAll(cmpStr(l.Rel, "relates"), "_", " "), l.Item.Type, name, l.ID))
	}
	if c.asJSON {
		c.println("[" + strings.Join(raws, ",") + "]")
		return nil
	}
	c.println(strings.Join(out, "\n"))
	return nil
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var glossaryRmCmd = &command{
	name: "rm", args: "TERM [--yes]", aliases: []string{"delete", "remove"},
	summary:  "delete an entry (agents: not locked ones)",
	examples: []string{"wp glossary rm \"old term\" --yes"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		yes := fs.Bool("yes", false, "don't ask")
		short(fs, "y", "yes")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which term?")
		}
		id, err := c.termID(joinArgs(pos), sf)
		if err != nil {
			return err
		}
		if err := c.confirm(*yes, fmt.Sprintf("Delete glossary entry #%d for good?", id)); err != nil {
			return err
		}
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/glossary/%d", id), nil); err != nil {
			return err
		}
		c.println(fmt.Sprintf("Deleted glossary entry #%d.", id))
		return nil
	},
}

var unlinkCmd = &command{
	name: "unlink", group: "Knowledge", args: "LINK-ID... | KEY URL|#N",
	summary: "remove a glossary/design link by number, or an issue's forge link",
	about: `Link numbers are printed by glossary/design link and show. [[wikilinks]]
go when the text drops them; agents can only remove links agents made. With
an issue KEY and a URL (or #N, this repo's pull request N), removes that
forge link; as wp pr unlink does, it puts back an issue moved to In Review.`,
	examples: []string{"wp unlink 31", "wp unlink WP-85 #71"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which link? Give its number, or an issue KEY and a URL")
		}
		if keyRE.MatchString(strings.ToUpper(pos[0])) {
			if len(pos) != 2 {
				return c.usage("expected an issue KEY and the link's URL (or #N for this repo's pull request N)")
			}
			return c.unlinkForge(pos[0], pos[1])
		}
		for _, p := range pos {
			id, ok := numRef(p)
			if !ok {
				return c.usage("%q isn't a link number", p)
			}
			if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/links/%d", id), nil); err != nil {
				return err
			}
			c.println(fmt.Sprintf("Removed link #%d.", id))
		}
		return nil
	},
}

var specCmd = &command{
	name: "spec", group: "Knowledge", args: "[TOPIC] [FLAGS]",
	summary: "what people intend for a feature, screen or decision",
	about: `Prints the design note TOPIC names (#ID, code like ADR7, title or best
match) with its additions, sub-notes, related notes, terms, tasks and PRs.
No TOPIC: the goals and principles in full, and an index of the rest.
Agents: read the spec before changing what it covers; follow approved
intent, treat unapproved parts as suggestions.`,
	examples: []string{"wp spec", "wp spec \"issue page\"", "wp spec ADR7"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		project, err := c.knowledgeScope(sf)
		if err != nil {
			return err
		}
		q := url.Values{}
		if project != "" {
			q.Set("project", project)
		}
		if len(pos) > 0 {
			q.Set("topic", joinArgs(pos))
		}
		return c.textCall("/api/v1/spec", q)
	},
}

var designCmd = &command{
	name: "design", group: "Knowledge", args: "SUBCOMMAND",
	aliases: []string{"designs", "decision", "decisions"},
	summary: "design notes: goals, features, screens, decisions",
	about: `Notes nest and link to issues, terms, pages, files and PRs. Decisions
(ADRs) are notes of kind decision, coded ADR7; plans and experiments are
pages. A NOTE is its number (12, #12) or code (ADR7).

People's notes count as approved. Agents' notes are proposals until a
person approves; on an approved note an agent can only add (design add) or
propose an edit (design edit), for a person to review. A decision written
with --for TASK counts on the request of the person who filed that task:
it shows as written for it, never as approved by them.

Kinds: ` + strings.Join(designKinds, ", ") + `. Status: active, superseded,
retired.`,
	examples: []string{
		`wp design new decision "Use SQLite" -d "One file to back up." --for WP-177`,
		"wp design show ADR7",
		"wp design ls --kind feature",
		`wp design add 12 "Mobile: the sidebar goes under the description."`,
	},
}

func init() {
	designCmd.subs = []*command{designLsCmd, designShowCmd, designNewCmd, designEditCmd, designAddCmd, designLinkCmd,
		designReviewCmd, designApproveCmd, designRejectCmd, designMergeCmd, designRmCmd}
	designCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return designLs(c, args)
		}
		return c.usage("no design command %q (for a note's spec: wp spec TOPIC)", args[0])
	}
}

var designLsCmd = &command{
	name: "ls", args: "[TEXT] [FLAGS]", aliases: []string{"list", "search"},
	summary:  "list design notes, with parents' and global ones",
	about:    `Active notes by kind. TEXT searches titles, bodies and additions.`,
	examples: []string{"wp design ls --kind decision", "wp design ls board", "wp design ls --pending"},
	run:      designLs,
}

func designLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	kind := fs.String("kind", "", "only this `KIND`: "+strings.Join(designKinds, ", "))
	archived := fs.Bool("archived", false, "superseded and retired instead")
	every := fs.Bool("every", false, "notes in any status")
	pending := fs.Bool("pending", false, "only notes awaiting a person")
	own := fs.Bool("own", false, "only the project's own notes")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{"project": project, "kind": *kind, "q": joinArgs(pos)} {
		if v != "" {
			q.Set(k, v)
		}
	}
	switch {
	case *every:
		q.Set("status", "all")
	case *archived:
		q.Set("status", "archived")
	}
	if *pending {
		q.Set("pending", "1")
	}
	if *own {
		q.Set("own", "1")
	}
	return c.textCall("/api/v1/design", q)
}

// noteID finds a design note by number or decision code (ADR7, ADR-0007,
// APP-ADR7: a bare code is looked up in this repo's project first).
func (c *cli) noteID(s string) (int64, error) {
	s = strings.TrimPrefix(s, "design:")
	if id, ok := numRef(s); ok {
		return id, nil
	}
	if !designCodeRE.MatchString(strings.TrimSpace(s)) {
		return 0, fmt.Errorf("%q isn't a design note number or code like ADR7 (wp design ls lists them)", s)
	}
	path := "/api/v1/design/" + esc(strings.TrimSpace(s))
	if home := c.homeProject(); home != "" {
		path += "?project=" + url.QueryEscape(home)
	}
	var d designInfo
	if err := c.do("GET", path, nil, &d); err != nil {
		return 0, err
	}
	return d.ID, nil
}

// designCodeRE is a decision code: ADR7, ADR-0007, APP-ADR7.
var designCodeRE = lazyre.New(`(?i)^(?:[A-Z][A-Z0-9]{1,9}-?)?ADR-?0*[0-9]{1,9}$`)

var designShowCmd = &command{
	name: "show", args: "NOTE...", aliases: []string{"view", "get"},
	summary:  "a note: body, additions, sub-notes, links, authors",
	examples: []string{"wp design show 12", "wp design show ADR7 ADR9"},
	run:      designShow,
}

func designShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which note? Give its number")
	}
	return c.each(listArgs(pos), true, func(ref string) error {
		id, err := c.noteID(ref)
		if err != nil {
			return err
		}
		return c.textCall(fmt.Sprintf("/api/v1/design/%d", id), url.Values{})
	})
}

type designInfo struct {
	ID       int64  `json:"id"`
	Code     string `json:"code"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Approved bool   `json:"approved"`
	Notice   string `json:"notice"`
	Pending  int    `json:"pending"`
}

func (d designInfo) line() string {
	s := fmt.Sprintf("design #%d %s [%s]", d.ID, d.Title, d.Kind)
	if d.Code != "" {
		s = fmt.Sprintf("design %s #%d %s [%s]", d.Code, d.ID, d.Title, d.Kind)
	}
	if !d.Approved {
		s += " (proposed)"
	}
	if d.Notice != "" {
		s += "\n" + d.Notice
	}
	return s
}

var designNewCmd = &command{
	name: "new", args: "[KIND] TITLE [FLAGS]", aliases: []string{"create"},
	summary: "write a design note (agents: a proposal)",
	about: `KIND (default note) comes first or via --kind; the body from -d or -f FILE.
A title starting "ADR-0007: " takes that decision number if free.
--for TASK links the note to the task and, when a person filed it,
accepts the note on their request; otherwise it stays a proposal.`,
	examples: []string{
		`wp design new feature "Board view" -f board.md --link APP-12`,
		`wp design new decision "Quiet notices" -f adr.md --for WP-176`,
	},
	run: designNew,
}

func designNew(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	body := fs.String("desc", "", "the body (`MARKDOWN`)")
	short(fs, "d", "desc")
	alias(fs, "body", "desc")
	file := fs.String("file", "", "read the body from `FILE` (- for stdin)")
	short(fs, "f", "file")
	kind := fs.String("kind", "", "`KIND`: "+strings.Join(designKinds, ", "))
	parent := fs.String("parent", "", "put it under this `NOTE`")
	code := fs.String("code", "", "give a decision this `CODE` (ADR12)")
	forTask := fs.String("for", "", "the `TASK` (WP-177) it is written for")
	var links listFlag
	fs.Var(&links, "link", "link to a `REF` (see design link; repeatable)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) >= 2 && *kind == "" {
		for _, k := range append(designKinds, "page", "view") {
			if strings.EqualFold(pos[0], k) {
				*kind, pos = strings.ToLower(pos[0]), pos[1:]
				break
			}
		}
	}
	if len(pos) == 0 {
		return c.usage("give a title")
	}
	if *file != "" {
		if *body, err = readBody(*file); err != nil {
			return err
		}
	}
	in := map[string]any{"title": joinArgs(pos), "kind": *kind, "body": *body, "links": []string(links)}
	if *code != "" {
		in["code"] = *code
	}
	if *forTask != "" {
		in["for"] = *forTask
	}
	if *parent != "" {
		id, err := c.noteID(*parent)
		if err != nil {
			return err
		}
		in["parent"] = id
	} else {
		project, err := c.knowledgeScope(sf)
		if err != nil {
			return err
		}
		if project == "" {
			return c.usage("which project? Use -p PROJECT or -p global")
		}
		if project == "global" {
			project = ""
		}
		in["project"] = project
	}
	raw, err := c.call("POST", "/api/v1/design", in)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var d designInfo
		_ = jsonInto(raw, &d)
		return "Created " + d.line()
	})
}

var designEditCmd = &command{
	name: "edit", args: "NOTE [FLAGS]", aliases: []string{"update", "set"},
	summary: "change a note (agents: proposes the edit if approved)",
	about: `The body is replaced whole. An agent's title or body edit to an approved
note becomes a proposal; agents can't change its kind, status, parent or
project.`,
	examples: []string{"wp design edit 12 -f board.md", "wp design edit 7 --status superseded"},
	run:      designEdit,
}

func designEdit(c *cli, args []string) error {
	fs := c.flags()
	title := fs.String("title", "", "new `TITLE`")
	body := fs.String("desc", "", "replace the body with this `MARKDOWN`")
	short(fs, "d", "desc")
	alias(fs, "body", "desc")
	file := fs.String("file", "", "replace the body from `FILE` (- for stdin)")
	short(fs, "f", "file")
	kind := fs.String("kind", "", "new `KIND`")
	status := fs.String("status", "", "`STATUS`: active, superseded, retired")
	parent := fs.String("parent", "", "move under `NOTE` (none: top level)")
	move := fs.String("move", "", "move with sub-notes to `PROJECT`, or global")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("which note? Give its number")
	}
	id, err := c.noteID(pos[0])
	if err != nil {
		return err
	}
	in := map[string]any{}
	if *title != "" {
		in["title"] = *title
	}
	if *file != "" {
		if *body, err = readBody(*file); err != nil {
			return err
		}
		in["body"] = *body
	} else if *body != "" {
		in["body"] = *body
	}
	for k, v := range map[string]string{"kind": *kind, "status": *status} {
		if v != "" {
			in[k] = v
		}
	}
	switch {
	case strings.EqualFold(*parent, "none"):
		in["parent"] = nil
	case *parent != "":
		pid, err := c.noteID(*parent)
		if err != nil {
			return err
		}
		in["parent"] = pid
	}
	if *move != "" {
		if strings.EqualFold(*move, "global") {
			in["project"] = ""
		} else {
			in["project"] = *move
		}
	}
	if len(in) == 0 {
		return c.usage("nothing to change; see wp design edit -h")
	}
	raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/design/%d", id), in)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var d designInfo
		_ = jsonInto(raw, &d)
		if d.Notice != "" {
			return fmt.Sprintf("Proposed an edit to design #%d %s.\n%s", d.ID, d.Title, d.Notice)
		}
		return "Saved " + d.line()
	})
}

var designAddCmd = &command{
	name: "add", args: "NOTE TEXT | -f FILE", aliases: []string{"append", "addition"},
	summary: "append to a note (agents: awaits approval)",
	about: `Kept as a separate addition with author and date; an agent's is
unapproved until a person approves, rejects or merges it.`,
	examples: []string{`wp design add 12 "Decided: no swimlanes on mobile."`},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		file := fs.String("file", "", "read the text from `FILE` (- for stdin)")
		short(fs, "f", "file")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which note?")
		}
		id, err := c.noteID(pos[0])
		if err != nil {
			return err
		}
		text := joinArgs(pos[1:])
		if *file != "" {
			if text, err = readBody(*file); err != nil {
				return err
			}
		}
		if strings.TrimSpace(text) == "" {
			return c.usage("add what? Give the text or -f FILE")
		}
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/design/%d/additions", id), map[string]any{"body": text})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var d designInfo
			_ = jsonInto(raw, &d)
			s := fmt.Sprintf("Added to design #%d %s.", d.ID, d.Title)
			if d.Notice != "" {
				s += "\n" + d.Notice
			}
			return s
		})
	},
}

var designLinkCmd = &command{
	name: "link", args: "NOTE REF... [--rel REL]",
	summary: "link a note to issues, terms, pages, notes, files, PRs",
	about: `A REF is an issue key, design:12, glossary:Term, page:4, a file path or a
URL.`,
	examples: []string{"wp design link 12 APP-40 glossary:Board", "wp design link 9 design:4 --rel supersedes"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		rel := fs.String("rel", "", "`RELATION`: relates (default), implements, depends_on, supersedes, tension")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return c.usage("give the note and at least one thing to link")
		}
		id, err := c.noteID(pos[0])
		if err != nil {
			return err
		}
		return c.addLinks(map[string]any{"type": "design", "id": id}, pos[1:], *rel)
	},
}

var designReviewCmd = &command{
	name: "review", args: "[FLAGS]", aliases: []string{"queue", "pending"},
	summary: "what awaits a person: proposed notes, additions, edits",
	about: `Lists proposed notes (#ID) and pending additions and edits (rID), oldest
first. Act on them with design approve, reject and merge.`,
	examples: []string{"wp design review -A"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return c.usage("unexpected %q", joinArgs(pos))
		}
		project, err := c.knowledgeScope(sf)
		if err != nil {
			return err
		}
		q := url.Values{}
		if project != "" {
			q.Set("project", project)
		}
		raw, err := c.call("GET", "/api/v1/design/review?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var items []struct {
				Note struct {
					ID    int64  `json:"id"`
					Title string `json:"title"`
					Kind  string `json:"kind"`
				} `json:"note"`
				Revision *struct {
					ID   int64  `json:"id"`
					Kind string `json:"kind"`
					Body string `json:"body"`
				} `json:"revision"`
				Agent     string `json:"agent"`
				CreatedAt string `json:"created_at"`
			}
			if err := jsonInto(raw, &items); err != nil {
				return string(raw)
			}
			if len(items) == 0 {
				return "Nothing waits for review."
			}
			var b strings.Builder
			for _, it := range items {
				when := it.CreatedAt
				if len(when) >= 10 {
					when = when[:10]
				}
				by := it.Agent
				if by == "" {
					by = "someone"
				}
				if it.Revision == nil {
					fmt.Fprintf(&b, "#%d  new %s %q, proposed by %s %s\n", it.Note.ID, it.Note.Kind, it.Note.Title, by, when)
					continue
				}
				fmt.Fprintf(&b, "r%d  %s to #%d %q by %s %s\n", it.Revision.ID, it.Revision.Kind, it.Note.ID, it.Note.Title, by, when)
				if it.Revision.Kind == "addition" {
					b.WriteString("     " + firstLine(it.Revision.Body) + "\n")
				}
			}
			return b.String()
		})
	},
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

// review approves, rejects or merges a revision (rID) or approves a
// proposed note (#ID).
func designVerdict(action string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		note := fs.String("message", "", "a note for the record (`TEXT`)")
		short(fs, "m", "message")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which? Give rID (an addition or edit) or #ID (a proposed note); wp design review lists them")
		}
		for _, ref := range pos {
			var path string
			if r, ok := strings.CutPrefix(strings.ToLower(ref), "r"); ok && !designCodeRE.MatchString(ref) {
				id, ok := numRef(r)
				if !ok {
					return c.usage("%q isn't a revision like r12", ref)
				}
				path = fmt.Sprintf("/api/v1/design/revisions/%d/%s", id, action)
			} else {
				if action != "approve" {
					return c.usage("only additions and edits (rID) can be %sed; to drop a proposed note, wp design rm", strings.TrimSuffix(action, "e"))
				}
				id, err := c.noteID(ref)
				if err != nil {
					return err
				}
				path = fmt.Sprintf("/api/v1/design/%d/approve", id)
			}
			raw, err := c.call("POST", path, map[string]any{"note": *note})
			if err != nil {
				return err
			}
			if err := c.emit(raw, func() string {
				var d designInfo
				_ = jsonInto(raw, &d)
				return fmt.Sprintf("%s: %s", map[string]string{"approve": "Approved", "reject": "Rejected", "merge": "Merged"}[action], d.line())
			}); err != nil {
				return err
			}
		}
		return nil
	}
}

var designApproveCmd = &command{
	name: "approve", args: "rID|#ID... [-m NOTE]", aliases: []string{"accept"},
	summary:  "approve an agent's addition, edit or note (people only)",
	examples: []string{"wp design approve r31", "wp design approve 14"},
	run:      designVerdict("approve"),
}

var designRejectCmd = &command{
	name: "reject", args: "rID... [-m WHY]",
	summary:  "reject an agent's addition or edit (people only; kept in history)",
	examples: []string{`wp design reject r32 -m "We decided against swimlanes."`},
	run:      designVerdict("reject"),
}

var designMergeCmd = &command{
	name: "merge", args: "rID...",
	summary:  "fold an addition into the note's body (people only)",
	examples: []string{"wp design merge r31"},
	run:      designVerdict("merge"),
}

var designRmCmd = &command{
	name: "rm", args: "NOTE [--yes]", aliases: []string{"delete", "remove"},
	summary: "delete a note and its history (people only)",
	about: `Sub-notes move to the top. To keep the record, use wp design edit NOTE
--status retired instead.`,
	examples: []string{"wp design rm 14 --yes"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		yes := fs.Bool("yes", false, "don't ask")
		short(fs, "y", "yes")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which note?")
		}
		id, err := c.noteID(pos[0])
		if err != nil {
			return err
		}
		if err := c.confirm(*yes, fmt.Sprintf("Delete design note #%d and its history for good?", id)); err != nil {
			return err
		}
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/design/%d", id), nil); err != nil {
			return err
		}
		c.println(fmt.Sprintf("Deleted design note #%d.", id))
		return nil
	},
}
