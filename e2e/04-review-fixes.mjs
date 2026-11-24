// Checks for the frontend review fixes. Run after 01-signup-flow (needs user robin + project WAYP).
import { chromium } from "playwright";
import fs from "node:fs";
import { execSync } from "node:child_process";
const [base, out, restart] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  colorScheme: "dark",
});
const page = await ctx.newPage();
let expectErrors = false;
page.on("pageerror", (e) => fail("pageerror: " + e.message));
page.on(
  "console",
  (m) =>
    m.type() === "error" &&
    !expectErrors &&
    fail("console: " + m.text().slice(0, 300)),
);
const H = { "X-Waypoint-Client": "web" };
const apiGet = async (p) =>
  (await page.request.get(base + "/api/v1" + p, { headers: H })).json();
const apiPost = async (p, d) =>
  (
    await page.request.post(base + "/api/v1" + p, { headers: H, data: d })
  ).json();
const apiPatch = async (p, d) =>
  (
    await page.request.patch(base + "/api/v1" + p, { headers: H, data: d })
  ).json();

// Open redirect: //evil.com must not leave the app.
await page.goto(base + "/login?next=" + encodeURIComponent("//evil.example/x"));
await page.getByLabel("Username or email").fill("robin");
await page.getByLabel("Password").fill("correct horse battery");
await page.getByRole("button", { name: "Sign in" }).click();
await page.waitForTimeout(800);
ok(page.url() === base + "/", "login next=//evil stays home: " + page.url());

// Markdown: raw form controls/styles stripped, code untouched, tasks toggle the right line.
const desc = [
  "Intro WAYP-1 and @robin.",
  '<style>body{display:none}</style><form action="/x"><input name="q" value="hi"><button>Go</button></form>',
  '<span style="position:fixed;inset:0;background:red">overlay</span>',
  "```",
  "- [ ] in code WAYP-2",
  "```",
  "- [ ] first",
  "1. [ ] second",
  "> - [ ] quoted",
  "`WAYP-3` inline code",
].join("\n");
const created = await apiPost("/issues", {
  project: "WAYP",
  title: "Markdown probe",
  description: desc,
});
await page.goto(`${base}/issue/${created.key}`);
await page.waitForTimeout(800);
const prose = page.locator(".prose-wp").first();
ok(
  // Copy buttons are added after sanitising, so only the markdown's own buttons count here.
  (await prose
    .locator("style, form, button:not(.code-copy), input:not(.task)")
    .count()) === 0,
  "raw style/form/input/button stripped",
);
ok((await prose.locator("[style]").count()) === 0, "style attributes stripped");
ok(
  (await prose.locator("input.task").count()) === 3,
  "three task checkboxes: " + (await prose.locator("input.task").count()),
);
ok(
  (await prose.locator("code a, pre a").count()) === 0,
  "no links inside code",
);
ok(
  (await prose.locator('a[href="/issue/WAYP-1"]').count()) === 1,
  "issue key linked in text",
);
ok(
  (await prose.locator(".mention").first().textContent()) === "@robin",
  "mention drops trailing dot",
);
await prose.locator("input.task").nth(1).click();
await page.waitForTimeout(700);
let d = (await apiGet(`/issues/${created.key}`)).description;
ok(
  d.includes("1. [x] second") &&
    d.includes("- [ ] in code") &&
    d.includes("- [ ] first"),
  "toggle hit the ordered task only",
);
await prose.locator("input.task").nth(2).click();
await page.waitForTimeout(700);
d = (await apiGet(`/issues/${created.key}`)).description;
ok(d.includes("> - [x] quoted"), "toggle in blockquote");
// Issue links navigate in-app (no full reload).
await page.evaluate(() => (window.__marker = 1));
await prose.locator('a[href="/issue/WAYP-1"]').click();
await page.waitForURL(/\/issue\/WAYP-1$/);
ok(
  await page.evaluate(() => window.__marker === 1),
  "issue link navigates without reload",
);

// Escape in the title editor inside the sheet doesn't save; in the description it saves and closes the editor (WP-81), then Esc closes the sheet.
await page.goto(`${base}/p/WAYP/board?issue=${created.key}`);
await page.waitForTimeout(800);
const title = page.getByRole("textbox", { name: "Title" });
await title.click();
await title.fill("Changed but cancelled");
await page.keyboard.press("Escape");
await page.waitForTimeout(600);
ok(
  (await apiGet(`/issues/${created.key}`)).title === "Markdown probe",
  "Esc in title does not save",
);
ok(
  await page.getByRole("dialog", { name: created.key }).isVisible(),
  "sheet still open after first Esc",
);
await page
  .locator("[role=dialog] .prose-wp")
  .first()
  .click({ position: { x: 5, y: 5 } });
const ta = page.locator("[role=dialog] textarea").filter({ hasText: "Intro" });
await ta.fill("typed then escaped");
await page.keyboard.press("Escape");
await page.waitForTimeout(500);
ok(
  (await apiGet(`/issues/${created.key}`)).description === "typed then escaped",
  "Esc in description saves and closes the editor",
);
ok(
  await page.getByRole("dialog", { name: created.key }).isVisible(),
  "sheet still open after editor Esc",
);
await page.keyboard.press("Escape");
await page.waitForTimeout(400);
ok(
  !(await page
    .getByRole("dialog", { name: created.key })
    .isVisible()
    .catch(() => false)),
  "next Esc closes the sheet",
);

