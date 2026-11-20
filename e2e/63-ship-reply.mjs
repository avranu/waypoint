// The reply agents end with after wp ship (WP-390): Admin → AI words the
// template; the preview fills in an example pull request; saving keeps it
// and "Use the default" puts it back. Desktop and 390px.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();

for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = (p) => page.request.get(base + "/api/v1" + p, { headers: H }).then((r) => r.json());

  await page.goto(`${base}/admin?tab=ai`);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Agents’ reply after wp ship" }) });
  const box = section.getByRole("textbox");
  await box.waitFor();
  ok((await box.inputValue()).includes("{task} shipped in PR #{pr}"), `${tag} shows the default template`);
  ok(await section.getByText("WP-123 shipped in PR #45").isVisible(), `${tag} the preview fills in an example`);

  await box.fill("{task} is in #{pr} ({url}).\nNothing else; clear away.");
  ok(await section.getByText("WP-123 is in #45 (https://git.example.com/acme/app/pulls/45).").isVisible(), `${tag} the preview follows the edit`);
  await section.getByRole("button", { name: "Save" }).click();
  await page.getByText("Reply template saved").waitFor();
  const saved = await api("/admin/ship-reply");
  ok(saved.custom && saved.reply === "{task} is in #{pr} ({url}).\nNothing else; clear away.", `${tag} saved: ${JSON.stringify(saved)}`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await section.scrollIntoViewIfNeeded();
  await section.screenshot({ path: `${out}/ship-reply-${width}-${scheme}.png` });

  await page.reload();
  await box.waitFor();
  ok((await box.inputValue()).startsWith("{task} is in #{pr}"), `${tag} kept after a reload`);
  await section.getByRole("button", { name: "Use the default" }).click();
  await page.getByText("Reply template back to the default").waitFor();
  ok(!(await api("/admin/ship-reply")).custom, `${tag} back to the default`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
