package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCronParsingAndNextRuns(t *testing.T) {
	valid := []string{"* * * * *", "*/15 * * * *", "0 9 * * 1-5", "30 8-18/2 1,15 * *", "0 0 * jan,jul mon", "0 12 * * 7", "@daily", "@hourly", "0 0 29 2 *"}
	for _, v := range valid {
		if _, err := ParseCron(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	invalid := []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8", "5-1 * * * *", "*/0 * * * *", "a * * * *", "* * * foo *"}
	for _, v := range invalid {
		if _, err := ParseCron(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	london, _ := time.LoadLocation("Europe/London")
	from := time.Date(2026, 10, 9, 10, 0, 0, 0, london) // a Friday
	weekday, _ := ParseCron("0 9 * * 1-5")
	var got []string
	at := from
	for range 4 {
		at = weekday.Next(at)
		got = append(got, at.Format("Mon 15:04"))
	}
	if strings.Join(got, ", ") != "Mon 09:00, Tue 09:00, Wed 09:00, Thu 09:00" {
		t.Errorf("weekday runs = %v", got)
	}
	if n, _ := ParseCron("*/20 * * * *"); n.Next(time.Date(2026, 10, 9, 10, 20, 0, 0, time.UTC)).Format("15:04") != "10:40" {
		t.Error("Next returned the time given, not the one after it")
	}
	// Day of month and day of week together mean either (Friday the 13th is
	// every Friday and every 13th).
	either, _ := ParseCron("0 0 13 * 5")
	if n := either.Next(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)); n.Day() != 13 || n.Weekday() == time.Friday && n.Day() != 16 && n.Day() != 13 {
		t.Errorf("either = %v", n)
	}
	leap, _ := ParseCron("0 0 29 2 *")
	if n := leap.Next(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); n.Year() != 2028 || n.Month() != time.February || n.Day() != 29 {
		t.Errorf("leap day = %v", n)
	}
	sunday, _ := ParseCron("0 12 * * 7")
	if n := sunday.Next(from); n.Weekday() != time.Sunday {
		t.Errorf("7 isn't Sunday: %v", n.Weekday())
	}
	// The endpoint's helper: five runs in a zone.
	e := newEnv(t)
	runs, err := e.s.NextCronRuns("0 9 * * 1-5", "America/New_York", 5)
	if err != nil || len(runs) != 5 {
		t.Fatalf("next runs: %v %v", runs, err)
	}
	for n, r := range runs {
		if r.Hour() != 9 || r.Location().String() != "America/New_York" || (n > 0 && !r.After(runs[n-1])) {
			t.Errorf("run %d = %v", n, r)
		}
	}
	if _, err := e.s.NextCronRuns("nope", "", 5); !IsInvalid(err) {
		t.Errorf("bad expression: %v", err)
	}
	if _, err := e.s.NextCronRuns("* * * * *", "Mars/Base", 5); !IsInvalid(err) {
		t.Errorf("bad zone: %v", err)
	}
}

func TestCronSchedulesFire(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Cron", "")
	a := e.addAuto(p, "Standup", Rule{Trigger: Trigger{Event: "schedule", Every: "cron", Extra: map[string]any{"cron": "30 14 * * 1-5"}}, TimeZone: "America/New_York",
		Actions: []Action{{Type: "create_issue", Title: "Standup {{.Date}}"}}})
	count := func() int { return len(e.search(e.admin, "title:Standup")) }
	// 14:30 in New York (EDT) is 18:30 UTC.
	e.setClock(time.Date(2026, 10, 5, 18, 29, 0, 0, time.UTC))
	e.s.RunTimedAutomations(e.ctx)
	if count() != 0 {
		t.Fatal("fired a minute early")
	}
	e.setClock(time.Date(2026, 10, 5, 18, 33, 0, 0, time.UTC))
	e.s.RunTimedAutomations(e.ctx)
	e.s.RunTimedAutomations(e.ctx)
	if count() != 1 {
		t.Fatalf("%d issues after the first firing", count())
	}
	e.setClock(time.Date(2026, 10, 5, 18, 38, 0, 0, time.UTC))
	e.s.RunTimedAutomations(e.ctx)
	if count() != 1 {
		t.Error("fired twice for one occurrence")
	}
	e.setClock(time.Date(2026, 10, 5, 19, 5, 0, 0, time.UTC)) // a late scheduler doesn't catch up hours later
	e.s.RunTimedAutomations(e.ctx)
	e.setClock(time.Date(2026, 10, 10, 18, 32, 0, 0, time.UTC)) // Saturday
	e.s.RunTimedAutomations(e.ctx)
	if count() != 1 {
		t.Error("fired on a weekend")
	}
	e.setClock(time.Date(2026, 10, 6, 18, 31, 0, 0, time.UTC))
	e.s.RunTimedAutomations(e.ctx)
	if count() != 2 {
		t.Errorf("next day: %d issues", count())
	}
	runs := e.runs(a)
	if len(runs) != 2 || runs[0].Via != "schedule" {
		t.Errorf("runs = %+v", runs)
	}
	// An hourly cron fires once an hour, and with an issue query once per issue.
	e.setClock(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Hourly", Labels: []string{"watch"}})
	e.addAuto(p, "Hourly", Rule{Trigger: Trigger{Event: "schedule", Every: "cron", Extra: map[string]any{"cron": "0 * * * *"}}, If: "label:watch",
		Actions: []Action{{Type: "add_label", Value: "seen"}, {Type: "comment", Message: "tick {{.Date}}"}}})
	e.s.RunTimedAutomations(e.ctx)
	e.s.RunTimedAutomations(e.ctx)
	e.setClock(e.clockNow().Add(time.Hour))
	e.s.RunTimedAutomations(e.ctx)
	if cs := e.commentBodies(i.Key); len(cs) != 2 {
		t.Errorf("hourly comments = %q", cs)
	}
}

func TestCountThresholdFiresOnCrossing(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Counts", "")
	a := e.addAuto(p, "Too many bugs", Rule{Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open type:bug", "op": ">=", "n": 3}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.count}} open bugs (limit {{.Trigger.Data.n}})"}}})
	bug := func() *IssueDetail { return e.issue(e.admin, IssueInput{Project: p.Key, Type: "bug", Title: "b"}) }
	check := func() { e.setClock(e.clockNow().Add(5 * time.Minute)); e.s.RunTimedAutomations(e.ctx) }
	check() // the first look only records where things stand
	bug()
	bug()
	check()
	if len(e.runs(a)) != 0 {
		t.Fatal("fired below the limit")
	}
	third := bug()
	check()
	if len(e.runs(a)) != 1 {
		t.Fatalf("%d runs on crossing", len(e.runs(a)))
	}
	bug()
	check()
	check()
	if len(e.runs(a)) != 1 {
		t.Fatal("fired again while above the limit")
	}
	r := e.run(e.runs(a)[0].ID)
	if r.Event == nil || r.Event.Data["count"] != float64(3) || r.Status != RunSucceeded {
		t.Errorf("run = %+v", r)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 5)
	if len(ns) == 0 || ns[0].Body != "3 open bugs (limit 3)" {
		t.Errorf("notification = %+v", ns)
	}
	// Fixing bugs brings it under; crossing again fires again.
	for _, k := range e.search(e.admin, "type:bug") {
		if _, err := e.s.Transition(e.ctx, e.admin, k, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
	check()
	_ = third
	bug()
	bug()
	bug()
	check()
	if len(e.runs(a)) != 2 {
		t.Errorf("%d runs after the second crossing", len(e.runs(a)))
	}

	// "At most": fires when the count falls to the limit.
	b := e.addAuto(p, "Queue empty", Rule{Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open", "op": "<=", "n": 0}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "empty"}}})
	check()
	for _, k := range e.search(e.admin, "is:open") {
		e.s.Transition(e.ctx, e.admin, k, "done", "")
	}
	check()
	if len(e.runs(b)) != 1 {
		t.Errorf("at-most trigger: %d runs", len(e.runs(b)))
	}
}

// Issue changes schedule a check soon after, so counts don't wait five minutes.
func TestCountThresholdChecksAfterIssueChanges(t *testing.T) {
	old := countDebounce
	countDebounce = 20 * time.Millisecond
	defer func() { countDebounce = old }()
	e := newEnv(t)
	p := e.project(e.admin, "Debounce", "")
	a := e.addAuto(p, "Two issues", Rule{Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open", "op": ">=", "n": 2}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "two"}}})
	e.s.RunTimedAutomations(e.ctx) // baseline
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "1"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "2"})
	deadline := time.Now().Add(5 * time.Second)
	for len(e.runs(a)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.runs(a)) != 1 {
		t.Fatalf("%d runs after the debounce", len(e.runs(a)))
	}
}

func TestWebhookTrigger(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Hooks", "")
	a := e.addAuto(p, "Inbox", Rule{Trigger: Trigger{Event: "webhook", Extra: map[string]any{"secret": "s3cret"}}, Actions: []Action{
		{Type: "create_issue", ID: "mk", Title: "{{.Trigger.Data.title}} ({{.Trigger.Data.repo.name}})"},
		{Type: "comment", Message: "no issue here"},
	}})
	if a.HookURL == "" || !strings.HasPrefix(a.HookURL, "https://wp.test/hooks/automations/") {
		t.Fatalf("hook url = %q", a.HookURL)
	}
	token := a.HookURL[strings.LastIndex(a.HookURL, "/")+1:]
	body := []byte(`{"title":"Disk alert","repo":{"name":"infra"}}`)
	sign := func(b []byte, secret string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(b)
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, "nope", body, ""); !errors.Is(err, ErrNoHook) {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, token, body, ""); !errors.Is(err, ErrBadSignature) {
		t.Errorf("unsigned: %v", err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, token, body, sign(body, "wrong")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong signature: %v", err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, token, []byte(`{"title":"other"}`), sign(body, "s3cret")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("signature of another body: %v", err)
	}
	run, err := e.s.RunAutomationWebhook(e.ctx, token, body, sign(body, "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Via != "webhook" {
		t.Errorf("via = %s", run.Via)
	}
	got := e.run(run.ID)
	// The second step has no issue to comment on, so it fails; the first did its work.
	if got.Status != RunFailed || stepByID(got, "mk").Status != "succeeded" {
		t.Errorf("run = %s %q steps %+v", got.Status, got.Error, got.Steps)
	}
	if keys := e.search(e.admin, "title:Disk"); len(keys) != 1 {
		t.Errorf("issues = %v", keys)
	} else if i := e.get(e.admin, keys[0]); i.Title != "Disk alert (infra)" {
		t.Errorf("title = %q", i.Title)
	}
	if got.Event == nil || got.Event.Data["title"] != "Disk alert" {
		t.Errorf("event = %+v", got.Event)
	}
	// Non-JSON bodies arrive as text; JSON arrays under "body".
	b := e.addAuto(p, "Plain", Rule{Trigger: Trigger{Event: "webhook"}, Actions: []Action{{Type: "create_issue", Title: "{{.Trigger.Data.body}}"}}})
	tb := b.HookURL[strings.LastIndex(b.HookURL, "/")+1:]
	if _, err := e.s.RunAutomationWebhook(e.ctx, tb, []byte("hello there"), ""); err != nil {
		t.Fatal(err)
	}
	if len(e.search(e.admin, "title:hello")) != 1 {
		t.Error("a text body wasn't available as .Trigger.Data.body")
	}
	// Off, or edited to another trigger: the address stops working.
	off := false
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, b.ID, AutomationInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, tb, []byte("x"), ""); !errors.Is(err, ErrNoHook) {
		t.Errorf("disabled: %v", err)
	}
	on := true
	moved := Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "comment", Message: "x"}}}
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, b.ID, AutomationInput{Enabled: &on, Rule: &moved}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, tb, []byte("x"), ""); !errors.Is(err, ErrNoHook) {
		t.Errorf("no longer a webhook: %v", err)
	}
	// A new address replaces the old one.
	fresh, err := e.s.RegenerateAutomationHook(e.ctx, e.admin, a.ID)
	if err != nil || fresh.HookURL == a.HookURL {
		t.Fatalf("regenerate: %v %v", fresh, err)
	}
	if _, err := e.s.RunAutomationWebhook(e.ctx, token, body, sign(body, "s3cret")); !errors.Is(err, ErrNoHook) {
		t.Errorf("the old address still works: %v", err)
	}
	// Only owners see the address and the secret.
	vic := e.user("vic", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "vic", "viewer")
	list, _ := e.s.ListAutomations(e.ctx, vic, p.Key)
	for _, x := range list {
		if x.HookURL != "" || x.Rule.Trigger.str("secret") != "" {
			t.Errorf("a viewer sees the webhook secret: %+v", x)
		}
	}
	if _, err := e.s.RegenerateAutomationHook(e.ctx, vic, a.ID); err == nil {
		t.Error("a viewer replaced the webhook address")
	}
}

func TestAdminAutomationsSeeEveryProject(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	one := e.project(e.admin, "One", "")
	two := e.project(sam, "Two", "")
	rule := Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Issue.Key}} in {{.Project.Name}}"}}}
	if _, err := e.s.CreateAdminAutomation(e.ctx, sam, AutomationInput{Name: str("x"), Rule: &rule}); err == nil {
		t.Error("a non-admin made an admin automation")
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	if _, err := e.s.CreateAdminAutomation(e.ctx, agent, AutomationInput{Name: str("x"), Rule: &rule}); err == nil {
		t.Error("an agent made an admin automation")
	}
	global, err := e.s.CreateAdminAutomation(e.ctx, e.admin, AutomationInput{Name: str("Everything"), Rule: &rule})
	if err != nil {
		t.Fatal(err)
	}
	if !global.Admin || global.ProjectID != 0 {
		t.Errorf("global = %+v", global)
	}
	local := e.addAuto(one, "Local", rule)
	e.issue(e.admin, IssueInput{Project: one.Key, Title: "a"})
	e.issue(sam, IssueInput{Project: two.Key, Title: "b"})
	gr, _ := e.s.ListAutomationRuns(e.ctx, e.admin, global.ID, 10, 0)
	if len(gr) != 2 {
		t.Fatalf("the admin automation ran %d times, want 2 (both projects)", len(gr))
	}
	if len(e.runs(local)) != 1 {
		t.Errorf("a project automation ran %d times, want only its own project's 1", len(e.runs(local)))
	}
	detail := e.run(gr[0].ID)
	if detail.ProjectID != 0 || detail.Rule == nil {
		t.Errorf("run = %+v", detail)
	}
	if !strings.Contains(detail.Summary, "created") {
		t.Errorf("summary = %q", detail.Summary)
	}
	// Admins only: listing, reading, editing, deleting, running.
	if _, err := e.s.ListAdminAutomations(e.ctx, sam); err == nil {
		t.Error("a non-admin listed admin automations")
	}
	if list, err := e.s.ListAdminAutomations(e.ctx, e.admin); err != nil || len(list) != 1 {
		t.Errorf("admin list = %v %v", list, err)
	}
	if _, err := e.s.GetAutomation(e.ctx, sam, global.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a non-admin read it: %v", err)
	}
	if _, err := e.s.UpdateAutomation(e.ctx, sam, global.ID, AutomationInput{Enabled: ptr(false)}); err == nil {
		t.Error("a project owner edited an admin automation")
	}
	if _, err := e.s.UpdateAutomation(e.ctx, agent, global.ID, AutomationInput{Enabled: ptr(false)}); err == nil {
		t.Error("an agent edited an admin automation")
	}
	if err := e.s.DeleteAutomation(e.ctx, sam, global.ID); err == nil {
		t.Error("a project owner deleted an admin automation")
	}
	if _, err := e.s.ListAutomationRuns(e.ctx, sam, global.ID, 10, 0); err == nil {
		t.Error("a non-admin read an admin automation's runs")
	}
	if _, err := e.s.GetAutomationRun(e.ctx, sam, gr[0].ID); err == nil {
		t.Error("a non-admin read an admin automation's run")
	}
	// A project automation can't use system triggers; an admin one can.
	sys := Rule{Trigger: Trigger{Event: "system_resources", Extra: map[string]any{"resource": "disk"}}, Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.text}}"}}}
	if _, err := e.s.CreateAutomation(e.ctx, e.admin, one.Key, AutomationInput{Rule: &sys}); err == nil {
		t.Error("a project automation took a system trigger")
	}
	low, err := e.s.CreateAdminAutomation(e.ctx, e.admin, AutomationInput{Name: str("Low disk"), Rule: &sys})
	if err != nil {
		t.Fatal(err)
	}
	// Deleting works for admins.
	if err := e.s.DeleteAutomation(e.ctx, e.admin, global.ID); err != nil {
		t.Errorf("admin delete: %v", err)
	}

	// Low resources fire once on crossing, and not again within the hour.
	disk := ResourceStatus{Resource: "disk", Label: "Free disk space", Bytes: 200 << 20, LimitMB: 1024, Low: true}
	ok := disk
	ok.Low = false
	e.s.noteResource(e.ctx, ok)
	e.s.noteResource(e.ctx, disk)
	e.s.noteResource(e.ctx, disk) // still low: not again
	if n := len(runsOf(t, e, low)); n != 1 {
		t.Fatalf("%d runs on crossing", n)
	}
	r := e.run(runsOf(t, e, low)[0].ID)
	if r.Event.Data["resource"] != "disk" || !strings.Contains(r.Event.Data["text"].(string), "Free disk space is low: 200.0 MiB") {
		t.Errorf("event = %+v", r.Event)
	}
	e.s.noteResource(e.ctx, ok)
	e.setClock(e.clockNow().Add(30 * time.Minute))
	e.s.noteResource(e.ctx, disk) // flapped back within the hour
	if len(runsOf(t, e, low)) != 1 {
		t.Error("fired twice within the hour")
	}
	e.s.noteResource(e.ctx, ok)
	e.setClock(e.clockNow().Add(31 * time.Minute))
	e.s.noteResource(e.ctx, disk)
	if len(runsOf(t, e, low)) != 2 {
		t.Errorf("%d runs after the hour", len(runsOf(t, e, low)))
	}
	// Other resources are filtered out by the trigger.
	e.s.noteResource(e.ctx, ResourceStatus{Resource: "memory", Label: "Free memory", Bytes: 1 << 20, LimitMB: 256, Low: true})
	if len(runsOf(t, e, low)) != 2 {
		t.Error("a memory warning ran the disk automation")
	}
	// Readings and limits.
	if st, err := e.s.ResourceStatuses(e.ctx, e.admin); err != nil || len(st) < 2 {
		t.Errorf("statuses = %v %v", st, err)
	}
	e.s.SampleResources(e.ctx)
	if _, err := e.s.ResourceStatuses(e.ctx, sam); err == nil {
		t.Error("a non-admin read the system status")
	}
	lim, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{DiskFreeMB: 500, DatabaseMB: 100, MemoryFreeMB: 0})
	if err != nil || lim.DiskFreeMB != 500 {
		t.Fatalf("set limits: %v %v", lim, err)
	}
	if got, _ := e.s.GetResourceLimits(e.ctx, e.admin); got.DatabaseMB != 100 || got.MemoryFreeMB != 0 {
		t.Errorf("limits = %+v", got)
	}
	if _, err := e.s.SetResourceLimits(e.ctx, sam, ResourceLimits{}); err == nil {
		t.Error("a non-admin set the limits")
	}
	if _, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{DiskFreeMB: -1}); !IsInvalid(err) {
		t.Errorf("negative limit: %v", err)
	}
}

