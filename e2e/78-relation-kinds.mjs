// Relation kinds (WP-1036): Add relation offers tension and supersedes, and
// a follow-up's fix or expansion; the relation lists show the subkind and
// read tension the same from both ends. Run after 01-signup-flow (user
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
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then(async (r) => {
        const t = await r.text();
        if (r.status() >= 300) throw new Error(`${m} ${p}: ${r.status()} ${t}`);
        try {
          return JSON.parse(t);
        } catch {
          return t;
        }
      });
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `RK${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Kinds ${tag}`, key });
  const first = await api("POST", "/issues", {
    project: key,
    title: "Export the board to CSV",
  });
  const later = await api("POST", "/issues", {
    project: key,
    title: "CSV export drops quoted commas",
  });
  const simple = await api("POST", "/issues", {
    project: key,
    title: "One-glance board for executives",
  });

  const extra = await api("POST", "/issues", {
    project: key,
    title: "Quote commas in the header row",
  });
  await page.goto(`${base}/issue/${later.key}`);
  await page.getByRole("tab", { name: "Details", exact: true }).click();
  await page.getByRole("button", { name: "Add relation" }).click();
  const box = page.getByRole("combobox", { name: "Task to relate" });
  await box.waitFor();
  ok(
    await box.evaluate((el) => el === document.activeElement),
    `${tag} + relation opens the add box focused`,
  );
  const kinds = page.getByRole("combobox", { name: "Kind of relation" });
  await kinds.click();
  const offered = await page.getByRole("option").allInnerTexts();
  ok(
    [
      "In tension with",
      "Supersedes",
      "Superseded by",
      "Follows up on",
      "Followed up by",
    ].every((l) => offered.some((o) => o.trim() === l)),
    `${tag} the new kinds are offered: ${offered.join(", ")}`,
  );
  await page.keyboard.press("Escape");
  ok(
    (await page
      .getByRole("combobox", { name: "Kind of follow-up" })
      .count()) === 0,
    `${tag} no subkind for relates`,
  );
  await kinds.click();
  await page
    .getByRole("option", { name: "Follows up on", exact: true })
    .click();
  await page.getByRole("combobox", { name: "Kind of follow-up" }).click();
  await page.getByRole("option", { name: "Fix", exact: true }).click();
  await page.screenshot({
    path: `${out}/add-follow-up-${width}-${scheme}.png`,
    fullPage: true,
  });
  ok(
    !(await page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth,
    )),
    `${tag} no sideways scroll in the form`,
  );
  await box.fill("Export the board");
  await page
    .getByRole("listbox", { name: "Matching tasks" })
    .getByRole("option", { name: new RegExp("Export the board to CSV") })
    .click();
  await page.getByText("Follows up on", { exact: true }).first().waitFor();
  let rels = (await api("GET", `/issues/${later.key}`)).relations ?? [];
  ok(
    rels.some(
      (r) =>
        r.kind === "follows" &&
        r.subkind === "fix" &&
        r.issue.key === first.key &&
        r.basis === "person",
    ),
    `${tag} the form adds a fix: ${JSON.stringify(rels.map((r) => [r.kind, r.subkind, r.basis]))}`,
  );
  ok(await box.isVisible(), `${tag} the add box stays open after adding`);
  await page
    .getByText("fix", { exact: true })
    .locator("visible=true")
    .first()
    .waitFor({ timeout: 5000 })
    .catch(() => {});
  ok(
    await page
      .getByText("fix", { exact: true })
      .locator("visible=true")
      .first()
      .isVisible(),
    `${tag} the row shows the subkind`,
  );

  await box.fill(`fix ${extra.key}`);
  await page.keyboard.press("Enter");
  await page.waitForFunction(
    async ([b, k, x]) => {
      const o = await (await fetch(`${b}/api/v1/issues/${k}`)).json();
      return (o.relations ?? []).some((r) => r.issue.key === x);
    },
    [base, later.key, extra.key],
  );
  rels = (await api("GET", `/issues/${later.key}`)).relations ?? [];
  ok(
    rels.some(
      (r) =>
        r.kind === "follows" &&
        r.subkind === "fix" &&
        r.issue.key === extra.key &&
        r.basis === "person",
    ),
    `${tag} typing "fix KEY" adds a fix: ${JSON.stringify(rels.map((r) => [r.kind, r.subkind, r.issue.key]))}`,
  );
  ok(await box.isVisible(), `${tag} the add box stays open after typing one`);
  await page.waitForFunction(() =>
    /^fix\s*$/i.test(
      document.querySelector('[aria-label="Task to relate"]')?.value ?? "x",
    ),
  );
  ok(true, `${tag} the relation words are kept and the rest cleared`);
  await box.fill(`fixed by ${simple.key}`);
  await page.keyboard.press("Enter");
  await page.waitForFunction(
    async ([b, k, x]) => {
      const o = await (await fetch(`${b}/api/v1/issues/${k}`)).json();
      return (o.relations ?? []).some((r) => r.issue.key === x);
    },
    [base, later.key, simple.key],
  );
  rels = (await api("GET", `/issues/${later.key}`)).relations ?? [];
  ok(
    rels.some(
      (r) =>
        r.kind === "followed_by" &&
        r.subkind === "fix" &&
        r.issue.key === simple.key,
    ),
    `${tag} "fixed by KEY" adds a followed-by fix`,
  );
  await box.fill("blocks board");
  const list = page.getByRole("listbox", { name: "Matching tasks" });
  await page.waitForFunction(() => {
    const l = document.querySelector('[role="listbox"]');
    return (
      l &&
      !l.className.includes("opacity") &&
      l.querySelectorAll('[role="option"]').length > 1
    );
  });
  const top = (await list.getByRole("option").first().innerText()).match(
    /[A-Z]+-\d+/,
  )[0];
  await page.keyboard.press("ArrowDown");
  const picked = (
    await list.getByRole("option", { selected: true }).innerText()
  ).match(/[A-Z]+-\d+/)[0];
  await page.keyboard.press("Enter");
  await page.waitForFunction(
    async ([b, k, x]) => {
      const o = await (await fetch(`${b}/api/v1/issues/${k}`)).json();
      return (o.relations ?? []).some(
        (r) => r.kind === "blocks" && r.issue.key === x,
      );
    },
    [base, later.key, picked],
  );
  rels = (await api("GET", `/issues/${later.key}`)).relations ?? [];
  ok(
    picked !== top &&
      !rels.some((r) => r.kind === "blocks" && r.issue.key === top),
    `${tag} Enter after an arrow adds the highlighted ${picked}, not the top ${top}`,
  );
  for (const r of rels.filter((r) => r.kind === "blocks"))
    await api("DELETE", `/issues/${later.key}/relations/${r.id}`);
  await page.keyboard.press("Escape");
  await box.waitFor({ state: "hidden" });
  ok(true, `${tag} Esc closes the add box`);
  await api(
    "DELETE",
    `/issues/${later.key}/relations/${rels.find((r) => r.issue.key === simple.key).id}`,
  );

  await api("POST", `/issues/${simple.key}/relations`, {
    kind: "tension",
    issue: later.key,
  });
  await page.reload();
  await page.getByRole("tab", { name: "Details", exact: true }).click();
  await page.getByText("In tension with", { exact: true }).waitFor();
  await page.screenshot({
    path: `${out}/relations-${width}-${scheme}.png`,
    fullPage: true,
  });
  ok(
    !(await page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth,
    )),
    `${tag} no sideways scroll in the list`,
  );
  await page.goto(`${base}/issue/${simple.key}`);
  await page.getByText("In tension with", { exact: true }).waitFor();
  ok(
    await page
      .getByRole("link", { name: new RegExp(later.key) })
      .first()
      .isVisible(),
    `${tag} tension from the other end`,
  );
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [390, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
