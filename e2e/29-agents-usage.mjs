// Usage analytics (WP-D20), in a real browser at desktop and phone widths,
// light and dark: before anything is reported the Usage tab teaches how to
// start; then a month of usage, tools, refusals and quota samples (as `wp
// usage sync` and `wp statusline` upload them) fills the tab: quota gauges
// with pace, tokens by day and model, the cache split, sortable breakdowns,
// tools with unused ones, Bash command heads, refusals with allow rules and
// suggestions with their evidence. The Trends tab (WP-196) draws any measure
// by day and repo with instruction changes marked, and compares usage before
// and after one.
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
const HOUR = 3600_000;
const DAY = 24 * HOUR;

const browser = await chromium.launch();

// repo, model, kind, machine, share of a day's work
const WORK = [
  ["acme/web", "claude-opus-5-5", "main", "devhost", 1],
  ["acme/web", "claude-opus-5-5", "Explore", "devhost", 0.5],
  ["acme/web", "claude-sonnet-5-5", "general-purpose", "devhost", 0.4],
  ["acme/api", "claude-opus-5-5", "main", "prodhost", 0.7],
  ["acme/api", "claude-haiku-5-5", "Explore", "prodhost", 0.3],
];

async function seed(api, call) {
  const now = Date.now();
  const hourOf = (t) => t - (t % HOUR);
  const chunks = [];
  let n = 0;
  for (const [repo, model, kind, machine, share] of WORK) {
    const messages = [];
    const tools = [];
    const ledger = [];
    for (let d = 0; d < 28; d++) {
      for (const hr of [3, 5, 9]) {
        const h = hourOf(now - d * DAY - hr * HOUR);
        const s = `${repo.split("/")[1]}-${kind}-${d % 4}`;
        const k = share * (1 + ((d * 7 + hr) % 5) / 5);
        messages.push({
          h,
          s,
          repo,
          model,
          kind,
          n: Math.round(12 * k),
          in: Math.round(30000 * k),
          out: Math.round(26000 * k),
          cw5: Math.round(110000 * k),
          cw1: 0,
          cr:
            Math.round(2400000 * k) +
            (kind === "main" && d % 9 === 0 ? 0 : 1_000_000),
        });
        const calls = Math.round(12 * k);
        const avg = 180_000 + (28 - d) * 6_000 + (kind === "main" ? 60_000 : 0);
        ledger.push({
          h,
          s,
          repo,
          model,
          kind,
          n: calls,
          ctx: calls * avg,
          o200: Math.round(calls * Math.min(1, avg / 330_000)),
          o300: Math.round(calls * Math.max(0, (avg - 200_000) / 200_000)),
          cr300: Math.round(calls * Math.max(0, (avg - 220_000) * 9)),
          cold: d % 5 === 0 ? 1 : 0,
          cold_w5: d % 5 === 0 ? 120_000 : 0,
          ...(kind === "main"
            ? {}
            : d % 3 === 0
              ? { forks: 1, fork_ctx: avg }
              : { fresh: 1, fresh_ctx: 25_000 }),
        });
        tools.push(
          {
            h,
            s,
            repo,
            model,
            kind: "builtin",
            tool: "Bash",
            calls: Math.round(20 * k),
            errors: Math.round(2 * k),
            refusals: d % 6 === 0 ? 1 : 0,
            bytes: 60000 * Math.round(k + 1),
          },
          {
            h,
            s,
            repo,
            model,
            kind: "builtin",
            tool: "Read",
            calls: Math.round(40 * k),
            errors: 1,
            bytes: 400000,
          },
          {
            h,
            s,
            repo,
            model,
            kind: "bash",
            tool: "Bash",
            detail: "git status",
            calls: Math.round(9 * k),
            bytes: 2000,
          },
          {
            h,
            s,
            repo,
            model,
            kind: "bash",
            tool: "Bash",
            detail: "git push",
            calls: 2,
            prompts: 2,
            refusals: d % 6 === 0 ? 1 : 0,
          },
          {
            h,
            s,
            repo,
            model,
            kind: "bash",
            tool: "Bash",
            detail: "go test",
            calls: Math.round(5 * k),
            errors: Math.round(k),
            bytes: 90000,
          },
          {
            h,
            s,
            repo,
            model,
            kind: "mcp",
            tool: "mcp__notion__search",
            detail: "notion",
            calls: 2,
            bytes: 30000,
          },
          { h, s, repo, model, kind: "skill", detail: "claude-api", calls: 1 },
          { h, s, repo, model, kind: "command", detail: "/review", calls: 1 },
          {
            h,
            s,
            repo,
            model,
            kind: "subagent",
            detail: kind === "main" ? "Explore" : "general-purpose",
            calls: 1,
          },
        );
      }
    }
    chunks.push({
      machine,
      id: `e2e-${n++}`,
      ledger,
      messages,
      tools,
      sessions: [0, 1, 2, 3].map((i) => ({
        s: `${repo.split("/")[1]}-${kind}-${i}`,
        repo,
        branch: "main",
        version: "2.1.293",
        cwd: `/work/${repo}`,
        title:
          i === 0 && kind === "main"
            ? `Refactor the ${repo.split("/")[1]} pipeline`
            : "",
        first: now - 20 * DAY,
        last: now - i * HOUR,
        compactions: i === 0 && kind === "main" ? 4 : 0,
      })),
      refusals: [0, 1, 2, 3, 4, 5].map((i) => ({
        t: now - i * 5 * HOUR,
        s: `${repo.split("/")[1]}-${kind}-0`,
        repo,
        model,
        tool: "Bash",
        detail: i % 3 === 0 ? "git push" : "kubectl apply",
        reason: i % 3 === 0 ? "Production Deploy" : "External System Writes",
        source: "classifier",
        excerpt:
          i % 3 === 0
            ? "git push origin main --force-with-lease"
            : "kubectl apply -f deploy/prod.yaml",
      })),
    });
  }
  chunks[0].refusals.push({
    t: now - HOUR,
    s: "web-main-0",
    repo: "acme/web",
    model: "claude-opus-5-5",
    tool: "Bash",
    detail: "curl",
    reason: "hook: guard.py",
    source: "hook",
    excerpt: "curl -H 'Authorization: ***' https://example.com",
  });
  const cliHour = hourOf(now - 3 * HOUR);
  const cliOf = (s, repo, days) =>
    [
      { kind: "file" },
      { kind: "bash", n: 60 },
      { kind: "wp", head: "wp next", n: 3 + days },
      { kind: "wp", head: "wp ls", n: 6 },
      { kind: "wp", head: "wp show", n: 8 },
      { kind: "help", head: "wp ls", n: 2 },
      { kind: "flag", head: "wp ls", detail: "--search", n: 2 },
      { kind: "error", head: "wp ls", detail: "unknown_flag", n: 2 },
      { kind: "unknown_flag", head: "wp ls", detail: "--search", n: 2 },
      { kind: "unknown_command", head: "wp babysit", detail: "wp babysit" },
      { kind: "error", head: "wp babysit", detail: "unknown_command" },
      { kind: "follow", head: "wp next", detail: "git fetch", n: 2 },
      { kind: "follow", head: "wp next", detail: "wp show", n: 1 },
      { kind: "signal", detail: "sleep", n: 3 },
      { kind: "signal", detail: "curl_forge" },
    ].map((r) => ({
      h: cliHour - days * 7 * 24 * HOUR,
      s,
      repo,
      model: "claude-opus-5-5",
      agent: "main",
      n: 1,
      ...r,
    }));
  chunks[0].cli = [
    ...cliOf("web-main-0", "acme/web", 0),
    ...cliOf("web-main-1", "acme/web", 1),
    ...cliOf("api-main-0", "acme/api", 0),
    ...cliOf("api-main-1", "acme/api", 2),
  ];
  const send = (machine, list, extra = {}) =>
    call("POST", "/usage/ingest", {
      machine,
      version: "test",
      chunks: list.map(({ machine: _m, ...c }) => c),
      ...extra,
    });
  const r = await send(
    "devhost",
    chunks.filter((c) => c.machine === "devhost"),
    {
      parsed: 40000,
      skipped: 12,
      unknown: { "frame-link": 12 },
      inventory: [
        { kind: "mcp", name: "notion" },
        { kind: "mcp", name: "drive-old" },
        { kind: "skill", name: "claude-api" },
        { kind: "skill", name: "never-used-skill" },
        { kind: "command", name: "review" },
        { kind: "command", name: "stale-cmd" },
        { kind: "agent", name: "Explore" },
      ],
    },
  );
  if (!r.ok()) throw new Error(`ingest: ${r.status()} ${await r.text()}`);
  const d = await send(
    "prodhost",
    chunks.filter((c) => c.machine === "prodhost"),
  );
  if (!d.ok())
    throw new Error(`ingest prodhost: ${d.status()} ${await d.text()}`);
  const reset7 = now + 3 * DAY;
  const reset5 = now + 90 * 60_000;
  for (const [span, pct, reset] of [
    ["seven_day", 71, reset7],
    ["five_hour", 86, reset5],
  ]) {
    const q = await call("POST", "/usage/quota", {
      machine: "devhost",
      session: "web-main-0",
      model: "claude-opus-5-5",
      [span]: { percent: pct, resets_at: reset },
    });
    if (!q.ok()) throw new Error(`quota: ${q.status()}`);
  }
  // history behind it (the API only takes samples from now)
  await api("POST", "/projects", { name: "Usage", key: "USG" });
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const uid = db
    .prepare("SELECT id FROM users WHERE username = 'robin'")
    .get().id;
  const start7 = reset7 - 7 * DAY;
  const ins = db.prepare(
    "INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine, session, model) VALUES (?,?,?,?,?,?,?,?)",
  );
  for (let i = 1; i <= 16; i++) {
    const at = start7 + (i * (4 * DAY - HOUR)) / 16;
    ins.run(
      uid,
      at,
      "seven_day",
      ((71 * (at - start7)) / (now - start7)) * 0.98,
      reset7,
      "devhost",
      "",
      "claude-opus-5-5",
    );
  }
  for (const wk of [1, 2, 3])
    for (let i = 1; i <= 4; i++)
      ins.run(
        uid,
        reset7 - wk * 7 * DAY - (5 - i) * DAY,
        "seven_day",
        30 + wk * 12 + i * 8,
        reset7 - wk * 7 * DAY,
        "devhost",
        "",
        "",
      );
  db.prepare(
    "UPDATE usage_machines SET last_sync_at = ? WHERE machine = 'prodhost'",
  ).run(now - 3 * DAY);
  const forge = db
    .prepare(
      `INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Usage forge', 'http://127.0.0.1:1', ?) RETURNING id`,
    )
    .get(now).id;
  // On this suite's own project: a repo on WAYP is one more than suites run
  // after this one in the same lane expect (79-objectives' dialog then asks
  // which repository, and its Set objective stays open).
  const project = db
    .prepare("SELECT id FROM projects WHERE key = 'USG'")
    .get().id;
  db.prepare(
    "INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/web', ?)",
  ).run(project, forge, now);
  const held = db.prepare(
    "INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?,?,?,?,?,?)",
  );
  const sessions = chunks.flatMap((c) =>
    c.sessions.map((x) => [c.machine, x.s]),
  );
  for (let i = 0; i < 10; i++) {
    const issue = await api("POST", "/issues", {
      project: "USG",
      title: `Usage task ${i + 1}`,
    });
    for (const [machine, s] of sessions.filter((_, j) => j % 10 === i))
      held.run(uid, machine, s, issue.id, now - 25 * DAY, now);
  }
  db.prepare(
    `INSERT INTO usage_marks(forge_id, repo, sha, at, author, title, url, paths) VALUES (?,?,?,?,?,?,?,?)`,
  ).run(
    forge,
    "acme/web",
    "abc123",
    now - 10 * DAY,
    "robin",
    "Trim CLAUDE.md to the essentials",
    "",
    '["CLAUDE.md"]',
  );
  db.close();
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(300);
    const tall = await page.evaluate(() => {
      const panes = [...document.querySelectorAll(".overflow-y-auto")];
      panes.forEach((e) => e.scrollTo(0, 0));
      return Math.max(...panes.map((e) => e.scrollHeight));
    });
    await page.setViewportSize({
      width,
      height: Math.min(Math.max(900, tall + 120), 9000),
    });
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
    await page.setViewportSize({ width, height: 900 });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const before = await api("GET", "/usage/overview?days=30");
  if (!before.has_data) {
    await page.goto(`${base}/agents?tab=usage`);
    await page.getByText("No usage reported yet").waitFor();
    ok(
      await page.getByText("wp usage sync --all").first().isVisible(),
      `${tag} the empty state teaches the commands`,
    );
    ok(
      await page.getByText("wp hook claude").first().isVisible(),
      `${tag} ... and the hook`,
    );
    await shot("usage-empty");
    await overflow("empty");
    await seed(api, call);
  }

  const o = await api("GET", "/usage/overview?days=30");
  ok(
    o.totals.messages > 1000 &&
      o.refusals.total > 20 &&
      o.suggestions.length >= 4,
    `${tag} API: ${o.totals.messages} messages, ${o.refusals.total} refusals, ${o.suggestions.length} suggestions`,
  );

  await page.goto(`${base}/agents?tab=usage`);
  await page.getByText("Fresh tokens").first().waitFor();
  await page.getByRole("meter", { name: /7 days quota used/ }).waitFor();
  ok(
    (await page
      .getByRole("meter", { name: /7 days quota used/ })
      .getAttribute("aria-valuenow")) === "71",
    `${tag} the 7-day gauge says 71%`,
  );
  ok(
    await page
      .getByText(/Will run out/)
      .first()
      .isVisible(),
    `${tag} the pace line warns`,
  );
  ok(
    await page.getByRole("meter", { name: /5 hours quota used/ }).isVisible(),
    `${tag} the 5-hour gauge shows`,
  );
  await page
    .getByRole("group", { name: /Seven-day quota used over the window/ })
    .waitFor();
  ok(true, `${tag} the week chart renders`);
  await page
    .getByRole("group", { name: "Fresh tokens per day by model family" })
    .waitFor();
  ok(true, `${tag} tokens by day render`);
  const led = page.getByTestId("token-ledger");
  await led.getByText("Context per call", { exact: true }).first().waitFor();
  ok(
    await led
      .getByText(/X4: 279k/)
      .first()
      .isVisible(),
    `${tag} the ledger shows the X4 baseline`,
  );
  await page
    .getByRole("group", { name: "Average context per call by day" })
    .waitFor();
  await page
    .getByRole("group", { name: "Share of calls over 300k by day" })
    .waitFor();
  ok(
    await led
      .getByRole("heading", { name: "Forks and fresh subagents" })
      .isVisible(),
    `${tag} forks and fresh subagents are compared`,
  );
  ok(
    await led
      .getByRole("heading", { name: "Per task, session and machine" })
      .isVisible(),
    `${tag} per task, session and machine`,
  );
  await led.getByRole("radio", { name: "Machine" }).click();
  await led.getByRole("cell", { name: "prodhost" }).waitFor();
  ok(true, `${tag} the ledger lists machines`);
  await led.getByRole("radio", { name: "Session" }).click();
  await led.getByText("Refactor the web pipeline").first().waitFor();
  ok(true, `${tag} ... and sessions with their titles`);
  const lg = await api("GET", "/usage/ledger?days=30");
  ok(
    lg.overall.calls > 500 &&
      lg.overall.cold_starts > 0 &&
      lg.spawns[0].forks > 0 &&
      lg.spawns[0].fresh > 0,
    `${tag} API ledger: ${lg.overall.calls} calls, avg ${Math.round(lg.overall.avg_context)}, ${lg.overall.cold_starts} cold`,
  );
  await led.screenshot({ path: `${out}/ledger-${width}-${scheme}.png` });
  const coldTile = await page
    .getByText(/^Cold starts \(inferred\)$/)
    .first()
    .evaluate((el) => ({
      cut: el.scrollWidth > el.clientWidth + 1,
      shown: el.getBoundingClientRect().width > 0,
    }));
  ok(
    coldTile.shown && !coldTile.cut,
    `${tag} the cold-starts tile label shows "inferred" whole`,
  );
  ok(
    (await page.getByText("Cache hit rate").first().isVisible()) &&
      (await page.getByText("API-equivalent cost").first().isVisible()),
    `${tag} cache and cost tiles show`,
  );
  ok(
    await page
      .getByText("Search subagents are running on a premium model")
      .isVisible(),
    `${tag} the model suggestion shows`,
  );
  ok(
    (await page.getByText("Never-used-skill").count()) +
      (await page.getByText("never-used-skill").count()) >
      0,
    `${tag} unused skill is listed`,
  );
  await shot("usage");
  const secs = page.locator("section");
  for (let i = 0; i < Math.min(await secs.count(), 14); i++)
    await secs
      .nth(i)
      .screenshot({
        path: `${out}/usage-section-${String(i).padStart(2, "0")}-${width}-${scheme}.png`,
      });
  await overflow("usage");

  await page
    .locator("section", { hasText: "Where it went" })
    .getByRole("radio", { name: "Machine" })
    .click();
  const table = page
    .locator("section", { hasText: "Where it went" })
    .locator("table");
  await table.getByRole("cell", { name: "prodhost" }).waitFor();
  ok(
    await table.getByRole("cell", { name: "devhost" }).isVisible(),
    `${tag} machines are listed`,
  );
  const cost = table
    .getByRole("columnheader", { name: /API cost/ })
    .getByRole("button");
  await cost.click();
  const first = async () =>
    (await table.locator("tbody tr").first().innerText()).split("\n")[0];
  const f1 = await first();
  await cost.click();
  await page.waitForTimeout(150);
  ok(
    (await first()) !== f1,
    `${tag} clicking a header again reverses the order (${f1} first, then ${await first()})`,
  );
  await page
    .locator("section", { hasText: "Where it went" })
    .getByRole("radio", { name: "Session" })
    .click();
  await page.getByText("Refactor the web pipeline").first().waitFor();
  ok(true, `${tag} sessions carry their titles`);

  ok(
    await page
      .getByRole("heading", { name: "MCP servers", exact: true })
      .isVisible(),
    `${tag} MCP servers are grouped`,
  );
  ok(
    (await page.getByText("drive-old").first().isVisible()) &&
      (await page.getByText("stale-cmd").first().isVisible()),
    `${tag} unused MCP servers and commands are listed`,
  );
  ok(
    await page.getByRole("cell", { name: "git push" }).first().isVisible(),
    `${tag} Bash heads show`,
  );
  ok(
    await page.getByText("Production Deploy").first().isVisible(),
    `${tag} refusal reasons show`,
  );
  ok(
    await page
      .getByRole("button", { name: "Copy allow rule" })
      .first()
      .isVisible(),
    `${tag} an allow rule can be copied`,
  );
  ok(
    await page.getByText("no rule can allow it").first().isVisible(),
    `${tag} hook refusals offer no rule`,
  );
  ok(
    (await page
      .getByText(/unread lines/)
      .first()
      .isVisible()) &&
      (await page
        .getByText(/never synced|synced 3 days ago/)
        .first()
        .isVisible()),
    `${tag} machines show when they last synced`,
  );

  await page
    .locator("section", { hasText: "Where it went" })
    .getByRole("radio", { name: "Session" })
    .click();
  await page
    .locator("section", { hasText: "Where it went" })
    .getByRole("row")
    .filter({ hasText: "Refactor the web pipeline" })
    .first()
    .click();
  await page.waitForURL(/session=web-main-0/);
  await page.getByText(/Session: Refactor the web pipeline/).waitFor();
  const one = await api("GET", "/usage/overview?days=30&session=web-main-0");
  ok(one.sessions === 1, `${tag} one session in the API answer`);
  await shot("usage-session");
  await page.getByRole("button", { name: "Show every session" }).click();
  await page.waitForFunction(() => !location.search.includes("session="));

  const tr = await api("GET", "/usage/trends?days=30");
  ok(
    tr.has_data &&
      tr.repos.length >= 2 &&
      tr.marks.some((m) => m.kind === "instructions") &&
      tr.marks.some((m) => m.kind === "cli"),
    `${tag} API trends: ${tr.dates.length} days, ${tr.repos.length} repos, ${tr.marks.length} marks, ${tr.factors.length} factors`,
  );
  await page.goto(`${base}/agents?tab=trends`);
  await page.getByRole("group", { name: "Cost per day" }).waitFor();
  ok(true, `${tag} the cost chart renders`);
  const over = page.locator("section", { hasText: "Over time" });
  await over.getByRole("radio", { name: "By repo" }).click();
  await page
    .getByRole("group", { name: "Cost per day by repository" })
    .waitFor();
  ok(
    await over.getByText("acme/web").first().isVisible(),
    `${tag} by repo, with a legend`,
  );
  await over.getByRole("combobox", { name: "Measure" }).click();
  await page.getByRole("option", { name: "Average context" }).click();
  await page.getByRole("group", { name: "Average context per day" }).waitFor();
  ok(
    !(await over
      .getByRole("radio", { name: "By repo" })
      .isVisible()
      .catch(() => false)),
    `${tag} an average has no repo split`,
  );
  ok(
    await over
      .getByRole("listitem")
      .filter({ hasText: "Trim CLAUDE.md to the essentials" })
      .isVisible(),
    `${tag} the instruction change is listed`,
  );
  await shot("trends");
  await over
    .getByRole("listitem")
    .filter({ hasText: "Trim CLAUDE.md" })
    .getByRole("button", { name: "Compare" })
    .click();
  const cmp = page.locator("section", { hasText: "Before and after" });
  await cmp.getByRole("cell", { name: "Tool calls" }).waitFor();
  ok(
    (
      await cmp.getByRole("combobox", { name: "Repository" }).innerText()
    ).includes("acme/web"),
    `${tag} comparing on the change's repo`,
  );
  const sessRows = await cmp.locator("tbody tr").count();
  await cmp.getByRole("radio", { name: "Per day" }).click();
  await cmp.getByRole("cell", { name: "Sessions" }).waitFor();
  ok(
    sessRows >= 8,
    `${tag} per session: ${sessRows} measures; per day shows too`,
  );
  const c = await api(
    "GET",
    `/usage/compare?at=${encodeURIComponent(tr.marks.find((m) => m.kind === "instructions").at)}&window=7`,
  );
  ok(
    c.daily[0].before.n === 7 &&
      c.daily[0].after.n === 7 &&
      c.sessions[0].before.n > 0,
    `${tag} API compare: ${c.daily[0].before.n}/${c.daily[0].after.n} days, ${c.sessions[0].before.n}/${c.sessions[0].after.n} sessions`,
  );
  const tasks = page.locator("section", { hasText: "tied to a task" });
  await tasks
    .getByRole("link", { name: /Usage task 1$/ })
    .first()
    .waitFor();
  ok(tr.tasks.length === 10, `${tag} ten tasks listed (${tr.tasks.length})`);
  const factors = page.getByRole("list", {
    name: "Traits by how closely they rank with cost",
  });
  ok(
    (await factors.getByRole("listitem").count()) >= 3,
    `${tag} traits are ranked against cost`,
  );
  await cmp.screenshot({
    path: `${out}/trends-compare-${width}-${scheme}.png`,
  });
  await overflow("trends");

  const cli = await api("GET", "/usage/cli?days=30");
  ok(
    cli.has_data &&
      cli.total.wp > 0 &&
      cli.commands.some((c) => c.head === "wp next") &&
      cli.repos.length === 2,
    `${tag} API cli: ${cli.total.wp} wp calls, ${cli.commands.length} commands, ${cli.repos.length} repos`,
  );
  await page.goto(`${base}/agents?tab=cli`);
  await page
    .getByRole("group", { name: /share of wp calls/i })
    .first()
    .waitFor();
  ok(true, `${tag} the wp CLI charts render`);
  const follows = page.locator("section", { hasText: "What follows" });
  ok(
    await follows.getByText("git fetch").first().isVisible(),
    `${tag} what follows wp next`,
  );
  ok(
    await page.getByText("--search").first().isVisible(),
    `${tag} the unknown flag is listed`,
  );
  ok(
    await page.getByText("wp babysit").first().isVisible(),
    `${tag} the unknown command is listed`,
  );
  await page
    .getByRole("row", { name: /acme\/web/ })
    .first()
    .click();
  await page.getByRole("button", { name: /acme\/web/ }).waitFor();
  ok(true, `${tag} a repo narrows the page`);
  await shot("cli");
  await overflow("cli");

  await page.goto(`${base}/admin?tab=ai`);
  await page.getByRole("heading", { name: "Prices" }).waitFor();
  await page
    .getByRole("button", { name: "Edit claude-opus-5-5", exact: true })
    .waitFor();
  ok(true, `${tag} the seeded prices are listed`);
  if (tag.startsWith("1440/light")) {
    await page.getByRole("button", { name: "Add price" }).click();
    await page.getByLabel("Model", { exact: true }).fill("gpt-e2e");
    await page.getByLabel(/^Input, /).fill("2.5");
    await page.getByLabel(/^Output, /).fill("10");
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "Save" })
      .click();
    await page.getByText("gpt-e2e", { exact: true }).waitFor();
    ok(true, `${tag} a price can be added`);
    await page.getByRole("button", { name: "Remove gpt-e2e" }).click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "Remove", exact: true })
      .click();
    await page
      .getByText("gpt-e2e", { exact: true })
      .waitFor({ state: "detached" });
    ok(true, `${tag} and removed`);
  }
  await shot("admin-prices");
  await overflow("prices");

  await page.goto(`${base}/agents?tab=usage&agent=haiku`);
  await page.getByText("Fresh tokens").first().waitFor();
  ok(
    !(await page
      .getByText("claude-sonnet-5-5")
      .first()
      .isVisible()
      .catch(() => false)),
    `${tag} only haiku after picking it`,
  );
  await page.goto(`${base}/agents?tab=usage&agent=gemini`);
  await page.getByText(/No usage in the last 30 days/).waitFor();
  ok(true, `${tag} a filter that matches nothing says so`);
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
