package core

import "time"

// What a review job's runner waits for, in the order it waits (cmd/wp
// runner_autopilot.go reads these, so the server's limits and the runner's
// steps are one definition, WP-1370): a busy session to go idle, /compact
// to show as busy and to finish, and the pasted prompt to start a turn.
const (
	ReviewIdleWait     = 3 * time.Minute
	ReviewCompactStart = 10 * time.Second
	ReviewCompactWait  = 6 * time.Minute
	ReviewBusyWait     = 15 * time.Second
	// reviewRunnerGrace is what the server allows beyond the runner's own
	// worst case: the poll that claims the job and the finish that reports it.
	reviewRunnerGrace = 90 * time.Second
	// reviewPromptWorst is the longest review prompt the paste settle is
	// reckoned for: a prompt carries every finding of an audit (WP-769).
	reviewPromptWorst = 256 * 1024
	// reviewJobTries is how many times a review job is queued for a round
	// before the round fails: the job the server cut for sitting too long
	// is asked for again, and a finished audit isn't lost with it.
	reviewJobTries = 3
)

// ReviewPasteSettle is how long the runner lets the session take in a
// pasted review prompt of n bytes before Enter.
func ReviewPasteSettle(n int) time.Duration {
	return 400*time.Millisecond + time.Duration(n/1024)*10*time.Millisecond
}

// reviewJobRunMax is how long a review job may run from the moment a runner
// claimed it: the runner's steps at their worst, and a grace.
func reviewJobRunMax() time.Duration {
	return ReviewIdleWait + ReviewCompactStart + ReviewCompactWait + ReviewBusyWait + ReviewPasteSettle(reviewPromptWorst) + reviewRunnerGrace
}
