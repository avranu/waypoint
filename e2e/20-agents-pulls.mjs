// Pull requests on the Agents page (WP-D15), in a real browser at desktop
// and phone widths, light and dark: Admin → AI words the note every bot
// review starts with, and the review a webhook sets off uses it; a pull
// request's pushes, a person's approval and comment, and its merge arrive as
// webhooks from a fake GitHub; an agent works the issue it's linked to. The
// Pull requests tab sums it up, and the pull request's sheet tells its story.
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
const SECRET = "e2e-pulls-secret";
const REPO = "me/shop";
const NOTE =
  "Low-trust review by a free model ({models}) · verify before acting.";

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

// The pull request as the fake GitHub reports it now.
const pr = {
  number: 7,
  title: "Checkout totals skip the first item",
  body: "Fixes the cart total.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>",
  html_url: "", // Set below to the fake's own address, so links to it count as its pull request.
  state: "open",
  merged: false,
  draft: false,
  user: { login: "robin" },
  head: { ref: "fix-totals", sha: "1111111aaaa" },
  base: { ref: "main" },
  labels: [],
  created_at: new Date(Date.now() - 26 * 3600e3).toISOString(),
  additions: 12,
  deletions: 3,
  changed_files: 2,
  commits: 2,
};

const seen = { reviews: [] };
const srv = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    const send = (code, body, type = "application/json") => {
      res.writeHead(code, { "Content-Type": type });
      res.end(
        body === undefined
          ? ""
          : typeof body === "string"
            ? body
            : JSON.stringify(body),
      );
    };
    const url = req.url;
    if (url === "/v1/models")
      return send(200, { data: [{ id: "fake/muse-spark-1.3" }] });
    if (url === "/v1/chat/completions") {
      const findings = {
        findings: [
          {
            file: "app/cart.go",
            line: 11,
            severity: "high",
            claim: "The loop starts at 1, so the first item is never counted.",
            suggestion: "\tfor n := 0; n < len(items); n++ {",
          },
        ],
      };
      return send(200, {
        choices: [{ message: { content: JSON.stringify(findings) } }],
      });
    }
    const m = url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)\/(.*)$/);
    if (!m) return send(404, { message: "Not Found" });
    const rest = m[2];
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest.startsWith("pulls?")) return send(200, []);
    if (
      rest === "pulls/7" ||
      rest.startsWith("pulls/7?") ||
      rest === "issues/7"
    ) {
      if ((req.headers.accept || "").includes("diff"))
        return send(200, DIFF, "text/plain");
      return send(200, pr);
    }
    if (rest.startsWith("pulls/7/reviews")) {
      if (req.method === "GET") return send(200, []);
      seen.reviews.push(JSON.parse(raw));
      return send(200, { id: 77, html_url: `${pr.html_url}#review-77` });
    }
    if (rest.startsWith("issues/7/comments")) return send(200, []);
    if (rest.startsWith("issues/7/reactions"))
      return req.method === "POST" ? send(201, { id: 9 }) : send(204);
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
pr.html_url = `${fake}/${REPO}/pull/7`;
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

