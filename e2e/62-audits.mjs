// Audits (WP-348), in a real browser at desktop and phone widths: a person
// asks for one from Agents → Audits (a branch, the default wording and
// models), a (fake) runner with Cursor takes the job and answers with
// findings, and the audit's page shows them, worst first, with what to check.
// A person confirms one, rejects one with a note and asks Claude to fix the
// rest; a pull request's audit posts its findings on the pull request and
// shows on its sheet. A model's words never become markup. What landed in
// the last two weeks (WP-548), asked for from the command palette with
// criteria: the job carries the period, paths and cap, findings below the
// floor are left out, and "Edit and run again" starts from the same ask; a
// period with nothing in it ends with nothing to read. The project's
// "audit agents' pull requests" switch saves, at desktop and phone widths.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const SECRET = "e2e-audits-secret";
const REPO = "me/ledger";
const HEAD = "7777777bbbb";
const KEY = "AUDE";
const at = new Date(Date.now() - 3600e3).toISOString();

const pr = {
  number: 7,
  title: "Round the totals once",
  body: "Moves rounding to the end.",
  html_url: "",
  state: "open",
  merged: false,
  draft: false,
  mergeable: true,
  user: { login: "robin" },
  head: { ref: "round-once", sha: HEAD, repo: { full_name: REPO } },
  base: { ref: "main" },
  labels: [],
  created_at: at,
};

// What the fake GitHub holds and records: the comments Waypoint posts.
const posted = [];
const srv = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    const send = (code, body, type = "application/json") => {
      res.writeHead(code, { "Content-Type": type });
      res.end(body === undefined ? "" : typeof body === "string" ? body : JSON.stringify(body));
    };
    const m = req.url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)(?:\/(.*))?$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2] ?? "";
    if (rest === "") return send(200, { default_branch: "main", allow_merge_commit: true, allow_squash_merge: true, allow_rebase_merge: true });
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, []);
    if (rest === "pulls/7" || rest.startsWith("pulls/7?") || rest === "issues/7") {
      if ((req.headers.accept || "").includes("diff")) return send(200, "", "text/plain");
      return send(200, pr);
    }
    if (rest.startsWith("pulls/7/reviews") || rest.startsWith("pulls/7/comments")) return send(200, []);
    if (rest.startsWith("issues/7/comments")) {
      if (req.method === "POST") {
        const body = JSON.parse(raw).body;
        posted.push(body);
        return send(201, { id: 900 + posted.length, body, html_url: `${pr.html_url}#issuecomment-${900 + posted.length}`, user: { login: "waypoint-bot" } });
      }
      return send(200, []);
    }
    if (rest.startsWith("issues/7/reactions")) return send(200, []);
    if (/^commits\/[^/]+\/status/.test(rest)) return send(200, { statuses: [] });
    if (rest.startsWith("commits/") && rest.includes("check-runs")) return send(200, { check_runs: [] });
    if (rest.startsWith("statuses/")) return send(201, {});
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
pr.html_url = `${fake}/${REPO}/pull/7`;
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

const wait = async (cond, what, ms = 15000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await cond();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 150));
  }
};

// A rejection's evidence: long, with one unbroken path in it.
const EVIDENCE =
  "Read internal/core/some_long_directory/another_long_name/file_with_long_name_test.go:1234, which streams rows through a cursor and never holds more than one page; the export test there runs 5 million rows under a 512 MB limit and passes, so the memory claim does not hold on the current code path.";

