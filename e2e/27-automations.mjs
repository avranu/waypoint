// Automations that run in steps (WP-D18), in a real browser at desktop and
// phone widths, light and dark: build one with a trigger, an "only if", a
// named step and a repeat group; see its run and the run's steps; run a
// manual one with the inputs it asks for; test one without changing
// anything; make a webhook automation and send to its address; preview a
// cron schedule; and Admin → Automations with the system limits.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KEY = "AUTO";

const browser = await chromium.launch();

async function run(width, scheme, full) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );
  // Nothing inside the open dialog pokes out sideways.
  const dialogFits = async (where) =>
    ok(
      await page.evaluate(() => {
        const d = document.querySelector('[role="dialog"]');
        return !d || d.scrollWidth <= d.clientWidth + 1;
      }),
      `${tag} the dialog fits its width (${where})`,
    );
  const choose = async (scope, label, option) => {
    await scope.getByRole("combobox", { name: label, exact: true }).first().click();
    await page.getByRole("option", { name: option, exact: true }).first().click();
  };
  const card = (name) =>
    page
      .locator("div.rounded-xl")
      .filter({ hasText: name })
      .filter({ has: page.getByRole("button", { name: "Runs", exact: true }) })
      .first();
  const addStep = async (scope, button, find, pick) => {
    await scope.getByRole("button", { name: button, exact: true }).first().click();
    await page.getByPlaceholder("Find an action…").fill(find);
    await page.getByRole("button", { name: pick }).first().click();
  };
  const dialog = (name) => page.getByRole("dialog", { name });
  const toSettings = async () => {
    await page.goto(`${base}/p/${KEY}/settings?tab=automations`);
    await page.getByRole("button", { name: "New automation" }).waitFor();
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  if (!(await api("GET", "/projects")).some((p) => p.key === KEY))
    await api("POST", "/projects", { name: "Automations", key: KEY });

  await toSettings();

  if (full) {
    // ---- build one: trigger, only if, a named step with its own only if, a repeat ----
    await page.getByRole("button", { name: "New automation" }).click();
    let dlg = dialog("New automation");
    await choose(dlg, "Trigger", "an issue is created");
    await dlg.getByRole("button", { name: "Only if…" }).click();
    await dlg.getByRole("textbox", { name: "Condition" }).fill("type:bug");
    await addStep(dlg, "choose an action", "notification", /Send a notification/);
    const steps = dlg.locator("section[aria-label=Rule] > ol > li");
    const first = steps.nth(0);
    await first.getByRole("textbox", { name: "Message" }).fill("A new bug: {{.Issue.Key}}");
    await first.getByRole("button", { name: /^name s1$/ }).click();
    await first.getByLabel("Step name").fill("tell");
    await first.getByRole("button", { name: "Only run if…" }).click();
    await first.getByRole("button", { name: "Ready-made" }).click();
    await page.getByRole("button", { name: /A person started it/ }).click();
    ok(
      (await first.getByLabel("Only if", { exact: true }).inputValue()).includes('eq .Trigger.By "person"'),
      `${tag} a ready-made only-if fills the template`,
    );
    await addStep(dlg, "and then…", "repeat", /Repeat steps/);
    const rep = steps.nth(1);
    await rep.getByLabel("Until", { exact: true }).fill("{{.Steps.tell.OK}}");
    await rep.getByLabel("At most").fill("2");
    await addStep(rep, "choose an action", "notification", /Send a notification/);
    await rep.getByRole("textbox", { name: "Message" }).fill("Still here: {{.Issue.Title}}");
    ok((await steps.count()) === 2, `${tag} two top-level steps, the repeat holding one`);
    await dlg.getByLabel("Name", { exact: true }).fill("Greet new bugs");
    await shot("editor");
    await dialogFits("editor");

    // Steps move with the arrows (and drag); nothing is saved.
    await first.getByRole("button", { name: "Move down" }).click();
    ok(
      (await steps.nth(0).getByRole("combobox", { name: "Action" }).first().innerText()).includes("repeat"),
      `${tag} Move down reorders the steps`,
    );
    await steps.nth(1).getByRole("button", { name: "Move up" }).first().click();
    ok(
      (await steps.nth(0).getByRole("combobox", { name: "Action" }).first().innerText()).includes("notify"),
      `${tag} Move up puts it back`,
    );
    // The drag handle works from the keyboard too: Space picks the step up, the arrow moves it, Space drops it.
    const firstIs = async (word) =>
      (await steps.nth(0).getByRole("combobox", { name: "Action" }).first().innerText()).includes(word);
    let dragged = false;
    for (let k = 0; k < 3 && !dragged; k++) {
      await steps.nth(0).getByRole("button", { name: "Drag to reorder" }).focus();
      await page.keyboard.press("Space");
      await page.waitForTimeout(400);
      await page.keyboard.press("ArrowDown");
      await page.waitForTimeout(400);
      await page.keyboard.press("Space");
      await page.waitForTimeout(400);
      dragged = await firstIs("repeat");
    }
    ok(dragged, `${tag} dragging a step by its handle with the keyboard reorders it`);
    await steps.nth(1).getByRole("button", { name: "Move up" }).first().click();

    await dlg.getByRole("button", { name: "Create" }).click();
    await dlg.waitFor({ state: "detached" });
    await page.getByText("Greet new bugs").first().waitFor();
    ok(true, `${tag} the automation was created`);

    // An issue that matches runs it; the run and its steps are kept.
    await api("POST", "/issues", { project: KEY, title: "Crash on save", type: "bug" });
    await card("Greet new bugs").getByText(/Ran 1 time/).waitFor();
    await card("Greet new bugs").getByRole("button", { name: "Runs", exact: true }).click();
    const runs = dialog(/Runs · Greet new bugs/);
    await runs.getByText("Succeeded").first().waitFor();
    await shot("runs");
    await runs.getByRole("button", { name: /Succeeded/ }).first().click();
    await dialog(/Run #\d+/).getByText("Round 1").waitFor();
    ok(
      (await dialog(/Run #\d+/).getByText("tell", { exact: true }).count()) > 0,
      `${tag} the run shows the step by its name`,
    );
    await shot("run-detail");
    await dialogFits("run detail");
    await page.keyboard.press("Escape"); // back to the list
    await page.keyboard.press("Escape"); // and out

    // ---- a test changes nothing ----
    await card("Greet new bugs").getByRole("button", { name: "Edit", exact: true }).click();
    dlg = dialog("Edit automation");
    await dlg.getByRole("button", { name: "Test", exact: true }).click();
    const test = dialog(/Test “Greet new bugs”/);
    await test.getByRole("button", { name: "Run the test" }).click();
    await test.getByText("Would run").first().waitFor();
    ok(await test.getByText("Nothing is changed or sent.").isVisible(), `${tag} a test says it changes nothing`);
    await shot("test");
    await test.getByRole("button", { name: "Close" }).first().click();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await card("Greet new bugs").getByText(/Ran 1 time/).waitFor();
    ok(true, `${tag} the test didn't count as a run`);

    // ---- a manual automation asks for its inputs ----
    await page.getByRole("button", { name: "New automation" }).click();
    dlg = dialog("New automation");
    await choose(dlg, "Trigger", "I press Run now");
    await dlg.getByRole("button", { name: "Add an input" }).click();
    await dlg.getByLabel("Input 1 name").fill("topic");
    await dlg.getByLabel("Label").fill("Topic");
    await dlg.getByRole("switch", { name: "Required" }).click();
    await addStep(dlg, "choose an action", "notification", /Send a notification/);
    const m1 = dlg.locator("section[aria-label=Rule] > ol > li").first();
    await choose(m1, "Who", "whoever triggered it");
    await m1.getByRole("textbox", { name: "Message" }).fill("Topic: {{.Inputs.topic}}");
    await dlg.getByLabel("Name", { exact: true }).fill("Announce a topic");
    await shot("manual-editor");
    await dlg.getByRole("button", { name: "Create" }).click();
    await dlg.waitFor({ state: "detached" });
    const manual = card("Announce a topic");
    await manual.getByRole("button", { name: "Run now" }).click();
    const ask = dialog(/Run “Announce a topic”/);
    await ask.getByRole("button", { name: "Run now" }).click();
    await ask.getByText("This one is needed").waitFor();
    ok(true, `${tag} a required input is asked for`);
    await ask.getByLabel(/Topic/).fill("release notes");
    await shot("run-now");
    await ask.getByRole("button", { name: "Run now" }).click();
    const done = dialog(/Run #\d+/);
    await done.getByText("release notes").first().waitFor();
    await done.getByText("Succeeded").first().waitFor();
    ok(true, `${tag} Run now runs with the inputs and opens the run`);
    await shot("manual-run");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");

    // ---- a webhook automation has an address ----
    await page.getByRole("button", { name: "New automation" }).click();
    dlg = dialog("New automation");
    await choose(dlg, "Trigger", "a request arrives at the webhook address");
    await dlg.getByText("The secret address appears here once you save.").waitFor();
    await addStep(dlg, "choose an action", "notification", /Send a notification/);
    await choose(dlg.locator("section[aria-label=Rule] > ol > li").first(), "Who", "whoever triggered it");
    await dlg.getByLabel("Name", { exact: true }).fill("Webhook intake");
    await dlg.getByRole("button", { name: "Create" }).click();
    dlg = dialog("Edit automation");
    await dlg.getByText("Webhook address", { exact: true }).waitFor();
    const hook = await dlg.locator("input[readonly]").first().inputValue();
    ok(hook.includes("/hooks/automations/"), `${tag} the saved webhook shows its address (${hook.slice(-12)})`);
    await shot("webhook");
    await dlg.getByRole("button", { name: "Done" }).click();
    const sent = await page.request.post(hook, { data: { hello: "world" } });
    ok(sent.status() === 202, `${tag} a POST to the address is accepted (${sent.status()})`);
    await card("Webhook intake").getByText(/Ran 1 time/).waitFor();
    ok(true, `${tag} the webhook started a run`);

    // ---- run now without inputs, a switched-off one, and a long run that waits and is cancelled ----
    const manualRule = (actions, more = {}) => ({ trigger: { event: "manual" }, actions, ...more });
    await api("POST", `/projects/${KEY}/automations`, { name: "Ping", enabled: true, rule: manualRule([{ type: "notify", user: "actor", message: "ping" }], { allow_agents: true }) });
    await api("POST", `/projects/${KEY}/automations`, { name: "Pong", enabled: false, rule: manualRule([{ type: "notify", user: "actor", message: "pong" }]) });
    await api("POST", `/projects/${KEY}/automations`, { name: "Slow one", enabled: true, rule: manualRule([{ type: "delay", duration: "1h" }]) });
    await toSettings();
    await card("Ping").getByText("Agents can run it").waitFor();
    ok(true, `${tag} a card says agents can run it`);
    await card("Slow one").getByText("Runs in the background").waitFor();
    ok(true, `${tag} a card with long steps says so`);
    ok(await card("Pong").getByRole("button", { name: "Run now" }).isDisabled(), `${tag} Run now is off while the automation is off`);
    await card("Ping").getByRole("button", { name: "Run now" }).click();
    await dialog(/Run #\d+/).getByText("Succeeded").first().waitFor();
    ok(true, `${tag} Run now without inputs runs straight away`);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await card("Slow one").getByRole("button", { name: "Run now" }).click();
    const slow = dialog(/Run #\d+/);
    await slow.getByText("Waiting", { exact: true }).first().waitFor({ timeout: 20000 });
    await shot("waiting");
    await slow.getByRole("button", { name: "Cancel" }).click();
    await slow.getByText("Cancelled", { exact: true }).first().waitFor();
    ok(true, `${tag} a waiting run can be cancelled`);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");

    // ---- cron shows when it will run ----
    await page.getByRole("button", { name: "New automation" }).click();
    dlg = dialog("New automation");
    await choose(dlg, "Trigger", "on a schedule");
    await choose(dlg, "Repeat", "a cron expression");
    await dlg.getByLabel("Cron", { exact: true }).fill("0 9 * * 1-5");
    await dlg.getByText("Next runs").waitFor();
    ok((await dlg.locator("li.tabular-nums").count()) === 5, `${tag} a cron expression previews its next five runs`);
    await shot("cron");
    await dlg.getByLabel("Cron", { exact: true }).fill("nonsense");
    await dlg.locator("p.text-danger").first().waitFor();
    ok(true, `${tag} a bad cron expression says what is wrong`);
    // What only the server can judge shows in the dialog, beside Save.
    await addStep(dlg, "choose an action", "notification", /Send a notification/);
    await dlg.getByRole("button", { name: "Create" }).click();
    await dlg.getByRole("alert").waitFor();
    ok(/cron|field/i.test(await dlg.getByRole("alert").innerText()), `${tag} the server's complaint shows in the dialog`);
    await dlg.getByRole("button", { name: "Cancel" }).click();

    // ---- every trigger and every action draws its settings without breaking ----
    const cat = await api("GET", "/automation-catalog");
    await page.getByRole("button", { name: "New automation" }).click();
    dlg = dialog("New automation");
    const clean = async (what) => {
      const text = await dlg.innerText();
      if (/undefined|\[object|NaN/.test(text)) fail(`${tag} ${what} shows raw values`);
    };
    const trigger = dlg.getByRole("combobox", { name: "Trigger", exact: true });
    await trigger.click();
    const nTriggers = await page.getByRole("option").count();
    await page.keyboard.press("Escape");
    ok(nTriggers >= cat.triggers.filter((t) => !t.admin).length, `${tag} every trigger is offered (${nTriggers})`);
    for (let i = 0; i < nTriggers; i++) {
      await trigger.click();
      await page.getByRole("option").nth(i).click();
      await dlg.getByRole("button", { name: "Create" }).waitFor();
      await clean(`trigger ${i + 1}`);
    }
    ok(true, `${tag} all ${nTriggers} triggers draw their settings`);
    await choose(dlg, "Trigger", "an issue is created");
    await addStep(dlg, "choose an action", "move to status", /Move to status/);
    const sweep = dlg.locator("section[aria-label=Rule] > ol > li").first().getByRole("combobox", { name: "Action", exact: true }).first();
    await sweep.click();
    const nActions = await page.getByRole("option").count();
    await page.keyboard.press("Escape");
    ok(nActions >= cat.actions.filter((a) => a.needs !== "pr").length, `${tag} every action is offered (${nActions})`);
    for (let i = 0; i < nActions; i++) {
      await sweep.click();
      await page.getByRole("option").nth(i).click();
      await dlg.getByRole("button", { name: "Create" }).waitFor();
      await clean(`action ${i + 1}`);
    }
    ok(true, `${tag} all ${nActions} actions draw their settings`);
    await shot("sweep");
    await dlg.getByRole("button", { name: "Cancel" }).click();

    // ---- rules written before steps existed open and save unchanged ----
    const OLD = [
      ["Old: ship it", { trigger: { event: "status_changed", from: "todo", to: "done" }, if: "type:bug", actions: [{ type: "set_priority", value: "high" }, { type: "add_label", value: "shipped" }, { type: "assign", user: "reporter" }] }],
      ["Old: weekly plan", { trigger: { event: "schedule", every: "week", weekday: 3, at: "10:30" }, actions: [{ type: "create_issue", title: "Weekly {{.Date}}", body: "Plan", labels: ["planning"], due: "+2d" }] }],
      ["Old: due soon", { trigger: { event: "due_soon", days: 2 }, actions: [{ type: "notify", user: "assignees", message: "Due soon: {{.Issue.Key}}" }, { type: "gotify", user: "watchers", title: "Due", message: "x", priority: 7 }] }],
      ["Old: review", { trigger: { event: "pr_updated" }, pr: { bases: ["main"], skip_labels: ["dependencies"] }, actions: [{ type: "review", focus: "security", max_findings: 5 }] }],
      // PR conditions, review options and an "only if" on what the review found (WP-D14 phase 5).
      ["Old: PR rules", { trigger: { event: "pr_updated" },
        pr: { authors: ["*[bot]", "ann"], skip_authors: ["renovate*"], labels: ["backend"], paths: ["web/**"], skip_paths: ["docs/**"], min_changed_lines: 5, max_changed_lines: 400, repos: ["me/*"] },
        actions: [{ type: "review", id: "review", min_severity: "P1", skip_files: ["**/*.snap"] }, { type: "notify", user: "owners", only_if: "{{or .Steps.review.P0 .Steps.review.P1}}", message: "{{.Steps.review.Worst}}" }] }],
      ["Old: labelled", { trigger: { event: "pr_labeled", label: "needs-review" }, pr: { repos: ["me/app"] }, actions: [{ type: "review" }] }],
      ["Old: field", { trigger: { event: "field_changed", field: "priority" }, actions: [{ type: "set_field", field: "due_date", value: "+3d" }, { type: "webhook", url: "https://example.com/hook", body: '{"k":"{{.Issue.Key}}"}' }] }],
      ["Old: quiet", { trigger: { event: "stale", days: 14 }, if: "is:unassigned", actions: [{ type: "comment", message: "Still needed?" }, { type: "add_to_sprint", value: "next" }] }],
    ];
    const sorted = (v) =>
      Array.isArray(v) ? v.map(sorted) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sorted(v[k])])) : v;
    const made = [];
    for (const [name, rule] of OLD) {
      const r = await api("POST", `/projects/${KEY}/automations`, { name, rule, enabled: false });
      made.push([name, r.id, JSON.stringify(sorted(r.rule))]);
    }
    await toSettings();
    for (const [name, id, before] of made) {
      await card(name).getByRole("button", { name: "Edit", exact: true }).click();
      const edit = dialog("Edit automation");
      await edit.getByRole("button", { name: "Save", exact: true }).click();
      await edit.waitFor({ state: "detached" });
      const after = JSON.stringify(sorted((await api("GET", `/automations/${id}`)).rule));
      ok(after === before, `${tag} ${name} saves unchanged${after === before ? "" : "\n  before " + before + "\n  after  " + after}`);
    }
    await overflow("settings");
    await shot("settings");
    await page.getByRole("heading", { name: "Ready-made" }).scrollIntoViewIfNeeded();
    await shot("gallery");
  } else {
    // ---- the same screens at phone width ----
    await card("Greet new bugs").getByRole("button", { name: "Edit", exact: true }).click();
    const dlg = dialog("Edit automation");
    await dlg.getByText("A new bug").first().waitFor().catch(() => {});
    await shot("editor");
    await dialogFits("editor");
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await card("Announce a topic").getByRole("button", { name: "Edit", exact: true }).click();
    await shot("manual-editor");
    await dialogFits("manual editor");
    await dialog("Edit automation").getByRole("button", { name: "Cancel" }).click();
    await card("Greet new bugs").getByRole("button", { name: "Runs", exact: true }).click();
    await dialog(/Runs · Greet new bugs/).getByRole("button", { name: /Succeeded/ }).first().click();
    await dialog(/Run #\d+/).getByText("Round 1").waitFor();
    await shot("run-detail");
    await dialogFits("run detail");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await overflow("settings");
    await shot("settings");
  }

  // ---- Admin → Automations ----
  await page.goto(`${base}/admin?tab=automations`);
  await page.getByRole("heading", { name: "Admin automations" }).waitFor();
  await page.getByRole("heading", { name: "System limits" }).waitFor();
  await page.getByText(/free of/).first().waitFor();
  ok(true, `${tag} the system readings show`);
  await page.getByText(/in \d+ pieces/).first().waitFor();
  await page.getByLabel("Transcript archive larger than (MB)").waitFor();
  ok(true, `${tag} the transcript archive reading and its limit show`);
  if (full) {
    await page.getByLabel("Free disk space below (MB)").fill("2048");
    await page.getByLabel("Transcript archive larger than (MB)").fill("5000");
    await page.getByRole("button", { name: "Save limits" }).click();
    await page.getByText("Limits saved").waitFor();
    await page.reload();
    await page.getByRole("heading", { name: "System limits" }).waitFor();
    await page.waitForFunction(
      () => [...document.querySelectorAll("input[type=number]")].some((i) => i.value === "2048"),
    );
    ok(
      (await page.getByLabel("Free disk space below (MB)").inputValue()) === "2048",
      `${tag} the saved limit comes back`,
    );
    ok(
      (await page.getByLabel("Transcript archive larger than (MB)").inputValue()) === "5000",
      `${tag} the transcript archive limit comes back`,
    );
    const low = page.locator("div.rounded-xl").filter({ hasText: "Low disk space" }).filter({ has: page.getByRole("button", { name: "Turn on" }) }).first();
    if (await low.count()) {
      await low.getByRole("button", { name: "Turn on" }).click();
      await card("Low disk space").waitFor();
      ok(true, `${tag} an admin ready-made turns on`);
    } else ok(false, `${tag} the Low disk space ready-made is listed`);
    await page.getByRole("button", { name: "New admin automation" }).click();
    const dlg = dialog("New admin automation");
    await dlg.getByRole("combobox", { name: "Trigger", exact: true }).click();
    await page.getByRole("option", { name: "a system resource", exact: true }).waitFor();
    ok(true, `${tag} admin triggers are offered in an admin automation`);
    await page.keyboard.press("Escape");
    // Admin automations draw every action too (there are no statuses or members to pick from).
    await dlg.getByRole("combobox", { name: "Trigger", exact: true }).click();
    await page.getByRole("option", { name: "a system resource", exact: true }).click();
    await addStep(dlg, "choose an action", "notification", /Send a notification/);
    const adminAction = dlg.locator("section[aria-label=Rule] > ol > li").first().getByRole("combobox", { name: "Action", exact: true }).first();
    await adminAction.click();
    const nAdmin = await page.getByRole("option").count();
    await page.keyboard.press("Escape");
    for (let i = 0; i < nAdmin; i++) {
      await adminAction.click();
      await page.getByRole("option").nth(i).click();
      await dlg.getByRole("button", { name: "Create" }).waitFor();
      if (/undefined|\[object|NaN/.test(await dlg.innerText())) fail(`${tag} admin action ${i + 1} shows raw values`);
    }
    ok(true, `${tag} all ${nAdmin} actions draw in an admin automation`);
    await shot("admin-editor");
    await dlg.getByRole("button", { name: "Cancel" }).click();
  }
  await overflow("admin");
  await shot("admin");

  // Project automations don't offer the admin-only triggers.
  if (full) {
    await toSettings();
    await page.getByRole("button", { name: "New automation" }).click();
    const dlg = dialog("New automation");
    await dlg.getByRole("combobox", { name: "Trigger", exact: true }).click();
    ok(
      (await page.getByRole("option", { name: "a system resource", exact: true }).count()) === 0,
      `${tag} a project automation has no system triggers`,
    );
    await page.keyboard.press("Escape");
    await dlg.getByRole("button", { name: "Cancel" }).click();
  }
  await ctx.close();
}

for (const [w, s, full] of looks([
  [1280, "light", true],
  [390, "dark", false],
])) {
  try {
    await run(w, s, full);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
