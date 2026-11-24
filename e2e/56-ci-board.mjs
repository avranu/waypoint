// The CI board (WP-225), in a real browser at desktop and phone widths, light
// and dark: a fake Forgejo with two repos, jobs running, queued and blocked,
// a job no runner can take, and a day of finished tasks. The page shows when
// everything should be done, the forecast, what runs and waits, and the last
// day; an admin sets the runner pool and the ETAs follow it. The board's
// reads are kept, so CI history (WP-490) charts the same day.
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
const KEY = "CIB";
const min = 60e3;
const now = Date.now();
const at = (minsAgo) => new Date(now - minsAgo * min).toISOString();
const EPOCH = "1970-01-01T00:00:00Z";

// What the fake Forgejo has: per repo, unfinished runs with their jobs, and
// finished tasks over the last day.
const repos = {
  "me/app": {
    active: [
      {
        id: 501, index_in_repo: 41, workflow_id: "ci.yml", status: "running", created: at(9), started: at(8), stopped: EPOCH,
        commit_sha: "a1", prettyref: "wp-12-search", trigger_event: "pull_request", title: "Search by label",
        html_url: "/me/app/actions/runs/41",
        jobs: [
          { id: 9001, name: "backend", status: "running", runs_on: ["forgejo-ci-heavy"], task_id: 7001, started: 8 },
          { id: 9002, name: "lint", status: "running", runs_on: ["forgejo-ci"], task_id: 7002, started: 7 },
          { id: 9003, name: "e2e", status: "waiting", runs_on: ["forgejo-ci-heavy"] },
          { id: 9004, name: "release", status: "blocked", runs_on: ["forgejo-ci"], needs: ["backend", "lint", "e2e"] },
        ],
      },
      {
        id: 502, index_in_repo: 42, workflow_id: "gpu.yml", status: "waiting", created: at(4), started: EPOCH, stopped: EPOCH,
        commit_sha: "a2", prettyref: "main", trigger_event: "push", title: "Benchmarks", html_url: "/me/app/actions/runs/42",
        jobs: [{ id: 9010, name: "bench", status: "waiting", runs_on: ["gpu"] }],
      },
    ],
    history: { backend: [14, 12, 16, 13, 25], lint: [3, 4, 3, 3, 5], e2e: [18, 20, 22, 19, 21], release: [2, 2, 3, 2, 2] },
  },
  "me/web": {
    active: [
      {
        id: 601, index_in_repo: 7, workflow_id: "test.yml", status: "waiting", created: at(3), started: EPOCH, stopped: EPOCH,
        commit_sha: "b1", prettyref: "wp-14-dark", trigger_event: "pull_request", title: "Dark mode", html_url: "/me/web/actions/runs/7",
        jobs: [{ id: 9101, name: "unit", status: "waiting", runs_on: ["forgejo-ci"] }],
      },
    ],
    history: { unit: [6, 7, 6, 8, 30, 6] },
  },
};

// Tasks: each history entry is one run (number n) whose jobs ran in turn,
// spread over the last day; one in four lint runs failed.
function tasksOf(repo) {
  const r = repos[repo];
  const out = [];
  let id = 6000;
  for (const run of r.active)
    for (const j of run.jobs)
      if (j.status === "running")
        out.push({ id: j.task_id, name: j.name, run_number: run.index_in_repo, workflow_id: run.workflow_id, status: "running",
          created_at: at(j.started), run_started_at: at(j.started), updated_at: at(0), url: `${run.html_url}/jobs/0`, head_sha: run.commit_sha });
  const names = Object.keys(r.history);
  const n = r.history[names[0]].length;
  for (let k = 0; k < n; k++) {
    let t = 22 * 60 - k * 240;
    const number = 30 - k;
    for (const name of names) {
      const d = r.history[name][k] ?? 5;
      const start = t - (name === names[0] ? 2 : 6);
      const end = start - d;
      out.push({ id: id++, name, run_number: number, workflow_id: repo === "me/app" ? "ci.yml" : "test.yml",
        status: name === "lint" && k % 4 === 1 ? "failure" : "success", created_at: at(start), run_started_at: at(start),
        updated_at: at(end), url: `/${repo}/actions/runs/${number}/jobs/0`, head_sha: "old" + k, display_title: "Run " + number, head_branch: "main" });
      t = end;
    }
  }
  return out.sort((a, b) => b.id - a.id);
}

function runsOf(repo) {
  const r = repos[repo];
  const n = r.history[Object.keys(r.history)[0]].length;
  const past = Array.from({ length: n }, (_, k) => ({ id: 400 + k, index_in_repo: 30 - k, workflow_id: "ci.yml", status: "success",
    created: at(22 * 60 - k * 240), started: at(22 * 60 - k * 240 - 1), stopped: at(22 * 60 - k * 240 - 40), title: "Run " + (30 - k) }));
  return [...r.active.map(({ jobs, ...run }) => run), ...past];
}

