package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// The server's compacted flag is checked against the transcript: a session that worked after its compaction is not compacted.

// transcriptCompaction is what a session's transcript says about its last
// compaction.
type transcriptCompaction struct {
	compacted bool
	replies   int
}

// readCompaction reads a Claude Code transcript for its last compaction
// (a compact_boundary) and the main conversation's replies since. A
// compaction is followed only by its summary and command output until the
// next prompt, so a reply means the session has worked since.
func readCompaction(path string) (transcriptCompaction, error) {
	var out transcriptCompaction
	f, err := os.Open(path) // #nosec G304 -- Claude Code's own transcript of the session
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	seen := map[string]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`compact_boundary`)) && !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Sidechain bool   `json:"isSidechain"`
			Message   struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Sidechain {
			continue
		}
		switch {
		case e.Type == "system" && e.Subtype == "compact_boundary":
			out.compacted, out.replies = true, 0
			clear(seen)
		case e.Type == "assistant":
			// Each content block of one reply is its own transcript line with the same message id: count the reply once.
			if e.Message.ID == "" || !seen[e.Message.ID] {
				out.replies++
				seen[e.Message.ID] = e.Message.ID != ""
			}
		}
	}
	return out, sc.Err()
}

// reviewCompaction decides whether the review job compacts the session
// before typing its review, and says why.
func reviewCompaction(transcript string, serverSaysCompacted bool) (compact bool, why string) {
	if transcript == "" {
		if serverSaysCompacted {
			return false, "it was compacted while its review waited for the audit (no transcript here to check)"
		}
		return true, "it wasn't compacted for its review yet"
	}
	c, err := readCompaction(transcript)
	switch {
	case err != nil && serverSaysCompacted:
		return false, fmt.Sprintf("it was compacted while its review waited for the audit (its transcript couldn't be read: %v)", err)
	case err != nil:
		return true, fmt.Sprintf("it wasn't compacted for its review yet (its transcript couldn't be read: %v)", err)
	case !c.compacted:
		if serverSaysCompacted {
			return true, "Waypoint had it compacted, but its transcript shows no compaction"
		}
		return true, "it has never been compacted"
	case c.replies > 0:
		replies := fmt.Sprintf("%d %s", c.replies, pluralWord(c.replies, "reply", "replies"))
		if serverSaysCompacted {
			return true, "it was compacted while its review waited, but has worked since (" + replies + ")"
		}
		return true, "it has worked since its last compaction (" + replies + ")"
	case serverSaysCompacted:
		return false, "it was compacted while its review waited for the audit, and its transcript shows nothing since"
	default:
		return false, "its transcript shows it compacted with nothing since"
	}
}
