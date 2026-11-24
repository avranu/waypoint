// The automation editor as a sentence ("When [trigger] …, only if …, then
// [action] …") and the workflow step agent picker: one automation per
// trigger family built through the UI, reopened and saved again unchanged;
// each agent choice, including a specific spec, round-trips.
// Run after 01-signup-flow (user robin, project WAYP).
import { chromium } from "playwright";
import fs from "node:fs";
import assert from "node:assert";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const same = (a, b, m) => {
  try {
    assert.deepStrictEqual(a, b);
    ok(true, m);
  } catch {
    fail(`${m}: got ${JSON.stringify(a)}, want ${JSON.stringify(b)}`);
  }
};
const browser = await chromium.launch();
const H = { "X-Waypoint-Client": "web" };

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const dialog = page.getByRole("dialog");
  const choose = async (name, option) => {
    await dialog.getByRole("combobox", { name, exact: true }).click();
    await page.getByRole("option", { name: option, exact: true }).click();
  };
  const combo = (name) =>
    dialog.getByRole("combobox", { name, exact: true }).innerText();

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const addAction = async (first, label) => {
    await dialog
      .getByRole("button", { name: first ? "choose an action" : "and then…" })
      .click();
    await page.getByPlaceholder("Find an action…").fill(label);
    await page
      .locator("[data-popover] button")
      .filter({ has: page.getByText(label, { exact: true }) })
      .first()
      .click();
  };
  const cases = [
    {
      family: "Issues",
      verbs: ["move its parent to", "assign it to"],
      build: async () => {
        await choose("Trigger", "the status changes");
        await choose("To status", "any done status");
        await dialog.getByRole("button", { name: "Only if…" }).click();
        await dialog.getByLabel("Condition", { exact: true }).fill("type:bug");
        await addAction(true, "Move parent to status");
        await choose("Status", "In Progress");
        await addAction(false, "Assign");
        await choose("Who", "the reporter");
      },
      want: {
        trigger: { event: "status_changed", to: "done" },
        if: "type:bug",
        actions: [
          { type: "set_parent_status", status: "In Progress" },
          { type: "assign", user: "reporter" },
        ],
      },
      check: async () =>
        (await combo("To status")) === "any done status" &&
        (await dialog.getByLabel("Condition", { exact: true }).inputValue()) ===
          "type:bug" &&
        (await combo("Who")) === "the reporter",
    },
    {
      family: "Time",
      verbs: ["create an issue"],
      build: async () => {
        await choose("Trigger", "on a schedule");
        await choose("Repeat", "every week");
        await choose("Weekday", "Friday");
        await dialog.getByLabel("Time", { exact: true }).fill("08:30");
        await addAction(true, "Create an issue");
        await dialog.getByLabel("Title", { exact: true }).fill("Weekly review");
        await dialog
          .getByRole("button", { name: "More options" })
          .first()
          .click();
        await dialog
          .getByLabel("Tags", { exact: true })
          .fill("planning, weekly");
      },
      want: {
        trigger: { event: "schedule", every: "week", at: "08:30", weekday: 5 },
        actions: [
          {
            type: "create_issue",
            title: "Weekly review",
            labels: ["planning", "weekly"],
          },
        ],
      },
      check: async () =>
        (await combo("Weekday")) === "Friday" &&
        (await dialog.getByLabel("Title", { exact: true }).inputValue()) ===
          "Weekly review",
    },
    {
      family: "Code",
      verbs: ["add a comment", "set"],
      build: async () => {
        await choose("Trigger", "a linked pull request is merged");
        await addAction(true, "Add a comment");
        await dialog
          .getByLabel("Comment", { exact: true })
          .fill("Merged, thanks!");
        await addAction(false, "Set a field");
        await choose("Field", "due date");
        await dialog.getByLabel("Value", { exact: true }).fill("+3d");
      },
      want: {
        trigger: { event: "pr_merged" },
        actions: [
          { type: "comment", message: "Merged, thanks!" },
          { type: "set_field", field: "due_date", value: "+3d" },
        ],
      },
      check: async () =>
        (await combo("Trigger")) === "a linked pull request is merged" &&
        (await dialog.getByLabel("Value", { exact: true }).inputValue()) ===
          "+3d",
    },
    {
      family: "Sprints",
      verbs: ["notify", "add the tag"],
      build: async () => {
        await choose("Trigger", "a sprint starts");
        await addAction(true, "Send a notification");
        await choose("Who", "the watchers");
        await addAction(false, "Add tag");
        await dialog.getByLabel("Tag", { exact: true }).fill("sprint-start");
      },
      want: {
        trigger: { event: "sprint_started" },
        actions: [
          { type: "notify", user: "watchers" },
          { type: "add_label", value: "sprint-start" },
        ],
      },
      check: async () => (await combo("Who")) === "the watchers",
    },
  ];
  for (const c of cases) {
    const name = `e2e ${c.family} ${tag}`;
    await page.goto(base + "/p/WAYP/settings?tab=automations");
    await page.getByRole("button", { name: "New automation" }).click();
    await dialog.getByRole("textbox", { name: "Name" }).fill(name);
    await c.build();
    if (c.family === "Issues") await shot("automation-issues");
    if (c.family === "Time") await shot("automation-time");
    await dialog.getByRole("button", { name: "Create" }).click();
    await dialog.waitFor({ state: "hidden" });
    const find = async () =>
      (await api("GET", "/projects/WAYP/automations")).find(
        (a) => a.name === name,
      );
    const a = await find();
    same(a?.rule, c.want, `${tag} ${c.family}: saved as built`);
    await page.getByRole("button", { name, exact: true }).click();
    await dialog.waitFor();
    ok(await c.check(), `${tag} ${c.family}: reopens with its values`);
    same(
      await dialog
        .getByRole("combobox", { name: "Action", exact: true })
        .allInnerTexts(),
      c.verbs,
      `${tag} ${c.family}: actions read as a sentence`,
    );
    await dialog.getByRole("button", { name: "Save" }).click();
    await dialog.waitFor({ state: "hidden" });
    same((await find())?.rule, c.want, `${tag} ${c.family}: round-trips`);
    if (a) await api("DELETE", `/automations/${a.id}`);
  }

  await page.goto(base + "/p/WAYP/settings?tab=workflows");
  await page.getByRole("button", { name: "New workflow" }).click();
  const wfName = `Agents ${tag}`;
  await dialog.getByRole("textbox", { name: "Name", exact: true }).fill(wfName);
  await dialog.getByRole("button", { name: "Add step" }).click();
  await dialog.getByRole("button", { name: "Add step" }).click();
  const steps = ["Plan", "Review", "Fix", "Polish"];
  for (const [i, s] of steps.entries())
    await dialog.getByLabel(`Step ${i + 1} name`).fill(s);
  await choose("Step 2 agent", "A different agent than before");
  await choose("Step 3 agent", "The same agent as before");
  await choose("Step 4 agent", "Specific agents…");
  const spec = dialog.getByLabel("Step 4 agent spec");
  ok((await spec.inputValue()) === "any", `${tag} specific starts at "any"`);
  await spec.fill("");
  ok(await spec.isVisible(), `${tag} emptying the spec keeps the field open`);
  await spec.fill("haiku 5.5");
  await shot("workflow-agents");
  await dialog.getByRole("button", { name: "Save workflow" }).click();
  await dialog.waitFor({ state: "hidden" });
  const wfFind = async () =>
    (await api("GET", "/workflows?project=WAYP")).find(
      (w) => w.name === wfName,
    );
  const wf = await wfFind();
  const agents = (w) => w?.steps.map((s) => s.agent ?? "");
  same(
    agents(wf),
    ["", "different", "same", "haiku 5.5"],
    `${tag} agents saved`,
  );
  await page.getByRole("button", { name: `Edit ${wfName}` }).click();
  await dialog.waitFor();
  ok(
    (await combo("Step 1 agent")) === "Issue's agent" &&
      (await combo("Step 2 agent")) === "A different agent than before" &&
      (await combo("Step 3 agent")) === "The same agent as before" &&
      (await combo("Step 4 agent")) === "Specific agents…" &&
      (await dialog.getByLabel("Step 4 agent spec").inputValue()) ===
        "haiku 5.5",
    `${tag} agents reopen as saved`,
  );
  const noScroll = await page.evaluate(
    () => document.documentElement.scrollWidth <= window.innerWidth,
  );
  ok(noScroll, `${tag} no sideways scroll`);
  await dialog.getByRole("button", { name: "Save workflow" }).click();
  await dialog.waitFor({ state: "hidden" });
  same(
    agents(await wfFind()),
    ["", "different", "same", "haiku 5.5"],
    `${tag} agents round-trip`,
  );
  if (wf) await api("DELETE", `/workflows/${wf.id}`);
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