let runnersVisible = true;
const srv = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => {
    const send = (code, body) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(body === undefined ? "" : JSON.stringify(body));
    };
    const u = new URL(req.url, "http://x");
    if (u.pathname === "/api/v1/admin/actions/runners") {
      if (!runnersVisible) return send(403, { message: "forbidden" });
      return send(200, [
        { id: 1, name: "runner1", status: "active", labels: ["forgejo-ci"], version: "13.2.0" },
        { id: 2, name: "runner2", status: "active", labels: ["forgejo-ci", "forgejo-ci-avx2", "forgejo-ci-heavy"], version: "13.2.0" },
      ]);
    }
    const m = u.pathname.match(/^\/api\/v1\/repos\/([^/]+\/[^/]+)(?:\/(.*))?$/);
    if (!m || !repos[m[1]]) return send(404, { message: "Not Found" });
    const repo = m[1];
    const rest = m[2] ?? "";
    if (rest === "") return send(200, { default_branch: "main", full_name: repo });
    if (rest.startsWith("hooks")) return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    if (rest === "actions/runs") {
      const st = u.searchParams.getAll("status");
      const list = runsOf(repo).filter((r) => !st.length || st.includes(r.status));
      return send(200, { total_count: list.length, workflow_runs: list });
    }
    const jm = rest.match(/^actions\/runs\/(\d+)\/jobs$/);
    if (jm) {
      const run = repos[repo].active.find((r) => r.id === Number(jm[1]));
      return send(200, (run?.jobs ?? []).map(({ started, ...j }) => ({ run_id: run.id, needs: [], task_id: 0, ...j })));
    }
    if (rest === "actions/tasks") {
      const st = u.searchParams.getAll("status");
      const page = Number(u.searchParams.get("page") ?? 1);
      const list = tasksOf(repo).filter((t) => !st.length || st.includes(t.status));
      return send(200, { total_count: list.length, workflow_runs: list.slice((page - 1) * 50, page * 50) });
    }
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
const browser = await chromium.launch();

async function signIn(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

let api, forgeID;
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  await api("POST", "/projects", { name: "CI board", key: KEY });
  const forge = await api("POST", "/forges", { kind: "forgejo", name: "CI board forge", base_url: fake, token: "tok", webhook_secret: "e2e-ci" });
  forgeID = forge.id;
  for (const repo of Object.keys(repos)) await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo, sync_issues: false });
  const b = await api("GET", "/ci");
  ok(b.repos.length >= 2 && b.repos.every((r) => !r.error || !r.repo.startsWith("me/")), `the board reads both repos: ${JSON.stringify(b.repos.map((r) => [r.repo, r.error]))}`);
  ok(b.summary.running === 2 && b.summary.waiting === 3 && b.summary.blocked === 1, `running/waiting/blocked: ${JSON.stringify(b.summary)}`);
  ok(b.summary.stuck === 1 && !b.summary.all_done_at, "the gpu job has no runner, so nothing says when all is done");
  const pool = b.pools.find((p) => p.forge_id === forgeID);
  ok(pool?.source === "runners" && pool.forge_runners.length === 2, `the pool comes from the forge's runners: ${pool?.source}`);
  ok(b.recent.length > 10 && b.stats.length >= 5, `a day of history: ${b.recent.length} finished, ${b.stats.length} jobs`);
}

async function look(width, scheme, edit) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const shot = async (name) => {
    await page.addStyleTag({ content: "html, body, #root, #root * { overflow: visible !important; } #root, #root > div { height: auto !important; }" });
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(`${base}/ci`);
  await page.getByText("Everything done").waitFor();
  const text = await page.locator("main, body").first().innerText();
  ok(text.includes("Never") && text.includes("can't start"), `${tag} says the gpu job keeps it from finishing`);
  ok(text.includes("no runner offers gpu"), `${tag} and why`);
  for (const s of ["Forecast", "Runs in progress", "Running · 2", "Queue · 4", "Last 24 hours", "Recently finished", "Jobs · last 24 hours", "Runner time by repo"])
    ok(text.includes(s), `${tag} shows ${s}`);
  ok(text.includes("needs backend, lint, e2e"), `${tag} a blocked job says what it needs`);
  await overflow("board");
  await shot("ci");

  if (edit) {
    const sec = page.locator("section").filter({ has: page.getByText("Runner pool · CI board forge") });
    await sec.getByRole("button", { name: "Edit" }).click();
    const caps = sec.getByLabel("Capacity");
    await caps.nth(0).fill("2");
    await caps.nth(1).fill("8");
    await sec.getByRole("button", { name: "Add runner" }).click();
    await sec.getByLabel("Runner name").nth(2).fill("cuda");
    await sec.getByLabel("Labels").nth(2).fill("gpu");
    await caps.nth(2).fill("1");
    await sec.getByRole("button", { name: "Save" }).click();
    await sec.getByText("set by an admin").waitFor();
    const b = await api("GET", "/ci");
    const pool = b.pools.find((p) => p.forge_id === forgeID);
    ok(pool?.source === "configured" && pool.capacity === 11, `the pool is saved: ${JSON.stringify(pool?.runners)}`);
    ok(b.summary.stuck === 0 && b.summary.all_done_at, "now everything has a runner and an ETA");
    await page.reload();
    await page.getByText("Everything done").waitFor();
    ok(!(await page.getByText("no runner offers gpu").count()), `${tag} the gpu job is no longer stuck`);
    await shot("ci-configured");
  }
  if (width < 600) await page.getByText("Forecast").first().scrollIntoViewIfNeeded();
  await ctx.close();
}

