package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var riskCmd = &command{
	name: "risk", group: "Knowledge", args: "THING...",
	summary: "how far to trust something: risk low, medium or high, and why",
	about: `Computed from who wrote it and who checked it; no model is asked.
  high    a forge stranger's text or PR, or text the safety screen held
  medium  written by an agent, or AI-looking text under a person's login,
          and not yet reviewed (wp review) or approved
  low     neither
A pull request also carries its tasks' risk. WQL: risk:high, risk:>=medium.

THING is APP-12, comment:123, PL13, term:45, ADR7 or design:12, pr:34,
prompt:88 or automation:5.`,
	examples: []string{"wp risk APP-12", "wp risk pr:34 ADR7", `wp ls "risk:high"`},
	run:      cmdRisk,
}

func cmdRisk(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("what? Give an issue key, comment:ID, page, term:ID, design note, pr:N, prompt:ID or automation:ID")
	}
	home := ""
	if sf.project != nil && *sf.project != "" {
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		home, _ = sc.home()
	} else {
		home = c.homeProject()
	}
	return c.each(listArgs(pos), false, func(ref string) error {
		targets, err := c.riskTargets(sf, ref)
		if err != nil {
			return err
		}
		var raw []byte
		for _, t := range targets {
			path := "/api/v1/risk/" + t[0] + "/" + esc(t[1])
			if home != "" {
				path += "?project=" + url.QueryEscape(home)
			}
			var ae *apiError
			if raw, err = c.call("GET", path, nil); err == nil || !errors.As(err, &ae) || ae.status != 404 {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		return c.emit(raw, func() string {
			var r struct {
				Level   string `json:"level"`
				Reasons []struct {
					Factor, Level, Text string
				} `json:"reasons"`
			}
			if err := jsonInto(raw, &r); err != nil {
				return string(raw)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s: %s\n", ref, r.Level)
			for _, x := range r.Reasons {
				fmt.Fprintf(&b, "  %s (%s): %s\n", x.Level, x.Factor, x.Text)
			}
			return b.String()
		})
	})
}

// riskTargets reads what wp risk was given: the (kind, ref) pairs for the
// API it could mean, most likely first.
func (c *cli) riskTargets(sf scopeFlags, ref string) ([][2]string, error) {
	k, v, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ref)), ":")
	switch k {
	case "pr", "pull":
		n, err := strconv.Atoi(strings.TrimPrefix(v, "#"))
		if err != nil || n < 1 {
			return nil, c.usage("%q isn't a pull request number", ref)
		}
		d, _, err := c.loadPR(sf, []string{strconv.Itoa(n)}, "")
		if err != nil {
			return nil, err
		}
		return [][2]string{{"pull", strconv.FormatInt(d.ID, 10)}}, nil
	case "design":
		return [][2]string{{"design", v}}, nil
	case "prompt", "message", "automation":
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "#"), 10, 64)
		if err != nil || n < 1 {
			return nil, c.usage("%q isn't a %s id", ref, k)
		}
		if k == "message" {
			k = "prompt"
		}
		return [][2]string{{k, strconv.FormatInt(n, 10)}}, nil
	}
	if designCodeRE.MatchString(strings.TrimSpace(ref)) {
		return [][2]string{{"design", strings.TrimSpace(ref)}}, nil
	}
	if t := reviewTargets(ref); len(t) > 0 && t[0][0] != "close" {
		return t, nil
	}
	return nil, c.usage("%q isn't an issue key, comment:ID, page code or number, term:ID, design note, pr:N, prompt:ID or automation:ID", ref)
}
