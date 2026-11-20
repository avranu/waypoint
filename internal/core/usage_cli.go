package core

// How agents use wp (WP-618): the counters `wp usage sync` reads from the
// transcripts, by week and by repo, for Agents → wp CLI and `wp usage cli`.
// The upload holds command names, flag names and fixed labels only, and
// ingest keeps to those shapes whatever a client sends.

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	cliTopCommands = 40
	cliTopUnknown  = 30
	cliTopDetails  = 8
)

var (
	cliHeadRE    = lazyre.New(`^wp( [a-z][a-z0-9-]{0,29}){0,5}$`)
	cliUnknownRE = lazyre.New(`^wp( [a-z][a-z0-9-]{0,29}){1,5}$`)
	cliFlagRE    = lazyre.New(`^--?[A-Za-z][A-Za-z0-9-]{0,29}$`)
	// a follow label is a command head: up to five short words, one space apart
	cliFollowRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9:_./+@-]{0,29}( -{0,2}[A-Za-z0-9][A-Za-z0-9:_./+@-]{0,29}){0,4}$`)
)

var (
	cliErrorClasses = []string{"unknown_flag", "unknown_command", "ambiguous_code", "usage"}
	cliSignals      = []string{"curl_forge", "sleep", "sleep_ci", "gh_tea"}
)

// UsageCLIRow is one counter of one hour of one session.
type UsageCLIRow struct {
	Hour    int64  `json:"h"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Agent   string `json:"agent,omitempty"` // main, sub
	Kind    string `json:"kind"`
	Head    string `json:"head,omitempty"`
	Detail  string `json:"detail,omitempty"`
	N       int64  `json:"n"`
}

// cliRowValid keeps each kind to the shape it is documented to have, so
// what is stored is never more than command names, flag names and labels.
func cliRowValid(r UsageCLIRow) bool {
	head := cliHeadRE.MatchString(r.Head)
	switch r.Kind {
	case "file", "bash":
		return r.Head == "" && r.Detail == ""
	case "wp", "help":
		return head && r.Detail == ""
	case "flag", "unknown_flag":
		return head && cliFlagRE.MatchString(r.Detail)
	case "error":
		return head && slices.Contains(cliErrorClasses, r.Detail)
	case "unknown_command":
		return head && cliUnknownRE.MatchString(r.Detail)
	case "follow":
		return head && cliFollowRE.MatchString(r.Detail)
	case "signal":
		return r.Head == "" && slices.Contains(cliSignals, r.Detail)
	}
	return false
}

