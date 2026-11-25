package main

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

var prAfterCmd = &command{
	name: "after", args: "M... [--pr N] [--off]",
	summary: "make a pull request merge only after others have",
	about: `Sets this branch's pull request (or --pr N) to merge only after pull
requests M... have merged (WP-912). While one of them is open it doesn't
merge, even armed, and gives up its place in the merge queue; when the last
one merges or closes it joins the line again at its place. --off drops the
wait. M is a number in the same repo, or owner/repo#M in another one on the
same forge.

You often needn't: a pull request whose task is blocked by another task
(wp relate KEY blocked-by OTHER) already waits for that task's open pull
request. With no M it shows what the pull request merges after.`,
	examples: []string{"wp pr after 244", "wp pr after 244 --pr 251", "wp pr after acme/api#12", "wp pr after 244 --off"},
	run:      cmdPRAfter,
}

func cmdPRAfter(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name) of --pr, if the number is ambiguous")
	which := fs.String("pr", "", "the pull request `N` that waits (default: this branch's)")
	off := fs.Bool("off", false, "drop the wait")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var targets []prRef
	for _, a := range pos {
		for _, s := range strings.Split(a, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			r, err := parsePRRef(s)
			if err != nil {
				return err
			}
			targets = append(targets, r)
		}
	}
	if *off && len(targets) == 0 {
		return fmt.Errorf("--off drops a wait: name the pull request it waits for (wp pr after M --off)")
	}
	var one []string
	if *which != "" {
		one = []string{*which}
	}
	d, raw, err := c.loadPR(sf, one, *repo)
	if err != nil {
		return err
	}
	method := "POST"
	if *off {
		method = "DELETE"
	}
	var dropped, unset []string
	for _, t := range targets {
		if !*off {
			continue
		}
		if slices.ContainsFunc(d.MergeAfter, func(w prMergeWait) bool { return w.Set && t.is(w, d.Repo) }) {
			dropped = append(dropped, t.String())
		} else {
			unset = append(unset, t.String())
		}
	}
	for _, t := range targets {
		q := url.Values{}
		if t.repo != "" {
			q.Set("repo", t.repo)
		}
		raw, err = c.call(method, fmt.Sprintf("/api/v1/pulls/%d/after/%d?%s", d.ID, t.number, q.Encode()), nil)
		if err != nil {
			return err
		}
	}
	var cur prDetail
	if err := jsonInto(raw, &cur); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var b strings.Builder
		switch {
		case !*off && len(targets) > 0:
			names := make([]string, len(targets))
			for n, t := range targets {
				names[n] = t.String()
			}
			fmt.Fprintf(&b, "%s#%d merges after %s.\n", cur.Repo, cur.Number, strings.Join(names, ", "))
		case *off:
			if len(dropped) > 0 {
				fmt.Fprintf(&b, "%s#%d no longer waits for %s.\n", cur.Repo, cur.Number, strings.Join(dropped, ", "))
			}
			if len(unset) > 0 {
				fmt.Fprintf(&b, "%s#%d wasn't set to merge after %s.\n", cur.Repo, cur.Number, strings.Join(unset, ", "))
			}
		}
		if len(cur.MergeAfter) == 0 {
			fmt.Fprintf(&b, "%s#%d merges after no other pull request.\n", cur.Repo, cur.Number)
		}
		b.WriteString(cur.mergeAfterText())
		if *off {
			for _, w := range cur.MergeAfter {
				for _, t := range targets {
					if !w.Set && t.is(w, cur.Repo) {
						fmt.Fprintf(&b, "%s still holds it through a task link (%s); unlink the tasks to drop that.\n", t, w.Why)
					}
				}
			}
		}
		return b.String()
	})
}

// prMergeWait is a pull request another merges after (core.PRMergeWait).
type prMergeWait struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	State  string `json:"state"`
	Why    string `json:"why"`
	Set    bool   `json:"set"`
	Mutual bool   `json:"mutual"`
}

// prRef is a pull request named on the command line: 12, #12 or
// owner/repo#12.
type prRef struct {
	repo   string
	number int
}

// is: r names w, from a pull request in repo.
func (r prRef) is(w prMergeWait, repo string) bool {
	return w.Number == r.number && strings.EqualFold(cmpStr(r.repo, repo), w.Repo)
}

func (r prRef) String() string {
	if r.repo == "" {
		return "#" + strconv.Itoa(r.number)
	}
	return r.repo + "#" + strconv.Itoa(r.number)
}

func parsePRRef(s string) (prRef, error) {
	var r prRef
	num := strings.TrimPrefix(s, "#")
	if i := strings.LastIndex(s, "#"); i > 0 {
		r.repo, num = s[:i], s[i+1:]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return r, fmt.Errorf("%q isn't a pull request: give a number, or owner/repo#N", s)
	}
	r.number = n
	return r, nil
}

// mergeAfterText lists what the pull request merges after, "" for nothing.
func (d *prDetail) mergeAfterText() string {
	if len(d.MergeAfter) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Merges after:\n")
	for _, w := range d.MergeAfter {
		ref := fmt.Sprintf("#%d", w.Number)
		if !strings.EqualFold(w.Repo, d.Repo) {
			ref = fmt.Sprintf("%s#%d", w.Repo, w.Number)
		}
		state := w.State
		if w.Mutual && (w.State == "open" || w.State == "draft") {
			state += " (waits for this one in turn: none of them holds)"
		}
		fmt.Fprintf(&b, "  %-10s %-10s %s\n", ref, state, w.Why)
	}
	return b.String()
}
