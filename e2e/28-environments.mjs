// Environments and deployments (WP-D19), in a real browser at desktop and
// phone widths, light and dark. A fake app answers health checks and a
// version endpoint; CI (a token) records deployments; a cluster brings dev
// environments up and down. The project's Environments tab shows cards and a
// folded list of stopped dev environments, an environment's page has its
// history, deployments and settings, the Environments page lists every
// project's with the down ones first, the sidebar counts them, and a pull
// request says where its commit runs.
// Run after 01-signup-flow (user robin, an admin). The server's egress policy
// allows private networks but not loopback, so the fake app listens on this
// machine's LAN address; without one the checks can't run and the suite only
// looks at what doesn't depend on them.
import { chromium } from "playwright";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KEY = "AENV";
const REPO = "acme/waypoint";
const PR_SHA = "abc1234def5678abc1234def5678abc1234def56";
const PROD_SHA = "3fa9c1e5b7d29a41c0de88a1b2c3d4e5f6a7b8c9";

// The fake app: /ok is up, /broken is down, /version says what runs.
const lan = Object.values(os.networkInterfaces())
  .flat()
  .find((i) => i && i.family === "IPv4" && !i.internal)?.address;
const app = http.createServer((req, res) => {
  if (req.url === "/ok") res.end("ok");
  else if (req.url === "/version") {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ version: "2.4.0", git: { commit: PROD_SHA, branch: "main" } }));
  } else res.writeHead(503).end("broken");
});
await new Promise((r) => app.listen(0, "0.0.0.0", r));
const host = `http://${lan ?? "127.0.0.1"}:${app.address().port}`;
const live = !!lan;
if (!live) console.log("note: no LAN address; the checks can't reach the fake app");

const browser = await chromium.launch();
let prID = 0;
let squashedID = 0;
let taskKey = "";

