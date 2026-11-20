// An agent's close waits for a person's review (WP-272) in a real browser at
// desktop and phone widths: the issue an agent moved to Done stays in Done
// with a "Not reviewed" chip on its card and row; Waiting on you counts the
// closes and links to Catch up; the issue page says which agent closed it and
// marks the close reviewed; the list reviews selected closes in bulk; a
// person's own close carries no chip.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
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
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const settle = () => page.waitForTimeout(400);
  const overflow = () =>
    page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth + 1,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `CR${width}${scheme[0]}`.toUpperCase();
  await api("POST", "/projects", { name: `Close review ${tag}`, key });
  const { token } = await api("POST", "/me/tokens", {
    name: `closer ${tag}`,
    agent: "anthropic/claude-haiku-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };

  // Robin files three; an agent closes two, Robin closes the third herself.
  const keys = [];
  for (const t of [
    "Agent closes this",
    "Agent closes that",
    "Robin closes this",
  ]) {
    keys.push(
      (await api("POST", "/issues", { project: key, title: `${t} ${tag}` }))
        .key,
    );
  }
  const [a1, a2, mine] = keys;
  for (const k of [a1, a2]) {
    const r = await call(
      "POST",
      `/issues/${k}/transition`,
      { status: "Done" },
      AG,
    );
    ok(r.ok(), `${tag} the agent moved ${k} to Done (${r.status()})`);
  }
  await api("POST", `/issues/${mine}/transition`, { status: "Done" });
  const got = Object.fromEntries(
    await Promise.all(
      keys.map(async (k) => [k, await api("GET", `/issues/${k}`)]),
    ),
  );
  ok(
    got[a1].close_unreviewed && got[a2].close_unreviewed,
    `${tag} API: the agent's closes wait for review`,
  );
  ok(!got[mine].close_unreviewed, `${tag} API: Robin's own close doesn't`);
  ok(
    got[a1].status.category === "done",
    `${tag} the agent's close still took effect`,
  );

  // ---- Waiting on you ----
  const waiting = await api("GET", "/waiting");
  ok(waiting.closes >= 2, `${tag} API: ${waiting.closes} closes wait on robin`);
  await page.goto(base + "/inbox");
  const section = page.getByRole("region", { name: "Waiting on you" });
  const row = section.getByRole("link", {
    name: /agents finished, not looked at yet/,
  });
  await row.waitFor();
  ok(
    (await row.innerText()).includes(`${waiting.closes} task`),
    `${tag} Waiting on you counts ${waiting.closes} closes`,
  );
  ok(!(await overflow()), `${tag} inbox has no sideways scroll`);
  await shot("inbox");
  // The row opens Catch up (WP-245); this project's closes are there.
  await page.goto(`${base}/catch-up?project=${key}`);
  await page.getByText(`Agent closes this ${tag}`).waitFor();
  ok(
    await page.getByText("1 of 2").isVisible(),
    `${tag} Catch up lists the agent's closes, not Robin's`,
  );
  await shot("catch-up");

  // ---- the board: closed cards stay in Done, the agent's carry the chip ----
  await page.goto(`${base}/p/${key}/board`);
  await page.getByText(`Agent closes this ${tag}`).first().waitFor();
  await settle();
  ok(
    (await page.getByTitle(/no person has reviewed the close/).count()) === 2,
    `${tag} board: two cards carry the chip`,
  );
  ok(
    await page.getByText("Not reviewed").first().isVisible(),
    `${tag} board: the chip reads Not reviewed`,
  );
  ok(!(await overflow()), `${tag} board has no sideways scroll`);
  await shot("board");

  // ---- the issue page: who closed it, and Mark reviewed ----
  await page.goto(`${base}/issue/${a1}`);
  const line = page.getByText(/Moved to Done by/);
  await line.waitFor();
  ok(
    await line.isVisible(),
    `${tag} issue page names the agent that closed it`,
  );
  await shot("issue-unreviewed");
  await page.getByRole("button", { name: "Mark reviewed" }).click();
  await page.getByText(/Reviewed by/).waitFor();
  ok(
    !(await api("GET", `/issues/${a1}`)).close_unreviewed,
    `${tag} Mark reviewed cleared the close`,
  );
  ok(!(await overflow()), `${tag} issue page has no sideways scroll`);
  await shot("issue-reviewed");

  // ---- the list: review the rest in bulk ----
  // closed issues show in the list once "done" is on
  await page.goto(`${base}/p/${key}/list?group=none&done=true`);
  const target = page
    .locator("div.group\\/sel")
    .filter({ hasText: `Agent closes that ${tag}` });
  await target.first().waitFor();
  ok(
    (await page.getByTitle(/no person has reviewed the close/).count()) === 1,
    `${tag} list: only the unreviewed close carries the chip`,
  );
  await target
    .first()
    .getByRole("button", { name: "Select" })
    .click({ force: true });
  const bulk = page.getByRole("button", { name: "Review 1 close" });
  await bulk.waitFor();
  await shot("list-bulk");
  await bulk.click();
  await page.getByText("Reviewed 1 close").waitFor();
  await page.waitForFunction(
    () =>
      !document.querySelector("[title*='no person has reviewed the close']"),
  );
  ok(
    !(await api("GET", `/issues/${a2}`)).close_unreviewed,
    `${tag} bulk review cleared the close`,
  );
  ok(!(await overflow()), `${tag} list has no sideways scroll`);
  await shot("list-reviewed");

  // ---- reopening and an agent's second close ask again ----
  await api("POST", `/issues/${a1}/transition`, { status: "To Do" });
  await call("POST", `/issues/${a1}/transition`, { status: "Done" }, AG);
  ok(
    (await api("GET", `/issues/${a1}`)).close_unreviewed,
    `${tag} an agent's second close waits again`,
  );
  await call("POST", `/review/closes`, { issues: [a1] });
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
