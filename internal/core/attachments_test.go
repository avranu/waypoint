package core

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func (e *env) attach(a *Actor, t AttachTarget, name string, data []byte) (*Attachment, error) {
	e.t.Helper()
	return e.s.AddAttachment(e.ctx, a, t, AttachmentInput{Name: name, Data: data})
}

func (e *env) count(t AttachTarget) int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM attachments WHERE target_type = ? AND target_id = ?`, t.Type, t.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAttachmentKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		ct      string
		kind    string
		preview string
	}{
		{"shot.png", pngBytes, "image/png", KindImage, ""},
		{"a.mp4", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), "video/mp4", KindVideo, ""},
		{"a.webm", []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01"), "video/webm", KindVideo, ""},
		{"README.md", []byte("# hi\n"), "text/markdown", KindText, "markdown"},
		{"run.log", []byte("12:00 started\n"), "text/x-log", KindText, "text"},
		{"data.json", []byte(`{"a":1}`), "application/json", KindText, "json"},
		{"rows.csv", []byte("a,b\n1,2\n"), "text/csv", KindText, "csv"},
		{"conf.yaml", []byte("a: 1\n"), "text/yaml", KindText, "text"},
		{"fix.diff", []byte("--- a\n+++ b\n"), "text/x-diff", KindText, "text"},
		{"notes", []byte("plain words"), "text/plain", KindText, "text"},
		{"x.html", []byte("<html><script>alert(1)</script>"), "text/html", KindFile, ""},
		{"x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "image/svg+xml", KindFile, ""},
		{"x.svg.txt", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), "text/xml", KindFile, ""},
		{"bin.json", []byte("\x00\x01\x02\xff\xfe"), "application/octet-stream", KindFile, ""},
		{"x.zip", []byte("PK\x03\x04rest"), "application/zip", KindFile, ""},
	} {
		ct := detectContentType(tc.name, tc.data)
		kind, preview := fileKind(ct)
		if ct != tc.ct || kind != tc.kind || preview != tc.preview {
			t.Errorf("%s: %q %s/%s, want %q %s/%s", tc.name, ct, kind, preview, tc.ct, tc.kind, tc.preview)
		}
	}
	long := append(bytes.Repeat([]byte("a"), 64<<10-1), []byte("é more")...)
	if !looksLikeText(long) {
		t.Error("text cut mid-character judged binary")
	}
}

func TestAttachmentTargetsAndAccess(t *testing.T) {
	e := newEnv(t)
	sam, lee := e.user("sam", false), e.user("lee", false)
	p := e.project(e.admin, "Files", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	e.s.SetMember(e.ctx, e.admin, p.Key, "lee", "viewer")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Has files"})
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Plan"), Category: str("plan"), Project: Some(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := e.s.CreatePage(e.ctx, sam, PageInput{Title: str("Diary"), Category: str("note")})
	if err != nil {
		t.Fatal(err)
	}
	note, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Screen"), Kind: str("screen"), Body: str("x"), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	global, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Principle"), Kind: str("goal"), Body: str("x")})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.s.AddComment(e.ctx, lee, i.Key, "see attached")
	if err != nil {
		t.Fatal(err)
	}
	issue, page, design := AttachTarget{TargetIssue, i.ID}, AttachTarget{TargetPage, pg.ID}, AttachTarget{TargetDesign, note.ID}
	comment, personal := AttachTarget{TargetComment, c.ID}, AttachTarget{TargetPage, mine.ID}

	for _, tg := range []AttachTarget{issue, page, design} {
		a, err := e.attach(sam, tg, "shot.png", pngBytes)
		if err != nil {
			t.Fatalf("%s: %v", tg, err)
		}
		if a.TargetType != tg.Type || a.TargetID != tg.ID || a.Kind != KindImage || !a.Inline || a.Uploader == nil || a.Uploader.Username != "sam" {
			t.Errorf("%s: %+v", tg, a)
		}
		list, err := e.s.ListAttachments(e.ctx, lee, tg)
		if err != nil || len(list) != 1 || list[0].ID != a.ID {
			t.Errorf("%s list = %v %v", tg, list, err)
		}
	}
	if d := e.get(e.admin, i.Key); len(d.Files) != 1 || d.Files[0].TargetType != TargetIssue {
		t.Errorf("issue files = %+v", d.Files)
	}
	if _, err := e.attach(lee, page, "x.txt", []byte("x")); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer attached to a page: %v", err)
	}
	ca, err := e.attach(lee, comment, "mine.png", pngBytes)
	if err != nil {
		t.Fatalf("author attaching to own comment: %v", err)
	}
	if _, err := e.attach(lee, issue, "x.txt", []byte("x")); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer attached to an issue: %v", err)
	}
	if cs, _ := e.s.ListComments(e.ctx, e.admin, i.Key); len(cs) != 1 || len(cs[0].Files) != 1 || cs[0].Files[0].ID != ca.ID {
		t.Errorf("comment files = %+v", cs)
	}
	if _, err := e.attach(lee, AttachTarget{TargetComment, c.ID + 99}, "x.txt", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing comment: %v", err)
	}
	other, _ := e.s.AddComment(e.ctx, e.admin, i.Key, "mine")
	if _, err := e.attach(lee, AttachTarget{TargetComment, other.ID}, "x.txt", []byte("x")); !errors.Is(err, ErrForbidden) {
		t.Errorf("attached to another's comment: %v", err)
	}
	if _, err := e.attach(sam, personal, "d.md", []byte("# d")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ListAttachments(e.ctx, e.admin, personal); !errors.Is(err, ErrNotFound) {
		t.Errorf("admin listed a personal page's files: %v", err)
	}
	if _, err := e.attach(sam, AttachTarget{TargetDesign, global.ID}, "x.txt", []byte("x")); err == nil {
		t.Error("non-admin attached to a global design note")
	}
	if _, err := e.attach(e.admin, AttachTarget{TargetDesign, global.ID}, "x.txt", []byte("x")); err != nil {
		t.Error(err)
	}
	if l, err := e.s.ListAttachments(e.ctx, lee, AttachTarget{TargetDesign, global.ID}); err != nil || len(l) != 1 {
		t.Errorf("global list = %v %v", l, err)
	}
	out := e.user("out", false)
	if _, err := e.s.ListAttachments(e.ctx, out, page); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider list: %v", err)
	}
	if _, err := e.attach(out, page, "x.txt", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider attach: %v", err)
	}
	list, _ := e.s.ListAttachments(e.ctx, e.admin, page)
	if _, _, _, err := e.s.AttachmentData(e.ctx, out, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider read: %v", err)
	}
	if err := e.s.DeleteAttachment(e.ctx, out, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider delete: %v", err)
	}
	if err := e.s.DeleteAttachment(e.ctx, lee, list[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer delete: %v", err)
	}
	if err := e.s.DeleteAttachment(e.ctx, sam, list[0].ID); err != nil {
		t.Error(err)
	}
	if err := e.s.DeleteAttachment(e.ctx, lee, ca.ID); err != nil {
		t.Errorf("author deleting own comment file: %v", err)
	}
	a, _ := e.s.AddAttachment(e.ctx, sam, page, AttachmentInput{Name: "x.png", Data: pngBytes, Caption: "  the plan  "})
	if a.Caption != "the plan" {
		t.Errorf("caption %q", a.Caption)
	}
	if a, err = e.s.SetAttachmentCaption(e.ctx, sam, a.ID, "new"); err != nil || a.Caption != "new" {
		t.Errorf("set caption: %+v %v", a, err)
	}
	if _, err := e.s.SetAttachmentCaption(e.ctx, lee, a.ID, "no"); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer set caption: %v", err)
	}
	for _, typ := range []string{TargetRequest, TargetSession} {
		if _, err := e.attach(e.admin, AttachTarget{typ, 1}, "x.txt", []byte("x")); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, err := e.attach(e.admin, AttachTarget{"bogus", 1}, "x.txt", []byte("x")); !IsInvalid(err) {
		t.Errorf("bogus target: %v", err)
	}
}

func TestAttachmentProvenanceAndLimits(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Limits", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Many"})
	it := AttachTarget{TargetIssue, i.ID}
	bot := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	a, err := e.attach(bot, it, "shot.png", pngBytes)
	if err != nil || a.Agent != "anthropic/claude-code" {
		t.Fatalf("agent upload: %+v %v", a, err)
	}
	if a, _ := e.attach(e.admin, it, "mine.png", pngBytes); a.Agent != "" {
		t.Errorf("person's upload marked %q", a.Agent)
	}
	if _, err := e.attach(&Actor{User: e.admin.User, ReadOnly: true}, it, "x.txt", []byte("x")); err == nil {
		t.Error("read-only token uploaded")
	}
	if _, err := e.attach(e.admin, it, "empty", nil); !IsInvalid(err) {
		t.Errorf("empty file: %v", err)
	}
	if got := e.s.MaxAttachmentBytes(); got != 25<<20 {
		t.Errorf("default limit %d", got)
	}
	e.s.cfg.MaxAttachmentMB = 1
	if _, err := e.attach(e.admin, it, "big.bin", bytes.Repeat([]byte("x"), 1<<20+1)); !IsInvalid(err) || !strings.Contains(err.Error(), "1 MB") {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := e.attach(e.admin, it, "ok.bin", bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	e.s.cfg.MaxAttachmentMB = 25
	if _, err := e.attach(e.admin, it, "big.bin", bytes.Repeat([]byte("x"), 1<<20+1)); err != nil {
		t.Errorf("raised limit: %v", err)
	}
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Many"), Project: Some(p.Key)})
	pt := AttachTarget{TargetPage, pg.ID}
	for n := 0; n < maxAttachmentsPerTarget; n++ {
		if _, err := e.attach(e.admin, pt, "f.txt", []byte("x")); err != nil {
			t.Fatalf("file %d: %v", n, err)
		}
	}
	if _, err := e.attach(e.admin, pt, "f.txt", []byte("x")); !IsInvalid(err) {
		t.Errorf("101st file: %v", err)
	}
	if _, err := e.attach(e.admin, it, "f.txt", []byte("x")); err != nil {
		t.Errorf("another target is full: %v", err)
	}
}

// Deleting a page, design note, comment or issue deletes its files; the
// issue's other files and other targets' are untouched.
func TestAttachmentsDieWithTheirTarget(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Gone", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Doomed"})
	keep := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Kept"})
	parent, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Parent"), Project: Some(p.Key)})
	child, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Child"), Parent: Some(parent.ID)})
	note, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Note"), Kind: str("screen"), Body: str("x"), Project: &p.Key})
	c, _ := e.s.AddComment(e.ctx, e.admin, i.Key, "hi")
	targets := []AttachTarget{{TargetIssue, i.ID}, {TargetIssue, keep.ID}, {TargetPage, parent.ID}, {TargetPage, child.ID}, {TargetDesign, note.ID}, {TargetComment, c.ID}}
	for _, tg := range targets {
		if _, err := e.attach(e.admin, tg, "f.png", pngBytes); err != nil {
			t.Fatalf("%s: %v", tg, err)
		}
	}
	if err := e.s.DeleteComment(e.ctx, e.admin, c.ID); err != nil {
		t.Fatal(err)
	}
	if e.count(targets[5]) != 0 || e.count(targets[0]) != 1 {
		t.Error("deleting a comment: its file stayed, or the issue's went")
	}
	if err := e.s.DeletePage(e.ctx, e.admin, parent.ID); err != nil {
		t.Fatal(err)
	}
	if e.count(targets[2]) != 0 || e.count(targets[3]) != 0 {
		t.Error("deleting a page: its files (or its sub-page's) stayed")
	}
	if err := e.s.DeleteDesign(e.ctx, e.admin, note.ID); err != nil {
		t.Fatal(err)
	}
	if e.count(targets[4]) != 0 {
		t.Error("deleting a design note: its files stayed")
	}
	c2, _ := e.s.AddComment(e.ctx, e.admin, i.Key, "again")
	e.attach(e.admin, AttachTarget{TargetComment, c2.ID}, "f.png", pngBytes)
	if err := e.s.DeleteIssue(e.ctx, e.admin, i.Key, false); err != nil {
		t.Fatal(err)
	}
	var left int
	e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM attachments`).Scan(&left)
	if left != 1 || e.count(targets[1]) != 1 {
		t.Errorf("%d files left after deleting the issue, want just the other issue's", left)
	}
}

