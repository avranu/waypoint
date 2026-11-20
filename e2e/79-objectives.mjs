// Objectives (WP-927): giving a task an objective from its menu, the banner
// with the round and its last check, an agent's proposal a person confirms,
// a round ending (the task opens again) and the queue's "Between rounds".
// Run after 01-signup-flow (user robin, project WAYP).
import { chromium } from "playwright";
import fs from "node:fs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const browser = await chromium.launch();

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
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
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const overflow = async (what) =>
    ok(
      (await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      )) || width > 600,
      `${tag} ${what} has no sideways scroll`,
    );
  const web = { "X-Waypoint-Client": "web" };
  const json = async (r) => {
    if (!r.ok()) fail(`${tag} ${r.status()} ${r.url()}: ${await r.text()}`);
    return r.json();
  };
  const newIssue = async (title) =>
    (
      await json(
        await page.request.post(base + "/api/v1/issues", {
          headers: web,
          data: { project: "WAYP", title, agent: "claude", status: "todo" },
        }),
      )
    ).key;

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  // ---- set one from the menu ----
  const key = await newIssue(`Make the weekly report fast ${tag}`);
  await page.goto(`${base}/issue/${key}`);
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Set an objective…").click();
  const dialog = page.getByRole("dialog");
  await dialog
    .getByLabel("Check command")
    .fill("go test ./internal/report -run TestWeekly");
  await dialog.getByLabel("Until").fill("< 1s");
  await dialog.getByLabel("Most rounds").fill("5");
  await shot("dialog");
  await overflow("the dialog");
  await dialog.getByRole("button", { name: "Set objective" }).click();
  await dialog.waitFor({ state: "detached" });
  const banner = page.getByRole("note", { name: "Objective" });
  await banner.filter({ hasText: "Objective · round 1 of 5" }).waitFor();
  ok(
    (await banner.textContent()).includes("runs in under 1s"),
    `${tag} banner says the target: ${await banner.textContent()}`,
  );

  // ---- a check shows on the banner ----
  const check = await json(
    await page.request.post(`${base}/api/v1/issues/${key}/objective/checks`, {
      headers: web,
      data: {
        ok: true,
        value: 1.42,
        base_ok: true,
        base_value: 2.1,
        base_sha: "0123abc",
        tree: "t1",
        runs: 5,
        p: 0.01,
      },
    }),
  );
  ok(
    check.met === false && check.check.improved,
    `${tag} 1.42s: better, not met`,
  );
  await page.reload();
  await banner.filter({ hasText: "This round’s last check" }).waitFor();
  ok(
    (await banner.textContent()).includes("not met"),
    `${tag} banner shows the check`,
  );
  await shot("issue");
  await overflow("the issue");

  // ---- changing it keeps the rest ----
  await banner.getByRole("button", { name: "Change…" }).click();
  const change = page.getByRole("dialog");
  ok(
    (await change.getByLabel("Until").inputValue()) === "< 1s" &&
      (await change.getByLabel("Most rounds").inputValue()) === "5",
    `${tag} the dialog opens with what is set`,
  );
  await change.getByLabel("Until").fill("< 500ms");
  await change.getByRole("button", { name: "Save" }).click();
  await change.waitFor({ state: "detached" });
  await banner.filter({ hasText: "under 500ms" }).waitFor();
  ok(true, `${tag} changed the target`);

  // ---- an agent's is a proposal until a person confirms it ----
  const token = (
    await json(
      await page.request.post(base + "/api/v1/me/tokens", {
        headers: web,
        data: { name: `opus obj ${tag}`, agent: "anthropic/claude-opus-5-5" },
      }),
    )
  ).token;
  const agent = { Authorization: `Bearer ${token}` };
  const loopKey = await newIssue(`Tidy the flakiest test ${tag}`);
  const proposed = await json(
    await page.request.put(`${base}/api/v1/issues/${loopKey}/objective`, {
      headers: agent,
      data: {},
    }),
  );
  ok(proposed.state === "proposed", `${tag} an agent's objective is proposed`);
  await page.goto(`${base}/issue/${loopKey}`);
  await banner.filter({ hasText: "Proposed objective" }).waitFor();
  await shot("proposed");
  await overflow("the proposal");
  await banner.getByRole("button", { name: "Confirm" }).click();
  await banner.filter({ hasText: "Objective · round 1" }).waitFor();
  ok(true, `${tag} confirmed from the banner`);

  // ---- a round ends: the task opens again and waits for the next ----
  await page.request.post(`${base}/api/v1/agent/take/${loopKey}`, {
    headers: agent,
    data: {},
  });
  const done = await page.request.post(`${base}/api/v1/agent/done/${loopKey}`, {
    headers: agent,
    data: { comment: "Fixed the first one" },
  });
  ok(
    done.ok() && (await done.text()).includes("round 2"),
    `${tag} done says the task opens again for round 2`,
  );
  const after = await json(
    await page.request.get(`${base}/api/v1/issues/${loopKey}`),
  );
  ok(
    after.objective?.round === 2 && after.status.category === "todo",
    `${tag} round 2, open again: ${after.status.name}, round ${after.objective?.round}`,
  );
  await page.reload();
  await banner.filter({ hasText: "Objective · round 2" }).waitFor();
  ok(
    (await banner.textContent()).includes(
      "Agents aren’t handed round 2 before",
    ),
    `${tag} banner says when the next round may start`,
  );
  await banner.getByText("1 finished round").click();
  await shot("rounds");
  await overflow("the rounds");

  await page.goto(`${base}/agents?tab=queue&project=WAYP`);
  await page
    .getByText(/^Between rounds/)
    .first()
    .waitFor();
  const wait = page.locator("li", { hasText: `Tidy the flakiest test ${tag}` });
  await wait.waitFor();
  ok(
    (await wait.textContent()).includes("isn't handed out before"),
    `${tag} queue says it waits between rounds`,
  );
  await shot("queue");
  await overflow("the queue");

  // ---- removed: an ordinary task again ----
  await page.goto(`${base}/issue/${loopKey}`);
  await banner.getByRole("button", { name: "Remove" }).click();
  await banner.waitFor({ state: "detached" });
  ok(true, `${tag} removed from the banner`);
  await ctx.close();
}

for (const [w, s] of [
  [1440, "light"],
  [390, "dark"],
])
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
