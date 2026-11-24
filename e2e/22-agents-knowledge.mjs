// Knowledge use (WP-D15) in a real browser at desktop and phone widths,
// light and dark: an agent looks a term, a page and a spec up and writes a
// task naming them (and a word to avoid); the Agents page's Knowledge tab
// shows the word to avoid first, the most used items and what agents read;
// the glossary list says how much each term is used; the entry, the page
// and the design note each show their own usage.
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
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  const settle = () => page.waitForTimeout(500);
  const visible = (text) =>
    page.getByText(text).filter({ visible: true }).first().isVisible();

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `KN${width}${scheme[0]}`.toUpperCase();
  await api("POST", "/projects", { name: `Knowledge ${tag}`, key });
  const { token } = await api("POST", "/me/tokens", {
    name: `knowledge bot ${tag}`,
    agent: "anthropic/claude-code",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const agent = (m, p, d) =>
    call(m, p, d, AG).then((r) => (r.status() === 204 ? null : r.json()));

  const claim = await api("POST", "/glossary", {
    term: "Claim",
    definition: "An agent's lease on a task, renewed while it works.",
    aliases: ["lease"],
    avoid: ["ticket"],
    project: key,
  });
  await api("POST", "/glossary", {
    term: "Step",
    definition: "Where a task is in its workflow.",
    project: key,
  });
  const decision = await api("POST", "/pages", {
    title: "Agents take work in order",
    category: "decision",
    project: key,
    body: "Pinned work first, then by urgency.",
  });
  const note = await api("POST", "/design", {
    title: "Agent queue",
    kind: "feature",
    project: key,
    body: "What each agent takes next, in order.",
  });
  const code = `${key}-${decision.code}`;

  await agent("GET", `/glossary/lookup?term=lease&project=${key}`);
  await agent("GET", `/pages/${decision.id}`);
  await agent(
    "GET",
    `/spec?topic=${encodeURIComponent("Agent queue")}&project=${key}`,
  );
  const task = await agent("POST", "/issues", {
    project: key,
    title: "Release the claim when a ticket closes",
    description: `Per ${code} and [[Agent queue]]: the lease ends with the task.`,
  });
  await api("POST", `/issues/${task.key}/comments`, {
    body: "Each claim should say which step it's at.",
  });

  await page.goto(`${base}/agents?tab=knowledge&project=${key}`);
  await page.getByText("Words to avoid that were written").waitFor();
  await settle();
  ok(await visible("“ticket”"), `${tag} the word to avoid is listed`);
  ok(await visible("What agents read"), `${tag} what agents read shows`);
  ok(
    await page
      .getByRole("listitem")
      .filter({ has: page.getByLabel("Looked up", { exact: true }) })
      .filter({ hasText: decision.title })
      .first()
      .isVisible(),
    `${tag} the agent's page lookup is listed`,
  );
  ok(
    await page
      .getByRole("list", { name: /Most used/ })
      .getByText("Claim")
      .isVisible(),
    `${tag} Claim is among the most used`,
  );
  await shot("knowledge-tab");

  await page.goto(`${base}/agents?tab=knowledge&project=${key}&agent=codex`);
  await page.getByText(/No knowledge used/).waitFor();
  ok(true, `${tag} another agent's knowledge use is empty`);
  await shot("knowledge-empty");

  await page.goto(`${base}/p/${key}/glossary`);
  await page.getByText("Claim", { exact: true }).first().waitFor();
  ok(
    await visible(/used \d+× ·/),
    `${tag} glossary rows say how much a term is used`,
  );
  await shot("glossary-list");

  await page.goto(`${base}/glossary/${claim.id}`);
  const usage = page.getByRole("region", { name: "Usage" });
  await usage.waitFor();
  const text = await usage.innerText();
  ok(
    /Looked up once/.test(text),
    `${tag} entry usage counts the lookup (${text.slice(0, 80)})`,
  );
  ok(
    /as “lease”/.test(text),
    `${tag} entry usage says the alias it was found by`,
  );
  ok(
    /“ticket”/.test(text) &&
      (await usage.getByLabel("Word to avoid", { exact: true }).count()) > 0,
    `${tag} entry usage shows the word to avoid`,
  );
  ok(
    /a comment on/.test(text),
    `${tag} entry usage shows the person's comment`,
  );
  await usage.scrollIntoViewIfNeeded();
  await shot("glossary-entry");

  await page.goto(`${base}/pages/${decision.id}`);
  const pu = page.getByRole("region", { name: "Usage" });
  await pu.waitFor();
  const ptext = await pu.innerText();
  ok(
    /Looked up once/.test(ptext) &&
      /Named in 1 place|named in 1 place/.test(ptext),
    `${tag} page usage (${ptext.slice(0, 80)})`,
  );
  ok(ptext.includes(code), `${tag} page usage shows the code it was named by`);
  await pu.scrollIntoViewIfNeeded();
  await shot("page-usage");

  await page.goto(`${base}/design/${note.id}`);
  const nu = page.getByRole("region", { name: "Usage" });
  await nu.waitFor();
  const ntext = await nu.innerText();
  ok(
    /Looked up once/.test(ntext) && /named in 1 place/.test(ntext),
    `${tag} design note usage (${ntext.slice(0, 80)})`,
  );
  await nu.scrollIntoViewIfNeeded();
  await shot("design-usage");

  if (width < 500) {
    await page.goto(`${base}/agents?tab=knowledge&project=${key}`);
    await page.getByText("Words to avoid that were written").waitFor();
    const over = await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    );
    ok(over <= 0, `${tag} the Knowledge tab fits the screen (${over}px over)`);
  }
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
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
