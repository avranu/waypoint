package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// UsageTrace is one task segment of one session, as uploaded.
type UsageTrace struct {
	Key          string      `json:"key"` // the task, as wp next printed it
	Session      string      `json:"s"`
	Seq          int         `json:"seq"` // the session's nth take, from 1
	Started      int64       `json:"started"`
	Last         int64       `json:"last"`
	Ended        string      `json:"ended,omitempty"` // ship, done, release, next; empty: still open
	Model        string      `json:"model,omitempty"`
	Calls        int64       `json:"calls"`
	Locate       int64       `json:"locate"`
	Infile       int64       `json:"infile"`
	Reads        int64       `json:"reads"`
	Edits        int64       `json:"edits"`
	Delegated    int64       `json:"delegated"`
	Other        int64       `json:"other"`
	PreCalls     int64       `json:"pre_calls"`
	PreLocate    int64       `json:"pre_locate"`
	PreInfile    int64       `json:"pre_infile"`
	PreReads     int64       `json:"pre_reads"`
	PreDelegated int64       `json:"pre_delegated"`
	FirstEditAt  *int64      `json:"first_edit_at,omitempty"`
	CtxStart     int64       `json:"ctx_start"`
	CtxFirstEdit *int64      `json:"ctx_first_edit,omitempty"`
	Compacted    bool        `json:"compacted,omitempty"`
	Files        []TraceFile `json:"files,omitempty"`
}

// TraceFile is one file a task segment touched, in first-touch order.
type TraceFile struct {
	Path    string `json:"p"`           // from the root of its checkout
	Repo    string `json:"o,omitempty"` // owner/name of its repository, when the checkout's origin told: a take's files are not all in one
	Nav     int    `json:"n"`           // navigation calls (locate+infile+reads) in the segment before its first touch
	Read    bool   `json:"r,omitempty"`
	Edited  bool   `json:"e,omitempty"`
	Seen    bool   `json:"s,omitempty"` // an earlier tool result in the segment had named it
	Created bool   `json:"c,omitempty"` // the agent made it: its first touch was a Write of a file not read before
}

// TaskTrace is a stored trace.
type TaskTrace struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	User      string `json:"user,omitempty"`
	Machine   string `json:"machine"`
	IssueID   int64  `json:"issue_id"`
	UpdatedAt int64  `json:"updated_at"`
	UsageTrace
}

const (
	traceMaxFiles = 80
	traceMaxPath  = 300
	// traceMaxCount bounds every count of a trace: no take makes a million
	// calls, and a sum of counts must stay a number the policy can fit.
	traceMaxCount = 1_000_000
	traceMaxCtx   = 100_000_000
)

var traceKeyRE = lazyre.New(`^[A-Za-z][A-Za-z0-9]{1,9}-\d+$`)

func validTraceEnd(e string) bool {
	switch e {
	case "", "ship", "done", "release", "next":
		return true
	}
	return false
}

// cleanTrace brings an uploaded trace within bounds; false drops it.
func cleanTrace(r *UsageTrace, maxMS int64) bool {
	r.Key, r.Session = strings.TrimSpace(r.Key), clip(r.Session, 80)
	if !traceKeyRE.MatchString(r.Key) || r.Session == "" || r.Seq < 1 || !validTraceEnd(r.Ended) {
		return false
	}
	if r.Started < usageMinHour || r.Started > maxMS || r.Last < r.Started {
		return false
	}
	r.Last = min(r.Last, maxMS)
	r.Model = clip(r.Model, 100)
	for _, p := range []*int64{&r.Calls, &r.Locate, &r.Infile, &r.Reads, &r.Edits, &r.Delegated, &r.Other, &r.PreCalls, &r.PreLocate, &r.PreInfile,
		&r.PreReads, &r.PreDelegated} {
		*p = min(max(*p, 0), traceMaxCount)
	}
	r.CtxStart = min(max(r.CtxStart, 0), traceMaxCtx)
	if r.CtxFirstEdit != nil && (*r.CtxFirstEdit < 0 || *r.CtxFirstEdit > traceMaxCtx) {
		r.CtxFirstEdit = nil
	}
	if r.FirstEditAt != nil && (*r.FirstEditAt < r.Started || *r.FirstEditAt > maxMS) {
		return false
	}
	files := make([]TraceFile, 0, min(len(r.Files), traceMaxFiles))
	for _, f := range r.Files {
		f.Path = clip(f.Path, traceMaxPath)
		if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.HasPrefix(f.Path, "~") || f.Path == ".." || strings.HasPrefix(f.Path, "../") || strings.Contains(f.Path, "/../") {
			continue
		}
		if f.Repo = clip(f.Repo, 200); strings.ContainsAny(f.Repo, " \t\n") || strings.Count(f.Repo, "/") < 1 {
			f.Repo = ""
		}
		f.Nav = min(max(f.Nav, 0), traceMaxCount)
		if files = append(files, f); len(files) == traceMaxFiles {
			break
		}
	}
	r.Files = files
	return true
}

