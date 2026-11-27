package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLedgerReplay classifies the rounds of saved babysit statuses (the
// JSON `wp pr babysit N --status --json` prints, one file per pull request)
// and prints the classes, to set gistShared and gistOverlap against a hand
// count. Files
// are read with git from WP_LEDGER_REPLAY_GIT. It runs only when
// WP_LEDGER_REPLAY names the directory of statuses.
func TestLedgerReplay(t *testing.T) {
	dir := os.Getenv("WP_LEDGER_REPLAY")
	if dir == "" {
		t.Skip("set WP_LEDGER_REPLAY to a directory of babysit statuses")
	}
	repo := os.Getenv("WP_LEDGER_REPLAY_GIT")
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	cache := map[string]string{}
	read := func(path, sha string) string {
		if repo == "" {
			return ""
		}
		key := sha + ":" + path
		if s, ok := cache[key]; ok {
			return s
		}
		out, err := exec.Command("git", "-C", repo, "show", key).Output()
		if err != nil {
			out = nil
		}
		cache[key] = string(out)
		return string(out)
	}
	work, final := map[string]int{}, map[string]int{}
	total := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v struct {
			Babysit  *Babysit           `json:"babysit"`
			Comments []*PRReviewComment `json:"comments"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Babysit == nil {
			continue
		}
		rounds := v.Babysit.Rounds
		for i := range rounds {
			rounds[i].Class = ""
		}
		replayRounds(rounds, v.Comments, read)
		for i := range rounds {
			r := &rounds[i]
			if r.Class == "" {
				continue
			}
			work[r.WorkClass]++
			final[r.Class]++
			total++
			var items []string
			for _, e := range r.Ledger {
				s := e.ID + "=" + e.Class
				if e.Of != "" && e.Of != e.ID {
					s += "(of " + e.Of + ")"
				}
				if e.Round > 0 {
					s += fmt.Sprintf("@r%d", e.Round)
				}
				if e.Symbol != "" {
					s += "[" + e.Symbol + "]"
				}
				items = append(items, s)
			}
			t.Logf("%s r%d %s/%s: %s", filepath.Base(f), r.N, r.WorkClass, r.Class, strings.Join(items, " "))
		}
	}
	report := func(name string, m map[string]int) {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d (%.0f%%)", k, m[k], 100*float64(m[k])/float64(total)))
		}
		t.Logf("%s over %d rounds: %s", name, total, strings.Join(parts, ", "))
	}
	report("work class", work)
	report("class", final)
}
