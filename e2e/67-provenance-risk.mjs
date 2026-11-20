// Provenance risk (WP-554): an issue an agent wrote reads medium until a
// person reviews it, one the safety screen held reads high, and a person's
// own reads low; the issue page shows the level and its factors, the API
// gives the reasons, and WQL risk: finds them. An automation whose rule an
// agent wrote reads medium in the list and the editor until a person saves
// it (WP-560). Desktop and phone widths.
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
const KEY = "RISK";

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

let mine, agents, held, auto;
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  await api("POST", "/projects", { name: "Risk", key: KEY });
  mine = await api("POST", "/issues", { project: KEY, title: "A person's own task" });
  agents = await api("POST", "/issues", { project: KEY, title: "A task an agent filed" });
  held = await api("POST", "/issues", { project: KEY, title: "A task the screen held" });
  auto = await api("POST", `/projects/${KEY}/automations`, {
    name: "Tidy branches",
    enabled: false,
    rule: { trigger: { event: "manual" }, actions: [{ type: "notify", user: "owners", message: "Tidy the branches." }] },
  });
  // As if an agent wrote them (agents act through a person's token), and
  // the screen held one.
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  db.prepare(`UPDATE issues SET text_agent = 'anthropic/claude-opus-5-5', reporter_agent = 'anthropic/claude-opus-5-5', reviewed_at = NULL WHERE id IN (${agents.id}, ${held.id})`).run();
  db.prepare(`INSERT INTO task_safety(issue_id, text_hash, verdict, reason, created_at, updated_at) VALUES (?, 'h', 'hold', 'Asks for the deploy key.', 0, 0)
    ON CONFLICT(issue_id) DO UPDATE SET verdict = 'hold', reason = excluded.reason`).run(held.id);
  db.prepare(`UPDATE automations SET rule_agent = 'anthropic/claude-opus-5-5' WHERE id = ?`).run(auto.id);
  db.close();

  const r = await api("GET", `/risk/issue/${held.key}`);
  ok(r.level === "high" && r.reasons?.[0]?.factor === "held", `the held task is high: ${JSON.stringify(r)}`);
  const m = await api("GET", `/risk/issue/${agents.key}`);
  ok(m.level === "medium" && m.reasons?.[0]?.factor === "agent_text", `the agent's task is medium: ${JSON.stringify(m)}`);
  const l = await api("GET", `/risk/issue/${mine.key}`);
  ok(l.level === "low" && !l.reasons, `a person's task is low: ${JSON.stringify(l)}`);
  const s = await api("GET", `/search?q=${encodeURIComponent("risk:>=medium")}&project=${KEY}`);
  const keys = (s.issues ?? []).map((i) => i.key).sort();
  ok(JSON.stringify(keys) === JSON.stringify([agents.key, held.key].sort()), `risk:>=medium finds both: ${keys}`);
  const a = await api("GET", `/risk/automation/${auto.id}`);
  ok(a.level === "medium" && /wrote its rule/.test(a.reasons?.[0]?.text), `the agent's automation is medium: ${JSON.stringify(a)}`);
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

  await page.goto(`${base}/issue/${held.key}`);
  const note = page.getByTestId("risk-note");
  await note.waitFor();
  await note.getByText(/held by the safety screen/).waitFor().catch(() => {});
  ok(/High risk/.test(await note.innerText()) && /held by the safety screen/.test(await note.innerText()), `${tag} the held task says high risk and why`);
  await overflow("held");
  await shot("held");

  await page.goto(`${base}/issue/${agents.key}`);
  await note.waitFor();
  await note.getByText(/unreviewed agent text/).waitFor().catch(() => {});
  ok(/Medium risk/.test(await note.innerText()) && /unreviewed agent text/.test(await note.innerText()), `${tag} the agent's task says medium risk`);
  await overflow("agent");
  await shot("agent");

  await page.goto(`${base}/issue/${mine.key}`);
  await page.getByText(mine.title).first().waitFor();
  ok((await note.count()) === 0, `${tag} a person's task shows no risk`);

  await page.goto(`${base}/p/${KEY}/settings?tab=automations`);
  const row = page.locator("div.rounded-xl", { has: page.getByRole("button", { name: "Tidy branches", exact: true }) }).last();
  await row.getByTestId("risk-note").waitFor();
  ok(/Medium risk/.test(await row.getByTestId("risk-note").innerText()), `${tag} the agent's automation says medium risk`);
  await overflow("automations");
  await shot("automations");
  await row.getByRole("button", { name: "Tidy branches", exact: true }).click();
  const editorNote = page.getByTestId("rule-agent-note");
  await editorNote.waitFor();
  ok(/save the rule yourself/.test(await editorNote.innerText()), `${tag} the editor says how to make the rule yours`);
  await overflow("editor");
  await shot("automation-editor");
  await page.keyboard.press("Escape");
  await ctx.close();
}

// A person saving the rule makes it theirs.
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  await page.goto(`${base}/p/${KEY}/settings?tab=automations`);
  await page.getByRole("button", { name: "Tidy branches", exact: true }).click();
  await page.getByTestId("rule-agent-note").waitFor();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  const r = await page.request.fetch(`${base}/api/v1/risk/automation/${auto.id}`, { headers: H }).then((x) => x.json());
  ok(r.level === "low", `saved by a person, the automation is low: ${JSON.stringify(r)}`);
  await page.getByTestId("risk-note").waitFor({ state: "detached" });
  ok((await page.getByTestId("risk-note").count()) === 0, "the list no longer shows a risk");
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
