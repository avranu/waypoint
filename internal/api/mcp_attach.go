package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Files on anything (WP-D16): the MCP tool. Bytes travel as base64, so it
// suits screenshots and small reports; wp attach handles big files.
var attachmentTools = []mcpTool{
	{Name: "attach_file", Title: "Attach a file",
		Description: "Attach a file (a screenshot, log, report, recording) to an issue, page, design note or comment, and get the Markdown that shows it. Put that Markdown in a description, comment or page (update_issue, add_comment, save_page, add_to_design) to embed it. Images and video play in place; text, Markdown, JSON, CSV and logs preview; the rest download.",
		InputSchema: obj([]string{"target", "name", "content_base64"}, map[string]any{
			"target":         str("What it belongs to: an issue key (WAY-12), page:PL13 (a page code or number), design:12 or comment:40"),
			"name":           str("File name with its extension, like shot.png or run.log"),
			"content_base64": str("The file's bytes, base64-encoded"),
			"caption":        str("Optional short label (the image's alt text)"),
			"project":        str("Project key a bare page code is in"),
		})},
}

func init() { mcpTools = append(mcpTools, attachmentTools...) }

// mcpAttach handles attach_file.
func (s *Server) mcpAttach(ctx context.Context, actor *core.Actor, name string, args mcpArgs) (string, bool, error) {
	if name != "attach_file" {
		return "", false, nil
	}
	t, err := s.svc.ResolveAttachTarget(ctx, actor, args.str("target"), args.str("project"))
	if err != nil {
		return "", true, err
	}
	data, err := decodeBase64File(args.str("content_base64"))
	if err != nil {
		return "", true, fmt.Errorf("%w: content_base64 isn't valid base64", errBadRequest)
	}
	a, err := s.svc.AddAttachment(ctx, actor, t, core.AttachmentInput{Name: args.str("name"), Caption: args.str("caption"), Data: data})
	if err != nil {
		return "", true, err
	}
	return fmt.Sprintf("Attached %s (%s, %d bytes) to %s.\nMarkdown to embed it:\n%s", a.Name, a.ContentType, a.Size, t, a.Markdown), true, nil
}

// decodeBase64File takes standard or URL-safe base64, with or without
// padding, line breaks, or a data: URI prefix.
func decodeBase64File(s string) ([]byte, error) {
	if strings.HasPrefix(s, "data:") {
		if _, rest, ok := strings.Cut(s, ","); ok {
			s = rest
		}
	}
	s = strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// mcpBodyLimit bounds one MCP request: room for a base64 file of the
// largest allowed size.
func (s *Server) mcpBodyLimit() int64 {
	return max(2<<20, s.svc.MaxAttachmentBytes()*4/3+(64<<10))
}
