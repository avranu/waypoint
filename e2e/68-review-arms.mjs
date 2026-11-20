// Review arms (WP-365): Admin → AI lists the ways a review can run (diff or
// diff+files, verifier on or off), each with its state and why, and an admin
// can pin one. Desktop and phone widths, light and dark.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

const runs = [[1440, "light"], [390, "light"], [390, "dark"]];
for (const [n, [width, scheme]] of runs.entries()) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  await login(page);
  const api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });

  await page.goto(`${base}/admin?tab=ai`);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Review arms" }) });
  await section.getByTestId("review-arms").waitFor();
  const rows = section.locator("li[data-arm]");
  ok((await rows.count()) === 4, `${tag} four arms show: ${await rows.count()}`);
  const row = section.locator('li[data-arm="diff/none"]');
  ok(await row.getByText("diff, no verifier").isVisible(), `${tag} the arm is named`);
  ok((await row.getAttribute("data-state")) !== "pinned", `${tag} nothing pinned yet`);

  if (n === 0) {
    const pick = async (name) => {
      await row.getByRole("combobox", { name: "Pin diff, no verifier" }).click();
      await page.getByRole("option", { name, exact: true }).click();
    };
    await pick("Pinned on");
    await page.locator('li[data-arm="diff/none"][data-state="pinned"]').waitFor();
    ok(await row.getByText("Pinned on", { exact: true }).first().isVisible(), `${tag} pinning shows Pinned on`);
    const d = await api("GET", "/ai/review-arms");
    const arm = d.arms.find((a) => a.key === "diff/none");
    ok(arm?.state === "pinned" && arm.pin === "on", `the pin is kept: ${JSON.stringify(arm)}`);
    ok(await row.getByText("every review uses it").isVisible(), `${tag} it says why`);
    await section.scrollIntoViewIfNeeded();
    await overflow("pinned");
    await shot("arms-pinned");
    await pick("Drawn");
    await page.locator('li[data-arm="diff/none"]:not([data-state="pinned"])').waitFor();
    ok(true, `${tag} unpinning puts it back to drawn`);
  }
  ok(await section.getByText(/characters of their text/).isVisible(), `${tag} the context budget shows`);
  await section.scrollIntoViewIfNeeded();
  await overflow("admin ai");
  await shot("arms");
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
