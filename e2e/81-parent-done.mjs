// A parent task can't be marked done while a sub-task is open (WP-1196,
// after WP-680 closed with 8 of 9 sub-tasks still To Do): the status picker
// says how many are open, the server refuses and names them, and once they
// close the parent closes.
// Run after 01-signup-flow (user robin, project WAYP).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const browser = await chromium.launch();

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const web = { "X-Waypoint-Client": "web" };
  const api = async (m, p, d) => {
    const r = await page.request.fetch(base + "/api/v1" + p, { method: m, headers: web, data: d });
    return { status: r.status(), body: await r.json().catch(() => ({})) };
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const parent = (await api("POST", "/issues", { project: "WAYP", title: `Ship the export ${tag}`, status: "todo" })).body;
  const a = (await api("POST", "/issues", { project: "WAYP", title: `Export as CSV ${tag}`, parent: parent.key, status: "in_progress" })).body;
  const b = (await api("POST", "/issues", { project: "WAYP", title: `Export as JSON ${tag}`, parent: parent.key, status: "done" })).body;

  // The API refuses and names the open one.
  const refused = await api("PATCH", `/issues/${parent.key}`, { status: "done" });
  ok(refused.status === 400 && JSON.stringify(refused.body).includes(a.key) && !JSON.stringify(refused.body).includes(b.key), `${tag} the API refuses and names only the open sub-task: ${refused.status} ${JSON.stringify(refused.body)}`);

  // The picker says it before the click; picking Done anyway shows why not.
  // (The sub-task is In Progress, so the only "To Do" is the parent's.)
  await page.goto(`${base}/issue/${parent.key}`);
  await page.locator('button[title="To Do"] >> visible=true').first().click();
  const done = page.getByRole("option", { name: /^Done/ }).filter({ hasText: "1 sub-task open" });
  await done.waitFor();
  ok(true, `${tag} the picker says 1 sub-task is open next to Done`);
  await shot("picker");
  await done.click();
  await page.getByText(new RegExp(`still open \\(${a.key}\\)`)).first().waitFor();
  ok(true, `${tag} picking Done says which sub-task is open`);
  await shot("refused");
  ok((await api("GET", `/issues/${parent.key}`)).body.status?.category !== "done", `${tag} the parent stays open`);

  // Won't Do on the sub-task closes it; then the parent can close.
  ok((await api("PATCH", `/issues/${a.key}`, { status: "canceled" })).status === 200, `${tag} the open sub-task is dropped`);
  const closed = await api("PATCH", `/issues/${parent.key}`, { status: "done" });
  ok(closed.status === 200 || (await api("GET", `/issues/${parent.key}`)).body.status?.category === "done", `${tag} with none open, the parent closes: ${closed.status} ${JSON.stringify(closed.body).slice(0, 200)}`);
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "dark"]]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
