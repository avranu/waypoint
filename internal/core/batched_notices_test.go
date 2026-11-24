package core

import (
	"strings"
	"testing"
	"time"
)

// twoBabysits: the agent shipped #7 and #8 and stopped; its runner on m1
// can post to its inbox socket.
func twoBabysits(t *testing.T) (*bsEnv, [2]*PullRequest, [2]*Babysit) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	pr7 := be.open()
	p8 := bsPR
	p8.Number, p8.HeadRef, p8.HeadSHA = 8, "add-import", "8888888bbbb"
	be.pullHook("opened", p8)
	be.s.Wait()
	be.drain()
	pr8 := be.pr(8)
	b7, b8 := be.start(pr7), be.start(pr8)
	be.hookAs("Stop")
	be.s.Wait()
	return be, [2]*PullRequest{pr7, pr8}, [2]*Babysit{b7, b8}
}

func (be *bsEnv) messageJobs() []string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT input FROM runner_jobs WHERE source IN ('session_message', 'session_batch') ORDER BY id`)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestBatchedNewsGoesInOneWakeUp(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	bs[0].Blocking, bs[1].Blocking = true, true
	be.setClock(be.clockNow().Add(10 * time.Minute))
	be.runner(be.admin, "m1", nil)
	be.s.babysitTell(be.ctx, bs[0], prs[0], "gave_up", "Waypoint gave up babysitting o/r#7: it failed.")
	if jobs := be.messageJobs(); len(jobs) != 0 {
		t.Fatalf("woke the agent while #8 still moves: %v", jobs)
	}
	be.s.CheckBabysits(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 0 {
		t.Fatalf("released early: %v", jobs)
	}
	be.s.babysitTell(be.ctx, bs[1], prs[1], "gave_up", "Waypoint gave up babysitting o/r#8: it failed too.")
	jobs := be.messageJobs()
	if len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") || !strings.Contains(jobs[0], "o/r#8") {
		t.Fatalf("want one post with both: %v", jobs)
	}
}

func TestBatchGoesWhenDue(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	bs[0].Blocking = true
	be.setClock(be.clockNow().Add(10 * time.Minute))
	be.s.babysitTell(be.ctx, bs[0], prs[0], "gave_up", "Waypoint gave up babysitting o/r#7.")
	be.setClock(be.clockNow().Add(batchBlockingWait + time.Second))
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") {
		t.Fatalf("a due batch: %v", jobs)
	}
}

func TestBatchKeepsAWarmCacheWarm(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	be.setClock(be.clockNow().Add(50 * time.Minute))
	b := bs[0]
	b.Notify = true
	be.s.babysitTell(be.ctx, b, prs[0], "merged", "o/r#7 merged.")
	var until int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT batch_until FROM session_messages WHERE step_key = ?`, "babysit:"+itoa64(b.ID)+":merged").Scan(&until); err != nil {
		t.Fatal(err)
	}
	if got, want := msTime(until).Sub(be.clockNow()), cacheWarm-cacheMargin-50*time.Minute; got != want {
		t.Fatalf("batch_until in %v, want %v: before the cache cools", got, want)
	}
}

func TestNewsThatCanWaitWaitsADayOnAColdCache(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	bs[0].Notify, bs[1].Notify = true, true
	be.s.babysitTell(be.ctx, bs[0], prs[0], "merged", "o/r#7 merged.")
	be.s.babysitTell(be.ctx, bs[1], prs[1], "merged", "o/r#8 merged.")
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	be.s.Wait() // background refreshes read the clock; let them finish before it moves
	if jobs := be.messageJobs(); len(jobs) != 0 {
		t.Fatalf("woke the agent for news it needn't see yet: %v", jobs)
	}
	be.setClock(be.clockNow().Add(batchColdWait + time.Second))
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") || !strings.Contains(jobs[0], "o/r#8") {
		t.Fatalf("want one post with both after a day: %v", jobs)
	}
}

