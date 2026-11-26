// Agent setup (WP-208, WP-ADR44), in a real browser at desktop and phone
// widths: four repos on a fake Forgejo, one in a project that opted out.
// Waypoint reads their agent files; Agents → Setup shows them at a glance,
// what is missing and what only one has; an admin writes the shared
// instructions and publishes them (a pull request on each repo without them,
// the opted-out one left alone); a file opens, steps to the next, and an
// edit opens a pull request; two machines report, one without wp's hooks.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const HEAD = "5c1d7e0f2a9b8c3d4e5f60718293a4b5c6d7e8f9";

const reviewer = "---\nname: reviewer\ndescription: Reviews a diff\n---\nReview the diff.\n";
const repos = {
  "setco/app": {
    "CLAUDE.md": "# App\n\nRun make check before you ship.\n",
    ".claude/agents/reviewer.md": reviewer,
    ".claude/skills/tidy/SKILL.md": "---\nname: tidy\ndescription: Tidy branches\n---\n",
    ".claude/settings.json": JSON.stringify({ permissions: { allow: ["Bash(make:*)"] } }),
    "src/main.go": "package main\n",
  },
  "setco/web": {
    "CLAUDE.md": "# Web\n",
    "docs/CLAUDE.md": "# Docs\n\nWrite plainly.\n",
    ".claude/agents/reviewer.md": reviewer,
  },
  "setco/lib": {
    "CLAUDE.md": "@AGENTS.md\n",
    "AGENTS.md": "# Lib\n",
    ".claude/agents/reviewer.md": reviewer,
    ".claude/agents/lib-only.md": "---\nname: lib-only\n---\n",
  },
  "setco/api": { "CLAUDE.md": "# API\n" },
};
// git's blob id, as Forgejo's contents call answers it.
const blob = (repo, p) => {
  const b = Buffer.from(repos[repo][p]);
  return crypto.createHash("sha1").update(Buffer.concat([Buffer.from(`blob ${b.length}\0`), b])).digest("hex");
};
const commits = [];
const pulls = [];

