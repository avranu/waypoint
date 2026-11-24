package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

const claimMaxAge = 7 * 24 * time.Hour

var claimsCmd = &command{
	name: "claims", group: "Tasks", args: "[prune [--release]]",
	summary: "list tasks taken on this machine; drop stale ones",
	about: `A claim belongs to one git checkout. Lists this project's (-A: the whole
machine). Claims whose issue is closed or whose directory is gone are
dropped each time; those over 7 days on open issues are listed in one line.`,
	examples: []string{"wp claims", "wp claims prune --release"},
	run:      cmdClaims,
	subs: []*command{
		{name: "prune", args: "[--release]", summary: "drop stale claims; --release also gives them back",
			run: cmdClaimsPrune},
	},
}

type claimRow struct {
	Key     string    `json:"key"`
	Dir     string    `json:"dir"`
	Since   time.Time `json:"claimed_at"`
	Status  string    `json:"status,omitempty"`
	Closed  bool      `json:"closed,omitempty"`
	Missing bool      `json:"issue_missing,omitempty"`
	Stale   string    `json:"stale,omitempty"` // why, when it is
}

// claimRows describes every claim, with the issues' statuses from one call.
func (c *cli) claimRows() []claimRow {
	rows := make([]claimRow, 0, len(c.state.Claims))
	keys := map[string]bool{}
	for d, cl := range c.state.Claims {
		rows = append(rows, claimRow{Key: cl.Key, Dir: d, Since: cl.ClaimedAt})
		keys[strings.ToUpper(cl.Key)] = true
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Since.Equal(rows[j].Since) {
			return rows[i].Since.Before(rows[j].Since)
		}
		return rows[i].Dir < rows[j].Dir
	})
	type st struct {
		name   string
		closed bool
	}
	known, asked := map[string]st{}, false
	if len(keys) > 0 {
		var list []string
		for k := range keys {
			list = append(list, k)
		}
		sort.Strings(list)
		q := url.Values{"q": {"key:" + strings.Join(list, ",")}, "limit": {"200"}, "fields": {"key,status"}}
		if raw, err := c.call("GET", "/api/v1/issues?"+q.Encode(), nil); err == nil {
			var res struct {
				Issues []struct {
					Key    string `json:"key"`
					Status struct {
						Name     string `json:"name"`
						Category string `json:"category"`
					} `json:"status"`
				} `json:"issues"`
			}
			if json.Unmarshal(raw, &res) == nil {
				asked = true
				for _, i := range res.Issues {
					known[strings.ToUpper(i.Key)] = st{i.Status.Name, isClosedCategory(i.Status.Category)}
				}
			}
		}
	}
	for n := range rows {
		r := &rows[n]
		if s, ok := known[strings.ToUpper(r.Key)]; ok {
			r.Status, r.Closed = s.name, s.closed
		} else if asked {
			r.Missing = true
		}
		var why []string
		if _, err := os.Stat(r.Dir); err != nil && os.IsNotExist(err) {
			why = append(why, "directory gone")
		}
		if r.Closed {
			why = append(why, "issue "+r.Status)
		}
		if !r.Since.IsZero() && time.Since(r.Since) > claimMaxAge {
			why = append(why, "older than 7 days")
		}
		r.Stale = strings.Join(why, ", ")
	}
	return rows
}

func claimAge(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// dead says the claim holds nothing: the issue is closed or the directory is
// gone. Those are dropped without being asked. An issue the server didn't
// return is not dead: it may be one this token can't see.
func (r claimRow) dead() bool {
	return r.Closed || strings.Contains(r.Stale, "directory gone")
}

// claimProject is the project key of a claim's issue (WP-12 is WP).
func claimProject(key string) string {
	if i := strings.LastIndex(key, "-"); i > 0 {
		return key[:i]
	}
	return key
}

func cmdClaims(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q (did you mean wp claims prune?)", pos[0])
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		if (sf.project != nil && *sf.project != "") || (sf.all != nil && *sf.all) || os.Getenv("WAYPOINT_PROJECT") != "" {
			return err
		}
		sc = &scope{}
	}
	var rows, dead []claimRow
	for _, r := range c.claimRows() {
		switch {
		case r.dead():
			dead = append(dead, r)
		case sc.all() || sc.contains(claimProject(r.Key)):
			rows = append(rows, r)
		}
	}
	if len(dead) > 0 {
		dropped := 0
		if err := c.updateState(func(s *State) {
			dropped = 0
			for _, r := range dead {
				if cl := s.Claims[r.Dir]; cl != nil && cl.Key == r.Key && cl.ClaimedAt.Equal(r.Since) {
					delete(s.Claims, r.Dir)
					dropped++
				}
			}
		}); err != nil {
			return fmt.Errorf("finished claims not dropped: %w", err)
		}
		dead = dead[:dropped:dropped]
	}
	if c.asJSON {
		b, _ := json.Marshal(rows)
		return c.emit(b, func() string { return "" })
	}
	if len(dead) > 0 {
		c.println(fmt.Sprintf("Dropped %d finished claim%s (issue closed or directory removed).", len(dead), plural(len(dead))))
	}
	if len(rows) == 0 {
		c.println("No tasks taken here.")
		return nil
	}
	var old []string
	for _, r := range rows {
		if r.Stale != "" {
			old = append(old, r.Key+" "+claimAge(r.Since))
			continue
		}
		c.println(fmt.Sprintf("%-8s %4s  %-12s %s", r.Key, claimAge(r.Since), cmpStr(r.Status, "?"), r.Dir))
	}
	if len(old) > 0 {
		c.println(fmt.Sprintf("%d older than 7 days: %s (wp claims prune --release gives back those the server still lists).", len(old), strings.Join(old, ", ")))
	}
	return nil
}

