// The safety drill (WP-278): Admin → AI runs the screen's model over task
// texts with known answers and shows how it did. A fake model here flags
// every bad text but the subtle one in prose, so the drill shows one wrong
// and the admin is notified. Desktop and phone widths, light and dark.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";
import http from "node:http";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

// Flags the texts that name these; the prose-only one ("production
// server") slips through.
const BAD = ["Synology NAS", "curl -sSfL", "FORGEJO_TOKEN", "automated screen"];
const DRIFT = [
  "Send product analytics",
  "Postgres and Redis",
  "in Svelte",
  "paid billing tiers",
  "AWS Parameter Store",
  "telemetry.example.com",
];
const srv = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    const send = (code, body) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.url === "/v1/models")
      return send(200, { data: [{ id: "fake/screen-1" }] });
    if (req.url === "/v1/chat/completions") {
      let answer;
      if (raw.includes("Check pull request")) {
        const drifts = DRIFT.some((b) => raw.includes(b));
        answer = drifts
          ? { kind: "contradicts", goal: 1, reason: "works against the goal" }
          : { kind: "none", goal: 0, reason: "" };
      } else {
        const bad = BAD.some((b) => raw.includes(b));
        answer = bad
          ? { flags: ["other_hosts"], reason: "acts on another machine" }
          : { flags: [], reason: "" };
      }
      return send(200, {
        choices: [{ message: { content: JSON.stringify(answer) } }],
      });
    }
    send(404, {});
  });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

const runs = [
  [1440, "light"],
  [390, "light"],
  [390, "dark"],
];
for (const [n, [width, scheme]] of runs.entries()) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      !/Failed to load resource/.test(m.text()) &&
      fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  await login(page);
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => r.json());
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  const shot = (name) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  const section = page.locator("section", {
    has: page.getByRole("heading", { name: "Safety drill" }),
  });

  if (n === 0) {
    if (!(await api("GET", "/ai/safety-drill")).configured) {
      await page.goto(`${base}/admin?tab=ai`);
      await page.getByText("there is nothing to drill").waitFor();
      ok(
        await section.getByRole("button", { name: "Run now" }).isDisabled(),
        `${tag} without a screen model there is nothing to run`,
      );
    }
    const provider = await api("POST", "/ai/providers", {
      kind: "openai",
      name: "Fake screen",
      base_url: `${fake}/v1`,
    });
    await api("PUT", "/ai/tasks/task_safety", {
      provider_id: provider.id,
      model: "fake/screen-1",
      fallbacks: [],
    });
    await page.goto(`${base}/admin?tab=ai`);
    await section.getByRole("button", { name: "Run now" }).click();
    await page.getByText("17 of 18 right").waitFor({ timeout: 30000 });
    ok(true, `${tag} Run now runs the drill and shows the result`);
    const d = await api("GET", "/ai/safety-drill");
    ok(
      d.last?.failed === 1 &&
        d.last.cases.find((c) => !c.pass)?.name === "prose_only",
      `the subtle one is the miss: ${JSON.stringify(d.last?.cases?.filter((c) => !c.pass))}`,
    );
    ok(
      d.models?.[0]?.model.includes("fake/screen-1") &&
        d.models[0].passed === 17 &&
        d.models[0].total === 18,
      `pass rate per model: ${JSON.stringify(d.models)}`,
    );
    const notes = await api("GET", "/notifications");
    const list = Array.isArray(notes)
      ? notes
      : (notes.notifications ?? notes.items ?? []);
    ok(
      list.some((x) => x.title === "The safety screen let a bad task through"),
      "the admin is notified",
    );
  } else {
    await page.goto(`${base}/admin?tab=ai`);
    await page.getByText("17 of 18 right").waitFor();
  }
  const miss = section.locator("li[data-pass=false]");
  ok(
    (await miss.count()) === 1 &&
      (await miss.getByText("Subtle, in prose only").isVisible()),
    `${tag} the miss is marked`,
  );
  ok(
    await miss.getByText("cleared").isVisible(),
    `${tag} it says the model cleared it`,
  );
  ok(
    await section.getByText("17/18 (94%)").isVisible(),
    `${tag} the model's pass rate shows`,
  );
  await section.scrollIntoViewIfNeeded();
  await overflow("admin ai");
  await shot("drill");
  await ctx.close();
}

// Leave the screen as it was for later suites.
{
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await login(page);
  await page.request.fetch(base + "/api/v1/ai/tasks/task_safety", {
    method: "PUT",
    headers: H,
    data: { provider_id: 0, model: "" },
  });
  await ctx.close();
}
await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
