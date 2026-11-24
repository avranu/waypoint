// The review gate, CI and merge when ready (WP-D21), in a real browser at
// desktop and phone widths, light and dark: a repo's Review gate switch in
// Project settings, a pull request whose review finds a P1 (the gate posts a
// failing waypoint/review status), the pull request's sheet with its checks,
// the gate, the finding and why it can't merge; dismissing the finding in the
// UI turns the gate green; merge when ready waits for CI, then a status
// webhook makes Waypoint merge it on the head it checked.
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
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m), c);
const H = { "X-Waypoint-Client": "web" };
const SECRET = "e2e-gate-secret";
const REPO = "me/gate";
const HEAD = "9999999aaaa";

const DIFF = `diff --git a/app/cart.go b/app/cart.go
--- a/app/cart.go
+++ b/app/cart.go
@@ -10,4 +10,5 @@ func Total() {
 	sum := 0
-	for _, i := range items {
+	for n := 1; n < len(items); n++ {
+		sum += items[n].Price
 	}
 	return sum
`;

const pr = {
  number: 3,
  title: "Cart total skips the first item",
  body: "Fixes the total.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)",
  html_url: "",
  state: "open",
  merged: false,
  draft: false,
  mergeable: true,
  user: { login: "robin" },
  head: { ref: "fix-total", sha: HEAD, repo: { full_name: REPO } },
  base: { ref: "main" },
  labels: [],
  created_at: new Date(Date.now() - 3 * 3600e3).toISOString(),
};

// What the fake GitHub holds: threads the review created, commit statuses
// (CI's, and what Waypoint posts), and what it was asked to merge.
const fakeState = { threads: [], posted: [], merges: [], ci: [], branchDeleted: false };
let nextID = 500;
const srv = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    const send = (code, body, type = "application/json") => {
      res.writeHead(code, { "Content-Type": type });
      res.end(body === undefined ? "" : typeof body === "string" ? body : JSON.stringify(body));
    };
    const url = req.url;
    if (url === "/v1/models") return send(200, { data: [{ id: "fake/muse-spark-1.3" }] });
    if (url === "/v1/chat/completions") {
      return send(200, {
        choices: [
          {
            message: {
              content: JSON.stringify({
                findings: [
                  {
                    file: "app/cart.go",
                    line: 11,
                    severity: "high",
                    claim: "The loop starts at 1, so the first item is never counted.",
                    suggestion: "\tfor n := 0; n < len(items); n++ {",
                  },
                ],
              }),
            },
          },
        ],
      });
    }
    if (url === "/api/graphql") {
      const q = JSON.parse(raw).query;
      if (q.includes("resolveReviewThread")) return send(200, { data: { resolveReviewThread: { thread: { isResolved: true } } } });
      return send(200, {
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false },
                nodes: fakeState.threads.map((t) => ({
                  id: t.id, isResolved: t.resolved, isOutdated: false, path: t.path, line: t.line,
                  resolvedBy: t.resolved ? { login: "alex" } : null,
                  comments: { nodes: t.comments },
                })),
              },
            },
          },
        },
      });
    }
    const m = url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)(?:\/(.*))?$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2] ?? "";
    if (rest === "") return send(200, { allow_merge_commit: false, allow_squash_merge: true, allow_rebase_merge: true });
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, []);
    if (rest === "pulls/3" || rest.startsWith("pulls/3?") || rest === "issues/3") {
      if ((req.headers.accept || "").includes("diff")) return send(200, DIFF, "text/plain");
      return send(200, pr);
    }
    if (rest === "pulls/3/merge" && req.method === "PUT") {
      fakeState.merges.push(JSON.parse(raw));
      Object.assign(pr, { state: "closed", merged: true, merged_at: new Date().toISOString(), merged_by: { login: "waypoint-bot" } });
      return send(200, { merged: true });
    }
    if (rest.startsWith("git/refs/heads/") && req.method === "DELETE") {
      fakeState.branchDeleted = true;
      return send(204);
    }
    if (rest.startsWith("pulls/3/reviews")) {
      if (req.method === "GET") return send(200, []);
      const body = JSON.parse(raw);
      for (const c of body.comments ?? []) {
        const id = nextID++;
        fakeState.threads.push({
          id: `PRRT_${id}`, resolved: false, path: c.path, line: c.line,
          comments: [{ databaseId: id, author: { login: "waypoint-bot" }, body: c.body, createdAt: new Date().toISOString(), commit: { oid: HEAD } }],
        });
      }
      return send(200, { id: 77, html_url: `${pr.html_url}#review-77` });
    }
    let s = rest.match(/^statuses\/(.+)$/);
    if (s && req.method === "POST") {
      fakeState.posted.push({ sha: s[1], ...JSON.parse(raw) });
      return send(201, {});
    }
    s = rest.match(/^commits\/([^/]+)\/status/);
    if (s) {
      const own = fakeState.posted.filter((p) => p.sha === s[1] && p.context === "waypoint/review").slice(-1);
      return send(200, {
        statuses: [...fakeState.ci, ...own.map((p) => ({ context: p.context, state: p.state, description: p.description }))],
      });
    }
    if (rest.startsWith("commits/") && rest.includes("check-runs")) return send(200, { check_runs: [] });
    if (rest.startsWith("contents/")) return send(404, { message: "Not Found" });
    if (rest.startsWith("issues/3/comments")) return send(200, []);
    if (rest.startsWith("issues/3/reactions")) return req.method === "POST" ? send(201, { id: 9 }) : send(204);
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
pr.html_url = `${fake}/${REPO}/pull/3`;
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