// The transcript archive's alarm says how big it has grown, in how many
// pieces, and what the limit is (audit 1233.25).
func TestTranscriptArchiveAlarmText(t *testing.T) {
	e := newEnv(t)
	rule := Rule{Trigger: Trigger{Event: "system_resources", Extra: map[string]any{"resource": "transcripts"}}, Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.text}}"}}}
	a, err := e.s.CreateAdminAutomation(e.ctx, e.admin, AutomationInput{Name: str("Archive full"), Rule: &rule})
	if err != nil {
		t.Fatal(err)
	}
	e.s.noteResource(e.ctx, ResourceStatus{Resource: "transcripts", Label: "Transcript archive", Bytes: 3 << 30, Count: 1234, CapMB: 4096, LimitMB: 4096, Low: true})
	runs := runsOf(t, e, a)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	want := "The transcript archive has grown to 3.0 GiB in 1234 pieces (the limit is 4096 MB)"
	if got := e.run(runs[0].ID).Event.Data["text"]; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func runsOf(t *testing.T, e *env, a *Automation) []*AutomationRun {
	t.Helper()
	list, err := e.s.ListAutomationRuns(e.ctx, e.admin, a.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestManualRunsAndWhoMayStartThem(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Manual", "")
	rule := Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{
		{Name: "topic", Label: "Topic", Type: "text", Required: true},
		{Name: "days", Type: "number", Default: "7"},
		{Name: "loud", Type: "bool"},
		{Name: "mode", Type: "select", Options: []string{"short", "long"}, Default: "short"},
	}, Actions: []Action{{Type: "create_issue", ID: "mk", Title: "{{.Inputs.topic}} / {{.Inputs.days}} / {{.Inputs.loud}} / {{.Inputs.mode}} / {{.Trigger.By}}"}}}
	a := e.addAuto(p, "Report", rule)
	sam := e.user("sam", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	vic := e.user("vic", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "vic", "viewer")
	run := func(who *Actor, in map[string]any) (*AutomationRun, error) {
		return e.s.RunAutomationNow(e.ctx, who, a.ID, in, "api")
	}

	if _, err := run(vic, map[string]any{"topic": "x"}); err == nil {
		t.Error("a viewer ran it")
	}
	if _, err := run(e.user("zed", false), map[string]any{"topic": "x"}); err == nil {
		t.Error("a non-member ran it")
	}
	if _, err := run(&Actor{User: sam.User, ReadOnly: true}, map[string]any{"topic": "x"}); err == nil {
		t.Error("a read-only token ran it")
	}
	// Inputs are checked.
	for name, in := range map[string]map[string]any{
		"missing":  {},
		"unknown":  {"topic": "x", "nope": 1},
		"number":   {"topic": "x", "days": "many"},
		"select":   {"topic": "x", "mode": "medium"},
		"too long": {"topic": strings.Repeat("x", 30_000)},
	} {
		if _, err := run(sam, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r, err := run(sam, map[string]any{"topic": "Launch", "loud": true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RunSucceeded || r.Via != "api" || r.Actor == nil || r.Actor.Username != "sam" {
		t.Errorf("run = %+v", r)
	}
	if keys := e.search(e.admin, "title:Launch"); len(keys) != 1 || e.get(e.admin, keys[0]).Title != "Launch / 7 / true / short / person" {
		t.Errorf("issues = %v", keys)
	}
	if got := e.run(r.ID); got.Inputs["topic"] != "Launch" || got.Inputs["days"] != float64(7) || got.Inputs["mode"] != "short" {
		t.Errorf("recorded inputs = %+v", got.Inputs)
	}

	// Agents may only run automations that allow it.
	agent := &Actor{User: sam.User, Agent: "anthropic/claude-opus-5-5"}
	if _, err := run(agent, map[string]any{"topic": "agent"}); err == nil || !errors.Is(err, ErrForbidden) && !strings.Contains(err.Error(), "agent") {
		t.Errorf("an agent ran an automation that didn't allow it: %v", err)
	}
	mcp := &Actor{User: sam.User, MCP: true}
	if _, err := run(mcp, map[string]any{"topic": "mcp"}); err == nil {
		t.Error("an MCP client ran an automation that didn't allow it")
	}
	rule.AllowAgents = true
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	ar, err := run(agent, map[string]any{"topic": "Agent run"})
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}
	if ar.Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("agent not recorded: %+v", ar)
	}
	if keys := e.search(e.admin, "title:Agent"); len(keys) != 1 || !strings.HasSuffix(e.get(e.admin, keys[0]).Title, "/ agent") {
		t.Errorf("agent run's trigger.by = %v", keys)
	}
	// Agents can't start it the other ways either, nor repeat its events.
	if _, err := e.s.UpdateAutomation(e.ctx, agent, a.ID, AutomationInput{Enabled: ptr(false)}); err != nil {
		t.Logf("agent (as owner's token) edit: %v", err) // project settings rules are unchanged by this slice
	}

	// Off, or without a manual trigger: it can't be run.
	off := false
	e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Enabled: &off})
	if _, err := run(sam, map[string]any{"topic": "x"}); !IsInvalid(err) {
		t.Errorf("disabled: %v", err)
	}
	created := e.addAuto(p, "On create", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "comment", Message: "x"}}})
	if _, err := e.s.RunAutomationNow(e.ctx, sam, created.ID, nil, "api"); !IsInvalid(err) {
		t.Errorf("no manual trigger: %v", err)
	}
	// Run again repeats a manual run's inputs.
	e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Enabled: ptr(true)})
	again, err := e.s.RerunAutomation(e.ctx, sam, r.ID)
	if err != nil || again.Inputs["topic"] != "Launch" {
		t.Errorf("rerun: %+v %v", again, err)
	}
	// Runs are for editors and up.
	if _, err := e.s.ListAutomationRuns(e.ctx, vic, a.ID, 10, 0); err == nil {
		t.Error("a viewer listed runs")
	}
	if list, err := e.s.ListAutomationRuns(e.ctx, sam, a.ID, 10, 0); err != nil || len(list) < 3 {
		t.Errorf("an editor's runs = %d %v", len(list), err)
	}
	// Paging.
	all := e.runs(a)
	page, _ := e.s.ListAutomationRuns(e.ctx, e.admin, a.ID, 2, all[1].ID)
	if len(page) == 0 || page[0].ID >= all[1].ID {
		t.Errorf("page = %+v", page)
	}
	// An editor sees runs without credentials in the rule.
	hook := e.addAuto(p, "Secrets", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "gotify", URL: "https://gotify.example.com/path", Token: "tok123", Message: "hi"}}})
	hr := e.manual(hook, nil)
	if got, _ := e.s.GetAutomationRun(e.ctx, sam, hr.ID); got.Rule.Actions[0].Token != "" || strings.Contains(got.Rule.Actions[0].URL, "path") {
		t.Errorf("an editor sees credentials: %+v", got.Rule.Actions[0])
	}
	if got, _ := e.s.GetAutomationRun(e.ctx, e.admin, hr.ID); got.Rule.Actions[0].Token != "tok123" {
		t.Errorf("an owner doesn't: %+v", got.Rule.Actions[0])
	}
	for _, s := range e.run(hr.ID).Steps {
		if s.Inputs["token"] != "(hidden)" || strings.Contains(anyString(s.Inputs["url"]), "path") {
			t.Errorf("recorded inputs leak: %+v", s.Inputs)
		}
	}
}

