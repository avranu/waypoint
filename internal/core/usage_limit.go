package core

import (
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Claude Code's plan-limit line has several forms ("5-hour limit reached ∙ resets 3am", "Claude AI usage limit reached|1762534800"); match them all, not only "hit your limit".

// limitPhrase matches a plan's usage limit in any of Claude Code's forms,
// and not its "Context limit reached", which is the conversation's length;
// and HTTP 429 as a status ("API Error: 429", "returned error: 429"), not
// the digits in a request id, a SHA or "PR #429".
var limitPhrase = lazyre.New(usagePhraseSrc + `|` + status429Src)

// usagePhrase is limitPhrase without the HTTP 429: a plan's spent usage,
// which audits tell apart from a transient 429 ("busy", retried soon).
var usagePhrase = lazyre.New(usagePhraseSrc)

const (
	usagePhraseSrc = `(?i)\b(?:hit|reached) your (?:[\w-]+ )?limit\b|\b(?:usage|session|weekly|daily|monthly|hourly|5-hour|five-hour|opus|sonnet|haiku|fable|mythos) limit (?:reached|hit)\b`
	status429Src   = `(?i)\b(?:error|status|code|http)\b[^\n\d]{0,12}\b429\b`
)

// limitText: the text says a usage or rate limit stopped the run.
func limitText(text string) bool {
	low := strings.ToLower(text)
	for _, w := range limitWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return limitPhrase.MatchString(text)
}

var (
	// "resets 5pm (UTC)", "resets 5:30pm", "resets 17:00 (Europe/London)",
	// "resets Nov 8, 5pm (UTC)", "resets Nov 8 at 5pm".
	resetsAt = lazyre.New(`(?i)\bresets\s+(?:(?:at|on)\s+)?(?:([a-z]{3})[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)
	// "usage limit reached|1762534800": the reset as Unix seconds.
	resetsEpoch = lazyre.New(`limit reached\|(\d{10})\b`)
)

// limitResetIn reads when a limit lifts from the text that reported it,
// the first such time after now.
func limitResetIn(text string, now time.Time) (time.Time, bool) {
	if m := resetsEpoch.FindStringSubmatch(text); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		if t := time.Unix(sec, 0).UTC(); t.After(now) {
			return t, true
		}
	}
	m := resetsAt.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	switch strings.ToLower(m[5]) {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "":
		if m[4] == "" {
			return time.Time{}, false
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	loc := time.UTC
	if tz := strings.TrimSpace(m[6]); tz != "" && !strings.EqualFold(tz, "utc") {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	local := now.In(loc)
	if m[1] != "" {
		month, ok := months[strings.ToLower(m[1])]
		day, _ := strconv.Atoi(m[2])
		if !ok || day < 1 || day > 31 {
			return time.Time{}, false
		}
		t := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0) // A reset named by month and day, read late in the year, is next year's date.
		}
		if !t.After(now) {
			// Already past (a wrong zone, a late read): it says nothing of
			// when the limit lifts, and a past time would retry at once.
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t.UTC(), true
}

var months = map[string]time.Month{"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December}