// applyTraces stores the traces of one upload. A trace whose task can't be
// found, or the uploader can't read, is dropped and counted. issues collects
// the ids of tasks whose rows were inserted or changed.
func (t *tx) applyTraces(ctx context.Context, uid int64, machine string, traces []UsageTrace, maxMS, now int64, res *UsageIngestResult, issues *[]int64) error {
	ids := map[string]int64{}
	for _, r := range traces {
		if !cleanTrace(&r, maxMS) {
			res.TracesDropped++
			continue
		}
		ref := strings.ToUpper(r.Key)
		id, known := ids[ref]
		if !known {
			if i, _, err := t.s.getIssueFor(ctx, t, t.actor, ref, RoleViewer, false); err == nil {
				id = i.ID
			}
			ids[ref] = id
		}
		if id == 0 {
			res.TracesDropped++
			continue
		}
		files, _ := json.Marshal(r.Files)
		if len(r.Files) == 0 {
			files = []byte("[]")
		}
		var oldID, oldLast, oldCalls, oldPre, oldEdits, oldFirst sql.NullInt64
		var oldEnded, oldFiles string
		err := t.QueryRowContext(ctx, `SELECT issue_id, last_at, calls, ended, pre_calls, edits, coalesce(first_edit_at, 0), files
			FROM task_traces WHERE user_id = ? AND machine = ? AND session = ? AND seq = ?`,
			uid, machine, r.Session, r.Seq).Scan(&oldID, &oldLast, &oldCalls, &oldEnded, &oldPre, &oldEdits, &oldFirst, &oldFiles)
		switch {
		case err == nil:
			first := int64(0)
			if r.FirstEditAt != nil {
				first = *r.FirstEditAt
			}
			if r.Last < oldLast.Int64 || oldID.Int64 == id && r.Last == oldLast.Int64 && r.Calls == oldCalls.Int64 && r.Ended == oldEnded &&
				r.PreCalls == oldPre.Int64 && r.Edits == oldEdits.Int64 && first == oldFirst.Int64 && string(files) == oldFiles {
				continue
			}
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, model, calls, locate, infile, reads, edits,
				delegated, other, pre_calls, pre_locate, pre_infile, pre_reads, pre_delegated, first_edit_at, ctx_start, ctx_first_edit, compacted, files, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine, session, seq) DO UPDATE SET issue_id = excluded.issue_id, started_at = excluded.started_at, last_at = excluded.last_at,
				ended = excluded.ended, model = excluded.model, calls = excluded.calls, locate = excluded.locate, infile = excluded.infile, reads = excluded.reads,
				edits = excluded.edits, delegated = excluded.delegated, other = excluded.other, pre_calls = excluded.pre_calls, pre_locate = excluded.pre_locate,
				pre_infile = excluded.pre_infile, pre_reads = excluded.pre_reads, pre_delegated = excluded.pre_delegated, first_edit_at = excluded.first_edit_at,
				ctx_start = excluded.ctx_start, ctx_first_edit = excluded.ctx_first_edit, compacted = excluded.compacted, files = excluded.files,
				updated_at = excluded.updated_at`,
			uid, machine, r.Session, r.Seq, id, r.Started, r.Last, r.Ended, r.Model, r.Calls, r.Locate, r.Infile, r.Reads, r.Edits,
			r.Delegated, r.Other, r.PreCalls, r.PreLocate, r.PreInfile, r.PreReads, r.PreDelegated, r.FirstEditAt, r.CtxStart, r.CtxFirstEdit,
			b2i(r.Compacted), string(files), now); err != nil {
			return err
		}
		res.Traces++
		for _, v := range []int64{id, oldID.Int64} {
			if v > 0 && !slices.Contains(*issues, v) {
				*issues = append(*issues, v)
			}
		}
	}
	return nil
}

// taskTracesLanded runs after an upload's traces are stored, with the tasks
// whose traces were inserted or changed. The prep policy hooks in here (WP-250).
func (s *Service) taskTracesLanded(ctx context.Context, issueIDs []int64) {
	s.prepPolicyTouch(ctx, issueIDs)
	if s.tracesLanded != nil {
		s.tracesLanded(issueIDs)
	}
}

// taskTraces is a task's traces, oldest first, whoever's they are: for the
// server's own use, with no access check.
func (s *Service) taskTraces(ctx context.Context, issueID int64) ([]*TaskTrace, error) {
	q := `SELECT t.id, t.user_id, coalesce(u.username, ''), t.machine, t.session, t.seq, t.issue_id, t.started_at, t.last_at, t.ended, t.model,
			t.calls, t.locate, t.infile, t.reads, t.edits, t.delegated, t.other, t.pre_calls, t.pre_locate, t.pre_infile, t.pre_reads, t.pre_delegated,
			t.first_edit_at, t.ctx_start, t.ctx_first_edit, t.compacted, t.files, t.updated_at
		FROM task_traces t LEFT JOIN users u ON u.id = t.user_id WHERE t.issue_id = ?`
	args := []any{issueID}
	rows, err := s.db.R.QueryContext(ctx, q+` ORDER BY t.started_at, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*TaskTrace
	for rows.Next() {
		var r TaskTrace
		var first, ctxFirst sql.NullInt64
		var compacted int
		var files string
		if err := rows.Scan(&r.ID, &r.UserID, &r.User, &r.Machine, &r.Session, &r.Seq, &r.IssueID, &r.Started, &r.Last, &r.Ended, &r.Model,
			&r.Calls, &r.Locate, &r.Infile, &r.Reads, &r.Edits, &r.Delegated, &r.Other, &r.PreCalls, &r.PreLocate, &r.PreInfile, &r.PreReads, &r.PreDelegated,
			&first, &r.CtxStart, &ctxFirst, &compacted, &files, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if first.Valid {
			r.FirstEditAt = &first.Int64
		}
		if ctxFirst.Valid {
			r.CtxFirstEdit = &ctxFirst.Int64
		}
		r.Compacted = compacted != 0
		r.Files = []TraceFile{}
		_ = json.Unmarshal([]byte(files), &r.Files)
		out = append(out, &r)
	}
	return out, rows.Err()
}
