package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// reviewWait bounds how long a step waits for its review: three attempts of
// up to 40 minutes, ten minutes apart, plus time in the queue.
const reviewWait = 4 * time.Hour

// reviewOptionsOf reads a review step's settings.
func reviewOptionsOf(act *Action, e *Event) reviewOptions {
	return reviewOptions{Models: act.Models, Focus: act.Focus, Force: act.Force || e.New == "full review", MaxFindings: act.MaxFindings,
		Reviewers: [][]ChainModel{extraChain(act.Extra["second_models"]), extraChain(act.Extra["third_models"])},
		Verifier:  extraChain(act.Extra["verify_models"]), NoVerify: anyBool(act.Extra["no_verify"]),
		MinSeverity: anyString(act.Extra["min_severity"]), SkipFiles: anyStrings(act.Extra["skip_files"])}
}

func checkReview(_ *Rule, a *Action) error {
	if v := anyString(a.Extra["min_severity"]); v != "" {
		if _, ok := parseSeverity(v); !ok {
			return invalid("min_severity is one of P0, P1, P2, P3")
		}
	}
	skip := anyStrings(a.Extra["skip_files"])
	if len(skip) > 50 {
		return invalid("skip_files can list at most 50 patterns")
	}
	if _, err := newGlobSet(skip); err != nil {
		return invalid("skip_files: %v", err)
	}
	return nil
}

func reviewStep(c *StepCtx) error {
	pr := c.Data().PR
	if pr == nil {
		return invalid("review needs a pull request: use it with a pull request trigger")
	}
	e := c.Event
	projectID := cmpID(c.Automation.ProjectID, e.ProjectID)
	var autoID *int64
	if c.Automation.ID > 0 {
		autoID = &c.Automation.ID
	}
	commentID, _ := e.Data["comment_id"].(string)
	enqueue := func() (int64, error) {
		return c.S.enqueueBotRun(c.Ctx, projectID, autoID, pr, "review", reviewOptionsOf(c.Step, e), e.New, commentID)
	}
	if !c.x.durable {
		_, err := enqueue()
		return err
	}
	const slot = 0
	var id int64
	if w, err := c.S.stepWait(c.Ctx, c.Run.ID, c.ID, c.Iteration, c.slotSeq(slot)); err != nil {
		return err
	} else if w != nil {
		id, _ = strconv.ParseInt(strings.TrimPrefix(w.key, "bot_run:"), 10, 64)
	}
	if id == 0 {
		var err error
		if id, err = enqueue(); err != nil {
			return err
		}
		c.Logf("queued review #%d", id)
	}
	c.Set("RunID", id)
	finished := func() (*Event, bool) {
		if r, err := c.S.botRun(c.Ctx, id); err == nil && r.Status != "queued" && r.Status != "running" {
			return &Event{Kind: EvBotRun, New: r.Status, Data: map[string]any{"bot_run": id}}, true
		}
		return nil, false
	}
	_, timedOut, err := c.WaitSlot(slot, EvBotRun, "bot_run:"+itoa64(id), "new!=queued,new!=running", reviewWait, finished)
	if err != nil {
		return err
	}
	if timedOut {
		return fmt.Errorf("the review (#%d) hadn't finished after %s", id, reviewWait)
	}
	return c.setReviewOutputs(id)
}

// setReviewOutputs leaves what a finished review found for later steps.
func (c *StepCtx) setReviewOutputs(id int64) error {
	r, err := c.S.botRun(c.Ctx, id)
	if err != nil {
		return err
	}
	c.Set("Status", r.Status)
	c.Set("Summary", r.Summary)
	c.Set("URL", r.ReviewURL)
	c.Set("Reviewed", r.Status == "done")
	switch r.Status {
	case "failed":
		return fmt.Errorf("the review failed: %s", cmp(r.Error, "every model failed"))
	case "cancelled":
		return fmt.Errorf("the review was cancelled")
	}
	rows, err := c.S.db.R.QueryContext(c.Ctx, runFindingKindsSQL, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make([]int, len(severities))
	found, posted, worst := 0, 0, len(severities)
	for rows.Next() {
		var sev, placement string
		if err := rows.Scan(&sev, &placement); err != nil {
			return err
		}
		rank := severityRank(sev)
		counts[rank]++
		found++
		if placement != "withheld" && placement != placementLater {
			posted++
		}
		worst = min(worst, rank)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.Set("Findings", found)
	c.Set("Posted", posted)
	for n, k := range counts {
		c.Set(fmt.Sprintf("P%d", n), k)
	}
	if worst < len(severities) {
		c.Set("Worst", fmt.Sprintf("P%d", worst))
	} else {
		c.Set("Worst", "")
	}
	c.Logf("%s", r.Summary)
	return nil
}

// reviewOutputs are the review step's outputs. They are set when later
// steps follow it, which is when it waits for the review to end.
var reviewOutputs = []OutputDef{
	{Name: "Findings", Type: "number", Doc: "findings this review raised (after the verifier and the limits)"},
	{Name: "Posted", Type: "number", Doc: "of them, posted on the forge (the rest are on private files with no changed line, or kept past the number the review shows)"},
	{Name: "Worst", Type: "text", Doc: "the highest severity found: P0 (critical) to P3 (low); empty when it found nothing"},
	{Name: "P0", Type: "number", Doc: "critical findings"}, {Name: "P1", Type: "number", Doc: "high"},
	{Name: "P2", Type: "number", Doc: "medium"}, {Name: "P3", Type: "number", Doc: "low"},
	{Name: "Reviewed", Type: "bool", Doc: "a review was posted (false when it was skipped, say at a head already reviewed)"},
	{Name: "Status", Type: "text", Doc: "done or skipped"},
	{Name: "Summary", Type: "text"}, {Name: "URL", Type: "text", Doc: "the review on the forge"},
	{Name: "RunID", Type: "number", Doc: "the bot run"},
}

func init() {
	RegisterPreset(Preset{ID: "review_p1", Label: "The review found a P1 or worse", Template: `{{or .Steps.{step}.P0 .Steps.{step}.P1}}`, Step: true})
	RegisterPreset(Preset{ID: "review_any", Label: "The review found anything", Template: `{{.Steps.{step}.Findings}}`, Step: true})
	RegisterPreset(Preset{ID: "review_none", Label: "The review found nothing", Template: `{{and .Steps.{step}.Reviewed (not .Steps.{step}.Findings)}}`, Step: true})
}
