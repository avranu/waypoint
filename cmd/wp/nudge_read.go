package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Big-read nudges (WP-145): a whole-file Read, `cat FILE` or a wide
// `sed -n A,Bp` of a large file is re-read by every later call, so the note
// names a cheaper way in. Same contract as the Bash nudges: a note, never a
// refusal, no network, a few ms (the file is stat'ed and at most bigReadSample
// bytes read; longer files get an estimate from size and average line length).

const (
	bigReadLines  = 800 // a Read/cat of more lines than this is nudged
	bigSedLines   = 400 // a sed range wider than this is nudged
	bigReadSample = 64 << 10
)

var (
	// cat [-flags] FILE as a whole command: not piped on, not redirected.
	reCatFile = lazyre.New("(?:^|[;&]\\s*)cat\\s+(?:-[A-Za-z]+\\s+)*([^\\s|;&<>$`\"'*?]+)\\s*(?:$|;|&&)")
	// sed -n 'A,Bp' [FILE]
	reSedRange = lazyre.New(`\bsed\s+(?:-[A-Za-z]+\s+)*-n\s*['"]?(\d+),(\d+)p['"]?`)
)

// fileLines is the line count of path, exact up to bigReadSample bytes and an
// estimate beyond; 0 when it can't be read.
func fileLines(path string) int {
	f, err := os.Open(path) // #nosec G304 -- the path the agent is about to read anyway
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return 0
	}
	buf := make([]byte, bigReadSample)
	n, _ := f.Read(buf)
	buf = buf[:n]
	lines := bytes.Count(buf, []byte{'\n'})
	if st.Size() <= int64(n) {
		return lines
	}
	if lines == 0 {
		return 1
	}
	return int(st.Size() * int64(lines) / int64(n))
}

func bigReadPath(p, cwd string) string {
	if p != "" && !filepath.IsAbs(p) {
		return filepath.Join(cwd, p)
	}
	return p
}

// bigReadNote: abs is the file when known, for the wp code show pointer.
func bigReadNote(subject, abs, cwd string) string {
	s := subject + ": use `grep -n`, Read with offset/limit"
	if h := outlineHint(abs, cwd); h != "" {
		s += ", or " + h
	}
	return s + "."
}

// bigReadFor returns the nudge for a Read call (path, limit) or a Bash
// command; kind is "" when there is none.
func bigReadFor(tool, cwd, path string, limit int, cmd string) (kind, text string) {
	switch tool {
	case "Read":
		if limit > 0 || path == "" {
			return
		}
		if n := fileLines(bigReadPath(path, cwd)); n > bigReadLines {
			return "big-read", bigReadNote(fmt.Sprintf("%s has ~%d lines", filepath.Base(path), n), bigReadPath(path, cwd), cwd)
		}
	case "Bash":
		if len(cmd) > 2000 {
			return
		}
		if m := reCatFile.FindStringSubmatch(cmd); m != nil {
			if n := fileLines(bigReadPath(m[1], cwd)); n > bigReadLines {
				return "big-cat", bigReadNote(fmt.Sprintf("%s has ~%d lines", filepath.Base(m[1]), n), bigReadPath(m[1], cwd), cwd)
			}
		}
		if m := reSedRange.FindStringSubmatch(cmd); m != nil {
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			if b-a+1 > bigSedLines {
				return "big-sed", bigReadNote(fmt.Sprintf("sed %d,%dp spans %d lines", a, b, b-a+1), "", cwd)
			}
		}
	}
	return
}
