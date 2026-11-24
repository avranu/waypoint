// Use what's left at the week's end (WP-262) in a real browser at desktop
// and phone widths: the Autopilot tab's card turns it on for all projects,
// says the plan in the week's last hours (on pace, running, what it does
// next), takes a cap, and the defaults form keeps it when it saves. Turned
// off, the plan line goes. Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();

// Two seven_day readings an hour apart, the week resetting in 8 hours: 52%
// now at 2 points an hour lands near 68%, well short of 100.
function readings() {
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  db.exec("DELETE FROM quota_samples");
  const uid = db
    .prepare("SELECT id FROM users WHERE username = 'robin'")
    .get().id;
  const now = Date.now();
  const reset = now + 8 * 3600_000;
  const ins = db.prepare(
    "INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine, session, model) VALUES (?,?,?,?,?,?,?,?)",
  );
  ins.run(
    uid,
    now - 3600_000,
    "seven_day",
    50,
    reset,
    "devhost",
    "",
    "claude-opus-5-5",
  );
  ins.run(uid, now, "seven_day", 52, reset, "devhost", "", "claude-opus-5-5");
  db.close();
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("PUT", "/autopilot", { limits: {}, reset: true });
  readings();

  await page.goto(`${base}/agents?tab=autopilot`);
  await page.getByRole("button", { name: "Autopilot settings" }).click();
  const card = page.getByTestId("autopilot-spend");
  await card.waitFor();
  const sw = page.getByRole("switch", {
    name: "Use what’s left at the week’s end",
  });
  ok(
    (await sw.getAttribute("aria-checked")) === "false",
    `${tag} it starts off`,
  );
  ok(
    (await page.getByTestId("autopilot-spend-why").count()) === 0,
    `${tag} off, no plan line`,
  );

  await sw.click();
  const why = page.getByTestId("autopilot-spend-why");
  await why.waitFor();
  const said = (await why.innerText()).replace(/\s+/g, " ");
  ok(
    /8 h left, 52% used, on pace for 6\d%/.test(said) &&
      /starts one more/.test(said),
    `${tag} the plan says where the week lands (${said})`,
  );
  const o = await api("GET", "/autopilot");
  ok(
    o.defaults.spend_left === true && o.spend.in_window && o.spend.start,
    `${tag} on for the server, in the window`,
  );
  await shot("on");
  await overflow("on");

  await page.locator("#spend-at-once").fill("5");
  await page.locator("#spend-at-once").press("Enter");
  await page.waitForFunction(
    ([b]) =>
      fetch(b + "/api/v1/autopilot", {
        headers: { "X-Waypoint-Client": "web" },
      })
        .then((r) => r.json())
        .then((o) => o.defaults.spend_at_once === 5),
    [base],
  );
  ok(true, `${tag} the cap saves`);
  await page.locator("#spend-at-once").fill("0");
  ok(
    /1 to 20/.test(await card.innerText()),
    `${tag} a cap of 0 is refused before saving`,
  );
  await page.locator("#spend-at-once").fill("5");

  await page.locator("#defaults-grace_seconds").fill("90");
  await page.locator("#defaults-grace_seconds").press("Enter");
  await page.waitForFunction(
    ([b]) =>
      fetch(b + "/api/v1/autopilot", {
        headers: { "X-Waypoint-Client": "web" },
      })
        .then((r) => r.json())
        .then((o) => o.defaults.grace_seconds === 90),
    [base],
  );
  const after = await api("GET", "/autopilot");
  ok(
    after.defaults.spend_left === true && after.defaults.spend_at_once === 5,
    `${tag} the defaults form keeps it (${JSON.stringify(after.defaults)})`,
  );

  await sw.click();
  await page.waitForFunction(
    () => !document.querySelector('[data-testid="autopilot-spend-why"]'),
  );
  ok(true, `${tag} off again, the plan line goes`);
  await page.keyboard.press("Escape");
  await card.waitFor({ state: "hidden" });
  const pageSw = page.getByRole("switch", {
    name: "Use what’s left",
    exact: true,
  });
  ok(
    (await pageSw.getAttribute("aria-checked")) === "false",
    `${tag} the page's toggle shows it off`,
  );
  await pageSw.click();
  await page.waitForFunction(
    ([b]) =>
      fetch(b + "/api/v1/autopilot", {
        headers: { "X-Waypoint-Client": "web" },
      })
        .then((r) => r.json())
        .then((o) => o.defaults.spend_left === true),
    [base],
  );
  ok(
    (await pageSw.getAttribute("aria-checked")) === "true",
    `${tag} the page's toggle turns it on`,
  );
  await shot("toggle-on");
  await overflow("toggle on");
  await api("PUT", "/autopilot", { limits: {}, reset: true });
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [390, "dark"],
]))
  await run(w, s);

const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
db.exec("PRAGMA busy_timeout = 5000");
db.exec("DELETE FROM quota_samples");
db.close();
await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\nspend-left suite passed");
