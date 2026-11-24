// Holds (WP-269): putting a task on hold from the issue's menu, the banner
// and the row marker, the queue's "On hold" group, and lifting it.
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

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const title = `Rerun the eval after the quota refreshes ${tag}`;
  const res = await page.request.post(base + "/api/v1/issues", {
    headers: { "X-Waypoint-Client": "web" },
    data: { project: "WAYP", title, agent: "claude", status: "todo" },
  });
  const key = (await res.json()).key;

  await page.goto(`${base}/issue/${key}`);
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Put on hold…").click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Until").fill("2099-11-03");
  await dialog.getByLabel("Why it waits").fill("The gateway's quota refreshes");
  await shot("dialog");
  await dialog.getByRole("button", { name: "Put on hold" }).click();
  await dialog.waitFor({ state: "detached" });
  const banner = page.getByRole("note").filter({ hasText: "On hold until" });
  await banner.waitFor();
  ok(
    (await banner.textContent()).includes("The gateway's quota refreshes"),
    `${tag} banner says why`,
  );
  await shot("issue");
  await overflow("the issue");
  const issue = await (
    await page.request.get(`${base}/api/v1/issues/${key}`)
  ).json();
  ok(
    issue.hold?.until?.startsWith("2099-11-0") && issue.hold.by === "robin",
    `${tag} hold saved: ${JSON.stringify(issue.hold)}`,
  );

  await page.goto(`${base}/p/WAYP/list`);
  const row = page.locator("[data-nav-key]", { hasText: title });
  await row.waitFor();
  ok(
    await row.getByText("On hold", { exact: true }).isVisible(),
    `${tag} list marks the hold`,
  );
  await shot("list");

  await page.goto(`${base}/agents?tab=queue&project=WAYP`);
  const wait = page.locator("li", { hasText: title });
  await wait.waitFor();
  ok(
    (await wait.textContent()).includes("On hold until"),
    `${tag} queue says it's on hold`,
  );
  await shot("queue");
  await overflow("the queue");
  await wait.getByRole("button", { name: "Lift hold" }).click();
  await page.getByText(`${key} is off hold`).waitFor();

  await page.request.put(`${base}/api/v1/issues/${key}/hold`, {
    headers: { "X-Waypoint-Client": "web" },
    data: {},
  });
  await page.goto(`${base}/issue/${key}`);
  const forever = page
    .getByRole("note")
    .filter({ hasText: "On hold until someone lifts it" });
  await forever.waitFor();
  await forever.getByRole("button", { name: "Lift hold" }).click();
  await forever.waitFor({ state: "detached" });
  ok(true, `${tag} lifted from the banner`);

  const web = { "X-Waypoint-Client": "web" };
  const dep = await (
    await page.request.post(base + "/api/v1/issues", {
      headers: web,
      data: { project: "WAYP", title: `Comes first ${tag}`, status: "todo" },
    })
  ).json();
  await page.request.put(`${base}/api/v1/issues/${key}/hold`, {
    headers: web,
    data: { after: [dep.key] },
  });
  await page.goto(`${base}/issue/${key}`);
  const waiting = page
    .getByRole("note")
    .filter({ hasText: `On hold until ${dep.key} is done` });
  await waiting.waitFor();
  await shot("issue-waits");
  await overflow("the issue with a condition");
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByText("Change hold…").click();
  const change = page.getByRole("dialog");
  ok(
    (await change.textContent()).includes(
      `It also waits until ${dep.key} is done`,
    ),
    `${tag} the dialog says what it still waits for`,
  );
  await change.getByLabel("Why it waits").fill("The dependency goes first");
  await change.getByRole("button", { name: "Save" }).click();
  await change.waitFor({ state: "detached" });
  await waiting.filter({ hasText: "The dependency goes first" }).waitFor();
  ok(true, `${tag} changing the hold keeps what it waits for`);
  await waiting.getByRole("button", { name: "Lift hold" }).click();
  await waiting.waitFor({ state: "detached" });

  const token = (
    await (
      await page.request.post(base + "/api/v1/me/tokens", {
        headers: web,
        data: { name: `opus ${tag}`, agent: "anthropic/claude-opus-5-5" },
      })
    ).json()
  ).token;
  const agent = { Authorization: `Bearer ${token}` };
  await page.request.post(`${base}/api/v1/agent/take/${key}`, {
    headers: agent,
    data: {},
  });
  const gave = await page.request.post(`${base}/api/v1/agent/release/${key}`, {
    headers: agent,
    data: { comment: "Needs a GPU" },
  });
  ok(gave.ok(), `${tag} an agent gave it back`);
  await page.goto(`${base}/agents?tab=queue&project=WAYP`);
  await page.getByText(/^Given back/).first().waitFor();
  const back = page.locator("li", { hasText: title });
  await back.waitFor();
  ok(
    (await back.textContent()).includes("An agent gave it back"),
    `${tag} queue says it was given back`,
  );
  await shot("queue-given-back");
  await overflow("the queue with a given-back task");
  await back.getByRole("button", { name: "End the wait" }).click();
  await page.getByText(`${key} can go out again`).waitFor();
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
