// Package testevidence is how a CI job hands Waypoint what its tests found
// (WP-704): each top-level test's outcome and time, which coverage blocks
// ran, and the accepted lint findings (#nosec, //nolint) of the tree.
//
// CI computed all of it and threw it away: coverage reached only the log,
// four partial numbers for internal/core, and no table held a test's
// result. The job now prints its evidence at the end of its log, as a few
// lines that start with Prefix, and Waypoint reads the log of every job
// that has carried such lines before (internal/core/ci_evidence.go). No
// token or upload: Waypoint already reads job logs with the forge's token,
// so nothing new is trusted. The payload is JSON, gzipped, base64, cut into
// lines of at most chunkSize characters.
//
// cmd/ci-evidence writes it from `go test -json` output, cover profiles
// and the source; any job (another language's tests) may print the same
// lines.
package testevidence

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Prefix starts every evidence line; Version is the payload's format.
const (
	Prefix  = "wp-evidence"
	Version = 1
	// chunkSize keeps each log line short enough for any log viewer.
	chunkSize = 4000
	// maxPayload bounds a decoded payload: a log can't make Waypoint
	// inflate more than this.
	maxPayload = 64 << 20
)

// Payload is one job's evidence.
type Payload struct {
	V int `json:"v"`
	// Shard is "I/N" when the job is one of N shards of the same tests:
	// their coverage adds up to the tree's.
	Shard string `json:"shard,omitempty"`
	// Tests are the top-level tests that ended (subtests count in theirs).
	Tests []Test `json:"tests"`
	// Cover is the coverage of every file the tests' packages hold, in
	// blocks; nil when the job measured none.
	Cover []File `json:"cover,omitempty"`
	// Suppressions are the tree's accepted findings; ScannedSuppressions
	// says the job looked (none found is then a fact, not a gap).
	Suppressions        []Suppression `json:"suppressions,omitempty"`
	ScannedSuppressions bool          `json:"scanned_suppressions,omitempty"`
}

// Test outcomes.
const (
	Pass = "pass"
	Fail = "fail"
	Skip = "skip"
)

// Test is one top-level test's result.
type Test struct {
	Package string `json:"package"` // repo-relative directory, e.g. internal/core
	Name    string `json:"name"`
	Outcome string `json:"outcome"` // pass, fail or skip
	Ms      int64  `json:"ms"`
}

// File is one source file's coverage: each block's statement count, in
// source order, and which blocks ran (bit i of Hit, low bit first).
// Shards of the same tree have the same blocks, so their Hit sets OR.
type File struct {
	Path  string `json:"path"` // repo-relative
	Stmts []int  `json:"stmts"`
	Hit   []byte `json:"hit"`
}

// Statements is the file's statement count and how many of them ran.
func (f File) Statements() (total, covered int) {
	for i, n := range f.Stmts {
		total += n
		if Ran(f.Hit, i) {
			covered += n
		}
	}
	return total, covered
}

// Ran says whether bit i of a Hit set is on.
func Ran(hit []byte, i int) bool {
	return i/8 < len(hit) && hit[i/8]&(1<<(i%8)) != 0
}

// Suppression kinds.
const (
	NoSec   = "nosec"  // gosec's #nosec
	NoLint  = "nolint" // golangci-lint's //nolint
	WPLint  = "wplint" // wplint's //wplint:ignore (internal/wplint)
	allRule = "all"
)

// Suppression is one accepted finding in the source: the rules it
// silences, where, why (the text after "--" or the second "//"), and the
// code it applies to with its whitespace squeezed, which outlives line
// moves.
type Suppression struct {
	Kind   string   `json:"kind"`
	Rules  []string `json:"rules,omitempty"` // none: every rule
	Path   string   `json:"path"`
	Line   int      `json:"line"`
	Code   string   `json:"code"`
	Reason string   `json:"reason,omitempty"`
}

// Encode writes p as evidence lines.
func Encode(w io.Writer, p *Payload) error {
	p.V = Version
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(p); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	s := base64.StdEncoding.EncodeToString(buf.Bytes())
	n := (len(s) + chunkSize - 1) / chunkSize
	for i := range n {
		end := min(len(s), (i+1)*chunkSize)
		if _, err := fmt.Fprintf(w, "%s v%d %d/%d %s\n", Prefix, Version, i+1, n, s[i*chunkSize:end]); err != nil {
			return err
		}
	}
	return nil
}

var lineRE = lazyre.New(`^` + Prefix + ` v(\d+) (\d+)/(\d+) ([A-Za-z0-9+/=]+)$`)

// ErrNone: the log carries no evidence.
var ErrNone = errors.New("no evidence in the log")

// Decode finds the evidence in a log's lines (forge timestamps and colour
// already stripped) and returns its last complete payload, ErrNone when
// there is none, or why a payload it found can't be read.
func Decode(lines []string) (*Payload, error) {
	var (
		chunks []string
		want   int
		last   string
		bad    error
		seen   bool
	)
	for _, l := range lines {
		m := lineRE.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		seen = true
		if v, _ := strconv.Atoi(m[1]); v != Version {
			bad = fmt.Errorf("evidence format v%d, this reads v%d", v, Version)
			continue
		}
		i, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		switch {
		case i == 1:
			chunks, want = []string{m[4]}, n
		case n == want && i == len(chunks)+1:
			chunks = append(chunks, m[4])
		default:
			chunks, want = nil, 0 // out of order: wait for the next payload
			continue
		}
		if len(chunks) == want {
			last = strings.Join(chunks, "")
			chunks, want = nil, 0
		}
	}
	if last == "" {
		if bad != nil {
			return nil, bad
		}
		if seen {
			// Evidence lines, but no whole payload: the log was cut.
			return nil, errors.New("evidence: no whole payload (the log was cut?)")
		}
		return nil, ErrNone
	}
	raw, err := base64.StdEncoding.DecodeString(last)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	var p Payload
	dec := json.NewDecoder(io.LimitReader(zr, maxPayload))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	if p.V != Version {
		return nil, fmt.Errorf("evidence format v%d, this reads v%d", p.V, Version)
	}
	return &p, nil
}

// ShardOf parses "I/N"; ok is false for anything else.
func ShardOf(s string) (i, n int, ok bool) {
	a, b, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	i, err1 := strconv.Atoi(a)
	n, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || n < 1 || i < 1 || i > n {
		return 0, 0, false
	}
	return i, n, true
}