// A host reports itself (after the pool was configured: a report takes over
// the pool): 5 GiB free of 100 GiB is low on disk.
async function lookHosts(width, scheme) {
  const tag = `${width}/${scheme} hosts`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  await page.goto(`${base}/ci`);
  await page.getByText("Everything done").waitFor();
  const hosts = page.locator("section").filter({ has: page.getByText(/^Hosts · /) });
  await hosts.waitFor();
  const ht = await hosts.innerText();
  ok(ht.includes("e2e-host") && ht.includes("0/4 slots") && ht.includes("forgejo-ci"), `${tag} the host, its slots and labels: ${ht.replace(/\n/g, " | ")}`);
  ok(ht.includes("Low disk") && ht.includes("5.0 GiB free of 100.0 GiB"), `${tag} the host shows its low disk`);
  ok(ht.includes("8 CPUs, 16.0 GiB") && !ht.includes("each job 0") && ht.includes("reported"), `${tag} the host's CPUs and memory and report age`);
  const pt = await page.locator("main, body").first().innerText();
  ok(pt.includes("from the hosts’ reports"), `${tag} the pool says it is from the hosts' reports`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await page.addStyleTag({ content: "html, body, #root, #root * { overflow: visible !important; } #root, #root > div { height: auto !important; }" });
  await page.screenshot({ path: `${out}/ci-hosts-${width}-${scheme}.png`, fullPage: true });
  await ctx.close();
}

// CI history (WP-490): the board's reads were kept, so the history has the
// day's jobs and full runs, by day and by week.
async function lookHistory(width, scheme) {
  const tag = `${width}/${scheme} history`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  await page.goto(`${base}/ci`);
  await page.getByRole("link", { name: "History", exact: true }).click();
  await page.waitForURL(/\/ci\/history/);
  await page.getByText("Last 7 days, against the 7 before").waitFor();
  const text = await page.locator("main, body").first().innerText();
  ok(/kept since/.test(text), `${tag} says since when history is kept`);
  for (const s of ["Jobs per day", "Runner hours per day", "Full CI run", "Wait for a runner", "Merges per day", "Time per merge", "Jobs, last 7 days against the 7 before"])
    ok(text.includes(s), `${tag} shows ${s}`);
  ok(text.includes("40m"), `${tag} a full run took 40m`);
  ok(text.includes("backend") && text.includes("unit"), `${tag} lists the jobs`);
  ok((await page.locator("[aria-label$=\"use arrow keys to read values\"]").count()) >= 6, `${tag} draws six charts`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await page.addStyleTag({ content: "html, body, #root, #root * { overflow: visible !important; } #root, #root > div { height: auto !important; }" });
  await page.screenshot({ path: `${out}/ci-history-${width}-${scheme}.png`, fullPage: true });
  await page.getByRole("radio", { name: "Week" }).click();
  await page.waitForURL(/bucket=week/);
  await page.getByText("Jobs per week").waitFor();
  ok(true, `${tag} switches to weeks`);
  await page.getByRole("link", { name: "Now", exact: true }).click();
  await page.getByText("Everything done").waitFor();
  ok(page.url().endsWith("/ci"), `${tag} back to the board`);
  await ctx.close();
}

try {
  for (const [w, s, e] of looks([[1280, "dark", false], [390, "light", false], [390, "dark", false], [1280, "light", true]], 3)) await look(w, s, e);
  const ack = await api("POST", "/ci/report", { forge: "CI board forge", host: "e2e-host", labels: ["forgejo-ci"], capacity: 4, cpus: 8,
    memory: 17179869184, disk_total: 107374182400, disk_free: 5368709120, containers: [] });
  ok(ack && !ack.error, `the host report is taken: ${JSON.stringify(ack)}`);
  const hb = await api("GET", "/ci");
  const host = hb.hosts?.find((h) => h.name === "e2e-host" && h.forge_id === forgeID);
  ok(host?.health === "low_disk", `the board lists the host as low on disk: ${JSON.stringify(host)}`);
  for (const [w, s] of looks([[1280, "light"], [390, "light"], [390, "dark"]])) await lookHosts(w, s);
  const h = await api("GET", "/ci/history?days=30");
  ok(h.since && h.repos.includes("me/app") && h.now.jobs >= 20 && h.now.runs >= 5 && h.now.run_p50 === 40 * 60,
    `the board's reads are kept as history: ${JSON.stringify({ since: h.since, repos: h.repos, now: h.now })}`);
  ok(h.points.length === 30 && h.jobs.some((j) => j.name === "backend"), `30 days by day, and the jobs: ${h.points.length}`);
  for (const [w, s] of looks([[1280, "light"], [390, "dark"], [1280, "dark"], [390, "light"]], [0, 1])) await lookHistory(w, s);
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
srv.close();
console.log(errors.length ? `${errors.length} failures` : "all ok");
process.exit(errors.length ? 1 : 0);
