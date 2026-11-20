package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Files on anything (WP-D16).
var attachCmd = &command{
	name: "attach", group: "Issues", args: "TARGET FILE... [--caption TEXT] [--embed]",
	aliases: []string{"upload"},
	summary: "attach files (screenshots, logs) to an issue, page or request",
	about: `Uploads each FILE and prints the Markdown that shows it. FILE - reads
stdin (give --name). Up to 25 MB each unless the server sets another limit.

TARGET: WP-12 (an issue), page:PL3, design:12, comment:40, request:R-4,
or session (the current agent session). Default: your current task.

--embed adds the Markdown to the issue description, page body or design
note; with --comment an issue gets a comment instead. In Claude Code with
wp's hooks, screenshots you Read attach to your task by themselves.
To view attachments others added, use wp files.`,
	examples: []string{
		`wp attach page:PL3 diagram.png --caption "Architecture" --embed`,
		"wp attach WP-12 run.log --embed --comment",
		"some-command | wp attach WP-12 - --name output.log",
	},
	run: cmdAttach,
}

// attachKinds are the words that start a TARGET (kind:ref).
var attachKinds = []string{"issue", "page", "design", "note", "comment", "request", "session"}

type attached struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	Markdown string `json:"markdown"`
}

func cmdAttach(c *cli, args []string) error {
	fs := c.flags()
	caption := fs.String("caption", "", "label / alt `TEXT`")
	embed := fs.Bool("embed", false, "add the Markdown to the target")
	asComment := fs.Bool("comment", false, "with --embed on an issue: as a comment")
	name := fs.String("name", "", "file `NAME` (required for stdin)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	spec, files, err := c.attachTarget(pos)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return c.usage("which files? wp attach TARGET FILE...")
	}
	if *name != "" && len(files) > 1 {
		return c.usage("--name names one file; give one FILE")
	}
	if *asComment && !*embed {
		return c.usage("--comment goes with --embed")
	}
	typ, ref, _ := strings.Cut(spec, ":")
	if *embed {
		if typ == "comment" || typ == "request" || typ == "session" {
			return c.usage("--embed works on issues, pages and design notes; paste the printed Markdown where you want it")
		}
		if *asComment && typ != "issue" {
			return c.usage("--comment is for issues")
		}
	}
	// Read everything first, so a typo in the last file name uploads nothing.
	type file struct {
		name string
		data []byte
	}
	var upload []file
	for _, f := range files {
		var data []byte
		n := *name
		if f == "-" {
			if n == "" {
				return c.usage("a file from stdin needs --name")
			}
			data, err = io.ReadAll(os.Stdin)
		} else {
			if n == "" {
				n = filepath.Base(f)
			}
			data, err = os.ReadFile(f) // #nosec G304 -- the user named the file
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if len(data) == 0 {
			return fmt.Errorf("%s is empty", f)
		}
		upload = append(upload, file{n, data})
	}
	// A page's title or code is resolved here, as wp page show does.
	if typ == "page" {
		id, err := c.pageRef(ref)
		if err != nil {
			return err
		}
		spec = fmt.Sprintf("page:%d", id)
	}
	var done []attached
	for _, f := range upload {
		q := url.Values{"target": {spec}, "name": {f.name}}
		if *caption != "" {
			q.Set("caption", *caption)
		}
		raw, err := c.callRaw("POST", "/api/v1/attachments?"+q.Encode(), "application/octet-stream", bytes.NewReader(f.data))
		if err != nil {
			if len(done) > 0 {
				c.note("Attached %d of %d files before this failed.", len(done), len(upload))
			}
			return fmt.Errorf("%s: %w", f.name, err)
		}
		var a attached
		if err := jsonInto(raw, &a); err != nil {
			return err
		}
		done = append(done, a)
	}
	md := make([]string, len(done))
	for i, a := range done {
		md[i] = a.Markdown
	}
	embedded := ""
	if *embed {
		if embedded, err = c.embedMarkdown(typ, spec, strings.Join(md, "\n\n"), *asComment); err != nil {
			return fmt.Errorf("attached, but couldn't embed: %w", err)
		}
	}
	if c.asJSON {
		b, err := json.MarshalIndent(map[string]any{"target": spec, "attachments": done, "embedded": embedded}, "", "  ")
		if err != nil {
			return err
		}
		c.println(string(b))
		return nil
	}
	for _, m := range md {
		c.println(m)
	}
	if embedded != "" {
		c.note("%s", embedded)
	}
	return nil
}

// attachTarget splits the positionals into the target and the files. The
// first word is the target when it looks like one (WP-12, page:PL3, session);
// else everything is a file and the target is the task taken in this directory.
func (c *cli) attachTarget(pos []string) (string, []string, error) {
	if len(pos) > 0 {
		first := pos[0]
		if strings.EqualFold(first, "session") {
			id := strings.TrimSpace(os.Getenv("WAYPOINT_SESSION"))
			if id == "" {
				return "", nil, c.usage("no agent session here: WAYPOINT_SESSION isn't set. Use session:ID")
			}
			return "session:" + id, pos[1:], nil
		}
		if typ, ref, ok := strings.Cut(first, ":"); ok && ref != "" && isAttachKind(typ) {
			typ = strings.ToLower(typ)
			if typ == "issue" {
				ref = strings.ToUpper(ref)
			}
			if typ == "design" || typ == "note" || typ == "comment" {
				ref = strings.TrimPrefix(ref, "#") // the server accepts #12; the embed path needs 12
			}
			return typ + ":" + ref, pos[1:], nil
		}
		if codeShapes[0].re.MatchString(first) {
			// API:R-30 is a request, not a file called that.
			if _, err := os.Stat(first); errors.Is(err, fs.ErrNotExist) {
				return "request:" + first, pos[1:], nil
			}
		}
		if keyRE.MatchString(strings.ToUpper(first)) {
			// A file called WP-12 is a file (and so is one we can't stat).
			if _, err := os.Stat(first); errors.Is(err, fs.ErrNotExist) {
				return "issue:" + strings.ToUpper(first), pos[1:], nil
			}
		}
	}
	key, _, err := c.currentKey(nil)
	if err != nil {
		return "", nil, c.usage("attach to what? Give a TARGET (WP-12, page:PL3, design:12, request:R-4, session) before the files")
	}
	return "issue:" + key, pos, nil
}

func isAttachKind(s string) bool {
	for _, k := range attachKinds {
		if strings.EqualFold(s, k) {
			return true
		}
	}
	return false
}

// embedMarkdown adds md to the target: an issue's description (or a comment
// on it), a page's body, or an addition to a design note.
func (c *cli) embedMarkdown(typ, spec, md string, asComment bool) (string, error) {
	_, ref, _ := strings.Cut(spec, ":")
	switch typ {
	case "issue":
		if asComment {
			if _, err := c.call("POST", "/api/v1/issues/"+esc(ref)+"/comments", map[string]string{"body": md}); err != nil {
				return "", err
			}
			return "Commented on " + ref + ".", nil
		}
		// The server appends in one transaction: no edit made meanwhile is lost.
		if _, err := c.call("PATCH", "/api/v1/issues/"+esc(ref), map[string]string{"append_description": md}); err != nil {
			return "", err
		}
		return "Added to the description of " + ref + ".", nil
	case "page":
		if _, err := c.call("PATCH", "/api/v1/pages/"+esc(ref), map[string]string{"append_body": md}); err != nil {
			return "", err
		}
		return "Added to the end of page #" + ref + ".", nil
	case "design", "note":
		raw, err := c.call("POST", "/api/v1/design/"+esc(ref)+"/additions", map[string]string{"body": md})
		if err != nil {
			return "", err
		}
		var d designInfo
		_ = jsonInto(raw, &d)
		s := "Added to design #" + ref + "."
		if d.Notice != "" {
			s += " " + d.Notice
		}
		return s, nil
	}
	return "", fmt.Errorf("can't embed in a %s", typ)
}