func cmdClaimsPrune(c *cli, args []string) error {
	fs := c.flags()
	release := fs.Bool("release", false, "also give the tasks back on the server (like wp release)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", pos[0])
	}
	var drop []claimRow
	for _, r := range c.claimRows() {
		if r.Stale != "" {
			drop = append(drop, r)
		}
	}
	if len(drop) == 0 {
		c.println("Nothing stale.")
		return nil
	}
	released := map[string]bool{}
	if *release {
		for _, r := range drop {
			if r.Closed || r.Missing {
				continue
			}
			body := map[string]any{"comment": "Released by wp claims prune: the claim in " + r.Dir + " was stale (" + r.Stale + ")."}
			if a := c.agentFor(r.Key); a != "" {
				body["agent"] = a
			}
			if _, err := c.call("POST", "/api/v1/agent/release/"+esc(r.Key), body); err == nil {
				released[r.Dir] = true
			}
		}
	}
	err = c.updateState(func(s *State) {
		for _, r := range drop {
			delete(s.Claims, r.Dir)
		}
	})
	if err != nil {
		return err
	}
	for _, r := range drop {
		line := fmt.Sprintf("Dropped %s (%s): %s", r.Key, r.Dir, r.Stale)
		if released[r.Dir] {
			line += "; released on the server"
		}
		c.println(line)
	}
	return nil
}

var prLinkCmd = &command{
	name: "link", args: "N KEY",
	summary:  "link pull request N to an issue",
	about:    "Doesn't move the issue; wp ship does that.",
	examples: []string{"wp pr link 12 WP-34"},
	run:      cmdPRLink,
}

var prUnlinkCmd = &command{
	name: "unlink", args: "N [KEY...]",
	summary: "unlink pull request N from issues (default: all)",
	about: `For a wrongly linked pull request. An issue the link moved to In Review
goes back to its old status, unless something moved it since or another
pull request is still linked.`,
	examples: []string{"wp pr unlink 71", "wp pr unlink 71 WP-85"},
	run:      cmdPRUnlink,
}

func cmdPRLink(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), when several attached repos have this number")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || !keyRE.MatchString(strings.ToUpper(pos[1])) {
		return c.usage("expected a pull request number and an issue KEY: wp pr link 12 WP-34")
	}
	d, _, err := c.loadPR(sf, pos[:1], *repo)
	if err != nil {
		return err
	}
	key := strings.ToUpper(pos[1])
	raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/links", map[string]string{"url": d.URL})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return fmt.Sprintf("Linked %s#%d to %s", d.Repo, d.Number, key) })
}

func cmdPRUnlink(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), when several attached repos have this number")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("which pull request? wp pr unlink N [KEY...]")
	}
	d, _, err := c.loadPR(sf, pos[:1], *repo)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, k := range pos[1:] {
		if !keyRE.MatchString(strings.ToUpper(k)) {
			return c.usage("%q isn't an issue key like WP-12", k)
		}
		want[strings.ToUpper(k)] = true
	}
	missing := maps.Clone(want)
	removed := 0
	for _, is := range d.Issues {
		if len(want) > 0 && !want[strings.ToUpper(is.Key)] {
			continue
		}
		delete(missing, strings.ToUpper(is.Key))
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/issues/%s/links/%d", esc(is.Key), is.LinkID), nil); err != nil {
			return fmt.Errorf("unlink %s: %w", is.Key, err)
		}
		removed++
		c.println(fmt.Sprintf("Unlinked %s#%d from %s", d.Repo, d.Number, is.Key))
	}
	for _, k := range slices.Sorted(maps.Keys(missing)) {
		c.println(fmt.Sprintf("%s#%d isn't linked to %s.", d.Repo, d.Number, k))
	}
	if removed == 0 && len(want) == 0 {
		c.println(fmt.Sprintf("%s#%d isn't linked to any issue.", d.Repo, d.Number))
	}
	return nil
}

// unlinkForge removes a link to a forge item (URL, or #N for this repo's
// pull request N) from an issue.
func (c *cli) unlinkForge(key, ref string) error {
	key = strings.ToUpper(key)
	target := ref
	if n, ok := strings.CutPrefix(ref, "#"); ok {
		d, _, err := c.loadPR(scopeFlags{}, []string{n}, "")
		if err != nil {
			return err
		}
		target = d.URL
	}
	raw, err := c.call("GET", "/api/v1/issues/"+esc(key), nil)
	if err != nil {
		return err
	}
	var is struct {
		Links []struct {
			ID  int64  `json:"id"`
			URL string `json:"url"`
		} `json:"links"`
	}
	if err := jsonInto(raw, &is); err != nil {
		return err
	}
	norm := func(u string) string { return strings.TrimSuffix(strings.TrimSpace(u), "/") }
	for _, l := range is.Links {
		if norm(l.URL) == norm(target) {
			if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/issues/%s/links/%d", esc(key), l.ID), nil); err != nil {
				return err
			}
			c.println(fmt.Sprintf("Unlinked %s from %s", l.URL, key))
			return nil
		}
	}
	return fmt.Errorf("%s isn't linked to %s (wp show %s lists its links)", target, key, key)
}
