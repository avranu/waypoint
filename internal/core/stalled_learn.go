package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	stallEvidenceDays   = 28
	stallEvidenceMin    = 10
	stallPrecisionNeeds = 0.5
	stallColdNeeds      = 0.25
)

var stallLadder = []float64{1, 1.5, 2, 3, 4}

// stallThresholds is what applies to a kind of job now.
type stallThresholds struct {
	Quiet    int    // minutes without progress before its session is told
	Why      string // why Quiet is longer than the setting ("" when it isn't)
	ColdWake bool   // a cold session may be woken for it
	ColdWhy  string // why not
}

// stallFact is one settled spell as the evidence counts it.
type stallFact struct {
	Class, Outcome, How string
	Length              int // minutes quiet, from its last progress to its end
}

func (s *Service) stallFacts(ctx context.Context, userID int64) []stallFact {
	since := s.now().Add(-stallEvidenceDays * 24 * time.Hour).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT class, outcome, told_how, (settled_at - quiet_from) / 60000 FROM stalled_jobs
		WHERE user_id = ? AND settled_at >= ? AND outcome IN ('acted', 'self', 'withdrawn')
		AND (skipped = '' OR told_at IS NOT NULL)`, userID, since)
	if err != nil {
		s.log.WarnContext(ctx, "stalled jobs: read the evidence", "err", err)
		return nil
	}
	defer rows.Close()
	var out []stallFact
	for rows.Next() {
		var f stallFact
		if rows.Scan(&f.Class, &f.Outcome, &f.How, &f.Length) == nil {
			out = append(out, f)
		}
	}
	return out
}

// stallThreshold is the quiet time and cold-wake rule for userID's jobs of
// class under pol.
func (s *Service) stallThreshold(ctx context.Context, userID int64, class string, pol StallPolicy) stallThresholds {
	th := stallThresholds{Quiet: pol.Quiet, ColdWake: true}
	if pol.Fixed {
		return th
	}
	facts := s.stallFacts(ctx, userID)
	q := stallQuietFor(facts, class, pol.Quiet)
	th.Quiet, th.Why = q.Quiet, q.Why
	th.ColdWake, th.ColdWhy = stallColdFor(facts)
	return th
}

// stallQuiet is the quiet time the evidence sets, with its figures.
type stallQuiet struct {
	Quiet  int
	Why    string
	Sample int // spells that reached the setting
	Stuck  int // of them, stopped after a notice
}

// stallQuietFor is the quiet time for class from facts: the class's own
// spells, else every class's.
func stallQuietFor(facts []stallFact, class string, base int) stallQuiet {
	var own []stallFact
	for _, f := range facts {
		if f.Class == class {
			own = append(own, f)
		}
	}
	whose := "`" + class + "` jobs'"
	if class == "" {
		whose = "these jobs'"
	}
	if q := stallQuietFrom(own, base); q.Sample >= stallEvidenceMin {
		if q.Quiet > base {
			q.Why = stallWhy(q, whose)
		}
		return q
	}
	q := stallQuietFrom(facts, base)
	if q.Sample < stallEvidenceMin {
		return stallQuiet{Quiet: base, Sample: q.Sample, Stuck: q.Stuck}
	}
	if q.Quiet > base {
		q.Why = stallWhy(q, "all jobs'")
	}
	return q
}

func stallQuietFrom(facts []stallFact, base int) stallQuiet {
	reached := func(t int) (stuck, n int) {
		for _, f := range facts {
			switch {
			case f.Outcome == "acted":
				stuck++
				n++
			case f.Length >= t:
				n++
			}
		}
		return stuck, n
	}
	stuck, n := reached(base)
	q := stallQuiet{Quiet: base, Sample: n, Stuck: stuck}
	if n < stallEvidenceMin {
		return q
	}
	for i, k := range stallLadder {
		t := int(float64(base)*k + 0.5)
		st, of := reached(t)
		if of > 0 && float64(st)/float64(of) >= stallPrecisionNeeds || i == len(stallLadder)-1 {
			q.Quiet = t
			return q
		}
	}
	return q
}

func stallWhy(q stallQuiet, whose string) string {
	return fmt.Sprintf("%s quiet time is %s: %d of %d spells in %d days that reached the setting were stuck", whose, minText(q.Quiet), q.Stuck, q.Sample, stallEvidenceDays)
}

// stallColdFor: cold wakes go on unless enough of them were seldom acted on.
func stallColdFor(facts []stallFact) (bool, string) {
	acted, n := 0, 0
	for _, f := range facts {
		if f.How == stallColdWake {
			n++
			if f.Outcome == "acted" {
				acted++
			}
		}
	}
	if n < stallEvidenceMin || float64(acted)/float64(n) >= stallColdNeeds {
		return true, ""
	}
	return false, fmt.Sprintf("%d of %d cold wakes in %d days led to the job being stopped", acted, n, stallEvidenceDays)
}

// jobClass is the kind of a background command, for the evidence: its
// program (wrappers like cd, env, timeout and nohup aside), with the
// subcommand for tools whose subcommands differ ("go test", "git push"),
// or "loop" for a shell loop (a wait for something).
func jobClass(cmd string) string {
	for _, part := range strings.FieldsFunc(cmd, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' }) {
		words := strings.Fields(part)
		for len(words) > 0 {
			w := words[0]
			switch {
			case w == "cd" || w == "export" || w == "source" || w == ".":
				words = nil
			case w == "env" || w == "nohup" || w == "time" || w == "nice" || w == "exec" || w == "command" || strings.Contains(w, "=") && !strings.HasPrefix(w, "="):
				words = words[1:]
			case w == "timeout":
				words = words[1:]
				for len(words) > 0 && (strings.HasPrefix(words[0], "-") || len(words[0]) > 0 && words[0][0] >= '0' && words[0][0] <= '9') {
					words = words[1:]
				}
			default:
				return classOf(words)
			}
		}
	}
	return ""
}

var stallSubcommands = []string{"git", "go", "npm", "npx", "wp", "docker", "make", "cargo", "pnpm", "yarn", "kubectl", "gh", "python", "python3", "uv"}

func classOf(words []string) string {
	w := words[0]
	if i := strings.LastIndexByte(w, '/'); i >= 0 && i < len(w)-1 {
		w = w[i+1:]
	}
	w = strings.Trim(w, `"'()`)
	switch w {
	case "while", "until", "for":
		return "loop"
	case "sleep":
		return "sleep"
	}
	if slices.Contains(stallSubcommands, w) {
		for _, sub := range words[1:] {
			if !strings.HasPrefix(sub, "-") {
				return excerpt(w+" "+strings.Trim(sub, `"'`), 40)
			}
		}
	}
	return excerpt(w, 40)
}

