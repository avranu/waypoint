package core

func init() {
	registerAction(ActionDef{Name: "babysit", Label: "Babysit the pull request", Group: "Pull request", Phrase: "babysit the pull request", Runner: true,
		Doc: "Start babysitting a pull request: Waypoint watches it until it merges, and each time its checks and reviews have settled with something left to do (a failed check, a conflict, a review comment not dealt with), a fresh Claude Code session on a runner of the person who last saved the automation deals with it. Starting one that is going does nothing. `model` is the rounds' Claude model (default sonnet). Outputs State",
		Params: []ParamDef{
			{Name: "model", Label: "Model", Type: PText, Advanced: true, Placeholder: babysitModel, Help: "The Claude model each round runs: sonnet (the default), opus or haiku (or a Claude model id of those). Fable and Mythos are refused: they are for planning."},
			prParam,
		},
		Outputs: []OutputDef{{Name: "State", Type: "text"}, {Name: "Number", Type: "number"}, {Name: "URL", Type: "text"}},
		Run:     babysitAction})
}

func babysitAction(c *StepCtx) error {
	pr, err := c.PullRequest("pr")
	if err != nil {
		return err
	}
	c.setPROutputs(pr)
	if pr.State == "merged" || pr.State == "closed" {
		c.Logf("the pull request is %s: nothing to babysit", pr.State)
		c.Set("State", "")
		return nil
	}
	if c.Automation == nil {
		return invalid("only an automation can babysit from a step")
	}
	b, err := c.S.StartBabysit(c.Ctx, c.Actor, pr.ID, BabysitInput{Model: c.Str("model"), Via: "automation"})
	if err != nil {
		return err
	}
	c.Logf("babysitting %s#%d (%s)", pr.Repo, pr.Number, b.State)
	c.Set("State", b.State)
	return nil
}
