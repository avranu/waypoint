//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// objRepo is a git repository on disk the runner reads as a clone URL.
type objRepo struct {
	t   *testing.T
	dir string
}

func newObjRepo(t *testing.T) *objRepo {
	t.Helper()
	r := &objRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *objRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the files and commits; it answers the commit's id.
func (r *objRepo) commit(files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

func (r *objRepo) job(j core.ObjectiveJob) core.ObjectiveJob {
	j.Key, j.Round, j.Repo, j.Ref = "WP-1", 1, r.dir, "main"
	if j.Budget == 0 {
		j.Budget = 20
	}
	return j
}

func readObjective(t *testing.T, p *runnerProc, run *jobRun, j core.ObjectiveJob) (outcome, *core.ObjectiveJobResult) {
	t.Helper()
	o := p.execObjective(context.Background(), run, j)
	if o.State != "done" {
		return o, nil
	}
	raw, err := json.Marshal(o.Result)
	if err != nil {
		t.Fatal(err)
	}
	var res core.ObjectiveJobResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return o, &res
}

// leftover is what the runner's work folder still holds: nothing, once a
// job is over.
func leftover(p *runnerProc) []string {
	ents, _ := os.ReadDir(filepath.Join(p.cacheDir(), "work"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// The answer's shape is what ObjectiveJobResult.clean accepts: a full head,
// a reading iff there is a check, the output kept short. clean is
// unexported, so its conditions are checked here from outside.
func wantClean(t *testing.T, j core.ObjectiveJob, res *core.ObjectiveJobResult) {
	t.Helper()
	if why := res.Clean(&j); why != "" {
		t.Errorf("the server would refuse this answer: %s\n%+v", why, res)
	}
	if res.Reading != nil && len(res.Reading.Output) > 2000 {
		t.Errorf("output %d bytes", len(res.Reading.Output))
	}
}

func TestObjectiveJobPassIsAnAnswerEitherWay(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"ok": ""})
	p := newTestProc(t)

	j := r.job(core.ObjectiveJob{Check: "echo checking; test -f ok", Measure: core.MeasurePass})
	o, res := readObjective(t, p, newRun(time.Minute), j)
	if res == nil || !res.Reading.OK || res.Reading.Exit != 0 || res.Reading.Runs != 1 {
		t.Fatalf("%+v %+v", o, res)
	}
	wantClean(t, j, res)

	j.Check = "echo broke; test -f missing"
	o, res = readObjective(t, p, newRun(time.Minute), j)
	if o.State != "done" || res == nil {
		t.Fatalf("a failing check is an answer, not a failed job: %+v", o)
	}
	if res.Reading.OK || res.Reading.Exit == 0 || !strings.Contains(res.Reading.Output, "broke") {
		t.Errorf("reading %+v", res.Reading)
	}
	if l := leftover(p); len(l) != 0 {
		t.Errorf("checkouts left: %v", l)
	}
}

func TestObjectiveJobValueUsesTheUnit(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"x": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: `printf '\033[32m81.3%%\033[0m of 1204 statements\n'`, Measure: core.MeasureValue, Unit: "%"})
	_, res := readObjective(t, p, newRun(time.Minute), j)
	if res == nil || !res.Reading.OK || res.Reading.Value == nil || *res.Reading.Value != 81.3 {
		t.Fatalf("%+v", res)
	}
	j.Check, j.Unit = "echo took 10-20 ms", "ms"
	_, res = readObjective(t, p, newRun(time.Minute), j)
	if res == nil || res.Reading.Value == nil || *res.Reading.Value != 20 && *res.Reading.Value != 10 {
		t.Fatalf("10-20 ms: %+v", res)
	}
	j.Check, j.Unit = "echo nothing here", ""
	o, res := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "done" || res.Reading.OK || !strings.Contains(res.Reading.Output, "nothing here") {
		t.Errorf("no number: %+v %+v", o, res)
	}
}

func TestObjectiveJobTimeReadsTheBaseBesideTheHead(t *testing.T) {
	r := newObjRepo(t)
	base := r.commit(map[string]string{"d": "0.12\n"})
	head := r.commit(map[string]string{"d": "0.002\n"})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "test -f built && sleep $(cat d)", Setup: "touch built", Measure: core.MeasureTime, Base: base, Budget: 15})
	o, res := readObjective(t, p, newRun(2*time.Minute), j)
	if res == nil {
		t.Fatalf("%+v", o)
	}
	wantClean(t, j, res)
	if res.Head != head || res.Base != base {
		t.Errorf("head %s base %s", res.Head, res.Base)
	}
	if res.BaseReading == nil || !res.BaseReading.OK || !res.Reading.OK {
		t.Fatalf("readings %+v / %+v (setup runs in each checkout)", res.Reading, res.BaseReading)
	}
	if res.Reading.Value == nil || res.BaseReading.Value == nil || *res.Reading.Value >= *res.BaseReading.Value {
		t.Errorf("head %v should be faster than base %v", res.Reading.Value, res.BaseReading.Value)
	}
	if res.Reading.Runs < 3 || res.BaseReading.Runs != res.Reading.Runs || res.Reading.Low == nil || res.Reading.High == nil {
		t.Errorf("runs %d / %d, low %v high %v", res.Reading.Runs, res.BaseReading.Runs, res.Reading.Low, res.Reading.High)
	}
	if res.P == nil || *res.P <= 0 || *res.P > 0.2 {
		t.Errorf("p = %v for a 60x change", res.P)
	}
}