// StalledReport is the figures on quiet background jobs: how each kind's
// spells went, and what the evidence sets now.
type StalledReport struct {
	Who       string               `json:"who"`
	Days      int                  `json:"days"`
	Policy    StallPolicy          `json:"policy"` // for every project
	MinSample int                  `json:"min_sample"`
	Needs     float64              `json:"precision_needs"`
	ColdNeeds float64              `json:"cold_needs"`
	Classes   []StalledClassFigure `json:"classes"`
	ColdWakes int                  `json:"cold_wakes"`
	ColdActed int                  `json:"cold_acted"`
	// ColdWake, ColdWhy: whether cold sessions are woken now (one person's).
	ColdWake *bool  `json:"cold_wake,omitempty"`
	ColdWhy  string `json:"cold_why,omitempty"`
}

// StalledClassFigure is one kind of job's spells.
type StalledClassFigure struct {
	Class     string `json:"class"`
	Spells    int    `json:"spells"`
	Told      int    `json:"told"`
	Woke      int    `json:"woke"`
	AtStop    int    `json:"at_stop"`
	Passive   int    `json:"passive"`
	KeepWarm  int    `json:"keep_warm"`
	ColdWake  int    `json:"cold_wake"`
	Acted     int    `json:"acted"`
	Self      int    `json:"self"`
	Withdrawn int    `json:"withdrawn"`
	Unknown   int    `json:"unknown"`
	Open      int    `json:"open"`
	// MedianQuiet: minutes quiet, median, of the spells that ended by
	// themselves.
	MedianQuiet int `json:"median_quiet"`
	// QuietNow: the quiet time its jobs get now (one person's, under the
	// policy for every project); 0 for everyone's.
	QuietNow int `json:"quiet_now,omitempty"`
}

