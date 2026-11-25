// Review stats (WP-371): Admin → AI shows how review findings held up, per
// reviewer model × strategy × prompt version and per verifier, and the
// verifier on against off. Seeds two reviews into the suite's database.
// Desktop and phone widths, light and dark.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

// Two reviews of one pull request: one verified (a finding kept and taken by
// an agent, one dropped and posted anyway then dismissed, one the evidence
// check dropped), one without a verifier (a fixed finding and a dismissed one).
const seed = (key) => {
  const db = new DatabaseSync(path.join(path.dirname(out), "wp.db"));
  db.exec("PRAGMA busy_timeout = 5000");
  const now = Date.now();
  const forge = db
    .prepare(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Stats forge', 'https://git.example.com', ?) RETURNING id`)
    .get(now).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(key).id;
  db.prepare("INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/stats', ?)").run(project, forge, now);
  const pr = db
    .prepare(
      `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, state, opened_at, created_at, updated_at)
       VALUES (?, 'acme/stats', 7, 'Stats', 'robin', 'https://git.example.com/acme/stats/pulls/7', 'open', ?, ?, ?) RETURNING id`,
    )
    .get(forge, now, now, now).id;
  const run = (arm, strategy, verified) =>
    db
      .prepare(
        `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at, strategy, prompt_version, arm, verified,
          reviewer_models)
         VALUES (?, ?, 'review', 'abc1234def', 'done', ?, ?, 'p3', ?, ?, '["groq/kimi-k3"]') RETURNING id`,
      )
      .get(project, pr, now, strategy, arm, verified).id;
  const finding = (r, strategy, state, verify, by, reason = "") =>
    db
      .prepare(
        `INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, verified_by, state, placement, ordinal,
          created_at, updated_at, strategy, prompt_version, verify, verify_reason)
         VALUES (?, ?, 'x.go', 1, 'high', 'c', 'groq/kimi-k3', ?, ?, 'inline', 1, ?, ?, ?, 'p3', ?, ?) RETURNING id`,
      )
      .get(r, pr, by, state, now, now, strategy, verify, reason).id;
  const v = run("diff+files/verify", "diff+files", 1);
  const kept = finding(v, "diff+files", "open", "kept", "openrouter/glm-5");
  db.prepare(
    `INSERT INTO review_comments(pr_id, kind, ext_id, finding_id, verdict, created_at, updated_at, seen_at) VALUES (?, 'finding', 'e2e1', ?, 'accepted', ?, ?, ?)`,
  ).run(pr, kept, now, now, now);
  finding(v, "diff+files", "dismissed", "dropped", "openrouter/glm-5");
  finding(v, "diff+files", "dropped", "unchecked", "", "evidence not in the file");
  const s = run("diff/none", "diff", 0);
  finding(s, "diff", "fixed", "skipped", "");
  finding(s, "diff", "dismissed", "skipped", "");
  db.close();
};

const runs = [[1440, "light"], [390, "light"], [390, "dark"]];
for (const [n, [width, scheme]] of runs.entries()) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  await login(page);
  const api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });

  if (n === 0) {
    const r = await api("POST", "/projects", { name: "Review stats", key: "RVST" });
    ok(r.ok(), `project made: ${r.status()}`);
    seed("RVST");
    const st = await (await api("GET", "/reviews/stats?project=RVST")).json();
    ok(st.totals.findings === 5 && st.totals.useful === 2 && st.totals.false === 2, `totals add up: ${JSON.stringify(st.totals)}`);
    ok(st.verifiers.length === 1 && st.verifiers[0].dropped === 1 && st.verifiers[0].drop_accuracy === 1, `the verifier's drop counts as right: ${JSON.stringify(st.verifiers)}`);
    const text = await (await api("GET", "/reviews/stats?project=RVST&format=text")).text();
    ok(/Reviewers \(model/.test(text) && /drops right/.test(text), `the text form reads: ${text.slice(0, 200)}`);
  }

  const totalsLine = async (days) => {
    const { totals: t } = await (await api("GET", `/reviews/stats?days=${days}`)).json();
    return `${t.findings} findings from ${t.runs} reviews`;
  };

  await page.goto(`${base}/admin?tab=ai`);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Review stats" }) });
  await section.getByTestId("review-stats").waitFor();
  const rows = section.locator('li[data-reviewer="kimi-k3"]');
  ok((await rows.count()) === 2, `${tag} a row per reviewer strategy: ${await rows.count()}`);
  const totals = await totalsLine(30);
  ok(await section.getByText(totals).isVisible(), `${tag} the totals show: ${totals}`);
  const ver = section.locator('li[data-verifier="glm-5"]');
  ok(await ver.getByText(/drops right/).isVisible(), `${tag} the verifier's drops show`);
  ok(await section.getByText("Kept by the verifier").isVisible(), `${tag} verifier on against off shows`);
  if (n === 0) {
    await section.getByRole("combobox", { name: "Review stats window" }).click();
    await page.getByRole("option", { name: "90 days", exact: true }).click();
    await section.getByText(await totalsLine(90)).waitFor();
    ok(true, `${tag} the window changes`);
  }
  await section.scrollIntoViewIfNeeded();
  await overflow("admin ai");
  await section.screenshot({ path: `${out}/stats-${width}-${scheme}.png` });
  await shot("stats-page");
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
