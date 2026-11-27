package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	chainFinds = 2
	chainCalls = 5
	chainKeep  = 3 * 24 * time.Hour
	chainKind  = "search-chain"
)

const chainText = "grep and sed waste tokens: wp code find WORDS, wp code show FILE:SYMBOL, and wp brief \"QUESTION\" are usually better."

// chainState is one transcript's counts.
type chainState struct {
	Finds int  `json:"finds"`
	Looks int  `json:"looks"`
	Done  bool `json:"done"` // told, or ran wp code or wp brief itself
	// LateTells counts the late runs that told without marking (WP-1398).
	LateTells int `json:"late_tells,omitempty"`
}

// The steps of a chain.
const (
	stepFind = "find"
	stepLook = "look"
	stepWP   = "wp"
)

var reSedPrint = lazyre.New(`^['"]?(\$|\d+)(,(\$|\d+))?p['"]?$`)

// chainStep says which step of a chain a call is, or "".
func chainStep(tool, cmd, path string) string {
	switch tool {
	case "Read":
		if path != "" {
			return stepLook
		}
		return ""
	case "Bash":
	default:
		return ""
	}
	if len(cmd) > 2000 {
		return ""
	}
	step := ""
	for _, seg := range shellWords(cmd) {
		words := stripWords(seg)
		if len(words) == 0 {
			continue
		}
		prog := filepath.Base(words[0])
		switch {
		case prog == "wp" && len(words) > 1 && (words[1] == "code" || words[1] == "brief"):
			return stepWP
		case searchPattern(words) != "":
			step = stepFind
		case step == "" && sliceOfFile(prog, words[1:]):
			step = stepLook
		}
	}
	return step
}

// sliceOfFile: the command reads part of a named file.
func sliceOfFile(prog string, args []string) bool {
	var pos []string
	quiet := false
	for _, a := range args {
		switch {
		case a == "-n":
			quiet = true
		case strings.HasPrefix(a, "-") && len(a) > 1:
		default:
			pos = append(pos, a)
		}
	}
	switch prog {
	case "sed":
		return quiet && len(pos) >= 2 && reSedPrint.MatchString(pos[0])
	case "head", "tail", "cat", "nl":
		for _, p := range pos {
			if !strings.HasPrefix(p, "/dev/") && !strings.HasPrefix(p, "+") && filepath.Ext(p) != "" {
				return true
			}
		}
	case "grep", "egrep", "fgrep", "rg":
		return len(pos) >= 2 && allFiles(pos[1:])
	}
	return false
}

// inCheckout: dir is in a git checkout, found by looking for .git upward
// (no subprocess: this runs before every call).
func inCheckout(dir string) bool {
	if dir == "" {
		return false
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// chainPath is the transcript's state file: the session, and the subagent
// when the call is a subagent's.
func (c *cli) chainPath(session, agent string) string {
	if c.dir == "" || !reSafeID.MatchString(session) || agent != "" && !reSafeID.MatchString(agent) {
		return ""
	}
	name := session
	if agent != "" {
		name += "." + agent
	}
	return filepath.Join(c.dir, "nudge", name+".json")
}

// chainNudge counts the call in its transcript and says whether this is the
// call to tell; canTell is false when another note takes the call, so the
// chain keeps counting and tells on a later call. Calls outside a git checkout aren't counted.
func (c *cli) chainNudge(in *hookEventInput, cwd, step string, canTell bool) bool {
	if step == "" || !inCheckout(cwd) {
		return false
	}
	path := c.chainPath(in.SessionID, in.AgentID)
	if path == "" {
		return false
	}
	unlock, err := lockFileWithin(path+".lock", hookLockWait(hookLateAfter(hookNudgeTimeout)))
	if err != nil {
		return false
	}
	defer unlock()
	if _, err := os.Stat(path); err != nil {
		pruneChains(filepath.Dir(path), time.Now())
	}
	st := &chainState{}
	if readJSON(path, st) != nil {
		return false
	}
	if st.Done {
		return false
	}
	tell := false
	switch step {
	case stepWP:
		st.Done = true
	case stepFind:
		st.Finds++
	case stepLook:
		st.Looks++
	}
	if !st.Done && canTell && st.Finds >= chainFinds && st.Finds+st.Looks >= chainCalls {
		tell = true
		if hookLate(hookLateAfter(hookNudgeTimeout)) {
			st.LateTells++
			st.Done = st.LateTells >= lateTellsMax
		} else {
			st.Done = true
		}
	}
	if writeJSON(path, st) != nil {
		return false
	}
	return tell
}

// pruneChains removes state files older than chainKeep.
func pruneChains(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > chainKeep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// checkoutRoot is the directory holding .git at or above dir; "" if none.
func checkoutRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// checkoutRel is abs relative to its checkout root; "" outside one.
func checkoutRel(abs string) string {
	root := checkoutRoot(filepath.Dir(abs))
	if root == "" {
		return ""
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// outlineHint is the big-read note's pointer to wp code show, for a file in
// the working directory's own git checkout whose language the code index
// outlines; "" otherwise (wp code show resolves against cwd's checkout).
func outlineHint(abs, cwd string) string {
	if abs == "" {
		return ""
	}
	root := checkoutRoot(filepath.Dir(abs))
	if root == "" || root != checkoutRoot(cwd) {
		return ""
	}
	if lang := codeindex.Language(abs); lang == "" || lang == "text" || lang == "css" {
		return ""
	}
	name := checkoutRel(abs)
	if name == "" {
		return ""
	}
	return "`wp code show " + name + "` for its outline, `" + name + ":SYMBOL` for one function"
}
