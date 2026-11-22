package main

import (
	"testing"
	"time"
)

func TestYieldingJobsTheLongestWaiterOnly(t *testing.T) {
	now := time.Now()
	p := &runnerProc{jobs: map[int64]*activeJob{}}
	short := &activeJob{id: 1, lane: laneHeavy, slotWaitSince: now.Add(-2 * time.Minute)}
	long := &activeJob{id: 2, lane: laneHeavy, slotWaitSince: now.Add(-9 * time.Minute)}
	p.jobs[1], p.jobs[2] = short, long
	y := p.yieldingJobs(now)
	if len(y) != 1 || !y[long] {
		t.Fatalf("yielding %v, want only the one waiting 9m", y)
	}
}

func TestYieldingJobsLightLaneNever(t *testing.T) {
	now := time.Now()
	p := &runnerProc{jobs: map[int64]*activeJob{}}
	p.jobs[1] = &activeJob{id: 1, lane: laneLight, slotWaitSince: now.Add(-time.Hour)}
	if y := p.yieldingJobs(now); len(y) != 0 {
		t.Fatalf("a light job yields: %v", y)
	}
	if h, l, _, _, _ := p.running(); h != 0 || l != 1 {
		t.Errorf("running counts heavy %d light %d, want 0 and 1", h, l)
	}
}

func TestYieldingJobsThreshold(t *testing.T) {
	now := time.Now()
	p := &runnerProc{jobs: map[int64]*activeJob{}}
	j := &activeJob{id: 1, lane: laneHeavy, slotWaitSince: now.Add(-slotWaitYield)}
	p.jobs[1] = j
	if y := p.yieldingJobs(now); !y[j] {
		t.Error("a job waiting exactly slotWaitYield doesn't yield")
	}
	j.slotWaitSince = now.Add(-slotWaitYield + time.Millisecond)
	if y := p.yieldingJobs(now); len(y) != 0 {
		t.Errorf("a job a millisecond short yields: %v", y)
	}
}

func TestRunningCountsAJobHeavyAgainOnceItsWaitEnds(t *testing.T) {
	p := &runnerProc{jobs: map[int64]*activeJob{}}
	j := heavyJob(1, 5*time.Minute)
	p.jobs[1] = j
	if h, _, _, _, _ := p.running(); h != 0 {
		t.Fatalf("a long waiter counts %d heavy", h)
	}
	p.mu.Lock()
	j.slotWaitSince = time.Time{}
	p.mu.Unlock()
	if h, _, _, _, _ := p.running(); h != 1 {
		t.Errorf("after the wait ended: %d heavy, want 1", h)
	}
}
