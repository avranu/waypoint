package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// reactionNames are the emoji the forges take on a comment (GitHub's names).
var reactionNames = []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

// ResolveInput names review comments to resolve.
type ResolveInput struct {
	IDs  []string `json:"ids"`
	Note string   `json:"note"` // what was done: posted as a reply on the comment
}

// ListReviewComments reads a pull request's review comments from the forge
// and lists them all, dealt with or not.
func (s *Service) ListReviewComments(ctx context.Context, actor *Actor, prID int64) ([]*PRReviewComment, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	if _, err := s.syncReviewComments(ctx, pr); err != nil {
		s.log.WarnContext(ctx, "read review comments", "pr", pr.ID, "err", err)
	}
	return s.reviewComments(ctx, s.db.R, pr.ID)
}

// ReplyToReviewComment answers a comment on the forge (in its thread where
// it has one) as Waypoint's bot, and records who asked. It changes no
// verdict.
func (s *Service) ReplyToReviewComment(ctx context.Context, actor *Actor, ref, body string) (*PRReviewComment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, invalid("say something (a reply)")
	}
	list, prs, err := s.loadComments(ctx, actor, []string{ref}, false)
	if err != nil {
		return nil, err
	}
	rc, pr := list[0], prs[list[0].prID]
	if rc.Kind == "finding" {
		return nil, invalid("%s sits in the review's body and has no thread to reply in: resolve or dismiss it with a note", rc.Ref)
	}
	if err := s.postReply(ctx, pr, rc, body, rcByActor(actor), s.roundOf(ctx, pr.ID)); err != nil {
		return nil, err
	}
	return s.reviewComment(ctx, s.db.R, rc.id)
}

