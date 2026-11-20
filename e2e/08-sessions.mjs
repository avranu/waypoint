// Lists sessions and signs out another one.
import { chromium } from "playwright";
const [base, out] = process.argv.slice(2);
const browser = await chromium.launch();
const errs = [];
const login = async (ua, vp) => {
  const ctx = await browser.newContext({ userAgent: ua, viewport: vp });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => errs.push(e.message));
  page.on(
    "console",
    (m) => m.type() === "error" && !/401/.test(m.text()) && errs.push(m.text()),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
};
const a = await login(
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36",
  { width: 1440, height: 900 },
);
const b = await login(
  "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
  { width: 390, height: 844 },
);
await a.goto(base + "/settings?tab=sessions");
await a.waitForTimeout(600);
await a.getByRole("button", { name: /\d+ others?/ }).click();
const rows = await a.locator("li", { hasText: /active/ }).count();
console.log(
  "sessions listed:",
  rows,
  "| this device badge:",
  await a.getByText("This device").count(),
  "| iOS row:",
  await a.getByText("Safari on iOS").count(),
);
await a.screenshot({ path: (out ?? ".") + "/sessions.png" });
await a.getByRole("button", { name: "Sign out others" }).click();
await a.getByRole("button", { name: "Sign out", exact: true }).click();
await a.waitForTimeout(700);
const bStatus = (
  await b.request.get(base + "/api/v1/me", {
    headers: { "X-Waypoint-Client": "web" },
  })
).status();
console.log(
  "other session after sign-out-others:",
  bStatus,
  "| this one:",
  (
    await a.request.get(base + "/api/v1/me", {
      headers: { "X-Waypoint-Client": "web" },
    })
  ).status(),
);
console.log(errs.length ? "ERRORS " + errs.join(" | ") : "no errors");
await browser.close();
