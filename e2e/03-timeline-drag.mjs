// Drags a timeline bar and checks the saved dates moved.
import { chromium } from "playwright";
const [base, out] = process.argv.slice(2);
const browser = await chromium.launch();
const page = await (
  await browser.newContext({
    viewport: { width: 1440, height: 900 },
    colorScheme: "dark",
  })
).newPage();
const errs = [];
page.on("pageerror", (e) => errs.push(e.message));
await page.goto(base + "/login");
await page.getByLabel("Username or email").fill("robin");
await page.getByLabel("Password").fill("correct horse battery");
await page.getByRole("button", { name: "Sign in" }).click();
await page.waitForURL(base + "/");
const h = { "X-Waypoint-Client": "web", "Content-Type": "application/json" };
await page.request.patch(base + "/api/v1/issues/WAYP-1", {
  headers: h,
  data: { start_date: "2026-10-12", due_date: "2026-10-16" },
});
await page.request.patch(base + "/api/v1/issues/WAYP-2", {
  headers: h,
  data: { due_date: "2026-10-20", parent: "WAYP-6" },
});
await page.goto(base + "/p/WAYP/timeline");
await page.getByRole("button", { name: "week", exact: true }).click();
await page.waitForTimeout(700);
const bar = page.locator('[title^="WAYP-1:"]');
const b = await bar.boundingBox();
await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
await page.mouse.down();
await page.mouse.move(b.x + b.width / 2 + 44 * 7, b.y + b.height / 2, {
  steps: 10,
});
await page.screenshot({ path: out + "/timeline-dragging.png" });
await page.mouse.up();
await page.waitForTimeout(800);
await page.screenshot({ path: out + "/timeline-after.png" });
const issue = await (
  await page.request.get(base + "/api/v1/issues/WAYP-1")
).json();
console.log(
  "dates",
  issue.start_date,
  issue.due_date,
  errs.length ? errs : "no errors",
);
await browser.close();
