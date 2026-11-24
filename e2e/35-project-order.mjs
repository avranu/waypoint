// Sidebar project order (WP-64): drag a project to reorder, a new project
// goes to the top, the menu switches between Custom order and Recently used,
// the keyboard moves a project from its menu, and the order is saved to the
// profile so the 390px menu shows it too.
// Run after 01-signup-flow (user robin).
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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const desktop = width >= 768;
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
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`),
  );
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const me = async () =>
    (await page.request.get(base + "/api/v1/me", { headers: H })).json();
  const makeProject = async (key, name) => {
    const r = await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name, key },
    });
    if (!r.ok()) fail(`${tag} create ${key}: ${r.status()}`);
    await sleep(1100); // active_at has a resolution of a second
  };
  const shown = async () =>
    (
      await page
        .locator("aside [data-project^=ZT]")
        .evaluateAll((els) => els.map((e) => e.getAttribute("data-project")))
    ).join(" ");
  const drag = async (from, to) => {
    const a = await page.locator(`aside [data-project=${from}]`).boundingBox();
    const b = await page.locator(`aside [data-project=${to}]`).boundingBox();
    await page.mouse.move(a.x + 60, a.y + a.height / 2);
    await page.mouse.down();
    await page.mouse.move(a.x + 60, a.y + a.height / 2 + (b.y > a.y ? 10 : -10));
    await page.mouse.move(b.x + 60, b.y + b.height / 2, { steps: 12 });
    return async () => {
      await page.mouse.up();
    };
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  if (!desktop) {
    const prefs = (await me()).prefs;
    const projects = await (
      await page.request.get(base + "/api/v1/projects", { headers: H })
    ).json();
    const idOf = Object.fromEntries(projects.map((p) => [p.key, p.id]));
    const want = (prefs.project_order ?? [])
      .map((id) => projects.find((p) => p.id === id)?.key)
      .filter((k) => k?.startsWith("ZT"));
    ok(want.length >= 3, `${tag} the desktop order was saved to the profile`);
    await page.goto(base + "/");
    await page.getByRole("button", { name: "Projects" }).click();
    const names = await page
      .locator("a[href^='/p/ZT']")
      .evaluateAll((els) => els.map((e) => e.getAttribute("href").split("/")[2]));
    ok(
      names.join(" ") === want.join(" ") && idOf.ZTD !== undefined,
      `${tag} the menu lists projects in the saved order (${names.join(" ")})`,
    );
    ok(
      (await page.locator("aside").count()) === 0,
      `${tag} no sidebar at this width`,
    );
    await sleep(400);
    await shot("menu");
    await ctx.close();
    return;
  }

  const cur = (await me()).prefs;
  await page.request.patch(base + "/api/v1/me", {
    headers: H,
    data: { prefs: { ...cur, project_sort: "", project_order: [] } },
  });
  for (const [k, n] of [
    ["ZTA", "Zeta Alpha"],
    ["ZTB", "Zeta Beta"],
    ["ZTC", "Zeta Gamma"],
  ])
    await makeProject(k, n);
  await page.goto(base + "/");
  await page.locator("aside [data-project=ZTC]").waitFor();
  ok(
    (await shown()) === "ZTC ZTB ZTA",
    `${tag} recent order before any drag (${await shown()})`,
  );
  await shot("recent");

  const drop = await drag("ZTA", "ZTC");
  ok(
    (await page.locator("aside [data-drop]").count()) === 1,
    `${tag} a line shows where it will land`,
  );
  await shot("dragging");
  // The sidebar reorders at once and saves to the profile behind it; on a
  // slow runner the save can take longer than the sleep, so wait for it
  // before reading the profile or reloading (a reload would cancel it).
  await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/api/v1/me") && r.request().method() === "PATCH",
    ),
    drop(),
  ]);
  await sleep(300);
  ok(
    (await shown()) === "ZTA ZTC ZTB",
    `${tag} dragged Alpha to the top (${await shown()})`,
  );
  let prefs = (await me()).prefs;
  ok(
    prefs.project_sort === "custom" && prefs.project_order.length >= 3,
    `${tag} saved to the profile (${prefs.project_sort})`,
  );
  ok(
    page.url() === base + "/",
    `${tag} dropping did not open the project (${page.url()})`,
  );
  await page.reload();
  await page.locator("aside [data-project=ZTC]").waitFor();
  ok((await shown()) === "ZTA ZTC ZTB", `${tag} order survives a reload`);

  await makeProject("ZTD", "Zeta Delta");
  await page.reload();
  await page.locator("aside [data-project=ZTD]").waitFor();
  ok(
    (await shown()) === "ZTD ZTA ZTC ZTB",
    `${tag} a new project is first (${await shown()})`,
  );

  const actions = page.getByRole("button", { name: "Project actions" });
  await actions.click();
  const custom = page.getByRole("menuitemradio", { name: "Custom order" });
  const recent = page.getByRole("menuitemradio", { name: "Recently used" });
  ok(
    (await custom.getAttribute("aria-checked")) === "true" &&
      (await recent.getAttribute("aria-checked")) === "false",
    `${tag} the menu marks Custom order`,
  );
  await sleep(400);
  await shot("menu");
  await recent.click();
  await sleep(300);
  ok(
    (await shown()) === "ZTD ZTC ZTB ZTA",
    `${tag} Recently used (${await shown()})`,
  );
  await actions.click();
  ok(
    (await page
      .getByRole("menuitemradio", { name: "Recently used" })
      .getAttribute("aria-checked")) === "true",
    `${tag} the menu marks Recently used`,
  );
  await page.getByRole("menuitemradio", { name: "Custom order" }).click();
  await sleep(300);
  ok(
    (await shown()) === "ZTD ZTA ZTC ZTB",
    `${tag} back to Custom order (${await shown()})`,
  );

  const row = page.locator("aside [data-project=ZTD]");
  await row.hover();
  await page.getByRole("button", { name: "Actions for Zeta Delta" }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("menuitem", { name: "Move down" }).click();
  await sleep(300);
  ok(
    (await shown()) === "ZTA ZTD ZTC ZTB",
    `${tag} Move down (${await shown()})`,
  );
  await page.locator("aside [data-project=ZTD]").hover();
  await page.getByRole("button", { name: "Actions for Zeta Delta" }).click();
  await page.getByRole("menuitem", { name: "Move up" }).click();
  await sleep(300);
  ok(
    (await shown()) === "ZTD ZTA ZTC ZTB",
    `${tag} Move up (${await shown()})`,
  );

  await page.locator("aside [data-project=ZTB] a").click();
  await page.waitForURL(base + "/p/ZTB/overview");
  ok(true, `${tag} a click opens the project`);
  await shot("after");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [390, "dark"],
], [0, 1]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
