package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAttachmentsOnAnything(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	robin, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	zed, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "zed", Email: "z@example.com", Password: "correct horse battery"})
	name := "Files"
	ja := &core.Actor{User: robin}
	svc.CreateProject(ctx, ja, core.ProjectInput{Name: &name})
	issue, _ := svc.CreateIssue(ctx, ja, core.IssueInput{Project: "FILE", Title: "Has files"})
	title, cat, proj := "Plan", "plan", "FILE"
	pg, err := svc.CreatePage(ctx, ja, core.PageInput{Title: &title, Category: &cat, Project: core.Some(proj)})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, ja, "t", false, 0)
	ztok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: zed}, "t", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	z := &client{t: t, base: ts.URL, http: http.DefaultClient, token: ztok}

	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	mp4 := "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom0123456789abcdefghijklmnopqrstuvwxyz"
	upload := func(target, fname, body string) core.Attachment {
		t.Helper()
		var a core.Attachment
		q := url.Values{"target": {target}, "name": {fname}, "caption": {"a caption"}}
		if code := c.json("POST", "/api/v1/attachments?"+q.Encode(), body, &a); code != 200 {
			t.Fatalf("upload %s to %s: %d", fname, target, code)
		}
		return a
	}
	pageTarget := fmt.Sprintf("page:%d", pg.ID)
	img := upload(pageTarget, "shot.png", png)
	if img.TargetType != "page" || img.TargetID != pg.ID || img.Kind != "image" || img.Caption != "a caption" || !img.Inline {
		t.Errorf("page image = %+v", img)
	}
	var viaCode core.Attachment
	if code := c.json("POST", "/api/v1/attachments?target=page:"+pg.Code+"&project=FILE&name=c.txt", "by code", &viaCode); code != 200 || viaCode.TargetID != pg.ID {
		t.Errorf("by code: %d %+v", code, viaCode)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "notes.md")
	io.WriteString(fw, "# Notes\n")
	mw.WriteField("caption", "from a form")
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/attachments?target=design:1", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("missing design note: %d", resp.StatusCode)
	}
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	fw, _ = mw.CreateFormFile("file", "notes.md")
	io.WriteString(fw, "# Notes\n")
	mw.WriteField("caption", "from a form")
	mw.Close()
	req, _ = http.NewRequest("POST", ts.URL+"/api/v1/issues/"+issue.Key+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("multipart upload: %d", resp.StatusCode)
	}
	var onIssue []core.Attachment
	if code := c.json("GET", "/api/v1/attachments?target="+issue.Key, nil, &onIssue); code != 200 || len(onIssue) != 1 || onIssue[0].Caption != "from a form" || onIssue[0].Preview != "markdown" {
		t.Errorf("issue files: %d %+v", code, onIssue)
	}

	html := upload(pageTarget, "evil.html", "<html><script>alert(1)</script></html>")
	svg := upload(pageTarget, "evil.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	txt := upload(pageTarget, "run.log", "line one\nline two <script>x</script>\n")
	vid := upload(pageTarget, "demo.mp4", mp4)

	get := func(token, url string, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	safe := func(r *http.Response, what string) {
		t.Helper()
		if r.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: nosniff %q csp %q", what, r.Header.Get("X-Content-Type-Options"), r.Header.Get("Content-Security-Policy"))
		}
	}
	for _, tc := range []struct {
		att    core.Attachment
		ct     string
		inline bool
	}{
		{img, "image/png", true},
		{vid, "video/mp4", true},
		{txt, "text/plain; charset=utf-8", true},
		{html, "application/octet-stream", false},
		{svg, "application/octet-stream", false},
	} {
		r, _ := get(tok, tc.att.URL, nil)
		disp := r.Header.Get("Content-Disposition")
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != tc.ct || strings.HasPrefix(disp, "inline") != tc.inline {
			t.Errorf("%s served as %d %q %q", tc.att.Name, r.StatusCode, r.Header.Get("Content-Type"), disp)
		}
		safe(r, tc.att.Name)
	}
	if r, _ := get(tok, img.URL+"?download=1", nil); !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %q", r.Header.Get("Content-Disposition"))
	}
	r, body := get(tok, vid.URL, map[string]string{"Range": "bytes=4-9"})
	if r.StatusCode != 206 || body != mp4[4:10] || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 4-9/%d", len(mp4)) || r.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("range: %d %q %q", r.StatusCode, body, r.Header.Get("Content-Range"))
	}
	r, _ = get(tok, vid.URL, nil)
	if r, _ := get(tok, vid.URL, map[string]string{"If-None-Match": r.Header.Get("ETag")}); r.StatusCode != 304 {
		t.Errorf("revalidate: %d", r.StatusCode)
	}
	if r, body := get(tok, txt.URL, map[string]string{"Range": "bytes=0-3"}); r.StatusCode != 206 || body != "line" {
		t.Errorf("text range: %d %q", r.StatusCode, body)
	}

	var onPage []core.Attachment
	if code := c.json("GET", "/api/v1/attachments?target="+pageTarget, nil, &onPage); code != 200 || len(onPage) != 6 {
		t.Errorf("page files: %d %d", code, len(onPage))
	}
	if code, _ := z.do("GET", "/api/v1/attachments?target="+pageTarget, nil); code != 404 {
		t.Errorf("outsider list: %d", code)
	}
	if r, _ := get(ztok, img.URL, nil); r.StatusCode != 404 {
		t.Errorf("outsider fetched: %d", r.StatusCode)
	}
	if code, _ := z.do("POST", "/api/v1/attachments?target="+pageTarget+"&name=x.txt", "x"); code != 404 {
		t.Errorf("outsider upload: %d", code)
	}
	if code, _ := z.do("DELETE", fmt.Sprintf("/api/v1/attachments/%d", img.ID), nil); code != 404 {
		t.Errorf("outsider delete: %d", code)
	}
	for _, bad := range []string{"", "page:", "nope:1", "page:99999", "request:R-1"} {
		if code, _ := c.do("GET", "/api/v1/attachments?target="+bad, nil); code != 400 && code != 404 {
			t.Errorf("target %q: %d", bad, code)
		}
	}
	var capd core.Attachment
	if code := c.json("PATCH", fmt.Sprintf("/api/v1/attachments/%d", img.ID), map[string]string{"caption": "new caption"}, &capd); code != 200 || capd.Caption != "new caption" {
		t.Errorf("caption: %d %+v", code, capd)
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/v1/attachments/%d", html.ID), nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if r, _ := get(tok, html.URL, nil); r.StatusCode != 404 {
		t.Errorf("deleted file still served: %d", r.StatusCode)
	}
}