func (t *tx) applyCLI(ctx context.Context, uid int64, machine string, rows []UsageCLIRow, maxHour int64) error {
	for _, r := range rows {
		if r.Hour < usageMinHour || r.Hour > maxHour || r.Session == "" || r.N <= 0 || !cliRowValid(r) {
			continue
		}
		agent := "main"
		if r.Agent == "sub" {
			agent = "sub"
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO cli_hours(user_id, hour, machine, session, repo, model, agent, kind, head, detail, n)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, hour, machine, session, repo, model, agent, kind, head, detail) DO UPDATE SET n = n + excluded.n`,
			uid, hourStart(r.Hour), machine, clip(r.Session, 80), clip(r.Repo, 200), clip(r.Model, 100), agent, r.Kind, r.Head, r.Detail, r.N); err != nil {
			return err
		}
	}
	return nil
}

// UsageCLIPeriod is the measures of a stretch of time: counts, and rates
// that are null when there's nothing to divide by.
type UsageCLIPeriod struct {
	Sessions     int64 `json:"sessions"`      // transcripts begun, subagents included
	MainSessions int64 `json:"main_sessions"` // of those, the agents people started
	Bash         int64 `json:"bash"`
	WP           int64 `json:"wp"`
	Help         int64 `json:"help"`
	HelpMain     int64 `json:"help_main"`
	UsageErrors  int64 `json:"usage_errors"`
	CurlForge    int64 `json:"curl_forge"`
	Sleep        int64 `json:"sleep"`
	SleepCI      int64 `json:"sleep_ci"`
	GhTea        int64 `json:"gh_tea"`

	BashPerSession  *float64 `json:"bash_per_session"`
	WPPerSession    *float64 `json:"wp_per_session"`
	HelpRate        *float64 `json:"help_rate"`  // help lookups per wp call
	ErrorRate       *float64 `json:"error_rate"` // usage errors per wp call
	HelpPerMain     *float64 `json:"help_per_main"`
	CurlPerSession  *float64 `json:"curl_per_session"`
	SleepPerSession *float64 `json:"sleep_per_session"`
}

func cliRatio(a, b int64) *float64 {
	if b == 0 {
		return nil
	}
	v := float64(a) / float64(b)
	return &v
}

func (p *UsageCLIPeriod) rates() {
	p.BashPerSession = cliRatio(p.Bash, p.Sessions)
	p.WPPerSession = cliRatio(p.WP, p.Sessions)
	p.HelpRate = cliRatio(p.Help, p.WP)
	p.ErrorRate = cliRatio(p.UsageErrors, p.WP)
	p.HelpPerMain = cliRatio(p.HelpMain, p.MainSessions)
	p.CurlPerSession = cliRatio(p.CurlForge, p.Sessions)
	p.SleepPerSession = cliRatio(p.Sleep, p.Sessions)
}

func (p *UsageCLIPeriod) add(agent, kind, detail string, n int64) {
	switch kind {
	case "file":
		p.Sessions += n
		if agent == "main" {
			p.MainSessions += n
		}
	case "bash":
		p.Bash += n
	case "wp":
		p.WP += n
	case "help":
		p.Help += n
		if agent == "main" {
			p.HelpMain += n
		}
	case "error":
		p.UsageErrors += n
	case "signal":
		switch detail {
		case "curl_forge":
			p.CurlForge += n
		case "sleep":
			p.Sleep += n
		case "sleep_ci":
			p.SleepCI += n
		case "gh_tea":
			p.GhTea += n
		}
	}
}

// UsageCLIWeek is a week's measures; Week is its Monday in the reader's zone.
type UsageCLIWeek struct {
	Week string `json:"week"`
	UsageCLIPeriod
}

// UsageCLICount is a name and how often.
type UsageCLICount struct {
	Name string `json:"name"`
	N    int64  `json:"n"`
}

// UsageCLIFollow is what ran within the next Bash calls after a command.
type UsageCLIFollow struct {
	Name  string   `json:"name"`
	N     int64    `json:"n"`
	Share *float64 `json:"share"` // of the command's calls, help aside
}

// UsageCLICommand is one wp command.
type UsageCLICommand struct {
	Head    string           `json:"head"`
	Calls   int64            `json:"calls"`
	Help    int64            `json:"help"`
	Errors  int64            `json:"errors"`
	Flags   []UsageCLICount  `json:"flags"`
	Follows []UsageCLIFollow `json:"follows"`
}

// UsageCLIUnknownFlag is a flag a command was given and doesn't have.
type UsageCLIUnknownFlag struct {
	Head string `json:"head"`
	Flag string `json:"flag"`
	N    int64  `json:"n"`
}

// UsageCLIRepo is one repo's use of wp in the period.
type UsageCLIRepo struct {
	Repo         string   `json:"repo"`
	Sessions     int64    `json:"sessions"`
	WP           int64    `json:"wp"`
	Help         int64    `json:"help"`
	Errors       int64    `json:"errors"`
	WPPerSession *float64 `json:"wp_per_session"`
	HelpRate     *float64 `json:"help_rate"`
	ErrorRate    *float64 `json:"error_rate"`
}

// UsageCLIReport answers Agents → wp CLI and `wp usage cli`.
type UsageCLIReport struct {
	From            time.Time             `json:"from"`
	To              time.Time             `json:"to"`
	Days            int                   `json:"days"`
	Who             string                `json:"who"`
	Repo            string                `json:"repo,omitempty"`
	HasData         bool                  `json:"has_data"` // anything synced, in any period
	Total           UsageCLIPeriod        `json:"total"`
	Weeks           []UsageCLIWeek        `json:"weeks"`
	Commands        []UsageCLICommand     `json:"commands"`
	UnknownFlags    []UsageCLIUnknownFlag `json:"unknown_flags"`
	UnknownCommands []UsageCLICount       `json:"unknown_commands"`
	ErrorClasses    []UsageCLICount       `json:"error_classes"`
	Repos           []UsageCLIRepo        `json:"repos"`
}

func cliMonday(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
}

// UsageCLI reports how agents used wp: repo narrows everything but the list
// of repos, which stays whole so another can be picked.
func (s *Service) UsageCLI(ctx context.Context, actor *Actor, q UsageQuery, repo string) (*UsageCLIReport, error) {
	sc, loc, first, label, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	repo = strings.TrimSpace(repo)
	o := &UsageCLIReport{From: first.UTC(), To: msTime(sc.to - 1), Days: q.Days, Who: label, Repo: repo, Weeks: []UsageCLIWeek{}, Commands: []UsageCLICommand{},
		UnknownFlags: []UsageCLIUnknownFlag{}, UnknownCommands: []UsageCLICount{}, ErrorClasses: []UsageCLICount{}, Repos: []UsageCLIRepo{}}
	if o.HasData, err = s.cliHasData(ctx, sc); err != nil {
		return nil, err
	}
	if o.Repos, err = s.cliRepos(ctx, sc); err != nil {
		return nil, err
	}
	if repo != "" {
		// Inside the project's repos, when a project is chosen too.
		inScope := !sc.repoFilter || slices.ContainsFunc(sc.repos, func(r string) bool { return strings.EqualFold(r, repo) })
		sc.repos, sc.repoFilter = nil, true
		if inScope {
			sc.repos = []string{repo}
		}
	}
	if err := s.cliWeeks(ctx, sc, loc, first, o); err != nil {
		return nil, err
	}
	if err := s.cliDetails(ctx, sc, o); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Service) cliHasData(ctx context.Context, sc *usageScope) (bool, error) {
	all := &usageScope{uids: sc.uids, everyone: sc.everyone, from: 0, to: sc.to}
	where, args := all.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT 1 FROM cli_hours WHERE `+where+` LIMIT 1`, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

func (s *Service) cliRepos(ctx context.Context, sc *usageScope) ([]UsageCLIRepo, error) {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT repo, kind, SUM(n) FROM cli_hours WHERE `+where+` AND kind IN ('file','wp','help','error') GROUP BY repo, kind`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*UsageCLIRepo{}
	for rows.Next() {
		var repo, kind string
		var n int64
		if err := rows.Scan(&repo, &kind, &n); err != nil {
			return nil, err
		}
		r := by[repo]
		if r == nil {
			r = &UsageCLIRepo{Repo: repo}
			by[repo] = r
		}
		switch kind {
		case "file":
			r.Sessions += n
		case "wp":
			r.WP += n
		case "help":
			r.Help += n
		case "error":
			r.Errors += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]UsageCLIRepo, 0, len(by))
	for _, r := range by {
		if r.WP == 0 {
			continue // somewhere agents worked without wp says nothing about it
		}
		r.WPPerSession, r.HelpRate, r.ErrorRate = cliRatio(r.WP, r.Sessions), cliRatio(r.Help, r.WP), cliRatio(r.Errors, r.WP)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WP != out[j].WP {
			return out[i].WP > out[j].WP
		}
		return out[i].Repo < out[j].Repo
	})
	return out, nil
}

// cliWeeks fills the total and every week of the period, those without data
// too, so a gap in the charts is a gap.
func (s *Service) cliWeeks(ctx context.Context, sc *usageScope, loc *time.Location, first time.Time, o *UsageCLIReport) error {
	byWeek := map[string]*UsageCLIPeriod{}
	var order []string
	end := time.UnixMilli(sc.to).In(loc)
	for w := cliMonday(first, loc); w.Before(end); w = w.AddDate(0, 0, 7) {
		key := w.Format(dateLayout)
		byWeek[key] = &UsageCLIPeriod{}
		order = append(order, key)
	}
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT hour, agent, kind, detail, SUM(n) FROM cli_hours WHERE `+where+`
		AND kind IN ('file','bash','wp','help','error','signal') GROUP BY hour, agent, kind, detail`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hour, n int64
		var agent, kind, detail string
		if err := rows.Scan(&hour, &agent, &kind, &detail, &n); err != nil {
			return err
		}
		o.Total.add(agent, kind, detail, n)
		if p := byWeek[cliMonday(time.UnixMilli(hour), loc).Format(dateLayout)]; p != nil {
			p.add(agent, kind, detail, n)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	o.Total.rates()
	for _, key := range order {
		p := byWeek[key]
		p.rates()
		o.Weeks = append(o.Weeks, UsageCLIWeek{Week: key, UsageCLIPeriod: *p})
	}
	return nil
}

func cliByCount(c []UsageCLICount) []UsageCLICount {
	sort.Slice(c, func(i, j int) bool {
		if c[i].N != c[j].N {
			return c[i].N > c[j].N
		}
		return c[i].Name < c[j].Name
	})
	return c
}

// cliDetails fills the commands, with their flags and what follows them,
// and the things that went wrong.
func (s *Service) cliDetails(ctx context.Context, sc *usageScope, o *UsageCLIReport) error {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, head, detail, SUM(n) FROM cli_hours WHERE `+where+`
		AND kind IN ('wp','help','flag','error','unknown_flag','unknown_command','follow') GROUP BY kind, head, detail`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type acc struct {
		UsageCLICommand
		flags, follows []UsageCLICount
	}
	by := map[string]*acc{}
	at := func(head string) *acc {
		a := by[head]
		if a == nil {
			a = &acc{UsageCLICommand: UsageCLICommand{Head: head, Flags: []UsageCLICount{}, Follows: []UsageCLIFollow{}}}
			by[head] = a
		}
		return a
	}
	classes := map[string]int64{}
	var unknownCmds []UsageCLICount
	for rows.Next() {
		var kind, head, detail string
		var n int64
		if err := rows.Scan(&kind, &head, &detail, &n); err != nil {
			return err
		}
		switch kind {
		case "wp":
			at(head).Calls += n
		case "help":
			at(head).Help += n
		case "flag":
			a := at(head)
			a.flags = append(a.flags, UsageCLICount{Name: detail, N: n})
		case "error":
			at(head).Errors += n
			classes[detail] += n
		case "unknown_flag":
			o.UnknownFlags = append(o.UnknownFlags, UsageCLIUnknownFlag{Head: head, Flag: detail, N: n})
		case "unknown_command":
			unknownCmds = append(unknownCmds, UsageCLICount{Name: detail, N: n})
		case "follow":
			a := at(head)
			a.follows = append(a.follows, UsageCLICount{Name: detail, N: n})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cmds := make([]UsageCLICommand, 0, len(by))
	for _, a := range by {
		if a.Calls == 0 {
			continue // only counted a follow-up, in a period that began after the call
		}
		cliByCount(a.flags)
		a.Flags = append(a.Flags, a.flags[:min(len(a.flags), cliTopDetails)]...)
		cliByCount(a.follows)
		for _, f := range a.follows[:min(len(a.follows), cliTopDetails)] {
			a.Follows = append(a.Follows, UsageCLIFollow{Name: f.Name, N: f.N, Share: cliRatio(f.N, a.Calls-a.Help)})
		}
		cmds = append(cmds, a.UsageCLICommand)
	}
	sort.Slice(cmds, func(i, j int) bool {
		if cmds[i].Calls != cmds[j].Calls {
			return cmds[i].Calls > cmds[j].Calls
		}
		return cmds[i].Head < cmds[j].Head
	})
	o.Commands = cmds[:min(len(cmds), cliTopCommands)]
	sort.Slice(o.UnknownFlags, func(i, j int) bool {
		a, b := o.UnknownFlags[i], o.UnknownFlags[j]
		if a.N != b.N {
			return a.N > b.N
		}
		return a.Head+a.Flag < b.Head+b.Flag
	})
	o.UnknownFlags = o.UnknownFlags[:min(len(o.UnknownFlags), cliTopUnknown)]
	cliByCount(unknownCmds)
	o.UnknownCommands = append(o.UnknownCommands, unknownCmds[:min(len(unknownCmds), cliTopUnknown)]...)
	for name, n := range classes {
		o.ErrorClasses = append(o.ErrorClasses, UsageCLICount{Name: name, N: n})
	}
	cliByCount(o.ErrorClasses)
	return nil
}
