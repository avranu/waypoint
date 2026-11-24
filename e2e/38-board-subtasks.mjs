// Checks: on the kanban board, a subtask that shares its parent's column folds into the
// parent's card as a collapsed "N subtasks" accordion (WP-69); one in another column stays
// a card of its own; the fold is remembered; dragging works for parents and nested subtasks;
// the toolbar toggle shows everything flat again.
// Needs user robin (run 01-signup-flow first).
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
for (const vp of looks([
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
])) {
  const tag = `${vp.width}px`;
  const ctx = await browser.newContext({ viewport: vp });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fail(`pageerror (${tag}): ` + e.message));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      fail(`console (${tag}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const send = async (method, p, d) =>
    (
      await page.request.fetch(base + "/api/v1" + p, {
        method,
        headers: H,
        data: d,
      })
    ).json();

  const key = "SB" + vp.width;
  if (!(await send("GET", "/projects")).some((p) => p.key === key))
    await send("POST", "/projects", { name: "Subtasks " + vp.width, key });
  const cols = (await send("GET", `/projects/${key}/board`)).columns.map(
    (c) => c.status,
  );
  const [, c1] = cols;
  const mk = (title, extra = {}) =>
    send("POST", "/issues", { project: key, title, ...extra });
  const P = await mk("Parent task", { type: "task" });
  const T = await mk("Standalone task");
  const S1 = await mk("Sub one", { type: "subtask", parent: P.key });
  const S2 = await mk("Sub two", { type: "subtask", parent: P.key });
  const S3 = await mk("Sub three", {
    type: "subtask",
    parent: P.key,
    status: c1.name,
  });
  const col = (i) => page.locator("section[data-col]").nth(i);
  const card = (k) => page.locator(`[data-nav-key="${k}"]`);
  const keysIn = (c) =>
    c
      .locator("[data-nav-key]")
      .evaluateAll((els) => els.map((e) => e.dataset.navKey));
  const strip = (k) => page.locator(`[aria-controls="subtasks-${k}"]`);
  const stripText = async (k) =>
    (await strip(k).innerText()).replace(/\s+/g, " ");
  const drag = async (from, to) => {
    const a = await from.boundingBox();
    const b = await to.boundingBox();
    await page.mouse.move(a.x + 30, a.y + 15);
    await page.mouse.down();
    await page.mouse.move(a.x + 40, a.y + 30, { steps: 4 });
    await page.mouse.move(b.x + 60, b.y + 15, { steps: 14 });
    await page.mouse.move(b.x + 62, b.y + 17, { steps: 2 });
    await page.mouse.up();
    await page.waitForTimeout(800);
  };

  await page.goto(`${base}/p/${key}/board`);
  await page.waitForSelector("section[data-col]");
  await card(P.key).waitFor();

  const first = await keysIn(col(0));
  ok(
    first.length === 2 && first.includes(P.key) && first.includes(T.key),
    `${tag}: column 1 shows only the parent and the standalone task: ${first}`,
  );
  ok(
    (await keysIn(col(1))).join() === S3.key,
    `${tag}: a subtask in another column is a card of its own`,
  );
  ok(
    (await stripText(P.key)).includes("2 of 3 subtasks") &&
      (await stripText(P.key)).includes("0/3 done"),
    `${tag}: strip: ${await stripText(P.key)}`,
  );
  ok(
    (await strip(P.key).getAttribute("aria-expanded")) === "false",
    `${tag}: strip starts collapsed`,
  );
  ok(
    (await col(0).locator("span.tabular-nums").first().innerText()) === "4",
    `${tag}: the column count still counts folded subtasks`,
  );
  await page.screenshot({ path: `${out}/${tag}-folded.png` });

  await strip(P.key).click();
  await card(S1.key).waitFor();
  ok(
    (await card(S2.key).isVisible()) &&
      (await strip(P.key).getAttribute("aria-expanded")) === "true",
    `${tag}: opening shows the subtasks inline`,
  );
  await page.screenshot({ path: `${out}/${tag}-open.png` });
  await page.reload();
  await card(S1.key).waitFor();
  ok(true, `${tag}: open state survives a reload`);
  await strip(P.key).click();
  ok((await card(S1.key).count()) === 0, `${tag}: collapses again`);
  await page.reload();
  await card(P.key).waitFor();
  ok(
    (await card(S1.key).count()) === 0,
    `${tag}: collapsed state survives a reload`,
  );

  if (vp.width > 600) {
    await strip(P.key).click();
    await card(S1.key).waitFor();
    await drag(card(S1.key), card(S3.key));
    const s1 = await send("GET", "/issues/" + S1.key);
    ok(
      s1.status.id === c1.id,
      `${tag}: dragging a nested subtask changes its status (${s1.status.name})`,
    );
    ok(
      (await keysIn(col(1))).includes(S1.key) &&
        !(await keysIn(col(0))).includes(S1.key),
      `${tag}: it now shows as a card in the other column`,
    );
    ok(
      (await stripText(P.key)).includes("1 of 3 subtasks"),
      `${tag}: the parent's strip counts what is left: ${await stripText(P.key)}`,
    );
    await drag(card(P.key), card(S3.key));
    const p = await send("GET", "/issues/" + P.key);
    ok(
      p.status.id === c1.id,
      `${tag}: dragging a parent changes its status (${p.status.name})`,
    );
    ok(
      (await keysIn(col(0))).includes(S2.key) &&
        (await stripText(P.key)).includes("2 of 3 subtasks"),
      `${tag}: the one left behind is its own card; strip: ${await stripText(P.key)}`,
    );
    await page.screenshot({ path: `${out}/${tag}-after-drag.png` });
  }

  const toggle = page.getByRole("button", { name: "Nest subtasks" });
  ok(
    (await toggle.getAttribute("aria-pressed")) === "true",
    `${tag}: the toggle starts on`,
  );
  await toggle.click();
  ok(
    (await toggle.getAttribute("aria-pressed")) === "false",
    `${tag}: the toggle turns off`,
  );
  const flat = [...(await keysIn(col(0))), ...(await keysIn(col(1)))];
  ok(
    [P, T, S1, S2, S3].every((i) => flat.includes(i.key)) &&
      (await strip(P.key).count()) === 0,
    `${tag}: flat shows every issue as a card, no strips`,
  );
  await page.screenshot({ path: `${out}/${tag}-flat.png` });
  await ctx.close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