func TestParseAndResolveAttachTarget(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Refs", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Thing"})
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Plan"), Category: str("plan"), Project: Some(p.Key)})
	for spec, want := range map[string]AttachTarget{
		i.Key:                           {TargetIssue, i.ID},
		"issue:" + i.Key:                {TargetIssue, i.ID},
		"page:" + itoa64(pg.ID):         {TargetPage, pg.ID},
		"page:" + pg.Code:               {TargetPage, pg.ID},
		"PAGE:#" + itoa64(pg.ID):        {TargetPage, pg.ID},
		"page:" + p.Key + "-" + pg.Code: {TargetPage, pg.ID},
	} {
		got, err := e.s.ResolveAttachTarget(e.ctx, e.admin, spec, p.Key)
		if err != nil || got != want {
			t.Errorf("%q = %v %v, want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"", "page:", "wat:1", "design:abc", "comment:0"} {
		if _, err := e.s.ResolveAttachTarget(e.ctx, e.admin, bad, ""); !IsInvalid(err) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := e.s.ResolveAttachTarget(e.ctx, e.admin, "page:9999", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing page: %v", err)
	}
	if _, err := e.s.ResolveAttachTarget(e.ctx, e.admin, "request:R-4", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("request before its slice lands: %v", err)
	}
}
