// Signs up through the first-run invite, creates the WAYP project and walks
// the main flow: quick-add, sprints, drag to board, sub-issues, comments.
import { chromium } from "playwright";
import fs from "node:fs";
import { fillMarkdown, mdBox } from "./editor.mjs";

const base = process.argv[2];
const invite = process.argv[3];
const out = process.argv[4];
fs.mkdirSync(out, { recursive: true });
const errors = [];

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  colorScheme: "dark",
});
const page = await ctx.newPage();
page.on(
  "console",
  (m) =>
    m.type() === "error" &&
    (console.log("CONSOLE", m.text().slice(0, 600)),
    errors.push("console: " + m.text())),
);
page.on(
  "pageerror",
  (e) => (
    console.log(
      "PAGEERROR",
      e.message,
      e.stack?.split("\n").slice(0, 6).join(" | "),
    ),
    errors.push("pageerror: " + e.message)
  ),
);
page.on(
  "response",
  (r) =>
    r.status() >= 400 &&
    r.url().includes("/api/") &&
    errors.push(`http ${r.status()} ${r.request().method()} ${r.url()}`),
);
const shot = (name) => page.screenshot({ path: `${out}/${name}.png` });

await page.goto(invite);
await page.getByLabel("Your name").fill("Robin Ames");
await page.getByLabel("Username").fill("robin");
await page.getByLabel("Email").fill("robin@example.com");
await page.getByLabel("Password").fill("correct horse battery");
await shot("01-invite");
await page.getByRole("button", { name: "Create account" }).click();
await page.waitForURL(base + "/");
await shot("02-home-empty");

// New project via sidebar.
await page.getByRole("button", { name: "New project" }).first().click();
await page.getByLabel("Name").fill("Waypoint");
await page.getByRole("button", { name: /Sprints/ }).click();
await shot("03-new-project");
await page.getByRole("button", { name: "Create project" }).click();
await page.waitForURL(/\/p\/WAYP\/board/);
await shot("04-board-empty-sprints");

// Backlog: add issues with quick syntax.
await page.getByRole("link", { name: "Backlog", exact: true }).click();
await page.waitForURL(/backlog/);
const add = page.getByPlaceholder(/Add to backlog/);
for (const t of [
  "Design the board view !high ~5 #ui",
  "Write the API docs ~3",
  "Fix login redirect /bug !urgent ~2",
  "Set up CI ~1",
  "Mobile polish #ui ~3",
]) {
  await add.fill(t);
  await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/api/v1/issues") && r.request().method() === "POST",
    ),
    add.press("Enter"),
  ]);
}
// Epic.
await page.getByPlaceholder("+ Add epic").fill("v1 launch");
await page.getByPlaceholder("+ Add epic").press("Enter");
await page.waitForTimeout(500);
await page.getByRole("button", { name: "Create sprint" }).click();
await page.getByRole("button", { name: "Create sprint" }).last().click();
await page.waitForTimeout(600);
await shot("05-backlog");

// Drag two issues into the sprint.
const sprintZone = page
  .locator("section", { hasText: "WAYP Sprint 1" })
  .first();
for (const title of ["Design the board view", "Fix login redirect"]) {
  const row = page.getByText(title).first();
  const rb = await row.boundingBox();
  const zb = await sprintZone.boundingBox();
  await page.mouse.move(rb.x + 40, rb.y + rb.height / 2);
  await page.mouse.down();
  await page.mouse.move(rb.x + 60, rb.y + 20, { steps: 4 });
  await page.mouse.move(zb.x + 200, zb.y + 60, { steps: 12 });
  await page.waitForTimeout(150);
  await page.mouse.up();
  await page.waitForTimeout(700);
  await shot("06-after-drag-" + title.split(" ")[0]);
}
await shot("06-backlog-planned");
await page.getByRole("button", { name: "Start" }).click();
await page.waitForTimeout(700);

// Board.
await page.getByRole("link", { name: "Board", exact: true }).click();
await page.waitForURL(/board/);
await page.waitForTimeout(500);
await shot("07-board");

// Drag a card to In Progress.
const card = page.getByText("Design the board view").first();
const col = page.locator("section", { hasText: "In Progress" }).first();
const cb = await card.boundingBox();
const colb = await col.boundingBox();
await page.mouse.move(cb.x + 20, cb.y + 10);
await page.mouse.down();
await page.mouse.move(cb.x + 40, cb.y + 30, { steps: 4 });
await page.mouse.move(colb.x + 120, colb.y + 80, { steps: 15 });
await page.waitForTimeout(150);
await page.mouse.up();
await page.waitForTimeout(800);
await shot("08-board-moved");

// Open the detail sheet.
await page.getByText("Fix login redirect").first().click();
await page.waitForTimeout(800);
await shot("09-issue-sheet");
await page.getByRole("button", { name: "Add sub-issue" }).click({ timeout: 3000 }).catch(() => {});
await page.getByPlaceholder(/Add a subtask/).fill("Reproduce on Safari @robin");
await page.getByPlaceholder(/Add a subtask/).press("Enter");
await page.waitForTimeout(600);
await fillMarkdown(
  mdBox(page, { placeholder: /Leave a comment/ }),
  "Looks like the **next** param is dropped. See WAYP-1.",
);
await page.keyboard.press("Control+Enter");
await page.waitForTimeout(600);
await shot("10-issue-sheet-filled");
await page.keyboard.press("Escape");
await page.keyboard.press("Escape");

// Command palette.
await page.keyboard.press("Control+k");
await page.keyboard.type("login");
await page.waitForTimeout(500);
await shot("11-palette");
await page.keyboard.press("Escape");

// List + home.
await page.getByRole("link", { name: "List", exact: true }).click();
await page.waitForTimeout(700);
await shot("12-list");
await page.goto(base + "/");
await page.waitForTimeout(700);
await shot("13-home");

// Light mode + mobile.
const m = await browser.newContext({
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 2,
  hasTouch: true,
  isMobile: true,
  colorScheme: "light",
  storageState: await ctx.storageState(),
});
const mp = await m.newPage();
mp.on("pageerror", (e) => errors.push("mobile pageerror: " + e.message));
await mp.goto(base + "/p/WAYP/board");
await mp.waitForTimeout(800);
await mp.screenshot({ path: `${out}/20-mobile-board.png` });
await mp.goto(base + "/issue/WAYP-3");
await mp.waitForTimeout(800);
await mp.screenshot({ path: `${out}/21-mobile-issue.png` });
await mp.goto(base + "/");
await mp.waitForTimeout(800);
await mp.screenshot({ path: `${out}/22-mobile-home.png` });

fs.writeFileSync(`${out}/errors.txt`, errors.join("\n"));
console.log(errors.length ? errors.join("\n") : "no errors");
await browser.close();
