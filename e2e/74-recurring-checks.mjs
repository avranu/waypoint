// Recurring mistakes (WP-710): a project's settings turn off filing a task
// when a kind of mistake keeps coming up. On by default; off is saved.
// Desktop and phone widths. Needs user robin and project WAYP (01-signup-flow).
import { chromium } from "playwright";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const NAME = "File recurring mistakes as checks";

const browser = await chromium.launch();

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

  const saved = async () => {
    const r = await page.request.get(`${base}/api/v1/projects/WAYP`, { headers: H });
    return (await r.json()).settings?.no_recurring_checks === true;
  };

  await page.goto(base + "/p/WAYP/settings");
  const toggle = page.getByRole("switch", { name: NAME });
  await toggle.waitFor();
  ok((await toggle.getAttribute("aria-checked")) === "true", `${tag}: on by default`);
  ok(!(await saved()), `${tag}: nothing saved yet`);

  await toggle.scrollIntoViewIfNeeded();
  await toggle.click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.waitForFunction(
    () => ![...document.querySelectorAll("button")].some((b) => b.textContent === "Save changes"),
  );
  ok(await saved(), `${tag}: off is saved`);
  await page.reload();
  await toggle.waitFor();
  ok((await toggle.getAttribute("aria-checked")) === "false", `${tag}: off after a reload`);
  ok(
    await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
    `${tag}: no sideways scroll`,
  );
  await toggle.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `${out}/settings-${tag}.png` });

  // Back on, for the next width.
  await toggle.click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.waitForFunction(
    () => ![...document.querySelectorAll("button")].some((b) => b.textContent === "Save changes"),
  );
  ok(!(await saved()), `${tag}: back on is saved`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
console.log("all ok");
