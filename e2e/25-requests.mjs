// Requests (WP-D16) in a real browser at desktop and phone widths, light and
// dark: an agent's session asks a question, a file change, a command, a
// refused tool call and a step only a person can do. The Inbox starts with
// "Waiting on you" (and the sidebar badge counts it); a request opens as a
// sheet and as /requests/WP:R-1 with its diff, command cells and decision
// buttons; deciding updates the lists at once, and a request filed while
// the page is open appears without a reload.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import crypto from "node:crypto";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KEY = "RQ";

const browser = await chromium.launch();
let agent; // headers of robin's agent token

const CLAUDE_MD = "# Rules\n\nBe nice.\nKeep answers short.\nAsk before deleting.\n";
// git's blob id, as wp computes it
const sha = (s) => crypto.createHash("sha1").update(`blob ${Buffer.byteLength(s)}\0`).update(s).digest("hex");

async function seed(api, call, w) {
  const projects = await api("GET", "/projects");
  if (projects.some((p) => p.key === KEY)) return;
  await api("POST", "/projects", { name: "Requests", key: KEY });
  const { token } = await api("POST", "/me/tokens", { name: "requests agent", agent: "anthropic/claude-code" });
  agent = { ...H, Authorization: `Bearer ${token}`, "X-Waypoint-Machine": "box" };
  // A forge and repo so a pull request is possible; the agent's session.
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const forge = db
    .prepare(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Forge', 'http://127.0.0.1:1', ?) RETURNING id`)
    .get(Date.now()).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
  db.prepare("INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', ?)").run(project, forge, Date.now());
  db.close();
  const hook = (event, extra = {}) =>
    call("POST", "/sessions/hook", { events: [{ event, session_id: "e2e-session", machine: "box", cwd: "/work/app", repo: "acme/app", branch: "feat/requests", ...extra }] }, agent);
  await hook("SessionStart", { model: "claude-opus-5" });
  await hook("UserPromptSubmit", { prompt: "Tidy up the config loader", title: "Config loader" });
  const as = { ...agent, "X-Waypoint-Session": "e2e-session" };
  const ask = (body) => call("POST", "/requests", body, as).then(async (r) => {
    if (!r.ok()) throw new Error(`ask ${body.title}: ${r.status()} ${await r.text()}`);
    return r.json();
  });
  await ask({
    kind: "question",
    title: "Which database should the new tests use?",
    body: "The old tests use **sqlite**, but the CI image has *postgres* too.\n\nSwitching costs about an hour.",
    questions: [
      { question: "Which database should the new tests use?", choices: [
        { label: "sqlite", description: "Fast, nothing to set up." },
        { label: "postgres", description: "Matches production." },
      ] },
    ],
  });
  await ask({
    kind: "change",
    title: "Tighten the rules in CLAUDE.md",
    body: "A hook protects CLAUDE.md, so I can't edit it myself.",
    message: "docs: tighten the rules",
    files: [
      { path: "CLAUDE.md", base_sha: sha(CLAUDE_MD), base: CLAUDE_MD, content: CLAUDE_MD.replace("Keep answers short.", "Keep answers short and plain.\nCite files as path:line.") },
      { path: "docs/NEW.md", content: "# New\n\nA new page.\n" },
    ],
  });
  await ask({
    kind: "command",
    title: "Run the migration",
    body: "It needs your database login.",
    cells: [{ command: "make migrate", dir: "/work/app/db" }, { command: "make seed" }],
  });
  await ask({ kind: "permission", title: "Allow Bash: wp pr log 12?", tool: "Bash", tool_input: { command: "wp pr log 12", description: "the failed check's log" }, reason: "Reading CI logs is outside what auto mode allows." });
  await ask({ kind: "action", title: "Set the DEPLOY_SSH_KEY secret", body: "In the repo settings under **Secrets**." });
  // Two earlier runs of the migration's first cell, so the notebook has history.
  const d2 = new DatabaseSync(`${process.env.DIR}/wp.db`);
  d2.exec("PRAGMA busy_timeout = 5000");
  const cell = d2.prepare("SELECT c.id FROM request_cells c JOIN requests r ON r.id = c.request_id WHERE r.kind = 'command' AND c.n = 1").get().id;
  const now = Date.now();
  d2.prepare("INSERT INTO request_cell_runs(cell_id, command, state, exit_code, output, error, run_by, created_at, finished_at) VALUES (?, 'make migrate', 'failed', 2, ?, '', 1, ?, ?)")
    .run(cell, "applying 0042_orders…\nERROR: relation \"orders\" does not exist\nmake: *** [migrate] Error 2\n", now - 600_000, now - 598_800);
  d2.prepare("INSERT INTO request_cell_runs(cell_id, command, state, exit_code, output, error, run_by, created_at, finished_at) VALUES (?, 'make migrate', 'done', 0, ?, '', 1, ?, ?)")
    .run(cell, "applying 0041_users…\napplying 0042_orders…\ndone: 2 migrations in 1.4s\n", now - 120_000, now - 118_600);
  d2.close();
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
    await page.waitForTimeout(300);
    const tall = await page.evaluate(() => {
      const panes = [...document.querySelectorAll(".overflow-y-auto")];
      return Math.max(...panes.map((e) => e.scrollHeight));
    });
    await page.setViewportSize({ width, height: Math.min(Math.max(900, tall + 120), 5000) });
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

  // Each run starts from the same five open requests (the earlier run's are reset).
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  db.exec("UPDATE requests SET state = 'open', decision = '', answer = '', note = '', decided_by = NULL, decided_at = NULL, delivered_at = NULL, snoozed_until = NULL, error = '', result = '{}', apply_path = '' WHERE number <= 5");
  db.exec("DELETE FROM requests WHERE number > 5");
  db.exec("DELETE FROM request_cell_runs WHERE exit_code IS NULL");
  // The second cell has a run queued for a runner, which says what it waits for (WP-503).
  db.exec("DELETE FROM runner_jobs WHERE source = 'request_cell' AND title = 'make seed'");
  const seedCell = db.prepare("SELECT c.id FROM request_cells c JOIN requests r ON r.id = c.request_id WHERE r.kind = 'command' AND c.n = 2").get().id;
  const job = db.prepare("INSERT INTO runner_jobs(owner_id, created_by, kind, title, input, timeout, source, source_id, created_at) VALUES (1, 1, 'command', 'make seed', '{}', 600, 'request_cell', ?, ?)").run(seedCell, Date.now());
  db.prepare("INSERT INTO request_cell_runs(cell_id, command, state, run_by, job_id, created_at) VALUES (?, 'make seed', 'queued', 1, ?, ?)").run(seedCell, job.lastInsertRowid, Date.now());
  db.close();

  const waiting = await api("GET", "/waiting");
  ok(waiting.requests.length === 5 && waiting.total >= 5, `${tag} API: ${waiting.requests.length} requests wait on robin`);

  // ---- the Inbox ----
  await page.goto(base + "/inbox");
  const section = page.getByRole("region", { name: "Waiting on you" });
  await section.waitFor();
  ok((await section.getByRole("button").count()) === 5, `${tag} five requests under Waiting on you`);
  ok(await section.getByText("Which database should the new tests use?").isVisible(), `${tag} the question is listed`);
  ok(await section.getByText(/File change · claude-code · box · RQ/).isVisible(), `${tag} each row says who, where and which project`);
  const badge = await page.locator("aside a[href='/inbox'], nav a[href='/inbox']").filter({ visible: true }).first().innerText();
  // The badge is the waiting requests plus the unread notifications that still
  // matter (WP-1141); other suites leave notifications behind, so compare with
  // what the API says, not with 5.
  const unread = (await api("GET", "/inbox")).summary.live_unread;
  const expected = waiting.total + unread;
  ok(
    badge.match(/\d+/)?.[0] === String(expected) && waiting.total >= 5,
    `${tag} the Inbox badge counts them (${JSON.stringify(badge)}; ${waiting.total} waiting + ${unread} unread)`,
  );
  await shot("inbox");
  await overflow("inbox");

  // ---- a question, in the sheet ----
  await section.getByText("Which database should the new tests use?").click();
  const sheet = page.getByRole("dialog", { name: /Request RQ:R-1/ });
  await sheet.waitFor();
  // The sheet opens before the request has loaded: wait for its title.
  ok(
    await sheet
      .getByRole("heading", { name: "Which database should the new tests use?" })
      .waitFor()
      .then(() => true, () => false),
    `${tag} the sheet shows the title`,
  );
  ok(await sheet.getByText("anthropic/claude-code").isVisible() && (await sheet.getByText(/box · acme\/app/).isVisible()), `${tag} the agent and its session show`);
  ok(await sheet.locator("strong", { hasText: "sqlite" }).isVisible(), `${tag} the ask is rendered as markdown`);
  const send = sheet.getByRole("button", { name: "Send answer" });
  ok(await send.isDisabled(), `${tag} nothing to send until a choice is made`);
  await sheet.getByRole("radio", { name: /postgres/ }).click();
  ok((await sheet.getByRole("radio", { name: /postgres/ }).getAttribute("aria-checked")) === "true", `${tag} a choice can be picked`);
  await shot("question");
  await send.click();
  await sheet.getByText(/answered/i).first().waitFor();
  const got = await api("GET", "/requests/RQ:R-1");
  ok(got.state === "answered" && got.answer === "postgres" && got.decided_by?.username === "robin", `${tag} the answer reached the server: ${got.state} ${got.answer}`);
  await page.keyboard.press("Escape");
  await sheet.waitFor({ state: "hidden" });
  // Request rows are buttons; other suites' design notes and unreviewed text add link rows.
  await page.waitForFunction(() => document.querySelectorAll("section[aria-label='Waiting on you'] li button").length === 4);
  ok(true, `${tag} the answered request left the waiting list at once`);

  // ---- the file change, as a page ----
  await page.goto(base + "/requests/RQ:R-2");
  await page.getByRole("heading", { name: "Tighten the rules in CLAUDE.md" }).waitFor();
  ok(await page.getByText("2 files").isVisible(), `${tag} the change counts its files`);
  ok(await page.locator("tr", { hasText: "Keep answers short and plain." }).first().isVisible(), `${tag} the diff shows an added line`);
  ok(await page.getByRole("button", { name: /docs\/NEW.md/ }).isVisible(), `${tag} a new file is listed`);
  await page.getByRole("button", { name: /docs\/NEW.md/ }).click(); // folds it
  ok((await page.getByText("A new page.").count()) === 0, `${tag} a file's diff folds`);
  const runner = page.getByRole("radio", { name: /Apply on box/ });
  ok(await runner.isDisabled(), `${tag} the runner choice is off with no runner online`);
  ok((await page.getByText("No runner online on box; connect one or open a PR instead.").count()) > 0, `${tag} it says why`);
  ok((await page.getByRole("radio", { name: /Open a pull request/ }).getAttribute("aria-checked")) === "true", `${tag} a pull request is the default`);
  await tallShot("change");
  await overflow("change");
  // The forge isn't reachable here: approving shows how it fails, and can be retried.
  await page.getByRole("button", { name: "Approve and apply" }).click();
  await page.getByText("Applying failed").waitFor();
  ok(await page.getByText(/forge has no API token/i).isVisible(), `${tag} a failed apply says why`);
  ok(await page.getByRole("button", { name: "Try again" }).isVisible(), `${tag} and offers to try again`);
  await shot("change-failed");
  // An approved change that altered nothing doesn't read as applied (WP-948).
  {
    const noop = new DatabaseSync(`${process.env.DIR}/wp.db`);
    noop.exec("PRAGMA busy_timeout = 5000");
    noop.prepare(`UPDATE requests SET state = 'applied', apply_path = '', error = '', result = ? WHERE number = 2`).run(
      JSON.stringify({ no_change: true, reason: "every file already has the requested content, so the change alters nothing." }),
    );
    noop.close();
  }
  await page.reload();
  await page.getByText("Approved, but it changed nothing").waitFor();
  ok(await page.getByText(/no commit or pull request was made/).isVisible(), `${tag} a no-op change says nothing was made`);
  ok((await page.getByText(/^Applied/).count()) === 0 && (await page.getByText("Changed nothing", { exact: true }).isVisible()), `${tag} and its state doesn't say applied`);
  await shot("change-no-op");
  await overflow("change no-op");

  // ---- the command notebook ----
  await page.goto(base + "/requests/RQ:R-3");
  await page.getByRole("heading", { name: "Run the migration" }).waitFor();
  const cell = page.getByRole("textbox", { name: "Command 1" });
  ok((await cell.inputValue()) === "make migrate", `${tag} the command is editable text`);
  ok(await page.getByText("exit 0").first().isVisible(), `${tag} the latest run shows its exit code`);
  ok(await page.getByText(/done: 2 migrations/).isVisible(), `${tag} and its output`);
  ok(await page.getByText("Waiting for the runner…").isVisible(), `${tag} a queued run says it waits`);
  ok(await page.getByText(/^(No runner can take this job\.|Waiting for .+ to come online\.)$/).isVisible(), `${tag} and what for`);
  await page.getByRole("button", { name: /earlier run/ }).click();
  await page.getByRole("button", { name: /exit 2/ }).click();
  ok(await page.getByText(/relation "orders" does not exist/).isVisible(), `${tag} the history keeps the failed run's output`);
  await cell.fill("make migrate ARGS=--dry-run");
  ok(await page.getByRole("button", { name: "Reset to the agent’s command" }).isVisible(), `${tag} an edited command can be reset`);
  await page.getByRole("button", { name: "Run again" }).first().click();
  // No runner is online here: the run is refused, with the way out.
  await page.getByText(/No runner online on box/).first().waitFor();
  ok(true, `${tag} running with no runner says so`);
  await tallShot("command");
  await overflow("command");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByText(/marked it done/).waitFor();
  ok((await api("GET", "/requests/RQ:R-3")).state === "done", `${tag} Done decides a command request`);
  // Decided, the editor is read-only: a reload shows the command that ran, and says the agent's.
  await page.reload();
  await page.getByText(/marked it done/).waitFor();
  ok((await cell.inputValue()) === "make migrate ARGS=--dry-run", `${tag} a reload of a decided request shows the command that ran`);
  ok((await page.getByTitle("The agent’s command").textContent()) === "agent’s: make migrate", `${tag} and the agent's beside it`);
  ok((await page.getByRole("button", { name: "Reset to the agent’s command" }).count()) === 0, `${tag} with no reset`);
  // A run that exits non-zero sends it back to the agent (WP-170); it can still be run again.
  const sentBack = new DatabaseSync(`${process.env.DIR}/wp.db`);
  sentBack.exec("PRAGMA busy_timeout = 5000");
  sentBack.prepare("UPDATE requests SET state = 'failed', decision = 'ran', note = '', error = 'exit 2', decided_by = 1, decided_at = ?, delivered_at = NULL WHERE number = 3").run(Date.now());
  sentBack.close();
  await page.reload();
  await page.getByText(/ran it and it failed \(exit 2\), so it went back to the agent/).waitFor();
  ok((await cell.inputValue()) === "make migrate ARGS=--dry-run", `${tag} a reload keeps the command the person ran`);
  await page.getByRole("button", { name: "Reset to the agent’s command" }).click();
  ok((await cell.inputValue()) === "make migrate", `${tag} and it can be reset to the agent's command`);
  ok(await page.getByRole("button", { name: "Run again" }).first().isVisible(), `${tag} a command sent back can be run again`);
  ok((await page.getByRole("button", { name: "Done" }).count()) === 0, `${tag} and isn't decided twice`);
  ok((await page.getByText("Applying failed").count()) === 0, `${tag} and doesn't read as a failed change`);
  await shot("command-sent-back");
  await overflow("command sent back");

  // ---- a refused tool call ----
  await page.goto(base + "/requests/RQ:R-4");
  await page.getByRole("heading", { name: /Allow Bash: wp pr log 12/ }).waitFor();
  ok(await page.locator("pre", { hasText: '"command": "wp pr log 12"' }).isVisible(), `${tag} the tool call's input shows`);
  ok(await page.getByText("Reading CI logs is outside what auto mode allows.").isVisible(), `${tag} and the reason`);
  // Allow from now on (WP-233): no rule until the person picks one, the exact rule, one prefix rule, or another.
  const allow = page.getByRole("radiogroup", { name: "Allow from now on" });
  ok((await allow.getByRole("radio", { name: /No rule/ }).getAttribute("aria-checked")) === "true", `${tag} no rule is picked at first`);
  ok(await page.getByRole("button", { name: "Approve", exact: true }).isVisible(), `${tag} so the button is a plain approval`);
  await allow.getByRole("radio", { name: /Bash\(wp pr log 12\)/ }).click();
  ok(await page.getByRole("button", { name: "Approve and add rule" }).isVisible(), `${tag} picking a rule makes the button add it`);
  ok(await allow.getByRole("radio", { name: /Bash\(wp pr log \*\)/ }).isVisible(), `${tag} and one prefix rule is offered`);
  await allow.getByRole("radio", { name: /Bash\(wp pr log \*\)/ }).click();
  await shot("permission-rule");
  await overflow("permission rule");
  await allow.getByRole("radio", { name: /Another rule/ }).click();
  ok(await page.getByRole("button", { name: "Approve and add rule" }).isDisabled(), `${tag} another rule needs one typed`);
  await allow.getByRole("radio", { name: /No rule/ }).click();
  ok(await page.getByRole("button", { name: "Approve", exact: true }).isVisible(), `${tag} no rule is a plain approval`);
  await page.getByRole("button", { name: "Add a note for the agent" }).click();
  await page.getByLabel("Note for the agent").fill("Only this pull request.");
  await shot("permission");
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await page.getByText(/No rule was added/).waitFor();
  const perm = await api("GET", "/requests/RQ:R-4");
  ok(perm.state === "approved" && perm.note === "Only this pull request." && !perm.result?.rule, `${tag} approved without a rule, with a note`);

  // ---- snooze ----
  await page.goto(base + "/requests/RQ:R-5");
  await page.getByRole("heading", { name: "Set the DEPLOY_SSH_KEY secret" }).waitFor();
  await page.getByRole("button", { name: "Snooze" }).click();
  await page.getByRole("menuitem", { name: "For an hour" }).click();
  await page.getByText(/Snoozed until/).waitFor();
  const w2 = await api("GET", "/waiting");
  ok(w2.requests.length === 0, `${tag} a snoozed request no longer waits on you`);

  // ---- a dismissal without a note isn't told to the agent (WP-197) ----
  await page.getByRole("button", { name: "Dismiss", exact: true }).click();
  await page.getByText(/the agent isn’t told, since there was no note for it/).waitFor();
  const quiet = await api("GET", "/requests/RQ:R-5");
  ok(quiet.state === "dismissed" && quiet.quiet === true && !!quiet.delivered_at, `${tag} a plain dismissal is quiet: ${quiet.state} ${quiet.quiet}`);
  await shot("dismissed-quiet");
  await overflow("dismissed quiet");

  // ---- live: a request filed while the Inbox is open appears without a reload ----
  await page.goto(base + "/inbox");
  await page.waitForTimeout(500);
  const filed = await call("POST", "/requests", { kind: "question", title: "Squash the migrations?", questions: [{ question: "Squash the migrations?" }] }, { ...agent, "X-Waypoint-Session": "e2e-session" });
  ok(filed.ok(), `${tag} the agent files another request`);
  await page.getByRole("region", { name: "Waiting on you" }).getByText("Squash the migrations?").waitFor();
  ok(true, `${tag} it appears in the open Inbox without a reload`);
  const one = page.getByRole("region", { name: "Waiting on you" });
  await one.getByText("Squash the migrations?").click();
  const sh2 = page.getByRole("dialog", { name: /Request RQ:R-6/ });
  await sh2.getByLabel("Your answer").fill("Yes, squash them: nothing reads the old ones.");
  await sh2.getByRole("button", { name: "Send answer" }).click();
  await sh2.getByText(/answered/i).first().waitFor();
  ok(true, `${tag} a free-text answer is sent`);

  // ---- age; a refused call is logged, not asked (WP-232); a terminal question the agent moved on from (WP-66) ----
  const hook = (event, extra = {}) =>
    call("POST", "/sessions/hook", { events: [{ event, session_id: "e2e-session", machine: "box", cwd: "/work/app", repo: "acme/app", branch: "feat/requests", ...extra }] }, agent);
  const old = await call("POST", "/requests", { kind: "action", title: "Rotate the API key" }, { ...agent, "X-Waypoint-Session": "e2e-session" });
  ok(old.ok(), `${tag} the agent files a step for a person`);
  const d3 = new DatabaseSync(`${process.env.DIR}/wp.db`);
  d3.exec("PRAGMA busy_timeout = 5000");
  d3.prepare("UPDATE requests SET created_at = created_at - ? WHERE number = 7 AND project_id = (SELECT id FROM projects WHERE key = ?)").run(3 * 86400_000, KEY);
  d3.close();
  await hook("PermissionDenied", {
    tool: "Bash",
    tool_use_id: `tu-${tag}`,
    tool_input: { command: "curl -X DELETE https://api.example.com/cache" },
    reason: "Network writes are outside what auto mode allows.",
  });
  await hook("PreToolUse", {
    tool: "AskUserQuestion",
    tool_use_id: `tq-${tag}`,
    tool_input: { questions: [{ question: "Which cache should I clear?", header: "Cache", multiSelect: false, options: [{ label: "CDN", description: "" }, { label: "API", description: "" }] }] },
  });
  await page.goto(base + "/inbox");
  const ws = page.getByRole("region", { name: "Waiting on you" });
  await ws.getByText("Rotate the API key").waitFor();
  await ws.getByText("Which cache should I clear?").waitFor();
  ok((await ws.locator("li button").count()) === 2, `${tag} the old step and the terminal question wait on you`);
  ok((await ws.locator("li button", { hasText: "curl -X DELETE" }).count()) === 0, `${tag} the refused call doesn't`);
  const oldRow = ws.locator("li button", { hasText: "Rotate the API key" });
  ok((await oldRow.locator("[data-old]").innerText()) === "3 days ago", `${tag} an old request says how old, and stands out`);
  ok((await ws.locator("li button", { hasText: "Which cache" }).locator("[data-old]").count()) === 0, `${tag} a fresh one doesn't`);
  await shot("waiting-age");
  await overflow("waiting-age");
  // The question shows what was refused just before it.
  await page.goto(base + "/requests/RQ:R-8");
  const before = page.getByRole("region", { name: "Refused just before this was asked" });
  await before.waitFor();
  ok(await before.getByText("curl -X DELETE https://api.example.com/cache").isVisible(), `${tag} a request shows the call refused just before it`);
  ok(await before.getByText("Network writes are outside what auto mode allows.").isVisible(), `${tag} and why`);
  await shot("refused-before");
  await overflow("refused-before");
  // Agents → Audit log → Refused calls lists it. A call refused twice in a row
  // offers the allow rules that would have let it through (WP-233).
  for (const n of [1, 2])
    await hook("PermissionDenied", {
      tool: "Bash",
      tool_use_id: `tr${n}-${tag}`,
      tool_input: { command: "wp pr checks 12" },
      reason: "Reading CI is outside what auto mode allows.",
    });
  await page.goto(`${base}/agents?tab=audit&view=refused&project=${KEY}`);
  await page.getByRole("tab", { name: "Refused calls", selected: true }).waitFor();
  await page.getByText("curl -X DELETE https://api.example.com/cache").first().waitFor();
  ok(true, `${tag} Audit log → Refused calls lists the refused call`);
  ok(await page.getByText(/^Last \d+ days: \d+ refused calls?( \(\d+ different\))? in 1 session/).isVisible(), `${tag} with the period's counts`);
  const rulesBox = page.getByRole("region", { name: "Allow rules" });
  await rulesBox.getByText("Bash(wp pr checks 12)").waitFor(); // one command: its prefix rule covers nothing more
  ok(await rulesBox.getByText(/^\d+ refused calls in 1 session/).first().isVisible(), `${tag} the rules that would have let repeated refusals through are ranked, with what they cover`);
  await shot("refused-calls");
  await overflow("refused-calls");
  const row = page.locator("li", { has: page.locator("pre", { hasText: "wp pr checks 12" }) }).first();
  ok(await row.getByText(/^refused \d+ times/).isVisible(), `${tag} repeats are one row`);
  await row.getByRole("button", { name: "Allow from now on…" }).click();
  const pick = row.getByRole("radiogroup", { name: "Allow from now on" });
  ok((await pick.getByRole("radio", { name: /Bash\(wp pr checks 12\)/ }).getAttribute("aria-checked")) === "true", `${tag} a row's narrowest rule is picked first`);
  ok((await pick.getByRole("radio", { name: /No rule/ }).count()) === 0, `${tag} with no "No rule": nothing waits on this pick`);
  await shot("refused-rule");
  await overflow("refused-rule");
  // This forge isn't reachable, so the pull request fails and the row says why.
  await row.getByRole("button", { name: "Add rule" }).click();
  await row.getByText(/wasn’t added|wasn't added/).waitFor();
  ok(true, `${tag} a rule whose pull request couldn't be opened says why`);
  const after = await api("GET", `/refusals?project=${KEY}&limit=1`);
  ok(after.refusals[0].command === "wp pr checks 12" && !after.refusals[0].rule && after.refusals[0].rules?.length > 0, `${tag} and still offers its rules`);
  // A new prompt: the agent moved on, so the terminal question leaves the list by itself.
  await page.goto(base + "/inbox");
  await ws.getByText("Which cache should I clear?").waitFor();
  await page.waitForTimeout(20);
  await hook("UserPromptSubmit", { prompt: "Clear the cache another way" });
  await page.waitForFunction(() => document.querySelectorAll("section[aria-label='Waiting on you'] li button").length === 1);
  ok(true, `${tag} the terminal question left Waiting on you once a new prompt came`);
  ok((await api("GET", "/requests/RQ:R-7")).state === "open", `${tag} the agent's own step still waits`);
  await page.goto(base + "/requests/RQ:R-8");
  await page.getByRole("heading", { name: /Which cache should I clear/ }).waitFor();
  ok(await page.getByText("Expired", { exact: true }).isVisible(), `${tag} it reads as expired`);
  ok(await page.getByText(/no longer waits on you\. The agent moved on: a new prompt started a later turn/).isVisible(), `${tag} and says why`);
  ok((await page.getByRole("button", { name: "Send answer", exact: true }).count()) === 0, `${tag} and can't be answered any more`);
  await shot("expired");
  await overflow("expired");
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [1280, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
// The queued "make seed" run stays in no runner's queue: a later suite's fake
// runner (62-audits) would claim it first, as commands come before audits.
{
  const tidy = new DatabaseSync(`${process.env.DIR}/wp.db`);
  tidy.exec("PRAGMA busy_timeout = 5000");
  tidy.exec("DELETE FROM request_cell_runs WHERE job_id IN (SELECT id FROM runner_jobs WHERE source = 'request_cell' AND title = 'make seed')");
  tidy.exec("DELETE FROM runner_jobs WHERE source = 'request_cell' AND title = 'make seed'");
  tidy.close();
}
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
