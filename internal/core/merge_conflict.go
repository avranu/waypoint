package core

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// conflictLog is the error .forgejo/scripts/queue-stack.sh prints when a
// head doesn't merge onto the ones before it.
var conflictLog = lazyre.New(`::error title=Merge queue stack::#(\d+) \(([0-9a-f]{7,64})\) doesn't merge cleanly onto the heads before it`)

// baseConflictLog is the error the script adds when that head doesn't merge
// cleanly onto the base's tip alone either (WP-457).
var baseConflictLog = lazyre.New(`::error title=Merge queue stack::#(\d+) \(([0-9a-f]{7,64})\) doesn't merge cleanly onto the base \(([^)\s]+)\) itself`)

// stackBaseConflict records that head doesn't merge cleanly onto the
// stack's base tip, as a refused update of that head to that tip: a round
// for its conflict gets its round at once, whatever its place in the line
// (WP-ADR152 removed rounds timed to a place), and it finds the conflict in
// its work packet (packetConflict). Its babysit is nudged to take the round
// now. Kept in memory like the update's own.
func (s *Service) stackBaseConflict(st *queueStack, h stackHead) {
	if st.BaseSHA == "" || h.SHA == "" {
		return
	}
	key := fmt.Sprintf("%d@%s", h.PRID, st.BaseSHA)
	if v, had := s.baseUpdated.Load(key); had && sameSHA(v.(baseUpdate).from, h.SHA) {
		return
	}
	s.baseUpdated.Store(key, baseUpdate{outcome: upConflict, at: s.now(), from: h.SHA, n: 1})
	s.nudgeBabysit(h.PRID, false)
}

// stackConflictAt is the index in st's heads of the head its run says
// doesn't merge cleanly onto the ones before it, -1 when no log read says,
// and whether the log says it doesn't merge onto the base's tip alone
// either. The failed jobs' logs are read first: any job that ran the stack
// step stopped there.
func (s *Service) stackConflictAt(ctx context.Context, f *Forge, st *queueStack, checks []CommitCheck) (int, bool) {
	jl, ok := s.Forges.(ForgeJobLogs)
	if !ok {
		return -1, false
	}
	var try []CommitCheck
	for _, c := range checks {
		if c.URL != "" && !skippedCheck(c) {
			try = append(try, c)
		}
	}
	slices.SortStableFunc(try, func(a, b CommitCheck) int {
		return boolRank(a.State != CheckFailed) - boolRank(b.State != CheckFailed)
	})
	for i, c := range try {
		if i == 6 {
			break
		}
		log, err := jl.JobLog(ctx, f, st.Repo, st.CommitSHA, c)
		if err != nil {
			continue
		}
		if k := parseConflictLog(log, st.Heads); k >= 0 {
			return k, onBaseLog(log, st.Heads[k], st.BaseSHA)
		}
	}
	return -1, false
}

// onBaseLog: log says head h doesn't merge cleanly onto base alone.
func onBaseLog(log string, h stackHead, base string) bool {
	for _, m := range baseConflictLog.FindAllStringSubmatch(log, -1) {
		if m[1] == strconv.Itoa(h.Number) && sameSHA(h.SHA, m[2]) && sameSHA(base, m[3]) {
			return true
		}
	}
	return false
}

// parseConflictLog: the index of the head the first conflict line names,
// -1 when there is none or it names no head of heads.
func parseConflictLog(log string, heads []stackHead) int {
	m := conflictLog.FindStringSubmatch(log)
	if m == nil {
		return -1
	}
	for i, h := range heads {
		if strconv.Itoa(h.Number) == m[1] && sameSHA(h.SHA, m[2]) {
			return i
		}
	}
	return -1
}

func headNames(heads []stackHead) string {
	var parts []string
	for _, h := range heads {
		parts = append(parts, fmt.Sprintf("#%d", h.Number))
	}
	if len(parts) == 0 {
		return "the base"
	}
	return "the base + " + strings.Join(parts, " + ")
}
