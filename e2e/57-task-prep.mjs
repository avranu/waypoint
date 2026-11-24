// Task prep (WP-212, WP-ADR8): a runner's code bundle is read by the prep
// model (a tiny OpenAI-compatible server run here), only picks that are in
// the bundle are kept, the project switch is on by default, and Agents →
// Usage compares closed tasks that were shown a prep, held out, or not
// prepared; a paid fallback (WP-243) is marked in Admin → AI. Desktop and
// phone widths, light and dark.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";
import http from "node:http";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const HOUR = 3600_000;

let prepCalls = 0;
const fake = http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const reply = (obj) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify(obj));
    };
    if (req.url.endsWith("/models"))
      return reply({ data: [{ id: "fake-prep" }] });
    const msgs = JSON.parse(body || "{}").messages ?? [];
    const prompt = msgs.find((m) => m.role === "user")?.content ?? "";
    let content = '{"points": 3, "confidence": "high", "reason": "Small."}';
    if (prompt.startsWith("Task ")) {
      prepCalls++;
      content = JSON.stringify({
        files: [
          { path: "internal/merge.go", why: "the gate" },
          { path: "made/up.go", why: "invented" },
        ],
        symbols: [
          { path: "internal/merge.go", name: "MergeReady", why: "decides" },
        ],
        tests: ["internal/merge_test.go"],
        related: [],
        terms: [],
        questions: ["Is a WIP: title a draft?"],
      });
    }
    reply({ choices: [{ message: { content } }] });
  });
});
await new Promise((r) => fake.listen(0, "127.0.0.1", r));
const fakeURL = `http://127.0.0.1:${fake.address().port}`;

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

// Setup, once: the model, a project, a prepared task, and a ledger of three
// closed tasks: one shown a prep, one held out, one not prepared.
const KEY = "TPREP";
let providerId;
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  const call = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers: H,
      data: d,
    });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  providerId = (
    await api("POST", "/ai/providers", {
      kind: "openai",
      name: "fake prep",
      base_url: fakeURL,
    })
  ).id;
  for (const task of ["prep", "estimate"])
    await api("PUT", `/ai/tasks/${task}`, {
      provider_id: providerId,
      model: "fake-prep",
      fallbacks: [],
    });
  await api("POST", "/projects", { name: "Task prep", key: KEY });
  const proj = await api("GET", `/projects/${KEY}`);
  ok(proj.settings.auto_prep === true, "a new project prepares tasks");
  const issues = [];
  for (const t of ["Refuse to merge drafts", "Held out task", "Never prepared"])
    issues.push(await api("POST", "/issues", { project: KEY, title: t }));

  const bundle = {
    repo: "robin/app",
    commit: "abc123def4567890",
    machine: "e2ebox",
    files: [
      {
        path: "internal/merge.go",
        lines: 120,
        why: ["named in the task"],
        symbols: [{ name: "MergeReady", kind: "func", start: 10, end: 40 }],
      },
      { path: "internal/merge_test.go", lines: 40, why: ["tests"] },
    ],
    tests: ["internal/merge_test.go"],
  };
  const up = await call("POST", `/issues/${issues[0].key}/prep`, bundle);
  ok(up.ok(), `uploading a bundle: ${up.status()}`);
  let prep;
  for (let n = 0; n < 50; n++) {
    prep = await api("GET", `/issues/${issues[0].key}/prep`);
    if (prep.state !== "pending") break;
    await page.waitForTimeout(100);
  }
  ok(
    prep.state === "ready" && prepCalls === 1,
    `the model read it: ${prep.state}, ${prepCalls} call`,
  );
  ok(
    prep.notes?.files.length === 1 && prep.dropped === 1,
    `only picks in the bundle kept: ${JSON.stringify(prep.notes?.files)} dropped ${prep.dropped}`,
  );
  ok(
    prep.notes?.questions[0] === "Is a WIP: title a draft?",
    "its questions are kept",
  );

  const now = Date.now();
  const h = now - (now % HOUR) - HOUR;
  const chunk = (s, k) => ({
    id: `prep-${s}`,
    messages: [
      {
        h,
        s,
        repo: "robin/app",
        model: "claude-opus-5-5",
        kind: "main",
        n: 10 * k,
        in: 20000 * k,
        out: 9000 * k,
        cw5: 50000 * k,
        cw1: 0,
        cr: 900000 * k,
      },
    ],
    ledger: [
      {
        h,
        s,
        repo: "robin/app",
        model: "claude-opus-5-5",
        kind: "main",
        n: 10 * k,
        ctx: 10 * k * 150000,
        o200: 0,
        o300: 0,
        cr300: 0,
        cold: 0,
        cold_w5: 0,
      },
    ],
    tools: [
      {
        h,
        s,
        repo: "robin/app",
        model: "claude-opus-5-5",
        kind: "builtin",
        tool: "Bash",
        calls: 12 * k,
        bytes: 1000,
      },
    ],
    sessions: [
      {
        s,
        repo: "robin/app",
        branch: "main",
        version: "2.1.293",
        cwd: "/work/app",
        title: "",
        first: h,
        last: h + 60_000,
        compactions: 0,
      },
    ],
  });
  const ing = await call("POST", "/usage/ingest", {
    machine: "e2ebox",
    version: "test",
    chunks: [chunk("tp-a", 1), chunk("tp-b", 2), chunk("tp-c", 3)],
  });
  ok(ing.ok(), `usage ingested: ${ing.status()}`);
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const uid = db
    .prepare("SELECT id FROM users WHERE username = 'robin'")
    .get().id;
  const sess = db.prepare(
    "INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?, 'e2ebox', ?, ?, ?, ?)",
  );
  ["tp-a", "tp-b", "tp-c"].forEach((s, i) =>
    sess.run(uid, s, issues[i].id, h, h),
  );
  db.prepare("UPDATE issues SET resolved_at = ? WHERE id IN (?, ?, ?)").run(
    now - 60_000,
    issues[0].id,
    issues[1].id,
    issues[2].id,
  );
  db.prepare(
    "UPDATE task_preps SET shown_at = ?, held_out = 0 WHERE issue_id = ?",
  ).run(h, issues[0].id);
  db.prepare(
    "INSERT INTO task_preps(issue_id, text_hash, state, bundle, held_out, shown_at, created_at, updated_at) VALUES (?, 'x', 'ready', '{}', 1, ?, ?, ?)",
  ).run(issues[1].id, h, h, h);
  db.close();
  const lg = await api("GET", "/usage/ledger?days=7");
  const g = Object.fromEntries((lg.prep ?? []).map((x) => [x.group, x]));
  ok(
    g.shown?.done === 1 && g.held_out?.done === 1 && g.none?.done >= 1,
    `ledger groups: ${JSON.stringify(Object.values(g).map((x) => [x.group, x.done, x.tool_calls]))}`,
  );
  ok(
    g.shown?.tool_calls === 12 && g.held_out?.tool_calls === 24,
    "tool calls per group",
  );
  await ctx.close();
}

