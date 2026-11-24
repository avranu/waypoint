// The Discover repositories picker lists the repos a deleted project left
// ignored, marked, after the offered ones, and "Offer again" clears the
// ignore. A fake GitHub lists the repos.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import http from "node:http";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const fake = (names) => {
  let n = 0;
  const srv = http.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    const page = new URL(req.url, "http://x").searchParams.get("page");
    const body =
      req.url.startsWith("/api/v3/user/repos") && page === "1"
        ? names.map((full_name) => ({
            id: ++n,
            full_name,
            owner: { login: full_name.split("/")[0], type: "User" },
          }))
        : [];
    res.end(JSON.stringify(body));
  });
  return new Promise((r) => srv.listen(0, "127.0.0.1", () => r(srv)));
};

const browser = await chromium.launch();
for (const [width, scheme] of looks(
  [
    [1280, "light"],
    [390, "dark"],
  ],
  [0, 1],
)) {
  const tag = `${width}${scheme[0]}`;
  const srv = await fake([`kept/${tag}-a`, `kept/${tag}-b`, `gone/${tag}-old`]);
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const name = `Ignored ${tag}`;
  const f = await api("POST", "/forges", {
    kind: "github",
    name,
    base_url: `http://127.0.0.1:${srv.address().port}`,
    token: "tok",
  });
  const key = `I${width}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Gone ${tag}`, key });
  await api("POST", `/projects/${key}/repos`, {
    forge_id: f.id,
    repo: `gone/${tag}-old`,
    sync_issues: false,
  });
  await api("DELETE", `/projects/${key}`);

  await page.goto(`${base}/admin?tab=forges`);
  const row = page.locator("li").filter({ hasText: name }).first();
  await row.getByRole("button", { name: `Actions for ${name}` }).click();
  await page.getByRole("menuitem", { name: "Discover repositories" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByText("Ignored", { exact: true }).waitFor();
  ok(
    await dialog.getByText(`gone/${tag}-old`).isVisible(),
    `${tag} the ignored repo is listed`,
  );
  ok(
    await dialog.getByText("ignored", { exact: true }).isVisible(),
    `${tag} it is marked ignored`,
  );
  const y = async (t) => (await dialog.getByText(t).boundingBox()).y;
  ok(
    (await y(`${tag}-b`)) < (await y(`gone/${tag}-old`)),
    `${tag} offered repos come before the ignored one`,
  );
  ok(
    (await dialog.getByRole("checkbox").count()) === 4,
    `${tag} the ignored repo has no checkbox`,
  );
  await page.screenshot({ path: `${out}/picker-${width}-${scheme}.png` });
  const sideways = await page.evaluate(
    () => document.documentElement.scrollWidth > innerWidth + 1,
  );
  ok(!sideways, `${tag} no sideways scroll`);

  await dialog.getByRole("button", { name: "Offer again" }).click();
  await dialog
    .getByText("Ignored", { exact: true })
    .waitFor({ state: "detached" });
  ok(
    (await dialog.getByRole("checkbox").count()) === 6,
    `${tag} the repo is offered like any other (checkbox, selected)`,
  );
  ok(
    await dialog.getByText("3 of 3 selected").isVisible(),
    `${tag} all three are selected`,
  );
  await page.screenshot({ path: `${out}/offered-${width}-${scheme}.png` });
  await ctx.close();
  srv.close();
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
