// Fixes from the third review round, in a real browser: a page's unsaved
// draft can't be saved into another page, quick checkbox toggles all stick,
// and the device page warns when a login started somewhere else.
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
const H = { "X-Waypoint-Client": "web" };

async function run(width) {
  const tag = `${width}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = (m, p, d, headers = {}) =>
    page.request
      .fetch(base + "/api/v1" + p, {
        method: m,
        headers: { ...H, ...headers },
        data: d,
      })
      .then((r) => (r.status() === 204 ? null : r.json()));

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  // ---- switching pages while editing (desktop: the tree stays beside the page) ----
  if (width > 500) {
    const a = await api("POST", "/pages", {
      title: `Alpha ${tag}`,
      body: "Body of alpha",
      project: "WAYP",
    });
    const b = await api("POST", "/pages", {
      title: `Beta ${tag}`,
      body: "Body of beta",
      project: "WAYP",
    });
    const tree = page.locator("aside");
    await page.goto(`${base}/pages/${b.id}`); // Beta is now cached
    await page.getByText("Body of beta").waitFor();
    await tree.getByRole("link", { name: `Alpha ${tag}` }).click();
    await page.getByText("Body of alpha").click();
    await page.locator("textarea").first().fill("DRAFT meant for alpha");
    await tree.getByRole("link", { name: `Beta ${tag}` }).click();
    await page.waitForURL(`${base}/pages/${b.id}`);
    await page.getByText("Body of beta").waitFor();
    ok(
      (await page.locator("textarea").count()) === 0,
      `${tag} editor closed when switching pages`,
    );
    ok(
      (await api("GET", `/pages/${b.id}`)).body === "Body of beta",
      `${tag} beta untouched`,
    );
  }

  // ---- quick checkbox toggles, with slow saves ----
  const c = await api("POST", "/pages", {
    title: `Tasks ${tag}`,
    body: "- [ ] one\n- [ ] two\n- [ ] three",
    project: "WAYP",
  });
  await page.route("**/api/v1/pages/*", async (route) => {
    if (route.request().method() === "PATCH")
      await new Promise((r) => setTimeout(r, 600));
    await route.continue();
  });
  await page.goto(`${base}/pages/${c.id}`);
  const boxes = page.locator("input.task");
  await boxes.first().waitFor();
  await boxes.nth(0).click();
  await boxes.nth(2).click();
  await page.waitForTimeout(2000);
  const body = (await api("GET", `/pages/${c.id}`)).body;
  ok(
    body.includes("[x] one") &&
      body.includes("[x] three") &&
      body.includes("[ ] two"),
    `${tag} both quick toggles saved: ${JSON.stringify(body)}`,
  );
  await page.unrouteAll();

  // ---- device login started from another address ----
  const start = await api(
    "POST",
    "/cli/login",
    { name: "someone's laptop" },
    { "X-Forwarded-For": "203.0.113.7" },
  );
  await page.goto(`${base}/device?code=${start.user_code}`);
  await page.getByText("someone's laptop").waitFor();
  ok(
    await page.getByText(/started from a different address/).isVisible(),
    `${tag} different-address warning shown`,
  );
  ok(
    await page.getByText("203.0.113.7").first().isVisible(),
    `${tag} warning names the address`,
  );
  await page.screenshot({ path: `${out}/device-warning-${width}.png` });
  await ctx.close();
}

for (const w of looks([1440, 390])) await run(w);
await browser.close();
console.log(errors.length ? `${errors.length} failures` : "all good");
process.exit(errors.length ? 1 : 0);
