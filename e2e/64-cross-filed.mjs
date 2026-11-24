// Cross-filed tasks (WP-378): an agent session working in another repo files
// a task here; the issue page says where it came from, the agent text tells
// this repo's agents to check it belongs, and a decline shows its reason.
// Run after 01-signup-flow (user robin, project WAYP).
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

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  const overflow = async (what) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} ${what} has no sideways scroll`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const { token } = await (await call("POST", "/me/tokens", { name: `cross ${n}`, agent: "anthropic/claude-opus-5-5" })).json();
  const agent = { ...H, Authorization: `Bearer ${token}` };
  const where = { session_id: `cli-cross-${n}`, machine: `box-${n}`, cwd: "/work/elsewhere", repo: "me/elsewhere" };
  const started = await call("POST", "/sessions/hook", { events: [{ event: "SessionStart", ...where, model: "claude-opus-5-5" }] }, agent);
  ok(started.ok(), `${tag} session started: ${started.status()}`);
  const as = { ...agent, "X-Waypoint-Session": where.session_id, "X-Waypoint-Machine": where.machine };
  const created = await call("POST", "/issues", { project: "WAYP", title: `Expose an export endpoint ${tag}`, agent: "claude" }, as);
  const issue = await created.json();
  ok(issue.filed_from?.repo === "me/elsewhere" && issue.filed_from.here, `${tag} recorded where it came from: ${JSON.stringify(issue.filed_from)}`);

  const text = await (await call("GET", `/issues/${issue.key}?format=text`, undefined, agent)).text();
  ok(text.includes("Filed from me/elsewhere") && text.includes(`wp decline ${issue.key}`), `${tag} agent text disclaims it: ${text.slice(0, 400)}`);

  await page.goto(`${base}/issue/${issue.key}`);
  const filed = page.locator("[data-testid=filed-from] >> visible=true");
  await filed.waitFor();
  ok((await filed.textContent()).includes("me/elsewhere"), `${tag} the issue page says where it came from`);
  await shot("filed");
  await overflow("the issue");

  const declined = await call("POST", `/agent/decline/${issue.key}`, { comment: "This repo has no export; it belongs in the reports service" }, agent);
  ok(declined.ok(), `${tag} declined: ${declined.status()} ${await declined.text()}`);
  await page.reload();
  await filed.filter({ hasText: "Declined: This repo has no export" }).waitFor();
  ok(true, `${tag} the decline shows its reason`);
  await shot("declined");
  await overflow("the declined issue");
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\ncross-filed suite passed");
