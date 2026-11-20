// Merged and stale branches on the forge (WP-140), in a real browser at
// desktop and phone widths, light and dark: a repo's Delete merged branches
// switch (on by default) in Project settings → Integrations, saved through
// the API, and the Agents overview's "Branches on the forge to tidy" listing
// what the sweep deletes and what it calls stale, read from a fake GitHub.
// Run after 01-signup-flow (user robin, an admin).
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
const REPO = "me/land";
const KEY = "LAND";
const day = 24 * 3600e3;
const branches = [
  { name: "main", sha: "a0", age: 1 },
  { name: "wp-130-old-merged", sha: "b1", age: 5 },
  { name: "wp-99-abandoned", sha: "c2", age: 45 },
  { name: "wp-141-in-work", sha: "d3", age: 1 },
];

// A fake GitHub: the repo, its branches, which are in main, and when they were made.
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
    if (rest === "") return send(200, { default_branch: "main" });
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("branches?")) return send(200, branches.map((b) => ({ name: b.name, protected: false, commit: { sha: b.sha } })));
    const cmp = rest.match(/^compare\/main\.\.\.(.+)\?/);
    if (cmp) return send(200, { ahead_by: cmp[1] === "b1" || cmp[1] === "a0" ? 0 : 2 });
    const c = rest.match(/^git\/commits\/(.+)$/);
    if (c) {
      const b = branches.find((x) => x.sha === c[1]);
      return send(200, { committer: { date: new Date(Date.now() - (b?.age ?? 1) * day).toISOString() } });
    }
    if (rest.startsWith("pulls")) return send(200, []);
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

let api, repoID;
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  await api("POST", "/projects", { name: "Land", key: KEY });
  const forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub land", base_url: fake, token: "tok", webhook_secret: "e2e-land" });
  repoID = (await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false })).id;
  const repos = await api("GET", `/projects/${KEY}/repos`);
  ok(repos[0].delete_merged_branches === true, "deleting merged branches is on by default");
}

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
  const sw = page.getByRole("switch", { name: "Delete this repo's merged branches on the forge" });
  await sw.waitFor();
  ok((await sw.getAttribute("aria-checked")) === "true", `${tag} the switch is on`);
  if (edit) {
    await sw.click();
    let repo;
    for (let i = 0; i < 40; i++) {
      repo = (await api("GET", `/projects/${KEY}/repos`))[0];
      if (repo.delete_merged_branches === false) break;
      await page.waitForTimeout(150);
    }
    ok(repo.delete_merged_branches === false, "turning it off is saved");
    await sw.click();
    for (let i = 0; i < 40; i++) {
      repo = (await api("GET", `/projects/${KEY}/repos`))[0];
      if (repo.delete_merged_branches === true) break;
      await page.waitForTimeout(150);
    }
    ok(repo.delete_merged_branches === true, "and on again");
  }
  await shot("integrations");
  await overflow("integrations");

  await page.goto(`${base}/agents`);
  const fold = page.getByRole("region", { name: "Branches on the forge to tidy" });
  await fold.waitFor();
  ok(await fold.getByText("wp-130-old-merged").isVisible(), `${tag} the merged branch is listed`);
  ok(await fold.getByText("Deleted in the next sweep").isVisible(), `${tag} and says it is deleted`);
  ok(await fold.getByText("wp-99-abandoned").isVisible(), `${tag} the stale branch is listed`);
  const text = await fold.innerText();
  ok(/no commit for 4[45] days/.test(text), `${tag} with why: ${text.replace(/\n/g, " | ")}`);
  ok(!(await fold.getByText("wp-141-in-work").isVisible()), `${tag} a branch in work is not`);
  await fold.screenshot({ path: `${out}/tidy-${width}-${scheme}.png` });
  await overflow("overview");
  await ctx.close();
}

try {
  for (const [w, s, e] of looks([[1280, "light", true], [1280, "dark", false], [390, "light", false], [390, "dark", false]])) await look(w, s, e);
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
srv.close();
console.log(errors.length ? `${errors.length} failures` : "all ok");
process.exit(errors.length ? 1 : 0);
