// Drafts (WP-42): quick capture with D, the Draft marker in the list, on
// the board and on the issue, publishing; draft pages from the Pages tree.
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
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`),
  );
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  await page.goto(`${base}/p/WAYP/list`);
  // D is bound once the app shell has mounted; on a slow runner a key
  // pressed straight after goto lands before that.
  await page.getByRole("button", { name: "Filter (F)" }).waitFor();
  await page.locator("body").press("d");
  const dialog = page.getByRole("dialog");
  await dialog.getByText("New draft").waitFor();
  ok(
    (await dialog
      .getByRole("switch", { name: "Draft" })
      .getAttribute("aria-checked")) === "true",
    `${tag} D opens a draft`,
  );
  const title = `Half a thought ${tag}`;
  await dialog.getByLabel("Issue title").fill(title);
  await shot("capture");
  await dialog.getByRole("button", { name: "Save draft" }).click();
  await dialog.waitFor({ state: "detached" });

  const row = page.locator("[data-nav-key]", { hasText: title });
  await row.waitFor();
  ok(
    await row.getByText("Draft", { exact: true }).isVisible(),
    `${tag} list marks the draft`,
  );
  const key = await row.getAttribute("data-nav-key");
  await shot("list");
  const boardTitle = `Board draft ${tag}`;
  await page.request.post(base + "/api/v1/issues", {
    headers: { "X-Waypoint-Client": "web" },
    data: { project: "WAYP", title: boardTitle, sprint: "active", draft: true },
  });
  await page.goto(`${base}/p/WAYP/board`);
  const card = page.locator("[data-nav-key]", { hasText: boardTitle });
  await card.first().waitFor();
  ok(
    await card.first().getByText("Draft", { exact: true }).isVisible(),
    `${tag} board marks the draft`,
  );

  await page.goto(`${base}/issue/${key}`);
  const note = page
    .getByRole("note")
    .filter({ hasText: "Agents can't see this issue" });
  await note.waitFor();
  await shot("issue");
  await note.getByRole("button", { name: "Publish" }).click();
  await note.waitFor({ state: "detached" });
  ok(true, `${tag} published ${key}`);

  await page.goto(`${base}/pages`);
  await page.getByRole("button", { name: "Draft", exact: true }).click();
  await page.waitForURL(/\/pages\/\d+/);
  const pnote = page
    .getByRole("note")
    .filter({ hasText: "Agents can't see this page" });
  await pnote.waitFor();
  await shot("page");
  await pnote.getByRole("button", { name: "Publish" }).click();
  await pnote.waitFor({ state: "detached" });
  ok(true, `${tag} published the page`);
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [390, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
