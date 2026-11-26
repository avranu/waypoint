package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var askKinds = opts(RequestQuestion, "A question", RequestAction, "Something only they can do")

// autoRequestKey names the request a step made, so a step run twice finds it.
func (c *StepCtx) autoRequestKey(slot int) string {
	return fmt.Sprintf("auto:%d:%s:%d:%d:%d", c.Run.ID, c.ID, c.Iteration, c.Attempt, slot)
}

func (s *Service) requestByAutoKey(ctx context.Context, key string) (int64, error) {
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? AND tool_use_id LIKE 'auto:%'`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// asker is the actor a request is made as: the person it is for, marked as
// the automation's (requests belong to whoever they are made as).
func (c *StepCtx) asker(userID int64) (*Actor, error) {
	u, err := c.S.userByID(c.Ctx, c.S.db.R, userID)
	if err != nil || u.Disabled {
		return nil, invalid("that person can't be asked")
	}
	return &Actor{User: u, Automation: c.Automation, Agent: "automation/" + c.Automation.Name, Depth: c.Actor.Depth, Budget: c.Actor.Budget}, nil
}

// makeRequest creates the request of an ask_person or create_request step
// (or finds the one an earlier pass made) and leaves its outputs.
func (c *StepCtx) makeRequest(slot int) (id int64, err error) {
	key := c.autoRequestKey(slot)
	if id, err = c.S.requestByAutoKey(c.Ctx, key); err != nil || id != 0 {
		if err == nil {
			c.setRequestOutputs(id)
		}
		return id, err
	}
	who := c.Str("user")
	ids := c.S.actionUserIDs(c.Ctx, c.data.Issue, c.Actor, who, c.stepProject())
	if len(ids) == 0 {
		return 0, invalid("no one to ask: %q isn't a person on this project", who)
	}
	title, err := c.Render("title")
	if err != nil {
		return 0, err
	}
	body, err := c.Render("body")
	if err != nil {
		return 0, err
	}
	in := RequestInput{Kind: cmp(c.Str("kind"), RequestQuestion), Title: strings.TrimSpace(title), Body: body, ToolUseID: key}
	if pid := c.stepProject(); pid != 0 {
		if p, perr := c.S.projectByID(c.Ctx, c.S.db.R, pid); perr == nil {
			in.Project = p.Key
		}
	}
	switch in.Kind {
	case RequestQuestion:
		q := Question{Question: in.Title}
		for _, ch := range c.Strings("choices") {
			q.Choices = append(q.Choices, Choice{Label: strings.TrimSpace(ch)})
		}
		in.Questions = []Question{q}
	case RequestAction:
	default:
		return 0, invalid("a person can be asked a question or to do something")
	}
	who2, err := c.asker(ids[0])
	if err != nil {
		return 0, err
	}
	r, _, err := c.S.CreateRequest(c.Ctx, who2, in)
	if err != nil {
		return 0, err
	}
	c.setRequestOutputs(r.ID)
	c.Logf("asked %s: %s (%s)", userName(r.User), r.Title, r.Code)
	return r.ID, nil
}

// setRequestOutputs leaves the request's code and link for later steps.
func (c *StepCtx) setRequestOutputs(id int64) {
	var number int
	var key string
	if err := c.S.db.R.QueryRowContext(c.Ctx, `SELECT r.number, coalesce(p.key, '') FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.id = ?`, id).Scan(&number, &key); err != nil {
		return
	}
	ref := requestRef(key, number)
	c.Set("Request", fmt.Sprintf("R-%d", number))
	c.Set("RequestRef", ref)
	c.Set("RequestURL", c.S.BaseURL()+"/requests/"+ref)
}

func init() {
	askParams := func(wait bool) []ParamDef {
		ps := []ParamDef{
			{Name: "user", Label: "Who", Type: PUser, Required: true, Options: opts("owners", "The project's owners", "actor", "Whoever triggered it", "reporter", "The reporter", "assignees", "The assignees"),
				Help: "A username, or one of the groups (the first person is asked)."},
			{Name: "kind", Label: "Asks", Type: PSelect, Default: RequestQuestion, Options: askKinds},
			{Name: "title", Label: "The question", Type: PTemplate, Required: true, Help: "What they see in their inbox."},
			{Name: "body", Label: "Details", Type: PTemplate, Help: "More context; markdown works."},
			{Name: "choices", Label: "Choices", Type: PList, Help: "One per line; empty lets them type an answer.", When: &ParamWhen{Param: "kind", In: []string{RequestQuestion}}},
		}
		if wait {
			ps = append(ps,
				ParamDef{Name: "timeout", Label: "Give up after", Type: PDuration, Required: true, Default: "3d", Min: f64(60), Max: f64(30 * 24 * 3600)},
				ParamDef{Name: "on_timeout", Label: "If nobody answers", Type: PSelect, Default: "fail", Options: opts("fail", "Fail the step", "continue", "Carry on")})
		}
		return ps
	}
	registerAction(ActionDef{Name: "ask_person", Label: "Ask a person", Group: "People", Phrase: "ask {user} {title}", Long: true,
		Doc:    "Make a request for `user` (a question, or a step only they can do) and wait for their decision. Outputs Answer, Decision (answered, done, dismissed), Answered, TimedOut",
		Params: askParams(true),
		Outputs: []OutputDef{{Name: "Answer", Type: "text", Doc: "what they answered"}, {Name: "Decision", Type: "text", Doc: "answered, done or dismissed"},
			{Name: "Answered", Type: "bool", Doc: "they answered or did it, rather than dismissing"}, {Name: "TimedOut", Type: "bool"}, {Name: "Request", Type: "text", Doc: "its code, R-12"}},
		Run: askPerson,
		Describe: func(c *StepCtx) string {
			t, _ := c.Render("title")
			return fmt.Sprintf("Would ask %s: %s", c.Str("user"), excerpt(t, 200))
		}})
	registerAction(ActionDef{Name: "create_request", Label: "Make a request for a person", Group: "People", Phrase: "make a request for {user}: {title}",
		Doc:    "Make a request for `user` and carry on without waiting. Outputs Request (its code) and RequestURL",
		Params: askParams(false), Outputs: []OutputDef{{Name: "Request", Type: "text", Doc: "its code, R-12"}, {Name: "RequestURL", Type: "text"}},
		Run: func(c *StepCtx) error {
			_, err := c.makeRequest(0)
			return err
		}})
}

func askPerson(c *StepCtx) error {
	id, err := c.makeRequest(0)
	if err != nil {
		return err
	}
	timeout, err := c.Dur("timeout", 3*24*time.Hour)
	if err != nil {
		return err
	}
	read := func() (state, decision, answer string, err error) {
		err = c.S.db.R.QueryRowContext(c.Ctx, `SELECT state, decision, answer FROM requests WHERE id = ?`, id).Scan(&state, &decision, &answer)
		return
	}
	_, timedOut, err := c.WaitSlot(0, EvRequestUpdated, "request:"+itoa64(id), "decision!=", timeout, func() (*Event, bool) {
		state, _, _, err := read()
		if err != nil || state == RequestOpen {
			return nil, false
		}
		return &Event{Kind: EvRequestUpdated, Data: map[string]any{"request": id, "decision": state}}, true
	})
	if err != nil {
		return err
	}
	if timedOut {
		if who, aerr := c.asker(c.requestUser(id)); aerr == nil {
			if _, werr := c.S.WithdrawRequest(c.Ctx, who, itoa64(id), "Nobody answered in time."); werr != nil {
				c.Logf("couldn't withdraw the request: %v", werr)
			}
		}
		c.Set("TimedOut", true)
		c.Set("Answered", false)
		if c.Str("on_timeout") == "continue" {
			return nil
		}
		return fmt.Errorf("nobody answered within %s", timeout)
	}
	state, _, answer, err := read()
	if err != nil {
		return err
	}
	c.Set("TimedOut", false)
	c.Set("Decision", state)
	c.Set("Answer", answer)
	c.Set("Answered", state == RequestAnswered || state == RequestDone)
	return nil
}

func (c *StepCtx) requestUser(id int64) int64 {
	var u int64
	_ = c.S.db.R.QueryRowContext(c.Ctx, `SELECT user_id FROM requests WHERE id = ?`, id).Scan(&u)
	return u
}
