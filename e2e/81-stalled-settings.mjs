// When sessions are told of stalled background jobs (WP-1045): Admin → AI sets
// the times for every project; a project's settings set its own, which wins
// and falls back when cleared. Times the server would refuse aren't saved.
// Desktop and 390px.
// Run after 01-signup-flow (user robin, an admin).
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
const TITLE = "Stalled background jobs";

let n = 0;
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const KEY = `SJ${"ABCD"[n++]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text())) fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  await api("POST", "/projects", { name: `Stalled ${tag}`, key: KEY });
  const noSideways = async (what) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);
  const until = async (what, pass) => {
    for (let i = 0; i < 60; i++) {
      if (pass(await api("GET", what.path))) return;
      await page.waitForTimeout(250);
    }
    fail(`${tag} never saved: ${what.say}`);
  };
  const uncustom = (path) => until({ path, say: `${path} back to what it inherits` }, (r) => !r.custom);
  const quietIs = (n) => until({ path: "/stalled-jobs/policy", say: `stall time ${n}` }, (r) => r.policy.quiet === n);

  await page.goto(`${base}/admin?tab=ai`);
  let section = page.locator("section", { has: page.getByRole("heading", { name: TITLE }) });
  const quiet = section.getByLabel("Stall after");
  await quiet.waitFor();
  ok((await quiet.inputValue()) === "15", `${tag} shows the default stall time`);
  ok((await section.getByLabel("Wake a cold session after").inputValue()) === "60", `${tag} shows the default cold wake`);
  ok(!(await section.getByRole("button", { name: "Use the default" }).isVisible()), `${tag} nothing to clear yet`);

  await section.getByLabel("Early, at a keep-warm ping").fill("40");
  ok(await section.getByRole("alert").isVisible(), `${tag} telling early after the stall time is refused`);
  await page.waitForTimeout(2800); // Autosave fires 900 ms after typing pauses; wait well past it so a save that should not happen has time to show.
  ok(!(await api("GET", "/stalled-jobs/policy")).custom, `${tag} and isn't saved`);
  await section.getByLabel("Early, at a keep-warm ping").fill("8");
  await quiet.fill("20");
  await section.getByText("Saved", { exact: true }).waitFor();
  let pol = await api("GET", "/stalled-jobs/policy");
  ok(pol.custom && pol.policy.quiet === 20 && pol.policy.early === 8 && pol.policy.cold_wake === 60, `${tag} saved: ${JSON.stringify(pol.policy)}`);
  await noSideways("Admin → AI");
  await section.scrollIntoViewIfNeeded();
  await section.screenshot({ path: `${out}/admin-${width}-${scheme}.png` });
  let writes = 0;
  const count = (r) => r.method() !== "GET" && /stalled-jobs|\/projects\/[^/]+$/.test(r.url()) && writes++;
  page.on("request", count);
  await page.waitForLoadState("networkidle"); // nothing still loading for the reload to cut off
  await page.reload();
  await section.getByLabel("Stall after").waitFor();
  await page.waitForTimeout(2000); // past the 900 ms autosave pause
  ok(writes === 0, `${tag} opening saved times sends no save (${writes})`);
  page.off("request", count);
  // A default typed in by hand must not count as the reset; Use the default still has to hold.
  await section.getByLabel("Stall after").fill("15");
  await section.getByLabel("Early, at a keep-warm ping").fill("10");
  await uncustom("/stalled-jobs/policy");
  await section.getByLabel("Stall after").fill("25");
  await quietIs(25);
  await section.getByRole("button", { name: "Use the default" }).click();
  await uncustom("/stalled-jobs/policy");
  await page.waitForTimeout(2000); // past the 900 ms autosave pause
  pol = await api("GET", "/stalled-jobs/policy");
  ok(!pol.custom && (await section.getByLabel("Stall after").inputValue()) === "15", `${tag} "Use the default" holds: ${JSON.stringify(pol)}`);
  await section.getByLabel("Early, at a keep-warm ping").fill("8");
  await section.getByLabel("Stall after").fill("20");
  await quietIs(20);

  await page.goto(`${base}/p/${KEY}/settings?tab=general`);
  section = page.locator("section", { has: page.getByRole("heading", { name: TITLE }) });
  await section.getByLabel("Stall after").waitFor();
  ok((await section.getByLabel("Stall after").inputValue()) === "20", `${tag} the project shows every project's times`);
  ok(!(await section.getByRole("button", { name: "Use every project’s" }).isVisible()), `${tag} nothing of its own yet`);
  await section.getByLabel("Stall after").fill("");
  ok(await section.getByRole("alert").isVisible(), `${tag} an empty stall time is refused`);
  await section.getByRole("switch", { name: "Tell sessions about stalled jobs" }).click();
  ok(!(await section.getByLabel("Stall after").isVisible()), `${tag} off hides the times`);
  await section.getByText("Saved", { exact: true }).waitFor();
  let own = await api("GET", `/projects/${KEY}/stalled-jobs`);
  ok(own.custom && own.policy.off && own.inherited?.quiet === 20, `${tag} the project keeps its own: ${JSON.stringify(own)}`);
  ok(!(await page.getByText("You have unsaved changes").isVisible()), `${tag} saving the times leaves the rest of the settings alone`);
  await noSideways("project settings");
  await section.scrollIntoViewIfNeeded();
  await section.screenshot({ path: `${out}/project-${width}-${scheme}.png` });
  await section.getByRole("button", { name: "Use every project’s" }).click();
  await uncustom(`/projects/${KEY}/stalled-jobs`);
  own = await api("GET", `/projects/${KEY}/stalled-jobs`);
  ok(!own.policy.off && own.policy.quiet === 20, `${tag} cleared, the project falls back to every project's times: ${JSON.stringify(own)}`);

  await page.goto(`${base}/admin?tab=ai`);
  section = page.locator("section", { has: page.getByRole("heading", { name: TITLE }) });
  await section.getByRole("button", { name: "Use the default" }).click();
  await uncustom("/stalled-jobs/policy");
  pol = await api("GET", "/stalled-jobs/policy");
  ok(pol.policy.quiet === 15, `${tag} back to the default`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
