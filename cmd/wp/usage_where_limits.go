package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// limitIncidents groups the limit records into incidents and finds, for each,
// when a model call next succeeded in any session. The transcripts carry no
// per-call marker of extra usage (a call's usage reads service_tier "standard"
// either way), so a dollar figure is given only when the latest limit record
// says extra usage was in use, and then as "may be extra usage".
func (sc *whScan) limitIncidents() whLimits {
	out := whLimits{Incidents: []whIncident{}, OtherErrs: sc.apiErrors}
	var ok []*whCall
	for _, c := range sc.order {
		if sc.in(c.ts) {
			ok = append(ok, c)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].ts < ok[j].ts })
	next := func(ts int64) *whCall {
		i := sort.Search(len(ok), func(i int) bool { return ok[i].ts > ts })
		if i < len(ok) {
			return ok[i]
		}
		return nil
	}
	sort.SliceStable(sc.limits, func(i, j int) bool { return sc.limits[i].ts < sc.limits[j].ts })
	var cur *whIncident
	for _, e := range sc.limits {
		if cur != nil && cur.Type == e.kind && cur.last.resets == e.resets {
			if n := next(cur.last.ts); n == nil || n.ts > e.ts {
				cur.Records++
				cur.last = e
				continue
			}
		}
		out.Incidents = append(out.Incidents, whIncident{Time: time.UnixMilli(e.ts).UTC().Format(time.RFC3339), Session: e.session, Type: e.kind, Records: 1, Text: e.text, startMS: e.ts, last: e})
		cur = &out.Incidents[len(out.Incidents)-1]
	}
	for i := range out.Incidents {
		in := &out.Incidents[i]
		e := in.last
		in.ExtraUsage, in.UsingOverage = whExtraUsage(e)
		if e.resets > 0 {
			in.ResetsAt = time.Unix(e.resets, 0).UTC().Format(time.RFC3339)
			in.endMS = e.resets * 1000
		}
		n := next(in.startMS)
		if n == nil {
			in.Lockout = "no successful call before the window ended"
		} else {
			secs := float64(n.ts-in.startMS) / 1000
			in.LockoutSeconds = &secs
			in.ResumedAt = time.UnixMilli(n.ts).UTC().Format(time.RFC3339)
			in.Lockout = "calls resumed after " + whDur(secs)
			if secs >= 60 {
				in.Lockout = "no successful call for " + whDur(secs)
			}
			in.ResumedEarly = in.endMS > 0 && n.ts < in.endMS && !in.UsingOverage
		}
		if in.UsingOverage {
			end := in.endMS
			if end == 0 || end > sc.end {
				end = sc.end
			}
			var spend float64
			for _, c := range ok {
				if c.ts >= in.startMS && c.ts <= end {
					spend += c.parts.total()
				}
			}
			in.SpendMayBeExtra = &spend
		}
	}
	return out
}

func whExtraUsage(e whLimit) (string, bool) {
	switch {
	case e.usingOverage:
		return "extra usage in use", true
	case e.reason != "":
		return "extra usage: off (" + e.reason + ")", false
	case e.overage == "rejected":
		return "extra usage: off", false
	case e.overage == "":
		return "extra usage: not stated by the record", false
	}
	return "extra usage: not in use (overage status " + e.overage + ")", false
}

func whOtherErrs(m map[string]int) string {
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var parts []string
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func whDur(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%.1fs", secs)
	}
	m := int(secs/60 + 0.5)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
