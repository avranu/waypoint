package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// guardLiveReport is the server's QuotaGuardLive.
type guardLiveReport struct {
	Who      string `json:"who"`
	Settings struct {
		Off bool `json:"off"`
	} `json:"settings"`
	Policy   *guardPolicyCopy `json:"policy"`
	Baseline *struct {
		At         time.Time `json:"at"`
		Days       int       `json:"days"`
		Sessions   int       `json:"sessions"`
		SessionP50 float64   `json:"session_p50"`
		SessionP90 float64   `json:"session_p90"`
		Hours      int       `json:"hours"`
		HourP95    float64   `json:"hour_p95"`
		HourMax    float64   `json:"hour_max"`
	} `json:"baseline"`
	Hold       *guardHoldCopy `json:"hold"`
	QuietUntil *time.Time     `json:"quiet_until"`
	OverSince  *time.Time     `json:"over_since"`
	HourUSD    float64        `json:"hour_usd"`
	Trips      []struct {
		At      time.Time `json:"at"`
		Session int64     `json:"session"`
		Title   string    `json:"title"`
		Rule    string    `json:"rule"`
		Action  string    `json:"action"`
		Cost    float64   `json:"cost"`
	} `json:"trips"`
	Days int `json:"days"`
}

func guardLiveText(l *guardLiveReport, now time.Time) string {
	var b strings.Builder
	state := "on"
	if l.Settings.Off {
		state = "OFF (`wp guard set --on` turns it on)"
	}
	fmt.Fprintf(&b, "Live quota guard (%s): %s. Automation spent $%.2f in the last hour.\n", l.Who, state, l.HourUSD)
	if h := l.Hold; h != nil {
		fmt.Fprintf(&b, "HOLD (%s) since %s until %s UTC: %s `wp guard lift` ends it.\n", h.Rule, h.Since.UTC().Format("Jan 2 15:04"),
			h.Until.UTC().Format("Jan 2 15:04"), strings.TrimSpace(h.Reason))
	}
	if l.QuietUntil != nil && l.QuietUntil.After(now) {
		fmt.Fprintf(&b, "Lifted: no new hold until %s UTC.\n", l.QuietUntil.UTC().Format("Jan 2 15:04"))
	}
	if l.OverSince != nil {
		fmt.Fprintf(&b, "Your autopilot limits have held it back since %s UTC.\n", l.OverSince.UTC().Format("Jan 2 15:04"))
	}
	p := l.Policy
	from := func(f string) string { return cmpStr(p.From[f], "default") }
	usd := func(v float64) string { return guardLimit(v, "$%.0f") }
	models := "no usual models known yet, so off"
	if len(p.Models) > 0 {
		models = "spend on a model priced above " + strings.Join(p.Models, ", ")
	}
	pace := "off"
	if p.Pace {
		pace = "on"
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  rule\tline\tfrom\tmeasures")
	for _, r := range [][4]string{
		{"churn", usd(p.ChurnUSD), from("churn_usd"), "an automation session's spend since its last change"},
		{"session", usd(p.SessionUSD), from("session_usd"), "its spend since automation gave it its work"},
		{"hours", guardLimit(p.Hours, "%.0fh"), from("hours"), "hours automation has driven it"},
		{"idle", guardLimit(p.IdleHours, "%.0fh"), from("idle_hours"), "your session untyped this long, then the session line"},
		{"model", usd(p.ModelUSD), from("model_usd"), models},
		{"burst", usd(p.BurstUSD), from("burst_usd"), "automation's spend in an hour (holds automation)"},
		{"pace", pace, from("pace"), "holds automation while your limits hold autopilot back and the week rises 3 points"},
	} {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r[0], r[1], r[2], r[3])
	}
	_ = tw.Flush()
	b.WriteString("A session is told at a line and stopped at 1.5 times it (3 times for model); list prices.\n")
	if bl := l.Baseline; bl != nil {
		fmt.Fprintf(&b, "Usual usage (%d days to %s UTC): %d automation sessions, p50 $%.2f, p90 $%.2f; %d hours, p95 $%.2f, max $%.2f.\n",
			bl.Days, bl.At.UTC().Format("Jan 2 15:04"), bl.Sessions, bl.SessionP50, bl.SessionP90, bl.Hours, bl.HourP95, bl.HourMax)
	}
	if len(l.Trips) == 0 {
		fmt.Fprintf(&b, "No trips in the last %d days.\n", l.Days)
		return b.String()
	}
	fmt.Fprintf(&b, "Trips, last %d days:\n", l.Days)
	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, t := range l.Trips {
		what := t.Title
		if t.Session > 0 {
			what = strings.TrimSpace(fmt.Sprintf("session %d %s", t.Session, t.Title))
		}
		cost := ""
		if t.Cost > 0 {
			cost = fmt.Sprintf("$%.2f", t.Cost)
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", t.At.UTC().Format("Jan 2 15:04"), t.Action, t.Rule, what, cost)
	}
	_ = tw.Flush()
	return b.String()
}

// guardSetBody is the PUT for wp guard set; given: the flags given.
func guardSetBody(given map[string]bool, nums map[string]float64, models, pace, def string, off, on bool) (map[string]any, error) {
	fields := map[string]string{"churn": "churn_usd", "session": "session_usd", "hours": "hours", "idle-hours": "idle_hours", "model-usd": "model_usd", "burst": "burst_usd"}
	set := map[string]any{}
	for f, field := range fields {
		if given[f] {
			if nums[f] < 0 {
				return nil, fmt.Errorf("--%s: 0 or more", f)
			}
			set[field] = nums[f]
		}
	}
	if models != "" {
		set["models"] = splitList(models)
	}
	switch strings.ToLower(pace) {
	case "":
	case "on":
		set["pace"] = true
	case "off":
		set["pace"] = false
	default:
		return nil, fmt.Errorf("--pace: on or off")
	}
	if off && on {
		return nil, fmt.Errorf("--off or --on, not both")
	}
	if len(set) == 0 && !off && !on && def == "" {
		return nil, fmt.Errorf("nothing to change: give a line, --off, --on or --default")
	}
	in := map[string]any{"settings": set, "merge": true}
	if off || on {
		in["on"] = on
	}
	if def != "" {
		in["clear"] = splitList(def)
	}
	return in, nil
}

func cmdUsageGuardSet(c *cli, args []string) error {
	fs := c.flags()
	off := fs.Bool("off", false, "turn the live guard off")
	on := fs.Bool("on", false, "turn it on")
	nums := map[string]*float64{
		"churn":      fs.Float64("churn", 0, "the churn line (`USD`; 0 turns a rule off)"),
		"session":    fs.Float64("session", 0, "the session line (`USD`)"),
		"hours":      fs.Float64("hours", 0, "the hours line (`H`)"),
		"idle-hours": fs.Float64("idle-hours", 0, "the idle line (`H`)"),
		"model-usd":  fs.Float64("model-usd", 0, "the model line (`USD`)"),
		"burst":      fs.Float64("burst", 0, "the burst line (`USD` in an hour)"),
	}
	models := fs.String("models", "", "the models automation may use (`A,B`, id prefixes)")
	pace := fs.String("pace", "", "hold on pace: `on|off`")
	def := fs.String("default", "", "put lines back to the usual ones (`FIELD,…|all`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	vals := map[string]float64{}
	for k, v := range nums {
		vals[k] = *v
	}
	in, err := guardSetBody(given, vals, *models, *pace, *def, *off, *on)
	if err != nil {
		return c.usage("%s", err.Error())
	}
	raw, err := c.call("PUT", "/api/v1/usage/guard/settings", in)
	if err != nil {
		return err
	}
	return c.emitGuardLive(raw)
}

func cmdUsageGuardLift(c *cli, args []string) error {
	fs := c.flags()
	quiet := fs.String("for", "2h", "keep a new hold off this long (`DURATION`, at most a week)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	raw, err := c.call("POST", "/api/v1/usage/guard/lift", map[string]string{"for": *quiet})
	if err != nil {
		return err
	}
	return c.emitGuardLive(raw)
}

func (c *cli) emitGuardLive(raw []byte) error {
	return c.emit(raw, func() string {
		var l guardLiveReport
		if json.Unmarshal(raw, &l) != nil || l.Policy == nil {
			return string(raw)
		}
		return guardLiveText(&l, time.Now())
	})
}