async function seed(api, call) {
  const projects = await api("GET", "/projects");
  if (projects.some((p) => p.key === KEY)) return;
  await api("POST", "/projects", { name: "Env checks", key: KEY });
  await api("PATCH", `/projects/${KEY}`, { settings: { dev_env_url: "https://{name}.dev.example.com" } });
  const { token } = await api("POST", "/me/tokens", { name: "ci" });
  const ci = { ...H, Authorization: `Bearer ${token}` };
  const asCI = (m, p, d) => call(m, p, d, ci);

  const prod = await api("POST", `/projects/${KEY}/environments`, {
    name: "production",
    url: `${host}/ok`,
    version_url: `${host}/version`,
    commit_field: "git.commit",
    branch_field: "git.branch",
    version_field: "version",
  });
  const staging = await api("POST", `/projects/${KEY}/environments`, { name: "staging", url: `${host}/broken` });
  for (const [env, status] of [
    ["production", "started"],
    ["production", "succeeded"],
  ])
    await asCI("POST", `/projects/${KEY}/environments/${env}/deployments`, {
      commit: PROD_SHA,
      branch: "main",
      version: "2.4.0",
      status,
      url: "https://ci.example.com/runs/412",
      notes: "release build",
    });
  await asCI("POST", `/projects/${KEY}/environments/staging/deployments`, { commit: "9d8c7b6a5f4e", branch: "main", status: "started", url: "https://ci.example.com/runs/413" });
  await asCI("POST", `/projects/${KEY}/environments/staging/deployments`, { commit: "9d8c7b6a5f4e", status: "failed", notes: "migration 0021 failed" });

  await asCI("POST", `/projects/${KEY}/environments/a23x42s/up`, {
    url: `${host}/ok`,
    branch: "fix/login",
    commit: PR_SHA,
    ttl_s: 3 * 86400,
  });
  await asCI("POST", `/projects/${KEY}/environments/old-branch/up`, { url: `${host}/ok` });
  await asCI("POST", `/projects/${KEY}/environments/old-branch/down`);

  if (live) {
    await api("POST", `/environments/${prod.id}/check`);
    await api("POST", `/environments/${staging.id}/check`);
    await new Promise((r) => setTimeout(r, 3300)); // a check in the last 3 s is reused
    await api("POST", `/environments/${staging.id}/check`); // A second failed check in a row marks the environment down.
    const dev = (await api("GET", `/projects/${KEY}/environments/a23x42s`)).id;
    await api("POST", `/environments/${dev}/check`);
  }

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const now = Date.now();
  const forge = db
    .prepare(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Forge', 'https://git.example.com', ?) RETURNING id`)
    .get(now).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
  db.prepare("INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, ?)").run(project, forge, REPO, now);
  prID = db
    .prepare(
      `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, head_ref, head_sha, state, opened_at, created_at, updated_at)
       VALUES (?, ?, 41, 'Fix the login redirect', 'robin', 'https://git.example.com/acme/waypoint/pulls/41', 'fix/login', ?, 'open', ?, ?, ?) RETURNING id`,
    )
    .get(forge, REPO, PR_SHA, now - 3600_000, now - 3600_000, now).id;
  // Production runs a squash-merged pull request's merge commit, not its head.
  squashedID = db
    .prepare(
      `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, head_ref, head_sha, merge_sha, state, opened_at, merged_at, created_at, updated_at)
       VALUES (?, ?, 40, 'Speed up search', 'robin', 'https://git.example.com/acme/waypoint/pulls/40', 'search', '7777777aaaa', ?, 'merged', ?, ?, ?, ?) RETURNING id`,
    )
    .get(forge, REPO, PROD_SHA, now - 7200_000, now - 5400_000, now - 7200_000, now).id;
  db.prepare(
    `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, base_ref, head_ref, head_sha, merge_sha, state, opened_at, merged_at, created_at, updated_at)
     VALUES (?, ?, 42, 'Dark mode toggle', 'robin', 'https://git.example.com/acme/waypoint/pulls/42', 'main', 'dark', '8888888bbbb', '9999999cccc', 'merged', ?, ?, ?, ?)`,
  ).run(forge, REPO, now - 3000_000, now - 1800_000, now - 3000_000, now);
  db.prepare("UPDATE pull_requests SET base_ref = 'main' WHERE id = ?").run(squashedID);
  db.close();
  const task = await api("POST", "/issues", { project: KEY, title: "Faster search" });
  await api("POST", `/issues/${task.key}/links`, { url: "https://git.example.com/acme/waypoint/pulls/40" });
  taskKey = task.key;
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(300);
    const tall = await page.evaluate(() => {
      const panes = [...document.querySelectorAll(".overflow-y-auto")];
      panes.forEach((e) => e.scrollTo(0, 0));
      return Math.max(...panes.map((e) => e.scrollHeight));
    });
    await page.setViewportSize({ width, height: Math.min(Math.max(900, tall + 120), 6000) });
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
    await page.setViewportSize({ width, height: 900 });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await seed(api, call);

  await page.goto(`${base}/p/${KEY}/environments`);
  await page.getByRole("heading", { name: "Dev environments" }).waitFor();
  const card = (name) => page.locator("div.rounded-xl", { has: page.getByRole("link", { name, exact: true }) }).first();
  for (const name of ["production", "staging", "a23x42s"])
    ok(await page.getByRole("link", { name, exact: true }).first().isVisible(), `${tag} card for ${name}`);
  if (live) {
    await page.getByText(/^Up since/).first().waitFor();
    ok(true, `${tag} production says Up since …`);
    ok((await card("staging").innerText()).includes("Down since"), `${tag} staging is down`);
    ok((await card("staging").innerText()).includes("HTTP 503"), `${tag} staging says why`);
    ok((await card("production").innerText()).includes("main"), `${tag} production shows its branch`);
    ok((await card("production").innerText()).includes("3fa9c1e"), `${tag} production shows its commit`);
    ok((await card("production").innerText()).includes("v2.4.0"), `${tag} production shows its version`);
  }
  ok((await card("production").innerText()).includes("Deployed"), `${tag} production shows its last deploy`);
  ok((await card("staging").innerText()).includes("Deploy failed"), `${tag} staging's deploy failed`);
  ok((await card("a23x42s").innerText()).includes("fix/login"), `${tag} the dev environment's branch`);
  ok(await page.getByText("Stopped (1)").isVisible(), `${tag} the stopped one is folded`);
  ok(!(await page.getByRole("link", { name: "old-branch", exact: true }).isVisible()), `${tag} …and hidden until opened`);
  await page.getByText("Stopped (1)").click();
  ok(await page.getByRole("link", { name: "old-branch", exact: true }).isVisible(), `${tag} …and there when opened`);
  const href = await page.locator(`a[href^="${host}/ok"]`).first().getAttribute("target");
  ok(href === "_blank", `${tag} a card's address opens in a new tab`);
  ok(
    (await page.locator(`a[href^="https://git.example.com/acme/waypoint/commit/"]`).count()) > 0,
    `${tag} commits link to the forge`,
  );
  await shot("project-tab");
  await overflow("project tab");

  await page.getByRole("button", { name: "Add environment" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add environment" });
  await dialog.getByLabel("Name").fill("preview-7");
  await dialog.getByLabel("Address").fill("https://pr-7.preview.example.com");
  await shot("add-dialog");
  await dialog.getByRole("button", { name: "Add environment" }).click();
  await page.waitForURL(`**/p/${KEY}/environments/preview-7`);
  await page.getByRole("heading", { name: "preview-7" }).waitFor();
  ok(true, `${tag} adding one opens its page`);

  await page.goto(`${base}/p/${KEY}/environments/production`);
  await page.getByRole("heading", { name: "production" }).waitFor();
  for (const section of ["What it runs", "Uptime", "Deployments", "Settings"])
    ok(await page.getByRole("heading", { name: section }).isVisible(), `${tag} production has ${section}`);
  ok(await page.getByText("Succeeded").first().isVisible(), `${tag} the deployment succeeded`);
  ok(await page.getByText("CI run").first().isVisible(), `${tag} the deployment links its CI run`);
  ok(await page.getByText("release build").isVisible(), `${tag} deployment notes show`);
  if (live) {
    ok(await page.getByRole("img", { name: /^Status over the last 48 hours/ }).isVisible(), `${tag} the 48 hour strip`);
    // The response-time chart shows a note, not a line, until a few checks have run.
    ok(
      (await page.getByText(/^Response time|Response times show here/).count()) > 0,
      `${tag} the response time chart (or its note)`,
    );
  }
  ok(await page.getByRole("img", { name: /^Uptime by day/ }).isVisible(), `${tag} uptime by day`);
  await shot("environment");
  await overflow("environment page");
  const save = page.getByRole("button", { name: "Save settings" });
  ok(await save.isDisabled(), `${tag} nothing to save yet`);
  const lag = page.getByRole("combobox", { name: "Not deployed alert" });
  ok((await lag.innerText()).includes("After 45 minutes"), `${tag} production's not-deployed alert starts at 45 minutes`);
  await page.getByLabel("Branch field").fill("git.branch");
  await page.getByLabel("Expected status").fill("200");
  await lag.click();
  await page.getByRole("option", { name: "After 2 hours", exact: true }).click();
  await save.click();
  await page.getByText("Settings saved").waitFor();
  const saved = await api("GET", `/projects/${KEY}/environments/production`);
  ok(saved.expected_status === 200, `${tag} settings saved`);
  ok(saved.lag_alert_m === 120, `${tag} the not-deployed alert saved`);
  await api("PATCH", `/environments/${saved.id}`, { expected_status: 0, lag_alert_m: 45 });

  await page.goto(`${base}/p/${KEY}/environments/preview-7`);
  await page.getByRole("button", { name: "Delete environment" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete" }).click();
  await page.waitForURL(`**/p/${KEY}/environments`);
  await page.getByRole("heading", { name: "Dev environments" }).waitFor();
  ok(!(await page.getByRole("link", { name: "preview-7", exact: true }).isVisible()), `${tag} deleted`);

  await page.goto(`${base}/environments`);
  await page.getByRole("heading", { name: "Environments", level: 1 }).waitFor();
  await page.getByRole("link", { name: "production", exact: true }).first().waitFor();
  const names = await page.locator("a[href*='/environments/']").evaluateAll((els) => els.map((e) => e.textContent.trim()));
  ok(names.includes("staging") && names.includes("production"), `${tag} the page lists the project's environments`);
  if (live) {
    ok(
      names.indexOf("staging") < names.indexOf("production"),
      `${tag} the one that is down comes first (${names.join(", ")})`,
    );
    ok(/1 down/.test(await page.locator("h1 + div, h1 ~ div").first().innerText()), `${tag} the header counts what is down`);
  }
  ok(!names.includes("old-branch"), `${tag} stopped environments are left out`);
  await shot("all-environments");
  await overflow("all environments");
  if (live) {
    if (width >= 768) {
      const badge = page.locator('aside a[href="/environments"]');
      ok((await badge.innerText()).includes("1"), `${tag} the sidebar counts one down`);
    } else {
      await page.getByRole("button", { name: "Projects" }).click();
      const down = page.getByRole("link", { name: /Environments\s*1 down/ });
      await down.waitFor().catch(() => {});
      ok(await down.isVisible(), `${tag} the phone's drawer counts one down`);
      await page.keyboard.press("Escape");
    }
  }

  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  await page.getByRole("heading", { name: "Where it runs" }).waitFor();
  ok(await page.getByText("In a23x42s").isVisible(), `${tag} the pull request is in the dev environment`);
  ok(await page.getByText(/since \d/).first().isVisible(), `${tag} …since a time`);
  await shot("pull-request");
  await overflow("pull request");
  await page.goto(`${base}/agents?tab=pulls&pr=${squashedID}`);
  await page.getByRole("heading", { name: "Where it runs" }).waitFor();
  ok(await page.getByText("In production").isVisible(), `${tag} the squash-merged pull request is in production`);
  await shot("pull-request-squashed");

  await page.goto(`${base}/live`);
  await page.getByRole("heading", { name: "What’s live", level: 1 }).waitFor();
  await page.getByRole("heading", { name: /^Live/ }).waitFor();
  ok(await page.getByText(/Live in production since/).first().isVisible(), `${tag} the squash-merged one is live in production`);
  ok(await page.getByText("1 merged change not in it").isVisible(), `${tag} production lacks one merge`);
  ok(await page.getByText("Nothing deployed yet").isVisible(), `${tag} staging never deployed`);
  ok(await page.getByRole("heading", { name: /^Merged, not live yet/ }).isVisible(), `${tag} the later merge isn't live yet`);
  ok(await page.getByText("Dark mode toggle").isVisible(), `${tag} …and is listed`);
  ok(await page.getByRole("heading", { name: /^Needs someone/ }).isVisible(), `${tag} the open one nobody set to merge needs someone`);
  ok(await page.getByRole("link", { name: "Faster search" }).isVisible(), `${tag} a shipped task shows by its title`);
  await shot("whats-live");
  await overflow("what's live");
  await page.goto(`${base}/issue/${taskKey}`);
  await page.getByRole("note").filter({ hasText: /Live in production since/ }).waitFor();
  ok(true, `${tag} the task says it is live`);
  await page.getByRole("tab", { name: "Summary", exact: true }).click();
  const life = page.getByRole("group", { name: "Lifecycle" }).filter({ visible: true });
  ok((await life.getByText(/^Created .+/).count()) >= 1, `${tag} the task shows its creation date`);
  ok((await life.getByText(/^ · v?2\.4\.0$/).count()) === 1, `${tag} …and the release it was written on`);
  const writtenTip = (await life.getByText(/^ · v?2\.4\.0$/).getAttribute("title")) ?? "";
  ok(/ran .*2\.4\.0.* when this was written; still live/.test(writtenTip), `${tag} …with what was live then in its tooltip: ${writtenTip}`);
  await shot("task-live");

  await page.goto(`${base}/p/${KEY}/settings?tab=environments`);
  await page.getByLabel("Address pattern").waitFor();
  ok((await page.getByLabel("Address pattern").inputValue()) === "https://{name}.dev.example.com", `${tag} the dev address shows`);
  await page.getByLabel("Address pattern").fill("no name here");
  ok(await page.getByText("include {name}").isVisible(), `${tag} a pattern without {name} is refused`);
  await shot("settings");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
], [0, 2])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
app.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
