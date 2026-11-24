// A project's Agents tab (WP-99): the tab lists that project's queue, open
// requests and sessions and nothing of another project; the global Agents
// page links to it; Pause agents stops the project; and the tab strip at 390
// wide never clips, with a More menu reaching every tab.
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
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then(async (r) => {
      const t = await r.text();
      if (r.status() >= 300) throw new Error(`${m} ${p}: ${r.status()} ${t}`);
      try { return JSON.parse(t); } catch { return t; }
    });
  const shot = async (name) => {
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };
  const noSideways = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const sfx = `${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  const a = `PA${sfx}`, b = `PB${sfx}`;
  await api("POST", "/projects", { name: `Agents A ${tag}`, key: a });
  await api("POST", "/projects", { name: `Agents B ${tag}`, key: b });
  const mk = (project, title) => api("POST", "/issues", { project, title, agent: "claude", priority: "high" });
  await mk(a, "Alpha parser rewrite");
  await mk(a, "Alpha docs pass");
  await mk(b, "Beta importer");

  await page.goto(`${base}/p/${a}/agents`);
  await page.getByRole("heading", { name: "Agents", exact: true }).waitFor();
  await page.getByText("Alpha parser rewrite").first().waitFor();
  ok(await page.getByText("Alpha docs pass").first().isVisible(), `${tag} the tab lists its project's queue`);
  ok((await page.getByText("Beta importer").count()) === 0, `${tag} nothing of the other project`);
  ok(await page.getByRole("button", { name: "Add work" }).first().isVisible(), `${tag} the queue's own actions are here`);
  ok((await page.getByText("Every agent’s queue").count()) === 0, `${tag} no second queue title`);
  ok((await page.getByText("Needs you").count()) === 0 && (await page.getByText("Sessions now").count()) === 0, `${tag} empty sections are hidden`);
  await shot("agents-a");
  await noSideways("project agents");

  await page.goto(`${base}/p/${b}/agents`);
  await page.getByText("Beta importer").first().waitFor();
  ok((await page.getByText("Alpha parser rewrite").count()) === 0, `${tag} the other project shows only its own`);

  await page.getByRole("button", { name: "Pause agents" }).click();
  await page.getByRole("button", { name: "Resume agents" }).waitFor();
  ok((await api("GET", `/projects/${b}`)).settings.agents_paused === true, `${tag} Pause agents pauses the project`);
  await shot("agents-paused");
  await page.getByRole("button", { name: "Resume agents" }).click();
  await page.getByRole("button", { name: "Pause agents" }).waitFor();
  ok((await api("GET", `/projects/${b}`)).settings.agents_paused !== true, `${tag} Resume agents resumes it`);

  await page.goto(`${base}/agents?tab=queue&project=${a}`);
  await page.getByRole("link", { name: `${a} Agents tab` }).click();
  await page.waitForURL(`${base}/p/${a}/agents`);
  ok(true, `${tag} the global Agents page links to the project's tab`);

  const labels = ["Board", "Backlog", "List", "Timeline", "Agents", "Reports", "Environments", "Pages", "Design", "Glossary", "Settings"];
  const nav = page.getByRole("navigation", { name: "Project sections" });
  const hidden = [];
  if (width < 700) await nav.getByRole("button", { name: "More" }).waitFor();
  await page.evaluate(() => document.fonts.ready);
  await page.waitForFunction(() => {
    const n = document.querySelector('nav[aria-label="Project sections"]');
    return n && n.scrollWidth <= n.clientWidth + 1;
  });
  let settled = "";
  for (let i = 0; i < 20; i++) {
    hidden.length = 0;
    for (const l of labels) if (!(await nav.getByRole("link", { name: l, exact: true }).isVisible())) hidden.push(l);
    const now = hidden.join(",");
    if (now === settled && (width >= 700 || hidden.length > 0)) break;
    settled = now;
    await page.waitForTimeout(250);
  }
  if (width < 700) {
    ok(hidden.length > 0 && (await nav.getByRole("button", { name: "More" }).isVisible()), `${tag} narrow screens get a More menu (${hidden.length} in it)`);
    ok(
      await nav.evaluate((n) => n.scrollWidth <= n.clientWidth + 1),
      `${tag} the tab strip does not clip`,
    );
    await nav.getByRole("button", { name: "More" }).click();
    for (const l of hidden) ok(await page.getByRole("menuitem", { name: l, exact: true }).isVisible(), `${tag} ${l} is in More`);
    await shot("tabs-more");
    await page.getByRole("menuitem", { name: hidden.at(-1), exact: true }).click();
    await page.waitForURL(new RegExp(`/p/${a}/`));
    ok(await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: hidden.at(-1), exact: true }).isVisible(), `${tag} the current tab stays in the strip`);
  } else {
    ok(hidden.length === 0, `${tag} every tab fits at desktop width`);
  }
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "dark"]], [0, 1]))
  try { await run(w, s); } catch (e) { fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`); }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
