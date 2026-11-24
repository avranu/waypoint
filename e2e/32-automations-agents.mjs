// Automations that run coding agents and pull requests (WP-D18, phase 2), in
// a real browser at desktop and phone widths, light and dark: the gallery's
// weekly code review turns on, its steps show in the editor with the model
// chain and whose runners it uses, a test says what it would do, the card
// that deploys is added switched off, a copy opens to edit, and a waiting run
// (ask a person) is carried on when the person answers.
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

const browser = await chromium.launch();

async function run(width, scheme, key, full) {
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
  const dialogFits = async (where) =>
    ok(
      await page.evaluate(() => {
        const d = document.querySelector('[role="dialog"]');
        return !d || d.scrollWidth <= d.clientWidth + 1;
      }),
      `${tag} the dialog fits its width (${where})`,
    );
  const dialog = (name) => page.getByRole("dialog", { name });
  const gallery = (name) =>
    page
      .locator("div.rounded-xl")
      .filter({ hasText: name })
      .filter({ has: page.getByRole("button", { name: /^(Turn on|Add, off)$/ }) })
      .first();
  const card = (name) =>
    page
      .locator("div.rounded-xl")
      .filter({ hasText: name })
      .filter({ has: page.getByRole("button", { name: "Runs", exact: true }) })
      .first();
  const toSettings = async () => {
    await page.goto(`${base}/p/${key}/settings?tab=automations`);
    await page.getByRole("button", { name: "New automation" }).waitFor();
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  if (!(await api("GET", "/projects")).some((p) => p.key === key))
    await api("POST", "/projects", { name: `Agents ${key}`, key });
  await toSettings();

  const weekly = gallery("Weekly code review into a PR");
  await weekly.waitFor();
  for (const tagText of ["Needs a runner", "Changes PRs"])
    ok((await weekly.getByText(tagText, { exact: true }).count()) > 0, `${tag} the weekly card says "${tagText}"`);
  for (const name of [
    "Ship merged PRs",
    "Merge when ready",
    "Retry flaky CI once",
    "Tell me when an environment goes down",
    "Tell me when an agent's question waits more than 30 minutes",
    "Daily digest of agent work",
    "Fix failing CI on agents' PRs",
  ])
    ok((await gallery(name).count()) > 0, `${tag} the gallery has "${name}"`);
  const ship = gallery("Ship merged PRs");
  ok((await ship.getByRole("button", { name: "Add, off" }).count()) === 1, `${tag} the deploy card is added off`);
  await shot("gallery");
  await overflow("gallery");

  await weekly.getByRole("button", { name: "Turn on" }).click();
  const mine = card("Weekly code review into a PR");
  await mine.waitFor();
  ok(true, `${tag} the weekly review is on`);
  await mine.getByRole("button", { name: "Edit", exact: true }).click();
  let dlg = dialog("Edit automation");
  const steps = dlg.locator("section[aria-label=Rule] > ol > li");
  await steps.first().waitFor();
  ok((await steps.count()) === 6, `${tag} it has six top-level steps (${await steps.count()})`);
  const actions = await steps.evaluateAll((els) =>
    els.map((e) => e.querySelector('[role="combobox"]')?.textContent ?? ""),
  );
  ok(
    /coding agent/i.test(actions[0]) && /pull request/i.test(actions[1]) && /repeat/i.test(actions[2]),
    `${tag} the steps read agent, pull request, repeat (${actions.slice(0, 3).join(" | ")})`,
  );
  const chain = dlg.getByTestId("agent-chain").first();
  await chain.waitFor();
  ok(
    (await chain.getByLabel("Model 1").inputValue()) === "opencode/muse-spark-1.3-contributor-free",
    `${tag} the agent step names its CLI and model`,
  );
  const note = dlg.getByTestId("runner-note");
  await note.waitFor();
  ok(/your\b.*runners/i.test(await note.innerText()), `${tag} the editor says whose runners it uses`);
  ok(/no runner yet/i.test(await note.innerText()), `${tag} and that robin has no runner yet`);
  await shot("weekly-editor");
  await dialogFits("weekly editor");

  await chain.getByRole("button", { name: "Add a fallback" }).click();
  ok((await chain.locator("li").count()) === 2, `${tag} a fallback model can be added`);
  await chain.getByLabel("CLI 2").selectOption("claude").catch(async () => {
    await chain.getByRole("combobox", { name: "CLI 2" }).click();
    await page.getByRole("option", { name: "Claude Code" }).click();
  });
  await chain.getByLabel("Model 2").fill("sonnet");
  await shot("weekly-fallback");

  await dlg.getByRole("button", { name: "Test", exact: true }).click();
  const test = dialog(/Test “Weekly code review into a PR”/);
  await test.getByRole("button", { name: "Run the test" }).click();
  await test.getByText("Would run").first().waitFor();
  ok(
    (await test.getByText(/Would run .*muse-spark/i).count()) > 0,
    `${tag} the test says the agent step would run Muse Spark`,
  );
  await shot("weekly-test");
  await test.getByRole("button", { name: "Close" }).first().click();
  await dlg.getByRole("button", { name: /^(Save|Create)$/ }).click();
  await dlg.waitFor({ state: "detached" });
  const saved = (await api("GET", `/projects/${key}/automations`)).find((a) => a.name === "Weekly code review into a PR");
  const chainSaved = saved.rule.actions[0].agents;
  ok(
    chainSaved?.length === 2 && chainSaved[1].cli === "claude" && chainSaved[1].model === "sonnet",
    `${tag} the saved rule keeps the two-model chain (${JSON.stringify(chainSaved)})`,
  );
  ok(saved.saved_by?.username === "robin", `${tag} the API says robin saved it`);

  await ship.getByRole("button", { name: "Add, off" }).click();
  const shipped = card("Ship merged PRs");
  await shipped.waitFor();
  ok(!(await shipped.getByRole("switch").first().isChecked()), `${tag} “Ship merged PRs” is added switched off`);
  await shot("ship-off");

  await toSettings();
  await gallery("Fix failing CI on agents' PRs").getByRole("button", { name: "Copy and edit" }).click();
  dlg = dialog("New automation");
  await dlg.getByLabel("Name", { exact: true }).waitFor();
  // the form fills the copy's name a moment after the dialog opens
  const copyName = await dlg.getByLabel("Name", { exact: true }).evaluate(
    (el) =>
      new Promise((resolve) => {
        const t = Date.now();
        const poll = () => (el.value.includes("(copy)") || Date.now() - t > 10000 ? resolve(el.value) : setTimeout(poll, 100));
        poll();
      }),
  );
  ok(copyName.includes("(copy)"), `${tag} a copy opens to edit`);
  ok((await dlg.getByTestId("agent-chain").count()) > 0, `${tag} the CI fixer shows its model chain`);
  await shot("copy-editor");
  await dialogFits("copy editor");
  await dlg.getByRole("button", { name: "Cancel" }).click();

  if (full) {
    await api("POST", `/projects/${key}/automations`, {
      name: "Ask robin",
      enabled: true,
      rule: {
        trigger: { event: "manual" },
        actions: [
          { type: "ask_person", id: "ask", user: "robin", title: "Ship it?", choices: ["yes", "no"], timeout: "1h" },
          { type: "notify", user: "robin", title: "Answered", message: "Robin said {{.Steps.ask.Answer}}" },
        ],
      },
    });
    await toSettings();
    const asking = card("Ask robin");
    await asking.getByRole("button", { name: "Run now" }).click();
    const runDlg = dialog(/Run #\d+/);
    await runDlg.getByText("Waiting").first().waitFor();
    ok(true, `${tag} a run that asks a person waits`);
    await shot("ask-waiting");
    const reqs = await api("GET", "/requests?state=open");
    const list = Array.isArray(reqs) ? reqs : (reqs.requests ?? reqs.items ?? []);
    const mineReq = list.find((r) => r.title === "Ship it?");
    ok(!!mineReq, `${tag} the question is in robin's requests`);
    if (mineReq) {
      await api("POST", `/requests/${mineReq.ref ?? mineReq.code}/decide`, { decision: "answer", text: "yes" });
      await runDlg.getByText("Succeeded").first().waitFor({ timeout: 20000 });
      ok(true, `${tag} answering carries the run on to the end`);
      await shot("ask-done");
    }
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  }
  await overflow("end");
  await ctx.close();
}

for (const [w, s, key, full] of looks([
  [1280, "light", "AGLA", true],
  [1280, "dark", "AGDA", false],
  [390, "light", "AGLB", false],
  [390, "dark", "AGDB", false],
])) {
  try {
    await run(w, s, key, full);
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
