// The safety screen (WP-270, WP-ADR10): a task with agent-written text no
// person reviewed waits on the Agents page under "Agent-written, not
// cleared" with what to check; the issue says it is held from agents;
// "Mark reviewed" releases it. No screen model here, so the rules decide.
// Desktop and phone widths, light and dark.
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
const KEY = "SAFE";

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

// One held task per run (each run reviews its own), and one that clears.
const runs = looks([[1440, "light"], [390, "light"], [390, "dark"]]);
const held = [];
let plain;
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  await api("POST", "/projects", { name: "Safety", key: KEY });
  for (const n of runs.keys())
    held.push(
      await api("POST", "/issues", {
        project: KEY,
        title: `Run task containers under gVisor ${n + 1}`,
        description: "farhost has only runc. Run `sudo apt-get install runsc` there and register it in /etc/docker/daemon.json.",
        agent: "claude",
      }),
    );
  plain = await api("POST", "/issues", { project: KEY, title: "Tidy the queue code", agent: "claude" });
  // As if an agent wrote them (agents act through a person's token).
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const ids = [...held, plain].map((i) => i.id);
  db.prepare(`UPDATE issues SET text_agent = 'anthropic/claude-opus-5-5', reporter_agent = 'anthropic/claude-opus-5-5', reviewed_at = NULL WHERE id IN (${ids.join(",")})`).run();
  db.close();
  const q = await api("GET", `/agents/queue?project=${KEY}`);
  ok(q.next?.some((it) => it.issue.key === plain.key), "the agent-written task without commands is in the queue");
  const w = q.waiting?.find((it) => it.issue.key === held[0].key);
  ok(w?.reason === "safety" && /runs as root/.test(w.detail), `the held one waits with why: ${w?.reason} ${w?.detail}`);
  const s = await api("GET", `/search?q=${encodeURIComponent("is:held")}&project=${KEY}`);
  ok(Array.isArray(s.issues), "is:held searches");
  await ctx.close();
}

for (const [n, [width, scheme]] of runs.entries()) {
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

  const task = held[n];
  await page.goto(`${base}/issue/${task.key}`);
  const banner = page.getByText(/Held from agents: runs as root/);
  await banner.waitFor();
  ok(await banner.isVisible(), `${tag} the issue says it is held from agents`);
  await overflow("issue");
  await shot("issue");

  await page.goto(`${base}/agents?tab=queue&project=${KEY}`);
  const group = page.getByRole("heading", { name: /Agent-written, not cleared/ });
  await group.waitFor();
  const row = page.locator("li", { hasText: task.title });
  ok(await row.getByText(/Held for a person: runs as root/).isVisible(), `${tag} the queue says what to check`);
  await overflow("queue");
  await shot("queue");
  await row.getByRole("button", { name: "Mark reviewed" }).click();
  await page.getByText(`${task.key} is reviewed: agents can take it`).waitFor();
  // Released: out of the waiting group, into what agents take next.
  await page.locator("li.cursor-grab", { hasText: task.title }).waitFor();
  ok(true, `${tag} reviewing releases it`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