// Comment: Edit opens the editor directly; delete asks first.
await page.goto(`${base}/issue/${created.key}`);
await page.getByLabel("Comment").fill("first comment");
await page.getByRole("button", { name: "Comment", exact: true }).click();
await page.waitForTimeout(600);
await page.getByRole("button", { name: "Comment actions" }).first().click();
// Menus are keyboard-reachable: focus lands on the first item.
ok(
  await page.evaluate(
    () => document.activeElement?.getAttribute("role") === "menuitem",
  ),
  "menu focuses first item",
);
await page.keyboard.press("ArrowDown");
ok(
  await page.evaluate(() => document.activeElement?.textContent === "Delete"),
  "ArrowDown moves to Delete",
);
await page.keyboard.press("ArrowUp");
await page.keyboard.press("Enter");
await page.waitForTimeout(300);
ok(
  (await page
    .locator("textarea")
    .filter({ hasText: "first comment" })
    .count()) === 1,
  "Edit opens editor at once",
);
await page
  .locator("textarea")
  .filter({ hasText: "first comment" })
  .fill("edited comment");
await page.keyboard.press("Control+Enter");
await page.waitForTimeout(700);
const comments = await apiGet(`/issues/${created.key}/comments`);
ok(comments[0]?.body === "edited comment", "comment edit saved");
await page.getByRole("button", { name: "Comment actions" }).first().click();
await page.getByRole("menuitem", { name: "Delete" }).click();
ok(
  await page.getByRole("dialog", { name: "Delete this comment?" }).isVisible(),
  "comment delete asks to confirm",
);
await page.getByRole("button", { name: "Delete", exact: true }).click();
await page.waitForTimeout(600);
ok(
  (await apiGet(`/issues/${created.key}/comments`)).length === 0,
  "comment deleted after confirm",
);

// Picker: ArrowDown + Enter with no matches doesn't throw.
await page
  .getByTitle(/^Priority: /)
  .locator("visible=true")
  .first()
  .click();
await page.keyboard.type("zzzz");
await page.keyboard.press("ArrowDown");
await page.keyboard.press("Enter");
await page.keyboard.press("Escape");

// Dialog focus trap: Tab cycles inside the create dialog.
await page.keyboard.press("c");
await page.waitForTimeout(300);
for (let i = 0; i < 25; i++) await page.keyboard.press("Tab");
ok(
  await page.evaluate(() => !!document.activeElement?.closest("[role=dialog]")),
  "Tab stays in dialog",
);
await page.keyboard.press("Escape");

// Double Enter in quick add creates one issue.
const before = (
  await apiGet("/issues?q=" + encodeURIComponent('project:WAYP "Dup check"'))
).total;
await page.goto(`${base}/p/WAYP/backlog`);
const add = page.getByPlaceholder(/Add to backlog/);
await add.fill("Dup check");
await add.press("Enter");
await add.press("Enter");
const countDup = async () =>
  (await apiGet("/issues?q=" + encodeURIComponent('project:WAYP "Dup check"')))
    .total;
// Wait for the first add to land (a busy runner can take seconds), then give a
// duplicate time to show up.
for (let i = 0; i < 40 && (await countDup()) === before; i++)
  await page.waitForTimeout(250);
await page.waitForTimeout(900);
const after = await countDup();
ok(after - before === 1, `double Enter adds once (${after - before})`);

// Token expiry typo is rejected rather than creating an immortal token.
await page.goto(`${base}/settings?tab=tokens`);
await page
  .getByRole("button", { name: /New token|Create a token|New API token/ })
  .first()
  .click();
await page.getByLabel("Name").fill("probe");
await page.getByLabel("Expires after (days)").fill("-7");
ok(
  await page.getByText("Use a whole number of days").isVisible(),
  "bad expiry shows an error",
);
ok(
  await page.getByRole("button", { name: "Create token" }).isDisabled(),
  "create disabled for bad expiry",
);
await page.keyboard.press("Escape");

// Prefs: two quick toggles both persist.
await apiPatch("/me", {
  prefs: {
    gotify_url: "https://gotify.example.com",
    gotify_token: "tok",
    week_start: 1,
  },
});
await page.goto(`${base}/settings?tab=notifications`);
await page.waitForTimeout(500);
const boxes = page.locator("input[type=checkbox]");
await boxes.nth(0).click();
await boxes.nth(1).click();
await page.waitForTimeout(1200);
const muted = (await apiGet("/me")).prefs.gotify_muted ?? [];
ok(
  muted.length === 2,
  "two quick mute toggles both saved: " + JSON.stringify(muted),
);

// Timeline at phone width: label column is narrow.
const m = await browser.newContext({
  viewport: { width: 390, height: 844 },
  colorScheme: "light",
  storageState: await ctx.storageState(),
});
const mp = await m.newPage();
mp.on("pageerror", (e) => fail("mobile pageerror: " + e.message));
await mp.goto(`${base}/p/WAYP/timeline`);
await mp.waitForTimeout(900);
const w = await mp
  .locator("text=Issue")
  .first()
  .evaluate((el) => el.getBoundingClientRect().width)
  .catch(() => 0);
ok(w > 0 && w <= 160, "timeline label column narrow on phone: " + w);
await mp.screenshot({ path: `${out}/timeline-390.png` });
await m.close();

// Live updates come back after the server restarts.
if (restart) {
  await page.goto(`${base}/p/WAYP/backlog`);
  await page.waitForTimeout(800);
  expectErrors = true;
  execSync(`${restart} keep`);
  await page.waitForTimeout(16000);
  expectErrors = false;
  const t = "Arrived live " + Date.now();
  await apiPost("/issues", { project: "WAYP", title: t });
  await page.waitForTimeout(2500);
  ok(
    await page.getByText(t).isVisible(),
    "live updates resume after a server restart",
  );
}

await browser.close();
console.log(
  errors.length ? `${errors.length} failures` : "all fe checks passed",
);
process.exit(errors.length ? 1 : 0);
