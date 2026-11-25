// No lost text (WP-68): failed saves keep what was typed and say why; long
// text in a title field becomes a short title plus the full text as the
// description; unsent quick-add, new-issue and title text survives a
// refresh; leaving with text that can't be kept asks first.
// Run after 01-signup-flow (user robin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
import { fillMarkdown, markdownOf, mdBox } from "./editor.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();

// Long enough to pass the 500-byte title limit, sentence first like Robin types.
const long = (tag) =>
  `Fix the flaky sync for ${tag}. ` +
  "The webhook arrives before the push is visible, so the head is stale and the review runs on the old commit. ".repeat(
    6,
  );

async function login(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

// Fixture: a kanban project of its own.
{
  const ctx = await browser.newContext();
  const page = await login(ctx);
  const projects = await (
    await page.request.get(base + "/api/v1/projects", { headers: H })
  ).json();
  if (!projects.some((p) => p.key === "LOST"))
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Lost text", key: "LOST" },
    });
  await ctx.close();
}

async function run(width) {
  const tag = `w${width}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: "light",
  });
  const page = await login(ctx);
  let forced = 0;
  let forcedTotal = 0;
  let excused = 0;
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) => {
      if (m.type() !== "error") return;
      if (/status of (400|500)/.test(m.text()) && excused < forcedTotal) {
        excused++;
        return;
      }
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
    },
  );
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}.png` });
  const api = async (p) =>
    (await page.request.get(base + "/api/v1" + p, { headers: H })).json();
  const failNext = (
    method,
    pattern,
    status = 500,
    msg = "the database is busy",
  ) =>
    page.route(pattern, (route) =>
      route.request().method() === method && forced < 1
        ? (forced++,
          forcedTotal++,
          route.fulfill({
            status,
            contentType: "application/json",
            body: JSON.stringify({ error: msg }),
          }))
        : route.continue(),
    );

  await page.goto(`${base}/p/LOST/board`);
  const addBtn = page.getByRole("button", {
    name: "Add to To Do",
    exact: true,
  });
  await addBtn.click();
  const box = page.getByLabel("New issue title");
  const t1 = `Survives a failure ${tag}`;
  await box.fill(t1);
  forced = 0;
  await failNext("POST", "**/api/v1/issues");
  await box.press("Enter");
  const alert1 = page
    .getByRole("alert")
    .filter({ hasText: "the database is busy" });
  await alert1
    .first()
    .waitFor({ timeout: 5000 })
    .catch(() => {});
  ok(
    await alert1.first().isVisible(),
    `${tag} quick add shows the server's message inline`,
  );
  ok(
    (await box.inputValue()) === t1,
    `${tag} quick add keeps the text after a failed create`,
  );
  await shot("quickadd-error");
  await page.unroute("**/api/v1/issues");
  await box.press("Enter");
  await page.locator("[data-nav-key]", { hasText: t1 }).first().waitFor();
  ok(
    (await box.inputValue()) === "",
    `${tag} quick add clears once the issue is created`,
  );
  ok(
    !(await alert1
      .first()
      .isVisible()
      .catch(() => false)),
    `${tag} the error clears on success`,
  );

  const lt = long(`board ${tag}`);
  await box.fill(lt);
  await page
    .getByText(/full text goes in the description/i)
    .first()
    .waitFor({ timeout: 3000 })
    .catch(() => {});
  ok(
    await page
      .getByText(/full text goes in the description/i)
      .first()
      .isVisible(),
    `${tag} quick add warns before sending text too long for a title`,
  );
  await shot("quickadd-long");
  await box.press("Enter");
  const card = page.locator("[data-nav-key]", {
    hasText: `Fix the flaky sync for board ${tag}`,
  });
  await card
    .first()
    .waitFor({ timeout: 5000 })
    .catch(() => {});
  ok(
    await card.first().isVisible(),
    `${tag} long quick-add text still creates an issue`,
  );
  ok((await box.inputValue()) === "", `${tag} and the box empties`);
  const longKey = await card
    .first()
    .getAttribute("data-nav-key")
    .catch(() => null);
  if (longKey) {
    const d = await api(`/issues/${longKey}`);
    ok(
      d.title === `Fix the flaky sync for board ${tag}.`,
      `${tag} the first sentence is the title: ${d.title}`,
    );
    ok(
      d.description.trim() === lt.trim(),
      `${tag} the full text is the description`,
    );
  }

  const t3 = `Half typed on the board ${tag}`;
  await box.fill(t3);
  await page.reload();
  const box3 = page.getByLabel("New issue title");
  await box3.waitFor({ timeout: 5000 }).catch(() => {});
  ok(
    (await box3.inputValue().catch(() => "")) === t3,
    `${tag} quick-add text is still there after a refresh`,
  );
  await shot("quickadd-restored");
  await box3.press("Escape").catch(() => {});
  await page.reload();
  await addBtn.waitFor();
  ok(
    !(await page.getByLabel("New issue title").isVisible()),
    `${tag} Esc discards the quick-add draft`,
  );

  await page.goto(`${base}/p/LOST/backlog`);
  const bl = page.getByRole("textbox", { name: "Add to backlog" });
  const t4 = `Half typed in the backlog ${tag}`;
  await bl.fill(t4);
  await page.reload();
  await bl.waitFor();
  ok(
    (await bl.inputValue()) === t4,
    `${tag} backlog add keeps its text over a refresh`,
  );
  await bl.fill(t4);
  await bl.press("Enter");
  await page.locator("[data-nav-key]", { hasText: t4 }).first().waitFor();
  await page.reload();
  await bl.waitFor();
  ok((await bl.inputValue()) === "", `${tag} and forgets it once created`);

  const base5 = `Edit my title ${tag}`;
  const created = await (
    await page.request.post(base + "/api/v1/issues", {
      headers: H,
      data: { project: "LOST", title: base5 },
    })
  ).json();
  await page.goto(`${base}/issue/${created.key}`);
  const title = page.getByRole("textbox", { name: "Title", exact: true });
  await title.waitFor();
  await title.click();
  await title.press("End");
  await title.type(" and more");
  forced = 0;
  await failNext("PATCH", `**/api/v1/issues/${created.key}`);
  await title.press("Enter");
  const alert5 = page
    .getByRole("alert")
    .filter({ hasText: "the database is busy" });
  await alert5
    .first()
    .waitFor({ timeout: 5000 })
    .catch(() => {});
  await page.waitForTimeout(800);
  ok(
    (await title.inputValue()) === base5 + " and more",
    `${tag} a failed title save keeps the edit (${await title.inputValue()})`,
  );
  ok(await alert5.first().isVisible(), `${tag} and shows the server's message`);
  await shot("title-error");
  await page.unroute(`**/api/v1/issues/${created.key}`);
  await title.focus();
  await title.press("Enter");
  await page.waitForTimeout(500);
  ok(
    (await api(`/issues/${created.key}`)).title === base5 + " and more",
    `${tag} Enter saves the title once the server is back`,
  );

  await title.click();
  await title.press("End");
  const extra = " x".repeat(260);
  await title.type(extra, { delay: 0 });
  await title.press("Enter");
  await page.waitForTimeout(800);
  ok(
    (await title.inputValue()) === base5 + " and more" + extra,
    `${tag} an over-long title stays in the editor after Enter`,
  );
  ok(
    await page
      .getByRole("alert")
      .filter({ hasText: /at most 500/ })
      .first()
      .isVisible(),
    `${tag} and the limit is shown`,
  );
  await shot("title-too-long");
  await title.press("Escape");
  ok(
    (await title.inputValue()) === base5 + " and more",
    `${tag} Esc cancels the edit`,
  );

  await title.click();
  await title.press("End");
  await title.type(" pending");
  await page.reload();
  await title.waitFor();
  ok(
    (await title.inputValue()) === base5 + " and more pending",
    `${tag} an unsaved title edit is restored after a refresh`,
  );
  await shot("title-restored");
  await title.focus();
  await title.press("Enter");
  await page.waitForTimeout(500);
  ok(
    (await api(`/issues/${created.key}`)).title === base5 + " and more pending",
    `${tag} and Enter saves it`,
  );

  await page.getByRole("button", { name: "Add a description…" }).click();
  const desc = page.getByRole("textbox", { name: "Description" });
  await desc.fill(`Notes I haven't saved ${tag}`);
  await page.reload();
  await desc.waitFor({ timeout: 5000 }).catch(() => {});
  ok(
    (await desc.evaluate((el) => (el.textContent ?? "").trim()).catch(() => "")) === `Notes I haven't saved ${tag}`,
    `${tag} an open description edit comes back after a refresh`,
  );
  ok(
    await page.getByText("Unsaved edit restored").isVisible(),
    `${tag} and says it was restored`,
  );
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page.waitForTimeout(500);
  ok(
    (await api(`/issues/${created.key}`)).description ===
      `Notes I haven't saved ${tag}`,
    `${tag} and saves`,
  );
  await page.reload();
  await title.waitFor();
  ok(
    !(await desc.isVisible()),
    `${tag} a saved description leaves no draft behind`,
  );

  await page.goto(`${base}/p/LOST/list`);
  await page.getByRole("button", { name: "Filter (F)" }).waitFor();
  await page.locator("body").press("c");
  let dialog = page.getByRole("dialog");
  const t7 = `From the dialog ${tag}`;
  await dialog.getByLabel("Issue title").fill(t7);
  await fillMarkdown(mdBox(dialog, { placeholder: /Add a description/ }), "Some notes");
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });
  await page.reload();
  await page.getByRole("button", { name: "Filter (F)" }).waitFor();
  await page.locator("body").press("c");
  dialog = page.getByRole("dialog");
  ok(
    (await dialog.getByLabel("Issue title").inputValue()) === t7,
    `${tag} the new-issue dialog brings back its title`,
  );
  ok(
    (await markdownOf(mdBox(dialog, { placeholder: /Add a description/ }))) ===
      "Some notes",
    `${tag} and its description`,
  );
  const lt7 = long(`dialog ${tag}`);
  await dialog.getByLabel("Issue title").fill(lt7);
  ok(
    await dialog
      .getByText(/full text goes in the description/i)
      .first()
      .isVisible(),
    `${tag} the dialog warns about a long title`,
  );
  await shot("dialog-long");
  await dialog.getByRole("button", { name: "Create issue" }).click();
  await dialog.waitFor({ state: "detached" });
  const made = page.locator("[data-nav-key]", {
    hasText: `Fix the flaky sync for dialog ${tag}`,
  });
  await made.first().waitFor();
  const dk = await made.first().getAttribute("data-nav-key");
  const d7 = await api(`/issues/${dk}`);
  ok(
    d7.description.startsWith(lt7.trim()) &&
      d7.description.endsWith("Some notes"),
    `${tag} the dialog puts the full text above the description`,
  );
  await page.locator("body").press("c");
  dialog = page.getByRole("dialog");
  ok(
    (await dialog.getByLabel("Issue title").inputValue()) === "",
    `${tag} a created issue leaves no draft behind`,
  );
  await page.keyboard.press("Escape");

  const nctx = await browser.newContext({ viewport: { width, height: 900 } });
  await nctx.addInitScript(() => {
    Storage.prototype.setItem = function (k) {
      if (String(k).startsWith("wp:draft:"))
        throw new Error("QuotaExceededError");
    };
  });
  const np = await login(nctx);
  await np.goto(`${base}/p/LOST/backlog`);
  const nb = np.getByRole("textbox", { name: "Add to backlog" });
  await nb.fill(`Unsaveable ${tag}`);
  const leave = np.getByRole("dialog").filter({ hasText: /unsaved/i });
  await np
    .getByRole("link", { name: /Board/ })
    .first()
    .click()
    .catch(() => {});
  await leave
    .first()
    .waitFor({ timeout: 3000 })
    .catch(() => {});
  ok(
    await leave.first().isVisible(),
    `${tag} leaving with text that can't be kept asks first`,
  );
  ok(/\/backlog/.test(np.url()), `${tag} and stays put until answered`);
  await np.screenshot({ path: `${out}/leave-guard-${width}.png` });
  await leave
    .getByRole("button", { name: "Stay" })
    .click()
    .catch(() => {});
  ok(
    (await nb.inputValue()) === `Unsaveable ${tag}`,
    `${tag} Stay keeps the text`,
  );
  await np
    .getByRole("link", { name: /Board/ })
    .first()
    .click()
    .catch(() => {});
  await leave
    .getByRole("button", { name: "Leave" })
    .click()
    .catch(() => {});
  await np.waitForURL(/\/board/).catch(() => {});
  ok(/\/board/.test(np.url()), `${tag} Leave goes`);
  await np.goto(`${base}/p/LOST/backlog`);
  await nb.fill(`Unsaveable ${tag}`);
  const unload = new Promise((res) => {
    np.once("dialog", (d) => (res(d.type()), d.accept()));
    setTimeout(() => res("none"), 3000);
  });
  await np.close({ runBeforeUnload: true });
  ok((await unload) === "beforeunload", `${tag} closing the tab asks first`);
  await nctx.close();
  await ctx.close();
}

for (const w of looks([1440, 390])) await run(w);
await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\nall good");