const srv = http.createServer(async (req, res) => {
  const send = (code, body, type = "application/json") => {
    res.writeHead(code, { "Content-Type": type });
    res.end(body === undefined ? "" : typeof body === "string" ? body : JSON.stringify(body));
  };
  let raw = "";
  for await (const chunk of req) raw += chunk;
  const u = new URL(req.url, "http://x");
  const m = u.pathname.match(/^\/api\/v1\/repos\/([^/]+\/[^/]+)(\/.*)?$/);
  if (!m) return send(404, { message: "Not Found" });
  const [, repo, rest = ""] = m;
  if (rest.startsWith("/hooks")) return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
  const files = repos[repo];
  if (!files) return rest === "" ? send(200, { default_branch: "main" }) : send(404, { message: "Not Found" });
  if (rest === "") return send(200, { default_branch: "main" });
  if (rest === "/branches/main") return send(200, { commit: { id: HEAD } });
  if (rest.startsWith("/git/trees/"))
    return send(200, { tree: Object.keys(files).map((path) => ({ path, type: "blob" })), truncated: false });
  if (rest.startsWith("/raw/")) {
    const p = decodeURIComponent(rest.slice(5));
    return p in files ? send(200, files[p], "text/plain") : send(404, { message: "Not Found" });
  }
  if (rest === "/contents" && req.method === "POST") {
    commits.push({ repo, ...JSON.parse(raw) });
    return send(201, { commit: { sha: crypto.randomBytes(20).toString("hex") } });
  }
  if (rest.startsWith("/contents/")) {
    const p = decodeURIComponent(rest.slice(10));
    return p in files ? send(200, { sha: blob(repo, p), type: "file" }) : send(404, { message: "Not Found" });
  }
  if (rest.startsWith("/labels"))
    return req.method === "GET" ? send(200, []) : send(201, { id: 7, name: JSON.parse(raw).name });
  if (rest === "/pulls" && req.method === "POST") {
    const b = JSON.parse(raw);
    const number = 100 + pulls.length;
    pulls.push({ repo, number, ...b });
    return send(201, {
      number, title: b.title, body: b.body, state: "open",
      html_url: `${fake}/${repo}/pulls/${number}`, head: { ref: b.head, sha: HEAD }, base: { ref: b.base },
      labels: [{ id: 7, name: "automerge" }], user: { login: "waypoint" },
    });
  }
  if (rest.startsWith("/pulls")) return req.method === "GET" ? send(200, []) : send(200, {});
  if (rest.startsWith("/branches/") && req.method === "DELETE") return send(204);
  return send(404, { message: "Not Found" });
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
const apiFor = (page) => (m, p, d) =>
  page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then(async (r) => {
    if (!r.ok()) throw new Error(`${m} ${p}: ${r.status()} ${await r.text()}`);
    return r.status() === 204 || r.status() === 202 ? null : r.json();
  });
const mine = (s) => s.repos.filter((r) => r.repo.startsWith("setco/"));
const repoRow = (page, name) =>
  page.locator("li").filter({ has: page.getByText(name, { exact: true }) }).first();

// ---- set up: a forge, two projects (one opted out), four repos, read ----
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  const api = apiFor(page);
  await api("POST", "/projects", { name: "Setup", key: "SETUP" });
  await api("POST", "/projects", { name: "Setup Lib", key: "SETLIB" });
  await api("PATCH", "/projects/SETLIB", { settings: { no_shared_setup: true } });
  const forge = await api("POST", "/forges", { kind: "forgejo", name: "Fake Forgejo setup", base_url: fake, token: "tok" });
  for (const r of ["setco/app", "setco/web", "setco/api"])
    await api("POST", "/projects/SETUP/repos", { forge_id: forge.id, repo: r, sync_issues: false });
  await api("POST", "/projects/SETLIB/repos", { forge_id: forge.id, repo: "setco/lib", sync_issues: false });

  await api("POST", "/agent-setup/refresh");
  let s;
  for (let i = 0; i < 100; i++) {
    s = await api("GET", "/agent-setup");
    if (mine(s).length === 4 && mine(s).every((r) => r.head)) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  const by = Object.fromEntries(mine(s).map((r) => [r.repo, r]));
  ok(mine(s).every((r) => r.head === HEAD), `every repo read: ${mine(s).map((r) => r.repo + "@" + r.head + " " + (r.error || ""))}`);
  ok(
    JSON.stringify(by["setco/app"].files.map((f) => f.path)) ===
      JSON.stringify(["CLAUDE.md", ".claude/agents/reviewer.md", ".claude/settings.json", ".claude/skills/tidy/SKILL.md"]),
    `app's agent files, not its code: ${by["setco/app"].files.map((f) => f.path)}`,
  );
  ok(by["setco/web"].files.some((f) => f.path === "docs/CLAUDE.md"), "a subfolder's CLAUDE.md is read");
  ok(by["setco/api"].missing.some((c) => c.kind === "agent" && c.name === "reviewer"), "api misses the reviewer most have");
  ok(by["setco/lib"].opted_out && by["setco/lib"].root_file === "AGENTS.md", `lib opted out, its root file AGENTS.md: ${by["setco/lib"].root_file}`);
  ok(mine(s).every((r) => r.shared === "none"), "no shared text yet");

  const report = (machine, hooks) =>
    api("POST", "/agent-setup/machines", {
      machine, os: "linux/amd64", wp_version: "0.25.1",
      files: [
        { path: ".claude/CLAUDE.md", content: "# Me\n\nAnswer briefly.\n" },
        { path: ".claude/settings.json", content: '{\n  "env": {\n    "OPENAI_API_KEY": "sk-live-test-123"\n  }\n}\n' },
        { path: ".claude/agents/planner.md", content: "---\nname: planner\n---\n" },
      ],
      checks: [hooks ? { key: "hooks", label: "wp hooks installed", state: "ok" } : { key: "hooks", label: "wp hooks installed", state: "fail", note: "not installed: run wp setup" }],
    });
  await report("devhost", true);
  await report("laptop", false);
  const f = await api("GET", "/agent-setup/machine-file?" + new URLSearchParams({ user: (await api("GET", "/agent-setup")).machines[0].user.id, machine: "devhost", path: ".claude/settings.json" }));
  ok(!f.content.includes("sk-") && f.content.includes("OPENAI_API_KEY"), "a machine's settings keep names, not values");
  await ctx.close();
}

// ---- Agents → Setup at desktop and phone widths ----
for (const [width, scheme] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  const tag = `${width}`;
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(`${base}/agents?tab=setup`);
  const glance = page.getByLabel("At a glance");
  await glance.waitFor();
  ok(await glance.getByText("1 needs fixing", { exact: true }).last().isVisible(), `${tag} the laptop needs fixing`);
  ok(await page.getByText(/wp hooks installed: not installed: run wp setup/).first().isVisible(), `${tag} says why`);
  const api = repoRow(page, "setco/api");
  ok(await api.getByText("Missing reviewer (agent)").isVisible(), `${tag} api misses the reviewer`);
  const lib = repoRow(page, "setco/lib");
  ok(await lib.getByText("Only here: lib-only (agent)").isVisible(), `${tag} lib's own agent`);
  ok(await lib.getByText("opted out").isVisible(), `${tag} lib is opted out`);

  if (width === 1280) {
    ok(await page.getByText("No shared instructions yet.").isVisible(), "no shared instructions yet");
    await page.getByRole("button", { name: "Edit", exact: true }).click();
    await page.getByRole("textbox", { name: "Shared instructions" }).fill("- Ship with wp ship.\n- Never sleep-poll CI.");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await page.getByText(/Saved version 1/).waitFor();
    await page.getByText("Version 1, ").waitFor();
    ok(await page.getByText("4 missing").isVisible(), "four repos miss it");
    await page.getByRole("button", { name: "Publish", exact: true }).click();
    const dlg = page.getByRole("dialog");
    ok(await dlg.getByText(/on 3 repos without it.*1 opted out is left alone/).isVisible(), "publish says what it does");
    await dlg.getByRole("button", { name: "Publish" }).click();
    await page.getByText(/Opening 3 pull requests; 1 opted out/).waitFor();
    for (let i = 0; i < 50 && pulls.length < 3; i++) await new Promise((r) => setTimeout(r, 100));
    ok(
      JSON.stringify(commits.map((c) => c.repo).sort()) === JSON.stringify(["setco/api", "setco/app", "setco/web"]),
      `a commit on each repo but lib: ${commits.map((c) => c.repo)}`,
    );
    const app = commits.find((c) => c.repo === "setco/app");
    const body = app && Buffer.from(app.files[0].content, "base64").toString();
    ok(
      app?.files[0].path === "CLAUDE.md" && app.files[0].operation === "update" && app.files[0].sha === blob("setco/app", "CLAUDE.md") &&
        body.startsWith("# App\n\nRun make check before you ship.\n") && body.includes("<!-- waypoint:shared") &&
        body.includes("- Never sleep-poll CI.") && body.includes("<!-- /waypoint:shared -->"),
      `app's CLAUDE.md keeps its own text and gains the block:\n${body}`,
    );
    ok(pulls.length === 3 && pulls.every((p) => p.labels?.includes(7) && /^waypoint\/shared-v1-/.test(p.head)), `three pull requests, labelled to merge: ${JSON.stringify(pulls.map((p) => [p.repo, p.head, p.labels]))}`);
    await repoRow(page, "setco/app").getByText(/#\d+ open/).first().waitFor();
    ok(true, "the repo row links its pull request");
  }

  await repoRow(page, "setco/app").locator("button[aria-expanded]").click();
  await page.getByRole("button", { name: /^CLAUDE\.md/ }).first().click();
  const dlg = page.getByRole("dialog");
  await dlg.getByText("Run make check before you ship.").waitFor();
  ok(await dlg.getByText("1 / 4").isVisible(), `${tag} counts the files`);
  await dlg.getByRole("button", { name: "Next file" }).click();
  await dlg.getByText("2 / 4").waitFor();
  await dlg.getByText("name: reviewer").waitFor();
  ok(true, `${tag} next steps to the next file`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll in the dialog`);
  await page.screenshot({ path: `${out}/setup-file-${width}.png` });
  if (width === 1280) {
    await dlg.getByRole("button", { name: "Edit" }).click();
    await dlg.getByRole("textbox", { name: ".claude/agents/reviewer.md" }).fill(reviewer + "Look for missing tests too.\n");
    await dlg.getByRole("textbox", { name: "What changed" }).fill("Reviewer looks for missing tests");
    const before = commits.length;
    await dlg.getByRole("button", { name: "Open pull request" }).click();
    await page.getByText(/Opened pull request #\d+/).waitFor();
    const c = commits[before];
    ok(
      c?.repo === "setco/app" && c.files[0].path === ".claude/agents/reviewer.md" &&
        Buffer.from(c.files[0].content, "base64").toString().endsWith("Look for missing tests too.\n") &&
        pulls.at(-1).title === "Reviewer looks for missing tests",
      `the edit is a commit and a pull request: ${JSON.stringify(c?.files?.map((f) => f.path))} ${pulls.at(-1)?.title}`,
    );
  }
  await page.keyboard.press("Escape");
  await dlg.waitFor({ state: "hidden" });

  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await page.screenshot({ path: `${out}/setup-${width}.png`, fullPage: true });
  await ctx.close();
}

await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
