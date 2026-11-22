package main

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// What the hooks measured at the start of the sessions wp usage suggest looks
// at (the session_snapshots Waypoint keeps), against the instruction files as
// they are on disk now (WP-1470): the second is only an estimate of what a
// session read, the first is what its own hook saw on the machine.

// sugMeasuredMax is the most sessions looked up (the newest first), so a long
// window is a bounded number of calls.
const sugMeasuredMax = 30

// sugMeasured is the median of the session-start snapshots found.
type sugMeasured struct {
	Sessions         int   `json:"sessions"` // with a snapshot
	Of               int   `json:"of"`       // asked about
	InstructionBytes int64 `json:"instruction_bytes"`
	InstructionFiles int64 `json:"instruction_files"`
	MemoryIndexBytes int64 `json:"memory_index_bytes"`
	MemoryBytes      int64 `json:"memory_bytes"`
	MemoryFiles      int64 `json:"memory_files"`
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
		s := snaps[0] // the first, taken as the session started
		var inv struct {
			Files []struct {
				Bytes int64 `json:"bytes"`
			} `json:"instruction_files"`
		}
		if len(s.Inventory) > 0 && json.Unmarshal(s.Inventory, &inv) == nil && inv.Files != nil {
			var sum int64
			for _, f := range inv.Files {
				sum += f.Bytes
			}
			instr, files = append(instr, sum), append(files, int64(len(inv.Files)))
		}
		if s.MemoryIndexBytes != nil {
			idx = append(idx, *s.MemoryIndexBytes)
		}
		if s.MemoryBytes != nil {
			mem = append(mem, *s.MemoryBytes)
		}
		if s.MemoryFiles != nil {
			memFiles = append(memFiles, *s.MemoryFiles)
		}
	}
	n := max(len(instr), len(idx), len(mem))
	if n == 0 {
		return nil, fmt.Sprintf("no session-start snapshot for %s (the hooks take one: wp hook claude)", plural1(len(ids), "of these sessions"))
	}
	return &sugMeasured{Sessions: n, Of: len(ids), InstructionBytes: median(instr), InstructionFiles: median(files),
		MemoryIndexBytes: median(idx), MemoryBytes: median(mem), MemoryFiles: median(memFiles)}, ""
}

// text is the measured sizes as a sentence, in tokens as estimated from bytes.
func (m *sugMeasured) text() string {
	s := fmt.Sprintf("Measured when the sessions started (median of %d of %d): instruction files %s in %s, memory index %s",
		m.Sessions, m.Of, tok(tokOf(m.InstructionBytes)), plural1(int(m.InstructionFiles), "file"), tok(tokOf(m.MemoryIndexBytes)))
	if m.MemoryFiles > 0 {
		s += fmt.Sprintf(" (%s in all, %s)", plural1(int(m.MemoryFiles), "memory file"), tok(tokOf(m.MemoryBytes)))
	}
	return s + " tokens, at about 4 bytes a token."
}
