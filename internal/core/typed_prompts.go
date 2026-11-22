package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Who typed a prompt (WP-1370). Waypoint tells its own typed prompts from a
// person's by record, never by how a prompt begins: every text it has a
// runner type into a session is written to typed_prompts in the same write
// that hands it over, and a hook's prompt is Waypoint's iff its normalised
// text is one of those records, unexpired and not yet used.

// Kinds of text Waypoint types into a session.
const (
	TypedReview   = "review"    // an adversarial review's prompt
	TypedCompact  = "compact"   // /compact
	TypedKeepWarm = "keep_warm" // a keep-warm ping
	TypedMessage  = "message"   // a message body typed instead of posted to the inbox
	TypedInbox    = "inbox"     // a batch of decisions and messages posted to an idle session (cache_warm markPosted)
	TypedKickoff  = "kickoff"   // the prompt autopilot gives a session it starts
	TypedStart    = "start"     // the text a session was started with (argv), which a person may have composed
	TypedExit     = "exit"      // /exit, typed before a restart
)

// typedSlack is how long after its job's own timeout a typed prompt may
// still arrive: the hook reports it when Claude Code takes it, and a runner
// waits for a busy session before it types (runnerTimeouts).
const typedSlack = 5 * time.Minute

// typedKickoffTTL is how long a kickoff the runner reports stays good: the
// autopilot job's timeout (runnerTimeouts) and its slack.
const typedKickoffTTL = 15*time.Minute + typedSlack

// typedKinds is the one table of who owns the words of each kind of text
// Waypoint types: true carries a person's words (a message or decision a
// person wrote, or the text a session was started with, which a person may
// have composed), false is Waypoint's own. Core and the CLI both read it
// (TypedCarriesPerson, TypedWaypointAlone); a kind not in it is read as a
// person's, so a new kind has to be put here to count as Waypoint's.
var typedKinds = map[string]bool{
	TypedReview:   false,
	TypedCompact:  false,
	TypedKeepWarm: false,
	TypedKickoff:  false,
	TypedExit:     false,
	TypedMessage:  true,
	TypedInbox:    true,
	TypedStart:    true,
}

// TypedCarriesPerson: Waypoint typed it, but the words are a person's.
func TypedCarriesPerson(kind string) bool {
	carries, known := typedKinds[kind]
	return kind != "" && (carries || !known)
}

// TypedWaypointAlone: Waypoint typed it and no person's words are in it.
func TypedWaypointAlone(kind string) bool {
	carries, known := typedKinds[kind]
	return known && !carries
}

// ---- normalisation ----

var (
	pasteOpen  = regexp.MustCompile(`^<pasted_content(?:\s[^>]*)?>`)
	pasteEnd   = regexp.MustCompile(`</pasted_content(?:\s[^>]*)?>$`)
	ansiSeq    = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?|[ -/]*[0-~]?)")
	ansiEscape = "\x1b"
)

// NormalisePrompt is a prompt as Waypoint compares it, the one function both
// sides use (recording what is typed, and recognising what a hook reports),
// so they can't disagree: paste wrappers round the whole text (every layer)
// are removed, escape sequences (CSI, OSC, bracketed-paste markers), byte
// order marks and every other format character (Unicode Cf, zero-width
// ones among them) are dropped, invalid UTF-8 becomes a space, every run of
// whitespace becomes one space, and the ends are trimmed. Idempotent.
func NormalisePrompt(text string) string {
	// Removing an escape sequence or a format character can join what was
	// around it into a new one, so the pass repeats until it changes nothing:
	// that is what makes the result its own normal form.
	for {
		n := normalisePromptOnce(text)
		if n == text {
			return n
		}
		text = n
	}
}

func normalisePromptOnce(text string) string {
	if !utf8.ValidString(text) {
		var b strings.Builder
		for i := 0; i < len(text); {
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && size == 1 {
				b.WriteByte(' ')
			} else {
				b.WriteString(text[i : i+size])
			}
			i += size
		}
		text = b.String()
	}
	text = ansiSeq.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, ansiEscape, "")
	text = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	for {
		inner, ok := unwrapPasted(text)
		if !ok {
			return text
		}
		text = inner
	}
}