func TestAttachmentSizeLimit(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	name := "Big"
	svc.CreateProject(ctx, &core.Actor{User: u}, core.ProjectInput{Name: &name})
	svc.CreateIssue(ctx, &core.Actor{User: u}, core.IssueInput{Project: "BIG", Title: "Files"})
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "t", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	if code, _ := c.do("POST", "/api/v1/attachments?target=BIG-1&name=ok.bin", strings.Repeat("x", 12<<20)); code != 200 {
		t.Errorf("12 MB: %d", code)
	}
	code, b := c.do("POST", "/api/v1/attachments?target=BIG-1&name=big.bin", strings.Repeat("x", 27<<20))
	if code != 400 && code != 413 && code != 422 {
		t.Errorf("27 MB: %d %s", code, b)
	}
}

func TestMCPAttachFile(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	name := "Mcp"
	ja := &core.Actor{User: u}
	svc.CreateProject(ctx, ja, core.ProjectInput{Name: &name})
	svc.CreateIssue(ctx, ja, core.IssueInput{Project: "MCP", Title: "Look at this"})
	tok, _, _ := svc.CreateAPIToken(ctx, ja, "t", false, 0)
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"))
	out := callMCPTool(t, ts.URL, tok, "attach_file", map[string]any{"target": "MCP-1", "name": "shot.png", "content_base64": png, "caption": "The bug"})
	if !strings.Contains(out, "![The bug](/api/v1/attachments/") || !strings.Contains(out, "image/png") {
		t.Errorf("attach_file: %s", out)
	}
	files, _ := svc.ListAttachments(ctx, ja, core.AttachTarget{Type: core.TargetIssue, ID: 1})
	if len(files) != 1 || files[0].Agent != "mcp" || files[0].Caption != "The bug" {
		t.Errorf("files = %+v", files)
	}
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("log line\n"), 400_000))
	if out := callMCPTool(t, ts.URL, tok, "attach_file", map[string]any{"target": "MCP-1", "name": "big.log", "content_base64": big}); !strings.Contains(out, "Attached big.log (text/x-log") {
		t.Errorf("big file: %.200s", out)
	}
	for _, args := range []map[string]any{
		{"target": "MCP-1", "name": "x.png", "content_base64": "!!not base64!!"},
		{"target": "nope:1", "name": "x.png", "content_base64": png},
		{"target": "page:999", "name": "x.png", "content_base64": png},
	} {
		if out := callMCPTool(t, ts.URL, tok, "attach_file", args); !strings.HasPrefix(out, "Error:") {
			t.Errorf("%v: %s", args, out)
		}
	}
	if b, err := decodeBase64File("data:image/png;base64," + strings.ReplaceAll(png, "AAAA", "AAAA\n")); err != nil || len(b) == 0 {
		t.Errorf("data URI: %v", err)
	}
	if b, err := decodeBase64File(base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe})); err != nil || !bytes.Equal(b, []byte{0xfb, 0xff, 0xfe}) {
		t.Errorf("url-safe: %v %x", err, b)
	}
}
