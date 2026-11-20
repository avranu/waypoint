package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression: minute hour day-of-month month
// day-of-week. Lists (1,15), ranges (1-5), steps (*/10, 8-18/2), month and
// weekday names, and @hourly, @daily, @weekly, @monthly, @yearly work. When
// both day fields are restricted, either one matching is enough, as in
// Vixie cron.
type Cron struct {
	min, hour, dom, mon, dow uint64
	domAny, dowAny           bool
}

var cronMonths = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var cronDays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

var cronShortcuts = map[string]string{
	"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@weekly": "0 0 * * 0",
	"@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
}

// ParseCron reads a cron expression.
func ParseCron(expr string) (*Cron, error) {
	expr = strings.TrimSpace(strings.ToLower(expr))
	if s, ok := cronShortcuts[expr]; ok {
		expr = s
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("a cron expression has five fields: minute hour day-of-month month day-of-week")
	}
	var c Cron
	var err error
	if c.min, _, err = cronField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.hour, _, err = cronField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.dom, c.domAny, err = cronField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.mon, _, err = cronField(f[3], 1, 12, cronMonths); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.dow, c.dowAny, err = cronField(f[4], 0, 7, cronDays); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	return &c, nil
}

// cronField reads one field into a bit set; star reports a bare * (or ?).
func cronField(s string, lo, hi int, names map[string]int) (bits uint64, star bool, err error) {
	if s == "*" || s == "?" {
		for i := lo; i <= hi; i++ {
			bits |= 1 << i
		}
		return bits, true, nil
	}
	num := func(v string) (int, error) {
		if n, ok := names[strings.ToLower(v)]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%q is not between %d and %d", v, lo, hi)
		}
		return n, nil
	}
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepS); err != nil || step < 1 {
				return 0, false, fmt.Errorf("%q is not a step", stepS)
			}
		}
		a, b := lo, hi
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			if a, err = num(x); err != nil {
				return 0, false, err
			}
			if b, err = num(y); err != nil {
				return 0, false, err
			}
			if a > b {
				return 0, false, fmt.Errorf("%q runs backwards", rng)
			}
		default:
			if a, err = num(rng); err != nil {
				return 0, false, err
			}
			if hasStep {
				b = hi // 5/10 means 5-max step 10
			} else {
				b = a
			}
		}
		for i := a; i <= b; i += step {
			bits |= 1 << i
		}
	}
	return bits, false, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// Matches reports whether t (to the minute) is a time the expression fires.
func (c *Cron) Matches(t time.Time) bool {
	return c.min&(1<<t.Minute()) != 0 && c.hour&(1<<t.Hour()) != 0 && c.mon&(1<<int(t.Month())) != 0 && c.dayMatches(t)
}

// Next is the first time after t (in t's zone) the expression fires, or the
// zero time if it never does within five years.
func (c *Cron) Next(t time.Time) time.Time {
	loc := t.Location()
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case c.mon&(1<<int(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case c.hour&(1<<t.Hour()) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case c.min&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// Latest is the most recent time at or before now (within window) the
// expression fired, if any.
func (c *Cron) Latest(now time.Time, window time.Duration) (time.Time, bool) {
	t := now.Truncate(time.Minute)
	for n := 0; time.Duration(n)*time.Minute <= window; n++ {
		if c.Matches(t) {
			return t, true
		}
		t = t.Add(-time.Minute)
	}
	return time.Time{}, false
}