// PromptSum is the SHA-256 (hex) of a prompt's whole normalised text; "" for
// text that normalises to nothing.
func PromptSum(text string) string {
	n := NormalisePrompt(text)
	if n == "" {
		return ""
	}
	h := sha256.Sum256([]byte(n))
	return hex.EncodeToString(h[:])
}

// PromptSums are the digests a prompt may be recorded under: its own
// (PromptSum) first, then those of the text with its paste wrappers taken
// off layer by layer without checking that they balance. Waypoint records the
// text it types as it is, and Claude Code may wrap it in <pasted_content>;
// when the text itself holds an unbalanced tag, NormalisePrompt leaves the
// wrapper on (it is not one round the whole text), so the wrapped prompt's
// own digest is not the record's.
func PromptSums(text string) []string {
	n := NormalisePrompt(text)
	if n == "" {
		return nil
	}
	sum := func(x string) string { h := sha256.Sum256([]byte(x)); return hex.EncodeToString(h[:]) }
	out := []string{sum(n)}
	for cur := n; ; {
		open, end := pasteOpen.FindString(cur), pasteEnd.FindStringIndex(cur)
		if open == "" || end == nil || end[0] < len(open) {
			return out
		}
		cur = NormalisePrompt(cur[len(open):end[0]])
		if cur == "" {
			return out
		}
		if x := sum(cur); !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
}

// ---- records ----

// recordTyped notes that text is being handed to a runner to type into
// session, good until now+ttl. Call it in the write that hands it over.
func recordTyped(ctx context.Context, q queryer, sessionID int64, kind, text string, now int64, ttl time.Duration) error {
	sum := PromptSum(text)
	if sum == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT INTO typed_prompts(session_id, hash, kind, created_at, expires_at) VALUES (?,?,?,?,?)`,
		sessionID, sum, kind, now, now+ttl.Milliseconds())
	return err
}

// recordTypedUnbound is recordTyped for a session that doesn't exist yet: the
// text is held for the user, and the first prompt of one of their new
// sessions that is exactly this text binds it (recogniseTyped).
func recordTypedUnbound(ctx context.Context, q queryer, userID int64, kind, text string, now int64, ttl time.Duration) error {
	sum := PromptSum(text)
	if sum == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT INTO typed_prompts(session_id, user_id, hash, kind, created_at, expires_at) VALUES (NULL,?,?,?,?,?)`,
		userID, sum, kind, now, now+ttl.Milliseconds())
	return err
}

// RecordTypedStart records the text a session is about to be started with on
// the command line, for the actor's user, before the session exists (WP-1370).
func (s *Service) RecordTypedStart(ctx context.Context, a *Actor, text string) error {
	// The permission of its route, as the sibling session hooks ask for it.
	if err := a.need(PermSessionsReport); err != nil {
		return err
	}
	if PromptSum(text) == "" {
		return invalid("nothing to record")
	}
	return s.write(ctx, a, func(t *tx) error {
		return recordTypedUnbound(ctx, t, a.id(), TypedStart, text, s.nowMS(), typedKickoffTTL)
	})
}