// StalledUsage reports quiet background jobs' spells and outcomes.
func (s *Service) StalledUsage(ctx context.Context, actor *Actor, q UsageQuery) (*StalledReport, error) {
	uids, everyone, label, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, err
	}
	if q.Days <= 0 {
		q.Days = stallEvidenceDays
	}
	q.Days = min(q.Days, 365)
	pol, _ := s.globalStallPolicy(ctx, s.db.R)
	rep := &StalledReport{Who: label, Days: q.Days, Policy: pol, MinSample: stallEvidenceMin, Needs: stallPrecisionNeeds, ColdNeeds: stallColdNeeds, Classes: []StalledClassFigure{}}
	who, args := "1", []any{}
	if !everyone {
		who, args = "user_id = ?", []any{uids[0]}
	}
	since := s.nowMS() - int64(q.Days)*24*time.Hour.Milliseconds()
	// #nosec G202 -- who is a fixed condition; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT class, told_at IS NOT NULL, told_how, outcome, settled_at IS NULL, (coalesce(settled_at, seen_at) - quiet_from) / 60000
		FROM stalled_jobs WHERE `+who+` AND seen_at >= ? ORDER BY class`, append(args, since)...)
	if err != nil {
		return nil, err
	}
	byClass := map[string]*StalledClassFigure{}
	lengths := map[string][]int{}
	for rows.Next() {
		var class, how, outcome string
		var told, open bool
		var length int
		if err := rows.Scan(&class, &told, &how, &outcome, &open, &length); err != nil {
			rows.Close()
			return nil, err
		}
		f := byClass[class]
		if f == nil {
			f = &StalledClassFigure{Class: class}
			byClass[class] = f
		}
		f.Spells++
		if told {
			f.Told++
		}
		switch how {
		case stallWoke:
			f.Woke++
		case stallAtStop:
			f.AtStop++
		case stallPassive:
			f.Passive++
		case stallKeepWarm:
			f.KeepWarm++
		case stallColdWake:
			f.ColdWake++
			rep.ColdWakes++
			if outcome == "acted" {
				rep.ColdActed++
			}
		}
		switch {
		case open:
			f.Open++
		case outcome == "acted":
			f.Acted++
		case outcome == "self":
			f.Self++
			lengths[class] = append(lengths[class], length)
		case outcome == "withdrawn":
			f.Withdrawn++
			lengths[class] = append(lengths[class], length)
		default:
			f.Unknown++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var facts []stallFact
	if !everyone {
		facts = s.stallFacts(ctx, uids[0])
		ok, why := stallColdFor(facts)
		rep.ColdWake, rep.ColdWhy = &ok, why
		if pol.Fixed {
			yes := true
			rep.ColdWake, rep.ColdWhy = &yes, ""
		}
	}
	for class, f := range byClass {
		if l := lengths[class]; len(l) > 0 {
			slices.Sort(l)
			f.MedianQuiet = l[len(l)/2]
		}
		if !everyone {
			f.QuietNow = pol.Quiet
			if !pol.Fixed {
				f.QuietNow = stallQuietFor(facts, class, pol.Quiet).Quiet
			}
		}
		rep.Classes = append(rep.Classes, *f)
	}
	slices.SortFunc(rep.Classes, func(a, b StalledClassFigure) int {
		if a.Spells != b.Spells {
			return b.Spells - a.Spells
		}
		return strings.Compare(a.Class, b.Class)
	})
	return rep, nil
}
