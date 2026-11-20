package core

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// How much of a task's comments an agent or a model is handed with the task
// (WP-449): each cut at CommentClip bytes, and past commentsMax the oldest
// few and the latest, with the count between; wp show has them all.
const (
	CommentClip = 2000
	commentsMax = 20
	commentsOld = 5
)

// CommentLine is one comment, its later lines indented under it:
//
//	[#12 2026-10-27 22:45 @robin] text
//
// With more set, a body longer than CommentClip is cut there and ends with more.
func CommentLine(c Comment, more string) string {
	who := "someone"
	if c.Author != nil {
		who = "@" + c.Author.Username
	}
	if c.Agent != "" {
		who = "agent " + c.Agent // not the person whose token it used
		if c.Unreviewed {
			who += ", unreviewed"
		} else if c.ReviewedBy != nil {
			who += ", reviewed by " + c.ReviewedBy.DisplayName
		}
	}
	body := strings.TrimRight(strings.TrimLeft(c.Body, "\r\n"), " \t\r\n")
	if more != "" {
		body = ClipText(body, CommentClip, more)
	}
	return fmt.Sprintf("  [#%d %s %s] %s\n", c.ID, c.CreatedAt.Format("2006-01-02 15:04"), who, strings.ReplaceAll(body, "\n", "\n    "))
}

// TaskCommentLines are an issue's comments, oldest first, as handed out with
// the task KEY: each cut at CommentClip, and past commentsMax the oldest
// commentsOld and the latest, with a line for those left out between.
func TaskCommentLines(key string, cs []Comment) string {
	more := "… (cut; wp show " + key + " has it all)"
	skip := max(0, len(cs)-commentsMax)
	var b strings.Builder
	for n, c := range cs {
		if skip > 0 && n == commentsOld {
			fmt.Fprintf(&b, "  … %d more between (wp show %s has every comment)\n", skip, key)
		}
		if skip > 0 && n >= commentsOld && n < commentsOld+skip {
			continue
		}
		b.WriteString(CommentLine(c, more))
	}
	return b.String()
}

// ClipText cuts s to at most n bytes, on a rune boundary, and adds more
// when it cut anything.
func ClipText(s string, n int, more string) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + more
}
