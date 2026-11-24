// Objectives (WP-927), in a real browser at desktop and phone widths. Waypoint
// reads every round itself: the commands run on the confirming person's runner
// and only that reading decides. This suite is that runner (feature
// "objective", owned by robin): it answers the reading job and the judge's
// audit, and the page is asserted at each stage. The worker's own readings are
// posted once, as a deliberate forgery, to show they decide nothing.
//   A person sets an objective from the task's menu; the banner shows it being
//   read from where it starts; the runner answers; the round waits to be
//   handed out; the worker finishes it; the reading is not met and the round
//   waits between rounds (and can be started now); a round that changed what
//   the check runs needs the person (accept, move on, end); the objectives
//   page lists it first; accepting it meets the objective and closes the task.
//   A judged objective shows its findings, then is met when the reviewer finds
//   nothing. An agent's proposal waits for a person to confirm it; ending an
//   objective asks first. The inbox and notification settings know objectives.
// Run after 01-signup-flow (user robin, project WAYP).
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
const REPO = "me/ledger-objectives";
const HEAD1 = "a".repeat(40);
const HEAD2 = "b".repeat(40);
const HEAD3 = "c".repeat(40);
const wait = async (cond, what, ms = 20000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await cond();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 150));
  }
};

// A fake GitHub that knows the repository and its default branch.
const srv = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => {
    const send = (code, body) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body ?? {}));
    };
    const m = req.url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)(?:\/(.*))?$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2] ?? "";
    if (rest === "")
      return send(200, {
        default_branch: "main",
        allow_merge_commit: true,
        allow_squash_merge: true,
        allow_rebase_merge: true,
      });
    if (rest.startsWith("hooks"))
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    if (rest.startsWith("pulls?")) return send(200, []);
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

// ---- set up once: a repository on WAYP and a runner of robin's that reads objectives ----
// The runner is online before any objective is set: a reading is asked for the
// moment an objective goes into effect.
let runner, agentHdr;
{
  const ctx = await browser.newContext();
  const page = await signIn(ctx);
  const api = async (m, p, d) => {
    const r = await page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers: H,
      data: d,
    });
    if (!r.ok()) fail(`setup ${m} ${p}: ${r.status()} ${await r.text()}`);
    return r.status() === 204 ? null : r.json();
  };
  const forge = await api("POST", "/forges", {
    kind: "github",
    name: "Fake GitHub objectives",
    base_url: fake,
    token: "tok",
    webhook_secret: "e2e-obj-secret",
  });
  await api("PUT", `/forges/${forge.id}/people`, {
    people: [{ login: "robin", user: "robin" }],
  });
  await api("POST", `/projects/WAYP/repos`, {
    forge_id: forge.id,
    repo: REPO,
    sync_issues: false,
  });
  runner = await api("POST", "/runner/register", {
    name: "objective-box",
    hostname: "objective-box",
    os: "linux",
    arch: "amd64",
    version: "e2e",
    allowed_dirs: ["/work"],
    clis: [{ name: "cursor", signed_in: true }],
    features: ["objective", "audit", "audit-scope"],
  });
  ok(runner?.id > 0, "a runner of robin's that reads objectives is online");
  const tok = await api("POST", "/me/tokens", {
    name: "opus objectives",
    agent: "anthropic/claude-opus-5-5",
  });
  agentHdr = { Authorization: `Bearer ${tok.token}` };
  await ctx.close();
}

