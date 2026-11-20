package core

import (
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const dateLayout = "2006-01-02"

func (s *Service) actorLocation(actor *Actor) *time.Location {
	if actor != nil && actor.User != nil && actor.User.Timezone != "" {
		if loc, err := time.LoadLocation(actor.User.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

var relDateRE = lazyre.New(`^([+-]?)(\d+)\s*([dwmy])$`)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseDate understands YYYY-MM-DD, today, tomorrow, yesterday, relative
// offsets (+3d, 2w, -1m), and weekday names (the next such day). An empty
// string returns nil.
func ParseDate(v string, now time.Time) (*string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == "none" {
		return nil, nil
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := func(t time.Time) (*string, error) {
		s := t.Format(dateLayout)
		return &s, nil
	}
	switch v {
	case "today", "now":
		return out(day)
	case "tomorrow", "tmrw":
		return out(day.AddDate(0, 0, 1))
	case "yesterday":
		return out(day.AddDate(0, 0, -1))
	case "next week", "nextweek":
		return out(day.AddDate(0, 0, 7))
	case "eow", "end of week":
		return out(day.AddDate(0, 0, (int(time.Friday)-int(day.Weekday())+7)%7))
	case "eom", "end of month":
		return out(time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, day.Location()))
	}
	if t, err := time.Parse(dateLayout, v); err == nil {
		return out(t)
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(v)); err == nil {
		return out(t.In(now.Location()))
	}
	if m := relDateRE.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[2])
		if m[1] == "-" {
			n = -n
		}
		switch m[3] {
		case "d":
			return out(day.AddDate(0, 0, n))
		case "w":
			return out(day.AddDate(0, 0, 7*n))
		case "m":
			return out(day.AddDate(0, n, 0))
		case "y":
			return out(day.AddDate(n, 0, 0))
		}
	}
	if len(v) >= 3 {
		if wd, ok := weekdays[v[:3]]; ok {
			diff := (int(wd) - int(day.Weekday()) + 7) % 7
			if diff == 0 {
				diff = 7
			}
			return out(day.AddDate(0, 0, diff))
		}
	}
	return nil, invalid("can't understand the date %q (try 2026-10-20, tomorrow, +3d, friday)", v)
}

func (s *Service) parseDateFor(actor *Actor, v string) (*string, error) {
	return ParseDate(v, s.now().In(s.actorLocation(actor)))
}
