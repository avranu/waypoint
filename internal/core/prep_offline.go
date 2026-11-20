package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Scoring picks without showing them (WP-250, WP-ADR104). An arm's picks
// can be judged before any agent is given them, two ways:
//
//   - against the files the task's merged work changed (prep_scores), on
//     the same tasks as the chat prep's picks, and
//   - against what an agent that was shown no pointers opened and edited
//     (the none arm's navigation traces): what the picks would have named
//     of the files it went looking for.
//
// An arm other than the chat prep is drawn for agents only once it has been
// scored on prepOfflineMin tasks beside the chat prep, and not while it
// names credibly fewer of the changed files than the chat prep does on the
// same tasks. Agentic picks for that are made at surplus on tasks already
// done (a backtest, prep_agent.go): from the repository at the prep's
// commit and the task's text as it was, so they say what the arm would have
// picked, and no agent's task is spent finding out.

const (
	// prepOfflineMin: tasks scored beside the chat prep before an arm is
	// drawn for agents.
	prepOfflineMin = 20
	// prepOfflineTight and prepOfflineMax: backtests go on until the
	// difference in recall is known this closely (90% half-width), or on
	// this many tasks.
	prepOfflineTight = 0.05
	prepOfflineMax   = 150
	// prepOfflineWindow: scores older than this aren't counted.
	prepOfflineWindow = 180 * 24 * time.Hour
)

// PrepOfflineArm is an arm's picks scored without showing them.
type PrepOfflineArm struct {
	Arm string `json:"arm"`
	// Against the non-test, non-doc files the merged work changed.
	Tasks  int `json:"tasks"`
	Truth  int `json:"truth"`
	Hits   int `json:"hits"`
	Picked int `json:"picked"`
	// Against the chat prep on the same tasks: the mean difference in the
	// share of changed files named, with its 90% interval.
	Pairs int     `json:"pairs"`
	Diff  float64 `json:"diff"`
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
	// Against agents shown no pointers: the files they opened and edited
	// (tests and docs left out), how many of each the picks named, and how
	// many of the picks they opened.
	Unprompted  int `json:"unprompted"`
	Opened      int `json:"opened"`
	OpenedNamed int `json:"opened_named"`
	Edited      int `json:"edited"`
	EditedNamed int `json:"edited_named"`
	Named       int `json:"named"`
	NamedOpened int `json:"named_opened"`
	// Screen: why agents aren't given the arm yet, or "".
	Screen string `json:"screen,omitempty"`
}

// wants says whether more backtests of the arm would tell something.
func (o *PrepOfflineArm) wants() bool {
	return o.Pairs < prepOfflineMin || ((o.Hi-o.Lo)/2 > prepOfflineTight && o.Pairs < prepOfflineMax)
}

func (o *PrepOfflineArm) String() string {
	ratio := func(k, n int) string {
		if n == 0 {
			return "-"
		}
		return fmt.Sprintf("%.2f", float64(k)/float64(n))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "    %-6s %d tasks merged: names %s of the changed files, %s of its picks changed", o.Arm, o.Tasks, ratio(o.Hits, o.Truth), ratio(o.Hits, o.Picked))
	if o.Arm != prepArmChat {
		if o.Pairs > 0 {
			fmt.Fprintf(&b, "; against chat on %d of the same tasks %+.2f [%+.2f, %+.2f]", o.Pairs, o.Diff, o.Lo, o.Hi)
		} else {
			b.WriteString("; none beside a chat prep yet")
		}
	}
	b.WriteString("\n")
	if o.Unprompted > 0 {
		fmt.Fprintf(&b, "           %d tasks an agent did with no pointers: names %s of the files it opened, %s of those it edited; it opened %s of the picks\n",
			o.Unprompted, ratio(o.OpenedNamed, o.Opened), ratio(o.EditedNamed, o.Edited), ratio(o.NamedOpened, o.Named))
	}
	if o.Screen != "" {
		fmt.Fprintf(&b, "           not drawn: %s\n", o.Screen)
	}
	return b.String()
}

// prepScore is one task's picks against its merged work.
type prepScore struct{ picked, truth, hits int }