const poll = async () => {
  const r = await fetch(base + "/api/v1/runner/poll", {
    method: "POST",
    headers: { ...H, "Content-Type": "application/json", Cookie: runnerCookie },
    body: JSON.stringify({ runner: runner.id, free: 1, wait: 1 }),
  });
  return (await r.json())?.job;
};
let runnerCookie = "";

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await signIn(ctx);
  runnerCookie = (await ctx.cookies())
    .map((c) => `${c.name}=${c.value}`)
    .join("; ");
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`),
  );
  const shot = (name, fullPage = true) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage,
    });
  const overflow = async (what) =>
    ok(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
      `${tag} ${what} has no sideways scroll`,
    );
  const json = async (r) => {
    if (!r.ok()) fail(`${tag} ${r.status()} ${r.url()}: ${await r.text()}`);
    return r.json();
  };
  const web = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers: H,
      data: d,
    });
  const newIssue = async (title) =>
    (
      await json(
        await web("POST", "/issues", {
          project: "WAYP",
          title,
          agent: "claude",
          status: "todo",
        }),
      )
    ).key;
  const banner = page.getByRole("note", { name: "Objective" });
  const has = async (text, what) => {
    // Playwright's string hasText is case-insensitive, so a RegExp is needed for an exact-case match.
    const exact = new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
    await banner
      .filter({ hasText: exact })
      .waitFor({ timeout: 15000 })
      .catch(() => {});
    const t = (await banner.textContent().catch(() => "")) ?? "";
    ok(t.includes(text), `${tag} ${what}: ${t.slice(0, 260)}`);
    return t;
  };
  const workRound = async (k, comment) => {
    const t = await page.request.post(`${base}/api/v1/agent/take/${k}`, {
      headers: agentHdr,
      data: {},
    });
    ok(
      t.ok(),
      `${tag} the worker takes ${k} (${t.status()} ${t.ok() ? "" : await t.text()})`,
    );
    const d = await page.request.post(`${base}/api/v1/agent/done/${k}`, {
      headers: agentHdr,
      data: { comment },
    });
    ok(
      d.ok(),
      `${tag} the worker finishes the round (${d.status()}: ${(await d.text()).slice(0, 160)})`,
    );
  };
  const state = async (k) => {
    const o = (await json(await web("GET", `/issues/${k}`))).objective ?? {};
    return JSON.stringify({
      health: o.health,
      phase: o.phase,
      why: o.phase_why,
      round: o.round,
      next_at: o.next_at,
      state: o.state,
    });
  };
  const answerJob = async (kind, result, finish = {}) => {
    const job = await wait(async () => {
      const j = await poll();
      return j;
    }, `a ${kind} job`);
    ok(
      job.kind === kind,
      `${tag} the runner is handed a ${kind} job (${job.kind})`,
    );
    const input =
      typeof job.input === "string" ? JSON.parse(job.input) : job.input;
    const r = await fetch(`${base}/api/v1/runner/jobs/${job.id}/finish`, {
      method: "POST",
      headers: {
        ...H,
        "Content-Type": "application/json",
        Cookie: runnerCookie,
      },
      body: JSON.stringify({
        state: "done",
        exit_code: 0,
        result: result(input),
        ...finish,
      }),
    });
    ok(r.ok, `${tag} the job is finished (${r.status})`);
    return input;
  };

  const key = await newIssue(`Make the weekly report fast ${tag}`);
  await page.goto(`${base}/issue/${key}`);
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Set an objective…").click();
  const dialog = page.getByRole("dialog");
  await dialog
    .getByLabel("Check command")
    .fill("go test ./internal/report -run TestWeekly");
  await dialog.getByLabel("Until", { exact: true }).fill("< 1s");
  await dialog.getByRole("button", { name: "Add a command" }).click();
  await dialog
    .getByLabel("Command that must keep passing 1")
    .fill("make build");
  await dialog.getByLabel("Between rounds").fill("90m");
  await dialog.getByLabel("Most rounds").fill("5");
  await shot("1-dialog", false);
  const box = await dialog.boundingBox();
  ok(
    box && box.x >= 0 && box.x + box.width <= width + 1,
    `${tag} the dialog fits the screen`,
  );
  await dialog.getByRole("button", { name: "Set objective" }).click();
  await dialog.waitFor({ state: "detached" });
  const t1 = await has("round 1 of 5", "the banner names the round");
  ok(/Reading/.test(t1), `${tag} it is being read from where it starts`);
  ok(
    t1.includes("make build"),
    `${tag} the banner says what must keep passing`,
  );
  await shot("2-reading");
  await overflow("the reading banner");

  const input = await answerJob("objective", (i) => ({
    head: HEAD1,
    reading: {
      ok: true,
      value: 2.1,
      low: 2.0,
      high: 2.2,
      runs: 5,
      exit: 0,
      output: "ok",
    },
    kept: (i.keep_passing ?? []).map((c) => ({
      command: c,
      ok: true,
      output: "",
    })),
  }));
  ok(
    input.check?.includes("TestWeekly") &&
      input.keep_passing?.[0] === "make build" &&
      !!input.repo,
    `${tag} the job carries the check, what must keep passing and the repo`,
  );
  const t2 = await has(
    "Ready",
    "the banner follows the reading with no reload",
  );
  ok(
    /2\.1/.test(t2) || /Waypoint/.test(t2),
    `${tag} the starting reading shows`,
  );
  await shot("3-ready");
  await overflow("the ready banner");

  const forged = await fetch(`${base}/api/v1/issues/${key}/objective/checks`, {
    method: "POST",
    headers: { ...H, "Content-Type": "application/json", Cookie: runnerCookie },
    body: JSON.stringify({
      ok: true,
      value: 0.2,
      base_ok: true,
      base_value: 2.1,
      base_sha: "0123abc",
      tree: "t1",
      runs: 5,
      p: 0.01,
    }),
  });
  ok(forged.ok, `${tag} a worker's reading is accepted (${forged.status})`);
  const streamUp = page.waitForResponse((r) =>
    r.url().includes("/api/v1/stream"),
  );
  await page.reload();
  await streamUp;
  const worker = banner.getByText(/The worker’s own readings/);
  await worker.waitFor();
  ok(
    /they don’t decide/.test(await worker.textContent()),
    `${tag} it is shown as not deciding`,
  );
  const issue0 = await json(await web("GET", `/issues/${key}`));
  ok(
    issue0.status.category !== "done" && issue0.objective.state === "active",
    `${tag} a worker saying "met" closed nothing`,
  );

  await workRound(key, "Cached the totals");
  console.log(`     api after done: ${await state(key)}`);
  await has("Reading", "round 1 is being read after the worker finishes");
  const read1 = await answerJob("objective", (i) => ({
    head: HEAD2,
    base: HEAD1,
    reading: { ok: true, value: 1.4, low: 1.3, high: 1.5, runs: 5, exit: 0 },
    base_reading: {
      ok: true,
      value: 2.1,
      low: 2.0,
      high: 2.2,
      runs: 5,
      exit: 0,
    },
    p: 0.01,
    kept: (i.keep_passing ?? []).map((c) => ({ command: c, ok: true })),
  }));
  ok(
    read1.round === 1 && read1.base === HEAD1,
    `${tag} the round is read beside where it started`,
  );
  const t3 = await has(
    "Waiting",
    "a round that isn't met waits between rounds",
  );
  ok(/1\.4/.test(t3), `${tag} the verdict reading shows: ${t3.slice(0, 200)}`);
  const start = banner.getByRole("button", { name: /Start round 2 now/ });
  await start.waitFor().catch(async (e) => {
    await shot("4-waiting-FAILED");
    throw new Error(`${e.message.split("\n")[0]} api: ${await state(key)}`);
  });
  await shot("4-waiting");
  await overflow("the waiting banner");
  await start.click();
  await banner
    .getByRole("button", { name: /Start round 2 now/ })
    .waitFor({ state: "detached" });
  ok(true, `${tag} "Start round 2 now" ends the wait`);

  await workRound(key, "Rewrote the benchmark");
  await answerJob("objective", (i) => ({
    head: HEAD3,
    base: HEAD2,
    reading: { ok: true, value: 0.8, low: 0.7, high: 0.9, runs: 5, exit: 0 },
    base_reading: { ok: true, value: 1.4, runs: 5, exit: 0 },
    p: 0.01,
    kept: (i.keep_passing ?? []).map((c) => ({ command: c, ok: true })),
    touched: ["internal/report/weekly_test.go"],
  }));
  const t4 = await has(
    "Needs you",
    "a reading over changed check files waits for the person",
  );
  ok(
    t4.includes("What you can do:") && t4.includes("weekly_test.go"),
    `${tag} it says what changed and what to do`,
  );
  ok(!/\bhold\b|\bheld\b/i.test(t4), `${tag} no "hold/held" wording`);
  for (const b of [
    "Accept the reading",
    "Move on without a reading",
    "End objective",
  ])
    ok(
      await banner.getByRole("button", { name: b }).isVisible(),
      `${tag} control "${b}" is offered`,
    );
  await shot("5-blocked");
  await overflow("the blocked banner");

  await page.goto(`${base}/objectives`);
  const rows = page.getByRole("list", { name: "Objectives" }).locator("> li");
  await rows.first().waitFor();
  ok(
    (await rows.first().textContent()).includes("Needs you"),
    `${tag} a block that needs a person is listed first`,
  );
  ok(
    await page
      .getByText(/need(s)? you/)
      .first()
      .isVisible(),
    `${tag} the page counts what needs the person`,
  );
  await shot("6-list");
  await overflow("the objectives page");
  await page.getByRole("button", { name: /^Needs you/ }).click();
  ok(
    (await rows.count()) >= 1 &&
      (await rows.first().textContent()).includes("Needs you"),
    `${tag} filtering by "Needs you" keeps the blocked one`,
  );
  await page.getByRole("button", { name: /^Needs you/ }).click();
  await rows.first().getByRole("link").first().click();
  await page.waitForURL(new RegExp(`/issue/${key}`));

  await banner.getByRole("button", { name: "Accept the reading" }).click();
  console.log(`     api after accept: ${await state(key)}`);
  const t5 = await has("Met", "accepting the reading meets the objective");
  ok(
    !(await banner.getByRole("button", { name: "End objective" }).isVisible()),
    `${tag} a met objective offers no controls to end`,
  );
  const issue1 = await json(await web("GET", `/issues/${key}`));
  ok(
    issue1.status.category === "done",
    `${tag} the task is closed (${issue1.status.name})`,
  );
  await shot("7-met");
  await overflow("the met banner");
  await page.goto(`${base}/objectives`);
  ok(
    !(await page
      .getByText(`Make the weekly report fast ${tag}`)
      .isVisible()
      .catch(() => false)),
    `${tag} met objectives are off the list until asked for`,
  );
  await page.getByLabel("Include met and ended").click();
  await page.getByText(`Make the weekly report fast ${tag}`).waitFor();
  ok(true, `${tag} "Include met and ended" shows it`);
  await shot("8-list-all");

  const jkey = await newIssue(`Review the report package ${tag}`);
  await page.goto(`${base}/issue/${jkey}`);
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Set an objective…").click();
  const jd = page.getByRole("dialog");
  await jd.getByText("Until a reviewer finds nothing").click();
  await jd
    .getByLabel("What done means")
    .fill("No function in internal/report takes more than one lock.");
  await jd.getByRole("button", { name: "Set objective" }).click();
  await jd.waitFor({ state: "detached" });
  await has("Reading", "a judged objective is read too");
  const findings = [
    {
      title: "Weekly takes the ledger lock twice in one call",
      path: "internal/report/weekly.go",
      line: 41,
      severity: "high",
      problem:
        "Weekly locks the ledger, calls Totals which locks it again, and a writer between the two sees a half-built report. ".repeat(
          3,
        ) + "<img src=x onerror=alert(1)>",
      confirm: "Run TestWeekly with -race while a writer posts entries.",
    },
    {
      title: "A path long enough to need wrapping",
      path: "internal/report/some/deeply/nested/directory/with/a/very/long/path/name/that/goes/on/report_export_helper.go",
      line: 7,
      severity: "medium",
      problem: "Helper.",
      confirm: "Read it.",
    },
  ];
  const audit = (summary, list) =>
    answerJob("audit", () => ({
      answer: "x",
      structured: { summary, findings: list },
    }));
  await answerJob("objective", () => ({ head: HEAD1 }));
  await audit("Two things left.", findings);
  const t6 = await has(
    "Weekly takes the ledger lock twice",
    "the reviewer's findings show where it starts",
  );
  await has("Ready", "the judged objective is ready for round 1");
  ok(t6.includes("report_export_helper.go"), `${tag} the long path shows`);
  ok(
    (await page.locator("img[src='x']").count()) === 0,
    `${tag} a model's words never become markup`,
  );
  await shot("9-findings");
  await overflow("the findings");
  await workRound(jkey, "Split the locks");
  await answerJob("objective", () => ({ head: HEAD2, base: HEAD1 }));
  await audit("One thing left.", findings.slice(1));
  const t7 = await has(
    "Round 2 waits to be handed out",
    "one thing left, so round 2 is open",
  );
  ok(
    !(await banner
      .getByRole("button", { name: /Start round 2 now/ })
      .isVisible()),
    `${tag} nothing to cut short`,
  );
  ok(
    t7.includes("1 of 2") ||
      t7.includes("helper") ||
      t7.includes("path long enough"),
    `${tag} the reviewer's remaining finding shows: ${t7.slice(0, 120)}`,
  );
  await workRound(jkey, "Moved the helper");
  await answerJob("objective", () => ({ head: HEAD3, base: HEAD2 }));
  await audit("Nothing left.", []);
  console.log(`     api after the empty audit: ${await state(jkey)}`);
  await has("Met", "a reviewer that finds nothing meets the objective");
  await shot("9b-judged-met");
  await overflow("the judged objective met");

  const pkey = await newIssue(`Tidy the flakiest test ${tag}`);
  const proposed = await json(
    await page.request.put(`${base}/api/v1/issues/${pkey}/objective`, {
      headers: agentHdr,
      data: {},
    }),
  );
  ok(
    proposed.state === "proposed",
    `${tag} an agent's objective is only proposed`,
  );
  await page.goto(`${base}/issue/${pkey}`);
  await has("Proposed", "the banner says it is a proposal");
  await banner.getByRole("button", { name: "Confirm this proposal" }).waitFor();
  await shot("10-proposed");
  await overflow("the proposal");
  await banner.getByRole("button", { name: "Confirm this proposal" }).click();
  await banner
    .getByRole("button", { name: "Confirm this proposal" })
    .waitFor({ state: "detached" });
  ok(true, `${tag} confirmed from the banner`);
  await banner.getByRole("button", { name: "End objective" }).click();
  const confirm = page.getByRole("alertdialog").or(page.getByRole("dialog"));
  await confirm.getByRole("button", { name: "End objective" }).click();
  await has("Ended", "ending it is asked first, then shown");
  await shot("11-ended");
  await overflow("the ended banner");

  const dkey = await newIssue(`Make the lint pass ${tag}`);
  await page.goto(`${base}/issue/${dkey}`);
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Set an objective…").click();
  const dd = page.getByRole("dialog");
  await dd.getByLabel("Check command").fill("make lint");
  await dd.getByLabel("Until", { exact: true }).fill("pass");
  const after = dd.getByLabel("Diagnose after");
  ok(
    (await after.inputValue()) === "2",
    `${tag} the dialog offers Diagnose after, 2 by default`,
  );
  await after.fill("1");
  await shot("13-diagnose-dialog", false);
  const sentSet = page.waitForRequest(
    (r) =>
      r.method() === "PUT" &&
      r.url().endsWith(`/api/v1/issues/${dkey}/objective`),
  );
  await dd.getByRole("button", { name: "Set objective" }).click();
  const sentBody = JSON.parse((await sentSet).postData() ?? "{}");
  ok(
    sentBody.diagnose_after === 1,
    `${tag} the dialog sends the Diagnose after it was given (${sentBody.diagnose_after})`,
  );
  await dd.waitFor({ state: "detached" });
  const failing = { ok: false, runs: 1, exit: 1, output: "FAIL: TestBoard" };
  await answerJob("objective", () => ({ head: HEAD1, reading: failing }));
  await has("Ready", "the lint objective is ready");
  await workRound(dkey, "Tried a fix");
  await answerJob("objective", () => ({
    head: HEAD2,
    base: HEAD1,
    reading: failing,
    base_reading: failing,
  }));
  await has(
    "a diagnose round",
    "a round that came no nearer is followed by a diagnose round",
  );
  const dline = await banner.textContent();
  ok(
    /ships no code/.test(dline),
    `${tag} the banner says the round ships no code: ${dline.slice(0, 200)}`,
  );
  await shot("14-diagnose-round");
  await overflow("the diagnose round");
  const dt = await page.request.post(`${base}/api/v1/agent/take/${dkey}`, {
    headers: agentHdr,
    data: {},
  });
  ok(dt.ok(), `${tag} the worker takes the diagnose round`);
  const dn = await page.request.post(`${base}/api/v1/agent/done/${dkey}`, {
    headers: agentHdr,
    data: { comment: "done" },
  });
  ok(
    dn.status() === 400 && (await dn.text()).includes("wp objective diagnose"),
    `${tag} wp done in a diagnose round is refused (${dn.status()})`,
  );
  const rec = await page.request.post(
    `${base}/api/v1/issues/${dkey}/objective/diagnose`,
    {
      headers: agentHdr,
      data: {
        text: "## Why\n\nThe **check** runs the whole suite, so one flaky test hides every fix.\n\nTry: run only `TestBoard`.\n\n<img src=x onerror=alert(1)>",
      },
    },
  );
  ok(
    rec.ok(),
    `${tag} the diagnosis is recorded (${rec.status()}: ${(await rec.text()).slice(0, 160)})`,
  );
  await page.reload();
  const diag = page.getByTestId("objective-diagnosis");
  await diag.waitFor();
  ok(
    (await diag.textContent()).includes("flaky test hides every fix"),
    `${tag} the latest diagnosis shows in the banner`,
  );
  ok(
    (await page.locator("img[src='x']").count()) === 0,
    `${tag} a diagnosis never becomes markup`,
  );
  ok(
    !/a diagnose round/.test(await banner.textContent()),
    `${tag} round 2 is a work round again`,
  );
  await banner.getByText(/finished round/).click();
  const rd = banner.getByTestId("round-diagnosis").first();
  await rd.waitFor();
  ok(
    /diagnosed/.test(await banner.textContent()),
    `${tag} the round list shows the diagnosed outcome`,
  );
  ok(
    /the latest, shown below/.test(await rd.textContent()),
    `${tag} the latest round's diagnosis is a pointer in the list`,
  );
  ok(
    (await banner.getByText("flaky test hides every fix").count()) === 1,
    `${tag} the diagnosis text shows once, not twice`,
  );
  await shot("15-diagnosed");
  await overflow("the recorded diagnosis");

  await page.goto(`${base}/settings?tab=notifications`);
  await page
    .getByRole("checkbox", { name: /Objectives/ })
    .waitFor()
    .catch(async (e) => {
      await shot("12-notifications-FAILED");
      throw e;
    });
  ok(true, `${tag} notification settings list Objectives`);
  await overflow("the notification settings");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [390, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
srv.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
