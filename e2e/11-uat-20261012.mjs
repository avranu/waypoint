// UAT 2026-10-12 (Waypoint WP-N3): starter tags, explainers that
// fold into a (?) and stay folded, tooltips instead of hint text, dropdowns
// that open at once, "Add status" behind a button, the project icon picker,
// parent "None", the workflow dialog, readable settings on a project
// background, and no sideways scrolling at phone, tablet and desktop widths.
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
const timings = [];

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
      .then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const settings = (key, tab) =>
    page.goto(`${base}/p/${key}/settings${tab ? `?tab=${tab}` : ""}`);
  const mobile = width < 640;

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const labels = (await api("GET", "/projects/WAYP")).labels.map((l) => l.name);
  ok(
    labels.includes("ui") && labels.includes("backend"),
    `${tag} starter tags ui and backend: ${labels}`,
  );

  await settings("WAYP", "workflow");
  const note = page.getByRole("note");
  const showHelp = page.getByRole("button", { name: "Show help: Statuses" });
  // Explainers start folded (an earlier viewport's run may have left it open).
  await showHelp.or(note).waitFor();
  if ((await note.count()) > 0) {
    await note.getByRole("button", { name: "Hide this help" }).click();
    await page.waitForTimeout(400);
  }
  await showHelp.waitFor();
  ok((await note.count()) === 0, `${tag} explainer folded`);
  await showHelp.click();
  await note.waitFor();
  ok(
    await note.getByText("Canceled").isVisible(),
    `${tag} status categories are in the explainer`,
  );
  await note.getByRole("button", { name: "Hide this help" }).click();
  await showHelp.waitFor();
  ok((await note.count()) === 0, `${tag} explainer folded away`);
  await page.waitForTimeout(400); // Wait for the preference save before reloading; the reload cancels it.
  await page.reload();
  await showHelp.waitFor();
  ok((await note.count()) === 0, `${tag} still folded after a reload`);
  ok(
    !(await api("GET", "/me")).prefs.open_help?.includes("settings:statuses"),
    `${tag} fold saved to the profile`,
  );
  await showHelp.click();
  await note.waitFor();
  await page.waitForTimeout(400);

  const nameWidth = await page
    .getByLabel("Status name")
    .first()
    .evaluate((e) => e.getBoundingClientRect().width);
  ok(
    nameWidth > 80,
    `${tag} status names are readable (${Math.round(nameWidth)}px)`,
  );

  ok(
    (await page.getByLabel("New status name").count()) === 0,
    `${tag} no new-status field until asked`,
  );
  await page.getByRole("button", { name: "Add status", exact: true }).click();
  const newStatus = page.getByLabel("New status name");
  ok(
    await newStatus.evaluate((e) => e === document.activeElement),
    `${tag} add status focuses the name`,
  );
  await page.getByRole("button", { name: "Cancel" }).click();
  ok((await newStatus.count()) === 0, `${tag} cancel hides the form again`);

  const time = async (what, trigger, ready) => {
    await trigger.waitFor();
    await page.waitForTimeout(300);
    const t0 = Date.now();
    await trigger.click();
    await ready.waitFor();
    const ms = Date.now() - t0;
    timings.push(`${what} ${width}: ${ms} ms`);
    ok(ms < 400, `${tag} ${what} opened in ${ms} ms`);
    await page.keyboard.press("Escape");
  };
  await time(
    "status category",
    page.getByRole("combobox", { name: "Category" }).first(),
    page.getByRole("option", { name: "Canceled" }),
  );
  await settings("WAYP", "members");
  await time(
    "member role",
    page.getByRole("combobox", { name: /^Role of / }).first(),
    page.getByRole("option", { name: "Viewer" }),
  );
  if (!mobile) {
    const row = page.getByRole("combobox", { name: /^Role of / }).first();
    const name = page.getByText("@robin", { exact: true });
    ok(
      (await name.isVisible()) && (await row.boundingBox()).width < 200,
      `${tag} member name shown beside a compact role picker`,
    );
  }

  await settings("WAYP");
  await page.getByRole("button", { name: /^(Choose icon|Icon: )/ }).click();
  await page.getByRole("button", { name: "🚀", exact: true }).click();
  ok(
    await page.getByRole("button", { name: "Icon: 🚀" }).isVisible(),
    `${tag} icon picked from the grid`,
  );
  await page.getByRole("button", { name: "Icon: 🚀" }).click();
  await page.getByLabel("Any emoji").fill("🦄");
  await page.getByLabel("Any emoji").press("Enter");
  ok(
    await page.getByRole("button", { name: "Icon: 🦄" }).isVisible(),
    `${tag} any typed emoji works`,
  );
  await page.getByRole("button", { name: "Discard" }).click();
  ok(
    await page
      .getByRole("combobox", { name: "Parent project" })
      .getByText("None", { exact: true })
      .isVisible(),
    `${tag} parent project reads "None"`,
  );
  ok(
    (await page.getByText("Nests this project under another").count()) === 0,
    `${tag} parent hint is not inline`,
  );
  const tip = page.getByRole("button", { name: "More info" }).first();
  if (mobile) await tip.click();
  else await tip.hover();
  ok(
    await page
      .getByRole("tooltip")
      .waitFor()
      .then(
        () => true,
        () => false,
      ),
    `${tag} a hint opens from its (?)`,
  );
  await shot("general");
  await page.keyboard.press("Escape");

  await settings("WAYP", "workflows");
  await page.getByRole("button", { name: "Edit Agent pipeline" }).click();
  const agent = page.getByLabel("Step 4 agent");
  await agent.waitFor();
  const [box, icon] = await agent.evaluate((el) => {
    const b = el.getBoundingClientRect();
    const i = el.parentElement.querySelector("svg").getBoundingClientRect();
    return [{ top: b.top, bottom: b.bottom }, { mid: i.top + i.height / 2 }];
  });
  ok(
    icon.mid > box.top && icon.mid < box.bottom,
    `${tag} robot icon sits in the agent field`,
  );
  const scrolls = await page
    .locator('textarea[aria-label$="instructions"]')
    .evaluateAll(
      (ts) => ts.filter((t) => t.scrollHeight > t.clientHeight + 2).length,
    );
  ok(scrolls === 0, `${tag} step descriptions grow instead of scrolling`);
  await shot("workflow-dialog");
  await page.keyboard.press("Escape");

  await settings("BGUA", "integrations");
  const bg = await page
    .getByRole("tabpanel")
    .evaluate((e) => getComputedStyle(e).backgroundColor);
  ok(
    bg !== "rgba(0, 0, 0, 0)" && bg !== "transparent",
    `${tag} settings panel has a surface over the background (${bg})`,
  );
  await shot("integrations-bg");

  for (const path of [
    "/",
    "/inbox",
    "/time",
    "/pages",
    "/search?q=is:open",
    "/p/WAYP/board",
    "/p/WAYP/backlog",
    "/p/WAYP/list",
    "/p/WAYP/timeline",
    "/p/WAYP/reports",
    "/issue/WAYP-1",
    "/p/WAYP/settings?tab=automations",
    "/settings?tab=connections",
    "/admin?tab=forges",
  ]) {
    await page.goto(base + path);
    await page.waitForTimeout(350);
    const over = await page.evaluate(
      () =>
        document.documentElement.scrollWidth >
        document.documentElement.clientWidth + 1,
    );
    ok(!over, `${tag} ${path} fits the width`);
  }
  await ctx.close();
}

// Setup: a project with a background for the contrast check.
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const r = await page.request.get(base + "/api/v1/projects/BGUA", {
    headers: H,
  });
  if (!r.ok())
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Backdrop UAT", key: "BGUA" },
    });
  await page.request.patch(base + "/api/v1/projects/BGUA", {
    headers: H,
    data: { background: "dusk" },
  });
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1024, "dark"],
  [768, "light"],
  [390, "dark"],
  [390, "light"],
  [1440, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(timings.join("\n"));
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
