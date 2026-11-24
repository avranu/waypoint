// Cleaning up projects, in a real browser at desktop and phone widths:
// turning on automatic projects for a forge with many repos asks which
// ones get projects first (unchecked repos stay out for good); the
// sidebar lists projects by recent activity; a project's "…" menu
// deletes it; and "Delete projects…" deletes several at once (checking a
// parent checks its sub-projects).
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

// A fake GitHub that lists `repos` (per run) and has no issues.
let current = [];
const gh = http.createServer((req, res) => {
  const send = (code, body) => {
    res.writeHead(code, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  };
  const u = new URL(req.url, "http://x");
  if (u.pathname === "/api/v3/user/repos")
    return send(
      200,
      (u.searchParams.get("page") ?? "1") === "1"
        ? current.map((full_name) => ({
            full_name,
            owner: {
              login: full_name.split("/")[0],
              type: full_name.startsWith("me") ? "User" : "Organization",
            },
          }))
        : [],
    );
  if (req.method === "POST") return send(201, { id: 1 });
  send(200, []);
});
await new Promise((r) => gh.listen(0, "127.0.0.1", r));
const ghURL = `http://127.0.0.1:${gh.address().port}`;
const browser = await chromium.launch();

async function run(width) {
  const tag = `w${width}`;
  const desktop = width >= 768;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
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

  const org = `org${tag}`;
  current = [
    ...Array.from({ length: 7 }, (_, i) => `${org}/repo${i}`),
    `me${tag}/notes`,
  ];
  await page.goto(base + "/admin?tab=forges");
  await page.getByRole("button", { name: "Connect forge" }).click();
  const dlg = page.getByRole("dialog");
  await dlg.getByLabel("Name").fill(`Forge ${tag}`);
  await dlg.getByLabel("Base URL").fill(ghURL);
  await dlg.getByLabel("Access token").fill("tok");
  await dlg.getByRole("switch", { name: "Automatic projects" }).click();
  await dlg.getByRole("button", { name: "Connect" }).click();
  const picker = page.getByRole("dialog", {
    name: `New repositories on Forge ${tag}`,
  });
  await picker.waitFor();
  ok(
    await picker.getByText("These would make 9 projects").isVisible(),
    `${tag} picker counts 8 repos plus the organisation's project`,
  );
  await picker.getByRole("checkbox", { name: "repo5" }).uncheck();
  await picker.getByRole("checkbox", { name: "repo6" }).uncheck();
  await picker.getByRole("checkbox", { name: "notes" }).uncheck();
  ok(
    await picker.getByRole("button", { name: "Create 6 projects" }).isVisible(),
    `${tag} unchecking three repos leaves 6 projects to make`,
  );
  await page.screenshot({ path: `${out}/picker-${width}.png` });
  await picker.getByRole("button", { name: "Create 6 projects" }).click();
  await picker.waitFor({ state: "detached" });
  const forge = (await api("GET", "/forges")).find(
    (f) => f.name === `Forge ${tag}`,
  );
  ok(forge?.auto_projects, `${tag} automatic projects turned on after picking`);
  const names = async () => (await api("GET", "/projects")).map((p) => p.name);
  let have = await names();
  ok(
    have.includes(org) &&
      have.includes("repo0") &&
      !have.includes("repo5") &&
      !have.includes("notes"),
    `${tag} only the picked repos got projects`,
  );
  const again = await api("GET", `/forges/${forge.id}/discover`);
  ok(again.length === 0, `${tag} skipped repos aren't offered again`);

  const proj = await api("POST", "/projects", { name: `Fresh ${tag}` });
  await api("POST", "/issues", { project: proj.key, title: "Recent work" });
  const sidebarNames = () =>
    page
      .locator("aside nav a[href^='/p/']")
      .evaluateAll((as) => as.map((a) => a.textContent.trim()));
  if (desktop) {
    await page.goto(base + "/");
    await page.locator("aside").getByText(`Fresh ${tag}`).waitFor();
    const order = await sidebarNames();
    ok(
      order[0].includes(`Fresh ${tag}`),
      `${tag} the most recently active project leads the sidebar (${order.slice(0, 3)})`,
    );
  }

  if (desktop) {
    const row = page
      .locator("aside nav .group")
      .filter({ hasText: `Fresh ${tag}` });
    await row.hover();
    await row.getByRole("button", { name: `Actions for Fresh ${tag}` }).click();
    await page.getByRole("menuitem", { name: "Delete…" }).click();
  } else {
    await page.goto(base + "/");
    await page.getByRole("button", { name: "Projects" }).click();
    await page.getByRole("button", { name: "Delete projects…" }).click();
    const d = page.getByRole("dialog", { name: "Delete projects" });
    await d.getByRole("checkbox", { name: new RegExp(`Fresh ${tag}`) }).check();
    await d.getByRole("button", { name: "Delete…" }).click();
  }
  const one = page.getByRole("dialog", { name: `Delete Fresh ${tag}?` });
  await one.waitFor();
  await page.screenshot({ path: `${out}/delete-one-${width}.png` });
  await one.getByRole("button", { name: "Delete forever" }).click();
  await one.waitFor({ state: "detached" });
  ok(!(await names()).includes(`Fresh ${tag}`), `${tag} deleted one project`);

  await page.goto(base + "/");
  if (desktop) {
    await page.getByRole("button", { name: "Project actions" }).click();
    await page.getByRole("menuitem", { name: "Delete projects…" }).click();
  } else {
    await page.getByRole("button", { name: "Projects" }).click();
    await page.getByRole("button", { name: "Delete projects…" }).click();
  }
  const many = page.getByRole("dialog", { name: "Delete projects" });
  await many.waitFor();
  await many
    .getByRole("checkbox", { name: new RegExp(`\\b${org}\\b`) })
    .check();
  await many.getByRole("checkbox", { name: /\brepo4\b/ }).uncheck();
  await page.screenshot({ path: `${out}/delete-many-${width}.png` });
  await many.getByRole("button", { name: "Delete 5…" }).click();
  const sure = page.getByRole("dialog", { name: "Delete 5 projects?" });
  await sure.getByRole("button", { name: "Delete 5 projects" }).click();
  await sure.waitFor({ state: "detached" });
  have = await names();
  ok(
    !have.includes(org) && !have.includes("repo0") && have.includes("repo4"),
    `${tag} deleted the organisation and four of its repos, kept repo4 (${have})`,
  );
  const after = await api("GET", `/forges/${forge.id}/discover`);
  ok(after.length === 0, `${tag} deleted projects' repos aren't offered again`);
  ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    `${tag} no sideways scroll`,
  );
  for (const p of await api("GET", "/projects"))
    if (p.name === "repo4") await api("DELETE", `/projects/${p.key}`);
  await ctx.close();
}

for (const w of looks([1280, 390])) await run(w);
await browser.close();
gh.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
