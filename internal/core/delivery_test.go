package core

import (
	"testing"
	"time"
)

// Delivery puts recent work at its step on the way to production (WP-389):
// merges the deployed commit has are live, later ones wait for a deploy,
// open pull requests say what they wait on, and held tasks are being worked.
func TestDeliveryStages(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	lf := newLandForge(se)
	prod := "production"
	if _, err := se.s.CreateEnvironment(se.ctx, se.admin, se.p.Key, EnvironmentInput{Name: &prod}); err != nil {
		t.Fatal(err)
	}
	merge := func(n int, sha string) {
		at := se.clockNow()
		se.pullHook("closed", ForgePR{Number: n, Title: "Change", Author: "sam", BaseRef: "main", HeadRef: "b" + sha, HeadSHA: "head" + sha, State: "merged", MergeSHA: sha, MergedAt: &at})
		se.s.Wait()
		// What the merge became (the webhook here doesn't say).
		if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET merge_sha = ? WHERE number = ?`, sha, n); err != nil {
			t.Fatal(err)
		}
		se.setClock(se.clockNow().Add(10 * time.Minute))
	}
	merge(1, "aaaaaaa1")
	merge(2, "aaaaaaa2")
	merge(3, "aaaaaaa3")
	task := se.issue(se.admin, IssueInput{Project: se.p.Key, Title: "The feature"})
	if _, err := se.s.AddLink(se.ctx, se.admin, task.Key, se.pr(3).URL, ""); err != nil {
		t.Fatal(err)
	}
	se.pullHook("opened", ForgePR{Number: 4, Title: "Open", Author: "sam", BaseRef: "main", HeadRef: "open", HeadSHA: "bbbbbbb4", State: "open"})
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET ci_state = 'failed' WHERE number = 4`); err != nil {
		t.Fatal(err)
	}
	// Production runs #2's merge; #1's is in it.
	if _, err := se.s.RecordDeploymentNamed(se.ctx, se.admin, se.p.Key, prod, DeployInput{Commit: "aaaaaaa2", Branch: "main", Status: DeploySucceeded}); err != nil {
		t.Fatal(err)
	}
	lf.landed("aaaaaaa2", "aaaaaaa1")

	d, err := se.s.Delivery(se.ctx, se.admin, DeliveryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	stage := map[int]*DeliveryItem{}
	for _, it := range d.Items {
		if it.PR != nil {
			stage[it.PR.Number] = it
		}
	}
	for n, want := range map[int]string{1: StageLive, 2: StageLive, 3: StageMerged, 4: StageStuck} {
		if it := stage[n]; it == nil || it.Stage != want {
			t.Errorf("#%d: %+v, want %s", n, it, want)
		}
	}
	if it := stage[3]; it != nil && (len(it.Tasks) != 1 || it.Tasks[0].Key != task.Key || it.Detail != "Merged, not deployed to production yet.") {
		t.Errorf("#3 = %+v", it)
	}
	if it := stage[1]; it != nil && (len(it.Envs) != 1 || it.Envs[0].Since == nil) {
		t.Errorf("#1 runs = %+v", it.Envs)
	}
	if len(d.Environments) != 1 || d.Environments[0].Missing != 1 {
		t.Errorf("environments = %+v", d.Environments)
	}
	if d.Items[0].Stage != StageStuck {
		t.Errorf("first = %s, want what's stuck first", d.Items[0].Stage)
	}

	// Deployed: all three are live.
	if _, err := se.s.RecordDeploymentNamed(se.ctx, se.admin, se.p.Key, prod, DeployInput{Commit: "aaaaaaa3", Branch: "main", Status: DeploySucceeded}); err != nil {
		t.Fatal(err)
	}
	lf.landed("aaaaaaa3", "aaaaaaa1")
	lf.landed("aaaaaaa3", "aaaaaaa2")
	d, err = se.s.Delivery(se.ctx, se.admin, DeliveryQuery{Project: se.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range d.Items {
		if it.PR != nil && it.PR.State == "merged" && it.Stage != StageLive {
			t.Errorf("after deploy #%d: %s %q", it.PR.Number, it.Stage, it.Detail)
		}
	}
	if d.Environments[0].Missing != 0 {
		t.Errorf("missing after deploy = %d", d.Environments[0].Missing)
	}
}

// A merge with no recorded SHA falls back to its head, which a squash
// merge doesn't contain: that "no" is unknown, never missing.
func TestDeliveryNoMergeSHAIsUnknown(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	newLandForge(se)
	prod := "production"
	if _, err := se.s.CreateEnvironment(se.ctx, se.admin, se.p.Key, EnvironmentInput{Name: &prod}); err != nil {
		t.Fatal(err)
	}
	at := se.clockNow()
	se.pullHook("closed", ForgePR{Number: 1, Title: "Change", Author: "sam", BaseRef: "main", HeadRef: "b1", HeadSHA: "headaaaa1", State: "merged", MergedAt: &at})
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET merge_sha = '' WHERE number = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.RecordDeploymentNamed(se.ctx, se.admin, se.p.Key, prod, DeployInput{Commit: "ccccccc9", Branch: "main", Status: DeploySucceeded}); err != nil {
		t.Fatal(err)
	}
	d, err := se.s.Delivery(se.ctx, se.admin, DeliveryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Environments) != 1 || d.Environments[0].Unknown != 1 || d.Environments[0].Missing != 0 {
		t.Errorf("environments = %+v", d.Environments)
	}
}

// A negative cached for the merge SHA must still reach the head fallback:
// the older PR, found through the cache, is unknown rather than missing.
func TestDeliveryCachedNegativeUsesHeadFallback(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	newLandForge(se)
	prod := "production"
	if _, err := se.s.CreateEnvironment(se.ctx, se.admin, se.p.Key, EnvironmentInput{Name: &prod}); err != nil {
		t.Fatal(err)
	}
	at := se.clockNow()
	se.pullHook("closed", ForgePR{Number: 1, Title: "Change", Author: "sam", BaseRef: "main", HeadRef: "b1", HeadSHA: "headaaaa1", State: "merged", MergedAt: &at})
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET merge_sha = 'headaaaa1' WHERE number = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.RecordDeploymentNamed(se.ctx, se.admin, se.p.Key, prod, DeployInput{Commit: "ccccccc9", Branch: "main", Status: DeploySucceeded}); err != nil {
		t.Fatal(err)
	}
	d, err := se.s.Delivery(se.ctx, se.admin, DeliveryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if e := d.Environments[0]; e.Unknown != 0 || e.Missing != 1 {
		t.Fatalf("first: %+v", e)
	}
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET merge_sha = '' WHERE number = 1`); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Minute)
	se.pullHook("closed", ForgePR{Number: 2, Title: "Newer", Author: "sam", BaseRef: "main", HeadRef: "b2", HeadSHA: "headbbbb2", State: "merged", MergedAt: &later})
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET merge_sha = 'bbbbbbb2' WHERE number = 2`); err != nil {
		t.Fatal(err)
	}
	d, err = se.s.Delivery(se.ctx, se.admin, DeliveryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if e := d.Environments[0]; e.Unknown != 1 || e.Missing != 1 {
		t.Errorf("second: %+v", e)
	}
}
