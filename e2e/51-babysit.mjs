// Babysitting a pull request (WP-134), in a real browser at desktop and phone
// widths: the Babysitter panel on a pull request's sheet offers Start; once
// started, Waypoint reads every reviewer's comments from the (fake) forge,
// marks them seen with 👀 when CI and reviews have settled, and, with no
// runner online, hands the round over (Needs you). Verdicts given through
// the API show as chips (👎 Rejected, 🎉 Fixed); a person dismisses the last
// comment and stops the babysit from the panel. Each round shows its class
// and what it was given (WP-438), and a park nobody was asked about shows
// as a rest, not a wait for a person (WP-930).
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
const SECRET = "e2e-babysit-secret";
const REPO = "me/baby";
const HEAD = "4444444aaaa";
const KEY = "BABY";
const at = new Date(Date.now() - 3600e3).toISOString();

const DIFF = `diff --git a/app/export.go b/app/export.go
--- a/app/export.go
+++ b/app/export.go
@@ -1,3 +1,6 @@
 package app
+
+func Export(rows []Row) {
+	for {
+	}
+}
`;

const pr = {
  number: 4,
  title: "Add the export",
  body: "Adds the export.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)",
  html_url: "",
  state: "open",
  merged: false,
  draft: false,
  mergeable: true,
  user: { login: "robin" },
  head: { ref: "add-export", sha: HEAD, repo: { full_name: REPO } },
  base: { ref: "main" },
  labels: [],
  created_at: at,
};

