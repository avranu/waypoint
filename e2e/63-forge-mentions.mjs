// Forge mentions (WP-379), in a real browser at desktop and phone widths:
// a pull request from a fake GitHub links the issue its branch names and
// the one it "Closes", and only records the one it is a "follow-up to"
// and the one a comment names. The issue page shows the mention as a chip
// under "On the forge", and the pull request's sheet lists it as
// "Mentioned, not linked".
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
const SECRET = "e2e-mentions-secret";
const REPO = "me/notes";

const pr = {
  number: 4,
  title: "Notes sync keeps order",
  body: "",
  html_url: "",
  state: "open",
  merged: false,
  draft: false,
  user: { login: "robin" },
  head: { ref: "", sha: "4444444dddd" },
  base: { ref: "main" },
  labels: [],
  created_at: new Date(Date.now() - 3600e3).toISOString(),
};

const srv = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => {
    const send = (code, body) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(body === undefined ? "" : JSON.stringify(body));
    };
    const m = req.url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)\/(.*)$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2];
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, [pr]);
    if (rest === "pulls/4" || rest.startsWith("pulls/4?") || rest === "issues/4") return send(200, pr);
    if (/^(pulls|issues)\/4\//.test(rest)) return send(200, []);
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

let own, closes, follows, commented, prID;
{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  const key = "FMN";
  await api("POST", "/projects", { name: "Notes", key });
  const forge = await api("POST", "/forges", {
    kind: "github", name: "Fake GitHub mentions", base_url: fake, token: "tok", webhook_secret: SECRET,
  });
  await api("POST", `/projects/${key}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false });
  await api("PUT", `/forges/${forge.id}/people`, { people: [{ login: "robin", user: "robin" }, { login: "alex", user: "robin" }] });
  const hook = async (event, payload) => {
    const body = JSON.stringify({ ...payload, repository: { full_name: REPO, owner: { type: "User" } } });
    const sig = "sha256=" + crypto.createHmac("sha256", SECRET).update(body).digest("hex");
    const r = await page.request.post(`${base}/api/v1/hooks/forge/${forge.id}`, {
      headers: { "Content-Type": "application/json", "X-GitHub-Event": event, "X-Hub-Signature-256": sig },
      data: body,
    });
    ok(r.ok(), `webhook ${event} accepted (${r.status()})`);
  };
  own = (await api("POST", "/issues", { project: key, title: "Sync keeps note order" })).key;
  closes = (await api("POST", "/issues", { project: key, title: "Notes shuffle on reload" })).key;
  follows = (await api("POST", "/issues", { project: key, title: "First sync design" })).key;
  commented = (await api("POST", "/issues", { project: key, title: "Sync conflicts" })).key;
  pr.head.ref = `${own.toLowerCase()}-order`;
  pr.body = `Follow-up to ${follows}.\n\nCloses ${closes}`;
  await hook("pull_request", { action: "opened", pull_request: pr, sender: { login: "robin" } });
  await hook("issue_comment", {
    action: "created",
    issue: { number: 4, title: pr.title, pull_request: {} },
    comment: { id: 701, body: `This might also help with ${commented}`, user: { login: "alex" }, html_url: `${pr.html_url}#issuecomment-701` },
  });
  let f;
  for (let i = 0; i < 50; i++) {
    f = await api("GET", `/issues/${follows}`);
    if (f.forge_mentions?.length) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  const viaOf = async (k) => (await api("GET", `/issues/${k}`)).links.find((l) => l.url === pr.html_url)?.via;
  ok((await viaOf(own)) === "branch", `the branch's issue is linked, via ${await viaOf(own)}`);
  ok((await viaOf(closes)) === "body", `the closed issue is linked, via ${await viaOf(closes)}`);
  ok(f.links.length === 0, `the follow-up isn't linked: ${JSON.stringify(f.links)}`);
  ok(f.forge_mentions?.[0]?.verb === "follows", `the follow-up is a mention: ${JSON.stringify(f.forge_mentions)}`);
  const c = await api("GET", `/issues/${commented}`);
  ok(c.links.length === 0 && c.forge_mentions?.[0]?.places?.[0] === "comment", `the comment's issue is a mention: ${JSON.stringify(c.forge_mentions)}`);
  const all = await api("GET", `/issues/${own}/forge-mentions?all=1`);
  ok(all.length === 1 && all[0].own && all[0].linked, `all of the own issue's mentions: ${JSON.stringify(all)}`);
  const list = await api("GET", `/pulls?project=${key}`);
  const p = list.pulls?.find((x) => x.number === 4);
  prID = p?.id;
  ok(p?.issues?.length === 2 && p?.mentions?.length === 2, `the pull request: ${p?.issues?.length} linked, ${p?.mentions?.length} mentioned`);
  await ctx.close();
}

async function look(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(`${base}/issue/${follows}`);
  const chip = page.getByRole("link", { name: /notes#4/ });
  await chip.waitFor();
  ok(await page.getByText("On the forge 1").isVisible(), `${tag} the issue shows its forge mention`);
  ok((await chip.getAttribute("title"))?.includes(`Follow-up to ${follows}.`), `${tag} the chip's tip quotes the line`);
  ok(await chip.getByText("follow-up to").isVisible(), `${tag} the chip says how it names it`);
  await chip.scrollIntoViewIfNeeded();
  await page.waitForTimeout(200);
  await shot("issue-forge-mention");
  await overflow("issue");

  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  await sheet.getByText("Mentioned, not linked").waitFor();
  ok(await sheet.getByRole("link", { name: follows }).isVisible(), `${tag} the sheet lists the follow-up as a mention`);
  ok(await sheet.getByRole("link", { name: commented }).isVisible(), `${tag} the sheet lists the comment's issue as a mention`);
  await sheet.getByText("Mentioned, not linked").scrollIntoViewIfNeeded();
  await page.waitForTimeout(200);
  await shot("pull-sheet-mentions");
  await overflow("sheet");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await look(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("all ok");
