// The Agents overview and sessions (WP-D16) in a real browser at desktop and
// phone widths, light and dark. Two agents work on two machines: their
// sessions (working, idle, waiting for a person), a question, tasks queued
// and held, pull requests with checks and a gate, pages and terms they
// wrote. The Overview tab (the default) shows all of it in one place:
// waiting on you, the summary, sessions by machine and branch, up next,
// pull requests, branches, recent changes with kind chips, what was done,
// environments and usage; sections fold and remember it. The Sessions tab
// lists and filters; a session has its own page with its summary, requests,
// files, usage and timeline; a request carries files too; the Usage tab's
// session rows link to the page; and a new request appears without a reload.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KEY = "OVW";
const PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==",
  "base64",
);

const browser = await chromium.launch();
let opus, haiku; // headers of robin's two agents
let sessionIDs = {};

// Picks an option from one of the app's dropdowns (a combobox + listbox).
async function pick(pg, name, option) {
  await pg.getByRole("combobox", { name, exact: true }).click();
  await pg.getByRole("option", { name: option, exact: true }).click();
}

const hour = (n) => Math.floor((Date.now() - n * 3600e3) / 3600e3) * 3600e3;

async function seed(api, call) {
  const projects = await api("GET", "/projects");
  if (projects.some((p) => p.key === KEY)) return;
  await api("POST", "/projects", { name: "Overview", key: KEY });
  const tok = async (name, agent) => {
    const { token } = await api("POST", "/me/tokens", { name, agent });
    return { ...H, Authorization: `Bearer ${token}` };
  };
  opus = await tok("opus", "anthropic/claude-code");
  haiku = await tok("haiku", "anthropic/claude-haiku-5-5");

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const forge = db
    .prepare(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Forge', 'http://127.0.0.1:1', ?) RETURNING id`)
    .get(Date.now()).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
  db.prepare("INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', ?)").run(project, forge, Date.now());
  // Pull requests: one open with checks and a gate and set to merge, one failing, one merged by an agent.
  const pr = db.prepare(`INSERT INTO pull_requests(forge_id, repo, number, title, author, url, base_ref, head_ref, head_sha, state, created_at, updated_at,
      opened_at, merged_at, agent, ci_state, ci_sha, gate_state, armed_at) VALUES (?, 'acme/app', ?, ?, 'robin', ?, 'main', ?, ?, ?, ?, ?, ?, ?, 'anthropic/claude-code', ?, ?, ?, ?)`);
  const now = Date.now();
  pr.run(forge, 5, "Overview tab for the Agents page", "http://127.0.0.1:1/acme/app/pulls/5", "feat/overview", "aaaa111", "open", now - 7200e3, now - 600e3, now - 7200e3, null, "passed", "aaaa111", "pending", now - 300e3);
  pr.run(forge, 6, "Dark theme for the sessions list", "http://127.0.0.1:1/acme/app/pulls/6", "feat/dark", "bbbb222", "open", now - 5400e3, now - 1200e3, now - 5400e3, null, "failed", "bbbb222", "failure", null);
  pr.run(forge, 4, "Record a session's files", "http://127.0.0.1:1/acme/app/pulls/4", "feat/files", "cccc333", "merged", now - 86400e3, now - 3600e3, now - 86400e3, now - 3600e3, "passed", "cccc333", "success", null);
  db.close();

  // Sessions, by their hooks: two on one machine (one working, one idle), one waiting for a person on another.
  const hook = (h, ev, extra = {}) =>
    call("POST", "/sessions/hook", { events: [{ event: ev, session_id: extra.session_id, machine: extra.machine, cwd: extra.cwd, repo: extra.repo, branch: extra.branch, ...extra }] }, h);
  const start = async (h, session_id, machine, repo, branch, model, title, prompt) => {
    const w = { session_id, machine, cwd: `/work/${repo.split("/")[1]}`, repo, branch };
    await hook(h, "SessionStart", { ...w, model });
    await hook(h, "UserPromptSubmit", { ...w, prompt, title });
    return w;
  };
  const s1 = await start(opus, "cli-ov-1", "ov-box", "acme/app", "feat/overview", "claude-opus-5-5", "Overview tab", "Build the Agents overview tab");
  const s2 = await start(opus, "cli-ov-2", "ov-box", "acme/app", "main", "claude-opus-5-5", "Tidy the config loader", "Tidy the config loader");
  await hook(opus, "Stop", { ...s2, last_message: "Config loader tidied: two functions merged, tests pass.\nNothing else changed.", input_tokens: 41000, output_tokens: 9200 });
  const s3 = await start(haiku, "cli-ov-3", "ov-laptop", "acme/web", "feat/dark", "claude-haiku-5-5", "Dark theme", "Add dark theme tokens");
  await hook(haiku, "Notification", { ...s3, notification_type: "permission_prompt", message: "Claude needs your permission to use Bash" });
  await hook(opus, "Stop", { ...s1, session_id: "cli-ov-1", last_message: "", input_tokens: 0, output_tokens: 0 }).catch(() => {});
  await hook(opus, "UserPromptSubmit", { ...s1, prompt: "Build the Agents overview tab", title: "Overview tab" });

  // A question from the working session.
  const as1 = { ...opus, "X-Waypoint-Session": "cli-ov-1", "X-Waypoint-Machine": "ov-box" };
  const ask = await call("POST", "/requests", {
    kind: "question",
    title: "Should the overview group sessions by machine first?",
    body: "Robin runs agents on several machines; grouping by machine first keeps each box together.",
    questions: [{ question: "Group by machine first?", choices: [{ label: "Machine, then repo" }, { label: "Repo, then machine" }] }],
  }, as1);
  if (!ask.ok()) throw new Error(`ask: ${ask.status()} ${await ask.text()}`);
  const later = await call("POST", "/requests", { kind: "action", title: "Rotate the staging deploy key", body: "Only a person can." }, as1);
  const lr = await later.json();
  await call("POST", `/requests/${lr.id}/snooze`, { minutes: 600 });

  // Tasks: two for claude (the head is urgent), one for haiku, one backlog; the agent takes the urgent one.
  const mk = (title, extra) => api("POST", "/issues", { project: KEY, title, ...extra });
  await mk("Fix the flaky sessions test", { agent: "claude", priority: "urgent" });
  await mk("Tidy the README", { agent: "claude", priority: "low" });
  await mk("Rename a CSS class", { agent: "haiku" });
  await mk("Someday: cache the overview", { agent: "claude", status: "backlog" });
  await mk("Write the changelog entry", { agent: "claude", priority: "high" });
  const next = await call("POST", "/agent/next", {}, as1);
  if (!next.ok()) throw new Error(`next: ${next.status()} ${await next.text()}`);
  const task = (await next.text()).match(/OVW-\d+/)?.[0];
  // Another agent finishes a task and files one for later.
  const as2 = { ...opus, "X-Waypoint-Session": "cli-ov-2", "X-Waypoint-Machine": "ov-box" };
  const next2 = await call("POST", "/agent/next", {}, as2);
  const task2 = (await next2.text()).match(/OVW-\d+/)?.[0];
  if (task2) {
    const d = await call("POST", `/agent/done/${task2}`, { comment: "Done in the config loader." }, as2);
    if (!d.ok()) throw new Error(`done: ${d.status()} ${await d.text()}`);
  }
  await call("POST", "/issues", { project: KEY, title: "Follow up: group sessions by repo too", status: "backlog" }, as2);

  // Knowledge written by agents.
  await call("POST", "/pages", { title: "How the overview is built", category: "note", project: KEY, body: "One request, many panels." }, as2);
  await call("POST", "/glossary", { term: "Overview", definition: "The first tab of the Agents page.", project: KEY }, as2);

  // Usage for the working session: tokens by model, tools, Bash heads, a refusal.
  const h = hour(1);
  const ing = await api("POST", "/usage/ingest", {
    machine: "ov-box", version: "test",
    chunks: [{
      id: "ov-1",
      messages: [
        { h, s: "cli-ov-1", repo: "acme/app", model: "claude-opus-5-5", kind: "main", n: 24, in: 61000, out: 38000, cw5: 190000, cw1: 0, cr: 3100000 },
        { h, s: "cli-ov-1", repo: "acme/app", model: "claude-haiku-5-5", kind: "Explore", n: 9, in: 12000, out: 6000, cw5: 20000, cw1: 0, cr: 210000 },
      ],
      tools: [
        { h, s: "cli-ov-1", repo: "acme/app", kind: "builtin", tool: "Bash", calls: 40, errors: 3, bytes: 90000 },
        { h, s: "cli-ov-1", repo: "acme/app", kind: "builtin", tool: "Edit", calls: 22, bytes: 12000 },
        { h, s: "cli-ov-1", repo: "acme/app", kind: "bash", tool: "Bash", detail: "go test", calls: 14, errors: 2, bytes: 70000 },
        { h, s: "cli-ov-1", repo: "acme/app", kind: "bash", tool: "Bash", detail: "git status", calls: 9 },
      ],
      sessions: [{ s: "cli-ov-1", repo: "acme/app", branch: "feat/overview", cwd: "/work/app", title: "Overview tab", first: hour(2), last: hour(0) }],
      refusals: [{ t: hour(1) + 5000, s: "cli-ov-1", repo: "acme/app", tool: "Bash", detail: "git push", reason: "Production Deploy", source: "classifier", excerpt: "git push origin main --force" }],
    }],
  });
  ok(ing && ing.accepted === 1, "seed: usage ingested");
  // An environment, so the strip has something to say.
  await api("POST", `/projects/${KEY}/environments`, { name: "production", kind: "production" }).catch(() => {});
  return task;
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
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  };
  const tallShot = async (name) => {
    await page.waitForTimeout(400);
    const tall = await page.evaluate(() => Math.max(...[...document.querySelectorAll(".overflow-y-auto")].map((e) => e.scrollHeight)));
    await page.setViewportSize({ width, height: Math.min(Math.max(900, tall + 120), 6000) });
    await page.waitForTimeout(200);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
    await page.setViewportSize({ width, height: 900 });
  };
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await seed(api, call);

  // ---- the API: one round trip ----
  const t0 = Date.now();
  const ov = await api("GET", `/agents/overview?days=30`);
  const took = Date.now() - t0;
  console.log(`     overview API answered in ${took} ms`);
  ok(ov.sessions.filter((s) => s.cli_id.startsWith("cli-ov-")).length === 3, `${tag} API: my three sessions are live (${ov.sessions_total} in all)`);
  ok(ov.waiting.requests.length >= 1 && ov.summary.waiting.count >= 1, `${tag} API: a request waits on robin`);
  ok(ov.next.some((g) => g.agent === "claude" && g.items.length > 0) && ov.next.some((g) => g.agent === "haiku"), `${tag} API: heads of the claude and haiku queues`);
  ok(ov.pulls.total >= 2 && ov.pulls.open.some((p) => p.armed && p.ci === "passed"), `${tag} API: two open pull requests, one set to merge`);
  ok(ov.branches.some((b) => b.branch === "feat/overview" && b.sessions.length === 1 && b.pr), `${tag} API: feat/overview has a session and a PR`);
  ok(ov.summary.done.parts.some((p) => p.kind === "issue") && ov.summary.done.parts.some((p) => p.kind === "pr"), `${tag} API: done has issues and PRs`);
  ok(ov.summary.deferred.parts.some((p) => p.kind === "request") && ov.summary.deferred.parts.some((p) => p.kind === "issue"), `${tag} API: deferred has a snoozed request and a filed issue`);
  const ch = await api("GET", "/changes?kind=page");
  ok(ch.changes.length >= 1 && ch.changes[0].agent === "anthropic/claude-code", `${tag} API: a page change by claude-code`);

  // ---- the Overview tab ----
  await page.goto(base + "/agents");
  await page.getByRole("tab", { name: "Overview" }).waitFor();
  const tabs = await page.getByRole("tab").allTextContents();
  ok(tabs.slice(0, 3).join() === "Overview,Queue,Sessions", `${tag} tabs: ${tabs.join(", ")}`);
  ok((await page.getByRole("tab", { name: "Overview" }).getAttribute("aria-selected")) === "true", `${tag} Overview is the default tab`);
  await page.getByText("Which agent", { exact: false }).count();
  await page.getByRole("region", { name: "Waiting on you" }).waitFor();
  ok(await page.getByRole("region", { name: "Waiting on you" }).getByText("Should the overview group sessions by machine first?").isVisible(), `${tag} the question is waiting on you`);
  for (const id of ["doing", "done", "deferred", "waiting"]) ok((await page.locator(`[data-summary=${id}]`).count()) === 1, `${tag} summary column ${id}`);
  const doing = page.locator("[data-summary=doing]");
  ok(await doing.getByText("Overview tab").first().isVisible(), `${tag} doing lists the working session`);
  const cols = await page.locator("[data-summary]").evaluateAll((els) => new Set(els.map((e) => Math.round(e.getBoundingClientRect().top))).size);
  ok(width >= 1024 ? cols === 1 : width < 640 ? cols === 4 : true, `${tag} summary columns: ${cols} row(s) of tops (${width >= 1024 ? "side by side" : "stacked"})`);

  const sessions = page.getByRole("region", { name: "Sessions now" });
  ok(await sessions.getByText("ov-box", { exact: true }).isVisible() && (await sessions.getByText("ov-laptop", { exact: true }).isVisible()), `${tag} sessions grouped by machine`);
  ok(await sessions.getByText("feat/overview").first().isVisible(), `${tag} sessions show their branch`);
  ok((await sessions.getByRole("img", { name: "Working" }).count()) >= 1 && (await sessions.getByRole("img", { name: "Waiting for you" }).count()) === 1 && (await sessions.getByRole("img", { name: "Idle" }).count()) === 1,
    `${tag} state dots: working, waiting for you, idle`);
  ok(await sessions.getByText("Config loader tidied: two functions merged, tests pass.").isVisible(), `${tag} the last update shows`);
  // Other suites on this server delegate issues too, and Up next shows only the
  // first few heads per agent: look at it through this suite's own project filter.
  await page.goto(`${base}/agents?project=${KEY}`);
  const next = page.getByRole("region", { name: "Up next" });
  await next.getByText("Rename a CSS class").waitFor();
  ok(await next.getByText("Fix the flaky sessions test").isVisible() || (await next.getByText("Write the changelog entry").isVisible()), `${tag} up next lists a claude task`);
  ok(await next.getByText("Rename a CSS class").isVisible(), `${tag} up next lists haiku's task`);
  await page.goto(base + "/agents");
  await page.getByRole("region", { name: "Pull requests" }).waitFor();
  const pulls = page.getByRole("region", { name: "Pull requests" });
  ok(await pulls.getByText("Checks passed").isVisible() && (await pulls.getByText("Checks failed").isVisible()) && (await pulls.getByText("Merges when ready").isVisible()), `${tag} pull requests show checks, gate and armed`);
  const branches = page.getByRole("region", { name: "Branches in work" });
  ok(await branches.getByText("feat/dark").first().isVisible() && (await branches.getByText("feat/overview").first().isVisible()), `${tag} branches in work`);
  const changes = page.getByRole("region", { name: "Recent changes" });
  ok(await changes.getByText("How the overview is built").isVisible(), `${tag} recent changes include the page`);
  await changes.getByRole("radio", { name: "Glossary" }).click();
  await changes.getByText("Overview", { exact: true }).first().waitFor();
  ok((await changes.getByText("How the overview is built").count()) === 0, `${tag} the Glossary chip leaves out pages`);
  await changes.getByRole("radio", { name: "All" }).click();
  const done = page.getByRole("region", { name: "Done recently" });
  ok(await done.getByText(/Closed by/).first().isVisible(), `${tag} done recently lists a closed issue`);
  ok(await done.getByText(/Merged by/).first().isVisible(), `${tag} …and a merged pull request`);
  ok(await page.getByText("Agent usage this week").isVisible(), `${tag} the usage strip`);
  await overflow("overview");
  await tallShot("overview");

  // Folding is remembered.
  await sessions.getByRole("button", { name: /Sessions now/ }).click();
  ok((await sessions.getByText("Tidy the config loader").count()) === 0, `${tag} Sessions now folds`);
  await page.reload();
  await page.getByRole("region", { name: "Sessions now" }).waitFor();
  ok((await page.getByRole("region", { name: "Sessions now" }).getByText("Tidy the config loader").count()) === 0, `${tag} …and stays folded after a reload`);
  await page.getByRole("region", { name: "Sessions now" }).getByRole("button", { name: /Sessions now/ }).click();
  ok(await page.getByRole("region", { name: "Sessions now" }).getByText("Tidy the config loader").isVisible(), `${tag} …and opens again`);

  // Filters carry: an agent that isn't there empties it with one-line hints.
  // (An agent matches the work anyone may take, so empty it with a project that has none.)
  if (!(await api("GET", "/projects")).some((p) => p.key === "OVEMT")) await api("POST", "/projects", { name: "Overview empty", key: "OVEMT" });
  await page.goto(base + "/agents?agent=gemini&project=OVEMT");
  await page.getByRole("region", { name: "Sessions now" }).getByText(/Agent sessions appear here/).waitFor();
  ok(await page.getByText("Nothing waits on you.").isVisible(), `${tag} nothing waits on gemini's behalf`);
  ok(await page.getByRole("region", { name: "Up next" }).getByText(/Issues delegated to an agent/).isVisible(), `${tag} empty sections say what fills them`);
  await shot("overview-empty");
  await page.goto(base + "/agents");

  // ---- a request opens as a sheet, with files ----
  await page.getByRole("region", { name: "Waiting on you" }).getByText("Should the overview group sessions by machine first?").click();
  const sheet = page.getByRole("dialog", { name: /Request/ });
  await sheet.waitFor();
  await sheet.getByRole("link", { name: /box/ }).waitFor();
  ok((await sheet.getByRole("link", { name: /box/ }).getAttribute("href")).includes("/agents/sessions/"), `${tag} the request links to the session page`);
  await sheet.getByLabel("Choose files to attach").setInputFiles({ name: "layout.png", mimeType: "image/png", buffer: PNG });
  await sheet.getByRole("img", { name: /layout.png/ }).or(sheet.getByText("layout.png")).first().waitFor();
  ok(true, `${tag} a file attached to the request shows`);
  await shot("request-sheet");
  await page.keyboard.press("Escape");

  // ---- the Sessions tab ----
  await page.getByRole("tab", { name: "Sessions" }).click();
  await page.getByRole("list", { name: "Sessions" }).waitFor();
  ok((await page.getByRole("list", { name: "Sessions" }).getByRole("listitem").count()) >= 3 && (await page.getByText("Dark theme").first().isVisible()), `${tag} the active sessions are listed`);
  await pick(page, "State", "Waiting for you");
  await page.getByText("Dark theme").first().waitFor();
  // The list refilters after the pick: wait for the others to leave rather than counting once.
  await page.getByText("Overview tab").first().waitFor({ state: "detached", timeout: 5000 }).catch(() => {});
  ok((await page.getByText("Overview tab").count()) === 0, `${tag} state filter: only those waiting for you`);
  await pick(page, "State", "Active");
  await pick(page, "Machine", "ov-box");
  await page.waitForFunction(() => document.querySelectorAll('ul[aria-label="Sessions"] > li').length === 2);
  ok(true, `${tag} machine filter: two on box`);
  await shot("sessions");
  await pick(page, "Machine", "All machines");

  // ---- a session's page ----
  await page.getByRole("list", { name: "Sessions" }).getByText("Overview tab").first().click();
  await page.waitForURL(/\/agents\/sessions\/\d+/);
  sessionIDs.main = page.url().match(/sessions\/(\d+)/)[1];
  await page.getByRole("heading", { name: "Overview tab" }).waitFor();
  ok(await page.getByRole("region", { name: "Summary" }).isVisible(), `${tag} the session page has its summary`);
  ok(await page.getByRole("region", { name: "Summary" }).getByText("Should the overview group sessions by machine first?").isVisible(), `${tag} …with its open question`);
  ok(await page.getByRole("heading", { name: /Requests · \d+/ }).isVisible(), `${tag} its requests`);
  await page.getByLabel("Choose files to attach").setInputFiles({ name: "trace.log", mimeType: "text/plain", buffer: Buffer.from("12:00 started\n12:01 done\n") });
  await page.getByText("trace.log").first().waitFor();
  ok(true, `${tag} a file attached to the session shows`);
  await page.getByText("Fresh tokens").first().waitFor();
  ok(await page.getByRole("heading", { name: "By model" }).isVisible() && (await page.getByText("opus 5.5").or(page.getByText("claude-opus-5-5")).first().isVisible()), `${tag} usage by model`);
  ok(await page.getByText("go test").first().isVisible(), `${tag} Bash command heads`);
  ok(await page.getByRole("heading", { name: "Permission refusals" }).isVisible(), `${tag} refusals`);
  ok(await page.getByRole("heading", { name: /Timeline/ }).isVisible(), `${tag} timeline`);
  await overflow("session page");
  await tallShot("session");
  // The summary of the question opens as a sheet from here, too.
  await page.getByRole("region", { name: "Summary" }).getByText("Should the overview group sessions by machine first?").click();
  await page.getByRole("dialog", { name: /Request/ }).waitFor();
  await page.keyboard.press("Escape");

  // ---- the Usage tab's session rows link to the page ----
  await page.goto(base + "/agents?tab=usage");
  await page.getByRole("radio", { name: "Session" }).click();
  const link = page.getByRole("link", { name: "Session", exact: true }).first();
  await link.waitFor();
  await link.click();
  await page.waitForURL(/\/agents\/sessions\/\d+/);
  ok(page.url().includes(`/agents/sessions/${sessionIDs.main}`), `${tag} a usage row opens its session`);

  // ---- live: a new request appears on the open overview ----
  await page.goto(base + "/agents");
  const region = page.getByRole("region", { name: "Waiting on you" });
  await region.waitFor();
  const before = await region.getByRole("listitem").count();
  const as1 = { ...opus, "X-Waypoint-Session": "cli-ov-1", "X-Waypoint-Machine": "ov-box" };
  const title = `Live question ${width}-${scheme}`;
  const r = await call("POST", "/requests", { kind: "question", title, questions: [{ question: "Yes?" }] }, as1);
  ok(r.ok(), `${tag} an agent asks while the page is open`);
  await region.getByText(title).first().waitFor({ timeout: 8000 });
  const after = await region.getByRole("listitem").count();
  ok(after === before + 1, `${tag} it appears without a reload (${before} → ${after})`);
  if (after !== before + 1) console.log((await region.innerText()).replace(/\n+/g, " | "));
  // Withdraw it so each run starts alike.
  const made = await r.json();
  await call("POST", `/requests/${made.id}/withdraw`, {}, as1);
  await ctx.close();
}

try {
  for (const [w, s] of looks([[1280, "light"], [1280, "dark"], [390, "light"], [390, "dark"]])) await run(w, s);
} finally {
  await browser.close();
}
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("agents overview: all good");
