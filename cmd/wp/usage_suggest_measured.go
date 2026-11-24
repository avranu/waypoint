package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Session-start snapshots are measured; disk sizes are only an estimate of what a session read.

// sugMeasuredMax is the most sessions looked up (the newest first), so a long
// window is a bounded number of calls.
const sugMeasuredMax = 30

// sugMeasured is the median of the session-start snapshots found. Each
// figure is the median over the snapshots that carry it, and says how many
// those were: a snapshot without an instruction-file inventory is not a
// session with no instruction files.
type sugMeasured struct {
	Sessions            int   `json:"sessions"`             // with a snapshot carrying any of the figures
	Of                  int   `json:"of"`                   // asked about
	InstructionSessions int   `json:"instruction_sessions"` // of Sessions, the ones whose snapshot listed the instruction files
	InstructionBytes    int64 `json:"instruction_bytes"`
	InstructionFiles    int64 `json:"instruction_files"`
	MemoryIndexSessions int   `json:"memory_index_sessions"`
	MemoryIndexBytes    int64 `json:"memory_index_bytes"`
	MemorySessions      int   `json:"memory_sessions"`
	MemoryBytes         int64 `json:"memory_bytes"`
	MemoryFiles         int64 `json:"memory_files"`
}

type sugSnapshot struct {
	MemoryFiles      *int64          `json:"memory_files"`
	MemoryBytes      *int64          `json:"memory_bytes"`
	MemoryIndexBytes *int64          `json:"memory_index_bytes"`
	Inventory        json.RawMessage `json:"inventory"`
}

// measuredStart looks the sessions up on the server, by the CLI's session id,
// and reads each one's first snapshot. nil, with why, when there is none to
// read: no server, nothing found, or the server did not answer.
func measuredStart(c *cli, ids []string) (*sugMeasured, string) {
	if c.server() == "" {
		return nil, "not signed in to a Waypoint server, so no session-start snapshots to read"
	}
	if len(ids) == 0 {
		return nil, ""
	}
	if len(ids) > sugMeasuredMax {
		ids = ids[:sugMeasuredMax]
	}
	var instr, files, idx, mem, memFiles []int64
	sessions := 0
	for _, id := range ids {
		var x struct {
			ID int64 `json:"id"`
		}
		q := url.Values{"cli_id": {id}, "machine": {machineName()}}
		if err := c.do("GET", "/api/v1/sessions/current?"+q.Encode(), nil, &x); err != nil || x.ID == 0 {
			continue
		}
		var snaps []sugSnapshot
		if err := c.do("GET", fmt.Sprintf("/api/v1/sessions/%d/snapshots", x.ID), nil, &snaps); err != nil || len(snaps) == 0 {
			continue
		}
		s := snaps[0]
		var inv struct {
			Files []struct {
				Bytes int64 `json:"bytes"`
			} `json:"instruction_files"`
		}
		got := false
		if len(s.Inventory) > 0 && json.Unmarshal(s.Inventory, &inv) == nil && inv.Files != nil {
			var sum int64
			for _, f := range inv.Files {
				sum += f.Bytes
			}
			instr, files = append(instr, sum), append(files, int64(len(inv.Files)))
			got = true
		}
		if s.MemoryIndexBytes != nil {
			idx = append(idx, *s.MemoryIndexBytes)
			got = true
		}
		if s.MemoryBytes != nil && s.MemoryFiles != nil {
			mem, memFiles = append(mem, *s.MemoryBytes), append(memFiles, *s.MemoryFiles)
			got = true
		}
		if got {
			sessions++
		}
	}
	if sessions == 0 {
		return nil, fmt.Sprintf("no session-start snapshot for %s of these (the hooks take one: wp hook claude)", plural1(len(ids), "session"))
	}
	return &sugMeasured{Sessions: sessions, Of: len(ids),
		InstructionSessions: len(instr), InstructionBytes: median(instr), InstructionFiles: median(files),
		MemoryIndexSessions: len(idx), MemoryIndexBytes: median(idx),
		MemorySessions: len(mem), MemoryBytes: median(mem), MemoryFiles: median(memFiles)}, ""
}

// text is the measured sizes as a sentence, in tokens as estimated from
// bytes. A figure no snapshot carried is said to be missing, not given as 0,
// and one fewer snapshots carried says how many.
func (m *sugMeasured) text() string {
	from := func(n int) string {
		if n == m.Sessions {
			return ""
		}
		return fmt.Sprintf(" (from %d of them)", n)
	}
	var parts []string
	if m.InstructionSessions > 0 {
		parts = append(parts, fmt.Sprintf("instruction files %s tokens in %s%s", tok(tokOf(m.InstructionBytes)), plural1(int(m.InstructionFiles), "file"), from(m.InstructionSessions)))
	} else {
		parts = append(parts, "instruction files not listed in any of them")
	}
	if m.MemoryIndexSessions > 0 {
		parts = append(parts, fmt.Sprintf("memory index %s tokens%s", tok(tokOf(m.MemoryIndexBytes)), from(m.MemoryIndexSessions)))
	}
	if m.MemorySessions > 0 && m.MemoryFiles > 0 {
		parts = append(parts, fmt.Sprintf("%s in all, %s tokens%s", plural1(int(m.MemoryFiles), "memory file"), tok(tokOf(m.MemoryBytes)), from(m.MemorySessions)))
	}
	return fmt.Sprintf("Measured when the sessions started (median of the %d of %d with a snapshot): %s; at about 4 bytes a token.", m.Sessions, m.Of, strings.Join(parts, ", "))
}