const KEY = "GATE";
let forge, prID, repoID;
const wait = async (cond, what, ms = 15000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await cond();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 150));
  }
};

// ---- set up: a review model, a project with a repo, the gate on, a pull request ----
let api, call, hook;
{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  hook = async (event, payload) => {
    const body = JSON.stringify({ ...payload, repository: { full_name: REPO, owner: { type: "User" } } });
    const sig = "sha256=" + crypto.createHmac("sha256", SECRET).update(body).digest("hex");
    const r = await fetch(`${base}/api/v1/hooks/forge/${forge.id}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-GitHub-Event": event, "X-Hub-Signature-256": sig },
      body,
    });
    ok(r.ok, `webhook ${event} accepted (${r.status})`);
  };
  let provider = (await api("GET", "/ai/providers")).find((p) => p.name === "Fake gate models");
  if (!provider)
    provider = await api("POST", "/ai/providers", { kind: "openai", name: "Fake gate models", base_url: `${fake}/v1` });
  await api("PUT", "/ai/tasks/review", { provider_id: provider.id, model: "fake/muse-spark-1.3", fallbacks: [] });
  await api("POST", "/projects", { name: "Gate", key: KEY });
  forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub gate", base_url: fake, token: "tok", webhook_secret: SECRET });
  await api("PUT", `/forges/${forge.id}/people`, { people: [{ login: "robin", user: "robin" }, { login: "alex", user: "robin" }] });
  const added = await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false });
  repoID = added.id;

  await page.goto(`${base}/p/${KEY}/settings?tab=integrations`);
  await page.getByText(REPO).first().waitFor();
  const sw = page.getByRole("switch", { name: "Review gate on this repo's pull requests" });
  ok((await sw.getAttribute("aria-checked")) === "false", "the review gate is off by default");
  await sw.click();
  await page.getByRole("combobox", { name: "Findings that block" }).waitFor();
  ok(true, "turning it on shows the severity");
  const repos = await api("GET", `/projects/${KEY}/repos`);
  ok(repos[0].review_gate === true && repos[0].gate_severity === "medium", `saved with P2 and above: ${JSON.stringify(repos[0].gate_severity)}`);
  await page.screenshot({ path: `${out}/settings-gate.png` });
  await api("PATCH", `/projects/${KEY}/repos/${repoID}`, { auto_merge: false, review_wait_minutes: 0, quiet_minutes: 0 });

  await hook("pull_request", { action: "opened", pull_request: pr, sender: { login: "robin" } });
  await wait(async () => (await api("GET", `/projects/${KEY}/bot-runs`)).every((r) => r.status === "done") && fakeState.posted.length >= 2, "the review and the gate status");
  const states = fakeState.posted.filter((p) => p.context === "waypoint/review").map((p) => p.state);
  ok(states[0] === "pending" && states.at(-1) === "failure", `gate statuses: ${states.join(" → ")}`);
  const last = fakeState.posted.at(-1);
  ok(/^1 open finding: P1 The loop starts at 1/.test(last.description), `status text: ${last.description}`);
  ok(last.target_url.includes("/agents?tab=pulls&pr="), `status links to Waypoint: ${last.target_url}`);
  const list = await api("GET", `/pulls?project=${KEY}`);
  prID = list.pulls[0].id;
  ok(list.pulls[0].gate === "failure", "the list says the gate fails");
  await ctx.close();
}

// ---- look at it, at each width and theme ----
async function look(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const shot = (name, full = false) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: full });
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  const says = async (where) => {
    const p = (await page.request.fetch(`${base}/api/v1/pulls?project=${KEY}`, { headers: H }).then((r) => r.json())).pulls.find((x) => x.id === prID);
    const text = (await where.innerText().catch((e) => `(${e.message.split("\n")[0]})`)).replace(/\s+/g, " ").slice(0, 600);
    return `api: ${JSON.stringify({ state: p?.state, gate: p?.gate, ci: p?.ci, armed: p?.armed })}; page: ${text}`;
  };
  await page.goto(`${base}/agents?tab=pulls`);
  await page.getByRole("button", { name: pr.title }).waitFor();
  const blocked = await page
    .getByText("Blocked by findings")
    .first()
    .waitFor({ timeout: 10000 })
    .then(() => true, () => false);
  if (!ok(blocked, `${tag} the row says findings block it`))
    console.log(`  ${tag} ${await says(page.locator("main").first())}`);
  await shot("pulls");
  await overflow("pulls");
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  await sheet
    .getByText("Not ready to merge")
    .waitFor()
    .catch(async (e) => {
      console.log(`  ${tag} ${await says((await sheet.count()) ? sheet : page.locator("main").first())}`);
      throw e;
    });
  for (const t of ["Review gate", "Blocking", "Findings", "1 open of 1", "Blocks", "The loop starts at 1"])
    ok(await sheet.getByText(t).first().isVisible(), `${tag} the sheet shows “${t}”`);
  ok(await sheet.getByText(/review gate: 1 open finding/).first().isVisible(), `${tag} the reasons name the gate`);
  ok(await sheet.getByRole("button", { name: "Dismiss" }).isVisible(), `${tag} Dismiss is offered`);
  ok(await sheet.getByRole("button", { name: "Merge now" }).isDisabled(), `${tag} Merge now waits`);
  await page.waitForTimeout(300);
  await shot("gate-sheet");
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

// ---- act on it: dismiss, wait for CI, merge when ready ----
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`act pageerror: ${e.message}`));
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  await sheet.getByRole("button", { name: "Dismiss" }).click();
  await sheet.getByText("Dismissed", { exact: true }).first().waitFor();
  ok(true, "Dismiss marks the finding dismissed");
  await sheet.getByText(/Dismissed by Robin Ames/).waitFor();
  ok(true, "the finding's history says who");
  await wait(() => fakeState.posted.at(-1)?.state === "success", "the gate turning green");
  ok(fakeState.posted.at(-1).description === "All 1 finding resolved", `gate status: ${fakeState.posted.at(-1).description}`);
  await sheet.getByText("Passing").waitFor();
  ok(true, "the sheet shows the gate passing");
  fakeState.ci = [{ context: "ci / backend (pull_request)", state: "pending", target_url: `${fake}/ci/1` }];
  await sheet.getByRole("button", { name: "Check now" }).click();
  await sheet.getByText(/1 check still running/).waitFor();
  ok(true, "the reasons say a check is running");
  await sheet.getByRole("button", { name: "Merge when ready" }).click();
  await sheet.getByText(/Merges when ready/).first().waitFor();
  ok(await sheet.getByRole("button", { name: "Turn off" }).isVisible(), "armed: Turn off is offered");
  await page.waitForTimeout(300);
  await page.screenshot({ path: `${out}/armed.png` });
  ok(fakeState.merges.length === 0, "nothing is merged while CI runs");
  fakeState.ci = [{ context: "ci / backend (pull_request)", state: "success", target_url: `${fake}/ci/1` }];
  await hook("status", { sha: HEAD, state: "success", context: "ci / backend (pull_request)" });
  await wait(() => fakeState.merges.length === 1, "the merge");
  const merge = fakeState.merges[0];
  ok(merge.sha === HEAD && merge.merge_method === "squash", `merged the head it checked: ${JSON.stringify(merge)}`);
  await wait(() => fakeState.branchDeleted, "the branch being deleted");
  ok(true, "the branch was deleted");
  await wait(async () => (await (await page.request.fetch(`${base}/api/v1/pulls?project=${KEY}`, { headers: H })).json()).pulls[0].state === "merged", "Waypoint recording it merged");
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  await sheet.getByText("Merging", { exact: true }).waitFor({ state: "detached" });
  ok(!(await sheet.getByRole("button", { name: "Merge now" }).isVisible()), "a merged pull request offers no Merge now");
  ok(!(await sheet.getByText(/Merges when ready/).isVisible()), "and is no longer armed");
  await page.screenshot({ path: `${out}/merged.png` });
  const audit = await (await page.request.fetch(base + "/api/v1/audit?project=" + KEY, { headers: H })).json();
  const actions = (audit.entries ?? []).map((e) => e.action);
  ok(actions.includes("pr.arm") && actions.includes("pr.merge") && actions.includes("finding.dismiss"), `audit: ${actions.join(", ")}`);
  await ctx.close();
}
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("all ok");
