// References and search by code (WP-122) in a real browser at desktop and
// phone widths. An agent's request (RF:R-1) is named in a task's description
// and a comment in several spellings; the text links what exists (the
// request, the decision, the other task) and leaves what does not as plain
// text. Searching R-1, R1 or RF:R-1 shows the request first as the best
// match, then the task and comment that write it, and a request is
// searchable by its words.
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
const KEY = "RF";

const browser = await chromium.launch();
let task;

async function seed(api, call) {
  if ((await api("GET", "/projects")).some((p) => p.key === KEY)) return;
  await api("POST", "/projects", { name: "References", key: KEY });
  const { token } = await api("POST", "/me/tokens", { name: "refs agent", agent: "anthropic/claude-code" });
  const agent = { ...H, Authorization: `Bearer ${token}`, "X-Waypoint-Machine": "box" };
  const db = new (await import("node:sqlite")).DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const forge = db.prepare(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo', 'Forge', 'http://127.0.0.1:1', ?) RETURNING id`).get(Date.now()).id;
  const project = db.prepare("SELECT id FROM projects WHERE key = ?").get(KEY).id;
  db.prepare("INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'refs/app', ?)").run(project, forge, Date.now());
  db.close();
  await call("POST", "/sessions/hook", { events: [{ event: "SessionStart", session_id: "refs-session", machine: "box", cwd: "/work/refs", repo: "refs/app", branch: "main" }] }, agent);
  const r = await call("POST", "/requests", { kind: "action", title: "Deploy Quarry 0.3.22 to production", body: "Auto mode blocked me; the exact command is here." }, { ...agent, "X-Waypoint-Session": "refs-session" });
  if (!r.ok()) throw new Error(`ask: ${r.status()} ${await r.text()}`);
  await api("POST", "/design", { title: "Use sqlite", kind: "decision", project: KEY, body: "Because." });
  await api("POST", "/projects", { name: "Second", key: "RG" });
  await api("POST", "/design", { title: "Use postgres", kind: "decision", project: "RG", body: "Because." });
  await api("POST", "/issues", { project: KEY, title: "ADR10 follow up" });
  const other = await api("POST", "/issues", { project: KEY, title: "Release notes" });
  task = await api("POST", "/issues", {
    project: KEY,
    title: "Follow up on the deploy",
    description: `RF:R-1: deploy Quarry. Also R-1 and R-99 (nothing), see ${other.key}, ADR1, ZZ-5.`,
  });
  await api("POST", `/issues/${task.key}/comments`, { body: "Still waiting on R1 before the release." });
  await api("POST", `/issues/${task.key}/comments`, { body: "Second note: ADR1 and RF:R-1 again." });
  await api("POST", `/issues/${task.key}/comments`, { body: "Third note: nothing to see, ZZ-5." });
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text())) fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  };
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"));
  await seed(api, call);

  let refCalls = 0;
  page.on("request", (r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/v1/refs" && refCalls++);
  await page.goto(base + `/issue/${task.key}`);
  const desc = page.locator(".prose-wp").first();
  await desc.getByText("Also", { exact: false }).waitFor();
  await page.waitForFunction(() => document.querySelectorAll(".prose-wp a.ref").length >= 3);
  const hrefs = await desc.locator("a.ref").evaluateAll((as) => as.map((a) => [a.textContent, a.getAttribute("href")]));
  const byText = Object.fromEntries(hrefs);
  ok(byText["RF:R-1"] === "/requests/RF:R-1", `${tag} RF:R-1 links the request (${JSON.stringify(hrefs)})`);
  ok(byText["R-1"] === "/requests/RF:R-1", `${tag} a bare R-1 resolves in the task's own project`);
  ok(byText["ADR1"]?.startsWith("/design/"), `${tag} ADR1 links the decision`);
  ok(Object.keys(byText).some((k) => /^RF-\d+$/.test(k)), `${tag} the other task is linked`);
  ok(!("R-99" in byText) && !("ZZ-5" in byText), `${tag} codes nothing has stay plain text`);
  await page.getByText("Still waiting on").waitFor();
  const note = await page.locator("a.ref", { hasText: /^R1$/ }).first().getAttribute("href");
  ok(note === "/requests/RF:R-1", `${tag} R1 in a comment links the request (${note})`);
  await page.getByText("Third note").waitFor();
  await page.waitForLoadState("networkidle");
  ok(refCalls === 2, `${tag} /refs is called once for the description and once for all comments (${refCalls})`);
  await shot("task");
  await overflow("task");
  await desc.locator("a.ref", { hasText: "RF:R-1" }).first().click();
  await page.getByRole("heading", { name: "Deploy Quarry 0.3.22 to production" }).waitFor();
  ok(true, `${tag} the link opens the request`);

  for (const q of ["R-1", "R1", "RF:R-1"]) {
    await page.goto(base + `/search?q=${encodeURIComponent(q)}`);
    const best = page.locator("section", { hasText: "Best match" }).first();
    await best.waitFor();
    ok(await best.getByText("Deploy Quarry 0.3.22 to production").isVisible(), `${tag} ${q}: the request is the best match`);
    await page.getByText("Follow up on the deploy").first().waitFor();
    ok(await page.getByText("mentions it").first().isVisible() || (await page.getByText("Follow up on the deploy").count()) > 0, `${tag} ${q}: the task that writes it follows`);
    if (q === "R-1") {
      await shot("search");
      await overflow("search");
    }
  }
  for (const q of ["ADR1", "ADR-0001"]) {
    await page.goto(base + `/search?q=${encodeURIComponent(q)}`);
    const heads = page.locator("section h2");
    await heads.first().waitFor();
    ok((await heads.first().innerText()).includes("Best match"), `${tag} ${q}: best match leads`);
    ok((await heads.nth(1).innerText()).includes("Design notes"), `${tag} ${q}: decisions are the first group`);
    ok(await page.locator("section").first().getByText("Use sqlite").isVisible(), `${tag} ${q}: the decision is the best match`);
    if (q === "ADR1") {
      await shot("search-adr");
      await overflow("search adr");
    }
  }
  for (const [proj, first] of [["RG", "Use postgres"], ["RF", "Use sqlite"]]) {
    await page.goto(base + `/p/${proj}/board`);
    await page.waitForLoadState("networkidle");
    await page.keyboard.press("Control+k");
    const input = page.getByPlaceholder(/Search issues/);
    await input.fill("ADR1");
    const rows = page.locator("[data-idx]");
    await page.getByText("Matches", { exact: true }).waitFor();
    await page.waitForFunction(() => document.querySelectorAll("[data-idx]").length >= 2);
    ok((await rows.first().innerText()).includes(first), `${tag} palette in ${proj}: ${first} comes first (${(await rows.first().innerText()).replace(/\n/g, " ")})`);
    const texts = await rows.allInnerTexts();
    ok(texts.filter((t) => t.includes("Use sqlite")).length === 1, `${tag} palette in ${proj}: no duplicates`);
    if (proj === "RG") await shot("palette-adr");
    await page.keyboard.press("Escape");
  }
  const only = await api("GET", "/search?q=" + encodeURIComponent("RF:R-1"));
  ok(only.issues?.some((i) => i.key === task.key), `${tag} issues-only search reads RF:R-1 as a code`);
  const words = await api("GET", "/search/all?q=production&kinds=requests");
  ok(words.groups[0]?.hits?.[0]?.key === "RF:R-1", `${tag} a request is found by its words`);
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
