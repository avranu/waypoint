package main

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

var prBodiesCmd = &command{
	name: "bodies", args: "[--since 30d] [--show N] [--mode shadow|model|template]",
	summary: "pull request descriptions both ways: the template's Summary and a free model's",
	about: `Waypoint writes every pull request's description from wp ship's -m, the
commits, the issue and the recorded tests. A free model rewords its Summary
from the same text (the task's title and description too), never the diff,
and a check lists any name, number, path or link it adds that the inputs
don't hold (WP-55).

The mode says what becomes of the model's version:
  shadow    (default) kept here only, for comparing; the template's is published
  model     replaces the template's when the check found nothing, the
            description is unedited and the pull request opened within 30 minutes
  template  no model is asked

Prints the counts over the window (written, failed, unsupported, published),
the model's median time and both versions' mean length; --show N adds the
last N pairs side by side. Admins; --mode is for people.`,
	examples: []string{"wp pr bodies", "wp pr bodies --show 10", "wp pr bodies --mode model"},
	run:      cmdPRBodies,
}

type prBodyPair struct {
	PR           string   `json:"pr"`
	URL          string   `json:"url"`
	Issue        string   `json:"issue"`
	State        string   `json:"state"`
	Model        string   `json:"model"`
	Summary      string   `json:"summary"`
	ModelSummary string   `json:"model_summary"`
	Unsupported  []string `json:"unsupported"`
	MS           int64    `json:"ms"`
	Used         bool     `json:"used"`
	Note         string   `json:"note"`
}

type prBodyReport struct {
	Mode          string         `json:"mode"`
	Since         string         `json:"since"`
	Counts        map[string]int `json:"counts"`
	Unsupported   int            `json:"unsupported"`
	Used          int            `json:"used"`
	MedianMS      int64          `json:"median_ms"`
	TemplateChars int            `json:"template_chars"`
	ModelChars    int            `json:"model_chars"`
	Pairs         []prBodyPair   `json:"pairs"`
}

func cmdPRBodies(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "count descriptions opened within `DURATION`: 30d, 12w (default 30d)")
	show := fs.Int("show", 0, "print the last `N` pairs side by side (at most 50)")
	mode := fs.String("mode", "", "switch to `MODE`: shadow, model or template")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	var raw []byte
	if *mode != "" {
		if raw, err = c.call("PUT", "/api/v1/admin/pr-bodies", map[string]string{"mode": *mode}); err != nil {
			return err
		}
	} else {
		d, err := parseSince(*since)
		if err != nil {
			return c.usage("%v", err)
		}
		q := url.Values{}
		if d > 0 {
			q.Set("days", strconv.Itoa(max(1, int(math.Ceil(d.Hours()/24)))))
		}
		if *show > 0 {
			q.Set("show", strconv.Itoa(*show))
		}
		if raw, err = c.call("GET", "/api/v1/admin/pr-bodies?"+q.Encode(), nil); err != nil {
			return err
		}
	}
	var r prBodyReport
	if err := jsonInto(raw, &r); err != nil {
		return err
	}
	return c.emit(raw, func() string { return r.text() })
}

func (r *prBodyReport) text() string {
	var b strings.Builder
	written := r.Counts["written"]
	fmt.Fprintf(&b, "Mode: %s. Last %s: %d written, %d failed, %d skipped, %d waiting.\n", r.Mode, r.Since, written, r.Counts["failed"], r.Counts["skipped"], r.Counts["pending"])
	if written > 0 {
		fmt.Fprintf(&b, "Of the written: %d name something the inputs don't hold, %d published. Median %.1fs; mean Summary %d chars (template) vs %d (model).\n",
			r.Unsupported, r.Used, float64(r.MedianMS)/1000, r.TemplateChars, r.ModelChars)
	}
	for _, p := range r.Pairs {
		head := p.PR
		if p.Issue != "" {
			head += " (" + p.Issue + ")"
		}
		fmt.Fprintf(&b, "\n== %s %s, %s", head, p.URL, p.State)
		if p.Model != "" {
			fmt.Fprintf(&b, " by %s", p.Model)
		}
		if p.Used {
			b.WriteString(", published")
		}
		b.WriteString("\n")
		if len(p.Unsupported) > 0 {
			b.WriteString("Not in the inputs: " + strings.Join(p.Unsupported, ", ") + "\n")
		}
		if p.Note != "" {
			b.WriteString("Note: " + p.Note + "\n")
		}
		b.WriteString("-- template:\n" + strings.TrimSpace(p.Summary) + "\n")
		if p.ModelSummary != "" {
			b.WriteString("-- model:\n" + strings.TrimSpace(p.ModelSummary) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
