// Visits every page and settings tab in light/dark, desktop and 390px.
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const browser = await chromium.launch();
for (const [scheme, vp] of looks([
  ["dark", { width: 1440, height: 900 }],
  ["light", { width: 390, height: 844 }],
])) {
  const ctx = await browser.newContext({
    viewport: vp,
    colorScheme: scheme,
    deviceScaleFactor: vp.width < 500 ? 2 : 1,
  });
  const page = await ctx.newPage();
  page.on("pageerror", (e) =>
    errors.push(`${scheme} pageerror ${page.url()}: ${e.message}`),
  );
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      errors.push(`${scheme} console ${page.url()}: ${m.text().slice(0, 300)}`),
  );
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      errors.push(
        `${scheme} http ${r.status()} ${r.request().method()} ${r.url()}`,
      ),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const pages = [
    "p/WAYP/timeline",
    "p/WAYP/reports",
    "p/WAYP/settings",
    "p/WAYP/settings?tab=workflow",
    "p/WAYP/settings?tab=labels",
    "p/WAYP/settings?tab=members",
    "p/WAYP/settings?tab=automations",
    "p/WAYP/settings?tab=integrations",
    "settings",
    "settings?tab=appearance",
    "settings?tab=password",
    "settings?tab=sessions",
    "settings?tab=notifications",
    "settings?tab=tokens",
    "settings?tab=connections",
    "admin",
    "admin?tab=invites",
    "admin?tab=forges",
    "admin?tab=webhooks",
    "time",
    "inbox",
    "search?q=is:open",
  ];
  for (const p of pages) {
    await page.goto(`${base}/${p}`);
    await page.waitForTimeout(900);
    await page.screenshot({
      path: `${out}/${scheme}-${p.replace(/[/?=:]/g, "_")}.png`,
      fullPage: false,
    });
  }
  await ctx.close();
}
fs.writeFileSync(`${out}/errors.txt`, errors.join("\n"));
console.log(errors.length ? errors.join("\n") : "no errors");
await browser.close();
