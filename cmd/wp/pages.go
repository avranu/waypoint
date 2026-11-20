package main

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// pageCategories is what the server accepts (a test checks); new pages
// without one are notes.
var pageCategories = []string{"plan", "idea", "experiment", "reference", "note", "decision", "problem", "feature", "bug"}

const defaultPageCategory = "note"

var pageCmd = &command{
	name: "page", group: "Pages", args: "SUBCOMMAND",
	aliases: []string{"pages", "doc", "docs"},
	summary: "pages: plans, ideas, experiments and notes",
	about: `Nestable markdown documents, linked to issues, in a project or personal.
Decisions are design notes (wp design new decision), not pages.

Categories: ` + strings.Join(pageCategories, ", ") + `.
A page's code is its category prefix and a number per project (PL13;
APP-PL13 from elsewhere). Prefixes: ` + prefixLegend() + `.
A PAGE is a code, a number (7 or #7) or an exact title.`,
	examples: []string{
		`wp page new "Parser plan" --category plan -f plan.md --issue APP-12`,
		"wp page show PL13",
	},
}

func init() {
	pageCmd.subs = []*command{pageLsCmd, pageShowCmd, pageNewCmd, pageEditCmd, pagePublishCmd, pageLinkCmd, pageUnlinkCmd, pageRmCmd, pageCodesCmd}
}

// pagePrefixes are the code prefixes of pageCategories (a test checks
// they match the server's).
var pagePrefixes = map[string]string{
	"plan": "PL", "idea": "I", "experiment": "X", "reference": "R", "note": "N",
	"decision": "D", "problem": "P", "feature": "F", "bug": "B",
}

func prefixLegend() string {
	var parts []string
	for _, c := range pageCategories {
		parts = append(parts, pagePrefixes[c]+" "+c)
	}
	return strings.Join(parts, ", ")
}

// pageCodeRE matches what can be a page code: PL13, APP-PL13, APPPL13 (any
// case); the server decides which page, if any, it is.
var pageCodeRE = lazyre.New(`^(?:[A-Za-z][A-Za-z0-9]{1,9}-?)?[A-Za-z]{1,2}[0-9]{1,9}$`)

