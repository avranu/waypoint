package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
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
	TypedKickoff  = "kickoff"   // the prompt autopilot gives a session it starts
)

// typedSlack is how long after its job's own timeout a typed prompt may
// still arrive: the hook reports it when Claude Code takes it, and a runner
// waits for a busy session before it types (runnerTimeouts).
const typedSlack = 5 * time.Minute

// typedKickoffTTL is how long a kickoff the runner reports stays good: the
// autopilot job's timeout (runnerTimeouts) and its slack.
const typedKickoffTTL = 15*time.Minute + typedSlack

// ---- normalisation ----

var (
	pasteOpen  = regexp.MustCompile(`^<pasted_content(?:\s[^>]*)?>`)
	pasteTags  = regexp.MustCompile(`<pasted_content(?:\s[^>]*)?>|</pasted_content>`)
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

// unwrapPasted: text is one <pasted_content> element from its first byte to
// its last, and inner is what is inside, trimmed. A wrapper that closes
// before the text ends, or never closes, isn't one round the whole text.
func unwrapPasted(text string) (inner string, ok bool) {
	open := pasteOpen.FindString(text)
	if open == "" || !strings.HasSuffix(text, "</pasted_content>") {
		return "", false
	}
	depth := 0
	for _, loc := range pasteTags.FindAllStringIndex(text, -1) {
		if text[loc[0]+1] == '/' {
			depth--
		} else {
			depth++
		}
		if depth == 0 && loc[1] != len(text) {
			return "", false // closed before the end
		}
		if depth < 0 {
			return "", false
		}
	}
	if depth != 0 {
		return "", false
	}
	return strings.TrimSpace(text[len(open) : len(text)-len("</pasted_content>")]), true
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
		}
	case JobAutopilot:
		var a AutopilotJob
		if json.Unmarshal(input, &a) != nil || a.ReviewOf == "" {
			return nil // a restart's prompt is made on the runner: AutopilotTyped
		}
		cli = a.CLIID
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
	sum := ev.PromptSum
	if sum == "" {
		sum = PromptSum(ev.Prompt)
	}
	if sum == "" {
		return "", nil
	}
	at := min(s.hookTime(ev), s.nowMS())
	err = t.QueryRowContext(ctx, `UPDATE typed_prompts SET consumed_at = ? WHERE consumed_at IS NULL AND id = (
		SELECT id FROM typed_prompts WHERE session_id = ? AND hash = ? AND consumed_at IS NULL AND expires_at > ? ORDER BY id LIMIT 1) RETURNING kind`,
		s.nowMS(), sessionID, sum, at).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		if n := NormalisePrompt(ev.Prompt); strings.HasPrefix(n, AutopilotPromptMark) {
			s.log.WarnContext(ctx, "a prompt with Waypoint's mark matches nothing Waypoint typed: read as the person's",
				"session", sessionID, "kind_guess", markKindGuess(n), "length", len(n), "raw_length", len(ev.Prompt),
				"sum", sum, "client_sum", ev.PromptSum, "text_sum", PromptSum(ev.Prompt))
		}
		return "", nil
	}
	return kind, err
}

// markKindGuess says what a mark-carrying prompt claims to be, for the log.
func markKindGuess(n string) string {
	if strings.HasPrefix(n, AutopilotReviewMark) {
		return TypedReview
	}
	return TypedKickoff
}

// sweepTypedPrompts removes records that expired or were used long enough
// ago to be of no use.
func (s *Service) sweepTypedPrompts(ctx context.Context) {
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM typed_prompts WHERE expires_at <= ? OR consumed_at <= ?`, now, now-typedSlack.Milliseconds()); err != nil {
		s.log.WarnContext(ctx, "typed prompts sweep", "err", err)
	}
}

// typedAuto: automation's prompt, which a person's turn follows (a review
// or kickoff), as against a ping, compaction or message, which keep whoever
// was in charge.
func typedAuto(kind string) bool { return kind == TypedReview || kind == TypedKickoff }
