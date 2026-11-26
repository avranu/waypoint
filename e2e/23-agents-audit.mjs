// The audit log on the Agents page (WP-D15), in a real browser at desktop
// and phone widths, light and dark: people's decisions made through the
// API (delegating, taking work back, reviewing an agent's text, an
// automation, an agent token) and ones other areas record (pinning and
// pausing agents, a released claim, the review note, a pull request
// approved and merged on a forge) read as sentences grouped by day; the
// kind chips and the person filter narrow it; the agent filter finds what
// concerns an agent; older entries load on demand; Export CSV downloads.
// Risky commands (WP-647): what wp record usage --from-transcripts sends is listed with its level,
// outcome and command, the level and outcome filters narrow it, a high one
// that ran notifies, and the notification's link opens the view.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();
// The suite's server database (run.sh keeps it next to the suites' output),
// for entries other areas of the page record.
const db = new DatabaseSync(path.join(path.dirname(out), "wp.db"));
db.exec("PRAGMA busy_timeout = 5000");
const seed =
  db.prepare(`INSERT INTO audit_log(user_id, actor, agent, about_agent, action, category,
  project_id, target_type, target_id, target, url, detail, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`);

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
    acceptDownloads: true,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d, headers) =>
    call(m, p, d, headers).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name, full = false) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: full,
    });

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const n = `${width}${scheme[0]}`.toUpperCase();
  const key = `AU${n}`;
  const proj = await api("POST", "/projects", { name: `Audit ${tag}`, key });
  const me = await api("GET", "/me");

  const parser = await api("POST", "/issues", {
    project: key,
    title: "Parser rewrite",
    agent: "claude",
  });
  const flaky = await api("POST", "/issues", {
    project: key,
    title: "Flaky test",
    agent: "haiku",
  });
  await api("PATCH", `/issues/${parser.key}`, { agent: "opus" });
  const docs = await api("POST", "/issues", {
    project: key,
    title: "Docs pass",
    agent: "any",
  });
  await api("PATCH", `/issues/${docs.key}`, { agent: "" });
  const { token } = await api("POST", "/me/tokens", {
    name: `audit bot ${tag}`,
    agent: "anthropic/claude-opus-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const own = await api(
    "POST",
    "/issues",
    { project: key, title: "Agent's own notes" },
    AG,
  );
  await api("POST", `/review/issue/${own.key}`);
  await api("POST", `/projects/${key}/automations`, {
    name: "Thank on done",
    rule: {
      trigger: { event: "status_changed", to: "done" },
      actions: [{ type: "comment", value: "Thanks" }],
    },
  });

  const now = Date.now();
  const rows = [
    [
      me.id,
      "",
      "",
      "claude",
      "queue.pin",
      "control",
      proj.id,
      "issue",
      null,
      parser.key,
      `/issue/${parser.key}`,
      "pinned #1",
      now - 50_000,
    ],
    [
      me.id,
      "",
      "",
      "haiku",
      "agent.pause",
      "control",
      proj.id,
      "",
      null,
      "haiku",
      "",
      "while the test suite is red",
      now - 40_000,
    ],
    [
      me.id,
      "",
      "",
      "anthropic/claude-code",
      "claim.release",
      "control",
      proj.id,
      "issue",
      null,
      parser.key,
      `/issue/${parser.key}`,
      "it was stuck",
      now - 30_000,
    ],
    [
      me.id,
      "",
      "",
      "",
      "settings.review_note",
      "settings",
      null,
      "",
      null,
      "",
      "",
      "**Automated review** by {models} · a bot, not a person.",
      now - 20_000,
    ],
    [
      null,
      "sam",
      "",
      "anthropic/claude-code",
      "pr.approved",
      "approval",
      proj.id,
      "pr",
      null,
      "robin/waypoint#41",
      "https://github.com/robin/waypoint/pull/41",
      "",
      now - 10_000,
    ],
    [
      null,
      "robin-mann",
      "",
      "anthropic/claude-code",
      "pr.merged",
      "approval",
      proj.id,
      "pr",
      null,
      "robin/waypoint#41",
      "https://github.com/robin/waypoint/pull/41",
      "into main",
      now - 5_000,
    ],
  ];
  for (const r of rows) seed.run(...r);
  for (let i = 0; i < 55; i++)
    seed.run(
      me.id,
      "",
      "",
      "claude",
      "queue.move",
      "control",
      proj.id,
      "issue",
      null,
      parser.key,
      `/issue/${parser.key}`,
      `moved to #${(i % 5) + 2}`,
      now - 2 * 86400_000 - i * 1000,
    );

  await page.goto(`${base}/agents?tab=audit&project=${key}`);
  const feed = page.getByRole("tabpanel");
  await feed.locator("li").first().waitFor();
  const text = await feed.innerText();
  for (const s of [
    `delegated ${parser.key} to`,
    `took ${docs.key} back from`,
    `wrote on ${own.key}`,
    "added the automation Thank on done",
    `pinned ${parser.key} in the agent queue`,
    "sam on the forge approved robin/waypoint#41",
    "merged robin/waypoint#41",
    "It was for claude.",
  ])
    ok(text.includes(s), `${tag} feed says "${s}"`);
  ok(!text.includes("queue.move"), `${tag} no raw action names`);
  await shot("feed");
  await feed.locator("li", { hasText: "paused" }).first().scrollIntoViewIfNeeded();
  await shot("feed-more");

  const before = await feed.locator("li").count();
  await feed.getByRole("button", { name: "Load older" }).click();
  await page.waitForFunction(
    (n) => document.querySelectorAll('[role="tabpanel"] li').length > n,
    before,
  );
  ok((await feed.locator("li").count()) > before, `${tag} older entries load`);

  const rowsText = () =>
    feed.locator("li").evaluateAll((els) => els.map((e) => e.innerText).join("\n"));
  const rowsWithout = (word) =>
    page.waitForFunction(
      (w) =>
        [...document.querySelectorAll('[role="tabpanel"] li')].every(
          (e) => !e.innerText.includes(w),
        ),
      word,
    );
  await page.getByRole("radio", { name: "Approvals" }).click();
  await rowsWithout("delegated");
  await feed.locator("li", { hasText: "merged" }).first().waitFor();
  const approvals = await rowsText();
  ok(
    approvals.includes("claude-opus-5-5 wrote on") && approvals.includes("merged"),
    `${tag} Approvals shows approvals only`,
  );
  await shot("approvals");
  await page.getByRole("radio", { name: "Delegation" }).click();
  await rowsWithout("merged");
  await feed.locator("li", { hasText: "delegated" }).first().waitFor();
  ok(
    (await rowsText()).includes("delegated"),
    `${tag} Delegation shows delegations only`,
  );
  await page.getByRole("radio", { name: "All" }).click();

  await page.goto(`${base}/agents?tab=audit&project=${key}&agent=haiku`);
  await feed.getByText("delegated").first().waitFor();
  const haiku = await rowsText();
  ok(
    haiku.includes(`delegated ${flaky.key} to`) &&
      !haiku.includes("opus") &&
      !haiku.includes("merged"),
    `${tag} agent filter keeps what concerns haiku`,
  );

  await page.goto(`${base}/agents?tab=audit&project=${key}`);
  await feed.locator("li").first().waitFor();
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("link", { name: "Export CSV" }).click(),
  ]);
  const csv = fs.readFileSync(await download.path(), "utf8");
  ok(
    csv.startsWith("time (UTC),person,") &&
      csv.includes("pr.merged") &&
      csv.includes(parser.key),
    `${tag} CSV export has the entries`,
  );

  const at0 = Date.now();
  const sess = `risky-${n}`;
  const risky = [
    { kind: "force_push", level: "high", label: "force push to main", command: "git push --force origin main", outcome: "ran", t: at0 - 60_000 },
    { kind: "delete", level: "medium", label: "recursive delete of src/old", command: "rm -rf src/old", outcome: "refused", source: "classifier", t: at0 - 120_000 },
    {
      kind: "cluster", level: "high", label: "kubectl delete", outcome: "error", t: at0 - 180_000,
      command: "cd deploy && ./render.sh --env prod --values values/prod.yaml --out /tmp/out.yaml && kubectl -n prod",
      segment: "kubectl -n prod delete pod web-1",
    },
    { kind: "discard", level: "medium", label: "git reset --hard", command: "git reset --hard origin/main", outcome: "unknown", t: at0 - 240_000 },
  ].map((x) => ({ s: sess, repo: "acme/web", model: "claude-opus-5-5", ...x }));
  const notes0 = db.prepare("SELECT count(*) AS c FROM notifications WHERE kind = 'risky' AND user_id = ?").get(me.id).c;
  const ing = await call("POST", "/usage/ingest", {
    machine: `m-${n}`,
    version: "test",
    chunks: [{ id: `risky-${n}`, risky }],
  });
  ok(ing.ok(), `${tag} risky commands ingest (${ing.status()})`);
  const notes = db.prepare("SELECT title, body FROM notifications WHERE kind = 'risky' AND user_id = ? ORDER BY id DESC").all(me.id);
  ok(
    notes.length === notes0 + 1 && notes[0].body.includes("force push to main in acme/web") && !notes[0].body.includes("reset"),
    `${tag} one notification, for the high ones that ran`,
  );

  await page.goto(`${base}/agents?tab=audit&view=risky`);
  const riskyList = page.locator('[role="tabpanel"] ul').first();
  await riskyList.locator("li").first().waitFor();
  ok(
    (await page.getByRole("tab", { name: "Risky commands" }).getAttribute("aria-selected")) === "true",
    `${tag} the notification's link opens Risky commands`,
  );
  const riskyText = () => riskyList.innerText();
  const all = await riskyText();
  ok(
    ["force push to main", "ran", "recursive delete of src/old", "refused by the classifier", "ran and failed", "…kubectl -n prod delete pod web-1", "no result", "acme/web", `m-${n}`].every((w) => all.includes(w)),
    `${tag} rows show label, outcome, command and where`,
  );
  ok(/\d+ high, \d+ medium/.test(await page.locator('[role="tabpanel"]').innerText()), `${tag} period counts`);
  await shot("risky");
  await page.getByRole("radio", { name: "High" }).click();
  await page.waitForFunction(() => ![...document.querySelectorAll('[role="tabpanel"] li')].some((e) => e.innerText.includes("src/old")));
  ok(!(await riskyText()).includes("Medium") && (await riskyText()).includes("force push to main"), `${tag} High keeps only high`);
  await page.getByRole("radio", { name: "All" }).click();
  await page.getByLabel("Outcome").click();
  await page.getByRole("option", { name: "Refused", exact: true }).click();
  await page.waitForFunction(() => ![...document.querySelectorAll('[role="tabpanel"] li')].some((e) => e.innerText.includes("force push")));
  ok((await riskyText()).includes("src/old"), `${tag} Refused keeps the refused`);
  await page.getByRole("tab", { name: "Decisions" }).click();
  await page.getByRole("radio", { name: "Approvals" }).waitFor();
  ok(!page.url().includes("view=risky"), `${tag} Decisions goes back`);

  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
]))
  await run(w, s);
await browser.close();
db.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