// What the fake GitHub holds: one thread on the diff, a bot's review, a
// person's comment; the emoji and replies Waypoint posts; CI's statuses.
const fakeState = {
  threads: [
    {
      id: "PRRT_1", resolved: false, path: "app/export.go", line: 4,
      comments: [{ databaseId: 501, author: { login: "alice" }, body: "This loop never ends when rows is empty.", createdAt: at, commit: { oid: HEAD } }],
    },
  ],
  reviews: [{ id: 601, user: { login: "llm-review" }, body: "Consider caching the export.", state: "COMMENTED", commit_id: HEAD, submitted_at: at, html_url: "" }],
  comments: [{ id: 701, user: { login: "carol" }, body: "Should exports include archived rows?", created_at: at, updated_at: at, html_url: "" }],
  reactions: [],
  replies: [],
  ci: [],
  posted: [],
};
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
    if (url === "/v1/chat/completions") return send(200, { choices: [{ message: { content: JSON.stringify({ findings: [] }) } }] });
    if (url === "/api/graphql") {
      const q = JSON.parse(raw).query;
      if (q.includes("resolveReviewThread")) {
        const id = JSON.parse(raw).variables.id;
        for (const t of fakeState.threads) if (t.id === id) t.resolved = true;
        return send(200, { data: { resolveReviewThread: { thread: { isResolved: true } } } });
      }
      return send(200, {
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false },
                nodes: fakeState.threads.map((t) => ({
                  id: t.id, isResolved: t.resolved, isOutdated: false, path: t.path, line: t.line,
                  resolvedBy: t.resolved ? { login: "waypoint-bot" } : null,
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
    if (rest === "") return send(200, { allow_merge_commit: true, allow_squash_merge: true, allow_rebase_merge: true });
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, []);
    if (rest === "pulls/4" || rest.startsWith("pulls/4?") || rest === "issues/4") {
      if ((req.headers.accept || "").includes("diff")) return send(200, DIFF, "text/plain");
      return send(200, pr);
    }
    if (rest.startsWith("pulls/4/reviews")) {
      if (req.method === "GET") return send(200, rest.includes("page=1") ? fakeState.reviews : []);
      return send(200, { id: 77, html_url: `${pr.html_url}#review-77` });
    }
    let r = rest.match(/^(pulls|issues)\/comments\/(\d+)\/reactions/);
    if (r && req.method === "POST") {
      fakeState.reactions.push(`${r[2]}:${JSON.parse(raw).content}`);
      return send(201, { id: fakeState.reactions.length });
    }
    r = rest.match(/^pulls\/4\/comments\/(\d+)\/replies/);
    if (r && req.method === "POST") {
      fakeState.replies.push(JSON.parse(raw).body);
      return send(201, { id: 900 + fakeState.replies.length });
    }
    if (rest.startsWith("issues/4/comments")) {
      if (req.method === "POST") {
        fakeState.replies.push(JSON.parse(raw).body);
        return send(201, { id: 800 + fakeState.replies.length, body: JSON.parse(raw).body, user: { login: "waypoint-bot" } });
      }
      return send(200, rest.includes("page=1") || !rest.includes("page=") ? fakeState.comments : []);
    }
    if (rest.startsWith("issues/4/reactions")) return req.method === "POST" ? send(201, { id: 9 }) : send(204);
    let s = rest.match(/^statuses\/(.+)$/);
    if (s && req.method === "POST") {
      fakeState.posted.push({ sha: s[1], ...JSON.parse(raw) });
      return send(201, {});
    }
    s = rest.match(/^commits\/([^/]+)\/status/);
    if (s) return send(200, { statuses: fakeState.ci });
    if (rest.startsWith("commits/") && rest.includes("check-runs")) return send(200, { check_runs: [] });
    if (rest.startsWith("contents/")) return send(404, { message: "Not Found" });
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
pr.html_url = `${fake}/${REPO}/pull/4`;
fakeState.reviews[0].html_url = `${pr.html_url}#pullrequestreview-601`;
fakeState.comments[0].html_url = `${pr.html_url}#issuecomment-701`;
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

const wait = async (cond, what, ms = 15000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await cond();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 150));
  }
};

// ---- set up: a review model, a project with a repo, a pull request with CI passed ----
let api, hook, forge, prID;
const setupCtx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
{
  const page = await signIn(setupCtx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
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
  let provider = (await api("GET", "/ai/providers")).find((p) => p.name === "Fake babysit models");
  if (!provider) provider = await api("POST", "/ai/providers", { kind: "openai", name: "Fake babysit models", base_url: `${fake}/v1` });
  await api("PUT", "/ai/tasks/review", { provider_id: provider.id, model: "fake/muse-spark-1.3", fallbacks: [] });
  await api("POST", "/projects", { name: "Babysat", key: KEY });
  forge = await api("POST", "/forges", { kind: "github", name: "Fake GitHub babysit", base_url: fake, token: "tok", webhook_secret: SECRET });
  await api("PUT", `/forges/${forge.id}/people`, { people: [{ login: "robin", user: "robin" }, { login: "alice", user: "robin" }, { login: "carol", user: "robin" }, { login: "llm-review", user: "robin" }] });
  const added = await api("POST", `/projects/${KEY}/repos`, { forge_id: forge.id, repo: REPO, sync_issues: false });
  await api("PATCH", `/projects/${KEY}/repos/${added.id}`, { auto_merge: false, review_wait_minutes: 0, quiet_minutes: 0 });
  await hook("pull_request", { action: "opened", pull_request: pr, sender: { login: "robin" } });
  await wait(async () => {
    const runs = await api("GET", `/projects/${KEY}/bot-runs`);
    return runs.length > 0 && runs.every((r) => r.status === "done" || r.status === "skipped");
  }, "the review");
  prID = (await api("GET", `/pulls?project=${KEY}`)).pulls[0].id;
  fakeState.ci = [{ context: "ci / test", state: "success", target_url: `${fake}/ci/1` }];
  await hook("status", { sha: HEAD, state: "success", context: "ci / test" });
  await wait(async () => (await api("GET", `/pulls/${prID}/readiness`)).reasons.every((x) => !/check/.test(x)), "CI read as passed");
}

// ---- start it from the panel ----
{
  const page = setupCtx.pages()[0];
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  const panel = sheet.getByRole("region", { name: "Babysitter" });
  await panel.getByRole("button", { name: "Start" }).waitFor();
  ok(true, "the panel offers Start");
  ok(!(await panel.getByRole("button", { name: "Stop" }).isVisible()), "and no Stop before it starts");
  await page.screenshot({ path: `${out}/start-1280.png` });
  await panel.getByRole("button", { name: "Start" }).click();
  await panel.getByText("Needs you").waitFor({ timeout: 20000 });
  ok(true, "with no runner, the round is handed over: Needs you");
  ok(await panel.getByText(/round 1 handed over: no runner/).isVisible(), "the panel says why");
  await panel.getByText("Review comments").waitFor();
  ok((await panel.locator('li[data-verdict="seen"]').count()) === 3, "three comments, each seen");
  ok(fakeState.reactions.includes("501:eyes") && fakeState.reactions.includes("701:eyes"), `👀 on the forge: ${fakeState.reactions.join(", ")}`);
  ok(!(await panel.getByRole("button", { name: "Start" }).isVisible()), "Start is gone once it runs");

  const view = await api("GET", `/pulls/${prID}/babysit`);
  const id = (author) => view.comments.find((c) => c.author === author).id;
  await api("POST", "/review-comments/ack", { ids: [id("llm-review")], verdict: "rejected", note: "It runs once a day; a cache buys nothing." });
  await api("POST", "/review-comments/ack", { ids: [id("alice")], verdict: "accepted" });
  await api("POST", "/review-comments/fixed", { ids: [id("alice")] });
  await panel.locator('li[data-verdict="fixed"]').waitFor();
  await panel.locator('li[data-verdict="rejected"]').waitFor();
  ok(true, "the panel follows the verdicts live");
  ok(fakeState.reactions.includes("501:+1") && fakeState.reactions.includes("501:hooray"), `👍 and 🎉 on alice's comment: ${fakeState.reactions.join(", ")}`);
  ok(fakeState.replies.some((b) => b.includes("@llm-review Not changing this: It runs once a day")), "the rejection got a reply");
  ok(fakeState.threads[0].resolved, "the fixed comment's thread was resolved");
}

// ---- look at it, at each width ----
async function look(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  await page.route("**/api/v1/pulls/*/babysit", async (route) => {
    const res = await route.fetch();
    const json = await res.json();
    const later = new Date(Date.now() + 3600e3).toISOString();
    json.babysit?.rounds.push(
      { n: 2, sha: HEAD, assessor: true, outcome: "done, nothing pushed", parked: "the assessor's round 2 on this head didn't settle it", rest_until: at, reply: "Nobody was asked about it. Nothing about it changed while it rested." },
      { n: 3, sha: HEAD, assessor: true, outcome: "done, nothing pushed", parked: "the assessor says it waits on something outside the pull request: the CI image has to be rebuilt", rest_until: later, reply: "Nobody was asked about it." },
      { n: 4, sha: HEAD, assessor: true, outcome: "done, nothing pushed", parked: "the assessor's round 4 on this head didn't settle it", rest_until: at, rerun_at: at },
      { n: 5, sha: HEAD, assessor: true, outcome: "done, nothing pushed", parked: "the assessor's round 5 on this head didn't settle it", rest_until: later },
    );
    await route.fulfill({ response: res, json });
  });
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  const panel = sheet.getByRole("region", { name: "Babysitter" });
  await panel.getByText("Needs you").waitFor();
  const rested = panel.locator('p[data-parked="rest"]');
  ok((await rested.count()) === 4, `${tag} the rests are shown as rests`);
  ok(await rested.first().getByText("Out of the merge line, rested (nobody asked):").isVisible(), `${tag} an ended rest says nobody was asked`);
  ok(await rested.nth(1).getByText("the CI image has to be rebuilt").isVisible(), `${tag} a rest says what the assessor waits for`);
  ok(await rested.nth(2).getByText("Out of the merge line, its checks running again (nobody asked):").isVisible(), `${tag} a rest re-running its checks says so`);
  ok(await rested.nth(3).getByText(/^Out of the merge line, resting until .+ \(nobody asked\):$/).isVisible(), `${tag} a rest says until when`);
  ok(!(await panel.getByText("waiting for a person").isVisible()) && !(await panel.getByText("Answered:").isVisible()), `${tag} a rest claims no person`);
  for (const t of ["👎 Rejected", "🎉 Fixed", "👀 Seen", "1 to deal with of 3", "This loop never ends", "Should exports include archived rows?"])
    ok(await panel.getByText(t).first().isVisible(), `${tag} the panel shows “${t}”`);
  const round = panel.locator('li[data-class="new"]').first();
  ok(await round.getByText("Round 1").isVisible(), `${tag} the round is listed`);
  ok(await round.getByText("New", { exact: true }).isVisible(), `${tag} with its class`);
  ok((await round.locator('li[data-class="new"]').count()) === 3, `${tag} and its three comments, each new`);
  ok(await panel.getByRole("button", { name: "Stop" }).isVisible(), `${tag} Stop is offered`);
  ok((await panel.getByRole("button", { name: "Dismiss" }).count()) === 1, `${tag} Dismiss only on the comment left to deal with`);
  ok(await panel.getByRole("link", { name: /Open c\d+ on the forge/ }).first().isVisible(), `${tag} each comment links to the forge`);
  await panel.scrollIntoViewIfNeeded();
  await page.waitForTimeout(300);
  await panel.screenshot({ path: `${out}/panel-${width}-${scheme}.png` });
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  const box = await panel.boundingBox();
  ok(box && box.width <= width, `${tag} the panel fits (${box?.width})`);
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [1280, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await look(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}

// ---- a person dismisses the last comment, then stops it ----
{
  const ctx = await browser.newContext({ viewport: { width: 390, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`act pageerror: ${e.message}`));
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const panel = page.getByRole("dialog", { name: "Pull request" }).getByRole("region", { name: "Babysitter" });
  await panel.getByRole("button", { name: "Dismiss" }).click();
  await panel.getByText("all 3 dealt with").waitFor();
  ok(true, "Dismiss deals with the last comment");
  await wait(async () => (await api("GET", `/pulls/${prID}/babysit`)).babysit.state === "ready", "the babysit to say ready");
  await panel.getByText("Ready", { exact: true }).waitFor();
  ok(await panel.getByText(/ready for a person to merge/).isVisible(), "not armed here: ready for a person to merge");
  await panel.getByRole("button", { name: "Stop" }).click();
  await panel.getByText("Stopped", { exact: true }).waitFor();
  ok(await panel.getByText(/stopped by/).isVisible(), "stopped, and by whom");
  ok(await panel.getByRole("button", { name: "Start" }).isVisible(), "Start is offered again");
  await page.waitForTimeout(300);
  await panel.screenshot({ path: `${out}/stopped-390.png` });
  await ctx.close();
}
await setupCtx.close();
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("all ok");
