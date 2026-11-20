// Checks for Robin's manual UAT items. Run after 01-signup-flow (user robin, project WAYP).
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

for (const [scheme, vp] of looks([
  ["dark", { width: 1440, height: 900 }],
  ["light", { width: 1440, height: 900 }],
  ["dark", { width: 390, height: 844 }],
  ["light", { width: 390, height: 844 }],
])) {
  const tag = `${scheme}-${vp.width}`;
  const mobile = vp.width < 500;
  const ctx = await browser.newContext({ viewport: vp, colorScheme: scheme });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 200)}`),
  );
  const api = (method, p, data) =>
    page.request
      .fetch(base + "/api/v1" + p, { method, headers: H, data })
      .then((r) => r.json().catch(() => null));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("PATCH", "/projects/WAYP", { background: "" });
  await api("POST", "/timer/stop");

  // 2. Filter is a button until wanted; F opens it; it stays open while filtering.
  await page.goto(base + "/p/WAYP/board");
  await page.waitForTimeout(500);
  ok(
    (await page.getByLabel("Filter issues").count()) === 0,
    `${tag} filter input hidden by default`,
  );
  ok(
    await page.getByRole("button", { name: "Filter (F)" }).isVisible(),
    `${tag} filter button shown`,
  );
  await page.keyboard.press("f");
  await page.waitForTimeout(150);
  ok(
    await page
      .getByLabel("Filter issues")
      .evaluate((el) => el === document.activeElement),
    `${tag} F opens and focuses filter`,
  );
  await page.keyboard.type("type:bug");
  await page.waitForTimeout(600);
  ok(page.url().includes("q=type"), `${tag} filter applies`);
  await page.locator("body").click({ position: { x: 5, y: vp.height - 120 } });
  ok(
    await page.getByLabel("Filter issues").isVisible(),
    `${tag} stays open while a filter is set`,
  );
  await page.getByLabel("Clear filter").click();
  await page.locator("body").click({ position: { x: 5, y: vp.height - 120 } });
  await page.waitForTimeout(400);
  ok(
    (await page.getByLabel("Filter issues").count()) === 0,
    `${tag} collapses when emptied and left`,
  );
  await page.screenshot({ path: `${out}/${tag}-board.png` });

  // 4. Timeline month labels never overlap, at every zoom.
  const iss = await api("POST", "/issues", {
    project: "WAYP",
    title: "Span probe",
    start_date: "2026-09-28",
    due_date: "2026-12-20",
  });
  await page.goto(base + "/p/WAYP/timeline");
  await page.waitForTimeout(700);
  for (const z of ["Week", "Month", "Quarter"]) {
    await page
      .getByRole("button", { name: new RegExp("^" + z + "$", "i") })
      .click();
    await page.waitForTimeout(300);
    const boxes = await page
      .locator(".relative.h-12 > div.top-1")
      .evaluateAll((els) =>
        els.map((e) => ({
          t: e.textContent,
          l: e.getBoundingClientRect().left,
          r: e.getBoundingClientRect().right,
        })),
      );
    const overlap = boxes.some((b, i) => i > 0 && b.l < boxes[i - 1].r + 2);
    const dup = boxes.some((b, i) => i > 0 && b.t === boxes[i - 1].t);
    ok(
      boxes.length > 0 && !overlap && !dup,
      `${tag} timeline ${z}: ${boxes.length} month labels, no overlap/dupes`,
    );
  }
  await page.screenshot({ path: `${out}/${tag}-timeline.png` });
  await api("DELETE", `/issues/${iss.key}`);

  // 5. Background previews live, before saving.
  await page.goto(base + "/p/WAYP/settings");
  await page.waitForTimeout(500);
  await page.getByRole("button", { name: "Dusk" }).click();
  await page.waitForTimeout(200);
  const layer = await page
    .locator("[data-bg] > div[aria-hidden]")
    .first()
    .evaluate((el) => getComputedStyle(el).backgroundImage)
    .catch(() => "");
  ok(layer.includes("gradient"), `${tag} background previews before save`);
  ok(
    (await api("GET", "/projects/WAYP")).background === "",
    `${tag} preview not saved`,
  );
  await page.screenshot({ path: `${out}/${tag}-settings-preview.png` });
  await page.goto(base + "/p/WAYP/board");
  await page.waitForTimeout(400);
  ok(
    (await page.locator("[data-bg]").count()) === 0,
    `${tag} unsaved preview is dropped on leaving`,
  );

  // 6. Empty week: no chart, just the empty state.
  const from = new Date();
  await page.goto(base + "/time");
  await page.waitForTimeout(600);
  for (let i = 0; i < 3; i++)
    await page.getByRole("button", { name: "Previous week" }).click();
  await page.waitForTimeout(600);
  ok(
    (await page.getByText("No time this week").count()) === 1 &&
      (await page.getByText("Hours per day").count()) === 0,
    `${tag} empty week: empty state only, no chart`,
  );
  await page.getByRole("button", { name: "This week" }).click();
  await page.waitForTimeout(300);

  // 7. Running timer: name it, backdate it, set elapsed, all in place.
  await page.getByRole("button", { name: "Start" }).click();
  await page.waitForTimeout(500);
  await page.getByRole("button", { name: "Name this work" }).click();
  await page.keyboard.type("Inbox zero");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(500);
  let t = (await api("GET", "/timer")).timer;
  ok(t?.note === "Inbox zero", `${tag} timer note edited in place`);
  await page
    .getByRole("button", { name: "Change how long this has been running" })
    .click();
  await page.keyboard.press("Control+a");
  await page.keyboard.type("1h30m");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(600);
  t = (await api("GET", "/timer")).timer;
  const ran = (Date.now() - new Date(t.started_at).getTime()) / 60000;
  ok(ran > 88 && ran < 92, `${tag} elapsed set to ~90m (${ran.toFixed(1)})`);
  await page
    .getByRole("button", { name: "Change when this timer started" })
    .click();
  const earlier = new Date(Date.now() - 2 * 3600_000);
  const hhmm = `${String(earlier.getHours()).padStart(2, "0")}:${String(earlier.getMinutes()).padStart(2, "0")}`;
  await page.getByLabel("Change when this timer started").fill(hhmm);
  await page.getByLabel("Change when this timer started").press("Enter");
  await page.waitForTimeout(600);
  t = (await api("GET", "/timer")).timer;
  const ran2 = (Date.now() - new Date(t.started_at).getTime()) / 60000;
  ok(
    ran2 > 118 && ran2 < 122,
    `${tag} start time backdated (~120m: ${ran2.toFixed(1)})`,
  );
  await page.screenshot({ path: `${out}/${tag}-timer.png` });
  await api("POST", "/timer/stop");
  await page.waitForTimeout(600);
  // With data: day labels read "Mon 12" style, and tooltips are humane.
  await page.reload();
  await page.waitForTimeout(700);
  const labels = await page
    .locator(".grid-cols-7 button")
    .evaluateAll((els) =>
      els.map((e) => [
        e.lastElementChild.innerText.replace(/\s+/g, " ").trim(),
        e.title,
      ]),
    );
  ok(
    labels.length === 7 &&
      labels.every(([l]) => /^[A-Za-z]{3}\.? \d{1,2}$/.test(l)),
    `${tag} day labels like "Mon 12": ${labels.map((x) => x[0]).join(", ")}`,
  );
  ok(
    labels.every(([, t]) => / — /.test(t) && !/: 0s/.test(t)),
    `${tag} humane tooltips: ${labels[0]?.[1]} / ${labels.find(([, t]) => /\d/.test(t.split("—")[1] ?? ""))?.[1]}`,
  );
  await page.screenshot({ path: `${out}/${tag}-time.png` });

  // 8. Add time: toggle on one line, slider drives the duration.
  await page.getByRole("button", { name: "Add time" }).click();
  await page.waitForTimeout(300);
  const endBtn = page.getByRole("button", { name: "End time" });
  const h = (await endBtn.boundingBox()).height;
  ok(h <= 30, `${tag} End time toggle on one line (h=${h})`);
  const slider = page.getByLabel("Duration slider");
  await slider.fill("9000");
  ok(
    (await page.getByLabel("Duration", { exact: true }).inputValue()) ===
      "2h30m",
    `${tag} slider sets duration`,
  );
  await page.getByLabel("Duration", { exact: true }).fill("45m");
  ok((await slider.inputValue()) === "2700", `${tag} duration moves slider`);
  await page.screenshot({ path: `${out}/${tag}-addtime.png` });
  await page.keyboard.press("Escape");

  await ctx.close();
}
await browser.close();
console.log(
  errors.length ? `${errors.length} failures` : "all uat checks passed",
);
process.exit(errors.length ? 1 : 0);