func TestObjectiveJobKeepPassingRunAtTheHead(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{KeepPassing: []string{"test -f a && echo fine", "echo broken; false"}})
	_, res := readObjective(t, p, newRun(time.Minute), j)
	if res == nil || len(res.Kept) != 2 {
		t.Fatalf("%+v", res)
	}
	wantClean(t, j, res)
	if res.Reading != nil {
		t.Errorf("no check, but a reading: %+v", res.Reading)
	}
	if !res.Kept[0].OK || !strings.Contains(res.Kept[0].Output, "fine") || res.Kept[1].OK || !strings.Contains(res.Kept[1].Output, "broken") {
		t.Errorf("kept %+v", res.Kept)
	}
}

func TestObjectiveJobKeepPassingInAMissingFolderFails(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	// At the root, test -f a would pass: the folder must not fall back to it.
	j := r.job(core.ObjectiveJob{Dir: "gone", KeepPassing: []string{"test -f a"}})
	o, res := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || !strings.Contains(o.Error, "gone") || res != nil {
		t.Errorf("%+v %+v", o, res)
	}
}

func TestObjectiveJobTouchedNamesCheckFilesThatChanged(t *testing.T) {
	r := newObjRepo(t)
	base := r.commit(map[string]string{"bench/run.sh": "echo 1\n", "src/a.go": "a\n", "t_test.go": "x\n"})
	r.commit(map[string]string{"bench/run.sh": "echo 2\n", "src/a.go": "b\n"})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "sh bench/run.sh", Measure: core.MeasureValue, Base: base, KeepPassing: []string{"go test ./... -run t_test.go"}})
	_, res := readObjective(t, p, newRun(time.Minute), j)
	if res == nil {
		t.Fatal("no answer")
	}
	if len(res.Touched) != 1 || res.Touched[0] != "bench/run.sh" {
		t.Errorf("touched %v: only the changed file the commands name", res.Touched)
	}
}

func TestObjectiveJobSetupFailingFailsTheJobPlainly(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "true", Setup: "echo no toolchain >&2; exit 4", Measure: core.MeasurePass})
	o, _ := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || !strings.Contains(o.Error, "setup") || !strings.Contains(o.Error, "no toolchain") {
		t.Errorf("%+v", o)
	}
	if l := leftover(p); len(l) != 0 {
		t.Errorf("checkouts left: %v", l)
	}
}

func TestObjectiveJobHungCommandIsKilledAtTheTimeout(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	pid := filepath.Join(t.TempDir(), "pid")
	j := r.job(core.ObjectiveJob{Check: "sleep 300 & echo $! > " + pid + "; wait", Measure: core.MeasurePass, Budget: 1})
	start := time.Now()
	o, _ := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || !strings.Contains(o.Error, "longer than") {
		t.Fatalf("%+v", o)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	raw, err := os.ReadFile(pid)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for deadline := time.Now().Add(8 * time.Second); alive(child); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Error("the check's child outlived the job: the process group wasn't killed")
			break
		}
	}
	if l := leftover(p); len(l) != 0 {
		t.Errorf("checkouts left: %v", l)
	}
}

func TestObjectiveJobRefGoneFailsWithAMessage(t *testing.T) {
	r := newObjRepo(t)
	base := r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "true", Measure: core.MeasurePass})
	j.Ref = "deleted-branch"
	o, _ := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || !strings.Contains(o.Error, "deleted-branch") {
		t.Errorf("branch gone: %+v", o)
	}
	j = r.job(core.ObjectiveJob{Check: "true", Measure: core.MeasureValue, Base: strings.Repeat("0", 40)})
	_ = base
	o, _ = readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || !strings.Contains(o.Error, "started from") {
		t.Errorf("base gone: %+v", o)
	}
}

func TestObjectiveJobOutputIsCapped(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "yes 0123456789abcdef | head -c 3000000; echo 'the end'; false", Measure: core.MeasurePass})
	run := newRun(time.Minute)
	_, res := readObjective(t, p, run, j)
	if res == nil || res.Reading.OK || !strings.Contains(res.Reading.Output, "the end") {
		t.Fatalf("%+v", res)
	}
	if len(res.Reading.Output) > 2000 {
		t.Errorf("answer carries %d bytes", len(res.Reading.Output))
	}
	if n := len(run.out.text()); n > 1<<20 {
		t.Errorf("the job's log holds %d bytes of the check's output", n)
	}
	wantClean(t, j, res)
}

