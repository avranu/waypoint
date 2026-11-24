package codeindex

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRewriteEvalOffline measures the rewritten-work notice (WP-782) over
// this checkout's history: for each merge on origin/main since REWRITE_SINCE
// (default 2026-11-03), the other tasks' pull requests whose lines it
// replaced within 14 days, and how the two tasks were related. It reads
// REWRITE_EVAL, a JSON object of `wp show --json` issues keyed by key (title
// and relations), and prints one line per pair for counting by hand.
// Skipped without it.
func TestRewriteEvalOffline(t *testing.T) {
	path := os.Getenv("REWRITE_EVAL")
	if path == "" {
		t.Skip()
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- a file the person running it names
	if err != nil {
		t.Fatal(err)
	}
	var issues map[string]struct {
		Title     string
		Relations []struct {
			Kind  string
			Issue struct{ Key string }
		}
	}
	if err := json.Unmarshal(raw, &issues); err != nil {
		t.Fatalf("REWRITE_EVAL: %v", err)
	}
	root, err := gitOut(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	root = strings.TrimSpace(root)
	out, err := gitOut(root, "log", "--first-parent", "origin/main", "--since="+cmp.Or(os.Getenv("REWRITE_SINCE"), "2026-11-03"), "--format=%H %ct %s")
	if err != nil {
		t.Fatal(err)
	}
	subj := regexp.MustCompile(`Merge pull request '.*' \(#(\d+)\) from (\S+) into`)
	keyRE := regexp.MustCompile(`(?i)wp-?(\d+)`)
	prTask := map[int]string{}
	type m struct {
		sha  string
		at   int64
		pr   int
		task string
	}
	var merges []m
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(l, " ", 3)
		if len(f) < 3 {
			continue
		}
		s := subj.FindStringSubmatch(f[2])
		if s == nil {
			continue
		}
		pr, _ := strconv.Atoi(s[1])
		task := ""
		if k := keyRE.FindStringSubmatch(s[2]); k != nil {
			task = "WP-" + k[1]
		}
		at, _ := strconv.ParseInt(f[1], 10, 64)
		prTask[pr] = task
		merges = append(merges, m{f[0], at, pr, task})
	}
	all, _ := gitOut(root, "log", "--first-parent", "origin/main", "--format=%s")
	for _, l := range strings.Split(all, "\n") {
		if s := subj.FindStringSubmatch(l); s != nil {
			pr, _ := strconv.Atoi(s[1])
			if _, ok := prTask[pr]; !ok {
				if k := keyRE.FindStringSubmatch(s[2]); k != nil {
					prTask[pr] = "WP-" + k[1]
				}
			}
		}
	}
	related := func(a, b string) string {
		for _, r := range issues[a].Relations {
			if r.Issue.Key == b {
				return r.Kind
			}
		}
		for _, r := range issues[b].Relations {
			if r.Issue.Key == a {
				return "rev:" + r.Kind
			}
		}
		return ""
	}
	for _, x := range merges {
		if x.task == "" {
			continue
		}
		tr, err := TraceFix(root, FixInput{Merge: x.sha, Base: "main", FiledAt: time.Unix(x.at, 0)})
		if err != nil || tr.Method != TraceSZZ {
			continue
		}
		for _, in := range tr.Introducers {
			a := prTask[in.PR]
			if a == "" || a == x.task {
				continue
			}
			days := float64(x.at-in.At/1000) / 86400
			if days > 14 {
				continue
			}
			fmt.Printf("%s (#%d) rewrote %d lines (%.0f%% of its removed) of %s (#%d), %.1fd earlier; rel=%q\n   B: %.70s\n   A: %.70s\n",
				x.task, x.pr, in.Lines, 100*in.Share, a, in.PR, days, related(x.task, a), issues[x.task].Title, issues[a].Title)
		}
	}
}
