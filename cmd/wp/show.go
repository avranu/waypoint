package main

import (
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// wp show takes any code (WP-237): an issue KEY, a request, a decision
// note, a page or a pull request, and shows it as that thing's own show
// command would. Agents reach for wp show first; refusing R-12 there only
// cost a second call. A code written so it could name two things (R1:
// request R-1 or reference page R1; PL3: page PL3 or issue PL-3) is looked
// up as each, and only when more than one exists does wp show ask which.

// showTarget is one thing a code could name: its kind and the code as
// that kind's command takes it.
type showTarget struct {
	kind, ref string
}

var (
	requestShapeRE = codeShapes[0].re
	pageShapeRE    = codeShapes[2].re
	pullShapeRE    = lazyre.New(`^#\d+$|^([\w.-]+/[\w.-]+)#(\d+)$`)
)

// showPrefix reads a kind prefix (page:PL13, request:R-1): the kind and
// what follows it, which goes whole to that kind's own show command, so
// its list (page:PL1,PL2) keeps the kind and a title may have commas.
func showPrefix(w string) (kind, rest string, ok bool) {
	t, rest, ok := strings.Cut(w, ":")
	if !ok {
		return "", "", false
	}
	kind = map[string]string{"issue": "issue", "request": "request", "design": "design", "decision": "design",
		"note": "design", "spec": "design", "page": "page", "pr": "pull", "pull": "pull"}[strings.ToLower(t)]
	return kind, strings.TrimSpace(strings.TrimRight(rest, ".,;")), kind != ""
}

// showRefs are the codes wp show was given: prefixed arguments whole,
// the rest split as listArgs does, where a bare number after a code is
// another of its kind (PL1,2 is PL1 and PL2; #12,13 is #12 and #13).
func showRefs(pos []string) []string {
	var out, plain []string
	flush := func() {
		refs := listArgs(plain)
		for i, r := range refs {
			if i == 0 || !isDigits(r) {
				continue
			}
			prev := strings.TrimRight(refs[i-1], ".;:")
			if p := strings.TrimRight(prev, "0123456789"); p != "" && p != prev {
				refs[i] = p + r
			}
		}
		out, plain = append(out, refs...), nil
	}
	for _, a := range pos {
		if _, _, ok := showPrefix(a); ok {
			flush()
			out = append(out, a)
			continue
		}
		plain = append(plain, a)
	}
	flush()
	return out
}

// showTargets are the things a word could name, most specific first;
// none when it isn't written like a code at all.
func showTargets(w string) []showTarget {
	if kind, rest, ok := showPrefix(w); ok {
		return []showTarget{{kind, rest}}
	}
	w = strings.TrimRight(w, ".,;:")
	var out []showTarget
	if requestShapeRE.MatchString(w) {
		out = append(out, showTarget{"request", w})
	}
	if designCodeRE.MatchString(w) {
		out = append(out, showTarget{"design", w})
	}
	if pageShapeRE.MatchString(w) {
		out = append(out, showTarget{"page", strings.ToUpper(w)})
	}
	if pullShapeRE.MatchString(w) {
		out = append(out, showTarget{"pull", w})
	}
	if k := normalKey(w); keyRE.MatchString(k) {
		out = append(out, showTarget{"issue", k})
	}
	return out
}

func cmdShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		key, _, err := c.currentKey(nil)
		if err != nil {
			return err
		}
		return c.showIssue(key)
	}
	refs := showRefs(pos)
	for _, r := range refs {
		if kind, rest, ok := showPrefix(r); ok && rest == "" {
			return c.usage("%q names no %s: give its code after the colon", r, kind)
		}
		if len(showTargets(r)) == 0 {
			return c.usage("%q isn't a code: give an issue KEY (like APP-12), a request (R-12), a decision (ADR7), a page (PL13) or a pull request (#12)", r)
		}
	}
	return c.each(refs, true, c.showCode)
}

// showCode shows what a code names. One that could name several things
// is looked up as each; it shows the one that exists, the first kind's
// not-found when none does, and the commands to choose from when several do.
func (c *cli) showCode(ref string) error {
	ts := showTargets(ref)
	if len(ts) == 1 {
		return c.showOne(ts[0])
	}
	var found []string
	var hit showTarget
	for _, t := range ts {
		if cmd := c.showExists(t); cmd != "" {
			found = append(found, cmd)
			hit = t
		}
	}
	switch len(found) {
	case 0:
		return c.showOne(ts[0])
	case 1:
		return c.showOne(hit)
	}
	return fmt.Errorf("%s names more than one thing; show the one you mean:\n  %s", ref, strings.Join(found, "\n  "))
}

// showExists is the command that shows t without ambiguity if t exists,
// else "".
func (c *cli) showExists(t showTarget) string {
	switch t.kind {
	case "issue":
		var i struct {
			Key string `json:"key"`
		}
		if c.do("GET", "/api/v1/issues/"+esc(t.ref), nil, &i) == nil && i.Key != "" {
			return "wp show " + i.Key
		}
	case "request":
		var r reqView // peek: looking isn't telling the agent its answer
		if c.do("GET", "/api/v1/requests/"+esc(t.ref)+"?peek=1", nil, &r) == nil && r.Code != "" {
			return "wp request show " + r.Code
		}
	case "design":
		if _, err := c.noteID(t.ref); err == nil {
			return "wp design show " + t.ref
		}
	case "page":
		if _, err := c.pageRef(t.ref); err == nil {
			return "wp page show " + t.ref
		}
	case "pull":
		return "wp pr show " + t.ref
	}
	return ""
}

// showOne runs t's kind's own show command on it.
func (c *cli) showOne(t showTarget) error {
	var args []string
	run := map[string]func(*cli, []string) error{"request": requestShow, "design": designShow, "page": pageShow, "pull": cmdPRShow}[t.kind]
	switch t.kind {
	case "issue":
		return c.each(listArgs([]string{t.ref}), true, func(k string) error { return c.showIssue(normalKey(k)) })
	case "pull":
		if m := pullShapeRE.FindStringSubmatch(t.ref); m != nil && m[1] != "" {
			args = []string{m[2], "--repo", m[1]}
		}
	}
	if args == nil {
		args = []string{t.ref}
	}
	if c.asJSON { // the command's own flags reset it
		args = append(args, "--json")
	}
	return run(c, args)
}

// showIssue prints an issue in full, with its prep.
func (c *cli) showIssue(key string) error {
	path := "/api/v1/issues/" + esc(key)
	if !c.asJSON {
		path += "?format=text"
	}
	raw, err := c.call("GET", path, nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		text := c.absoluteAttachmentLinks(string(raw))
		prep := c.showPrep(key)
		if prep == "" {
			return text
		}
		return withPrep(text, prep)
	})
}