// What the model answers: worst last here, so the page has to sort them.
const FINDINGS = [
  {
    title: "Rounding drops a cent on negative totals",
    path: "ledger/round.go",
    line: 12,
    severity: "medium",
    problem: "`math.Floor` rounds -0.005 down to -0.01, so refunds come out a cent short.",
    confirm: "Total a refund of -0.005 and compare with the bank's figure.",
  },
  {
    title: "The export reads every row into memory",
    path: "ledger/export.go",
    line: 40,
    severity: "low",
    problem: "A ledger of a few million rows makes the export run out of memory. <img src=x onerror=alert(1)>",
    confirm: "Export a ledger of 5 million rows with the memory limit at 512 MB.",
  },
  {
    title: "Anyone can close another person's ledger",
    path: "ledger/close.go",
    line: 88,
    severity: "high",
    problem: "Close checks that the ledger exists but not who owns it.",
    confirm: "Sign in as a second person and POST /ledgers/1/close.",
  },
];

// ---- set up: a project with a repo and a pull request, a runner with Cursor ----
let api, page, runner, prID;
// The one request a check means to be refused (WP-548's bad path).
let refusing = false;
const setupCtx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
{
  page = await signIn(setupCtx);
  page.on("pageerror", (e) => fail(`pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !(refusing && m.text().includes("status of 400"))) fail(`console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  await api("POST", "/projects", { name: "Ledger", key: KEY });
  const forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub audits", base_url: fake, token: "tok", webhook_secret: SECRET });
  await api("PUT", `/forges/${forge.id}/people`, { people: [{ login: "robin", user: "robin" }] });
  const added = await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false });
  await api("PATCH", `/projects/${KEY}/repos/${added.id}`, { auto_merge: false, review_wait_minutes: 0, quiet_minutes: 0 });
  const body = JSON.stringify({ action: "opened", pull_request: pr, sender: { login: "robin" }, repository: { full_name: REPO, owner: { type: "User" } } });
  const sig = "sha256=" + crypto.createHmac("sha256", SECRET).update(body).digest("hex");
  const r = await fetch(`${base}/api/v1/hooks/forge/${forge.id}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-GitHub-Event": "pull_request", "X-Hub-Signature-256": sig },
    body,
  });
  ok(r.ok, `webhook accepted (${r.status})`);
  prID = await wait(async () => (await api("GET", `/pulls?project=${KEY}`)).pulls?.[0]?.id, "the pull request");
  runner = await api("POST", "/runner/register", {
    name: "audit-box",
    hostname: "audit-box",
    os: "linux",
    arch: "amd64",
    version: "e2e",
    allowed_dirs: ["/work"],
    clis: [{ name: "cursor", signed_in: true }],
    features: ["audit", "audit-scope"],
  });
  ok(runner?.id > 0, "a runner with Cursor is online");
}

// The fake runner takes one audit job and answers it.
async function answer(findings, summary, extra = {}) {
  const job = await wait(async () => {
    const w = await api("POST", "/runner/poll", { runner: runner.id, free: 1, wait: 1 });
    return w?.job;
  }, "the audit job");
  ok(job.kind === "audit", `the runner gets an audit job (${job.kind})`);
  const input = typeof job.input === "string" ? JSON.parse(job.input) : job.input;
  const done = await api("POST", `/runner/jobs/${job.id}/finish`, {
    state: "done",
    exit_code: 0,
    result: extra.nothing
      ? { nothing: extra.nothing, answer: "", commits: [], change_commits: 0 }
      : { answer: JSON.stringify({ summary, findings }), structured: { summary, findings }, ...extra },
  });
  ok(done?.state === "done", `the job is finished (${JSON.stringify(done)})`);
  return input;
}

// ---- ask for one from Agents → Audits ----
let auditID;
{
  await page.goto(`${base}/agents?tab=audits&project=${KEY}`);
  await page.getByText(/^No audits (yet|match)$/).waitFor();
  ok(true, "the tab starts empty, and says so");
  await page.getByRole("button", { name: "New audit" }).click();
  const dlg = page.getByRole("dialog", { name: "New audit" });
  await dlg.getByRole("combobox", { name: "Project", exact: true }).click();
  await page.getByRole("option", { name: `Ledger (${KEY})`, exact: true }).click();
  const repoBox = dlg.getByRole("combobox", { name: "Repository", exact: true });
  await wait(async () => (await repoBox.innerText()).includes(REPO), "the repository list");
  ok(true, "its one repository is picked");
  await dlg.getByRole("radio", { name: "Branch" }).click();
  const words = dlg.getByLabel("What to look for");
  await wait(async () => (await words.inputValue()).startsWith("Find the defects"), "the default wording");
  ok(true, `the wording starts as the default: “${await words.inputValue()}”`);
  ok((await dlg.getByLabel("Model 1").inputValue()) === "grok-4.7-high", "Grok 4.7 high is the first model");
  ok(!(await dlg.getByText("Post the findings on the pull request").isVisible()), "no posting for a branch");
  ok(await dlg.getByText("Have Claude check and fix the findings").isVisible(), "Claude fixing is offered");
  await dlg.getByRole("button", { name: "Start audit" }).click();
  await page.waitForTimeout(300);
  ok(await dlg.isVisible(), "a branch is needed before it starts");
  await page.screenshot({ path: `${out}/dialog-1280.png` });
  await dlg.getByRole("textbox", { name: "Branch" }).fill("round-once");
  await words.fill("Look at how totals are rounded and who may close a ledger.");
  ok(await dlg.getByRole("button", { name: "Default" }).isVisible(), "an edited wording can go back to the default");
  await dlg.getByRole("button", { name: "Start audit" }).click();
  await page.waitForURL(/\/audits\/\d+$/);
  auditID = Number(page.url().split("/").pop());
  ok(auditID > 0, `it opens the new audit (#${auditID})`);
  await page.getByText(/Queued|Running/).first().waitFor();
  ok(true, "the audit is waiting for the runner, or running");
  await page.getByRole("button", { name: "Cancel", exact: true }).waitFor();
  ok(true, "a running audit can be cancelled");
  await page.screenshot({ path: `${out}/queued-1280.png` });

  const input = await answer(FINDINGS, "Read the rounding and the close handler. Three defects, one a hole.");
  ok(input.read_only === true && input.cli === "cursor" && input.model === "grok-4.7-high", `a read-only job for Grok through Cursor: ${JSON.stringify({ cli: input.cli, model: input.model, read_only: input.read_only })}`);
  ok(JSON.stringify(input).includes("who may close a ledger"), "the job carries the edited wording");
  await page.getByText("Done", { exact: true }).first().waitFor({ timeout: 15000 });
  await page.locator('li[id^="finding-"]').nth(2).waitFor();
  ok(true, "the page follows it to Done, live, with its findings");
  const items = await page.locator('li[id^="finding-"]').allInnerTexts();
  ok(items[0]?.includes("Anyone can close") && items[1]?.includes("Rounding drops") && items[2]?.includes("The export"), `worst first: ${items.map((t) => t.split("\n")[1]).join(" | ")}`);
  ok((await page.locator('li[id^="finding-"] img').count()) === 0, "a model's image tag never becomes an image");
  ok((await page.locator('li[id^="finding-"] [onerror]').count()) === 0, "nor runs anything");
  ok(await page.getByText("How to confirm it").first().isVisible(), "each finding says how to confirm it");
  ok(await page.getByRole("link", { name: "ledger/close.go:88" }).isVisible(), "findings link to the line on the forge");

  const item = (t) => page.locator('li[id^="finding-"]', { hasText: t });
  await item("Anyone can close").getByRole("button", { name: "Confirm" }).click();
  await item("Anyone can close").getByText("Confirmed", { exact: true }).waitFor();
  ok(true, "Confirm marks it confirmed");
  await item("The export").getByRole("button", { name: "Reject" }).click();
  await item("The export").getByPlaceholder("Why it isn’t a defect (optional)").fill("Exports stream since v2.");
  await item("The export").getByRole("button", { name: "Reject" }).click();
  await page.getByText("To do (2)").waitFor();
  ok(true, "a rejected finding leaves the to-do list");
  await page.getByRole("radio", { name: "All" }).click();
  await item("The export").getByText(/Rejected by .*Exports stream since v2/).waitFor();
  ok(true, "the rejection shows who and why");
  await page.getByRole("radio", { name: /To do/ }).click();

  // An agent's rejection carries evidence and the model and effort it ran as
  // (WP-1825). Only an agent's mark sets the decided_* columns, so write them:
  // this checks the page shows them; the rule an agent's mark is held to is
  // internal/core's tests (audit_rejection_test.go).
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const res = db
    .prepare(
      `UPDATE code_audit_findings SET evidence = ?, decided_model = 'claude-sonnet-5-5', decided_effort = 'high', decided_strength = ?
       WHERE audit_id = ? AND title LIKE 'The export%' AND state = 'rejected'`,
    )
    .run(EVIDENCE, "at_least: sonnet 5.5 (high) is as strong as sonnet", auditID);
  db.close();
  ok(res.changes === 1, "the rejected finding to show has evidence and the model it was rejected as written");
}

// ---- look at it, at each width ----
async function look(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const p = await signIn(ctx);
  p.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  p.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const sideways = async (what) => ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);

  await p.goto(`${base}/agents?tab=audits&project=${KEY}`);
  await p.getByRole("link", { name: /me\/ledger branch round-once/ }).first().waitFor();
  ok(await p.getByText("1 high").first().isVisible(), `${tag} the list shows the severities`);
  await p.waitForTimeout(300);
  await p.screenshot({ path: `${out}/list-${width}-${scheme}.png` });
  await sideways("the list");

  await p.goto(`${base}/audits/${auditID}`);
  await p.getByText("Anyone can close another person's ledger").waitFor();
  for (const t of ["What it was asked", "To look for", "grok-4.7-high", "Three defects, one a hole."])
    ok(await p.getByText(t).first().isVisible(), `${tag} the page shows “${t}”`);
  ok(await p.getByRole("button", { name: "Fix with Claude" }).isVisible(), `${tag} Fix with Claude is offered`);
  ok(await p.getByRole("button", { name: "Run again", exact: true }).isVisible(), `${tag} Run again is offered`);
  await p.waitForTimeout(300);
  await p.screenshot({ path: `${out}/audit-${width}-${scheme}.png`, fullPage: true });
  await sideways("the audit");

  await p.goto(`${base}/agents?tab=audits`);
  await p.getByRole("button", { name: "New audit" }).click();
  const dlg = p.getByRole("dialog", { name: "New audit" });
  await dlg.getByLabel("What to look for").waitFor();
  await p.waitForTimeout(300);
  await p.screenshot({ path: `${out}/dialog-${width}-${scheme}.png` });
  const box = await dlg.boundingBox();
  ok(box && box.width <= width, `${tag} the dialog fits (${box?.width})`);
  await sideways("the dialog");
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [1280, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await look(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}

// ---- a rejection's evidence and the model it ran as, at both widths ----
for (const width of [1280, 390]) {
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  try {
    const p = await signIn(ctx);
    await p.goto(`${base}/audits/${auditID}`);
    await p.getByRole("radio", { name: "All" }).click();
    const rej = p.locator('li[id^="finding-"]', { hasText: "The export" });
    await rej.getByText(/evidence: Read internal\/core/).waitFor();
    ok(await rej.getByText(EVIDENCE, { exact: false }).isVisible(), `${width} the rejection's evidence is shown`);
    ok(
      await rej.getByText("as claude-sonnet-5-5 (high), at least: sonnet 5.5 (high) is as strong as sonnet", { exact: false }).isVisible(),
      `${width} the rejection shows the model, effort and how it compared with the finder`,
    );
    ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${width} the rejected finding: no sideways scroll`);
    const clipped = await rej.getByText(/evidence: Read internal\/core/).evaluate((el) => el.scrollWidth > el.clientWidth);
    ok(!clipped, `${width} the long path in the evidence wraps, not clipped`);
    const li = await rej.boundingBox();
    ok(li && li.x >= 0 && li.x + li.width <= width, `${width} the finding fits the screen (${li?.x}+${li?.width})`);
    await rej.screenshot({ path: `${out}/rejected-evidence-${width}.png` });
  } catch (e) {
    fail(`${width} rejected evidence: ${e.message.split("\n")[0]}`);
  }
  await ctx.close();
}

// ---- Claude fixes the rest ----
{
  await page.goto(`${base}/audits/${auditID}`);
  await page.getByRole("button", { name: "Fix with Claude" }).click();
  await page.getByRole("link", { name: new RegExp(`Claude’s task ${KEY}-\\d+`) }).waitFor();
  const a = await api("GET", `/audits/${auditID}`);
  ok(!!a.fix_issue, `a task for Claude: ${a.fix_issue}`);
  const task = await api("GET", `/issues/${a.fix_issue}`);
  ok(task.title?.startsWith(`Check audit #${auditID}: 2 findings`), `the task holds the confirmed and the open finding: ${task.title}`);
  ok(task.description?.includes("Confirmed by ") && !task.description.includes("reads every row"), "the confirmed one says so; the rejected one is left out");
  ok(!(await page.getByRole("button", { name: "Fix with Claude" }).isVisible()), "Fix with Claude is gone once asked");
}

// ---- a pull request's audit: on its sheet, and posted on it ----
{
  const p = setupCtx.pages()[0];
  await p.setViewportSize({ width: 390, height: 900 });
  await p.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = p.getByRole("dialog", { name: "Pull request" });
  const section = sheet.getByRole("region", { name: "Audits" });
  await section.getByText("None yet.").waitFor();
  ok(true, "the sheet has an Audits section, empty");
  await section.getByRole("button", { name: "Audit" }).click();
  const dlg = p.getByRole("dialog", { name: "New audit" });
  await dlg.getByText(`${REPO}#7`).waitFor();
  ok(await dlg.getByText("Post the findings on the pull request").isVisible(), "posting is offered for a pull request");
  await p.screenshot({ path: `${out}/pr-dialog-390.png` });
  await dlg.getByRole("button", { name: "Start audit" }).click();
  await p.waitForURL(/\/audits\/\d+$/);
  const prAudit = Number(p.url().split("/").pop());
  await answer([FINDINGS[2]], "Read the pull request's change. One hole.");
  await wait(async () => (await api("GET", `/audits/${prAudit}`)).comment_url, "the comment on the pull request");
  ok(posted.length === 1 && posted[0].includes("Anyone can close another person") && posted[0].includes(`/audits/${prAudit}`), `the findings are posted, linked back: ${posted[0]?.slice(0, 200)}`);
  await p.getByRole("link", { name: /comment/i }).first().waitFor();
  ok(true, "the audit links to its comment");
  await p.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  await section.getByRole("link", { name: /Audit by grok-4.7-high/ }).waitFor();
  ok(await section.getByText("1 high").isVisible(), "the sheet lists it, with its severity");
  await section.scrollIntoViewIfNeeded();
  await p.waitForTimeout(300);
  await section.screenshot({ path: `${out}/pr-section-390.png` });
  ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), "390 the sheet: no sideways scroll");
}

// ---- what landed lately, with criteria, from the command palette (WP-548) ----
let recentID;
{
  const p = setupCtx.pages()[0];
  await p.setViewportSize({ width: 1280, height: 900 });
  await p.goto(`${base}/p/${KEY}/board`);
  await p.waitForLoadState("networkidle");
  await p.keyboard.press("Control+k");
  await p.getByPlaceholder(/Search issues/).fill("audit what landed");
  await p.screenshot({ path: `${out}/recent-palette-1280.png` });
  await p.getByRole("option", { name: /Audit what landed this week/ }).click();
  const dlg = p.getByRole("dialog", { name: "New audit" });
  await dlg.getByRole("radio", { name: "Last week" }).waitFor();
  ok((await dlg.getByRole("radio", { name: "Recent" }).getAttribute("aria-checked")) === "true", "the palette opens it on recent changes");
  ok((await dlg.getByRole("radio", { name: "Last week" }).getAttribute("aria-checked")) === "true", "over the last week");
  ok((await dlg.getByRole("combobox", { name: "Project", exact: true }).innerText()).includes(KEY), "in the project it was opened from");
  ok(await dlg.getByRole("link", { name: "Weekly Grok audit of what landed" }).isVisible(), "it points at the weekly automation");
  await dlg.getByRole("radio", { name: "Last 2 weeks" }).click();
  await dlg.getByRole("button", { name: "Security" }).click();
  await dlg.getByRole("button", { name: "Concurrency" }).click();
  ok((await dlg.getByRole("button", { name: "Security" }).getAttribute("aria-pressed")) === "true", "a focus area turns on");
  await dlg.getByLabel("Only these paths").fill("ledger\n../etc");
  await dlg.getByLabel("Leave out").fill("ledger/export.go");
  await dlg.getByRole("combobox", { name: "Report", exact: true }).click();
  await p.getByRole("option", { name: "High or worse", exact: true }).click();
  await dlg.getByLabel("Show at a time").fill("5");
  await p.screenshot({ path: `${out}/recent-dialog-1280.png` });
  refusing = true;
  await dlg.getByRole("button", { name: "Start audit" }).click();
  await dlg.getByText(/isn't a path inside the repository/).waitFor();
  refusing = false;
  ok(true, "a path that climbs out is refused, in the dialog");
  await dlg.getByLabel("Only these paths").fill("ledger");
  await dlg.getByRole("button", { name: "Start audit" }).click();
  await p.waitForURL(/\/audits\/\d+$/);
  recentID = Number(p.url().split("/").pop());
  const asked = Date.now() / 1000;
  const input = await answer(FINDINGS, "Read two weeks of the ledger.", { base: "c0ffee1234567890", diff_from: "beef001234567890", change_commits: 3 });
  ok(Math.abs(input.diff_since - (asked - 14 * 86400)) < 300 && !input.diff_from, `the job reads from two weeks back (${input.diff_since})`);
  ok(JSON.stringify(input.diff_paths) === '["ledger"]' && input.diff_exclude?.includes("ledger/export.go"), `with the paths: ${JSON.stringify([input.diff_paths, input.diff_exclude])}`);
  const schema = typeof input.json_schema === "string" ? input.json_schema : JSON.stringify(input.json_schema);
  ok(schema && !schema.includes('maxItems'), "the schema carries no number");
  for (const t of ["Look hardest at", "Security:", "Concurrency:", "Audit only these parts", "Leave these out", "Report only findings of high severity or worse"])
    ok(input.prompt.includes(t), `the prompt says “${t}”`);
  await p.getByText("Done", { exact: true }).first().waitFor({ timeout: 15000 });
  await p.locator('li[id^="finding-"]').first().waitFor();
  ok((await p.locator('li[id^="finding-"]').count()) === 1, "only the high finding is kept");
  ok(await p.getByText(/2 of the answer's 3 findings were below the audit's lowest severity, high, and are left out/).isVisible(), "the summary says two were left out");
  for (const t of ["the last 14 days", "Hardest at", "Security", "Concurrency", "Leaving out", "ledger/export.go", "High or worse, showing 5 findings at a time", "3 commits", "beef001234..c0ffee1234"])
    ok(await p.getByText(t, { exact: false }).first().isVisible(), `the page shows “${t}”`);
  await p.screenshot({ path: `${out}/recent-audit-1280.png`, fullPage: true });

  await p.getByRole("button", { name: "Edit and run again" }).click();
  const again = p.getByRole("dialog", { name: "New audit" });
  await again.getByRole("radio", { name: "Last 2 weeks" }).waitFor();
  ok((await again.getByRole("radio", { name: "Last 2 weeks" }).getAttribute("aria-checked")) === "true", "the copy keeps the period");
  ok((await again.getByRole("button", { name: "Security" }).getAttribute("aria-pressed")) === "true", "and the focus areas");
  ok((await again.getByLabel("Only these paths").inputValue()) === "ledger" && (await again.getByLabel("Show at a time").inputValue()) === "5", "and the paths and cap");
  await again.getByRole("radio", { name: "Since a date…" }).click();
  await again.getByRole("textbox", { name: "Span" }).fill("3d");
  await again.getByRole("button", { name: "Start audit" }).click();
  await p.waitForURL((u) => u.pathname !== `/audits/${recentID}` && /\/audits\/\d+$/.test(u.pathname));
  const second = Number(p.url().split("/").pop());
  await answer([], "", { nothing: "Nothing landed on main in the last 3 days." });
  await p.getByText("nothing to read").first().waitFor({ timeout: 15000 });
  ok(await p.getByText("Nothing landed on main in the last 3 days.").isVisible(), "a period with nothing in it says so");
  const a2 = await api("GET", `/audits/${second}`);
  ok(a2.since === "3d" && a2.focus?.includes("security") && a2.max_findings === 5 && !a2.model, `the copy is its own ask, and no model ran: ${JSON.stringify({ since: a2.since, focus: a2.focus, max: a2.max_findings, model: a2.model })}`);

  const twelve = Array.from({ length: 12 }, (_, n) => ({
    title: `Nit number ${n + 1}`,
    path: "ledger/nit.go",
    line: n + 1,
    severity: "low",
    problem: "A small thing.",
    confirm: "Read the line.",
  }));
  const more = await api("POST", "/audits", { target: "branch", project: KEY, repo: REPO, ref: "round-once", max_findings: 5 });
  ok(more?.id > 0, `an audit asked to show 5 starts (${JSON.stringify(more)?.slice(0, 120)})`);
  const sent = await answer(twelve, "A dozen small things.");
  const sentSchema = typeof sent.json_schema === "string" ? sent.json_schema : JSON.stringify(sent.json_schema);
  ok(!sentSchema.includes("maxItems") && !/\b(5|12)\b findings/.test(sent.prompt), "the model is told no number");
  await p.goto(`${base}/audits/${more.id}`);
  await p.locator('li[id^="finding-"]').first().waitFor();
  ok((await p.locator('li[id^="finding-"]').count()) === 5, "the page lists 5 findings");
  const showMore = p.getByRole("button", { name: "Show 7 more" });
  ok(await showMore.isVisible(), "and offers Show 7 more");
  await showMore.scrollIntoViewIfNeeded();
  await p.screenshot({ path: `${out}/show-5-of-12-${(p.viewportSize() || {}).width}.png` });
  await showMore.click();
  ok((await p.locator('li[id^="finding-"]').count()) === 12, "which shows all 12");
  ok(((await api("GET", `/audits/${more.id}`)).findings || []).length === 12, "and the API holds all 12");
  await p.setViewportSize({ width: 390, height: 900 });
  await p.goto(`${base}/audits/${more.id}`);
  await p.locator('li[id^="finding-"]').first().waitFor();
  ok(await p.getByRole("button", { name: "Show 7 more" }).isVisible(), "at 390px too");
  ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), "no sideways scroll at 390px");
  await p.getByRole("button", { name: "Show 7 more" }).scrollIntoViewIfNeeded();
  await p.screenshot({ path: `${out}/show-5-of-12-390.png` });
  await p.setViewportSize({ width: 1280, height: 900 });
}

// ---- the recent audit and its dialog at each width ----
for (const [width, scheme] of looks(
  [
    [1280, "light"],
    [390, "dark"],
  ],
  [0, 1],
)) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const p = await signIn(ctx);
  p.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  try {
    await p.goto(`${base}/audits/${recentID}`);
    await p.getByText("Hardest at").waitFor();
    await p.waitForTimeout(300);
    await p.screenshot({ path: `${out}/recent-audit-${width}-${scheme}.png`, fullPage: true });
    ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} the recent audit: no sideways scroll`);
    await p.getByRole("button", { name: "Edit and run again" }).click();
    const dlg = p.getByRole("dialog", { name: "New audit" });
    await dlg.getByText("Look hardest at", { exact: true }).waitFor();
    await p.waitForTimeout(300);
    await p.screenshot({ path: `${out}/recent-dialog-${width}-${scheme}.png` });
    const box = await dlg.boundingBox();
    ok(box && box.width <= width, `${tag} the dialog fits (${box?.width})`);
    ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} the recent dialog: no sideways scroll`);
  } catch (e) {
    fail(`${tag} recent: ${e.message.split("\n")[0]}`);
  }
  await ctx.close();
}

