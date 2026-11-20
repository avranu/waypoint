// Lineage (WP-275): an agent's task records the task its session was working
// on; the issue page and `wp show` name the chain, and from depth 3 the
// safety screen holds the task for a person (WP-ADR83). A review up the chain shortens
// it. Desktop and phone widths.
// Needs user robin (run 01-signup-flow first).
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
const KEY = "LINE";

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

let root, t1, t2, t3;
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  await api("POST", "/projects", { name: "Lineage", key: KEY });
  root = await api("POST", "/issues", { project: KEY, title: "Speed up the board" });
  t1 = await api("POST", "/issues", { project: KEY, title: "Cache the board query" });
  t2 = await api("POST", "/issues", { project: KEY, title: "Also add an index" });
  t3 = await api("POST", "/issues", { project: KEY, title: "Vacuum the database after the index" });
  // As if agent A wrote t1 while working on the person's task, agent B
  // wrote t2 while working on t1, and agent C t3 while working on t2
  // (sessions are what record it; the core tests cover that).
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const set = db.prepare(`UPDATE issues SET text_agent = ?, reporter_agent = ?, reviewed_at = NULL, origin_issue_id = ?, origin_depth = ? WHERE id = ?`);
  set.run("anthropic/claude-opus-5-5", "anthropic/claude-opus-5-5", root.id, 0, t1.id);
  set.run("anthropic/claude-sonnet-5-5", "anthropic/claude-sonnet-5-5", t1.id, 1, t2.id);
  set.run("anthropic/claude-haiku-5-5", "anthropic/claude-haiku-5-5", t2.id, 2, t3.id);
  db.close();

  const d3 = await api("GET", `/issues/${t3.key}`);
  ok(d3.lineage?.depth === 3 && d3.lineage.held && d3.lineage.chain?.[0]?.key === t2.key && d3.lineage.chain?.[1]?.key === t1.key && d3.lineage.chain?.[2]?.key === root.key,
    `t3 is at depth 3, held: ${JSON.stringify(d3.lineage)}`);
  ok(d3.safety?.verdict === "hold" && /depth 3/.test(d3.safety.reason ?? ""), `the screen holds t3 by rule: ${JSON.stringify(d3.safety)}`);
  const d2 = await api("GET", `/issues/${t2.key}`);
  ok(d2.lineage?.depth === 2 && !d2.lineage.held && d2.lineage.chain?.[0]?.key === t1.key, `t2 is at depth 2, not held: ${JSON.stringify(d2.lineage)}`);
  const d1 = await api("GET", `/issues/${t1.key}`);
  ok(d1.lineage?.depth === 1 && !d1.lineage.held, `t1 is at depth 1: ${JSON.stringify(d1.lineage)}`);
  const dr = await api("GET", `/issues/${root.key}`);
  ok(!dr.lineage, "a person's task has no lineage");
  const text = await page.request.fetch(`${base}/api/v1/issues/${t3.key}?format=text`, { headers: H }).then((r) => r.text());
  ok(text.includes(`Written while working on ${t2.key}`) && text.includes("depth 3"), `wp show names the chain:\n${text}`);
  await ctx.close();
}

for (const [width, scheme] of looks([[1440, "light"], [390, "light"], [390, "dark"]])) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  await login(page);
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  const note = page.getByTestId("lineage-note");

  await page.goto(`${base}/issue/${t3.key}`);
  await note.waitFor();
  const n3 = await note.innerText();
  ok(/Written while working on/.test(n3) && n3.includes(t2.key) && n3.includes(t1.key) && n3.includes(root.key) && /depth 3/.test(n3), `${tag} t3 names its chain: ${n3}`);
  await note.getByRole("link", { name: t1.key }).click();
  await page.waitForURL(`${base}/issue/${t1.key}`);
  // the note re-renders in place: wait for t1's, not t3's still on screen
  await note.getByText(/depth 1/).waitFor().catch(() => {});
  ok(/depth 1/.test(await note.innerText()), `${tag} the link opens t1, at depth 1`);
  await page.goto(`${base}/issue/${t3.key}`);
  await note.waitFor();
  await overflow("t3");
  await shot("held");

  await page.goto(`${base}/issue/${root.key}`);
  await page.getByText(root.title).first().waitFor();
  ok((await note.count()) === 0, `${tag} a person's task shows no chain`);
  await ctx.close();
}

// A person's review of t1 shortens t3's chain and releases it.
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  await page.request.fetch(`${base}/api/v1/review/issue/${t1.key}`, { method: "POST", headers: H });
  const d3 = await page.request.fetch(`${base}/api/v1/issues/${t3.key}`, { headers: H }).then((r) => r.json());
  ok(d3.lineage?.depth === 2 && !d3.lineage.held && d3.lineage.chain?.[1]?.reviewed, `after t1's review t3 is at depth 2: ${JSON.stringify(d3.lineage)}`);
  ok(d3.safety?.verdict !== "hold", `and no longer held: ${JSON.stringify(d3.safety)}`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