func TestObjectiveFeatureIsAdvertised(t *testing.T) {
	if !objectiveFeature() {
		t.Skip("no git or sh here")
	}
	found := false
	for _, f := range runnerFeatureList() {
		found = found || f == core.FeatureObjective
	}
	if !found {
		t.Error("a runner that can read an objective doesn't say so")
	}
}

// A time that can't be run three times in its budget fails the job, with the
// run's length, how many fit and the ways out: never a reading from too few
// runs.
func TestObjectiveJobTimeTooSlowToRepeatFails(t *testing.T) {
	r := newObjRepo(t)
	base := r.commit(map[string]string{"a": "1"})
	r.commit(map[string]string{"a": "2"})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "sleep 1.2", Measure: core.MeasureTime, Base: base, Budget: 3})
	o, res := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || res != nil {
		t.Fatalf("a time from too few runs was answered: %+v", o)
	}
	for _, want := range []string{"too slow to time", "one run took", "timed run(s) fit", "needs 3", "pass or value", "faster"} {
		if !strings.Contains(o.Error, want) {
			t.Errorf("the message lacks %q:\n%s", want, o.Error)
		}
	}
	if l := leftover(p); len(l) != 0 {
		t.Errorf("checkouts left: %v", l)
	}
	j.Measure, j.Check = core.MeasureValue, "sleep 1.2; echo 5"
	if o, res := readObjective(t, p, newRun(time.Minute), j); res == nil || res.Reading.Value == nil {
		t.Errorf("a value of the slow check: %+v", o)
	}
}

// One run longer than the whole budget is a stop that names the way out.
func TestObjectiveJobSingleRunOverBudgetFails(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	j := r.job(core.ObjectiveJob{Check: "sleep 30", Measure: core.MeasureTime, Budget: 1})
	o, res := readObjective(t, p, newRun(time.Minute), j)
	if o.State != "failed" || res != nil || !strings.Contains(o.Error, "longer than 1s") || !strings.Contains(o.Error, "pass or value") {
		t.Errorf("%+v", o)
	}
}

// With no check, the setup still runs before the keep-passing commands,
// and a setup that fails stops the reading instead of letting them answer
// for a tree that was never built.
func TestObjectiveJobSetupRunsForKeepPassingOnly(t *testing.T) {
	r := newObjRepo(t)
	r.commit(map[string]string{"a": ""})
	p := newTestProc(t)
	o, res := readObjective(t, p, newRun(time.Minute), r.job(core.ObjectiveJob{Setup: "touch built", KeepPassing: []string{"test -f built"}}))
	if o.State != "done" || res == nil || len(res.Kept) != 1 || !res.Kept[0].OK {
		t.Fatalf("%+v %+v", o, res)
	}
	o, _ = readObjective(t, p, newRun(time.Minute), r.job(core.ObjectiveJob{Setup: "echo no build >&2; exit 3", KeepPassing: []string{"true"}}))
	if o.State != "failed" || !strings.Contains(o.Error, "setup") || !strings.Contains(o.Error, "no build") {
		t.Fatalf("%+v", o)
	}
}

// A change list git can't produce is an error, never an empty list: empty
// reads as "the round left its own check alone".
func TestObjectiveTouchedFailsWhenTheListCantBeRead(t *testing.T) {
	p := newTestProc(t)
	sha := strings.Repeat("a", 40)
	got, err := p.objectiveTouched(context.Background(), filepath.Join(t.TempDir(), "no-repo"), sha, strings.Repeat("b", 40), core.ObjectiveJob{Check: "sh run.sh"})
	if err == nil {
		t.Fatalf("no error, list %v", got)
	}
}

// The folder is compared as git names it, however it was written: a round
// that changes bench/run.sh has changed the check of an objective in
// "./bench", "bench/" or "bench//".
func TestObjectiveTouchedCleansTheFolder(t *testing.T) {
	r := newObjRepo(t)
	base := r.commit(map[string]string{"bench/run.sh": "echo 1\n", "other.txt": "a"})
	head := r.commit(map[string]string{"bench/run.sh": "echo 2\n", "other.txt": "b"})
	p := newTestProc(t)
	for _, dir := range []string{"bench", "./bench", "bench/", "./bench//", "bench/."} {
		got, err := p.objectiveTouched(context.Background(), r.dir, base, head, core.ObjectiveJob{Dir: dir, Check: "sh run.sh"})
		if err != nil || len(got) != 1 || got[0] != "bench/run.sh" {
			t.Errorf("folder %q: %v, %v", dir, got, err)
		}
	}
	if got, err := p.objectiveTouched(context.Background(), r.dir, base, head, core.ObjectiveJob{Dir: ".", Check: "cat other.txt"}); err != nil || len(got) != 1 || got[0] != "other.txt" {
		t.Errorf("the root: %v, %v", got, err)
	}
}