// sessionByCLI is the id of a user's session by the CLI's own id (0: none).
func sessionByCLI(ctx context.Context, q queryer, userID int64, cliID string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ? ORDER BY id DESC LIMIT 1`, userID, cliID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// recordHandedOver records what a claimed runner job will type into its
// session, in the claim's own write: nothing is typed that isn't recorded.
// A job claimed again (a retry) records again.
func (s *Service) recordHandedOver(ctx context.Context, q queryer, kind string, input []byte, source string, timeoutSec int, ownerID int64) error {
	type typed struct{ kind, text string }
	var cli string
	var texts []typed
	switch kind {
	case JobMessage:
		var m MessageJob
		if json.Unmarshal(input, &m) != nil {
			return nil
		}
		cli = m.CLIID
		switch {
		case m.Compact:
			texts = append(texts, typed{TypedCompact, "/compact"})
		case m.Type:
			kind := TypedMessage
			if source == cacheWarmSource {
				kind = TypedKeepWarm
			}
			texts = append(texts, typed{kind, m.Body})
		default:
			// A batch posted to the inbox, which the session reads by its own
			// prompt: decisions and messages people wrote (wakeups.go), so it
			// carries a person's words. Claude Code may wrap what it shows
			// (a peer note's heading); only an exact body is recognised.
			texts = append(texts, typed{TypedInbox, m.Body})
		}
	case JobSession:
		// A session started with text Waypoint composed: the session has no
		// id yet. A kickoff the runner composes after the claim records itself.
		var j SessionJob
		if json.Unmarshal(input, &j) != nil || j.Prompt == "" {
			return nil
		}
		return recordTypedUnbound(ctx, q, ownerID, TypedStart, j.Prompt, s.nowMS(), time.Duration(timeoutSec)*time.Second+typedKickoffTTL)
	case JobAutopilot:
		var a AutopilotJob
		if json.Unmarshal(input, &a) != nil {
			return nil
		}
		cli = a.CLIID
		if a.ReviewOf == "" {
			// A restart types /exit into the old session; its kickoff is made
			// on the runner: AutopilotTyped.
			if cli == "" {
				return nil
			}
			sid, err := sessionByCLI(ctx, q, ownerID, cli)
			if err != nil || sid == 0 {
				return err
			}
			return recordTyped(ctx, q, sid, TypedExit, "/exit", s.nowMS(), time.Duration(timeoutSec)*time.Second+typedSlack)
		}
		// The runner compacts first unless the transcript says it needn't.
		texts = append(texts, typed{TypedCompact, "/compact"}, typed{TypedReview, a.Review})
	}
	if cli == "" || len(texts) == 0 {
		return nil
	}
	sid, err := sessionByCLI(ctx, q, ownerID, cli)
	if err != nil || sid == 0 {
		return err
	}
	timeout := time.Duration(timeoutSec)*time.Second + typedSlack
	now := s.nowMS()
	for _, x := range texts {
		if err := recordTyped(ctx, q, sid, x.kind, x.text, now, timeout); err != nil {
			return err
		}
	}
	return nil
}

// AutopilotTyped records a prompt the runner made itself (the kickoff, from
// wp next's answer) just before it types it into session cliID (WP-1370).
// The step must be waiting for this job, as AutopilotStepWanted asks.
func (s *Service) AutopilotTyped(ctx context.Context, a *Actor, step, job int64, cliID, text string) error {
	// Only a runner types for autopilot, as its route says (runners.host).
	if err := a.need(PermRunnersHost); err != nil {
		return err
	}
	return s.write(ctx, a, func(t *tx) error {
		var n int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE id = ? AND user_id = ? AND state = ? AND job_id = ?`, step, a.id(), StepStarting, job).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrForbidden
		}
		sid, err := sessionByCLI(ctx, t, a.id(), cliID)
		if err != nil {
			return err
		}
		if sid == 0 {
			return ErrNotFound
		}
		return recordTyped(ctx, t, sid, TypedKickoff, text, s.nowMS(), typedKickoffTTL)
	})
}

