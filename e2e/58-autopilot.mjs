// Autopilot (WP-194) in a real browser at desktop and phone widths, light and
// dark: the Agents page's Autopilot tab. Turn autopilot on for a project,
// give the project its own grace and merge policy, set a default limit (the
// fields show what they inherit); a session that finishes a task makes a step
// that counts down, which can be stopped (a second one is started at once);
// the task summary the agent left is listed with what waits on the person and
// the request filed for it, counts as unread on the tab until it is marked
// read, and can be marked unread again; turning the project off ends the step.
// A week's quota past its ceiling pauses it, and the quota limits can be
// turned off for a while and back on from the banner. A start of autopilot's
// own has no session yet: its line says so, and only Stop is offered.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import crypto from "node:crypto";
import fs from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const KEY = `AP${"ABCDEFGH"[n]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d, headers) =>
    call(m, p, d, headers).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );
  const pick = async (scope, name, option) => {
    await scope.getByRole("combobox", { name, exact: true }).click();
    await page.getByRole("option", { name: option, exact: true }).click();
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  await api("POST", "/projects", { name: `Autopilot ${n}`, key: KEY });
  const { token } = await api("POST", "/me/tokens", { name: `autopilot ${n}`, agent: "anthropic/claude-code" });
  const agent = { ...H, Authorization: `Bearer ${token}` };

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  db.exec("DELETE FROM quota_samples");
  db.close();
  await page.goto(`${base}/agents?tab=autopilot`);
  await page.getByRole("tab", { name: /^Autopilot/, selected: true }).waitFor();
  await page.getByText("Last hour", { exact: true }).waitFor();
  ok((await page.locator('section[aria-label="Readings"] [title*="No recent quota reading"]').count()) === 1, `${tag} missing quota readings are said so (tooltip)`);
  const row = page.getByTestId(`autopilot-${KEY}`);
  await row.waitFor();
  ok(!(await row.innerText()).includes("No sessions"), `${tag} a project with autopilot off shows no status`);
  await shot("off");
  await overflow("off");

  await page.getByRole("switch", { name: `Autopilot in ${KEY}` }).click();
  await row.getByText("No sessions of yours open here").waitFor();
  ok(true, `${tag} turning a project on says no session of yours is open there`);
  await row.getByRole("button", { name: `Limits for ${KEY}` }).click();
  const dlg = page.getByRole("dialog", { name: "Autopilot settings" });
  await dlg.waitFor();
  const grace = page.locator(`#p-${KEY}-grace_seconds`);
  ok((await grace.getAttribute("placeholder")) === "60", `${tag} the grace field shows the inherited 60`);
  await dlg
    .locator("div.relative")
    .filter({ has: page.getByRole("combobox", { name: "Adversarial review", exact: true }) })
    .getByRole("button", { name: "More info" })
    .click();
  const reviewHint = page.getByText("whether autopilot is on or off", { exact: false }).filter({ visible: true }).first();
  await reviewHint.waitFor();
  ok(await reviewHint.isVisible(), `${tag} the review setting says it holds with autopilot off too (WP-1007)`);
  await shot("review-hint");
  await overflow("review hint open");
  await page.keyboard.press("Escape");
  if (!(await dlg.isVisible())) await row.getByRole("button", { name: `Limits for ${KEY}` }).click();
  await grace.fill("3600");
  await grace.press("Enter");
  await pick(dlg, "Merge wait", "Never wait");
  await page.waitForFunction(async ([b, k]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return p && p.limits.grace_seconds === 3600 && p.limits.merge === "never";
  }, [base, KEY]);
  ok(true, `${tag} the project's overrides are saved`);
  await shot("project-limits");
  await overflow("project limits open");

  await pick(dlg, "Priority", "High or above");
  await pick(dlg, "Approved only", "On");
  await page.locator(`#p-${KEY}-per_day`).fill("10");
  const atOnce = page.locator(`#p-${KEY}-at_once`);
  ok((await atOnce.getAttribute("placeholder")) === "1", `${tag} sessions at once shows the inherited 1`);
  await atOnce.fill("2");
  await page.locator(`#p-${KEY}-hours`).fill("9-18");
  await page.locator(`#p-${KEY}-days`).fill("weekdays");
  await page.locator(`#p-${KEY}-query`).fill("label:x order:created");
  ok(/not their order/.test(await dlg.innerText()), `${tag} a query with order: is refused before saving`);
  await page.locator(`#p-${KEY}-query`).fill("-label:risky");
  await page.locator(`#p-${KEY}-query`).press("Enter");
  await page.waitForFunction(async ([b, k]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return p && p.limits.hours === "09:00-18:00" && p.limits.days === "mon-fri" && p.limits.min_priority === "high" && p.limits.at_once === 2 && p.limits.query === "-label:risky";
  }, [base, KEY]);
  ok(true, `${tag} the criteria and pace are saved in one spelling`);
  await row.getByTestId("autopilot-criteria").filter({ hasText: "mon-fri" }).waitFor({ timeout: 5000 }).catch(() => {});
  const words = await row.getByTestId("autopilot-criteria").innerText();
  ok(
    /high priority or above, approved by a person, matching -label:risky/.test(words) && /10 a day, 2 sessions at once, 09:00-18:00, mon-fri/.test(words),
    `${tag} the row says the criteria and pace (${words.replace(/\s+/g, " ")})`,
  );
  ok((await page.locator(`#p-${KEY}-hours`).inputValue()) === "09:00-18:00", `${tag} the hours field shows the kept spelling`);
  await shot("criteria");
  await overflow("criteria");
  await api("PUT", `/autopilot/projects/${KEY}`, { limits: { grace_seconds: 3600, merge: "never" } });
  await page.reload();
  await row.waitFor();
  ok((await row.getByTestId("autopilot-criteria").count()) === 0, `${tag} with no criteria or pace the row says nothing of them`);

  await row.getByRole("button", { name: `Limits for ${KEY}` }).click();
  await dlg.getByRole("tab", { name: "Defaults" }).click();
  const hourly = page.locator("#defaults-max_hourly_usd");
  ok((await hourly.getAttribute("placeholder")) === "75", `${tag} the default hourly limit shows Waypoint's 75`);
  await hourly.fill(String(40 + n));
  await hourly.press("Enter");
  await page.waitForFunction(async ([b, v]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    return o.defaults.max_hourly_usd === v;
  }, [base, 40 + n]);
  await dlg.getByRole("tab", { name: KEY, exact: true }).click();
  await page.locator(`#p-${KEY}-max_hourly_usd`).waitFor();
  await page.waitForFunction(([k, v]) => document.querySelector(`#p-${k}-max_hourly_usd`)?.getAttribute("placeholder") === v, [KEY, String(40 + n)], { timeout: 5000 }).catch(() => {});
  const inherited = await page.locator(`#p-${KEY}-max_hourly_usd`).getAttribute("placeholder");
  ok(inherited === String(40 + n), `${tag} the project inherits the new default (${inherited})`);
  await page.keyboard.press("Escape");
  await dlg.waitFor({ state: "hidden" });
  await page.locator('section[aria-label="Readings"]').getByText(`$${40 + n}.00`, { exact: true }).waitFor();
  ok(true, `${tag} the readings use the new limit`);

  {
    const qdb = new DatabaseSync(`${process.env.DIR}/wp.db`);
    qdb.exec("PRAGMA busy_timeout = 5000");
    const uid = qdb.prepare("SELECT id FROM users WHERE username = 'robin'").get().id;
    qdb.prepare("INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine, session, model) VALUES (?,?,?,?,?,?,?,?)")
      .run(uid, Date.now(), "seven_day", 91, Date.now() + 3 * 24 * 3600_000, "devhost", "", "claude-opus-5-5");
    qdb.close();
    await page.reload();
    const banner = page.locator('section[aria-label="Readings"] [role=status]');
    await banner.getByText("Autopilot is paused:").waitFor();
    ok(/week's quota is at 91%/.test(await banner.innerText()), `${tag} the week's quota pauses it`);
    await banner.getByRole("button", { name: "Turn quota limits off" }).click();
    ok((await page.getByRole("menuitem", { name: /Until the week resets/ }).count()) === 1, `${tag} the quota limits can be off until the week resets`);
    await shot("quota-off-menu");
    await overflow("quota off menu");
    await page.getByRole("menuitem", { name: "For an hour" }).click();
    await banner.getByText("Quota limits are off").waitFor();
    const said = await banner.innerText();
    ok(/91%/.test(said) && /keeps starting tasks/.test(said), `${tag} while off it says what they would pause for (${said.replace(/\s+/g, " ")})`);
    const o = await api("GET", "/autopilot");
    ok(!o.meter.paused_because && o.meter.quota_off_until, `${tag} the quota limits are off for the server too`);
    await shot("quota-off");
    await overflow("quota off");
    await banner.getByRole("button", { name: "Turn back on" }).click();
    await banner.getByText("Autopilot is paused:").waitFor();
    ok(true, `${tag} turning them back on pauses it again`);
    const qdb2 = new DatabaseSync(`${process.env.DIR}/wp.db`);
    qdb2.exec("PRAGMA busy_timeout = 5000");
    qdb2.exec("DELETE FROM quota_samples");
    qdb2.close();
  }

  const finish = async (suffix, machine, last, prompt) => {
    const where = { session_id: `cli-ap-${n}-${suffix}`, machine, cwd: `/work/ap${n}`, branch: `feat/ap${n}${suffix}` };
    const hook = (event, extra = {}) => api("POST", "/sessions/hook", { events: [{ event, ...where, ...extra }] }, agent);
    const as = { ...agent, "X-Waypoint-Session": where.session_id, "X-Waypoint-Machine": machine };
    await hook("SessionStart", { model: "claude-opus-5-5", source: "startup" });
    {
      const born = new DatabaseSync(`${process.env.DIR}/wp.db`);
      born.exec("PRAGMA busy_timeout = 5000");
      const uid = born.prepare("SELECT id FROM users WHERE username = 'robin'").get().id;
      const pid = born.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
      const sid = born.prepare("SELECT id FROM agent_sessions WHERE cli_id = ? AND machine = ?").get(where.session_id, machine).id;
      const at = Date.now() - 2 * 24 * 3600 * 1000;
      born.prepare(`INSERT INTO autopilot_steps (user_id, project_id, session_id, stop_at, state, due_at, next_session, next_session_at, created_at, updated_at)
        VALUES (?, ?, NULL, ?, 'started', ?, ?, ?, ?, ?)`).run(uid, pid, at, at, sid, at, at, at);
      const sum = crypto.createHash("sha256").update(prompt).digest("hex");
      born.prepare(`INSERT INTO typed_prompts (session_id, user_id, hash, kind, created_at, expires_at) VALUES (?, ?, ?, 'kickoff', ?, ?)`).run(
        sid, uid, sum, Date.now(), Date.now() + 3600 * 1000);
      born.close();
    }
    const issue = await api("POST", "/issues", { project: KEY, title: `Task ${suffix} of ${KEY}`, agent: "claude" });
    const took = await call("POST", `/agent/take/${issue.key}`, {}, as);
    if (!took.ok()) throw new Error(`take ${issue.key}: ${took.status()} ${await took.text()}`);
    await page.waitForTimeout(5);
    await hook("UserPromptSubmit", { prompt, title: `Session ${suffix}` });
    const done = await call("POST", `/agent/done/${issue.key}`, { comment: "done" }, as);
    if (!done.ok()) throw new Error(`done ${issue.key}: ${done.status()} ${await done.text()}`);
    await hook("Stop", { last_message: last, pending: { tasks: [], crons: [] } });
    return issue.key;
  };
  const k1 = await finish(
    "a",
    `ap-box-${n}`,
    `Finished the parser: **all green**, PR is up.\n\n- Added error recovery\n- Ran \`go test ./...\`\n\n**Waiting on you:**\n- Rotate the forge token before Friday\n`,
    "Waypoint autopilot started this session for the parser.",
  );
  const k2 = await finish("b", `ap-laptop-${n}`, "Renamed the CSS class; nothing else changed.", "Waypoint autopilot started this session for the CSS rename.");

  await page.goto(`${base}/agents?tab=autopilot`);
  const live = row.getByTestId("autopilot-step");
  await live.first().waitFor();
  await page.waitForFunction((k) => document.querySelectorAll(`[data-testid="autopilot-${k}"] [data-testid="autopilot-step"]`).length >= 2, KEY);
  const stepText = await row.innerText();
  ok(/in \d+(\.\d+)? ?h|in \d+ min/.test(stepText), `${tag} the untouched session's step counts down (${stepText.replace(/\s+/g, " ").slice(0, 160)})`);
  ok(stepText.includes(`ap-box-${n}`) && stepText.includes(`after ${k1}`), `${tag} the step names the machine and the finished task`);
  const unread = page.getByRole("tab", { name: /^Autopilot/ });
  ok(/\b2\b/.test(await unread.innerText()), `${tag} the tab counts 2 unread summaries (${(await unread.innerText()).replace(/\s+/g, " ")})`);
  const cards = page.getByTestId("task-summary");
  await cards.nth(1).waitFor();
  ok((await cards.count()) === 2, `${tag} both summaries are listed`);
  const card1 = cards.filter({ hasText: k1 });
  ok((await card1.locator("strong", { hasText: "all green" }).count()) === 1, `${tag} the summary body is rendered as markdown`);
  const asks = card1.getByTestId("summary-asks");
  await asks.waitFor();
  ok(/Possibly waiting on you/.test(await asks.innerText()) && /Rotate the forge token/.test(await asks.innerText()), `${tag} what waits on the person is highlighted`);
  ok((await asks.getByRole("link", { name: /Filed as request/ }).count()) === 1, `${tag} it links to the request filed for it`);
  ok((await card1.getByRole("link", { name: k1 }).getAttribute("href")) === `/issue/${k1}`, `${tag} the card links to its issue`);
  ok((await cards.filter({ hasText: k2 }).getByTestId("summary-asks").count()) === 0, `${tag} a summary that asks nothing has no ask box`);
  await shot("with-data");
  await overflow("with data");
  if (width < 768) {
    const fab = page.locator('button[aria-label="New issue"]');
    const shown = async () => (await fab.evaluate((el) => getComputedStyle(el).opacity)) === "1";
    await page.mouse.move(195, 450);
    await page.mouse.wheel(0, 800);
    await page.waitForFunction(() => document.querySelector('button[aria-label="New issue"]')?.getAttribute("aria-hidden") === "true");
    await page.waitForTimeout(300);
    ok(!(await shown()), `${tag} the + button hides while scrolling down`);
    await page.screenshot({ path: `${out}/fab-hidden-${width}-${scheme}.png` });
    await page.mouse.wheel(0, -300);
    await page.waitForFunction(() => !document.querySelector('button[aria-label="New issue"]')?.hasAttribute("aria-hidden"));
    await page.waitForTimeout(300);
    ok(await shown(), `${tag} and comes back on scrolling up`);
    await page.screenshot({ path: `${out}/fab-back-${width}-${scheme}.png` });
  }

  const steps = row.getByTestId("autopilot-step");
  await steps.filter({ hasText: `ap-box-${n}` }).getByRole("button", { name: "Stop" }).click();
  await row.getByText("1 earlier today").waitFor();
  ok(true, `${tag} a stopped step moves to the earlier ones`);
  await row.getByRole("button", { name: /earlier today/ }).click();
  await row.locator('[data-testid="autopilot-step"][data-state="stopped"]').waitFor();
  await steps.filter({ hasText: `ap-laptop-${n}` }).getByRole("button", { name: "Start now" }).click();
  await page.waitForFunction((k) => !/ in \d+ (min|h)/.test(document.querySelector(`[data-testid="autopilot-${k}"]`)?.textContent ?? ""), KEY);
  ok(true, `${tag} Start now takes the step out of its countdown (${(await row.innerText()).replace(/\s+/g, " ").slice(-200)})`);
  await shot("stopped");
  await overflow("after stop / start now");

  const idle = row.getByTestId("autopilot-session").filter({ hasText: `ap-box-${n}` });
  await idle.waitFor();
  ok(/Idle/.test(await idle.innerText()), `${tag} the stopped session is listed idle (${(await idle.innerText()).replace(/\s+/g, " ")})`);
  await shot("idle-session");
  await overflow("idle session");
  await idle.getByRole("button", { name: "Start next task" }).click();
  await page.waitForFunction(async ([b, k, m]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return !p.sessions.some((x) => x.machine === m) && p.steps.some((s) => s.session?.machine === m && s.updated_at > s.created_at);
  }, [base, KEY, `ap-box-${n}`]);
  ok(true, `${tag} Start next task makes a step of the idle session`);

  await card1.getByRole("button", { name: "Mark read" }).click();
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="task-summary"]').length === 1);
  await page.waitForFunction(() => /\b1\b/.test([...document.querySelectorAll('[role="tab"]')].find((e) => e.textContent.startsWith("Autopilot"))?.textContent ?? ""), null, { timeout: 5000 }).catch(() => {});
  ok(/\b1\b/.test(await unread.innerText()), `${tag} the tab counts 1 after marking one read`);
  await page.getByRole("radio", { name: "All" }).click();
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="task-summary"]').length >= 2);
  const read1 = page.getByTestId("task-summary").filter({ hasText: k1 });
  ok((await read1.getAttribute("data-read")) === "true", `${tag} All shows the read one, marked read`);
  await read1.getByRole("button", { name: "Mark unread" }).click();
  await page.waitForFunction((k) => [...document.querySelectorAll('[data-testid="task-summary"]')].find((e) => e.textContent.includes(k))?.dataset.read === "false", k1);
  ok(true, `${tag} a summary can be marked unread again`);
  await page.getByRole("radio", { name: "Unread" }).click();
  await page.getByRole("button", { name: "Mark all read" }).filter({ visible: true }).click();
  await page.getByText("Nothing unread").waitFor();
  ok(!/\d/.test((await unread.innerText()).replace("Autopilot", "")), `${tag} Mark all read clears the tab's count`);
  await shot("read");
  await overflow("all read");

  {
    const ready = await api("POST", "/issues", { project: KEY, title: `Own start of ${KEY}`, agent: "claude" });
    const own = new DatabaseSync(`${process.env.DIR}/wp.db`);
    own.exec("PRAGMA busy_timeout = 5000");
    const uid = own.prepare("SELECT id FROM users WHERE username = 'robin'").get().id;
    const pid = own.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
    const iid = own.prepare("SELECT id FROM issues WHERE project_id = ? AND number = ?").get(pid, Number(ready.key.split("-")[1])).id;
    const at = Date.now();
    const add = own.prepare(`INSERT INTO autopilot_steps (user_id, project_id, session_id, stop_at, state, reason, detail, due_at, next_issue, job_id, created_at, updated_at)
      VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)`);
    add.run(uid, pid, at, "waiting", "runner", "no runner of yours can start a session for it", at, null, null, at, at);
    add.run(uid, pid, at, "starting", "", "", at, iid, 987654, at, at);
    own.close();
    await page.reload();
    const ownSteps = row.getByTestId("autopilot-step").filter({ hasText: "A session of its own" });
    await ownSteps.nth(1).waitFor();
    const starting = ownSteps.filter({ has: page.getByRole("button", { name: "Stop" }) });
    ok((await starting.count()) === 1, `${tag} a start under way can be stopped, one that waits has nothing to stop`);
    ok((await row.getByTestId("autopilot-step").filter({ hasText: "A session of its own" }).getByRole("button", { name: "Start now" }).count()) === 0, `${tag} no Start now without a session`);
    console.log(`     ${tag} own steps: ${(await ownSteps.allInnerTexts()).join(" | ").replace(/\s+/g, " ")}`);
    await shot("own-start");
    await overflow("own start");
    await starting.getByRole("button", { name: "Stop" }).click();
    await page.waitForFunction(async ([b, k]) => {
      const o = await (await fetch(b + "/api/v1/autopilot")).json();
      return !o.projects.find((x) => x.project_key === k).steps.some((s) => !s.session && s.state === "starting");
    }, [base, KEY]);
    ok(true, `${tag} Stop ends autopilot's own start`);
  }

  await page.goto(`${base}/agents?tab=autopilot&project=${KEY}`);
  await row.waitFor();
  ok((await page.locator('[data-testid^="autopilot-A"]').count()) === 1, `${tag} the project filter narrows the project list`);
  await page.getByRole("switch", { name: `Autopilot in ${KEY}` }).click();
  await page.waitForFunction((k) => document.querySelector(`[data-testid="autopilot-${k}"] [role=switch]`)?.getAttribute("aria-checked") === "false", KEY);
  const o = await api("GET", "/autopilot");
  const p = o.projects.find((x) => x.project_key === KEY);
  ok(!p.enabled && !p.steps.some((s) => s.state === "due" || s.state === "waiting"), `${tag} turning it off leaves no live step`);
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\nautopilot suite passed");
