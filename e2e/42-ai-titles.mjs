// AI titles (WP-67): long text typed into a quick-add box gets a short title
// from the title model, the text becomes the description, board cards and
// list rows show the description on hover, and a failing model loses nothing.
// The model is a tiny OpenAI-compatible server run here.
// Needs user robin (run 01-signup-flow first).
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

// The fake model: titles for "Text to title" prompts, points for estimates,
// or a 500 when mode is "fail".
let mode = "ok";
let titleCalls = 0;
const fake = http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const reply = (obj) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify(obj));
    };
    if (req.url.endsWith("/models")) return reply({ data: [{ id: "fake-title" }] });
    const msgs = JSON.parse(body || "{}").messages ?? [];
    const prompt = msgs.find((m) => m.role === "user")?.content ?? "";
    let content = '{"points": 3, "confidence": "high", "reason": "Small."}';
    if (prompt.startsWith("Text to title")) {
      titleCalls++;
      if (mode === "fail") {
        res.statusCode = 500;
        return res.end("model is down");
      }
      content = JSON.stringify({ title: "Speed up boards with many cards." });
    }
    reply({ choices: [{ message: { content } }] });
  });
});
await new Promise((r) => fake.listen(0, "127.0.0.1", r));
const fakeURL = `http://127.0.0.1:${fake.address().port}`;

const text = (tag) =>
  `The board gets slow when a column has more than a hundred cards (${tag}). ` +
  "Scrolling stutters and drag and drop lags behind the pointer. It started after the last release.";

const browser = await chromium.launch();
let providerId = null;
for (const vp of looks([
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
])) {
  const tag = `${vp.width}px`;
  const ctx = await browser.newContext({ viewport: vp });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`pageerror (${tag}): ` + e.message));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      !/status of 500/.test(m.text()) &&
      fail(`console (${tag}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = {
    get: async (p) => (await page.request.get(base + "/api/v1" + p, { headers: H })).json(),
    send: async (m, p, d) =>
      (await page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })).json(),
  };

  if (providerId === null) {
    const prov = await api.send("POST", "/ai/providers", {
      kind: "openai",
      name: "fake titles",
      base_url: fakeURL,
    });
    providerId = prov.id;
    await api.send("PUT", "/ai/tasks/title", {
      provider_id: providerId,
      model: "fake-title",
      fallbacks: [],
    });
    await api.send("PUT", "/ai/tasks/estimate", {
      provider_id: providerId,
      model: "fake-title",
      fallbacks: [],
    });
  }

  const key = "AT" + vp.width;
  if (!(await api.get("/projects")).some((p) => p.key === key))
    await api.send("POST", "/projects", { name: "AI titles " + vp.width, key });
  const proj = await api.get(`/projects/${key}`);
  ok(proj.settings.auto_title === true, `${tag}: a new project has auto_title on`);

  mode = "ok";
  const t1 = text(tag);
  await page.goto(`${base}/p/${key}/board`);
  await page.waitForSelector("section[data-col]");
  const col = page.locator("section[data-col]").first();
  await col.getByRole("button", { name: "Add issue" }).click();
  const box = col.getByLabel("New issue title");
  await box.fill(t1);
  await box.press("Enter");
  const card = col.locator("[data-nav-key]").filter({ hasText: "Speed up boards with many cards" });
  await card.waitFor();
  ok(true, `${tag}: the card got the model's title`);
  const hover = await card.locator("[title]").first().getAttribute("title");
  ok(
    hover?.includes("Speed up boards with many cards") && hover.includes("The board gets slow when a column"),
    `${tag}: hovering the card shows the description: ${JSON.stringify(hover?.slice(0, 80))}`,
  );
  await page.screenshot({ path: `${out}/${tag}-board-card.png` });
  const key1 = await card.getAttribute("data-nav-key");
  const issue1 = await api.get(`/issues/${key1}`);
  ok(issue1.description === t1, `${tag}: the typed text is the description`);
  ok(issue1.title === "Speed up boards with many cards", `${tag}: the title is short: ${issue1.title}`);
  const hist = await api.get(`/issues/${key1}/history`);
  ok(
    JSON.stringify(hist).includes("AI title"),
    `${tag}: history credits the AI title`,
  );

  await page.goto(`${base}/p/${key}/list`);
  const row = page.locator(`[data-nav-key="${key1}"]`);
  await row.waitFor();
  const rowTitles = await row.locator("[title]").evaluateAll((els) => els.map((e) => e.title));
  ok(
    rowTitles.some((t) => t.includes("The board gets slow when a column")),
    `${tag}: hovering a list row shows the description`,
  );
  await page.screenshot({ path: `${out}/${tag}-list-row.png` });

  mode = "fail";
  const before = titleCalls;
  const t2 = text(tag + " again");
  const made = await api.send("POST", "/issues", { project: key, title: t2 });
  for (let n = 0; n < 50 && titleCalls === before; n++) await page.waitForTimeout(100);
  await page.waitForTimeout(500);
  const kept = await api.get(`/issues/${made.key}`);
  ok(titleCalls > before, `${tag}: the model was asked`);
  ok(kept.title === t2 && !kept.description, `${tag}: a failed model leaves the text as typed`);

  mode = "ok";
  await page.goto(`${base}/p/${key}/settings`);
  const sw = page.getByRole("switch", { name: "Write titles for long text automatically" });
  await sw.waitFor();
  ok((await sw.getAttribute("aria-checked")) === "true", `${tag}: the settings switch is on`);
  await sw.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `${out}/${tag}-settings.png` });
  await sw.click();
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await page.getByText("Settings saved").waitFor();
  const off = await api.get(`/projects/${key}`);
  ok(off.settings.auto_title === false, `${tag}: turning it off saves`);
  const calls = titleCalls;
  const quiet = await api.send("POST", "/issues", { project: key, title: text(tag + " quiet") });
  await page.waitForTimeout(1500);
  ok(
    titleCalls === calls && (await api.get(`/issues/${quiet.key}`)).title.startsWith("The board gets slow"),
    `${tag}: with it off the model isn't asked`,
  );
  await ctx.close();
}

// Leave no model behind for later suites.
if (providerId !== null) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await page.request.delete(`${base}/api/v1/ai/providers/${providerId}`, { headers: H });
  await ctx.close();
}
await browser.close();
fake.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
