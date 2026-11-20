package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Fetching what wp attach stored (WP-92): attachments sit behind the login,
// so an agent can't curl them without reading its token. wp files downloads
// them with the CLI's own client and prints local paths to open.
var filesCmd = &command{
	name: "files", group: "Issues", args: "[TARGET] [--dir DIR]",
	aliases: []string{"fetch"},
	summary: "download attachments to local files",
	about: `Prints one line per saved file: its path, then caption or name. Open
images with your file-reading tool; don't curl them. TARGET is as for
wp attach (default: your current task). For an issue that is every file in
its description and comments plus unembedded ones. Files go to
wp-files/TARGET/ under the temp directory, overwritten on rerun.`,
	examples: []string{
		"wp files WP-12",
		"wp files page:PL3 --dir ./shots",
	},
	run: cmdFiles,
}

// fileInfo is an attachment as the API describes it, plus where it was
// found and (once fetched) where it went.
type fileInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Caption string `json:"caption,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Size    int64  `json:"size,omitempty"`
	URL     string `json:"url"`
	// Source is where it belongs: the description, a comment, or just the
	// target ("attached").
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
}

// attachLinkRE finds an attachment in Markdown: /api/v1/attachments/ID/NAME.
var attachLinkRE = lazyre.New(`/api/v1/attachments/(\d+)/([^)\s"'<>\]]+)`)

func cmdFiles(c *cli, args []string) error {
	fs := c.flags()
	dir := fs.String("dir", "", "save into `DIR`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var spec string
	if len(pos) == 0 {
		key, _, err := c.currentKey(nil)
		if err != nil {
			return c.usage("whose files? Give a TARGET (WP-12, page:PL3, design:12, request:R-4)")
		}
		spec = "issue:" + key
	} else {
		var rest []string
		if spec, rest, err = c.attachTarget(pos); err != nil {
			return err
		}
		if len(rest) > 0 {
			return c.usage("expected one TARGET")
		}
	}
	typ, ref, _ := strings.Cut(spec, ":")
	if typ == "page" {
		id, err := c.pageRef(ref)
		if err != nil {
			return err
		}
		spec, ref = fmt.Sprintf("page:%d", id), strconv.FormatInt(id, 10)
	}
	var files []fileInfo
	if typ == "issue" {
		files, err = c.issueFiles(ref)
	} else {
		files, err = c.targetFiles(spec, "attached")
	}
	if err != nil {
		return err
	}
	label := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(strings.ToUpper(ref))
	if typ != "issue" {
		label = typ + "-" + label
	}
	if *dir == "" {
		*dir = filepath.Join(os.TempDir(), "wp-files", label)
	}
	if len(files) == 0 {
		if c.asJSON {
			c.println("[]")
		} else {
			c.println("No attachments on " + label + ".")
		}
		return nil
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	failed := 0
	for i := range files {
		f := &files[i]
		f.Path = filepath.Join(*dir, fileLocalName(f))
		if f.Path, err = filepath.Abs(f.Path); err != nil {
			return err
		}
		data, err := c.callRaw("GET", f.URL, "", nil)
		if err == nil {
			err = os.WriteFile(f.Path, data, 0o600)
		}
		if err != nil {
			c.note("%s: %v", f.Name, err)
			f.Path = ""
			failed++
		}
		f.URL = c.server() + f.URL
	}
	if c.asJSON {
		b, err := json.MarshalIndent(files, "", "  ")
		if err != nil {
			return err
		}
		c.println(string(b))
	} else {
		for _, f := range files {
			if f.Path == "" {
				continue
			}
			c.println(f.Path + "  " + f.describe())
		}
	}
	if failed > 0 {
		return fmt.Errorf("couldn't fetch %d of %d files", failed, len(files))
	}
	return nil
}

// describe is the line after the path: the caption, else the name, and where
// the file belongs when that isn't just "attached to the target".
func (f fileInfo) describe() string {
	s := f.Name
	if f.Caption != "" {
		s = f.Caption + " (" + f.Name + ")"
	}
	if f.Source != "" && f.Source != "attached" {
		s += " - " + f.Source
	}
	return s
}

// fileLocalName is ID-NAME, safe as one path element.
func fileLocalName(f *fileInfo) string {
	name := strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(f.Name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return strconv.FormatInt(f.ID, 10) + "-" + name
}

// targetFiles lists the files on one target (the API checks access).
func (c *cli) targetFiles(spec, source string) ([]fileInfo, error) {
	var list []fileInfo
	if err := c.do("GET", "/api/v1/attachments?target="+url.QueryEscape(spec), nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Source = source
	}
	return list, nil
}

// issueFiles gathers an issue's files: those attached to it, those attached
// to its comments, and any attachment its description or comments link to
// that belongs to something else.
func (c *cli) issueFiles(key string) ([]fileInfo, error) {
	var d struct {
		Description string     `json:"description"`
		Files       []fileInfo `json:"attachments"`
	}
	if err := c.do("GET", "/api/v1/issues/"+esc(key), nil, &d); err != nil {
		return nil, err
	}
	var comments []struct {
		ID    int64      `json:"id"`
		Body  string     `json:"body"`
		Files []fileInfo `json:"attachments"`
	}
	if err := c.do("GET", "/api/v1/issues/"+esc(key)+"/comments", nil, &comments); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []fileInfo
	add := func(list []fileInfo, source string) {
		for _, f := range list {
			if !seen[f.ID] {
				seen[f.ID] = true
				f.Source = source
				out = append(out, f)
			}
		}
	}
	// Linked from the text first, so "description" wins over "attached".
	linked := func(text string) []fileInfo {
		var l []fileInfo
		for _, m := range attachLinkRE.FindAllStringSubmatch(text, -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			name, err := url.PathUnescape(m[2])
			if err != nil {
				name = m[2]
			}
			l = append(l, fileInfo{ID: id, Name: name, URL: m[0]})
		}
		return l
	}
	byID := map[int64]fileInfo{}
	for _, f := range d.Files {
		byID[f.ID] = f
	}
	for _, cm := range comments {
		for _, f := range cm.Files {
			byID[f.ID] = f
		}
	}
	// A link carries only the id and name: take the rest from the listing.
	richer := func(l []fileInfo) []fileInfo {
		for i, f := range l {
			if g, ok := byID[f.ID]; ok {
				l[i] = g
			}
		}
		return l
	}
	add(richer(linked(d.Description)), "description")
	for _, cm := range comments {
		add(richer(linked(cm.Body)), "comment "+strconv.FormatInt(cm.ID, 10))
	}
	add(d.Files, "attached")
	for _, cm := range comments {
		add(cm.Files, "comment "+strconv.FormatInt(cm.ID, 10))
	}
	return out, nil
}

// absoluteAttachmentLinks turns the /api/v1/attachments/… links in text the
// server printed into full URLs, which a person can click.
func (c *cli) absoluteAttachmentLinks(text string) string {
	if c.server() == "" {
		return text
	}
	return strings.ReplaceAll(text, "](/api/v1/attachments/", "]("+c.server()+"/api/v1/attachments/")
}
