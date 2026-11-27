package core

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// dbMetricsText is the part of `wp server metrics` that is not Prometheus
// text (WP-1946): how the database's two pools are used and waited for, and
// which callers' statements took the most time.
func (s *Service) dbMetricsText() string {
	var b strings.Builder
	b.WriteString("\ndatabase connections (since the server started; the writer is one connection, so its waits are the queue for writes)\n")
	b.WriteString("pool    open  in use  idle  max open  callers that waited  total wait\n")
	for _, p := range []struct {
		name string
		st   sql.DBStats
	}{{"writer", s.db.W.Stats()}, {"readers", s.db.R.Stats()}} {
		fmt.Fprintf(&b, "%-7s %4d  %6d  %4d  %8d  %19d  %s\n", p.name, p.st.OpenConnections, p.st.InUse, p.st.Idle,
			p.st.MaxOpenConnections, p.st.WaitCount, p.st.WaitDuration.Round(time.Millisecond))
	}
	top, dropped := s.db.SlowCallers(15)
	if s.db.Slow() == 0 {
		b.WriteString("\nslow database callers: timing is off (WAYPOINT_SLOW_STATEMENT=0)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\nslowest database callers by total time (statements and write transactions over 50ms; logged over %s as \"slow statement\")\n", s.db.Slow())
	if len(top) == 0 {
		b.WriteString("none yet\n")
	} else {
		b.WriteString("  total  count  longest  pool               caller\n")
	}
	for _, c := range top {
		fmt.Fprintf(&b, "%7s  %5d  %7s  %-17s  %s\n", c.Total.Round(time.Millisecond), c.Count, c.Longest.Round(time.Millisecond), c.Pool, c.Caller)
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "(%d statements came from callers beyond the 500 kept)\n", dropped)
	}
	return b.String()
}

// grepLines keeps the lines of text that match re.
func grepLines(text string, re *regexp.Regexp) string {
	var out strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		if line != "" && re.MatchString(line) {
			out.WriteString(line + "\n")
		}
	}
	return out.String()
}
