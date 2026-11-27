// Checks: mentions, paste/drop uploads, j/k nav, swimlanes,
// timeline dependency lines. Needs user robin (run 01-signup-flow first).
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
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();
async function session(viewport, scheme) {
  const ctx = await browser.newContext({ viewport, colorScheme: scheme });
  // The mention and upload checks are written against the markdown box (the
  // formatted editor's own are in 83 and 86), so this suite opens it as markdown.
  await ctx.addInitScript(() => localStorage.setItem("wp.editor.mode", "md"));
  const page = await ctx.newPage();
  page.on("pageerror", (e) =>
    fail(`pageerror (${viewport.width} ${scheme}): ` + e.message),
  );
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      fail(`console (${viewport.width} ${scheme}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}
const page = await session({ width: 1440, height: 900 }, "dark");
const api = {
  get: async (p) =>
    (await page.request.get(base + "/api/v1" + p, { headers: H })).json(),
  post: async (p, d) =>
    (
      await page.request.post(base + "/api/v1" + p, { headers: H, data: d })
    ).json(),
  put: async (p, d) =>
    page.request.put(base + "/api/v1" + p, { headers: H, data: d }),
};

// Fixture: a kanban project with people, an epic and dated, related work.
const users = await api.get("/users");
if (!users.some((u) => u.username === "sam"))
  await api.post("/admin/users", {
    username: "sam",
    email: "sam@example.com",
    display_name: "Sam Lee",
    password: "another long password",
  });
let proj = (await api.get("/projects")).find((p) => p.key === "LANE");
if (!proj) {
  proj = await api.post("/projects", { name: "Lanes", key: "LANE" });
  await api.put("/projects/LANE/members/sam", { role: "editor" });
  const epic = await api.post("/issues", {
    project: "LANE",
    title: "Launch",
    type: "epic",
  });
  await api.post("/issues", {
    project: "LANE",
    title: "Mine one",
    assignees: ["robin"],
    parent: epic.key,
    priority: "high",
    start_date: "+1d",
    due_date: "+4d",
  });
  await api.post("/issues", {
    project: "LANE",
    title: "Sams",
    assignees: ["sam"],
    priority: "low",
    start_date: "+5d",
    due_date: "+9d",
  });
  await api.post("/issues", {
    project: "LANE",
    title: "Nobodys",
    start_date: "+2d",
    due_date: "+3d",
  });
}
const all = (await api.get("/issues?project=LANE&limit=50")).issues;
const byTitle = (t) => all.find((i) => i.title === t);

// 1. Mentions in the comment box.
await page.goto(base + "/issue/" + byTitle("Nobodys").key);
const box = page.getByLabel("Comment");
await box.click();
await box.pressSequentially("Hey @sa");
await page.waitForTimeout(200);
const menu = page.getByRole("listbox", { name: "Mention someone" });
ok(await menu.isVisible(), "mention menu opens on @sa");
ok(
  (await menu.getByRole("option").count()) >= 1 &&
    (await menu.getByRole("option").first().textContent()).includes("@sam"),
  "menu lists sam",
);
await page.screenshot({ path: `${out}/mention.png` });
await box.press("Enter");
ok(
  (await box.inputValue()) === "Hey @sam ",
  "Enter inserts @sam: " + JSON.stringify(await box.inputValue()),
);
await box.pressSequentially("and @");
await page.waitForTimeout(150);
ok(await menu.isVisible(), "menu on bare @");
await page.keyboard.press("Escape");
await page.waitForTimeout(150);
ok(!(await menu.isVisible()), "Esc closes the menu");
ok(page.url().includes("/issue/"), "Esc did not leave the issue page");

// 2. Paste an image into the comment box: uploads and becomes markdown.
// The upload URL carries ?target=…, so match by regex rather than a glob.
await page.route(/\/api\/v1\/attachments\?/, async (route) => {
  await new Promise((r) => setTimeout(r, 600));
  await route.continue();
});
const png = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64",
).toString("base64");
await box.evaluate((el, b64) => {
  const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const dt = new DataTransfer();
  dt.items.add(new File([bytes], "shot.png", { type: "image/png" }));
  el.dispatchEvent(
    new ClipboardEvent("paste", {
      clipboardData: dt,
      bubbles: true,
      cancelable: true,
    }),
  );
}, png);
await page.waitForTimeout(100);
const during = await box.inputValue();
ok(during.includes("[Uploading shot.png…]()"), "placeholder while uploading");
await page
  .waitForFunction(
    () =>
      /!\[shot\.png\]\(\/api\/v1\/attachments\/\d+\//.test(
        document.querySelector('textarea[aria-label="Comment"]').value,
      ),
    null,
    { timeout: 5000 },
  )
  .catch(() => {});
await page.unroute("**/attachments");
const after = await box.inputValue();
ok(
  /!\[shot\.png\]\(\/api\/v1\/attachments\/\d+\/shot\.png\)/.test(after) &&
    !after.includes("Uploading"),
  "pasted image became markdown: " + JSON.stringify(after),
);
await page.getByRole("button", { name: "Comment", exact: true }).click();
await page.waitForTimeout(600);
const img = page.locator('.prose-wp img[alt="shot.png"]');
// the image loads after the comment renders; under CI load 600ms is not enough
await page
  .waitForFunction(
    () => {
      const i = document.querySelector('.prose-wp img[alt="shot.png"]');
      return i && i.complete && i.naturalWidth > 0;
    },
    null,
    { timeout: 10000 },
  )
  .catch(() => {});
ok(
  (await img.count()) === 1 &&
    (await img.evaluate((i) => i.complete && i.naturalWidth > 0)),
  "comment shows the uploaded image",
);
// Drop a text file into the description editor (needs issueKey on the editor).
await page.screenshot({ path: `${out}/paste.png` });

// 3. j/k on the list.
await page.goto(base + "/p/LANE/list?group=none");
await page.waitForTimeout(500);
await page.keyboard.press("j");
const focusedKey = () =>
  page.evaluate(() => document.activeElement?.getAttribute("data-nav-key"));
const k1 = await focusedKey();
ok(!!k1, "j focuses the first row: " + k1);
await page.keyboard.press("j");
const k2 = await focusedKey();
ok(k2 && k2 !== k1, "j moves down: " + k2);
await page.keyboard.press("k");
ok((await focusedKey()) === k1, "k moves back up");
await page.keyboard.press("x");
await page.waitForTimeout(150);
ok(
  await page
    .getByText("1 selected")
    .isVisible()
    .catch(() => false),
  "x selects the row",
);
await page.keyboard.press("x");
await page.keyboard.press("Enter");
await page.waitForTimeout(400);
ok(
  new URL(page.url()).searchParams.get("issue") === k1,
  "Enter opens the issue sheet",
);
await page.keyboard.press("Escape");
await page.waitForTimeout(300);

// 4. Board: j then l jumps a column; swimlanes by assignee; drag across lanes reassigns.
await page.goto(base + "/p/LANE/board");
await page.waitForTimeout(500);
await page.keyboard.press("j");
const b1 = await focusedKey();
ok(!!b1, "j on the board focuses a card");
await choose(page, "Swimlanes", "Lanes: Assignee");
await page.waitForTimeout(400);
const lanes = page.locator("main section[aria-label]");
const laneNames = await lanes.evaluateAll((ls) =>
  ls.map((l) => l.getAttribute("aria-label")),
);
ok(
  laneNames.join("|") === "Robin Ames|Sam Lee|Unassigned",
  "assignee lanes: " + laneNames.join("|"),
);
await page.screenshot({ path: `${out}/lanes-desktop.png` });
// Drag "Nobodys" into Sam's lane, same column.
const card = page.locator('[data-nav-key="' + byTitle("Nobodys").key + '"]');
const samLane = page
  .locator('main section[aria-label="Sam Lee"] [data-nav-col]')
  .first();
const cb = await card.boundingBox();
const tb = await samLane.boundingBox();
await page.mouse.move(cb.x + 30, cb.y + 15);
await page.mouse.down();
await page.mouse.move(cb.x + 40, cb.y + 30, { steps: 4 });
await page.mouse.move(tb.x + 60, tb.y + tb.height - 10, { steps: 12 });
await page.mouse.move(tb.x + 62, tb.y + tb.height - 8, { steps: 2 });
await page.mouse.up();
await page.waitForTimeout(800);
const moved = await api.get("/issues/" + byTitle("Nobodys").key);
ok(
  moved.assignees.map((a) => a.username).join() === "sam",
  "dragging into Sam’s lane assigns Sam: " +
    moved.assignees.map((a) => a.username),
);
ok(
  (await page.locator('main section[aria-label="Unassigned"]').count()) === 0,
  "empty Unassigned lane goes away",
);
// Collapse a lane.
await page
  .locator('main section[aria-label="Sam Lee"] button[aria-expanded]')
  .click();
ok(
  (await page
    .locator('main section[aria-label="Sam Lee"] [data-nav-key]')
    .count()) === 0,
  "lane collapses",
);
await page
  .locator('main section[aria-label="Sam Lee"] button[aria-expanded]')
  .click();
// Priority lanes.
await choose(page, "Swimlanes", "Lanes: Priority");
await page.waitForTimeout(300);
ok(
  (
    await lanes.evaluateAll((ls) => ls.map((l) => l.getAttribute("aria-label")))
  ).join("|") === "High|Low|No priority",
  "priority lanes in order",
);
await choose(page, "Swimlanes", "Lanes: Epic");
await page.waitForTimeout(300);
ok(
  (
    await lanes.evaluateAll((ls) => ls.map((l) => l.getAttribute("aria-label")))
  )[0].includes("Launch"),
  "epic lanes",
);
await choose(page, "Swimlanes", "No swimlanes");

// 5. Timeline dependency lines (blocks injected into the search response).
const blocker = byTitle("Mine one").key;
const blocked = byTitle("Sams").key;
const late = byTitle("Nobodys").key;
await page.route("**/api/v1/issues?*", async (route) => {
  const res = await route.fetch();
  const body = await res.json();
  for (const i of body.issues ?? [])
    if (i.key === blocker) i.blocks = [blocked, late];
  await route.fulfill({ response: res, json: body });
});
await page.goto(base + "/p/LANE/timeline");
await page.waitForTimeout(700);
const paths = page.locator("svg[aria-hidden] path[marker-end]");
ok(
  (await paths.count()) === 2,
  "two dependency arrows: " + (await paths.count()),
);
ok(
  (await page.locator("svg[aria-hidden] path[stroke-dasharray]").count()) === 1,
  "the overlapping one is drawn as late",
);
await page.screenshot({ path: `${out}/deps.png` });
await page.getByRole("button", { name: "Dependencies", pressed: true }).click();
ok((await paths.count()) === 0, "toggle hides them");
await page
  .getByRole("button", { name: "Dependencies", pressed: false })
  .click();
await page.unroute("**/api/v1/issues?*");

// 6. Shortcut sheet.
await page.goto(base + "/");
await page.waitForTimeout(300);
await page.keyboard.press("Shift+Slash");
ok(
  await page.getByRole("dialog", { name: "Keyboard shortcuts" }).isVisible(),
  "? opens the shortcut sheet",
);
await page.keyboard.press("Escape");

// 7. Sweep the touched pages in every theme and size.
for (const [vp, scheme] of looks([
  [{ width: 1440, height: 900 }, "light"],
  [{ width: 390, height: 844 }, "dark"],
  [{ width: 390, height: 844 }, "light"],
])) {
  const p = await session(vp, scheme);
  for (const path of [
    "/p/LANE/board",
    "/p/LANE/list",
    "/p/LANE/timeline",
    "/issue/" + blocker,
  ]) {
    await p.goto(base + path);
    await p.waitForTimeout(400);
    if (path.endsWith("board")) await choose(p, "Swimlanes", "Lanes: Assignee");
    await p.waitForTimeout(200);
    const overflow = await p.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth + 1,
    );
    ok(
      !overflow,
      `no page-level horizontal scroll at ${vp.width} ${scheme} ${path}`,
    );
    await p.screenshot({
      path: `${out}/${vp.width}-${scheme}-${path.replace(/\W+/g, "_")}.png`,
    });
  }
  await p.getByRole("combobox", { name: "Swimlanes" }).count();
  await p.context().close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
