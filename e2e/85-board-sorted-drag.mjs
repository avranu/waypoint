// Checks: on a board whose filter sorts by something other than manual rank (?q=order:priority),
// a drop says nothing about rank (WP-1800): dragging a card within a column leaves every manual
// rank as it was and the card returns; dragging it to another column changes its status and
// still leaves manual order alone.
// The backlog page has the same rules (WP-1800).
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
  const wide = vp.width > 600; // touch dragging is not exercised by the other board suites either
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

  const key = "BS" + vp.width;
  if (!(await send("GET", "/projects")).some((p) => p.key === key))
    await send("POST", "/projects", { name: "Sorted drag " + vp.width, key });
  const cols = (await send("GET", `/projects/${key}/board`)).columns.map(
    (c) => c.status,
  );
  const [, c1] = cols;
  const mk = (title, priority, extra = {}) =>
    send("POST", "/issues", { project: key, title, priority, ...extra });
  const A = await mk("Rank first", "low");
  const B = await mk("Rank second", "medium");
  const C = await mk("Rank third", "high");
  const D = await mk("Other column", "none", { status: c1.name });
  const manual = async () =>
    (await send("GET", `/projects/${key}/board?q=order:rank`)).columns
      .flatMap((c) => c.issues)
      .map((i) => `${i.key}@${i.rank}`)
      .sort()
      .join();
  const col = (i) => page.locator("section[data-col]").nth(i);
  const card = (k) => page.locator(`[data-nav-key="${k}"]`);
  const keysIn = (c) =>
    c
      .locator("[data-nav-key]")
      .evaluateAll((els) => els.map((e) => e.dataset.navKey));
  // drag drops from onto to. With patches (an issue key) the drop must save, and
  // drag returns once that issue's PATCH has answered; without, the drop sends
  // nothing, and drag gives the page a moment to (not) act on it.
  const drag = async (from, to, patches) => {
    const a = await from.boundingBox();
    const b = await to.boundingBox();
    const saved =
      patches &&
      page.waitForResponse(
        (r) =>
          r.url().endsWith(`/issues/${patches}`) &&
          r.request().method() === "PATCH",
      );
    await page.mouse.move(a.x + 30, a.y + 15);
    await page.mouse.down();
    await page.mouse.move(a.x + 40, a.y + 30, { steps: 4 });
    await page.mouse.move(b.x + 60, b.y + 15, { steps: 14 });
    await page.mouse.move(b.x + 62, b.y + 17, { steps: 2 });
    await page.mouse.up();
    if (saved) return void (await saved);
    // wplint:ignore e2e-wait a same-column drop under a sort sends no request: there is nothing to wait on
    await page.waitForTimeout(800);
  };

  await page.goto(
    `${base}/p/${key}/board?q=${encodeURIComponent("order:priority")}`,
  );
  await page.waitForSelector("section[data-col]");
  await card(C.key).waitFor();
  const shown = [A.key, B.key, C.key].join();
  ok(
    (await keysIn(col(0))).join() === shown,
    `${tag}: the board shows the priority order: ${await keysIn(col(0))}`,
  );
  const before = await manual();

  if (wide) {
    await drag(card(A.key), card(C.key));
    ok(
      (await manual()) === before,
      `${tag}: a drop within a column leaves manual ranks alone: ${before} -> ${await manual()}`,
    );
    ok(
      (await keysIn(col(0))).join() === shown,
      `${tag}: the card returns to its sorted place: ${await keysIn(col(0))}`,
    );
    await page.reload();
    await card(C.key).waitFor();
    ok(
      (await keysIn(col(0))).join() === shown,
      `${tag}: still the priority order after a reload`,
    );

    await drag(card(B.key), card(D.key), B.key);
    const b = await send("GET", "/issues/" + B.key);
    ok(
      b.status.id === c1.id,
      `${tag}: a drop in another column changes the status (${b.status.name})`,
    );
    ok(
      (await manual()) === before,
      `${tag}: ...and leaves manual ranks alone: ${await manual()}`,
    );
    ok(
      (await keysIn(col(1))).includes(B.key),
      `${tag}: it shows in the other column: ${await keysIn(col(1))}`,
    );
  }
  await page.screenshot({ path: `${out}/${tag}-sorted-drag.png` });

  // The backlog page: a sprint project with three backlog rows and one in a planned sprint.
  const bkey = "BL" + vp.width;
  if (!(await send("GET", "/projects")).some((p) => p.key === bkey))
    await send("POST", "/projects", {
      name: "Sorted backlog " + vp.width,
      key: bkey,
      settings: { mode: "sprints" },
    });
  const sprint = await send("POST", `/projects/${bkey}/sprints`, {
    name: "Sprint " + vp.width,
  });
  const mkb = (title, priority) =>
    send("POST", "/issues", { project: bkey, title, priority });
  const [A2, B2, C2, E2] = [
    await mkb("Rank first", "low"),
    await mkb("Rank second", "medium"),
    await mkb("Rank third", "high"),
    await mkb("In the sprint", "none"),
  ];
  await send("PATCH", "/issues/" + E2.key, { sprint: sprint.name });
  const ranks = async () => {
    const d = await send("GET", `/projects/${bkey}/backlog?q=order:rank`);
    return [...d.backlog, ...d.sprints.flatMap((x) => x.issues)]
      .map((i) => `${i.key}@${i.rank}`)
      .sort()
      .join();
  };
  const rows = () =>
    page
      .locator("[data-nav-key]")
      .evaluateAll((els) => els.map((e) => e.dataset.navKey))
      .then((ks) => ks.filter((k) => [A2, B2, C2].some((i) => i.key === k)));
  const sortedRows = [A2.key, B2.key, C2.key].join();
  await page.goto(
    `${base}/p/${bkey}/backlog?q=${encodeURIComponent("order:priority")}`,
  );
  await card(C2.key).waitFor();
  ok(
    (await rows()).join() === sortedRows,
    `${tag}: the backlog shows the priority order: ${await rows()}`,
  );
  await page.screenshot({ path: `${out}/${tag}-sorted-backlog.png` });
  if (wide) {
    const ranked = await ranks();
    await drag(card(A2.key), card(C2.key));
    ok(
      (await ranks()) === ranked,
      `${tag}: a backlog drop within the section leaves ranks alone: ${await ranks()}`,
    );
    ok(
      (await rows()).join() === sortedRows,
      `${tag}: the row returns to its sorted place: ${await rows()}`,
    );
    await drag(card(B2.key), card(E2.key), B2.key);
    const moved = await send("GET", "/issues/" + B2.key);
    ok(
      moved.sprint_name === sprint.name,
      `${tag}: a drop into the sprint moves the issue there (${moved.sprint_name})`,
    );
    ok(
      (await ranks()) === ranked,
      `${tag}: ...and leaves ranks alone: ${await ranks()}`,
    );
    await page.goto(`${base}/p/${bkey}/backlog`);
    await card(C2.key).waitFor();
    const unsorted = await ranks();
    await drag(card(C2.key), card(A2.key), C2.key);
    const after = await ranks();
    const rank = (r, k) =>
      Number(
        r
          .split(",")
          .find((x) => x.startsWith(k + "@"))
          .split("@")[1],
      );
    ok(
      after !== unsorted && rank(after, C2.key) > rank(after, A2.key),
      `${tag}: without order: a drop still moves the rank: ${unsorted} -> ${after}`,
    );
  }
  await ctx.close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