type pageInfo struct {
	ID         int64  `json:"id"`
	Code       string `json:"code"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Category   string `json:"category"`
	ProjectKey string `json:"project_key"`
	ChildCount int    `json:"child_count"`
	Draft      bool   `json:"draft"`
	Notice     string `json:"notice"`
	Ancestors  []struct {
		Title string `json:"title"`
	} `json:"ancestors"`
	Children []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"children"`
	Issues []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"issues"`
}

func (p pageInfo) line() string {
	s := fmt.Sprintf("#%d %s", p.ID, p.Title)
	if p.Code != "" {
		s = p.Code + " " + s
	}
	if p.Category != "" {
		s += " [" + p.Category + "]"
	}
	if p.Draft {
		s += " draft"
	}
	if p.ProjectKey != "" {
		s += " project:" + p.ProjectKey
	} else {
		s += " (personal)"
	}
	if p.ChildCount > 0 {
		s += fmt.Sprintf(" (%d sub-pages)", p.ChildCount)
	}
	return s
}

var pageLsCmd = &command{
	name: "ls", args: "[TEXT] [FLAGS]", aliases: []string{"list", "search"},
	summary:  "list pages in this repo's project, or search them",
	about:    `Newest first. TEXT searches titles and bodies.`,
	examples: []string{"wp page ls parser", "wp page ls --category plan --issue APP-12"},
	run:      pageLs,
}

func pageLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	personal := fs.Bool("personal", false, "only your personal pages (-p none)")
	category := fs.String("category", "", "only this `CATEGORY`")
	issue := fs.String("issue", "", "only pages linked to this issue `KEY`")
	text := fs.String("q", "", "(hidden) the old way to give TEXT")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *text != "" {
		pos = append(pos, *text)
	}
	q := url.Values{}
	switch {
	case *personal || strings.EqualFold(*sf.project, "none"):
		q.Set("project", "none")
	default:
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		if !sc.all() {
			home, err := sc.home()
			if err != nil {
				return err
			}
			q.Set("project", home)
			c.note("Pages in %s, from %s.", home, sc.from)
		}
	}
	for k, v := range map[string]string{"category": *category, "q": joinArgs(pos), "issue": strings.ToUpper(*issue)} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !c.asJSON {
		q.Set("format", "text") // the server's lines say who wrote each page
	}
	raw, err := c.call("GET", "/api/v1/pages?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func pageID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q isn't a page number", s)
	}
	return id, nil
}

// pageRef finds a page by number, code (in the scope's project first) or
// exact title (among pages you can see).
func (c *cli) pageRef(ref string) (int64, error) {
	if id, err := pageID(ref); err == nil {
		return id, nil
	}
	if pageCodeRE.MatchString(ref) {
		var p pageInfo
		path := "/api/v1/pages/" + esc(strings.ToUpper(ref))
		if home := c.homeProject(); home != "" {
			path += "?project=" + url.QueryEscape(home)
		}
		err := c.do("GET", path, nil, &p)
		var ae *apiError
		if err == nil {
			return p.ID, nil
		} else if !errors.As(err, &ae) || (ae.status != 400 && ae.status != 404) {
			return 0, err
		}
		// Not a code after all: try it as a title, else say what was wrong.
		if id, terr := c.pageByTitle(ref); terr == nil {
			return id, nil
		}
		return 0, err
	}
	return c.pageByTitle(ref)
}

// pageByTitle finds a page by its exact title (among pages you can see).
func (c *cli) pageByTitle(ref string) (int64, error) {
	var pages []pageInfo
	if err := c.do("GET", "/api/v1/pages?q="+url.QueryEscape(ref), nil, &pages); err != nil {
		return 0, err
	}
	var hits []pageInfo
	for _, p := range pages {
		if strings.EqualFold(p.Title, ref) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].ID, nil
	case 0:
		return 0, fmt.Errorf("no page titled %q; wp page ls %s finds similar ones", ref, ref)
	}
	var opts []string
	for _, p := range hits {
		opts = append(opts, p.line())
	}
	return 0, fmt.Errorf("several pages are titled %q; use the number:\n  %s", ref, strings.Join(opts, "\n  "))
}

var pageShowCmd = &command{
	name: "show", args: "PAGE...", aliases: []string{"view", "cat"},
	summary:  "show pages",
	examples: []string{"wp page show PL13 7", `wp page show "Parser plan"`},
	run:      pageShow,
}

func pageShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which page? Give its number or title")
	}
	// Several codes (PL13 PL14, PL13,PL14) are several pages; other words
	// are one page's title.
	refs := listArgs(pos)
	for _, r := range refs {
		if !pageCodeRE.MatchString(r) && !isDigits(r) {
			refs = []string{joinArgs(pos)}
			break
		}
	}
	return c.each(refs, true, func(ref string) error {
		id, err := c.pageRef(ref)
		if err != nil {
			return err
		}
		path := fmt.Sprintf("/api/v1/pages/%d", id)
		if !c.asJSON {
			path += "?format=text" // with who wrote it and whether a person reviewed it
		}
		raw, err := c.call("GET", path, nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return c.absoluteAttachmentLinks(string(raw)) })
	})
}

// homeProject is the scope's project, for codes without one; "" when the
// scope has none (or several).
func (c *cli) homeProject() string {
	sc, err := c.implicitScope()
	if err != nil || sc.all() {
		return ""
	}
	home, err := sc.home()
	if err != nil {
		return ""
	}
	return home
}

var pageNewCmd = &command{
	name: "new", args: `[CATEGORY] "TITLE" [FLAGS]`, aliases: []string{"create", "add"},
	summary: "write a new page",
	about: `CATEGORY, if the first of two or more words, is one of: ` + strings.Join(pageCategories, ", ") + `
(default ` + defaultPageCategory + `; pick the one that fits). The body is markdown from -d or -f
(- is stdin). Decisions belong in design notes (wp design new decision).`,
	examples: []string{
		`wp page new idea "Half a thought" --draft`,
		`wp page new "Parser plan" --category plan -f plan.md --issue APP-12`,
	},
	run: pageNew,
}

func pageNew(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "put it in this `PROJECT`")
	short(fs, "p", "project")
	personal := fs.Bool("personal", false, "personal: only you see it (-p none)")
	body := fs.String("desc", "", "the body (`MARKDOWN`)")
	short(fs, "d", "desc")
	alias(fs, "body", "desc")
	file := fs.String("file", "", "read the body from `FILE` (- for stdin)")
	short(fs, "f", "file")
	alias(fs, "body-file", "file")
	category := fs.String("category", "", "`CATEGORY` (default "+defaultPageCategory+")")
	parent := fs.String("parent", "", "nest under this `PAGE`")
	code := fs.String("code", "", "give it this `CODE` (PL13); sets the category")
	draft := fs.Bool("draft", false, "a draft, hidden from agents")
	var issues listFlag
	fs.Var(&issues, "issue", "link to issue `KEY` (repeat)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) >= 2 && slices.Contains(pageCategories, strings.ToLower(pos[0])) && *category == "" {
		*category, pos = strings.ToLower(pos[0]), pos[1:]
	}
	if len(pos) == 0 {
		return c.usage("give a title")
	}
	in := map[string]any{"title": joinArgs(pos)}
	if *code != "" {
		in["code"] = strings.ToUpper(*code)
	}
	if *draft {
		in["draft"] = true
	}
	if *category == "" && *code == "" {
		*category = defaultPageCategory
	}
	if *category != "" {
		if !slices.Contains(pageCategories, strings.ToLower(*category)) {
			return c.usage("category is one of %s", strings.Join(pageCategories, ", "))
		}
		in["category"] = strings.ToLower(*category)
	}
	if *file != "" {
		if *body, err = readBody(*file); err != nil {
			return err
		}
	}
	if *body != "" {
		in["body"] = *body
	}
	if !*personal && !strings.EqualFold(*project, "none") {
		sc, err := c.scopeFor(scopeFlags{project: project})
		if err != nil {
			return err
		}
		if !sc.all() {
			if in["project"], err = sc.home(); err != nil {
				return err
			}
		}
	}
	if *parent != "" {
		if in["parent"], err = c.pageRef(*parent); err != nil {
			return err
		}
	}
	if len(issues) > 0 {
		up := make([]string, len(issues))
		for i, k := range issues {
			up[i] = strings.ToUpper(k)
		}
		in["issues"] = up
	}
	raw, err := c.call("POST", "/api/v1/pages", in)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var p pageInfo
		_ = jsonInto(raw, &p)
		out := "Created " + p.line()
		if p.Notice != "" {
			out += "\n" + p.Notice
		}
		return out
	})
}

var pageEditCmd = &command{
	name: "edit", args: "PAGE [FLAGS]", aliases: []string{"update"},
	summary: "change a page's title, body or category",
	about:   `The body is replaced whole (-d or -f).`,
	examples: []string{
		`wp page edit PL7 --title "Parser plan v2"`,
		"wp page edit 7 -f plan.md",
	},
	run: pageEdit,
}

func pageEdit(c *cli, args []string) error {
	fs := c.flags()
	title := fs.String("title", "", "new `TITLE`")
	body := fs.String("desc", "", "replace the body (`MARKDOWN`)")
	short(fs, "d", "desc")
	alias(fs, "body", "desc")
	file := fs.String("file", "", "replace the body from `FILE` (- stdin)")
	short(fs, "f", "file")
	alias(fs, "body-file", "file")
	category := fs.String("category", "", "new `CATEGORY`, or none")
	code := fs.String("code", "", "give it this free `CODE`; sets the category")
	drop := fs.String("drop-code", "", "drop an old `CODE` (stays reserved)")
	var link, unlink listFlag
	fs.Var(&link, "issue", "link issue `KEY` (repeat)")
	fs.Var(&unlink, "unlink", "unlink issue `KEY` (repeat)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which page? Give its number or title")
	}
	id, err := c.pageRef(joinArgs(pos))
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
	switch {
	case strings.EqualFold(*category, "none"):
		in["category"] = ""
	case *category != "":
		in["category"] = strings.ToLower(*category)
	}
	if *code != "" {
		in["code"] = strings.ToUpper(*code)
	}
	if len(in) == 0 && len(link) == 0 && len(unlink) == 0 && *drop == "" {
		return c.usage("nothing to change; see wp page edit -h")
	}
	if err := c.linkPage(id, link, unlink); err != nil {
		return err
	}
	method, path := "GET", fmt.Sprintf("/api/v1/pages/%d", id)
	var payload any
	if len(in) > 0 {
		method, payload = "PATCH", in
	}
	raw, err := c.call(method, path, payload)
	if err != nil {
		return err
	}
	if *drop != "" {
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/pages/%d/codes/%s", id, esc(strings.ToUpper(*drop))), nil); err != nil {
			return err
		}
	}
	return c.emit(raw, func() string {
		var p pageInfo
		_ = jsonInto(raw, &p)
		return "Saved " + p.line()
	})
}

func (c *cli) linkPage(id int64, link, unlink []string) error {
	for _, k := range link {
		if _, err := c.call("PUT", fmt.Sprintf("/api/v1/pages/%d/issues/%s", id, esc(strings.ToUpper(k))), nil); err != nil {
			return err
		}
	}
	for _, k := range unlink {
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/pages/%d/issues/%s", id, esc(strings.ToUpper(k))), nil); err != nil {
			return err
		}
	}
	return nil
}

var pageLinkCmd = &command{
	name: "link", args: "PAGE KEY...",
	summary:  "link a page to issues",
	examples: []string{"wp page link 7 APP-12 APP-13"},
	run:      func(c *cli, args []string) error { return pageLinks(c, args, true) },
}

var pageUnlinkCmd = &command{
	name: "unlink", args: "PAGE KEY...",
	summary:  "unlink a page from issues",
	examples: []string{"wp page unlink 7 APP-12"},
	run:      func(c *cli, args []string) error { return pageLinks(c, args, false) },
}

func pageLinks(c *cli, args []string, on bool) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return c.usage("expected a PAGE and issue KEYs")
	}
	id, err := c.pageRef(pos[0])
	if err != nil {
		return err
	}
	keys, verb := pos[1:], "Linked"
	if on {
		err = c.linkPage(id, keys, nil)
	} else {
		err, verb = c.linkPage(id, nil, keys), "Unlinked"
	}
	if err != nil {
		return err
	}
	return c.emit([]byte(`{}`), func() string {
		return fmt.Sprintf("%s page #%d and %s", verb, id, strings.ToUpper(strings.Join(keys, ", ")))
	})
}

var pagePublishCmd = &command{
	name: "publish", args: "PAGE",
	summary: "(people) publish a draft page, so agents can see it",
	about:   `Also publishes the drafts under it. Only a person can.`,
	run:     pagePublish,
}

func pagePublish(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which page?")
	}
	id, err := c.pageRef(joinArgs(pos))
	if err != nil {
		return err
	}
	raw, err := c.call("POST", fmt.Sprintf("/api/v1/pages/%d/publish", id), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var p pageInfo
		_ = jsonInto(raw, &p)
		return "Published " + p.line()
	})
}

var pageRmCmd = &command{
	name: "rm", args: "PAGE [--yes]", aliases: []string{"delete"},
	summary:  "delete a page for good",
	about:    `Deletes a page. Asks first at a terminal; elsewhere it needs --yes.`,
	examples: []string{"wp page rm 7 --yes"},
	run:      pageRm,
}

func pageRm(c *cli, args []string) error {
	fs := c.flags()
	yes := fs.Bool("yes", false, "don't ask")
	short(fs, "y", "yes")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which page?")
	}
	id, err := c.pageRef(joinArgs(pos))
	if err != nil {
		return err
	}
	if err := c.confirm(*yes, fmt.Sprintf("Delete page #%d for good?", id)); err != nil {
		return err
	}
	if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/pages/%d", id), nil); err != nil {
		return err
	}
	return c.emit([]byte(`{}`), func() string { return fmt.Sprintf("Deleted page #%d", id) })
}

var pageCodesCmd = &command{
	name: "codes", args: "[PREFIX=NEXT...]",
	summary: "show or raise the next free page codes",
	about: `PREFIX=NEXT makes new pages start there (owners only; never lower), for
docs whose codes were used elsewhere.`,
	examples: []string{"wp page codes PL=15 D=27"},
	run:      pageCodes,
}

func pageCodes(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	if sc.all() {
		return c.usage("codes are per project: run it in a project's repo or give -p PROJECT")
	}
	home, err := sc.home()
	if err != nil {
		return err
	}
	path := "/api/v1/projects/" + esc(home) + "/page-codes"
	if !c.asJSON {
		path += "?format=text"
	}
	method, body := "GET", map[string]int64{}
	for _, a := range pos {
		k, v, ok := strings.Cut(a, "=")
		n, err := strconv.ParseInt(v, 10, 64)
		if !ok || err != nil {
			return c.usage("expected PREFIX=NEXT, like PL=15, not %q", a)
		}
		body[strings.ToUpper(strings.TrimSpace(k))] = n
	}
	var payload any
	if len(body) > 0 {
		method, payload = "PUT", body
	}
	raw, err := c.call(method, path, payload)
	if err != nil {
		return err
	}
	c.note("Page codes in %s.", home)
	return c.emit(raw, func() string { return string(raw) })
}
