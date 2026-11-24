// What agents did (WP-D15), in a real browser at desktop and phone widths,
// light and dark: agents take delegated tasks through their tokens, report
// tokens with the model their transcript named, link pull requests and
// finish; review bots answer. The Agents page's Activity tab shows the
// totals, agent time per day, the agents grouped by family, the review
// bots' models and the latest runs with their pull requests, and picking an
// agent narrows it.
// Run after 01-signup-flow (user robin, an admin). The runs' times are moved
// into the past in the suite's own database, so the history has some shape.
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
const KEY = "AACT";
const DAY = 86400_000;
const MIN = 60_000;

// title, delegated to, which agent takes it, its model, how it ends, days
// ago, minutes worked, tokens in/out, cost, its pull request
const TASKS = [
  ["Parse the webhook payloads", "claude", "code", "claude-opus-5-5", "done", 12, 95, 182000, 9100, 2.41, 101],
  ["Retry failed deliveries", "claude", "code", "claude-opus-5-5", "done", 9, 140, 240000, 15800, 3.62, 102],
  ["Document the CLI flags", "haiku", "haiku", "", "done", 8, 25, 41000, 3900, 0.12, 103],
  ["Fix the timeline drag", "any", "codex", "gpt-5.1-codex", "done", 6, 70, 99000, 7000, 0.88, 104],
  ["Speed up search", "claude", "code", "claude-sonnet-5-5", "released", 5, 40, 61000, 2100, 0.4, 0],
  ["Board keyboard shortcuts", "haiku", "haiku45", "", "done", 3, 35, 52000, 4100, 0.16, 105],
  ["Agent queue explainer", "claude", "code", "claude-opus-5-5", "done", 1, 80, 133000, 8800, 1.92, 106],
  ["Trace slow writes", "claude", "code", "claude-opus-5-5", "working", 0, 25, 38000, 1200, 0.55, 0],
];

const browser = await chromium.launch();

