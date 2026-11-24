package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Batching several targets per call exists because each tool call re-reads an agent's whole context.

// listArgs splits arguments on commas as well as spaces, and gives a bare
// number the prefix of the code before it: "R-5," "6" is R-5 and R-6,
// WP-12,15 is WP-12 and WP-15. Only a prefix ending in a dash carries: a
// finding's 281 after a comment's c14 stays 281.
func listArgs(pos []string) []string {
	var out []string
	prefix := ""
	for _, a := range pos {
		for _, s := range strings.Split(a, ",") {
			s = strings.TrimSpace(s)
			switch {
			case s == "":
				continue
			case isDigits(s):
				s = prefix + s
			default:
				prefix = numberPrefix(s)
			}
			out = append(out, s)
		}
	}
	return out
}

// numberPrefix is what comes before a code's number (R- of R-12), or ""
// when it isn't KEY-N shaped.
func numberPrefix(s string) string {
	p := strings.TrimRight(s, "0123456789")
	if p == s || !strings.HasSuffix(p, "-") {
		return ""
	}
	return p
}

// each runs do for every target in turn. One that fails doesn't stop the
// rest: it is reported on stderr with the target, and the command exits 1
// at the end. gap puts a blank line between long outputs (wp show's).
func (c *cli) each(targets []string, gap bool, do func(target string) error) error {
	if len(targets) == 1 {
		return do(targets[0])
	}
	failed := 0
	// With --json the batch is one JSON array, one element per target that printed output, whether it failed or not.
	var results []json.RawMessage
	realOut := c.out
	if c.asJSON {
		defer func() { c.out = realOut }()
	}
	for i, t := range targets {
		if gap && i > 0 && !c.asJSON {
			c.println()
		}
		var buf bytes.Buffer
		if c.asJSON {
			c.out = &buf
		}
		err := do(t)
		if c.asJSON {
			c.out = realOut
			if b := bytes.TrimSpace(buf.Bytes()); len(b) > 0 {
				if json.Valid(b) {
					results = append(results, json.RawMessage(b))
				} else if q, merr := json.Marshal(string(b)); merr == nil {
					results = append(results, q)
				}
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, errHelpShown) {
			return err
		}
		failed++
		var ec exitCode
		if !errors.As(err, &ec) {
			fmt.Fprintf(c.err, "wp %s: %s: %v\n", c.cur.path(), t, err)
		}
	}
	if c.asJSON {
		if results == nil {
			results = []json.RawMessage{}
		}
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		c.println(string(b))
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// currentKeys is currentKey for commands that take several issues: the
// leading arguments that are issue keys (WP-12 WP-15, WP-12,15), else the
// task taken here. The rest are the command's other arguments.
func (c *cli) currentKeys(pos []string) ([]string, []string, error) {
	n, prefix := 0, ""
	for n < len(pos) {
		var ok bool
		if ok, prefix = keyList(pos[n], prefix); !ok {
			break
		}
		n++
	}
	if n == 0 {
		key, rest, err := c.currentKey(pos)
		if err != nil {
			return nil, rest, err
		}
		return []string{key}, rest, nil
	}
	var keys []string
	for _, k := range listArgs(pos[:n]) {
		keys = append(keys, normalKey(k))
	}
	return keys, pos[n:], nil
}

// keyList says whether an argument is issue keys, separated by commas; a
// bare number counts only after a key it takes the prefix of (WP-12, 15).
// prefix is that of the key before; it returns the one after.
func keyList(arg, prefix string) (bool, string) {
	parts := 0
	for _, s := range strings.Split(arg, ",") {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			continue
		case isDigits(s):
			if prefix == "" {
				return false, prefix
			}
		case !keyRE.MatchString(normalKey(s)):
			return false, prefix
		default:
			prefix = numberPrefix(s)
		}
		parts++
	}
	return parts > 0, prefix
}

// normalKey is an issue key as typed (app-12., issue:APP-12) in its usual form.
func normalKey(s string) string {
	s = strings.ToUpper(strings.TrimRight(s, ".;:"))
	return strings.TrimPrefix(s, "ISSUE:")
}
