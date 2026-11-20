package main

import (
	"encoding/json"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp usage meaning: the judgements of meaning about goals (WP-1035).

var usageMeaningCmd = &command{
	name: "meaning", args: "",
	summary: "how the judgements of meaning about goals stand: deciders, labels, scorers",
	about: `Three questions are asked of pairs of texts in the background: do two goals
overlap, does a goal say nothing beyond its parent, and does a task's text
already convey a parent goal. Several scorers answer each pair (shared
words, an embedding model, an NLI model, a language model), kept by hash so
only new or reworded text is scored again; a decider per question turns
the scores into yes or no.

The decider is a fixed prior until labels from what happened pick one on
held-out labels: suggestions people acted on, dismissed or left alone,
drift flags on parent goals an agent was not shown, and cases a language
model built from each goal. It is evaluated again only when a scorer
changes or the labels since show it erring more than it did held out.

Per question: the decider, the labels by source and answer, the last
evaluation and every candidate's held-out figures, the labels since and
the decider's errors on them. Then the reports sent to the gateway and how
often a goal's nearest goal by embedding is its parent. Admins only.`,
	examples: []string{"wp usage meaning", "wp usage meaning --json"},
	run:      cmdUsageMeaning,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageMeaningCmd)
}

func cmdUsageMeaning(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	raw, err := c.call("GET", "/api/v1/usage/meaning", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r core.MeaningReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return r.String()
	})
}