func TestNewsThatCanWaitDoesNotContinueAStop(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	bs[0].Notify = true
	be.s.babysitTell(be.ctx, bs[0], prs[0], "merged", "o/r#7 merged.")
	if res := be.hookAs("Stop"); len(res.Messages) != 0 {
		t.Fatalf("continued a stop with news that can wait: %q", res.Text)
	}
	be.s.queueMessage(be.ctx, be.agent.Session, "do the next thing", "test:next", messageFrom{}, false)
	if res := be.hookAs("Stop"); len(res.Messages) != 2 || !strings.Contains(res.Text, "o/r#7 merged") {
		t.Fatalf("want it along with work: %d %q", len(res.Messages), res.Text)
	}
}

func TestBlockingNewsTakesWaitingNewsAlong(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	bs[0].Notify, bs[1].Blocking = true, true
	be.s.babysitTell(be.ctx, bs[0], prs[0], "merged", "o/r#7 merged.")
	be.runner(be.admin, "m1", nil)
	be.s.babysitTell(be.ctx, bs[1], prs[1], "gave_up", "Waypoint gave up babysitting o/r#8.")
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") || !strings.Contains(jobs[0], "o/r#8") {
		t.Fatalf("want one post with both: %v", jobs)
	}
}

func TestStatusAndHooksTakeBatchedNews(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	be.setClock(be.clockNow().Add(10 * time.Minute))
	be.s.babysitTell(be.ctx, bs[0], prs[0], "gave_up", "Waypoint gave up babysitting o/r#7.")
	v, err := be.s.BabysitStatus(be.ctx, be.agent, prs[1].ID)
	if err != nil || !strings.Contains(v.Batched, "o/r#7") {
		t.Fatalf("status = %+v %v", v, err)
	}
	if v, _ := be.s.BabysitStatus(be.ctx, be.agent, prs[1].ID); v.Batched != "" {
		t.Errorf("handed over twice: %q", v.Batched)
	}

	be.s.babysitTell(be.ctx, bs[1], prs[1], "closed", "o/r#8 was closed without merging.")
	be.s.queueMessage(be.ctx, be.agent.Session, "do the next thing", "test:next", messageFrom{}, false)
	res := be.hookAs("Stop")
	if len(res.Messages) != 2 || !strings.Contains(res.Text, "o/r#8 was closed") {
		t.Fatalf("stop took %d: %q", len(res.Messages), res.Text)
	}
}

// News that needs the agent (CI gave up, a person is needed) isn't urgent
// unless the agent said its other work waits on the pull request.
func TestNewsThatNeedsTheAgentCanWait(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	be.setClock(be.clockNow().Add(10 * time.Minute))
	be.runner(be.admin, "m1", nil)
	be.s.babysitTell(be.ctx, bs[0], prs[0], "gave_up", "Waypoint gave up babysitting o/r#7.")
	be.s.babysitTell(be.ctx, bs[1], prs[1], "gave_up", "Waypoint gave up babysitting o/r#8.")
	be.setClock(be.clockNow().Add(batchBlockingWait + time.Second))
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 0 {
		t.Fatalf("woke the agent for news that can wait: %v", jobs)
	}
	be.setClock(be.clockNow().Add(cacheWarm))
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") || !strings.Contains(jobs[0], "o/r#8") {
		t.Fatalf("want one post with both before the cache cools: %v", jobs)
	}
	be.s.babysitTell(be.ctx, bs[0], prs[0], "needs_person@x", "o/r#7 needs a person.")
	if res := be.hookAs("Stop"); !strings.Contains(res.Text, "o/r#7 needs a person") {
		t.Fatalf("a stop didn't take it: %q", res.Text)
	}
}

// --blocking: the merge itself goes within minutes, alone if nothing else
// moves.
func TestBlockingBabysitWakesAtOnce(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	pr := be.open()
	b, err := be.s.StartBabysit(be.ctx, be.agent, pr.ID, BabysitInput{Blocking: true})
	if err != nil || !b.Blocking || !b.Notify {
		t.Fatalf("start blocking = %+v %v", b, err)
	}
	be.hookAs("Stop")
	be.s.Wait()
	be.setClock(be.clockNow().Add(2 * time.Hour))
	be.runner(be.admin, "m1", nil)
	be.s.babysitTell(be.ctx, b, pr, "merged", "o/r#7 merged.")
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7 merged") {
		t.Fatalf("a blocking merge: %v", jobs)
	}
}
