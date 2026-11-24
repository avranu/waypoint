// Package strength orders the Claude models an agent can run as, so one
// agent's verdict on another's finding can be held to the finder's strength.
// It needs nothing from package core.
package strength

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Efforts are the reasoning levels, weakest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// rank is a family's place in the order; fable and mythos share one.
var rank = map[string]int{"haiku": 1, "sonnet": 2, "opus": 3, "fable": 4, "mythos": 4}

var families = []string{"haiku", "sonnet", "opus", "fable", "mythos"}

// Model is what is known of a model: its family ("" when not one of the
// Claude families), version (Major 0 when unknown) and effort ("" when
// unknown).
type Model struct {
	Name   string
	Family string
	Major  int
	Minor  int
	Effort string
}

var (
	after  = regexp.MustCompile(`^[\s._-]*(\d{1,2})(?:[\s._-](\d{1,2}))?(?:[^\d]|$)`)
	before = regexp.MustCompile(`(\d+)[._-](\d{1,2})[\s._-]*$`)
	// lone is a major version alone before the family (claude-3-opus).
	dated = regexp.MustCompile(`[\s._-]\d{8}$`)
	lone  = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})[\s._-]*$`)
)

// Parse reads a model name ("claude-sonnet-5-5", "anthropic/claude-code
// sonnet 5.5", "sonnet") and the effort it runs at, "" when not known.
func Parse(name, effort string) Model {
	m := Model{Name: strings.TrimSpace(name)}
	if e := strings.ToLower(strings.TrimSpace(effort)); slices.Contains(Efforts, e) {
		m.Effort = e
	}
	low := strings.ToLower(m.Name)
	at, fam := -1, ""
	for _, f := range families {
		if i := strings.Index(low, f); i >= 0 && (at < 0 || i < at) {
			at, fam = i, f
		}
	}
	if fam == "" {
		return m
	}
	m.Family = fam
	// A date after the family (claude-3-opus-20240229) isn't a version.
	tail := low[at+len(fam):]
	if d := dated.FindString(tail); d != "" {
		tail = tail[:len(tail)-len(d)]
	}
	if g := after.FindStringSubmatch(tail); g != nil {
		m.Major, _ = strconv.Atoi(g[1])
		m.Minor, _ = strconv.Atoi(g[2])
	} else if g := before.FindStringSubmatch(strings.TrimRight(low[:at], " -_.")); g != nil {
		m.Major, _ = strconv.Atoi(g[1])
		m.Minor, _ = strconv.Atoi(g[2])
	} else if g := lone.FindStringSubmatch(strings.TrimRight(low[:at], " -_.")); g != nil {
		m.Major, _ = strconv.Atoi(g[1])
	}
	return m
}

// Known says whether the family is one of the ordered ones.
func (m Model) Known() bool { return m.Family != "" }

// String is "sonnet 5.5 (high)", leaving out what is unknown; a model of no
// known family is its name.
func (m Model) String() string {
	if m.Family == "" {
		return cmpStr(m.Name, "unknown")
	}
	s := m.Family
	if m.Major > 0 {
		s += fmt.Sprintf(" %d.%d", m.Major, m.Minor)
	}
	if m.Effort != "" {
		s += " (" + m.Effort + ")"
	}
	return s
}

func cmpStr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// Verdict is how a rejecter compares with a finder.
type Verdict string

const (
	AtLeast     Verdict = "at_least"
	Weaker      Verdict = "weaker"
	NotCompared Verdict = "not_compared"
)

// Compare says whether rejecter is as strong as finder, and why. Family
// decides first, then version, then effort; a version or effort unknown on
// either side counts as equal for that part. A finder outside the ordered
// families isn't compared; a rejecter outside them is weaker, as nothing is
// known of it.
func Compare(rejecter, finder Model) (Verdict, string) {
	if !rejecter.Known() {
		return Weaker, "Waypoint doesn't know which model this session runs"
	}
	if !finder.Known() {
		return NotCompared, fmt.Sprintf("%s isn't a model Waypoint ranks", finder)
	}
	if r, f := rank[rejecter.Family], rank[finder.Family]; r != f {
		if r < f {
			return Weaker, fmt.Sprintf("%s is a weaker family than %s", rejecter.Family, finder.Family)
		}
		return AtLeast, fmt.Sprintf("%s is a stronger family than %s", rejecter.Family, finder.Family)
	}
	if rejecter.Major > 0 && finder.Major > 0 {
		r, f := rejecter.Major*100+rejecter.Minor, finder.Major*100+finder.Minor
		if r < f {
			return Weaker, fmt.Sprintf("%s is an older version than %s", rejecter, finder)
		}
		if r > f {
			return AtLeast, fmt.Sprintf("%s is a newer version than %s", rejecter, finder)
		}
	}
	if rejecter.Effort != "" && finder.Effort != "" {
		r, f := slices.Index(Efforts, rejecter.Effort), slices.Index(Efforts, finder.Effort)
		if r < f {
			return Weaker, fmt.Sprintf("%s effort is below %s", rejecter.Effort, finder.Effort)
		}
	}
	return AtLeast, fmt.Sprintf("%s is as strong as %s", rejecter, finder)
}
