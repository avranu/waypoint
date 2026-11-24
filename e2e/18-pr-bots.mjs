// Bots on pull requests (WP-D14), in a real browser: Admin → AI gets a review
// model with fallbacks; a project's default "Review pull requests" rule reads
// as a sentence and opens in the editor; a ready-made rule can be copied and
// edited; Integrations has a Bots switch. Then a signed pull_request webhook
// from a fake GitHub starts a review: the first model is rate limited, the
// fallback answers, the review lands on the fake GitHub with a line comment,
// and the project's Bots tab shows the run, the models and the finding.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const SECRET = "e2e-secret";

const DIFF = `diff --git a/app/login.go b/app/login.go
--- a/app/login.go
+++ b/app/login.go
@@ -10,4 +10,5 @@ func Login() {
 	user := find()
-	if user == nil {
+	if user != nil {
+		return errNoUser
 	}
 	return nil
`;

// One fake for everything: an OpenAI-compatible model server under /v1 and
// a GitHub API under /api/v3 that knows one pull request per repo.
const seen = { reviews: [], reactions: [], llm: [], verified: 0 };
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
    if (url === "/api/tags")
      return send(200, {
        models: [
          { name: "qwen3:8b", details: { parameter_size: "8.2B" } },
          { name: "llama3.2:latest", details: {} },
        ],
      });
    if (url === "/v1/models")
      return send(200, {
        data: [
          { id: "fake/muse-spark-1.3" },
          { id: "fake/nemotron-3-ultra" },
          { id: "fake/space-bunny" },
        ],
      });
    if (url === "/v1/chat/completions") {
      const body = JSON.parse(raw);
      seen.llm.push(body.model);
      if (
        JSON.stringify(body.messages).includes(
          "You check findings a code reviewer is about to post",
        )
      ) {
        seen.verified++;
        const verdicts = {
          verdicts: [
            {
              n: 1,
              verdict: "keep",
              reason: "Line 11 tests user != nil.",
              line: 0,
              severity: "",
              claim: "",
            },
          ],
        };
        return send(200, {
          choices: [{ message: { content: JSON.stringify(verdicts) } }],
        });
      }
      if (body.model.includes("muse"))
        return send(429, { error: { message: "rate limited" } });
      const findings = {
        findings: [
          {
            file: "app/login.go",
            line: 11,
            severity: "high",
            claim: "The nil check is inverted, so every login fails.",
            suggestion: "\tif user == nil {",
          },
        ],
      };
      return send(200, {
        choices: [{ message: { content: JSON.stringify(findings) } }],
      });
    }
    let m = url.match(/^\/api\/v3\/repos\/([^/]+\/[^/]+)\/(.*)$/);
    if (!m) return send(404, { message: "Not Found" });
    const [, repo, rest] = m;
    if (rest.startsWith("hooks")) {
      if (rest.includes("pings")) return send(204);
      return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
    }
    if (rest === "pulls/5" || rest.startsWith("pulls/5?")) {
      if ((req.headers.accept || "").includes("diff"))
        return send(200, DIFF, "text/plain");
      return send(200, {
        number: 5,
        title: "Fix login",
        body: "",
        html_url: `https://github.example/${repo}/pull/5`,
        state: "open",
        draft: false,
        user: { login: "sam" },
        head: { ref: "fix-login", sha: "1111111aaaa" },
        base: { ref: "main" },
        labels: [],
      });
    }
    if (rest.startsWith("pulls/5/reviews")) {
      if (req.method === "GET") return send(200, []);
      seen.reviews.push(JSON.parse(raw));
      return send(200, {
        id: 77,
        html_url: `https://github.example/${repo}/pull/5#review-77`,
      });
    }
    if (rest.startsWith("issues/5/comments")) return send(200, []);
    if (rest.startsWith("issues/5/reactions")) {
      seen.reactions.push(`${req.method} ${raw}`);
      return req.method === "POST" ? send(201, { id: 9 }) : send(204);
    }
    if (rest.startsWith("contents/app/login.go"))
      return send(
        200,
        "package app\n\nfunc Login() error {\n\tuser := find()\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n\treturn nil\n}\n",
        "text/plain",
      );
    if (rest.startsWith("contents/"))
      return send(404, { message: "Not Found" });
    return send(404, { message: "Not Found" });
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;