// prepScoresOf reads scores, one per task: its latest (a prep made again by
// another model is scored again).
// modelLike "" is any model.
func (s *Service) prepScoresOf(ctx context.Context, source, modelLike string) (map[int64]prepScore, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, picked, truth, hits FROM prep_scores
		WHERE truth > 0 AND scored_at >= ? AND source = ? AND (? = '' OR model LIKE ?) ORDER BY scored_at, id`,
		s.now().Add(-prepOfflineWindow).UnixMilli(), source, modelLike, modelLike)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]prepScore{}
	for rows.Next() {
		var id int64
		var p prepScore
		if err := rows.Scan(&id, &p.picked, &p.truth, &p.hits); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// prepOfflineScores is the arms' picks against merged work: the chat
// prep's, and the agentic prep's beside it.
func (s *Service) prepOfflineScores(ctx context.Context) (chat, agent *PrepOfflineArm, err error) {
	cs, err := s.prepScoresOf(ctx, "prep", "")
	if err != nil {
		return nil, nil, err
	}
	as, err := s.prepScoresOf(ctx, "trial", prepAgentModelPrefix+"%")
	if err != nil {
		return nil, nil, err
	}
	chat, agent = &PrepOfflineArm{Arm: prepArmChat}, &PrepOfflineArm{Arm: prepArmAgent}
	for _, p := range cs {
		chat.Tasks++
		chat.Truth, chat.Hits, chat.Picked = chat.Truth+p.truth, chat.Hits+p.hits, chat.Picked+p.picked
	}
	var diffs []float64
	for id, p := range as {
		agent.Tasks++
		agent.Truth, agent.Hits, agent.Picked = agent.Truth+p.truth, agent.Hits+p.hits, agent.Picked+p.picked
		if c, ok := cs[id]; ok {
			diffs = append(diffs, float64(p.hits-c.hits)/float64(p.truth))
		}
	}
	agent.Pairs = len(diffs)
	agent.Diff, agent.Lo, agent.Hi = meanInterval(diffs)
	switch {
	case agent.Pairs < prepOfflineMin:
		agent.Screen = fmt.Sprintf("scored beside the chat prep on %d of the %d tasks it needs before agents get it", agent.Pairs, prepOfflineMin)
	case agent.Hi < 0:
		agent.Screen = fmt.Sprintf("names fewer of the changed files than the chat prep on the same %d tasks: %+.2f [%+.2f, %+.2f]", agent.Pairs, agent.Diff, agent.Lo, agent.Hi)
	}
	return chat, agent, nil
}

// meanInterval is a mean and its 90% interval. With one value or none the
// interval is everything a difference of shares can be.
func meanInterval(v []float64) (mean, lo, hi float64) {
	n := float64(len(v))
	if n == 0 {
		return 0, -1, 1
	}
	for _, x := range v {
		mean += x
	}
	mean /= n
	if n < 2 {
		return mean, -1, 1
	}
	var ss float64
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	se := math.Sqrt(ss / (n - 1) / n)
	return mean, mean - 1.645*se, mean + 1.645*se
}

// prepOffline is every arm's offline scores. rows are the policy's tasks:
// those whose agent was shown nothing and left a trace are what the picks
// are held against.
func (s *Service) prepOffline(ctx context.Context, rows []*prepRow) ([]PrepOfflineArm, error) {
	chat, agent, err := s.prepOfflineScores(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.arm != prepArmNone || !r.traced || !r.closed {
			continue
		}
		var files []TraceFile
		if json.Unmarshal([]byte(r.files), &files) != nil {
			continue
		}
		var repo string
		_ = s.db.R.QueryRowContext(ctx, `SELECT repo FROM task_preps WHERE issue_id = ?`, r.issue).Scan(&repo)
		opened, edited := map[string]bool{}, map[string]bool{}
		for _, f := range files {
			if !mistakeFile(f.Path) || f.Created {
				continue // a file the agent made is nothing a pointer could name
			}
			key := f.Path
			if f.Repo != "" && repo != "" && !strings.EqualFold(f.Repo, repo) {
				// a file of a repository the prep didn't read: the agent
				// needed it and no pick can be it, whatever its path
				key = f.Repo + "\x00" + f.Path
			}
			opened[key] = true
			if f.Edited {
				edited[key] = true
			}
		}
		if len(opened) == 0 {
			continue
		}
		var notes string
		if err := s.db.R.QueryRowContext(ctx, `SELECT notes FROM task_preps WHERE issue_id = ? AND state = 'ready'`, r.issue).Scan(&notes); err == nil {
			chat.unprompted(notes, opened, edited)
		}
		if err := s.db.R.QueryRowContext(ctx, `SELECT t.notes FROM prep_trials t JOIN task_preps tp ON tp.issue_id = t.issue_id AND tp.text_hash = t.text_hash
			WHERE t.issue_id = ? AND t.kind = ? AND t.error = '' ORDER BY t.created_at DESC LIMIT 1`,
			r.issue, prepAgentKind).Scan(&notes); err == nil {
			agent.unprompted(notes, opened, edited)
		}
	}
	return []PrepOfflineArm{*chat, *agent}, nil
}

// unprompted adds one task an agent did with no pointers.
func (o *PrepOfflineArm) unprompted(notes string, opened, edited map[string]bool) {
	var n PrepNotes
	if json.Unmarshal([]byte(notes), &n) != nil {
		return
	}
	picks := map[string]bool{}
	for f := range prepPickFiles(&n) {
		if mistakeFile(f) {
			picks[f] = true
		}
	}
	if len(picks) == 0 {
		return
	}
	o.Unprompted++
	o.Opened, o.Edited, o.Named = o.Opened+len(opened), o.Edited+len(edited), o.Named+len(picks)
	for f := range picks {
		if opened[f] {
			o.NamedOpened++
			o.OpenedNamed++
		}
		if edited[f] {
			o.EditedNamed++
		}
	}
}