// ---- set up once: the note, a project with a repo, a pull request's life ----
let key, prID, forge;
{
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const hook = async (event, payload) => {
    const body = JSON.stringify({
      ...payload,
      repository: { full_name: REPO, owner: { type: "User" } },
    });
    const sig =
      "sha256=" +
      crypto.createHmac("sha256", SECRET).update(body).digest("hex");
    const r = await page.request.post(
      `${base}/api/v1/hooks/forge/${forge.id}`,
      {
        headers: {
          "Content-Type": "application/json",
          "X-GitHub-Event": event,
          "X-Hub-Signature-256": sig,
        },
        data: body,
      },
    );
    ok(r.ok(), `webhook ${event} accepted (${r.status()})`);
  };
  const waitRuns = async (n) => {
    let runs = [];
    for (let i = 0; i < 100; i++) {
      runs = await api("GET", `/projects/${key}/bot-runs`);
      if (
        runs.length >= n &&
        runs.every((r) => !["queued", "running"].includes(r.status))
      )
        break;
      await new Promise((r) => setTimeout(r, 200));
    }
    return runs;
  };

  let provider = (await api("GET", "/ai/providers")).find(
    (p) => p.name === "Fake pulls models",
  );
  if (!provider)
    provider = await api("POST", "/ai/providers", {
      kind: "openai",
      name: "Fake pulls models",
      base_url: `${fake}/v1`,
    });
  await api("PUT", "/ai/tasks/review", {
    provider_id: provider.id,
    model: "fake/muse-spark-1.3",
    fallbacks: [],
  });
  await page.goto(`${base}/admin?tab=ai`);
  const note = page.getByLabel("Note on every review");
  await note.waitFor();
  ok(
    (await note.inputValue()).includes("{models}"),
    "the default note names the models",
  );
  await note.fill(NOTE);
  await page
    .getByText("Low-trust review by a free model (fake/muse-spark-1.3)")
    .waitFor();
  ok(true, "the preview fills in the model");
  // Several forms on Admin → AI have a Save button, so scope it to this form.
  const noteForm = page.locator("form").filter({ has: note });
  await noteForm.getByRole("button", { name: "Save" }).click();
  await noteForm.getByRole("button", { name: "Use the default" }).waitFor();
  const saved = await api("GET", "/admin/review-note");
  ok(saved.custom && saved.note === NOTE, `note saved: ${saved.note}`);

  key = "PRS";
  await api("POST", "/projects", { name: "Shop", key });
  forge = await api("POST", "/forges", {
    kind: "github",
    name: "Fake GitHub pulls",
    base_url: fake,
    token: "tok",
    webhook_secret: SECRET,
  });
  await api("POST", `/projects/${key}/repos`, {
    forge_id: forge.id,
    repo: REPO,
    sync_issues: false,
  });
  // Their forge logins are Waypoint people here: text from anyone else waits for a person (WP-281).
  await api("PUT", `/forges/${forge.id}/people`, {
    people: [
      { login: "robin", user: "robin" },
      { login: "alex", user: "robin" },
      { login: "renovate[bot]", user: "robin" },
    ],
  });

  await hook("pull_request", {
    action: "opened",
    pull_request: pr,
    sender: { login: "robin" },
  });
  let runs = await waitRuns(1);
  ok(
    runs[0]?.status === "done",
    `first review: ${runs[0]?.status} ${runs[0]?.error ?? ""}`,
  );
  ok(
    seen.reviews[0]?.body?.startsWith(
      "Low-trust review by a free model (`fake/muse-spark-1.3`) · verify before acting.",
    ),
    `the posted review starts with the note: ${JSON.stringify(seen.reviews[0]?.body?.split("\n")[0])}`,
  );
  ok(
    /<sub>Low-trust review by a free model/.test(
      seen.reviews[0]?.comments?.[0]?.body ?? "",
    ),
    "line comments are signed with the note",
  );
  pr.head.sha = "2222222bbbb";
  await hook("pull_request", {
    action: "synchronize",
    pull_request: pr,
    sender: { login: "robin" },
  });
  await waitRuns(2);
  await hook("pull_request_review", {
    action: "submitted",
    pull_request: pr,
    review: {
      id: 501,
      user: { login: "alex" },
      state: "approved",
      body: "Looks right now.",
      commit_id: "2222222bbbb",
      html_url: `${pr.html_url}#pullrequestreview-501`,
    },
  });
  await hook("issue_comment", {
    action: "created",
    issue: { number: 7, title: pr.title, pull_request: {} },
    comment: {
      id: 601,
      body: "Could we add a test for an empty cart?",
      user: { login: "alex" },
      html_url: `${pr.html_url}#issuecomment-601`,
    },
  });
  const issue = await api("POST", "/issues", {
    project: key,
    title: "Cart total is wrong",
    agent: "any",
  });
  await api("POST", `/issues/${issue.key}/links`, { url: pr.html_url });
  const { token } = await api("POST", "/me/tokens", {
    name: "pulls agent",
    agent: "anthropic/claude-code",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  await call("POST", `/agent/take/${issue.key}`, {}, AG);
  await new Promise((r) => setTimeout(r, 1200));
  await call(
    "POST",
    `/agent/done/${issue.key}`,
    { usage: { input_tokens: 18400, output_tokens: 2100, cost_usd: 0.42 } },
    AG,
  );
  Object.assign(pr, {
    state: "closed",
    merged: true,
    merged_at: new Date().toISOString(),
    merged_by: { login: "alex" },
  });
  await hook("pull_request", {
    action: "closed",
    pull_request: pr,
    sender: { login: "alex" },
  });

  const bump = {
    ...pr,
    number: 8,
    title: "Bump lodash",
    state: "open",
    merged: false,
    merged_at: null,
    merged_by: null,
    user: { login: "renovate[bot]" },
    html_url: `${fake}/${REPO}/pull/8`,
    body: "",
    head: { ref: "renovate/lodash", sha: "3333333cccc" },
  };
  await hook("pull_request", {
    action: "opened",
    pull_request: bump,
    sender: { login: "renovate[bot]" },
  });

  const list = await api("GET", `/pulls?project=${key}`);
  const p = list.pulls?.find((x) => x.number === 7);
  prID = p?.id;
  ok(
    p?.state === "merged" &&
      p.merged_by === "alex" &&
      p.pushes === 1 &&
      p.approvals?.includes("alex") &&
      p.comments.people === 1 &&
      p.reviews.bots === 2 &&
      p.issues?.length === 1 &&
      p.input_tokens === 18400,
    `the merged pull request's summary: ${JSON.stringify(p && { state: p.state, pushes: p.pushes, reviews: p.reviews, comments: p.comments, approvals: p.approvals, issues: p.issues?.length, tokens: p.input_tokens })}`,
  );
  ok(
    p?.agents?.includes("anthropic/claude-opus-5-5") &&
      p.agents.includes("anthropic/claude-code"),
    `agents: ${p?.agents}`,
  );
  ok(
    list.totals?.merged === 1 && list.totals.open_now === 1,
    `totals: ${JSON.stringify(list.totals)}`,
  );
  const audit = await page.request.fetch(base + "/api/v1/audit", {
    headers: H,
  });
  if (audit.ok()) ok(true, "audit endpoint answered (another slice's)");
  await ctx.close();
}

// ---- look at it, at each width and theme ----
async function look(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const shot = (name, full = false) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: full,
    });
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(`${base}/agents?tab=pulls`);
  await page
    .getByRole("button", { name: "Checkout totals skip the first item" })
    .waitFor();
  ok(
    await page.getByText("Approvals by people").isVisible(),
    `${tag} totals show`,
  );
  ok(
    await page
      .getByText(/merged .* by alex/)
      .first()
      .isVisible(),
    `${tag} the row says who merged it`,
  );
  ok(
    await page.getByText("1 push").first().isVisible(),
    `${tag} iterations show`,
  );
  ok(await page.getByText("P1").first().isVisible(), `${tag} findings show`);
  await page.waitForTimeout(300);
  await shot("pulls");
  await overflow("pulls");
  await page.getByRole("radio", { name: "Merged" }).click();
  await page
    .getByRole("button", { name: "Bump lodash" })
    .waitFor({ state: "detached" });
  ok(true, `${tag} the Merged filter hides the open one`);
  await page.getByRole("radio", { name: "All" }).click();
  await page.getByTitle("Show only anthropic/claude-opus-5-5").first().click();
  await page.waitForURL(/agent=anthropic/);
  await page
    .getByRole("button", { name: "Bump lodash" })
    .waitFor({ state: "detached" });
  ok(true, `${tag} an agent chip filters to that agent's pull requests`);
  await page.goto(`${base}/agents?tab=pulls&pr=${prID}`);
  const sheet = page.getByRole("dialog", { name: "Pull request" });
  await sheet.getByText("What happened").waitFor();
  for (const t of [
    "approved it",
    "merged it",
    "Agents’ work on its issues",
    "Bot reviews",
    "Cart total is wrong",
  ])
    ok(
      await sheet.getByText(t).first().isVisible(),
      `${tag} the sheet shows “${t}”`,
    );
  await page.waitForTimeout(300);
  await shot("pull-sheet");
  await sheet.locator("..").evaluate((el) => el.scrollTo(0, el.scrollHeight));
  await shot("pull-sheet-end");
  await overflow("sheet");
  await page.keyboard.press("Escape");
  await page.waitForURL((u) => !u.searchParams.has("pr"));
  ok(true, `${tag} Esc closes the sheet`);
  await page.goto(`${base}/admin?tab=ai`);
  await page.getByLabel("Note on every review").waitFor();
  await page.getByText("Preview").scrollIntoViewIfNeeded();
  await shot("admin-review-note");
  await overflow("admin ai");
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
