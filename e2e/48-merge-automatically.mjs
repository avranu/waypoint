// Merge automatically (WP-133), in a real browser at desktop and phone
// widths, light and dark: a repo's Merge automatically switch (on by
// default) and its folded Merge rules in Project settings → Integrations,
// saved through the API; a pull request the setting armed, whose sheet says
// so and what it waits for (the reviewers, until when). Which base branches
// it covers (WP-1041): any, the default, all but the default, or matching
// globs (release/** offered first).
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
const SECRET = "e2e-auto-secret";
const REPO = "me/auto";
const KEY = "AUTO";

const pr = {
  number: 4,
  title: "Tidy the settings page",
  body: "",
  html_url: "",
  state: "open",
  merged: false,
  draft: false,
  mergeable: true,
  user: { login: "robin" },
  head: { ref: "tidy", sha: "4444444dddd", repo: { full_name: REPO } },
  base: { ref: "main" },
  labels: [],
  created_at: new Date(Date.now() - 600e3).toISOString(),
};

// A fake GitHub: webhooks, the pull request, no checks yet.
const srv = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => {
    const send = (code, body) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(body === undefined ? "" : JSON.stringify(body));
    };
    const m = req.url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)(?:\/(.*))?$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2] ?? "";
    if (rest === "") return send(200, { allow_squash_merge: true, default_branch: "main" });
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, []);
    if (rest === "pulls/4") return send(200, pr);
    if (rest.match(/^commits\/[^/]+\/status/)) return send(200, { statuses: [] });
    if (rest.startsWith("commits/") && rest.includes("check-runs")) return send(200, { check_runs: [] });
    if (rest.startsWith("statuses/")) return send(201, {});
    if (rest.startsWith("issues/4/")) return req.method === "POST" ? send(201, { id: 9 }) : send(200, []);
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
pr.html_url = `${fake}/${REPO}/pull/4`;
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

let api, forge, repoID, prID;
// ---- set up: a project with a repo ----
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  await api("POST", "/projects", { name: "Auto", key: KEY });
  forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub auto", base_url: fake, token: "tok", webhook_secret: SECRET });
  await api("PUT", `/forges/${forge.id}/people`, { people: [{ login: "robin", user: "robin" }] });
  repoID = (await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false })).id;
  const repos = await api("GET", `/projects/${KEY}/repos`);
  ok(repos[0].auto_merge === true && repos[0].review_wait_minutes === 30 && repos[0].quiet_minutes === 10, `on by default: ${JSON.stringify(repos[0])}`);
}