async function seed(api, call) {
  const projects = await api("GET", "/projects");
  if (projects.some((p) => p.key === KEY)) return;
  await api("POST", "/projects", { name: "Agent activity", key: KEY });
  const tokens = {};
  for (const [name, agent] of [
    ["code", "anthropic/claude-code"],
    ["haiku", "anthropic/claude-haiku-5-5"],
    ["haiku45", "anthropic/claude-haiku-4-5"],
    ["codex", "openai/codex"],
  ]) {
    const { token } = await api("POST", "/me/tokens", { name: `activity ${name}`, agent });
    tokens[name] = { ...H, Authorization: `Bearer ${token}` };
  }
  const as = (who, m, p, d) => call(m, p, d, tokens[who]);
  const keys = [];
  for (const [title, spec, who, model, end, , , tin, tout, cost, pr] of TASKS) {
    const issue = await api("POST", "/issues", { project: KEY, title, agent: spec, priority: "high" });
    keys.push(issue.key);
    const took = await as(who, "POST", `/agent/take/${issue.key}`);
    if (!took.ok()) throw new Error(`take ${issue.key}: ${took.status()} ${await took.text()}`);
    await as(who, "POST", `/agent/usage/${issue.key}`, {
      input_tokens: tin,
      output_tokens: tout,
      cost_usd: cost,
      ...(model ? { model } : {}),
    });
    const link = () =>
      pr &&
      as(who, "POST", `/issues/${issue.key}/links`, {
        url: `https://github.com/acme/waypoint/pull/${pr}`,
        title,
      });
    if (end === "done") {
      await as(who, "POST", `/issues/${issue.key}/comments`, { body: `Done; PR #${pr} is up.` });
      await as(who, "POST", `/agent/done/${issue.key}`);
      await link();
    } else if (end === "released") {
      await link();
      await as(who, "POST", `/agent/release/${issue.key}`, { comment: "Needs a decision first." });
    } else await link();
  }
  await as("haiku", "POST", "/issues", {
    project: KEY,
    title: "Shortcuts clash with the browser's",
  });

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const now = Date.now();
  const forge = db
    .prepare(
      `INSERT INTO forges(kind, name, base_url, created_at) VALUES ('github', 'GitHub', 'https://github.com', ?) RETURNING id`,
    )
    .get(now).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
  TASKS.forEach(([, , , , end, daysAgo, minutes, , , , pr], n) => {
    const issue = db
      .prepare("SELECT id FROM issues WHERE project_id = ? AND number = ?")
      .get(project, n + 1).id;
    const start = now - daysAgo * DAY - minutes * MIN - 2 * 3600_000 * (n % 3);
    const stop = end === "working" ? null : start + minutes * MIN;
    db.prepare("UPDATE agent_runs SET started_at = ?, ended_at = ? WHERE issue_id = ?").run(start, stop, issue);
    if (end === "working")
      db.prepare("UPDATE issues SET claimed_at = ? WHERE id = ?").run(start, issue);
    db.prepare("UPDATE comments SET created_at = ? WHERE issue_id = ?").run(start + minutes * MIN, issue);
    if (pr) {
      const url = `https://github.com/acme/waypoint/pull/${pr}`;
      const merged = pr !== 106 && stop ? stop + 3 * 3600_000 : null;
      db.prepare(
        `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, state, opened_at, merged_at, created_at, updated_at)
         VALUES (?, 'acme/waypoint', ?, ?, 'robin', ?, ?, ?, ?, ?, ?)`,
      ).run(forge, pr, TASKS[n][0], url, merged ? "merged" : "open", start + 20 * MIN, merged, start, now);
      db.prepare("UPDATE external_links SET state = ? WHERE url = ?").run(merged ? "merged" : "open", url);
    }
  });
  const prID = db.prepare("SELECT id FROM pull_requests WHERE number = 106").get().id;
  for (const [daysAgo, log, findings] of [
    [
      1,
      [
        { model: "opencode/muse-spark-1.3-contributor-free", outcome: "rate_limit", ms: 2100 },
        { model: "opencode/nemotron-3-ultra-free", outcome: "ok", ms: 48000 },
      ],
      ["opencode/nemotron-3-ultra-free", "opencode/nemotron-3-ultra-free"],
    ],
    [4, [{ model: "opencode/muse-spark-1.3-contributor-free", outcome: "ok", ms: 31000 }], []],
    [7, [{ model: "opencode/muse-spark-1.3-contributor-free", outcome: "ok", ms: 27000 }], ["opencode/muse-spark-1.3-contributor-free"]],
  ]) {
    const at = now - daysAgo * DAY;
    const run = db
      .prepare(
        `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, log, summary, created_at, started_at, finished_at)
         VALUES (?, ?, 'review', 'abc1234', 'done', ?, 'reviewed', ?, ?, ?) RETURNING id`,
      )
      .get(project, prID, JSON.stringify(log), at, at, at + 60000).id;
    for (const model of findings)
      db.prepare(
        `INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, created_at) VALUES (?, ?, 'app/main.go', 12, 'high', 'Nil check inverted', ?, ?)`,
      ).run(run, prID, model, at);
  }
  db.close();
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
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
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  // The app scrolls inside the app shell, not the window, so the window is resized to fit the whole page.
  const shot = async (name) => {
    await page.waitForTimeout(300);
    const tall = await page.evaluate(() => {
      const panes = [...document.querySelectorAll(".overflow-y-auto")];
      panes.forEach((e) => e.scrollTo(0, 0));
      return Math.max(...panes.map((e) => e.scrollHeight));
    });
    await page.setViewportSize({ width, height: Math.min(Math.max(900, tall + 120), 6000) });
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
    await page.setViewportSize({ width, height: 900 });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await seed(api, call);

  const act = await api("GET", `/agents/activity?project=${KEY}`);
  ok(
    act.totals.tasks_done === 6 && act.totals.prs_merged === 5 && act.totals.bot_reviews === 3,
    `${tag} API totals: ${act.totals.tasks_done} done, ${act.totals.prs_merged} merged, ${act.totals.bot_reviews} bot reviews`,
  );

  await page.goto(`${base}/agents?tab=activity&project=${KEY}`);
  await page.getByText("Tasks finished").waitFor();
  const tile = page.locator("dl > div", { hasText: "Tasks finished" });
  ok((await tile.locator("dd").first().innerText()) === "6", `${tag} tasks tile says 6`);
  await page.getByRole("group", { name: /Agent time per day/ }).waitFor();
  ok(true, `${tag} the daily chart renders`);
  ok(
    await page
      .getByText("anthropic/claude-code", { exact: true })
      .filter({ visible: true })
      .first()
      .isVisible(),
    `${tag} claude-code is listed`,
  );
  ok(
    await page.getByText(/on claude-opus-5-5/).filter({ visible: true }).first().isVisible(),
    `${tag} its model shows`,
  );
  ok(
    await page.getByText("nemotron-3-ultra-free").first().isVisible(),
    `${tag} review bot models show`,
  );
  ok(
    await page.getByText("Trace slow writes").first().isVisible(),
    `${tag} the running task is in the latest runs`,
  );
  ok(
    (await page.locator('a[href="https://github.com/acme/waypoint/pull/101"]').count()) > 0,
    `${tag} a run links its pull request`,
  );
  await shot("activity");
  await overflow("activity");

  const tasks = (n) =>
    page.waitForFunction(
      (n) =>
        [...document.querySelectorAll("dl > div")]
          .find((d) => d.textContent.includes("Tasks finished"))
          ?.querySelector("dd")?.textContent === n,
      n,
    );
  await page
    .getByRole("button", { name: /^claude-haiku\s*family$/ })
    .filter({ visible: true })
    .first()
    .click();
  await page.waitForURL(/agent=claude-haiku(&|$)/);
  await tasks("2");
  ok(true, `${tag} the claude-haiku family covers both versions`);

  await page
    .getByRole("button", { name: "anthropic/claude-haiku-5-5" })
    .filter({ visible: true })
    .first()
    .click();
  await page.waitForURL(/agent=anthropic%2Fclaude-haiku-5-5|agent=anthropic\/claude-haiku-5-5/);
  await page.waitForFunction(
    () =>
      [...document.querySelectorAll("dl > div")]
        .find((d) => d.textContent.includes("Tasks finished"))
        ?.querySelector("dd")?.textContent === "1",
  );
  ok(
    !(await page.getByText("Trace slow writes").isVisible()),
    `${tag} other agents' runs are gone once haiku is picked`,
  );
  await shot("activity-haiku");

  await page.goto(`${base}/agents?tab=activity&project=${KEY}&agent=gemini`);
  await page.getByText(/No agent work in the last 30 days/).waitFor();
  ok(await page.getByText("wp hook claude").isVisible(), `${tag} the empty state teaches`);
  await shot("activity-empty");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s);
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
