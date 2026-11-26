// Dependency lines on the timeline: one arrow per "blocks" link between the
// right bars, red when the blocked issue starts before its blocker ends,
// following a bar while it's dragged, highlighted on hover, and a toggle
// that's remembered. Run after 01-signup-flow (user robin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const near = (a, b, m, tol = 3) =>
  ok(Math.abs(a - b) <= tol, `${m} (${a.toFixed(1)} vs ${b.toFixed(1)})`);
const browser = await chromium.launch();
const H = { "X-Waypoint-Client": "web" };
const iso = (n) => {
  const d = new Date();
  d.setDate(d.getDate() + n);
  return d.toLocaleDateString("en-CA");
};

let made = false;
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
  const api = async (m, p, d) => {
    const r = await page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers: H,
      data: d,
    });
    if (!r.ok()) fail(`${tag} ${m} ${p}: ${r.status()} ${await r.text()}`);
    return r.status() === 204 ? null : r.json();
  };
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const dates = {
    "DEPS-1": { start_date: iso(0), due_date: iso(4) },
    "DEPS-2": { start_date: iso(6), due_date: iso(9) },
    "DEPS-3": { start_date: iso(2), due_date: iso(5) },
    "DEPS-4": { start_date: null, due_date: iso(12) },
  };
  if (!made) {
    made = true;
    await api("POST", "/projects", { key: "DEPS", name: "Deps" });
    for (const t of ["Blocker", "On time", "Too early", "Ship"])
      await api("POST", "/issues", { project: "DEPS", title: t, type: "task" });
    await api("POST", "/issues/DEPS-1/relations", {
      kind: "blocks",
      issue: "DEPS-2",
    });
    await api("POST", "/issues/DEPS-1/relations", {
      kind: "blocks",
      issue: "DEPS-3",
    });
    await api("POST", "/issues/DEPS-2/relations", {
      kind: "blocks",
      issue: "DEPS-4",
    });
  }
  for (const [k, d] of Object.entries(dates))
    await api("PATCH", "/issues/" + k, d);

  await page.goto(base + "/p/DEPS/timeline");
  const zoom = width < 640 ? "month" : "week";
  const px = zoom === "week" ? 44 : 18;
  await page.getByRole("button", { name: zoom, exact: true }).click();
  const svg = page.getByTestId("dependency-lines");
  await svg.waitFor();
  const lines = svg.locator("path[data-dep]");
  ok((await lines.count()) === 3, `${tag} three lines`);
  ok(
    (await svg.locator('[data-dep="DEPS-1>DEPS-3"][data-late]').count()) ===
      1 && (await svg.locator("path[data-late]").count()) === 1,
    `${tag} only the too-early link is late`,
  );

  const bar = (k) =>
    page.locator(`[title^="${k}:"], [title^="${k} due"]`).first();
  const ends = (key) =>
    svg.locator(`[data-dep="${key}"]`).evaluate((p) => {
      const m = p.ownerSVGElement.getScreenCTM();
      const a = p.getPointAtLength(0).matrixTransform(m);
      const b = p.getPointAtLength(p.getTotalLength()).matrixTransform(m);
      return { x1: a.x, y1: a.y, x2: b.x, y2: b.y };
    });
  const check = async (from, to, what) => {
    const e = await ends(`${from}>${to}`);
    const a = await bar(from).boundingBox();
    const b = await bar(to).boundingBox();
    near(e.x1, a.x + a.width, `${tag} ${what} starts at ${from}'s end`);
    near(e.y1, a.y + a.height / 2, `${tag} ${what} starts on ${from}'s row`);
    near(
      e.x2,
      b.x,
      `${tag} ${what} ends at ${to}'s start`,
      to === "DEPS-4" ? 6 : 3,
    );
    near(e.y2, b.y + b.height / 2, `${tag} ${what} ends on ${to}'s row`);
  };
  const reveal = () =>
    page.evaluate(() => {
      const a = document.querySelector('[title^="DEPS-1:"]');
      const box = a.closest('[style*="--label"]');
      const sc = box.parentElement;
      const label = parseFloat(box.style.getPropertyValue("--label"));
      sc.scrollLeft +=
        a.getBoundingClientRect().left -
        sc.getBoundingClientRect().left -
        label -
        8;
    });
  await reveal();
  await check("DEPS-1", "DEPS-2", "A→B");
  await check("DEPS-2", "DEPS-4", "B→milestone");

  await bar("DEPS-2").hover();
  const hot = await svg
    .locator("path[data-hot]")
    .evaluateAll((ps) => ps.map((p) => p.dataset.dep).sort());
  ok(
    JSON.stringify(hot) === JSON.stringify(["DEPS-1>DEPS-2", "DEPS-2>DEPS-4"]),
    `${tag} hovering B highlights its two lines (${hot})`,
  );
  await page.screenshot({ path: `${out}/hover-${width}-${scheme}.png` });
  await page.mouse.move(5, 5);
  ok(
    (await svg.locator("path[data-hot]").count()) === 0,
    `${tag} highlight clears`,
  );

  await page.goto(base + "/p/DEPS/timeline?issue=DEPS-3");
  await svg.waitFor();
  await page.mouse.move(5, 5);
  const open = await svg
    .locator("path[data-hot]")
    .evaluateAll((ps) => ps.map((p) => p.dataset.dep));
  ok(
    JSON.stringify(open) === JSON.stringify(["DEPS-1>DEPS-3"]),
    `${tag} the open issue's line is highlighted (${open})`,
  );
  await page.goto(base + "/p/DEPS/timeline");
  await svg.waitFor();

  await reveal();
  const c = await bar("DEPS-3").boundingBox();
  const before = await ends("DEPS-1>DEPS-3");
  await page.mouse.move(c.x + c.width / 2, c.y + c.height / 2);
  await page.mouse.down();
  await page.mouse.move(c.x + c.width / 2 + px * 4, c.y + c.height / 2, {
    steps: 8,
  });
  const during = await ends("DEPS-1>DEPS-3");
  near(during.x2 - before.x2, px * 4, `${tag} line follows the dragged bar`);
  ok(
    (await svg.locator("path[data-late]").count()) === 0,
    `${tag} warning clears mid-drag once C starts after A ends`,
  );
  await page.screenshot({ path: `${out}/drag-${width}-${scheme}.png` });
  await page.mouse.up();
  await page.waitForTimeout(600);
  const saved = await api("GET", "/issues/DEPS-3");
  ok(saved.start_date === iso(6), `${tag} drop saved (${saved.start_date})`);
  near(
    (await ends("DEPS-1>DEPS-3")).x2,
    during.x2,
    `${tag} line stays after the drop`,
  );

  await page
    .getByRole("button", { name: "Dependencies", pressed: true })
    .click();
  ok((await svg.count()) === 0, `${tag} lines hidden`);
  await page.reload();
  await page
    .getByRole("button", { name: "Dependencies", pressed: false })
    .waitFor();
  ok(
    (await page.getByTestId("dependency-lines").count()) === 0,
    `${tag} hidden after reload`,
  );
  await page
    .getByRole("button", { name: "Dependencies", pressed: false })
    .click();
  ok(
    (await page.getByTestId("dependency-lines").count()) === 1,
    `${tag} shown again`,
  );

  const sideways = await page.evaluate(
    () =>
      document.documentElement.scrollWidth >
      document.documentElement.clientWidth,
  );
  ok(!sideways, `${tag} no sideways page scroll`);
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
]))
  await run(w, s);
await browser.close();
if (errors.length) {
  console.log(errors.length, "failures");
  process.exit(1);
}
console.log("all good");
