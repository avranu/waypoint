// Checks: token permissions (WP-111). A token made with the Assistant preset
// creates issues but is refused starting a session or deleting; the refusals
// show on the token's row, Allow grants one, and the token can then use it. At desktop and
// phone widths. Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const visible = (loc) =>
  loc.waitFor({ timeout: 5000 }).then(
    () => true,
    () => false,
  );

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
      !m.text().includes("403") &&
      fail(`console (${tag}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = "TP" + vp.width;
  const projects = await (
    await page.request.get(base + "/api/v1/projects", { headers: H })
  ).json();
  if (!projects.some((p) => p.key === key))
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Tokens " + vp.width, key },
    });

  await page.goto(base + "/settings?tab=tokens");
  await page.getByRole("button", { name: "New token" }).click();
  const dialog = page.getByRole("dialog");
  const name = "hermes " + vp.width;
  await dialog.getByLabel("Name").fill(name);
  await dialog.getByRole("radio", { name: /Custom/ }).click();
  const create = dialog.getByRole("checkbox", { name: /Create issues/ });
  await create.waitFor();
  ok(
    await create.isChecked(),
    `${tag}: Custom starts from the Assistant preset`,
  );
  ok(
    !(await dialog
      .getByRole("checkbox", { name: /Start sessions/ })
      .isChecked()),
    `${tag}: Assistant doesn't start sessions`,
  );
  const noHScroll = await page.evaluate(
    () => document.documentElement.scrollWidth <= window.innerWidth,
  );
  ok(noHScroll, `${tag}: no page-level horizontal scroll with the editor open`);
  await page.screenshot({ path: `${out}/${tag}-create.png` });
  await dialog.getByRole("button", { name: "Create token" }).click();
  const shown = dialog.locator("input[readonly]");
  await shown.waitFor();
  const secret = await shown.inputValue();
  ok(/^wpt_/.test(secret ?? ""), `${tag}: the token is shown once`);
  await dialog.getByRole("button", { name: "Done" }).click();

  const as = (method, path, data) =>
    page.request.fetch(base + "/api/v1" + path, {
      method,
      headers: { Authorization: "Bearer " + secret },
      data,
    });
  const created = await as("POST", "/issues", {
    project: key,
    title: "From Hermes",
  });
  ok(created.status() === 200, `${tag}: the token creates an issue`);
  const issue = await created.json();
  const started = await as("POST", `/agents/start/${issue.key}`, {});
  ok(
    started.status() === 403,
    `${tag}: starting a session is refused: ${started.status()}`,
  );
  const refused = await as("DELETE", `/issues/${issue.key}`);
  ok(
    refused.status() === 403,
    `${tag}: deleting is refused: ${refused.status()}`,
  );

  await page.reload();
  const row = page.locator("li", { hasText: name });
  ok(
    await visible(row.getByText("2 refused")),
    `${tag}: the row shows the refusals`,
  );
  ok(
    await visible(row.getByText(/\d+ permissions/)),
    `${tag}: the row summarises its access`,
  );
  await row.getByText("2 refused").click();
  const edit = page.getByRole("dialog");
  await edit.getByText("Refused recently").waitFor();
  await page.screenshot({ path: `${out}/${tag}-refused.png` });
  await edit
    .locator("li", { hasText: "Delete issues" })
    .getByRole("button", { name: "Allow" })
    .click();
  ok(
    await edit.getByRole("checkbox", { name: /Delete issues/ }).isChecked(),
    `${tag}: Allow ticks the permission`,
  );
  ok(
    await visible(edit.getByText(/Dangerous:/)),
    `${tag}: a dangerous grant is flagged`,
  );
  await edit.getByRole("button", { name: "Save" }).click();
  await edit.waitFor({ state: "detached" });
  const tokens = await (
    await page.request.get(base + "/api/v1/me/tokens", { headers: H })
  ).json();
  const t = tokens.find((x) => x.name === name);
  ok(t?.permissions?.includes("issues.delete"), `${tag}: the grant is saved`);
  ok(
    !t?.permissions?.includes("sessions.start"),
    `${tag}: only what was allowed is granted`,
  );
  const again = await as("DELETE", `/issues/${issue.key}`);
  ok(again.ok(), `${tag}: deleting works once allowed: ${again.status()}`);

  await row.getByRole("button", { name: "Permissions" }).click();
  await page
    .getByRole("dialog")
    .getByRole("radio", { name: /Read only/ })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Save" }).click();
  await page.getByRole("dialog").waitFor({ state: "detached" });
  ok(
    await visible(row.getByText("read-only")),
    `${tag}: switched to read-only`,
  );
  const ro = await as("POST", "/issues", { project: key, title: "Refused" });
  ok(
    ro.status() === 403,
    `${tag}: a read-only token can't create: ${ro.status()}`,
  );
  await ctx.close();
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
console.log("all token permission checks passed");
