// AI estimate + sub-tasks (when a model is set up), attachments, templates,
// time-based automations, the admin AI tab and the web push section.
// Run after 01-signup-flow (user robin); creates the DEMO project.
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
// Picks an option from one of the app's dropdowns (a combobox + listbox).
const choose = async (pg, name, option) => {
  await pg.getByRole("combobox", { name, exact: true }).click();
  await pg.getByRole("option", { name: option, exact: true }).click();
};

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const browser = await chromium.launch();
const H = { "X-Waypoint-Client": "web" };

async function run(width, scheme) {
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.on("pageerror", (e) =>
    fail(`${width}/${scheme} pageerror: ${e.message}`),
  );
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      fail(`${width}/${scheme} console: ${m.text().slice(0, 300)}`),
  );
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      fail(
        `${width}/${scheme} http ${r.status()} ${r.request().method()} ${r.url()}`,
      ),
  );
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => r.json().catch(() => null));
  const shot = (n) =>
    page.screenshot({ path: `${out}/${width}-${scheme}-${n}.png` });
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const tag = `${width}${scheme[0]}${Date.now() % 100000}`;
  if (!(await api("GET", "/projects/DEMO"))?.key)
    await api("POST", "/projects", { name: "Demo", key: "DEMO" });
  const aiReady = ((await api("GET", "/ai/tasks")) ?? []).every(
    (t) => t.provider_id,
  );
  const issue = await api("POST", "/issues", {
    project: "DEMO",
    title: `Add CSV export to the time report (${tag})`,
    description: "Download a timesheet as CSV, grouped by project.",
  });

  await page.goto(base + "/issue/" + issue.key);
  if (aiReady) {
    await page.locator('[title="Story points"]:visible').click();
    await page
      .getByRole("button", { name: "Estimate with AI" })
      .first()
      .click();
    const use = page.getByRole("button", { name: /^Use \d+$/ });
    await use.waitFor({ timeout: 90000 });
    await shot("estimate");
    const label = await use.textContent();
    await use.click();
    await page.waitForTimeout(800);
    const after = await api("GET", "/issues/" + issue.key);
    ok(
      after.points === Number(label.replace("Use ", "")),
      `${tag} estimate applied: ${after.points}`,
    );

    await page.getByRole("button", { name: "Suggest" }).click();
    const addBtn = page.getByRole("button", { name: /^Add \d+ sub-tasks?$/ });
    await addBtn.waitFor({ timeout: 90000 });
    await shot("subtasks");
    await addBtn.click();
    await page.waitForTimeout(1500);
    const withKids = await api("GET", "/issues/" + issue.key);
    ok(
      withKids.children.length >= 2,
      `${tag} suggested sub-tasks added: ${withKids.children.length}`,
    );
  } else
    console.log(
      "skip AI checks (no model set up; set WAYPOINT_AI_OPENCODE_URL)",
    );

  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
    "base64",
  );
  await page
    .locator("input[type=file]")
    .first()
    .setInputFiles([
      { name: "shot.png", mimeType: "image/png", buffer: png },
      {
        name: "notes.txt",
        mimeType: "text/plain",
        buffer: Buffer.from("hello"),
      },
    ]);
  await page.getByRole("img", { name: "shot.png" }).waitFor({ timeout: 10000 });
  ok(
    await page.getByRole("link", { name: "notes.txt" }).isVisible(),
    `${tag} file listed`,
  );
  await shot("attachments");

  await page.goto(base + "/p/DEMO/settings?tab=templates");
  await page.getByRole("button", { name: "New template" }).click();
  await page.getByLabel("Template name").fill("Bug " + tag);
  await page.getByPlaceholder("Bug: ", { exact: true }).fill("Bug: ");
  await page.getByPlaceholder(/^Reproduce/).fill("Reproduce\nFix");
  await page.getByRole("button", { name: "Save template" }).click();
  await page.getByText("Bug " + tag).waitFor();
  await shot("templates");
  await page.keyboard.press("c");
  await page.getByRole("button", { name: "Template", exact: true }).click();
  await page
    .getByRole("option", { name: new RegExp("^Bug " + tag + " 2 sub-tasks") })
    .click();
  await page.getByLabel("Issue title").fill("Bug: crash on save " + tag);
  await page.getByRole("button", { name: "Create issue" }).click();
  await page.waitForTimeout(1200);
  const found = await api(
    "GET",
    "/issues?q=" + encodeURIComponent(`"crash on save ${tag}"`),
  );
  const created =
    found.issues?.[0] && (await api("GET", "/issues/" + found.issues[0].key));
  ok(
    created?.children?.length === 2,
    `${tag} issue from template has sub-tasks: ${created?.children?.length}`,
  );

  await page.goto(base + "/p/DEMO/settings?tab=automations");
  await page
    .getByRole("button", {
      name: /New automation|Add automation|Create automation/,
    })
    .first()
    .click();
  await choose(page, "Trigger", "on a schedule");
  await choose(page, "Repeat", "every week");
  await choose(page, "Weekday", "Monday");
  await shot("automation");
  await page.keyboard.press("Escape");

  await page.goto(base + "/admin?tab=ai");
  await page.getByText("Estimate story points").waitFor();
  await shot("admin-ai");
  await page.goto(base + "/settings?tab=notifications");
  await page.getByText("Notifications on this device").waitFor();
  await shot("push");
  await ctx.close();
}

for (const [w, s] of looks([[1440, "dark"], [390, "light"]])) await run(w, s);
await browser.close();
console.log(errors.length ? `${errors.length} errors` : "no errors");
process.exit(errors.length ? 1 : 0);
