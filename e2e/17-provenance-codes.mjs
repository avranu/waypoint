// Provenance, review, page codes and agents' clocks (WP-D11) in a real
// browser at desktop and phone widths: typing a project key keeps focus;
// an agent's issue and comment say which agent wrote them until a person
// marks them reviewed; an agent's paused claim shows as paused; pages show
// their codes (an imported "PL13: …" title keeps PL13).
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const settle = () => page.waitForTimeout(400);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const n = `${width}${scheme[0]}`.toUpperCase();
  const key = `PV${n}`;
  await api("POST", "/projects", { name: `Provenance ${tag}`, key });
  const { token } = await api("POST", "/me/tokens", {
    name: `prov bot ${tag}`,
    agent: "anthropic/claude-haiku-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const agent = (m, p, d) => call(m, p, d, AG);

  await page.goto(`${base}/p/${key}/settings?tab=general`);
  const keyInput = page.getByRole("textbox", { name: "Key" });
  await keyInput.click();
  await keyInput.press("End");
  await page.keyboard.type("ZQ");
  ok(
    (await keyInput.inputValue()) === `${key}ZQ`,
    `${tag} key field took every letter (${await keyInput.inputValue()})`,
  );
  ok(
    await keyInput.evaluate((el) => el === document.activeElement),
    `${tag} key field kept focus after the note appeared`,
  );
  ok(
    await page.getByText(/old keys keep working/).isVisible(),
    `${tag} key change note shows`,
  );
  await shot("key-typing");
  await page.getByRole("button", { name: "Discard" }).click();

  const made = await (
    await agent("POST", "/issues", {
      project: key,
      title: `Agent-written ${tag}`,
      description: "The model wrote this.",
    })
  ).json();
  await agent("POST", `/issues/${made.key}/comments`, {
    body: "I checked the parser; it's fine.",
  });
  await page.goto(`${base}/issue/${made.key}`);
  const written = page.getByText("Written by claude-haiku-5-5");
  await written.waitFor();
  ok(await written.isVisible(), `${tag} issue says which agent wrote it`);
  ok(
    (await page.getByText("Not reviewed").count()) >= 2,
    `${tag} issue text and agent comment are marked not reviewed`,
  );
  ok(
    (await page
      .getByText("claude-haiku-5-5", { exact: true })
      .filter({ visible: true })
      .count()) > 0,
    `${tag} comment author is the agent`,
  );
  ok(
    (await page
      .getByText(/via Robin/)
      .filter({ visible: true })
      .count()) === 0,
    `${tag} agent work isn't credited to the person whose token it used`,
  );
  await shot("agent-issue");
  await page.getByRole("button", { name: "Mark reviewed" }).first().click();
  await page.getByText("Reviewed by").first().waitFor();
  await page.getByText("Not reviewed").first().waitFor({ state: "hidden" });
  ok(
    (await page.getByText("Not reviewed").count()) === 0,
    `${tag} reviewing the issue covers its agent comments`,
  );
  await shot("agent-issue-reviewed");

  const work = await api("POST", "/issues", {
    project: key,
    title: `Delegated ${tag}`,
    agent: "any",
  });
  await agent("POST", `/agent/take/${work.key}`, {});
  await agent("POST", `/agent/pause/${work.key}`, {});
  await page.goto(`${base}/issue/${work.key}`);
  await page
    .getByText(/claude-haiku-5-5 paused/)
    .or(page.locator('[title*="is idle (its clock is paused)"]'))
    .filter({ visible: true })
    .first()
    .waitFor();
  ok(true, `${tag} idle agent's claim shows as paused`);
  await shot("claim-paused");
  await agent("POST", `/agent/resume/${work.key}`, {});
  await page.reload();
  await page
    .getByText(/claude-haiku-5-5 is on it/)
    .or(page.locator('[title*="is working on this"]'))
    .filter({ visible: true })
    .first()
    .waitFor();
  ok(true, `${tag} resumed agent's claim shows as working`);

  const p1 = await api("POST", "/pages", {
    title: "Roadmap",
    category: "plan",
    project: key,
  });
  const p2 = await api("POST", "/pages", {
    title: "PL13: A provider could refuse",
    category: "plan",
    project: key,
  });
  ok(
    p1.code === "PL1" && p2.code === "PL13",
    `${tag} codes ${p1.code} ${p2.code}`,
  );
  ok(
    p2.title === "A provider could refuse",
    `${tag} imported code left the title`,
  );
  const byCode = await api("GET", `/pages/PL13?project=${key}`);
  ok(byCode.id === p2.id, `${tag} page found by code`);
  await agent("POST", "/pages", {
    title: "Agent idea",
    category: "idea",
    project: key,
  });
  await page.goto(`${base}/pages/${p2.id}`);
  await page
    .getByText("PL13", { exact: true })
    .filter({ visible: true })
    .first()
    .waitFor();
  ok(true, `${tag} page view shows its code`);
  await shot("page-code");
  const term = await api("POST", "/glossary", {
    term: "Share",
    definition: "Access to one issue and everything beneath it.",
    project: key,
    avoid: ["invite to issue"],
  });
  await page.goto(`${base}/glossary/${term.id}`);
  await page
    .getByText("Avoid", { exact: true })
    .filter({ visible: true })
    .first()
    .waitFor();
  ok(
    await page
      .getByText("invite to issue")
      .filter({ visible: true })
      .first()
      .isVisible(),
    `${tag} glossary entry lists its words to avoid`,
  );
  await page.getByRole("button", { name: "Add a word to avoid" }).click();
  const add = page.getByRole("textbox", { name: "Add a word to avoid" });
  await add.fill("grant access");
  await add.press("Enter");
  await settle();
  const saved = await api("GET", `/glossary/${term.id}`);
  ok(
    saved.avoid?.includes("grant access"),
    `${tag} added a word to avoid (${saved.avoid})`,
  );
  const look = await api(
    "GET",
    `/glossary/lookup?term=grant%20access&project=${key}`,
  );
  ok(
    look.instead?.[0]?.id === term.id,
    `${tag} looking up a word to avoid points at the term`,
  );
  await shot("glossary-avoid");
  await page.goto(`${base}/p/${key}/pages`).catch(() => {});
  await page.goto(`${base}/pages?project=${key}`);
  await settle();
  await shot("pages-tree");
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
