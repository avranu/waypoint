// A session's adversarial review on the page (WP-752) in a real browser at
// desktop and phone widths, light and dark: Agents → Sessions and the session
// page say a review is queued, and that one failed and why, without sideways
// scroll on a long reason.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
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
const why = "no runner took Grok's audit within 15 minutes, and Waypoint opened the pull request without it";

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const KEY = `HR${"ABCDEFGH"[n]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const api = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  const overflow = () => page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("POST", "/projects", { name: `Held review ${n}`, key: KEY });
  const issue = await api("POST", "/issues", { project: KEY, title: "Keep the review going" });

  const { token } = await api("POST", "/me/tokens", { name: `held ${tag}`, agent: "anthropic/claude-opus-5-5" });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const start = (sid) =>
    api("POST", "/sessions/hook", { events: [{ event: "SessionStart", session_id: sid, machine: "box", cwd: "/work/app" }] }, AG);
  const queuedCli = `e2e-held-q-${n}`;
  const failedCli = `e2e-held-f-${n}`;
  await start(queuedCli);
  await start(failedCli);
  const auditCli = `e2e-held-a-${n}`;
  const runningCli = `e2e-held-r-${n}`;
  await start(auditCli);
  await start(runningCli);

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const id = (cli) => db.prepare(`SELECT id FROM agent_sessions WHERE cli_id = ?`).get(cli).id;
  const issueID = db.prepare(`SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.key = ? AND i.number = ?`).get(KEY, Number(issue.key.split("-")[1])).id;
  const now = Date.now();
  const round = db.prepare(
    `INSERT INTO work_log(issue_id, kind, state, agent, prompt, session_id, reason, before_pr, created_at, updated_at)
     VALUES (?, 'review', ?, 'claude', 'Attack this.', ?, ?, '{"repo":"a/b"}', ?, ?)`,
  );
  round.run(issueID, "pending", id(queuedCli), "", now - 60_000, now - 60_000);
  round.run(issueID, "failed", id(failedCli), why, now - 60_000, now - 30_000);
  round.run(issueID, "pending", id(auditCli), "", now - 90_000, now - 90_000);
  round.run(issueID, "running", id(runningCli), "", now - 125_000, now - 125_000);
  const auditRound = db.prepare(`SELECT id FROM work_log WHERE session_id = ?`).get(id(auditCli)).id;
  const projectID = db.prepare(`SELECT id FROM projects WHERE key = ?`).get(KEY).id;
  const userID = db.prepare(`SELECT id FROM users WHERE username = 'robin'`).get().id;
  db.prepare(
    `INSERT INTO autopilot_steps(user_id, project_id, session_id, stop_at, state, reason, detail, due_at, created_at, updated_at, review_id)
     VALUES (?, ?, ?, ?, 'waiting', 'audit', 'waiting', ?, ?, ?, ?)`,
  ).run(userID, projectID, id(auditCli), now, now + 120_000, now, now, auditRound);
  const queuedRound = db.prepare(`SELECT id FROM work_log WHERE session_id = ?`).get(id(queuedCli)).id;
  db.prepare(
    `INSERT INTO autopilot_steps(user_id, project_id, session_id, stop_at, state, reason, detail, due_at, created_at, updated_at, review_id)
     VALUES (?, ?, ?, ?, 'due', '', '', ?, ?, ?, ?)`,
  ).run(userID, projectID, id(queuedCli), now, now + 3_600_000, now, now, queuedRound);
  const queuedID = id(queuedCli);
  const failedID = id(failedCli);
  db.close();

  await page.goto(`${base}/agents?tab=sessions&state=all`);
  const chips = page.getByTestId("session-review");
  await chips.filter({ hasText: "Review queued" }).first().waitFor();
  await chips.filter({ hasText: "Review failed" }).first().waitFor();
  ok(((await chips.filter({ hasText: "Review failed" }).first().getAttribute("title")) || "").includes(why), `${tag} the failed chip says why`);
  const rgb = (c) => c.evaluate((el) => getComputedStyle(el.firstElementChild).backgroundColor);
  const blue = (color) =>
    page.evaluate((color) => {
      const g = Object.assign(document.createElement("canvas"), { width: 1, height: 1 }).getContext("2d");
      g.fillStyle = "#fff";
      g.fillRect(0, 0, 1, 1);
      g.fillStyle = color;
      g.fillRect(0, 0, 1, 1);
      const [r, gr, b] = g.getImageData(0, 0, 1, 1).data;
      return b > gr && gr > r;
    }, color);
  const failedChip = chips.filter({ hasText: "Review failed" }).first();
  const working = chips.filter({ hasText: "Reviewing" }).first();
  await working.waitFor();
  const queuedAudit = chips.filter({ hasText: "Audit queued" }).first();
  await queuedAudit.waitFor();
  ok(/rgb\((37, 99, 235|96, 165, 250)\)/.test(await rgb(working)), `${tag} a review being worked on is solid blue: ${await rgb(working)}`);
  ok(await blue(await rgb(queuedAudit)), `${tag} a queued audit is pale blue: ${await rgb(queuedAudit)}`);
  ok(!(await blue(await rgb(failedChip))), `${tag} a failed review isn't blue: ${await rgb(failedChip)}`);
  const clock = working.getByTestId("session-review-clock");
  const first = await clock.textContent();
  const [mins, secs] = first.split(":").map(Number);
  ok(/^\d+:\d\d$/.test(first) && mins * 60 + secs >= 125, `${tag} the clock counts from when the review was triggered: ${first}`);
  await page.waitForTimeout(2100);
  ok((await clock.textContent()) !== first, `${tag} the clock runs`);
  ok((await chips.filter({ hasText: "Review failed" }).first().getByTestId("session-review-clock").count()) === 0, `${tag} a failed review has no clock`);
  ok(!(await overflow()), `${tag} the sessions list has no sideways scroll`);
  await shot("list");

  await page.goto(`${base}/agents/sessions/${queuedID}`);
  await page.getByTestId("session-review").filter({ hasText: `Review queued on ${issue.key}` }).waitFor();
  ok(!(await overflow()), `${tag} a queued review's page has no sideways scroll`);

  await page.goto(`${base}/agents/sessions/${failedID}`);
  const failed = page.getByTestId("session-review").filter({ hasText: `Review failed on ${issue.key}` });
  await failed.waitFor();
  ok((await failed.textContent()).includes(why), `${tag} the session page gives the reason`);
  ok(!(await overflow()), `${tag} a failed review's page has no sideways scroll`);
  await shot("failed");
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