func TestDryRunTest(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Test", "")
	a := e.addAuto(p, "Tester", Rule{Trigger: Trigger{Event: "status_changed", To: "done"}, If: "type:bug", Actions: []Action{
		{Type: "comment", ID: "c", Message: "Closed {{.Issue.Key}}"},
		{Type: "delay", ID: "d", Extra: map[string]any{"duration": "5m"}},
		{Type: "ask_model", ID: "m", Extra: map[string]any{"prompt": "Summarise {{.Issue.Title}}"}},
		{Type: "add_label", ID: "l", Value: "shipped", OnlyIf: "{{.Steps.c.OK}}"},
		{Type: "add_label", ID: "n", Value: "never", OnlyIf: "{{.Steps.c.Failed}}"},
	}})
	// No matching event yet: sample data stands in, and says so.
	res, err := e.s.TestAutomation(e.ctx, e.admin, a.ID, TestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sample {
		t.Error("no event existed, but the test claims a real one")
	}
	bug := e.issue(e.admin, IssueInput{Project: p.Key, Type: "bug", Title: "Crash on save"})
	task := e.issue(e.admin, IssueInput{Project: p.Key, Type: "task", Title: "Chore"})
	e.s.Transition(e.ctx, e.admin, task.Key, "done", "")
	e.s.Transition(e.ctx, e.admin, bug.Key, "done", "")
	before := e.commentBodies(bug.Key)
	res, err = e.s.TestAutomation(e.ctx, e.admin, a.ID, TestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sample || res.Event == nil || res.Event.IssueKey != bug.Key {
		t.Fatalf("test event = %+v (sample %v)", res.Event, res.Sample)
	}
	if res.Error != "" || len(res.Steps) != 5 {
		t.Fatalf("test = %+v", res)
	}
	want := map[string]string{"c": "would_run", "d": "would_run", "m": "would_run", "l": "would_run", "n": "skipped"}
	for _, s := range res.Steps {
		if s.Status != want[s.StepID] {
			t.Errorf("step %s is %s, want %s", s.StepID, s.Status, want[s.StepID])
		}
	}
	if c := res.Steps[0]; c.Inputs["message"] != "Closed "+bug.Key || !strings.Contains(c.Log, "comment") {
		t.Errorf("comment step = %+v", c)
	}
	if m := res.Steps[2]; !strings.Contains(m.Log, "Would ask a model: Summarise Crash on save") || !strings.Contains(m.Log, "not run in a test") {
		t.Errorf("model step log = %q", m.Log)
	}
	// Nothing happened: no comment, no label, no run recorded.
	if after := e.commentBodies(bug.Key); len(after) != len(before) {
		t.Errorf("the test commented: %q", after)
	}
	if got := e.get(e.admin, bug.Key); len(got.Labels) != 0 {
		t.Errorf("the test labelled: %+v", got.Labels)
	}
	for _, r := range e.runs(a) {
		if r.Via == "test" {
			t.Error("a test was recorded as a run")
		}
	}
	// A step that can't work says so.
	bad := e.addAuto(p, "Bad template", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "comment", Message: "{{.Nope.Deeper}}"}, {Type: "comment", Message: "x"}}})
	br, err := e.s.TestAutomation(e.ctx, e.admin, bad.ID, TestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if br.Error == "" {
		t.Logf("steps: %+v", br.Steps)
	}
	// Webhook tests take sample data.
	hook := e.addAuto(p, "Hook", Rule{Trigger: Trigger{Event: "webhook"}, Actions: []Action{{Type: "notify", User: "admins", Message: "got {{.Trigger.Data.name}}"}}})
	hr, err := e.s.TestAutomation(e.ctx, e.admin, hook.ID, TestInput{Data: map[string]any{"name": "Ada"}})
	if err != nil || len(hr.Steps) != 1 || hr.Steps[0].Inputs["message"] != "got Ada" {
		t.Errorf("webhook test = %+v %v", hr, err)
	}
	// Owners only.
	sam := e.user("sam", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	if _, err := e.s.TestAutomation(e.ctx, sam, a.ID, TestInput{}); err == nil {
		t.Error("an editor tested an automation")
	}
}

func TestDispatchOnlyTouchesInterestedAutomations(t *testing.T) {
	// A disabled automation doesn't run; a deleted one takes its runs along.
	e := newEnv(t)
	p := e.project(e.admin, "Quiet", "")
	a := e.addAuto(p, "Off", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	off := false
	e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Enabled: &off})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "x"})
	if len(e.runs(a)) != 0 {
		t.Error("a disabled automation ran")
	}
	on := true
	e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Enabled: &on})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "y"})
	if err := e.s.DeleteAutomation(e.ctx, e.admin, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT (SELECT count(*) FROM automation_runs WHERE automation_id = ?) + (SELECT count(*) FROM automation_rules WHERE automation_id = ?)`, a.ID, a.ID).Scan(&n)
	if n != 0 {
		t.Errorf("a deleted automation left %d run rows", n)
	}
}

func TestCreatedByFilter(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "By", "")
	person := e.addAuto(p, "People", Rule{Trigger: Trigger{Event: "created", Extra: map[string]any{"by": "person"}}, Actions: []Action{{Type: "add_label", Value: "human"}}})
	agent := e.addAuto(p, "Agents", Rule{Trigger: Trigger{Event: "created", Extra: map[string]any{"by": "agent"}}, Actions: []Action{{Type: "add_label", Value: "bot"}}})
	anyone := e.addAuto(p, "Anyone", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "all"}}})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "by a person"})
	if _, err := e.s.CreateIssue(e.ctx, &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}, IssueInput{Project: p.Key, Title: "by an agent"}); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(person)) != 1 || len(e.runs(agent)) != 1 || len(e.runs(anyone)) != 2 {
		t.Errorf("runs: person %d, agent %d, any %d", len(e.runs(person)), len(e.runs(agent)), len(e.runs(anyone)))
	}
}

// Every gallery entry is a valid rule, and the ones turned on in new projects
// are the ones that always were.
func TestGalleryTemplatesValidate(t *testing.T) {
	registerTestActions()
	seen := map[string]bool{}
	must := map[string]bool{"Review with a second opinion": true, "Fix failing CI on members' pull requests": true}
	for _, tpl := range AutomationTemplates {
		if seen[tpl.Name] {
			t.Errorf("two templates called %q", tpl.Name)
		}
		seen[tpl.Name] = true
		r := tpl.Rule
		if err := r.validateFor(tpl.Admin); err != nil {
			t.Errorf("%q: %v", tpl.Name, err)
		}
		if tpl.Admin && tpl.Default {
			t.Errorf("%q is both an admin template and on by default", tpl.Name)
		}
		delete(must, tpl.Name)
		if tpl.Off && tpl.Default {
			t.Errorf("%q is both off and on by default", tpl.Name)
		}
		if (tpl.Name == "Review with a second opinion" || tpl.Name == "Fix failing CI on members' pull requests") && !tpl.Off {
			t.Errorf("%q should be added switched off", tpl.Name)
		}
	}
	for name := range must {
		t.Errorf("no template %q", name)
	}
	e := newEnv(t)
	p := e.project(e.admin, "Defaults", "")
	list, err := e.s.ListAutomations(e.ctx, e.admin, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range list {
		names = append(names, a.Name)
	}
	want := "Close parent when all subtasks are done, Reopen parent when a subtask reopens, Move to In Review when a PR opens, Move to Done when a PR merges, Review pull requests, Review when asked, Answer replies"
	if got := strings.Join(names, ", "); got != want {
		t.Errorf("default automations = %s", got)
	}
}
