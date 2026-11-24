// Search finds what the searcher means (WP-610): "PR" is that word, not a
// prefix of "project" and "process"; title matches and the project in view
// come first; resting the pointer on a result shows its details after a
// pause; the issue picker is wide enough to read titles. Run after
// 01-signup-flow (user robin, an admin).
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
  const desktop = width >= 1024;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme, hasTouch: !desktop });
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

  const sfx = `${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  const here = `SR${sfx}`;
  const there = `SO${sfx}`;
  await api("POST", "/projects", { name: `Search here ${tag}`, key: here });
  await api("POST", "/projects", { name: `Search there ${tag}`, key: there });
  const long = "Background on the project: the process of prompt prep, production promotion and pricing. ".repeat(8);
  const noise = [];
  for (const t of ["Project process prompts", "Production pricing", "Promote the preview build"])
    noise.push(await api("POST", "/issues", { project: there, title: t, description: long }));
  const elsewhere = await api("POST", "/issues", { project: there, title: "Review every PR before merge" });
  const want = await api("POST", "/issues", { project: here, title: "Consider improvements to our PR process",
    description: "Reviews wait too long and the merge line stalls. Ideas: smaller PRs, a draft tier, review on push." });
  await api("POST", "/issues", { project: here, title: "Tidy the release notes" });
  const src = await api("POST", "/issues", { project: here, title: "Release checklist" });

  await page.goto(`${base}/p/${here}/board`);
  await page.waitForLoadState("networkidle");
  await page.keyboard.press("Control+k");
  const box = page.getByPlaceholder(/Search issues, notes, terms/);
  await box.fill("PR");
  const row = page.locator("button", { hasText: want.title });
  await row.waitFor();
  await page.waitForFunction(() => ![...document.querySelectorAll("button[data-idx]")].some((b) => b.textContent.includes("Release checklist")));
  const rows = await page.locator("button[data-idx]").allInnerTexts();
  const first = rows.findIndex((t) => /Consider improvements|Review every PR/.test(t));
  ok(first >= 0 && rows[first].includes(want.title), `${tag} palette: the project in view's PR issue comes first (${rows.slice(0, 4).join(" | ").replace(/\n/g, " ")})`);
  ok(noise.every((n) => !rows.some((t) => t.includes(n.title))), `${tag} palette: no issues that only say project/process/prompt (${rows.join(" | ").replace(/\n/g, " ")})`);
  if (desktop) {
    await row.hover();
    await page.waitForTimeout(250);
    ok(!(await page.getByRole("tooltip").isVisible()), `${tag} palette: no card while the pointer just passes`);
    const card = page.getByRole("tooltip");
    await card.waitFor();
    ok((await card.innerText()).includes("draft tier"), `${tag} palette: the card shows the description`);
    await page.screenshot({ path: `${out}/palette-${width}-${scheme}.png` });
    await box.press("End");
    ok(!(await card.isVisible()), `${tag} palette: typing hides the card`);
  } else {
    await page.screenshot({ path: `${out}/palette-${width}-${scheme}.png` });
  }
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} palette: no sideways scroll`);
  await page.keyboard.press("Escape");

  await page.goto(`${base}/issue/${src.key}`);
  await page.getByRole("button", { name: "Add relation" }).click();
  const search = page.getByRole("combobox", { name: "Task to relate" });
  await search.waitFor();
  await search.fill("PR");
  const list = page.getByRole("listbox", { name: "Matching tasks" });
  const pick = list.getByRole("option", { name: new RegExp(want.title) }).first();
  await pick.waitFor();
  await page.waitForFunction(() => ![...document.querySelectorAll('[role=listbox] [role=option]')].some((b) => b.textContent.includes("Tidy the release notes")));
  const picks = await list.getByRole("option").allInnerTexts();
  ok(picks[0]?.includes(want.title), `${tag} picker: the project in view's PR issue comes first (${picks.slice(0, 3).join(" | ")})`);
  ok(noise.every((n) => !picks.some((t) => t.includes(n.title))), `${tag} picker: no project/process/prompt noise`);
  const w = await pick.evaluate((el) => el.getBoundingClientRect().width);
  ok(w >= (desktop ? 440 : 300), `${tag} picker rows are wide enough to read (${Math.round(w)}px)`);
  await page.screenshot({ path: `${out}/picker-${width}-${scheme}.png` });
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} picker: no sideways scroll`);
  await Promise.all([
    page.waitForResponse((r) => r.url().includes("/relations") && r.request().method() === "POST"),
    pick.click(),
  ]);
  const rels = (await api("GET", `/issues/${src.key}`)).relations ?? [];
  ok(rels.some((r) => r.issue.key === want.key), `${tag} picking relates it`);
  ok(await search.isVisible(), `${tag} the add box stays open for the next one`);
  // Enter waits for the search answer and takes the top match, never a row the list still shows for the earlier words.
  await search.fill("review every merge");
  await page.keyboard.press("Enter");
  await page.waitForResponse((r) => r.url().includes("/relations") && r.request().method() === "POST");
  const rels2 = (await api("GET", `/issues/${src.key}`)).relations ?? [];
  ok(rels2.some((r) => r.issue.key === elsewhere.key) && rels2.length === 2, `${tag} Enter before the results arrive picks the match (${rels2.map((r) => r.issue.key).join(" ")})`);

  const epic = await api("POST", "/issues", { project: here, title: "Release epic", type: "epic" });
  const kid = await api("POST", "/issues", { project: here, title: "Write the upgrade guide", parent: epic.key });
  await page.goto(`${base}/issue/${kid.key}`);
  await page.getByRole("button", { name: new RegExp(epic.key) }).first().click();
  await page.getByPlaceholder("Search for a parent…").fill("PR process");
  await page.keyboard.press("Enter");
  await page.waitForResponse((r) => r.url().includes(`/issues/${kid.key}`) && r.request().method() === "PATCH");
  const parent = (await api("GET", `/issues/${kid.key}`)).parent?.key;
  ok(parent === want.key, `${tag} typing a parent's words and Enter picks the top hit (parent ${parent ?? "removed"})`);
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "dark"]]))
  try { await run(w, s); } catch (e) { fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`); }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
