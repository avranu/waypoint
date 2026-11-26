// Checks: on the kanban board, the empty column area starts an inline add (WP-63),
// and issues added there land at the top of the column, rapid entry included (WP-73);
// so do issues a person adds any other way, such as the full new-issue dialog (WP-494).
// Needs user robin (run 01-signup-flow first).
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
for (const vp of looks([
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
])) {
  const tag = `${vp.width}px`;
  const ctx = await browser.newContext({ viewport: vp });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fail(`pageerror (${tag}): ` + e.message));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`console (${tag}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = {
    get: async (p) =>
      (await page.request.get(base + "/api/v1" + p, { headers: H })).json(),
    post: async (p, d) =>
      (
        await page.request.post(base + "/api/v1" + p, { headers: H, data: d })
      ).json(),
  };

  const key = "QA" + vp.width;
  if (!(await api.get("/projects")).some((p) => p.key === key))
    await api.post("/projects", { name: "Quick add " + vp.width, key });
  const titles = (c) =>
    c.locator("[data-nav-key]").evaluateAll((els) => els.map((e) => e.textContent));

  await page.goto(`${base}/p/${key}/board`);
  await page.waitForSelector("section[data-col]");

  const last = page.locator("section[data-col]").last();
  await last.getByText("Nothing here").click();
  const lastBox = last.getByLabel("New issue title");
  ok(await lastBox.isVisible(), `${tag}: clicking Nothing here opens the add box`);
  ok(
    await lastBox.evaluate((e) => e === document.activeElement),
    `${tag}: the add box is focused`,
  );
  await page.screenshot({ path: `${out}/${tag}-empty-click.png` });
  await page.keyboard.press("Escape");

  const first = page.locator("section[data-col]").first();
  for (const t of ["Seed one", "Seed two"])
    await api.post("/issues", { project: key, title: t });
  await page.reload();
  await page.waitForSelector("section[data-col]");
  await first.locator("[data-nav-key]").filter({ hasText: "Seed two" }).waitFor();
  await first.getByRole("button", { name: "Add issue" }).click();
  const input = first.getByLabel("New issue title");
  await input.fill("Added first");
  await input.press("Enter");
  await first.locator("[data-nav-key]").filter({ hasText: "Added first" }).waitFor();
  ok(
    (await input.isVisible()) &&
      (await input.evaluate((e) => e === document.activeElement)),
    `${tag}: the box stays open and focused`,
  );
  await input.fill("Added second");
  await input.press("Enter");
  await first.locator("[data-nav-key]").filter({ hasText: "Added second" }).waitFor();
  const order = ["Added second", "Added first", "Seed two", "Seed one"];
  const got = (await titles(first)).map((t) => order.find((x) => t.includes(x)));
  ok(
    got.join("|") === order.join("|"),
    `${tag}: newest at the top of the column: ${got.join("|")}`,
  );
  await page.screenshot({ path: `${out}/${tag}-added-top.png` });
  await ctx.close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
