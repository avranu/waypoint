package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp ci doctor: is CI all right, and if not, what's wrong (WP-483).
func (s *Server) ciDoctor(w http.ResponseWriter, r *http.Request) (any, error) {
	d, err := s.svc.CIDoctor(r.Context(), actorFrom(r))
	if err != nil || !wantsText(r) {
		return d, err
	}
	return textResponse(ciDoctorText(d, r.URL.Query().Get("repo"), queryBool(r, "verbose"))), nil
}

// ciDoctorText puts the verdict and the bottleneck first, then what to do,
// then the numbers behind them; verbose adds the per-merge timings, stacks,
// runner history and every job.
func ciDoctorText(d *core.CIDoctor, repo string, verbose bool) string {
	var o strings.Builder
	dur := func(sec int64) string { return core.ShortDuration(time.Duration(sec) * time.Second) }
	ago := func(t time.Time) string { return core.ShortDuration(d.GeneratedAt.Sub(t)) + " ago" }
	match := func(r string) bool { return repo == "" || r == "" || strings.EqualFold(r, repo) }

	fmt.Fprintf(&o, "CI doctor, %s: %s\n", d.GeneratedAt.Local().Format("15:04"), strings.ToUpper(d.Verdict))
	bn := d.Bottleneck
	fmt.Fprintf(&o, "Bottleneck (%s): %s.\n", bn.Kind, bn.Summary)
	if verbose {
		for _, e := range bn.Evidence {
			fmt.Fprintf(&o, "  - %s\n", e)
		}
	}

	var fs []core.CIFinding
	for _, f := range d.Findings {
		if match(f.Repo) {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		o.WriteString("\nNothing wrong found.\n")
	} else {
		o.WriteString("\nFindings:\n")
		for _, f := range fs {
			fmt.Fprintf(&o, "%-7s %s\n", strings.ToUpper(f.Level), f.Title)
			for _, line := range strings.Split(f.Detail, "\n") {
				if line != "" {
					fmt.Fprintf(&o, "        %s\n", line)
				}
			}
			if f.Next != "" {
				fmt.Fprintf(&o, "        next: %s\n", f.Next)
			}
		}
	}
	// Only where CI is stuck (WP-544): one line, the first that applies.
	for _, u := range d.UrgentLane {
		if match(u.Repo) {
			fmt.Fprintf(&o, "\nUrgent lane: %s\n", u.Text)
			break
		}
	}

	o.WriteString("\nMerge lines (longest to drain first):\n")
	hidden := false
	shown := 0
	for _, l := range d.Lines {
		if !match(l.Repo) {
			continue
		}
		shown++
		name := l.Repo
		if l.Base != "main" {
			name += " → " + l.Base
		}
		fmt.Fprintf(&o, "%s: %d armed, %d moving", name, l.Armed, l.Moving)
		if n := len(l.NeedsPerson); n > 0 {
			fmt.Fprintf(&o, ", %d need a person", n)
		}
		if l.Resting > 0 {
			fmt.Fprintf(&o, ", %d resting", l.Resting)
		}
		if n := len(l.GaveUp); n > 0 {
			fmt.Fprintf(&o, ", %d given up", n)
		}
		if l.Unwatched > 0 {
			fmt.Fprintf(&o, ", %d not babysat", l.Unwatched)
		}
		if l.Drain > 0 {
			fmt.Fprintf(&o, "; drains in about %s", dur(l.Drain))
		}
		o.WriteString(".\n")
		if f := l.Front; f != nil {
			stalled := ""
			if l.Stalled {
				stalled = " STALLED"
			}
			fmt.Fprintf(&o, "  front #%d %s: %s %s", f.Number, clipText(f.Title, 60), strings.ReplaceAll(f.State, "_", " "), dur(f.WaitFor))
			if f.Wait != "" {
				fmt.Fprintf(&o, " (%s)", clipText(f.Wait, 100))
			}
			fmt.Fprintf(&o, "; at the front %s%s\n", dur(l.AtFront), stalled)
		}
		last := "none this week"
		if l.LastMerge != nil {
			last = ago(*l.LastMerge)
		}
		if !verbose {
			hidden = true
			fmt.Fprintf(&o, "  merges 6h/24h/7d: %d/%d/%d, last %s; a merge takes %s (6h median)\n", l.Merges6h, l.Merges24h, l.Merges7d, last, durOr(l.PerMerge6h, dur))
			continue
		}
		fmt.Fprintf(&o, "  merges 6h/24h/7d: %d/%d/%d, last %s; a merge takes %s/%s/%s (median, same windows)\n",
			l.Merges6h, l.Merges24h, l.Merges7d, last, durOr(l.PerMerge6h, dur), durOr(l.PerMerge24h, dur), durOr(l.PerMerge7d, dur))
		if st := l.Steps; st.N > 0 {
			fmt.Fprintf(&o, "  a merge, median of %d today: update+rounds %s | before full run %s | runner wait %s | full run %s | after green %s = %s\n",
				st.N, dur(st.Update), dur(st.BeforeFull), dur(st.RunnerWait), dur(st.FullRun), dur(st.AfterGreen), dur(st.Total))
			if len(l.EndedBy) > 0 {
				var parts []string
				for i, j := range l.EndedBy {
					if i == 3 {
						break
					}
					parts = append(parts, fmt.Sprintf("%s in %d (takes %s)", j.Name, j.Merges, dur(j.P50)))
				}
				fmt.Fprintf(&o, "  full run ends on: %s\n", strings.Join(parts, ", "))
			}
			if len(l.HeldAfterGreen) > 0 {
				var parts []string
				for i, h := range l.HeldAfterGreen {
					if i == 4 {
						break
					}
					parts = append(parts, fmt.Sprintf("%s %s in %d", clipText(h.What, 70), dur(h.P50), h.Merges))
				}
				fmt.Fprintf(&o, "  after green, held by (of %d merges): %s\n", l.HeldOf, strings.Join(parts, " | "))
			}
		}
		if st := l.Stacks; st.Tried > 0 {
			fmt.Fprintf(&o, "  stacked places 6h: %d, %d passed, %s", st.Tried, st.Passed, core.StackFates(st))
			if st.Running > 0 {
				fmt.Fprintf(&o, ", %d running", st.Running)
			}
			o.WriteString("\n")
		}
		for i, m := range l.Merged {
			if i == 8 {
				fmt.Fprintf(&o, "    … %d more\n", len(l.Merged)-i)
				break
			}
			fmt.Fprintf(&o, "    #%d %s merged %s, took %s", m.Number, clipText(m.Title, 50), m.MergedAt.Local().Format("15:04"), dur(m.Total))
			if m.Known {
				fmt.Fprintf(&o, " = %s+%s+%s+%s+%s", dur(m.Update), dur(m.BeforeFull), dur(m.RunnerWait), dur(m.FullRun), dur(m.AfterGreen))
				if m.Reruns > 0 {
					fmt.Fprintf(&o, ", %d job re-run%s", m.Reruns, map[bool]string{true: "", false: "s"}[m.Reruns == 1])
				}
				if m.Early {
					o.WriteString(", merged before its full run passed")
				}
				if len(m.Held) > 0 {
					var parts []string
					for i, h := range m.Held {
						if i == 2 {
							break
						}
						parts = append(parts, fmt.Sprintf("%s %s", clipText(h.What, 50), dur(h.Seconds)))
					}
					fmt.Fprintf(&o, "; after green: %s", strings.Join(parts, ", "))
				}
			}
			o.WriteString("\n")
		}
	}
	if shown == 0 {
		o.WriteString("  none with pull requests armed or merged today\n")
	}

	p := d.Pool
	o.WriteString("\nRunners:\n")
	fmt.Fprintf(&o, "  now: %d of %d slots running, %d waiting for a runner", p.Running, p.Capacity, p.Waiting)
	if p.Waiting > 0 {
		fmt.Fprintf(&o, " (oldest %s)", dur(p.OldestWait))
	}
	fmt.Fprintf(&o, ", %d waiting for other jobs\n", p.Blocked)
	if verbose {
		fmt.Fprintf(&o, "  last 3h: %.0f%% busy, %.1f waiting on average (peak %.1f), wait %s typical / %s p90\n",
			p.Busy3h*100, p.AvgWaiting3h, p.PeakWaiting3h, dur(p.WaitP50_3h), dur(p.WaitP90_3h))
		fmt.Fprintf(&o, "  %s: %.0f%% busy, peak %.1f waiting, wait %s typical / %s p90; %d jobs, %d failed, %d cancelled\n",
			core.CIDayLabel(p.Since24h, d.GeneratedAt), p.Busy24h*100, p.PeakWaiting24h, dur(p.WaitP50_24h), dur(p.WaitP90_24h), p.Jobs24h, p.Failed24h, p.Cancelled24h)
	}
	if !verbose {
		hidden = true
	}
	if p.Saturated {
		o.WriteString("  SATURATED: jobs wait for runners\n")
	}

	if len(d.Jobs) > 0 {
		head := false
		for _, j := range d.Jobs {
			if !match(j.Repo) {
				continue
			}
			if !verbose && !j.Unreliable && j.OnePR == 0 {
				hidden = true
				continue
			}
			if !head {
				head = true
				o.WriteString("\nJobs (last 24h; times are full-tier runs that passed):\n")
			}
			var tags []string
			if j.Unreliable {
				tags = append(tags, "UNRELIABLE")
			}
			if j.OnePR > 0 {
				tags = append(tags, fmt.Sprintf("fails only with #%d", j.OnePR))
			}
			if j.Recovered {
				tags = append(tags, fmt.Sprintf("passing again, last %d", j.PassStreak))
			}
			switch j.SlowRank {
			case 0:
			case 1:
				tags = append(tags, "slowest")
			default:
				tags = append(tags, fmt.Sprintf("slowest #%d", j.SlowRank))
			}
			fmt.Fprintf(&o, "  %s %s: %d runs, %d passed, %s", j.Repo, j.Name, j.Runs, j.Success, core.JobFates(j))
			if j.P50 > 0 {
				fmt.Fprintf(&o, "; %s typical, %s p90", dur(j.P50), dur(j.P90))
			}
			fmt.Fprintf(&o, " [%s]\n", strings.Join(tags, ", "))
		}
	}

	if len(d.Mains) > 0 {
		head := false
		for _, m := range d.Mains {
			if !match(m.Repo) {
				continue
			}
			if !verbose && m.Red24h == 0 && m.Status == "success" {
				hidden = true
				continue
			}
			if !head {
				head = true
				o.WriteString("\nBase branches (latest push run):\n")
			}
			fmt.Fprintf(&o, "  %s %s: run #%d %s, %s (%s); %d of %d push runs red in 24h\n",
				m.Repo, m.Branch, m.Run, m.Status, shortSHA(m.SHA), ago(m.At), m.Red24h, m.Runs)
		}
	}

	o.WriteString("\nArmed pull requests that work on CI:\n")
	n := 0
	for _, w := range d.CIWork {
		if !match(w.Repo) {
			continue
		}
		n++
		pos := "not moving"
		if w.Position > 0 {
			pos = fmt.Sprintf("#%d in line", w.Position)
		}
		fmt.Fprintf(&o, "  %s #%d %s (%s; %s)", w.Repo, w.Number, clipText(w.Title, 70), pos, strings.ReplaceAll(cmpStr(w.State, "no babysit"), "_", " "))
		if w.Issue != "" {
			fmt.Fprintf(&o, " %s", w.Issue)
		}
		if w.Urgent {
			o.WriteString(" [urgent]")
		}
		o.WriteString("\n")
	}
	if n == 0 {
		o.WriteString("  none\n")
	}
	if hidden {
		o.WriteString("\nMore detail: -v\n")
	}
	return o.String()
}

func durOr(sec int64, dur func(int64) string) string {
	if sec == 0 {
		return "-"
	}
	return dur(sec)
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
