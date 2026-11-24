// Related issues without a model (WP-296): "Find related" on an issue lists
// what its text says it waits for, the same title and much the same words,
// and adds one in a click, or says one isn't related so it isn't suggested
// again from either end (WP-958); the project setting to relate new issues
// automatically shows with no AI set up. Run after 01-signup-flow (user
// robin, an admin).
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

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then(async (r) => {
      const t = await r.text();
      if (r.status() >= 300) throw new Error(`${m} ${p}: ${r.status()} ${t}`);
      try { return JSON.parse(t); } catch { return t; }
    });
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `RL${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Related ${tag}`, key });
  for (const t of ["Login form styling", "Billing invoice emails", "Calendar export feed", "Avatar upload sizes"])
    await api("POST", "/issues", { project: key, title: t, description: `Unrelated work on ${t.toLowerCase()}.` });
  const waits = await api("POST", "/issues", { project: key, title: "Rate limits for map downloads" });
  const alike = await api("POST", "/issues", { project: key, title: "Weekly walk stops early on production",
    description: "On production the weekly Sanborn walk stops at a campus town and never resumes." });
  const twin = await api("POST", "/issues", { project: key, title: "Sanborn county sheets skipped in campus towns",
    description: "The weekly Sanborn walk skips the county sheets of a campus town." });
  const src = await api("POST", "/issues", { project: key, title: "Sanborn walk: campus towns",
    description: `Production's weekly Sanborn walk reaches a campus town and never resumes. Blocked by ${waits.key}.` });

  // The description names a task, so the Relations section is already showing.
  await page.goto(`${base}/issue/${src.key}`);
  await page.getByRole("button", { name: "Find related" }).click();
  const dialog = page.getByRole("dialog", { name: "Related issues" });
  await dialog.locator("li", { hasText: alike.key }).waitFor();
  ok(await dialog.getByText("is blocked by").isVisible(), `${tag} the stated dependency is listed`);
  ok(await dialog.getByText(/^Shares /).first().isVisible(), `${tag} a reason names the shared words`);
  await page.screenshot({ path: `${out}/find-related-${width}-${scheme}.png`, fullPage: true });
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await dialog.locator("li", { hasText: waits.key }).getByRole("button", { name: "Add" }).click();
  await dialog.locator("li", { hasText: waits.key }).getByRole("button", { name: "Added" }).waitFor();
  const rels = (await api("GET", `/issues/${src.key}`)).relations ?? [];
  ok(rels.some((r) => r.kind === "blocked_by" && r.issue.key === waits.key), `${tag} Add links it`);
  const twinRow = dialog.locator("li", { hasText: twin.key });
  await twinRow.getByRole("button", { name: "Not related" }).click();
  await twinRow.getByText("Not related", { exact: true }).waitFor();
  ok(!(await twinRow.getByRole("button", { name: "Add" }).isVisible()), `${tag} a dismissed row has no Add`);
  await twinRow.getByRole("button", { name: "Undo" }).click();
  await twinRow.getByRole("button", { name: "Add" }).waitFor();
  const undone = (await api("GET", `/issues/${src.key}/relations/suggested`)).suggestions.map((x) => x.key);
  ok(undone.includes(twin.key), `${tag} an undone dismissal is suggested again: ${undone}`);
  await twinRow.getByRole("button", { name: "Not related" }).click();
  await twinRow.getByRole("button", { name: "Undo" }).waitFor();
  ok(!(await dialog.locator("li", { hasText: waits.key }).getByRole("button", { name: "Not related" }).isVisible()), `${tag} an added row can't be dismissed`);
  await page.screenshot({ path: `${out}/find-related-decided-${width}-${scheme}.png`, fullPage: true });
  await dialog.getByRole("button", { name: "Done" }).click();
  const again = (await api("GET", `/issues/${src.key}/relations/suggested`)).suggestions.map((x) => x.key);
  ok(!again.includes(twin.key) && again.includes(alike.key), `${tag} the dismissed one stays out, the rest stay: ${again}`);
  const back = (await api("GET", `/issues/${twin.key}/relations/suggested`)).suggestions.map((x) => x.key);
  ok(!back.includes(src.key), `${tag} and from the other end: ${back}`);

  await api("POST", `/issues/${alike.key}/relations`, { kind: "follows", issue: src.key });
  await page.goto(`${base}/issue/${alike.key}`);
  await page.getByText("Follows up on", { exact: true }).waitFor();
  ok(await page.getByRole("link", { name: new RegExp(src.key) }).first().isVisible(), `${tag} the follow-up names what it follows`);
  await page.screenshot({ path: `${out}/follow-up-${width}-${scheme}.png`, fullPage: true });
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll on a follow-up`);
  await page.goto(`${base}/issue/${src.key}`);
  await page.getByText("Followed up by", { exact: true }).waitFor();

  await page.goto(`${base}/p/${key}/settings`);
  await page.getByText("Relate new issues automatically").first().waitFor();
  ok(await page.getByRole("switch", { name: "Relate new issues automatically" }).isVisible(), `${tag} the auto-relate setting shows without AI`);
  await page.screenshot({ path: `${out}/settings-${width}-${scheme}.png`, fullPage: true });
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "dark"]]))
  try { await run(w, s); } catch (e) { fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`); }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
