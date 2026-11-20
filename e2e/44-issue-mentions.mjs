// Mentions on the issue page (WP-101): keys in a comment become muted
// "Mentions" / "Mentioned by" chips, an issue with none shows no section.
// Keys in a person's description relate the issues instead, on an open page
// without a reload (WP-781). Run after 01-signup-flow (user robin, an admin).
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

  const key = `MN${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Mentions ${tag}`, key });
  const a = await api("POST", "/issues", { project: key, title: "Parser rewrite" });
  const b = await api("POST", "/issues", { project: key, title: "Docs pass", description: `Waits on ${a.key}; see \`${key}-99\`.` });
  const c = await api("POST", "/issues", { project: key, title: "Release notes", description: `Covers ${a.key} and ${b.key}.` });

  const relation = (i) => page.getByRole("link", { name: `${i.key} ${i.title}` });

  await page.goto(`${base}/issue/${c.key}`);
  await relation(a).waitFor();
  await relation(b).waitFor();
  ok(((await api("GET", `/issues/${c.key}`)).relations ?? []).length === 2, `${tag} named tasks are relations`);
  ok((await page.getByText(/^Mentions \d+$/).count()) === 0, `${tag} a relation isn't repeated as a mention`);

  // A comment's keys are mentions only.
  const d = await api("POST", "/issues", { project: key, title: "Changelog" });
  await api("POST", `/issues/${c.key}/comments`, { body: `Compare ${d.key}.` });
  await page.goto(`${base}/issue/${c.key}`);
  await page.getByText("Mentions 1", { exact: true }).waitFor();
  ok(await page.getByRole("link", { name: d.key }).first().isVisible(), `${tag} the mentioned key is a link`);
  ok((await page.getByText("Mentioned by").count()) === 0, `${tag} an empty group is hidden`);
  await page.screenshot({ path: `${out}/mentions-${width}-${scheme}.png`, fullPage: true });
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);

  await page.goto(`${base}/issue/${d.key}`);
  await page.getByText("Mentioned by 1", { exact: true }).waitFor();
  ok((await page.getByText("Mentions 0").count()) === 0, `${tag} the other direction reads "Mentioned by"`);

  const lone = await api("POST", "/issues", { project: key, title: "Nothing here" });
  await page.goto(`${base}/issue/${lone.key}`);
  await page.getByText("Nothing here").first().waitFor();
  ok((await page.getByRole("region", { name: "Mentions" }).count()) === 0, `${tag} no section when there are none`);

  // Saving a description that names a task shows the relation at once.
  await page.getByRole("button", { name: "Add a description…" }).click();
  await page.getByPlaceholder(/Markdown supported/).fill(`See ${d.key}.`);
  await relation(d).waitFor();
  ok(true, `${tag} the saved description's relation shows without a reload`);

  // So does a new task naming the one this page has open.
  const e = await api("POST", "/issues", { project: key, title: "Follow-up", description: `After ${lone.key}.` });
  await relation(e).waitFor();
  ok(true, `${tag} a task naming the open one shows up without a reload`);
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "dark"]]))
  try { await run(w, s); } catch (e) { fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`); }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
