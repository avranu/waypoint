// Relink a linked repo (WP-154), in a real browser at desktop and phone
// widths: Project settings -> Integrations -> the repo's Relink button asks
// for the new owner/name and points the attachment at it, keeping the project.
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
const KEY = "RLK";
const IDS = { "me/old": 71, "me/new": 72, "me/newer": 73 };

// A fake GitHub that knows three repos.
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
    if (rest === "") return IDS[m[1]] ? send(200, { id: IDS[m[1]], full_name: m[1], allow_squash_merge: true }) : send(404, { message: "Not Found" });
    if (rest.startsWith("hooks")) return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
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

let api;
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  await api("POST", "/projects", { name: "Relink", key: KEY });
  const forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub relink", base_url: fake, token: "tok", webhook_secret: "s" });
  await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: "me/old", sync_issues: false });
}

async function look(width, target) {
  const tag = `${width}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);
  const current = (await api("GET", `/projects/${KEY}/repos`))[0].repo;
  await page.goto(`${base}/p/${KEY}/settings?tab=integrations`);
  await page.getByText(current).first().waitFor();
  await page.getByRole("button", { name: new RegExp(`Relink ${current}`) }).click();
  const dlg = page.getByRole("dialog");
  await dlg.getByLabel("New repository").fill(target);
  await page.screenshot({ path: `${out}/relink-${width}.png`, fullPage: true });
  await overflow("dialog");
  await dlg.getByRole("button", { name: "Relink" }).click();
  await page.getByText(target, { exact: true }).first().waitFor();
  const repos = await api("GET", `/projects/${KEY}/repos`);
  ok(repos.length === 1 && repos[0].repo === target, `${tag} relinked to ${target}: ${JSON.stringify(repos.map((r) => r.repo))}`);
  await page.screenshot({ path: `${out}/relinked-${width}.png`, fullPage: true });
  await overflow("after");
  await ctx.close();
}

try {
  for (const [w, target] of looks([[1280, "me/new"], [390, "me/newer"]])) await look(w, target);
  // The forge doesn't have it: the error is shown and nothing changes.
  const bad = await api("POST", `/projects/${KEY}/repos/${(await api("GET", `/projects/${KEY}/repos`))[0].id}/move`, { repo: "me/missing", relink: true });
  ok(/couldn't find/.test(JSON.stringify(bad)), `a missing repo is refused: ${JSON.stringify(bad)}`);
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
srv.close();
console.log(errors.length ? `${errors.length} failures` : "all ok");
process.exit(errors.length ? 1 : 0);