// ---- the project's "audit agents' pull requests" switch (WP-355), at each width ----
for (const width of [1280, 390]) {
  const p = setupCtx.pages()[0];
  await p.setViewportSize({ width, height: 900 });
  await p.goto(`${base}/p/${KEY}/settings?tab=general`);
  const sw = p.getByRole("switch", { name: "Audit agents’ pull requests" });
  await sw.scrollIntoViewIfNeeded();
  const before = (await sw.getAttribute("aria-checked")) === "true";
  ok(before === (width !== 1280), `${width} the switch shows the saved setting (${before})`);
  await sw.click();
  await p.getByRole("button", { name: "Save changes" }).click();
  await p.getByText("Settings saved").first().waitFor();
  const proj = await api("GET", `/projects/${KEY}`);
  ok(!!proj.settings?.audit_prs === !before, `${width} saving the switch sets audit_prs to ${!before}`);
  const row = sw.locator("xpath=..");
  await row.screenshot({ path: `${out}/setting-${width}.png` });
  const box = await row.boundingBox();
  ok(box && box.x >= 0 && box.x + box.width <= width, `${width} the setting's row fits (${box?.x}+${box?.width})`);
  ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${width} settings: no sideways scroll`);
}

// ---- the project's "owns the CI runners" switch (WP-589): saving it keeps the audit switch ----
for (const width of [1280, 390]) {
  const p = setupCtx.pages()[0];
  await p.setViewportSize({ width, height: 900 });
  await p.goto(`${base}/p/${KEY}/settings?tab=general`);
  const sw = p.getByRole("switch", { name: "Owns the CI runners" });
  await sw.scrollIntoViewIfNeeded();
  const before = (await sw.getAttribute("aria-checked")) === "true";
  ok(before === (width !== 1280), `${width} the runners switch shows the saved setting (${before})`);
  const audit = !!(await api("GET", `/projects/${KEY}`)).settings?.audit_prs;
  await sw.click();
  await p.getByRole("button", { name: "Save changes" }).click();
  await p.getByText("Settings saved").first().waitFor();
  const proj = await api("GET", `/projects/${KEY}`);
  ok(!!proj.settings?.ci_runners === !before && !!proj.settings?.audit_prs === audit, `${width} saving the switch sets ci_runners to ${!before}, audit_prs kept`);
  const row = sw.locator("xpath=..");
  await row.screenshot({ path: `${out}/runners-setting-${width}.png` });
  const box = await row.boundingBox();
  ok(box && box.x >= 0 && box.x + box.width <= width, `${width} the runners setting's row fits (${box?.x}+${box?.width})`);
  ok(!(await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${width} runners setting: no sideways scroll`);
}

await api("DELETE", `/runners/${runner.id}`).catch(() => {});
await setupCtx.close();
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("all ok");
