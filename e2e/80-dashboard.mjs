// Dashboards and the task Summary (WP-780). A project's Overview says in a
// sentence what Waypoint sees, shows its pace, goals, columns and what needs
// attention; "Ask an agent" files one task for a problem and the row then
// points at it; cards hide and stay hidden; Details shows the queries. The
// all-projects Overview has a card per project. A task opens on Summary:
// where it stands and who asked; an agent's write-up is one paragraph there
// and whole under Details, a person's description shows whole.
// Desktop and 390px.
// Needs user robin (run 01-signup-flow first).
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

let n = 0;
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const KEY = `DSH${"ABCD"[n++]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  const overflow = async (what) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = async (m, p, d, headers) => (await call(m, p, d, headers)).json();

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page.waitForURL(base + "/");
  await api("POST", "/projects", { name: `Harbor ${tag}`, key: KEY });
  const { token } = await api("POST", "/me/tokens", { name: `dash ${tag}`, agent: "anthropic/claude-opus-5-5" });
  const agent = { ...H, Authorization: `Bearer ${token}` };

  const tasks = [];
  for (const title of ["Chart the tides", "Paint the docks", "Fix the crane", "Order new rope"])
    tasks.push(await api("POST", "/issues", { project: KEY, title }));
  for (const t of tasks.slice(0, 3)) await call("PATCH", `/issues/${t.key}`, { status: "In Progress" });
  const yesterday = new Date(Date.now() - 2 * 864e5).toISOString().slice(0, 10);
  const late = await call("PATCH", `/issues/${tasks[3].key}`, { due_date: yesterday });
  ok(late.ok(), `${tag} a task is past its date: ${late.status()}`);
  const written = await api(
    "POST",
    "/issues",
    {
      project: KEY,
      title: "Rework the berth booking",
      description:
        "## Why\n\nBoats book a berth for the night without calling the office.\n\n## Plan\n\n- add `berths` table\n- migrate `bookings.go`\n- wire the API",
    },
    agent,
  );
  const mine = await api("POST", "/issues", {
    project: KEY,
    title: "Ask the harbour master",
    description: "Ask about winter berths before December.",
  });

  // ---- a project's Overview ----
  await page.goto(`${base}/p/${KEY}`);
  await page.waitForURL(`${base}/p/${KEY}/overview`);
  const says = page.getByTestId("understanding");
  await says.waitFor();
  ok(/\b6 open tasks/.test(await says.innerText()) && /no goals yet/.test(await says.innerText()), `${tag} it says what it sees: ${await says.innerText()}`);
  ok(await page.getByText("Done a week").isVisible(), `${tag} the pace tile shows`);
  const stage = page.locator('[data-stage="In Progress"]');
  const held = (await stage.locator("[data-stage-count]").innerText()).trim();
  ok(held === "3", `${tag} In Progress holds 3: ${held}`);
  const attention = page.getByRole("list", { name: "Needs attention", exact: true });
  const overdue = attention.locator("[data-signal=overdue]");
  await overdue.waitFor();
  ok(await attention.locator("[data-signal=no_goals]").isVisible(), `${tag} no goals is flagged`);
  ok(await attention.locator("[data-signal=no_goals]").getByRole("button", { name: "Suggest goals", exact: true }).isVisible(), `${tag} with Suggest goals`);
  await shot("overview");
  await overflow("the project Overview");

  // One click files a task for an agent, once.
  await overdue.getByRole("button", { name: "Ask an agent", exact: true }).click();
  await page.getByText(/Filed DSH[A-D]-\d+ for an agent/).waitFor();
  const filed = overdue.getByRole("link", { name: /DSH[A-D]-\d+/ });
  await filed.waitFor();
  const fkey = (await filed.innerText()).trim();
  const fi = await api("GET", `/issues/${fkey}`);
  ok(fi.agent === "any" && fi.description.includes(tasks[3].key), `${tag} the filed task is for any agent and lists the late task: ${fi.agent}`);
  const again = await api("POST", `/projects/${KEY}/dashboard/fix`, { signal: "overdue" });
  ok(again.created === false && again.issue.key === fkey, `${tag} a second click finds the first task`);

  // Details shows the query; a card hides and stays hidden.
  await page.getByRole("button", { name: "Details", exact: true }).click();
  ok(await overdue.getByText("is:overdue").isVisible(), `${tag} Details shows the query`);
  await page.getByRole("button", { name: "Hide details", exact: true }).click();
  await page.getByRole("button", { name: "Customize", exact: true }).click();
  await page.getByRole("checkbox", { name: "Pace", exact: true }).uncheck();
  await page.reload();
  await says.waitFor();
  ok((await page.getByRole("heading", { name: "Pace", exact: true }).count()) === 0, `${tag} a hidden card stays hidden`);
  ok(await page.getByRole("heading", { name: "How work moves", exact: true }).isVisible(), `${tag} the others stay`);
  await page.getByRole("button", { name: "Customize", exact: true }).click();
  await page.getByRole("checkbox", { name: "Pace", exact: true }).check();
  await page.getByRole("button", { name: "Customize", exact: true }).click();

  // ---- every project ----
  await page.goto(`${base}/overview`);
  const card = page.locator(`[data-project-card=${KEY}]`);
  await card.waitFor();
  ok(/\b[67] open tasks/.test(await card.innerText()), `${tag} the project's card counts its open tasks`);
  ok(await page.getByRole("list", { name: "Needs attention", exact: true }).getByText(`Harbor ${tag}`).first().isVisible(), `${tag} problems name their project`);
  await shot("all-projects");
  await overflow("the all-projects Overview");

  // ---- a task's Summary ----
  await page.evaluate(() => localStorage.setItem("wp:issue-tab", '"summary"'));
  await page.goto(`${base}/issue/${written.key}`);
  const summary = page.getByRole("region", { name: "Summary", exact: true });
  await summary.waitFor();
  ok(await page.getByRole("tab", { name: "Summary", exact: true }).getAttribute("aria-selected") === "true", `${tag} a task opens on Summary`);
  const life = summary.getByRole("group", { name: "Lifecycle" });
  ok(/Created .+ by claude-/.test(await life.innerText()), `${tag} the lifecycle says who wrote it: ${(await life.innerText()).split("\n")[0]}`);
  ok(await page.getByText("Boats book a berth for the night without calling the office.").isVisible(), `${tag} the agent's opening paragraph shows`);
  ok((await page.getByText("bookings.go").count()) === 0, `${tag} its implementation plan does not`);
  await shot("task-summary");
  await overflow("a task's Summary");
  await page.getByRole("tab", { name: "Details", exact: true }).click();
  ok(await page.getByRole("tab", { name: "Details", exact: true }).getAttribute("aria-selected") === "true", `${tag} the write-up opens Details`);
  await page.getByText("bookings.go").first().waitFor();
  ok(true, `${tag} Details shows the whole write-up`);
  await page.getByRole("tab", { name: "Summary", exact: true }).click();
  await page.goto(`${base}/issue/${mine.key}`);
  await page.getByRole("region", { name: "Summary", exact: true }).waitFor();
  ok(await page.getByText("Ask about winter berths before December.").isVisible(), `${tag} a person's description shows whole on Summary`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\nall dashboard checks passed");
