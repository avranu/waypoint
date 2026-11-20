// A stalled background job on its session's page (WP-605) in a real browser
// at desktop and phone widths: the runner reports it, and the timeline shows
// a line for it without sideways scroll.
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
const browser = await chromium.launch();

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const api = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  const overflow = () => page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const { token: agentTok } = await api("POST", "/me/tokens", { name: `stalled agent ${tag}`, agent: "anthropic/claude-opus-5-5" });
  const { token: runnerTok } = await api("POST", "/me/tokens", { name: `stalled runner ${tag}` });
  const AG = { ...H, Authorization: `Bearer ${agentTok}` };
  const RU = { ...H, Authorization: `Bearer ${runnerTok}` };
  const cli = `e2e-stalled-${n}`;
  const w = { session_id: cli, machine: "box", cwd: "/work/app" };
  await api("POST", "/sessions/hook", { events: [{ event: "SessionStart", ...w }] }, AG);
  await api("POST", "/sessions/hook", { events: [{ event: "UserPromptSubmit", ...w, prompt: "push once lint is done" }] }, AG);
  const runner = await api("POST", "/runner/register", { name: "box", hostname: "box", os: "linux", arch: "amd64", version: "test", allowed_dirs: ["/work"] }, RU);
  const command = 'while pgrep -f "golangci-lint run" >/dev/null; do sleep 5; done; git push';
  const told = await api(
    "POST",
    "/runner/stalled-jobs",
    {
      runner: runner.id,
      jobs: [
        {
          session: cli,
          task: "b1x9q",
          pid: 4242,
          group: true,
          command,
          quiet_minutes: 30,
          evidence: ["Running 30 min; no output at all.", "`pgrep -f 'golangci-lint run'` matches the job's own `bash -c` command line, so it always finds a process."],
          running: ["pid 4242: bash (the job's shell)", "pid 5000: sleep 5"],
        },
      ],
    },
    RU,
  );
  ok(told.jobs?.length === 1 && /its session/.test(told.jobs[0].outcome), `${tag} the runner's report is answered: ${JSON.stringify(told)}`);
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  const id = db.prepare(`SELECT id FROM agent_sessions WHERE cli_id = ?`).get(cli).id;
  db.close();

  await page.goto(`${base}/agents/sessions/${id}`);
  const line = page.locator("li", { hasText: "No progress from background job" });
  await line.waitFor();
  ok((await line.getByText("b1x9q").count()) === 1, `${tag} the line names the job`);
  ok((await line.getByText(/^No progress for 30 min; /).count()) === 1, `${tag} the line says how long and what was done`);
  ok(!(await overflow()), `${tag} no sideways scroll`);
  await line.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `${out}/stalled-${width}-${scheme}.png` });
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
