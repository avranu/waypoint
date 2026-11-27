package main

import (
	"fmt"
	"strings"
)

var prCommentsCmd = &command{
	name: "comments", args: "[N] [--settled]",
	summary: "a pull request's review comments and where each stands",
	about: `Lists review comments not dealt with, from every reviewer, with id (c12),
author, file:line, state and text. "resolved by" is what Waypoint recorded
(person, agent, forge), not the forge's user.`,
	examples: []string{"wp pr comments 12 --settled"},
	run:      cmdPRComments,
}

var prReplyCmd = &command{
	name: "reply", args: "ID -m TEXT",
	summary: "reply to a review comment in its thread",
	about: `Ids come from wp pr comments or wp pr todo. Posts in the comment's thread
(else the conversation). Changes no verdict. An agent may resolve a
person's comment only after replying or with a note.`,
	examples: []string{`wp pr reply c12 -m "Moved the check into the handler, in 3f2a9c1."`},
	run:      cmdPRReply,
}

var prReactCmd = &command{
	name: "react", args: "ID EMOJI",
	summary: "put an emoji on a review comment (+1 -1 laugh hooray confused heart rocket eyes)",

	examples: []string{"wp pr react c12 eyes", "wp pr react c12 👍"},
	run:      cmdPRReact,
}

var prResolveCmd = &command{
	name: "resolve", args: "ID… [-m NOTE]",
	summary: "close review comments",
	about: `-m posts the note as a reply first. Forgejo has no thread-resolve API, so
there the resolution is kept in Waypoint only; it counts for the merge gate.
Agents may resolve any comment after saying what they did (reply first, or
-m here), except one waiting for a person or settled by one. Takes comment
ids (c12); wp pr dismiss takes finding numbers.`,
	examples: []string{`wp pr resolve c12 c13 -m "Fixed in 3f2a9c1"`},
	run:      cmdPRResolve,
}

var prUnresolveCmd = &command{
	name: "unresolve", args: "ID…",
	summary: "take a resolution or dismissal back: the comment is owed again",

	examples: []string{"wp pr unresolve c12"},
	run:      cmdPRUnresolve,
}

// threadComment is one comment of GET /pulls/{id}/comments.
type threadComment struct {
	reviewComment
	Bot        bool     `json:"bot"`
	Resolved   bool     `json:"resolved"`
	ResolvedBy string   `json:"resolved_by"`
	Replied    bool     `json:"replied"`
	Outdated   bool     `json:"outdated"`
	Reactions  []string `json:"reactions"`
	URL        string   `json:"url"`
	Thread     string   `json:"thread"`
}

func (t threadComment) place() string {
	if t.Path == "" {
		return t.Kind
	}
	if t.Line > 0 {
		return fmt.Sprintf("%s:%d", t.Path, t.Line)
	}
	return t.Path
}

func cmdPRComments(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	settled := fs.Bool("settled", false, "include settled comments")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	d, _, err := c.loadPR(sf, pos, *repo)
	if err != nil {
		return err
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/pulls/%d/comments", d.ID), nil)
	if err != nil {
		return err
	}
	var list []threadComment
	if err := jsonInto(raw, &list); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var b strings.Builder
		shown := 0
		for _, t := range list {
			if t.Gone || (t.Dealt && !*settled) {
				continue
			}
			shown++
			state := strings.ReplaceAll(t.Verdict, "_", " ")
			switch {
			case t.ResolvedBy != "":
				state += " by " + t.ResolvedBy
			case t.Resolved:
				state += ", resolved on the forge"
			}
			if t.Replied {
				state += ", replied"
			}
			who := t.Author
			if t.Bot {
				who += " (bot)"
			}
			fmt.Fprintf(&b, "%-5s %s  %s  [%s]\n", t.ID, t.place(), who, state)
			for _, l := range strings.Split(strings.TrimSpace(stripHTMLComments(t.Body)), "\n") {
				fmt.Fprintf(&b, "      %s\n", l)
			}
		}
		if shown == 0 {
			if *settled {
				return fmt.Sprintf("No review comments on #%d.", d.Number)
			}
			return fmt.Sprintf("No review comments left to deal with on #%d (--settled lists them).", d.Number)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

// stripHTMLComments drops the markers Waypoint hides in its comments.
func stripHTMLComments(s string) string {
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+3:]
	}
}

func cmdPRReply(c *cli, args []string) error {
	fs := c.flags()
	text := fs.String("m", "", "the reply (`TEXT`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || strings.TrimSpace(*text) == "" {
		return c.usage("wp pr reply ID -m TEXT")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/"+pos[0]+"/reply", map[string]string{"body": *text})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return fmt.Sprintf("Replied to %s.", pos[0]) })
}

func cmdPRReact(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return c.usage("wp pr react ID EMOJI (+1 -1 laugh hooray confused heart rocket eyes)")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/"+pos[0]+"/react", map[string]string{"emoji": pos[1]})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return fmt.Sprintf("Put %s on %s.", pos[1], pos[0]) })
}

func cmdPRResolve(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "what was done (`NOTE`), posted as a reply; agents need it unless they replied")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("name the review comments (like c12; wp pr comments lists them)")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/resolve", map[string]any{"ids": pos, "note": *note})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return resolvedLines(raw) })
}

func cmdPRUnresolve(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("name the review comments (like c12)")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/unresolve", map[string]any{"ids": pos})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return resolvedLines(raw) })
}

// resolvedLines says where each comment stands, and whether the forge shows
// it resolved.
func resolvedLines(raw []byte) string {
	var list []threadComment
	if err := jsonInto(raw, &list); err != nil {
		return string(raw)
	}
	var lines []string
	for _, t := range list {
		where := "in Waypoint only"
		if t.Resolved {
			where = "on the forge too"
		}
		if t.Verdict != "resolved" {
			where = ""
		}
		lines = append(lines, strings.TrimSpace(fmt.Sprintf("%s %s %s", t.ID, strings.ReplaceAll(t.Verdict, "_", " "), where)))
	}
	return strings.Join(lines, "\n")
}