const browser = await chromium.launch();

async function run(width) {
  const tag = `${width}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}.png`, fullPage: true });

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const providers = await api("GET", "/ai/providers");
  let provider = providers.find((p) => p.name === "Fake models");
  if (!provider)
    provider = await api("POST", "/ai/providers", {
      kind: "openai",
      name: "Fake models",
      base_url: `${fake}/v1`,
    });
  await api("PUT", "/ai/tasks/review", {
    provider_id: provider.id,
    model: "fake/muse-spark-1.3",
    fallbacks: [],
  });
  await page.goto(`${base}/admin?tab=ai`);
  await page.getByText("Coding agents on your runners").waitFor();
  await page.getByTestId("coding-agents").waitFor();
  const agents = page.getByTestId("coding-agents");
  const nLi = await agents.locator("li").count();
  const states = await agents
    .locator("li[data-state]")
    .evaluateAll((l) => l.map((x) => x.getAttribute("data-state")));
  ok(
    nLi === 4 && states.length === 4,
    `${tag} Admin → AI lists the four coding agents with their state (li ${nLi}, states ${JSON.stringify(states)}, text ${JSON.stringify((await agents.innerText().catch(() => "")).slice(0, 300))})`,
  );
  await page.getByRole("button", { name: "Connect a computer" }).click();
  await page.getByText("Connect a runner").waitFor();
  await shot("admin-ai-agents");
  await overflow("admin ai");
  const refused = await page.request.fetch(`${base}/api/v1/ai/tasks/review`, {
    method: "PUT",
    headers: { "X-Waypoint-Client": "web" },
    data: {
      provider_id: provider.id,
      model: "fake/muse-spark-1.3",
      fallbacks: [{ provider_id: 0, model: "", cli: "claude" }],
    },
  });
  ok(
    refused.status() === 400,
    `${tag} an AI task refuses a coding agent (${refused.status()})`,
  );
  const row = page.locator("li", { hasText: "Review pull requests" }).first();
  await row.getByRole("button", { name: /Add a fallback model/ }).click();
  await page.getByRole("option", { name: "fake/nemotron-3-ultra" }).click();
  await row.getByText("fake/nemotron-3-ultra").waitFor();
  await row.getByRole("button", { name: /Add another fallback/ }).click();
  await page.getByPlaceholder(/type a pattern/).fill("fake/space-*");
  await page.getByRole("option", { name: /Use “fake\/space-\*”/ }).click();
  await row.getByText("fake/space-*").waitFor();
  const tasks = await api("GET", "/ai/tasks");
  const review = tasks.find((t) => t.task === "review");
  ok(
    review.model === "fake/muse-spark-1.3" &&
      review.fallbacks.map((f) => f.model).join(",") ===
        "fake/nemotron-3-ultra,fake/space-*",
    `${tag} review chain saved: ${review.model} → ${review.fallbacks.map((f) => f.model).join(" → ")}`,
  );
  await row.getByRole("button", { name: "Try earlier" }).last().click();
  await page.waitForTimeout(300);
  const reordered = (await api("GET", "/ai/tasks")).find(
    (t) => t.task === "review",
  );
  ok(
    reordered.fallbacks[0].model === "fake/space-*",
    `${tag} fallbacks reorder`,
  );
  await row.getByRole("button", { name: "Try earlier" }).last().click();
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Add provider" }).click();
  const pdlg = page.getByRole("dialog");
  await pdlg.getByRole("combobox", { name: "Kind" }).click();
  await page.getByRole("option", { name: "Ollama (local or remote)" }).click();
  await pdlg.getByLabel("Name").fill(`Ollama ${tag}`);
  await pdlg.getByLabel("URL").fill(`${fake}/v1`);
  await pdlg.getByRole("button", { name: "Add" }).click();
  const orow = page.locator("li", { hasText: `Ollama ${tag}` }).first();
  await orow.getByText(fake, { exact: true }).waitFor();
  ok(
    await orow.getByText("Ollama", { exact: true }).isVisible(),
    `${tag} the Ollama provider is saved at its server root`,
  );
  await orow.getByRole("button", { name: `Actions for Ollama ${tag}` }).click();
  await page.getByRole("menuitem", { name: "Test connection" }).click();
  await page.getByText(`Ollama ${tag} answered: 2 models`).waitFor();
  ok(true, `${tag} Test connection lists the Ollama models`);
  await shot("admin-ai-ollama");
  const ollama = (await api("GET", "/ai/providers")).find(
    (p) => p.name === `Ollama ${tag}`,
  );
  await api("DELETE", `/ai/providers/${ollama.id}`);
  {
    const { DatabaseSync } = await import("node:sqlite");
    const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
    db.exec("PRAGMA busy_timeout = 5000");
    db.prepare(
      "INSERT INTO server_settings(key, value) VALUES ('review_drop_post', '0') ON CONFLICT(key) DO UPDATE SET value = '0'",
    ).run();
    db.prepare(
      "INSERT INTO server_settings(key, value) VALUES ('review_arm_pins', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
    ).run(JSON.stringify({ "diff/verify": "on", "diff+files/verify": "on" }));
    db.close();
  }
  await api("PUT", "/ai/tasks/verify_review", {
    provider_id: provider.id,
    model: "fake/space-bunny",
    fallbacks: [],
  });
  await page.reload();
  await page
    .locator("li", { hasText: "Verify review findings" })
    .getByText("fake/space-bunny")
    .first()
    .waitFor();
  ok(true, `${tag} Admin → AI lists the verifier with its model`);
  await shot("admin-ai");
  await overflow("admin ai");

  const key = `B${width}`;
  await api("POST", "/projects", { name: `Bots ${tag}`, key });
  const forge = await api("POST", "/forges", {
    kind: "github",
    name: `Fake GitHub ${tag}`,
    base_url: fake,
    token: "tok",
    webhook_secret: SECRET,
  });
  await api("POST", `/projects/${key}/repos`, {
    forge_id: forge.id,
    repo: `me/app${tag}`,
    sync_issues: false,
  });
  await api("PUT", `/forges/${forge.id}/people`, {
    people: [{ login: "sam", user: "robin" }],
  });

  await api("PATCH", `/forges/${forge.id}`, { bot_token: "bot-tok" });
  await page.goto(`${base}/admin?tab=forges`);
  ok(
    await page
      .getByText("bot account")
      .first()
      .waitFor()
      .then(
        () => true,
        () => false,
      ),
    `${tag} the forge shows its bot account`,
  );
  await page.goto(`${base}/p/${key}/settings?tab=automations`);
  await page.getByText("When a pull request is opened or pushed to").waitFor();
  ok(
    await page
      .getByText(/review it with the review models/)
      .first()
      .isVisible(),
    `${tag} the review rule reads as a sentence`,
  );
  ok(
    await page
      .getByText(/When someone comments @waypoint review on a pull request/)
      .isVisible(),
    `${tag} the review-when-asked rule reads as a sentence`,
  );
  for (const name of [
    "Review with a second opinion",
    "Fix failing CI on members' pull requests",
  ]) {
    const card = page
      .locator("div.rounded-xl", { hasText: name })
      .filter({ has: page.getByRole("button", { name: "Add, off" }) })
      .first();
    await card.waitFor();
    await card.scrollIntoViewIfNeeded();
    ok(true, `${tag} the gallery offers "${name}", added switched off`);
  }
  await shot("automations");
  await overflow("automations");
  await page.getByRole("button", { name: "Review pull requests" }).click();
  await page.getByRole("dialog").getByText("ready pull requests").waitFor();
  ok(
    !(await page.getByRole("button", { name: "Only if…" }).isVisible()),
    `${tag} no issue condition on a pull request rule`,
  );
  {
    const ed = page.getByRole("dialog");
    await ed.getByRole("button", { name: "More conditions…" }).click();
    await page.getByRole("menuitem", { name: "Touching certain files" }).click();
    await ed.getByLabel("Touching certain files", { exact: true }).fill("web/**, internal/**");
    await ed.getByRole("button", { name: "More conditions…" }).click();
    await page.getByRole("menuitem", { name: "Except when it only touches certain files" }).click();
    await ed.getByLabel("Except when it only touches certain files", { exact: true }).fill("docs/**");
    ok(
      (await ed.getByText("only if", { exact: true }).isVisible()) &&
        (await ed.getByRole("button", { name: "Remove “Touching certain files”" }).isVisible()),
      `${tag} the files condition shows as a chip under "only if"`,
    );
  }
  await shot("editor-review");
  await overflow("editor");
  await page.getByRole("button", { name: "Cancel" }).click();

  await page
    .locator("div.rounded-xl", { hasText: "Security review of pull requests" })
    .getByRole("button", { name: "Copy and edit" })
    .click();
  const dlg = page.getByRole("dialog");
  const filled = async (label, test) => {
    const field = dlg.getByLabel(label);
    for (let i = 0; i < 50; i++) {
      const v = await field.inputValue().catch(() => "");
      if (test(v)) return true;
      await page.waitForTimeout(100);
    }
    return false;
  };
  ok(
    await filled(
      "Name",
      (v) => v === "Security review of pull requests (copy)",
    ),
    `${tag} copy and edit names the copy`,
  );
  ok(
    await filled("Focus", (v) => /security/i.test(v)),
    `${tag} the copy keeps the focus`,
  );
  await dlg.getByRole("button", { name: /Use other models/ }).click();
  await page.getByRole("option", { name: "fake/nemotron-3-ultra" }).click();
  await dlg
    .getByRole("button", { name: "Add a coding agent on my runner" })
    .first()
    .click();
  await page.getByRole("menuitem", { name: "Claude Code" }).click();
  await dlg.getByLabel("Model of Claude Code").fill("sonnet");
  const claudeReady = (await api("GET", "/runners")).some(
    (r) =>
      r.online &&
      r.clis.some((c) => c.name === "claude" && c.signed_in !== false),
  );
  if (!claudeReady) await dlg.getByTestId("runner-note").first().waitFor();
  else await page.waitForTimeout(1500);
  ok(
    (await dlg.getByTestId("runner-note").count()) > 0 === !claudeReady,
    `${tag} the editor notes a missing Claude Code runner only when there is none (ready: ${claudeReady})`,
  );
  ok(
    /Claude Code sonnet \(runner\)/.test(await dlg.innerText()),
    `${tag} the rule's sentence names the coding agent`,
  );
  await shot("editor-copy");
  await dlg.getByRole("button", { name: "Create" }).click();
  await page.getByText("Security review of pull requests (copy)").waitFor();
  const autos = await api("GET", `/projects/${key}/automations`);
  const copy = autos.find((a) => a.name.endsWith("(copy)"));
  ok(
    copy?.rule.actions[0].models?.[0]?.model === "fake/nemotron-3-ultra" &&
      copy.rule.pr?.skip_labels?.includes("dependencies"),
    `${tag} the copy saved its models and filter`,
  );
  const agent = copy?.rule.actions[0].models?.find((m) => m.cli);
  ok(
    agent?.cli === "claude" && agent.model === "sonnet" && !agent.provider_id,
    `${tag} the copy saved the coding agent on a runner`,
  );

  await api("DELETE", `/automations/${copy.id}`);

  await page.goto(`${base}/p/${key}/settings?tab=integrations`);
  const sw = page.getByRole("switch", { name: /Bots on this repo/ });
  await sw.waitFor();
  ok(
    (await sw.getAttribute("aria-checked")) === "true",
    `${tag} bots on by default`,
  );
  await shot("integrations");
  await overflow("integrations");

  {
    const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
    db.exec("PRAGMA busy_timeout = 5000");
    db.prepare(
      "INSERT INTO server_settings(key, value) VALUES ('review_arm_pins', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
    ).run(JSON.stringify({ "diff/verify": "on", "diff+files/verify": "on" }));
    db.close();
  }
  const before = seen.reviews.length;
  const payload = JSON.stringify({
    action: "opened",
    pull_request: {
      number: 5,
      title: "Fix login",
      body: "",
      html_url: `https://github.example/me/app${tag}/pull/5`,
      state: "open",
      merged: false,
      draft: false,
      user: { login: "sam" },
      head: { ref: "fix-login", sha: "1111111aaaa" },
      base: { ref: "main" },
      labels: [],
    },
    repository: { full_name: `me/app${tag}`, owner: { type: "User" } },
  });
  const sig =
    "sha256=" +
    crypto.createHmac("sha256", SECRET).update(payload).digest("hex");
  const hook = await page.request.post(
    `${base}/api/v1/hooks/forge/${forge.id}`,
    {
      headers: {
        "Content-Type": "application/json",
        "X-GitHub-Event": "pull_request",
        "X-Hub-Signature-256": sig,
      },
      data: payload,
    },
  );
  ok(hook.ok(), `${tag} webhook accepted (${hook.status()})`);
  let runs = [];
  for (let i = 0; i < 100; i++) {
    runs = await api("GET", `/projects/${key}/bot-runs`);
    if (runs[0] && !["queued", "running"].includes(runs[0].status)) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  ok(
    runs[0]?.status === "done",
    `${tag} review run done: ${runs[0]?.status} ${runs[0]?.summary ?? ""} ${runs[0]?.error ?? ""}`,
  );
  ok(seen.reviews.length === before + 1, `${tag} one review posted`);
  const posted = seen.reviews.at(-1);
  ok(
    posted?.event === "COMMENT" &&
      posted.comments?.[0]?.path === "app/login.go" &&
      posted.comments[0].line === 11 &&
      /```suggestion/.test(posted.comments[0].body),
    `${tag} the review has the line comment with a suggestion`,
  );
  ok(
    seen.verified > 0 && /checked by `fake\/space-bunny`/.test(posted.body),
    `${tag} the verifier checked the finding before it was posted`,
  );
  await page.goto(`${base}/p/${key}/settings?tab=bots`);
  await page.getByText("The nil check is inverted").waitFor();
  ok(
    await page
      .getByText(/checked by .*space-bunny/)
      .first()
      .isVisible(),
    `${tag} the finding says the verifier checked it`,
  );
  ok(
    await page.getByText("P1").first().isVisible(),
    `${tag} severity badge shows`,
  );
  await page.getByText(/Answered by nemotron-3-ultra/).click();
  await page.getByText("rate limited").first().waitFor();
  ok(true, `${tag} the model log shows the rate-limited first model`);
  await shot("bots");
  await overflow("bots");
  await page.getByRole("radio", { name: "Failed" }).click();
  await page.getByText("No runs match.").waitFor();
  await page.getByRole("radio", { name: "All" }).click();
  await page.getByText("The nil check is inverted").waitFor();
  ok(true, `${tag} the status filter narrows the runs`);
  await page
    .getByRole("link", { name: "Review pull requests" })
    .first()
    .click();
  await page.getByText("Edit automation").waitFor();
  ok(true, `${tag} the run's rule opens in the editor`);
  await ctx.close();
}

try {
  for (const w of looks([1280, 390])) await run(w);
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
