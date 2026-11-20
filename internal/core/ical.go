package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CalendarFeed renders an iCalendar feed for the user owning token: due
// dates of their open work and their projects' sprints. Google Calendar (and
// any other calendar app) can subscribe to it by URL.
func (s *Service) CalendarFeed(ctx context.Context, token string) ([]byte, error) {
	u, err := s.userByCalendarToken(ctx, token)
	if err != nil {
		return nil, err
	}
	actor := &Actor{User: u, ReadOnly: true}
	var b strings.Builder
	w := func(line string) { b.WriteString(foldICS(line)) }
	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:-//Waypoint//Waypoint//EN")
	w("CALSCALE:GREGORIAN")
	w("METHOD:PUBLISH")
	w("X-WR-CALNAME:Waypoint – " + escICS(u.Name()))
	w("X-PUBLISHED-TTL:PT30M")
	w("REFRESH-INTERVAL;VALUE=DURATION:PT30M")
	stamp := s.now().UTC().Format("20060102T150405Z")
	host := strings.TrimPrefix(strings.TrimPrefix(s.BaseURL(), "https://"), "http://")
	if host == "" {
		host = "waypoint"
	}
	res, err := s.Search(ctx, actor, SearchParams{
		Query: "assignee:me has:due is:open",
		Limit: 1000, DefaultOrder: "updated",
	})
	if err != nil {
		return nil, err
	}
	for _, i := range res.Issues {
		due, err := time.Parse(dateLayout, *i.DueDate)
		if err != nil {
			continue
		}
		start := due
		if i.StartDate != nil {
			if st, err := time.Parse(dateLayout, *i.StartDate); err == nil && !st.After(due) {
				start = st
			}
		}
		w("BEGIN:VEVENT")
		w(fmt.Sprintf("UID:issue-%d@%s", i.ID, host))
		w("DTSTAMP:" + stamp)
		w("DTSTART;VALUE=DATE:" + start.Format("20060102"))
		w("DTEND;VALUE=DATE:" + due.AddDate(0, 0, 1).Format("20060102"))
		w("SUMMARY:" + escICS(i.Key+" "+i.Title))
		w("DESCRIPTION:" + escICS(fmt.Sprintf("%s · %s priority\n%s/issue/%s", i.Status.Name, PriorityName(i.Priority), s.BaseURL(), i.Key)))
		w("URL:" + s.BaseURL() + "/issue/" + i.Key)
		w("CATEGORIES:" + escICS(i.ProjectKey))
		w("TRANSP:TRANSPARENT")
		w("END:VEVENT")
	}
	projects, err := s.ListProjects(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Role == "guest" || p.Settings.Mode != "sprints" {
			continue
		}
		sprints, err := s.listSprints(ctx, s.db.R, p.ID, false)
		if err != nil {
			return nil, err
		}
		for _, sp := range sprints {
			if sp.StartDate == nil || sp.EndDate == nil {
				continue
			}
			st, err1 := time.Parse(dateLayout, *sp.StartDate)
			en, err2 := time.Parse(dateLayout, *sp.EndDate)
			if err1 != nil || err2 != nil {
				continue
			}
			w("BEGIN:VEVENT")
			w(fmt.Sprintf("UID:sprint-%d@%s", sp.ID, host))
			w("DTSTAMP:" + stamp)
			w("DTSTART;VALUE=DATE:" + st.Format("20060102"))
			w("DTEND;VALUE=DATE:" + en.AddDate(0, 0, 1).Format("20060102"))
			w("SUMMARY:" + escICS("🏁 "+sp.Name))
			if sp.Goal != "" {
				w("DESCRIPTION:" + escICS(sp.Goal))
			}
			w("URL:" + s.BaseURL() + "/p/" + p.Key + "/backlog")
			w("TRANSP:TRANSPARENT")
			w("END:VEVENT")
		}
	}
	w("END:VCALENDAR")
	return []byte(b.String()), nil
}

func escICS(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}

// foldICS folds a content line at 75 octets, per RFC 5545, without
// splitting UTF-8 sequences.
func foldICS(line string) string {
	var b strings.Builder
	n := 0
	for _, r := range line {
		l := len(string(r))
		if n+l > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += l
	}
	b.WriteString("\r\n")
	return b.String()
}
