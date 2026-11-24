// Checks: the issue page's points control reads "Points" when unset, and the
// "Estimate with AI" action lives inside the points picker (even when points are
// set), not next to it (WP-86). The AI endpoints are mocked, so no model is needed.
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
  await page.route("**/api/v1/ai/tasks", (r) =>
    r.fulfill({
      json: [
        {
          task: "estimate",
          label: "Estimate",
          description: "",
          provider_id: 1,
          model: "m",
          fallbacks: [],
        },
      ],
    }),
  );
  await page.route("**/api/v1/issues/*/ai/estimate", (r) =>
    r.fulfill({
      json: {
        points: 5,
        confidence: "medium",
        reason: "Mocked.",
        model: "mock",
      },
    }),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = {
    get: async (p) =>
      (await page.request.get(base + "/api/v1" + p, { headers: H })).json(),
    post: async (p, d) =>
      (
        await page.request.post(base + "/api/v1" + p, { headers: H, data: d })
      ).json(),
  };

  const key = "PT" + vp.width;
  if (!(await api.get("/projects")).some((p) => p.key === key))
    await api.post("/projects", { name: "Points " + vp.width, key });
  const issue = await api.post("/issues", {
    project: key,
    title: "Points label",
  });

  await page.goto(`${base}/issue/${issue.key}`);
  // Picking a field from More opens its picker at once; this closes it before the face is checked.
  const more = page.locator('[aria-label="More fields"]:visible');
  if (await more.count()) {
    await more.first().click();
    await page.getByRole("menuitem", { name: "Points", exact: true }).click();
    await page.getByRole("button", { name: /Estimate with AI/ }).waitFor();
    await page.keyboard.press("Escape");
  }
  const points = page.locator('[title="Story points"]:visible');
  await points.waitFor();
  const estimate = page.getByRole("button", { name: /Estimate with AI/ });

  // The strip reads "Points"; the side panel's labelled row reads "None".
  const face = (await points.textContent()).trim();
  ok(
    face === "Points" || face === "None",
    `${tag}: unset reads short (${face})`,
  );
  ok(
    (await estimate.count()) === 0,
    `${tag}: no Estimate button beside the control`,
  );

  await points.click();
  await estimate.waitFor();
  await page.screenshot({ path: `${out}/${tag}-picker-offer.png` });
  await estimate.click();
  const use = page.getByRole("button", { name: "Use 5" });
  await use.waitFor();
  await page.screenshot({ path: `${out}/${tag}-picker-result.png` });
  await use.click();
  await page
    .locator('[title="Story points"]:visible')
    .getByText("5 pts")
    .waitFor();
  // The label updates before the save lands, so the stored value is polled.
  let saved = null;
  for (let i = 0; i < 20; i++) {
    saved = (await api.get("/issues/" + issue.key)).points;
    if (saved === 5) break;
    await page.waitForTimeout(250);
  }
  if (saved !== 5) saved = (await api.get("/issues/" + issue.key)).points;
  ok(saved === 5, `${tag}: estimate applied`);

  ok(
    (await estimate.count()) === 0,
    `${tag}: set points show no Estimate button`,
  );
  await points.click();
  await estimate.waitFor();
  ok(
    await estimate.isVisible(),
    `${tag}: the picker still offers Estimate with AI`,
  );
  await page.screenshot({ path: `${out}/${tag}-picker-set.png` });
  await ctx.close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