// recogniseTyped says whether the prompt a hook reports is one Waypoint
// typed into the session, and which kind. The prompt's sum is the hook's own
// (the client digests the whole prompt, so a --no-prompts hook needs to send
// no text) or the text's. The match uses its record up in the same write:
// UPDATE ... WHERE consumed_at IS NULL, so of two hooks for one record only
// one wins. Everything else is the person's. A prompt that starts with the
// autopilot mark but matches nothing is the person's too, and logged, so a
// Waypoint prompt wrongly turned away shows.
//
// Upgrade: a prompt the previous server typed just before this one started
// has no record, so it reads as the person's, once, and is logged if it
// carried the mark.
func (s *Service) recogniseTyped(ctx context.Context, t *tx, sessionID int64, ev *HookEvent) (kind string, err error) {
	var sums []string
	if ev.PromptSum != "" {
		sums = append(sums, ev.PromptSum)
	}
	for _, x := range PromptSums(ev.Prompt) {
		if !slices.Contains(sums, x) {
			sums = append(sums, x)
		}
	}
	sum := ""
	if len(sums) > 0 {
		sum = sums[0]
	}
	if len(sums) == 0 && ev.TypedID == 0 {
		return "", nil
	}
	at := min(s.hookTime(ev), s.nowMS())
	// The same invocation posted again (the CLI's outbox resends after a lost
	// answer) is answered as the first was: from the record that invocation
	// used up, or from the tombstone it left when it matched none, and it uses
	// no other. A resend with no key is a new prompt and takes the next record.
	if ev.Key != "" {
		// On the same text: a key is one invocation's, so it is no answer for
		// other text. (A hook that sent an id has no text to compare.)
		q, args := `SELECT kind FROM typed_prompts WHERE session_id = ? AND consumed_key = ?`, []any{sessionID, ev.Key}
		if len(sums) > 0 {
			q += ` AND hash IN (?` + strings.Repeat(`,?`, len(sums)-1) + `)`
			for _, x := range sums {
				args = append(args, x)
			}
		}
		err = t.QueryRowContext(ctx, q+` ORDER BY id LIMIT 1`, args...).Scan(&kind)
		if err == nil {
			return kind, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	err = sql.ErrNoRows
	if ev.TypedID != 0 {
		// The hook matched a record itself (a --no-prompts hook sends no text
		// or digest): the record is that one, and only if it is this
		// session's, or held for this user's session not yet started.
		err = t.QueryRowContext(ctx, `UPDATE typed_prompts SET consumed_at = ?, consumed_key = ?, session_id = ? WHERE consumed_at IS NULL AND id = ? AND expires_at > ? AND (
			session_id = ? OR (session_id IS NULL AND user_id = (SELECT user_id FROM agent_sessions WHERE id = ?)
			  AND NOT EXISTS (SELECT 1 FROM session_events WHERE session_id = ? AND kind = 'prompt'))) RETURNING kind`,
			s.nowMS(), ev.Key, sessionID, ev.TypedID, at, sessionID, sessionID, sessionID).Scan(&kind)
	}
	for _, sm := range sums {
		if !errors.Is(err, sql.ErrNoRows) {
			break
		}
		err = t.QueryRowContext(ctx, `UPDATE typed_prompts SET consumed_at = ?, consumed_key = ? WHERE consumed_at IS NULL AND id = (
		SELECT id FROM typed_prompts WHERE session_id = ? AND hash = ? AND consumed_at IS NULL AND expires_at > ? ORDER BY id LIMIT 1) RETURNING kind`,
			s.nowMS(), ev.Key, sessionID, sm, at).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			// A text held for a session not started yet: only the first prompt of
			// one of the same user's sessions can be it.
			err = t.QueryRowContext(ctx, `UPDATE typed_prompts SET consumed_at = ?, consumed_key = ?, session_id = ? WHERE consumed_at IS NULL AND id = (
			SELECT id FROM typed_prompts WHERE session_id IS NULL AND hash = ? AND consumed_at IS NULL AND expires_at > ?
			  AND user_id = (SELECT user_id FROM agent_sessions WHERE id = ?)
			  AND NOT EXISTS (SELECT 1 FROM session_events WHERE session_id = ? AND kind = 'prompt') ORDER BY id LIMIT 1) RETURNING kind`,
				s.nowMS(), ev.Key, sessionID, sm, at, sessionID, sessionID).Scan(&kind)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		n := NormalisePrompt(ev.Prompt)
		pending := 0
		_ = t.QueryRowContext(ctx, `SELECT count(*) FROM typed_prompts WHERE session_id = ? AND consumed_at IS NULL AND expires_at > ?`, sessionID, at).Scan(&pending)
		if strings.HasPrefix(n, AutopilotPromptMark) || pending > 0 {
			// Every prompt that matches nothing while Waypoint has text pending
			// for the session is logged, not only one with the mark: a Waypoint
			// prompt turned away for a changed byte would otherwise be silent.
			s.log.WarnContext(ctx, "a prompt with Waypoint's mark matches nothing Waypoint typed: read as the person's",
				"session", sessionID, "kind_guess", markKindGuess(n), "pending", pending, "typed_id", ev.TypedID, "length", len(n), "raw_length", len(ev.Prompt),
				"sum", sum, "client_sum", ev.PromptSum, "text_sum", PromptSum(ev.Prompt))
		}
		if ev.Key != "" {
			// So the same invocation, sent again, is the person's again even if
			// a record was recorded for that text meanwhile.
			now := s.nowMS()
			if _, err := t.ExecContext(ctx, `INSERT INTO typed_prompts(session_id, hash, kind, created_at, expires_at, consumed_at, consumed_key) VALUES (?,?,'',?,?,?,?)`,
				sessionID, sum, now, now, now, ev.Key); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	return kind, err
}

// PendingTyped is a record a hook may match itself: the digest to compare
// with core.PromptSum of what it was given, and the kind.
type PendingTyped struct {
	ID   int64  `json:"id"`
	Sum  string `json:"sum"`
	Kind string `json:"kind"`
}

// PendingTypedPrompts lists the unused, unexpired records the session's next
// prompt may be: its own, and the texts held for the user's sessions not
// started yet, the newest 200. A hook that sends no prompt text
// (--no-prompts) compares them locally and reports only the id it matched, or none.
func (s *Service) PendingTypedPrompts(ctx context.Context, a *Actor, cliID string) ([]PendingTyped, error) {
	if err := a.need(PermSessionsReport); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, hash, kind FROM typed_prompts
		WHERE consumed_at IS NULL AND expires_at > ? AND hash != '' AND (
		  session_id = (SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ? ORDER BY id DESC LIMIT 1)
		  OR (session_id IS NULL AND user_id = ?)) ORDER BY id DESC LIMIT 200`, s.nowMS(), a.id(), cliID, a.id())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingTyped{}
	for rows.Next() {
		var p PendingTyped
		if err := rows.Scan(&p.ID, &p.Sum, &p.Kind); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// markKindGuess says what a mark-carrying prompt claims to be, for the log.
func markKindGuess(n string) string {
	if strings.HasPrefix(n, AutopilotReviewMark) {
		return TypedReview
	}
	return TypedKickoff
}

// HookOutboxMaxAge is how long the CLI keeps a hook event it couldn't send
// to resend it. A record a prompt used up stays that long, so a resend of
// the event is still answered as the first was (recogniseTyped).
const HookOutboxMaxAge = 24 * time.Hour

// sweepTypedPrompts removes records that expired unused, and those used up
// longer ago than a hook's resend window.
func (s *Service) sweepTypedPrompts(ctx context.Context) {
	now := s.nowMS()
	if _, err := s.writeRows(ctx, `DELETE FROM typed_prompts WHERE (consumed_at IS NULL AND expires_at <= ?) OR consumed_at <= ?`, now, now-HookOutboxMaxAge.Milliseconds()); err != nil {
		s.log.WarnContext(ctx, "typed prompts sweep", "err", err)
	}
}

// typedAuto: automation's prompt, which a person's turn follows (a review
// or kickoff), as against a ping or compaction, which keep whoever was in
// charge.
func typedAuto(kind string) bool { return kind == TypedReview || kind == TypedKickoff }

// typedCarriesPerson: Waypoint typed it, but the words are a person's (a
// message relayed for someone). Who typed it and whether a person took over
// are separate answers (WP-1370): this kind is Waypoint's typing, and still
// a person's turn to whatever reads the session's timeline for takeover,
// ownership and activity, as a body typed into the pane was before records
// told who typed it. A review, compact, keep-warm or kickoff is nobody's turn.
func typedCarriesPerson(kind string) bool { return TypedCarriesPerson(kind) }

// typedByWaypointAlone: Waypoint typed it and no person's words are in it.
func typedByWaypointAlone(kind string) bool { return TypedWaypointAlone(kind) }

// IsKeepWarmPing: text is the whole keep-warm ping Waypoint sends, up to the
// whitespace a terminal changes. The mark alone is not enough: a person can
// start a prompt with it (WP-1370). The CLI's local guard uses this where it
// must treat the ping as no person's.
func IsKeepWarmPing(text string) bool { return PromptSum(text) == PromptSum(keepWarmBody) }

// KeepWarmPing is the ping's whole text (tests of the CLI's guard type it).
func KeepWarmPing() string { return keepWarmBody }
