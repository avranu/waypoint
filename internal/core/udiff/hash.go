package udiff

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ChangeHash hashes what a git diff changes and where each change sits: the
// files (paths, renames, modes, binary patches) and, for each run of added and
// removed lines, the run itself, the unchanged line just before and just after
// it (a marker where the hunk starts or ends there) and its hunk's heading (the
// function git names after the "@@" line numbers). Left out: the line numbers,
// "similarity" lines, "index" lines (but a binary file's, its only content) and the rest of the context. A merge of the
// base that moves a change down the file leaves the hash alone; one that edits
// the lines beside a change or the function it is in, or resolves a conflict,
// changes it (WP-1808 then reviews the head: the conservative side). Line
// endings are content: "+b" and "+b\r" differ. Without the neighbours and the
// heading, moving an added line from one function to another could hash the
// same (two functions both have "if enabled {" and "}"); every run in a hunk
// carries the heading, not only the hunk's first.
func ChangeHash(diff string) string {
	h := sha256.New()
	put := func(l string) {
		h.Write([]byte(strconv.Itoa(len(l))))
		h.Write([]byte{':'})
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	const start, end = "\x00hunk-start", "\x00hunk-end"
	inHunk, inBinary := false, false
	index := "" // the file's index line, kept for a binary
	before, heading := start, ""
	var run []string
	flush := func(after string) {
		if len(run) == 0 {
			return
		}
		put("@" + heading)
		put("<" + before)
		for _, l := range run {
			put(l)
		}
		put(">" + after)
		run = run[:0]
	}
	lines := strings.Split(diff, "\n")
	for n, l := range lines {
		if n == len(lines)-1 && l == "" {
			break
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush(end)
			inHunk, inBinary, before, heading, index = false, false, start, "", ""
			put(l)
		case !inHunk && strings.HasPrefix(l, "Binary files "):
			// A binary's only content in the diff is its blob ids, on the
			// index line: another blob is another change.
			put(index)
			put(l)
		case inBinary:
			put(l)
		case strings.HasPrefix(l, "GIT binary patch"):
			inBinary = true
			put(l)
		case strings.HasPrefix(l, "@@"):
			flush(end)
			// "@@ -a,b +c,d @@ heading": the heading, without the numbers.
			_, fn, _ := strings.Cut(l[2:], "@@")
			inHunk, before, heading = true, start, strings.TrimSpace(fn)
		case inHunk && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			run = append(run, l)
		case inHunk && strings.HasPrefix(l, "\\"):
			if len(run) > 0 {
				run = append(run, l) // "\ No newline at end of file" belongs to the line before
			}
		case inHunk: // an unchanged line
			flush(l)
			before = l
		case strings.HasPrefix(l, "index "):
			index = l
		case strings.HasPrefix(l, "similarity index "), strings.HasPrefix(l, "dissimilarity index "):
		default:
			put(l)
		}
	}
	flush(end)
	return hex.EncodeToString(h.Sum(nil))
}