for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      !/Failed to load resource/.test(m.text()) &&
      fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  await login(page);
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(`${base}/p/${KEY}/settings`);
  const sw = page.getByRole("switch", { name: "Prepare tasks for agents" });
  await sw.waitFor();
  ok(
    (await sw.getAttribute("aria-checked")) === "true",
    `${tag} the settings switch is on`,
  );
  await sw.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `${out}/settings-${width}-${scheme}.png` });
  await overflow("settings");

  await page.goto(`${base}/agents?tab=usage`);
  const sec = page.getByRole("table", { name: "Closed tasks by task prep" });
  await sec.waitFor();
  for (const label of ["Prepared, shown", "Prepared, held out", "Not prepared"])
    ok(
      await sec.getByText(label).isVisible(),
      `${tag} the prep table has "${label}"`,
    );
  await sec.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `${out}/usage-prep-${width}-${scheme}.png` });
  await overflow("usage");

  await page.request.put(`${base}/api/v1/ai/tasks/prep`, {
    headers: H,
    data: {
      provider_id: providerId,
      model: "fake-prep",
      fallbacks: [{ provider_id: providerId, model: "fake-haiku" }],
    },
  });
  await page.goto(`${base}/admin?tab=ai`);
  const paid = page.getByRole("button", { name: "Paid: fake-haiku" });
  await paid.waitFor();
  ok(
    (await paid.getAttribute("aria-pressed")) === "false",
    `${tag} a fallback starts free`,
  );
  await paid.click();
  await page.waitForFunction(
    () =>
      document
        .querySelector('[aria-label="Paid: fake-haiku"]')
        ?.getAttribute("aria-pressed") === "true",
  );
  const saved = await (
    await page.request.get(`${base}/api/v1/ai/tasks`, { headers: H })
  ).json();
  const fb = (Array.isArray(saved) ? saved : saved.tasks).find(
    (t) => t.task === "prep",
  ).fallbacks;
  ok(
    fb[0]?.paid === true,
    `${tag} the paid mark is saved: ${JSON.stringify(fb)}`,
  );
  await paid.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: `${out}/admin-ai-paid-${width}-${scheme}.png`,
  });
  await overflow("admin ai");
  await ctx.close();
}

{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  await page.request.delete(`${base}/api/v1/ai/providers/${providerId}`, {
    headers: H,
  });
  await ctx.close();
}
await browser.close();
fake.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
