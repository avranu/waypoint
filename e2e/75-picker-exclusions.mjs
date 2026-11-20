// Pickers leave out what is already attached (WP-778): "Add relation" doesn't
// offer the issue's relations (of that kind) or the issue itself, the parent picker doesn't
// offer its subtasks, a page's "Add a link" search doesn't offer what the
// page links to (a key typed out still says "linked"), and "Link to a goal"
// doesn't offer the goal the task is linked to.
// Desktop and phone widths. Needs user robin (01-signup-flow).
import { chromium } from "playwright";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KEY = "PICKX";

const browser = await chromium.launch();
let seed;

for (const width of [1280, 390]) {
  const tag = `${width}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = async (m, p, d) => {
    const r = await page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
    if (!r.ok()) throw new Error(`${m} ${p}: ${r.status()} ${await r.text()}`);
    return r.status() === 204 ? null : r.json();
  };

  if (!seed) {
    await api("POST", "/projects", { name: "Picker checks", key: KEY });
    const issue = (title, parent) => api("POST", "/issues", { project: KEY, title, parent });
    // Has a parent, so the parent picker shows without adding the property.
    const top = await issue("Harbor top task");
    const main = await issue("Harbor main task", top.key);
    const linked = await issue("Harbor linked task");
    const unlinked = await issue("Harbor unlinked task");
    await issue("Harbor subtask", main.key);
    await api("POST", `/issues/${main.key}/relations`, { kind: "relates", issue: linked.key });
    const goal = (title) => api("POST", "/design", { title, kind: "goal", project: KEY });
    const served = await goal("Harbor served goal");
    await goal("Harbor other goal");
    const implemented = await goal("Harbor implemented goal");
    await api("POST", "/links", {
      from: { type: "issue", key: main.key },
      to: { type: "design", id: served.id },
      rel: "serves",
    });
    await api("POST", "/links", {
      from: { type: "issue", key: main.key },
      to: { type: "design", id: implemented.id },
      rel: "implements",
    });
    const doc = await api("POST", "/pages", { title: "Harbor notes", project: KEY, body: "Notes." });
    await api("POST", "/links", {
      from: { type: "page", id: doc.id },
      to: { type: "issue", key: linked.key },
      rel: "relates",
    });
    seed = { main, linked, unlinked, doc };
  }
  const { main, linked, unlinked, doc } = seed;
  // A row of the open picker, found from its search box: the page itself
  // shows the same issues and goals in its own buttons.
  let box;
  const row = (title) =>
    box.locator("xpath=ancestor::div[2]").getByRole("option", { name: new RegExp(title) });

  await page.goto(`${base}/issue/${main.key}`);
  await page.getByRole("heading", { name: "Relations" }).waitFor();

  // ---- relations ----
  await page.getByRole("button", { name: "Add relation" }).click();
  // The add box lists matches inline (WP-1196); it opens focused.
  const rbox = page.getByRole("combobox", { name: "Task to relate" });
  await rbox.waitFor();
  const list = page.getByRole("listbox", { name: "Matching tasks" });
  const opt = (title) => list.getByRole("option", { name: new RegExp(title) });
  await opt("Harbor unlinked task").first().waitFor();
  ok((await opt("Harbor linked task").count()) === 0, `${tag}: a related issue isn't offered`);
  ok((await opt("Harbor main task").count()) === 0, `${tag}: the issue itself isn't offered`);
  await rbox.fill("Harbor linked");
  await list.waitFor({ state: "hidden" });
  ok(true, `${tag}: searching for a related issue finds nothing to add`);
  await page.screenshot({ path: `${out}/picker-relation-${width}.png` });
  await rbox.fill("Harbor unlinked");
  await opt("Harbor unlinked task").first().waitFor();
  ok(true, `${tag}: searching still finds an unrelated issue`);
  // A pair that's only related can still be marked as a blocker.
  await page.getByRole("combobox", { name: "Kind of relation" }).click();
  await page.getByRole("option", { name: "Blocks", exact: true }).click();
  await rbox.fill("Harbor linked");
  await opt("Harbor linked task").first().waitFor();
  ok(true, `${tag}: a related issue is offered for a different kind of relation`);
  await page.getByRole("button", { name: "Done adding" }).click();
  await rbox.waitFor({ state: "hidden" });

  // ---- parent ----
  await page.locator('button:has-text("Harbor top task"):visible').first().click();
  box = page.getByPlaceholder("Search for a parent…");
  await box.fill("Harbor");
  await row("Harbor unlinked task").first().waitFor();
  ok((await row("Harbor subtask").count()) === 0, `${tag}: a subtask isn't offered as the parent`);
  await page.keyboard.press("Escape");

  // ---- goal ----
  await page.getByText("Harbor served goal").first().waitFor();
  await page.getByRole("button", { name: "Link to a goal…" }).first().click();
  box = page.getByPlaceholder("Which goal does it serve?");
  await box.waitFor();
  await row("Harbor other goal").first().waitFor();
  ok((await row("Harbor served goal").count()) === 0, `${tag}: the linked goal isn't offered`);
  ok(
    (await row("Harbor implemented goal").count()) === 1,
    `${tag}: a goal the task only implements can still be picked`,
  );
  await page.keyboard.press("Escape");

  // ---- links on a page ----
  await page.goto(`${base}/pages/${doc.id}`);
  await page.getByRole("button", { name: "Add a link" }).click();
  const links = (box = page.getByRole("combobox", { name: "Find something to link" }));
  await links.fill("Harbor");
  await row("Harbor unlinked task").first().waitFor();
  ok((await row("Harbor linked task").count()) === 0, `${tag}: a linked issue isn't in the search`);
  await links.fill(linked.key);
  await page.getByText("linked", { exact: true }).waitFor();
  ok(true, `${tag}: a linked key typed out says linked`);
  await links.fill(unlinked.key);
  await row(unlinked.key).first().waitFor();
  ok(await row(unlinked.key).first().isEnabled(), `${tag}: an unlinked key typed out can be picked`);
  ok(
    !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
    `${tag}: no sideways scroll`,
  );
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
console.log("all ok");