// postReply answers rc on the forge and records it; an error when the forge
// refused (the comment is then not marked answered).
func (s *Service) postReply(ctx context.Context, pr *PullRequest, rc *PRReviewComment, body string, by rcBy, round int) error {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if rc.Kind != "inline" {
		body = replyLead(rc, "") + body
	}
	if rc.relayID != "" {
		body = relayMarker(rc.relayID, "reply", "") + body
	}
	id := s.replyToComment(ctx, f, botForge(f), pr, rc, body)
	if id == "" {
		return invalid("the forge didn't take the reply on %s", rc.Ref)
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		var user any
		if by.userID != 0 {
			user = by.userID
		}
		if _, err := t.ExecContext(ctx, `UPDATE review_comments SET reply_id = ? WHERE id = ?`, id, rc.id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO review_comment_events(comment_id, pr_id, old_verdict, verdict, replied, by_kind, by_name, user_id, round, note, created_at)
			VALUES (?,?,?,'',1,?,?,?,?,?,?)`, rc.id, pr.ID, rc.Verdict, by.kind, by.name, user, round, excerpt(body, 2000), s.nowMS()); err != nil {
			return err
		}
		projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, New: "reply", Data: map[string]any{"pr_id": pr.ID, "comment": rc.Ref}})
		return nil
	})
}

// ReactToReviewComment puts an emoji on a comment, as Waypoint's bot.
func (s *Service) ReactToReviewComment(ctx context.Context, actor *Actor, ref, emoji string) (*PRReviewComment, error) {
	emoji = strings.TrimSpace(emoji)
	emoji = cmp(map[string]string{"👍": "+1", "👎": "-1", "😄": "laugh", "🎉": "hooray", "😕": "confused", "❤️": "heart", "🚀": "rocket", "👀": "eyes",
		"thumbsup": "+1", "thumbsdown": "-1"}[emoji], emoji)
	if !slices.Contains(reactionNames, emoji) {
		return nil, invalid("emoji must be one of %s", strings.Join(reactionNames, " "))
	}
	list, prs, err := s.loadComments(ctx, actor, []string{ref}, false)
	if err != nil {
		return nil, err
	}
	rc, pr := list[0], prs[list[0].prID]
	nt, ok := s.Forges.(ForgeReviewNotes)
	if !ok || rc.Kind == "finding" || rc.Kind == "relayed" {
		return nil, invalid("%s can't take an emoji", rc.Ref)
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(rc.Reactions, emoji) {
		if err := nt.ReactTo(ctx, botForge(f), pr.Repo, pr.Number, rc.Kind, rc.extID, emoji); err != nil {
			if errors.Is(err, ErrUnsupported) {
				return nil, invalid("the forge takes no emoji on a %s", kindName(rc.Kind))
			}
			return nil, err
		}
		rc.Reactions = append(rc.Reactions, emoji)
		b, _ := json.Marshal(rc.Reactions)
		if _, err := s.db.W.ExecContext(ctx, `UPDATE review_comments SET reactions = ? WHERE id = ?`, string(b), rc.id); err != nil {
			return nil, err
		}
	}
	return rc, nil
}

// ResolveReviewComments resolves review comments (`wp pr resolve`): the
// reply (the note), the thread resolved on the forge where it lets Waypoint,
// and always the verdict in Waypoint; a Waypoint finding it settles stops
// blocking the review gate.
func (s *Service) ResolveReviewComments(ctx context.Context, actor *Actor, in ResolveInput) ([]*PRReviewComment, error) {
	in.Note = strings.TrimSpace(in.Note)
	list, prs, err := s.commentsFor(ctx, actor, in.IDs)
	if err != nil {
		return nil, err
	}
	if actor.IsAgent() && in.Note == "" {
		for _, rc := range list {
			if !s.agentReplied(ctx, rc.id) {
				return nil, invalid("say what you did about %s first: reply (wp pr reply) or resolve with a note (-m); a resolved comment is closed for the person who wrote it", rc.Ref)
			}
		}
	}
	by := rcByActor(actor)
	for _, rc := range list {
		pr := prs[rc.prID]
		if rc.Kind == "finding" && in.Note == "" {
			return nil, invalid("%s is one of the review's findings: say why with a note", rc.Ref)
		}
		if err := s.markComment(ctx, pr, rc, VerdictResolved, in.Note, "", by, s.roundOf(ctx, pr.ID)); err != nil {
			return nil, err
		}
	}
	s.afterCommentChange(ctx, prs)
	return list, nil
}

// UnresolveReviewComments takes a resolution (or a dismissal) back: the
// comment is owed again, and its finding blocks the gate again.
func (s *Service) UnresolveReviewComments(ctx context.Context, actor *Actor, ids []string) ([]*PRReviewComment, error) {
	list, prs, err := s.commentsFor(ctx, actor, ids)
	if err != nil {
		return nil, err
	}
	by := rcByActor(actor)
	for _, rc := range list {
		pr := prs[rc.prID]
		if rc.Resolved && rc.Kind == "inline" && rc.threadID != "" {
			if th, ok := s.Forges.(ForgeThreads); ok {
				f, err := s.forgeByID(ctx, pr.ForgeID)
				if err != nil {
					return nil, err
				}
				err = th.UnresolveThread(ctx, botForge(f), pr.Repo, &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA}, ReviewThread{ID: rc.threadID, Path: rc.Path, Line: rc.Line})
				if err != nil {
					return nil, invalid("%s's thread is resolved on the forge and it can't be reopened from here (%v): reopen it there", rc.Ref, err)
				}
			}
		}
		if rc.Verdict == VerdictNew && !rc.Resolved {
			continue
		}
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			if err := s.recordVerdict(ctx, t, rc.id, pr.ID, VerdictNew, "", "", "", by, s.roundOf(ctx, pr.ID), "reopened"); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE review_comments SET resolved = 0 WHERE id = ?`, rc.id); err != nil {
				return err
			}
			if rc.FindingID != 0 && (rc.findingState == FindingResolved || rc.findingState == FindingDismissed) {
				fb := findingBy(by)
				if fb.kind == "babysitter" {
					fb.kind = "system"
				}
				if _, err := s.setFindingState(ctx, t, rc.FindingID, FindingOpen, fb, pr.HeadSHA, "reopened"); err != nil {
					return err
				}
			}
			rc.Verdict, rc.Resolved = VerdictNew, false
			projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
			if err != nil {
				return err
			}
			t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, New: "reopened", Data: map[string]any{"pr_id": pr.ID, "comment": rc.Ref}})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	s.afterCommentChange(ctx, prs)
	return list, nil
}

// afterCommentChange has the review gate and the babysit see what changed.
func (s *Service) afterCommentChange(ctx context.Context, prs map[int64]*PullRequest) {
	s.nudgeBabysits(prs)
	for id := range prs {
		s.goBG(func() { s.checkPR(context.WithoutCancel(ctx), id, "comment") })
	}
}

// agentReplied says whether an agent has answered a comment on the forge.
func (s *Service) agentReplied(ctx context.Context, id int64) bool {
	var n int
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM review_comment_events WHERE comment_id = ? AND replied = 1 AND by_kind = 'agent'`, id).Scan(&n)
	return n > 0
}