// ---- the switch and the rules, at each width and theme ----
async function look(width, scheme, edit) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(`${base}/p/${KEY}/settings?tab=integrations`);
  await page.getByText(REPO).first().waitFor();
  const sw = page.getByRole("switch", { name: "Merge this repo's pull requests automatically" });
  const rules = page.getByRole("button", { name: "Merge rules" });
  ok(await rules.isVisible(), `${tag} Merge rules is there, folded`);
  ok((await rules.getAttribute("aria-expanded")) === "false", `${tag} folded by default`);
  await rules.click();
  const checks = page.getByRole("textbox", { name: "Required checks" });
  await checks.waitFor();
  ok(await page.getByLabel("Review wait").isVisible(), `${tag} review wait shown`);
  if (edit) {
    ok((await sw.getAttribute("aria-checked")) === "true", "Merge automatically is on by default");
    await checks.fill("local-ci");
    await checks.press("Enter");
    const reviewers = page.getByRole("textbox", { name: "External reviewers" });
    await reviewers.fill("llm-review");
    await reviewers.press("Enter");
    const waitBox = page.getByLabel("Review wait");
    await waitBox.fill("45");
    await waitBox.press("Enter");
    await page.getByLabel("Quiet").fill("5");
    await page.getByLabel("Quiet").blur();
    let repo;
    for (let i = 0; i < 40; i++) {
      repo = (await api("GET", `/projects/${KEY}/repos`))[0];
      if (repo.quiet_minutes === 5 && repo.review_wait_minutes === 45 && repo.external_reviewers.length === 1) break;
      await page.waitForTimeout(150);
    }
    ok(
      repo.required_checks.join() === "local-ci" && repo.external_reviewers.join() === "llm-review" && repo.review_wait_minutes === 45 && repo.quiet_minutes === 5,
      `rules saved: ${JSON.stringify(repo)}`,
    );
    await page.getByLabel("Quiet").fill("5000");
    await page.getByLabel("Quiet").blur();
    ok(await page.getByText("From 0 to 1440 minutes").isVisible(), "an out-of-range quiet says so");
    await page.getByLabel("Quiet").fill("5");
    await page.getByLabel("Quiet").blur();

    const into = page.getByRole("combobox", { name: "Branches Merge automatically covers" });
    const choose = async (name) => {
      await into.click();
      await page.getByRole("option", { name, exact: true }).click();
    };
    ok((await into.textContent()).includes("Any branch"), `${tag} into any branch by default`);
    const saved = async (want) => {
      for (let i = 0; i < 40; i++) {
        repo = (await api("GET", `/projects/${KEY}/repos`))[0];
        if (want(repo)) return true;
        await page.waitForTimeout(150);
      }
      return false;
    };
    await choose("All but the default branch");
    ok(await saved((r) => r.auto_merge_into === "others" && r.default_branch === "main"), `${tag} all but the default saved: ${JSON.stringify(repo)}`);
    await page.reload();
    await into.waitFor();
    ok((await into.textContent()).includes("All but main"), `${tag} it names the default branch`);
    await choose("Branches matching…");
    ok(await saved((r) => r.auto_merge_into === "match" && r.auto_merge_bases.join() === "release/**"), `${tag} matching starts from release/**: ${JSON.stringify(repo)}`);
    const bases = page.getByRole("textbox", { name: "Branches Merge automatically covers" });
    await bases.waitFor();
    ok(await page.getByText("release/**", { exact: true }).isVisible(), `${tag} the release/** chip shows`);
    await bases.fill("hotfix/*");
    await bases.press("Enter");
    ok(await saved((r) => r.auto_merge_bases.join() === "release/**,hotfix/*"), `${tag} a pattern added: ${JSON.stringify(repo)}`);
    await shot("integrations-into");
    await overflow("integrations, matching");
    await choose("Any branch");
    ok(await saved((r) => r.auto_merge_into === "any"), `${tag} back to any branch`);
  }
  await shot("integrations");
  await overflow("integrations");

  if (!prID) return ctx.close();
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  await sheet.getByText("Not ready to merge").waitFor();
  ok(await sheet.getByText("set by the repo’s Merge automatically").isVisible(), `${tag} the sheet says the setting armed it`);
  ok(await sheet.getByText(/waiting for .*llm-review on 4444444 until/).isVisible(), `${tag} the sheet says what it waits for and until when`);
  await shot("pull");
  await overflow("pull");
  await ctx.close();
}

try {
  await look(1280, "light", true);
  const body = JSON.stringify({ action: "opened", pull_request: pr, sender: { login: "robin" }, repository: { full_name: REPO, owner: { type: "User" } } });
  const sig = "sha256=" + crypto.createHmac("sha256", SECRET).update(body).digest("hex");
  const r = await fetch(`${base}/api/v1/hooks/forge/${forge.id}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-GitHub-Event": "pull_request", "X-Hub-Signature-256": sig },
    body,
  });
  ok(r.ok, `webhook accepted (${r.status})`);
  for (let i = 0; i < 40 && !prID; i++) {
    const list = await api("GET", `/pulls?project=${KEY}`);
    if (list.pulls?.[0]?.armed) prID = list.pulls[0].id;
    else await new Promise((res) => setTimeout(res, 150));
  }
  ok(!!prID, "the setting armed the new pull request");
  for (const [w, s] of looks([[1280, "light"], [1280, "dark"], [390, "light"], [390, "dark"]])) await look(w, s, false);

  await api("PATCH", `/projects/${KEY}/repos/${repoID}`, { auto_merge: false });
  let armed = true;
  for (let i = 0; i < 40 && armed; i++) {
    armed = (await api("GET", `/pulls?project=${KEY}`)).pulls[0].armed;
    if (armed) await new Promise((res) => setTimeout(res, 150));
  }
  ok(!armed, "turning it off disarms what it armed");

  await api("PATCH", `/projects/${KEY}/repos/${repoID}`, { auto_merge: true, auto_merge_into: "match", auto_merge_bases: ["release/**"] });
  for (const [w, s] of looks([[390, "light"], [390, "dark"], [1280, "dark"]])) {
    const tag = `${w}/${s} matching`;
    const ctx = await browser.newContext({ viewport: { width: w, height: 900 }, colorScheme: s });
    const page = await signIn(ctx);
    page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
    await page.goto(`${base}/p/${KEY}/settings?tab=integrations`);
    await page.getByRole("textbox", { name: "Branches Merge automatically covers" }).waitFor();
    ok(await page.getByText("release/**", { exact: true }).isVisible(), `${tag} the pattern shows`);
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
    await page.screenshot({ path: `${out}/integrations-matching-${w}-${s}.png`, fullPage: true });
    await ctx.close();
  }
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
srv.close();
console.log(errors.length ? `${errors.length} failures` : "all ok");
process.exit(errors.length ? 1 : 0);
