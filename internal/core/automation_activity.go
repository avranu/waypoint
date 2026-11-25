package core

import (
	"fmt"
	"strings"
)

func activityText(o *Overview) string {
	var b strings.Builder
	list := func(title string, l SummaryList) {
		if l.Count == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s (%d)\n", title, l.Count)
		var parts []string
		for _, p := range l.Parts {
			parts = append(parts, p.Label)
		}
		if len(parts) > 0 {
			b.WriteString(strings.Join(parts, ", ") + "\n")
		}
		for _, it := range l.Items {
			fmt.Fprintf(&b, "- %s %s", it.Ref, it.Title)
			if it.Detail != "" {
				fmt.Fprintf(&b, " (%s)", it.Detail)
			}
			if it.Agent != "" {
				fmt.Fprintf(&b, " by %s", it.Agent)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	list("Done", o.Summary.Done)
	list("In progress", o.Summary.Doing)
	list("Deferred", o.Summary.Deferred)
	list("Waiting on a person", o.Summary.Waiting)
	if o.Pulls.Total > 0 {
		fmt.Fprintf(&b, "%d pull requests are open.\n", o.Pulls.Total)
	}
	return strings.TrimSpace(b.String())
}

func init() {
	registerAction(ActionDef{Name: "agent_activity", Label: "Look up what agents did", Group: "Agents", Phrase: "look up what agents did in the last {days} days",
		Doc: "Read what the agents did, are doing, deferred and wait on you for, over the last `days` days (the project's, or for an admin automation every project's, as the person it acts as can see). Outputs Text (markdown), Done, Doing, Waiting, Deferred counts",
		Params: []ParamDef{
			{Name: "days", Label: "Days", Type: PNumber, Default: 1, Min: f64(1), Max: f64(31)},
			{Name: "agent", Label: "Only this agent", Type: PText, Advanced: true, Placeholder: "claude", Help: "An agent spec; empty is every agent."},
		},
		Outputs: []OutputDef{{Name: "Text", Type: "text", Doc: "a markdown account"}, {Name: "Done", Type: "number"}, {Name: "Doing", Type: "number"},
			{Name: "Waiting", Type: "number", Doc: "waiting on a person"}, {Name: "Deferred", Type: "number"}},
		Run: func(c *StepCtx) error {
			q := OverviewQuery{Days: c.Int("days", 1), Agent: c.Str("agent")}
			if pid := c.stepProject(); pid != 0 {
				p, err := c.S.projectByID(c.Ctx, c.S.db.R, pid)
				if err != nil {
					return err
				}
				q.Project = p.Key
			}
			o, err := c.S.AgentOverview(c.Ctx, c.Actor, q)
			if err != nil {
				return err
			}
			text := activityText(o)
			if text == "" {
				text = "Nothing happened."
			}
			c.Set("Text", text)
			c.Set("Done", o.Summary.Done.Count)
			c.Set("Doing", o.Summary.Doing.Count)
			c.Set("Waiting", o.Summary.Waiting.Count)
			c.Set("Deferred", o.Summary.Deferred.Count)
			return nil
		}})
}
