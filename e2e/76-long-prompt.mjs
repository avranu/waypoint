// A session's long prompt on its page (WP-860) in a real browser at desktop
// and phone widths: the timeline line shows its start, and a click opens the
// whole prompt, newlines and all, without sideways scroll.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();
const tail = "and the last line, past the first 300 characters, is still here.";
const prompt = `Look into why the guard refused this:\n\n${"npx tsc --noEmit -p . && ".repeat(20)}\n\n${tail}`;

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const api = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  const overflow = () => page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const { token } = await api("POST", "/me/tokens", { name: `long prompt ${tag}`, agent: "anthropic/claude-opus-5-5" });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const cli = `e2e-long-prompt-${n}`;
  const w = { session_id: cli, machine: "box", cwd: "/work/app" };
  await api("POST", "/sessions/hook", { events: [{ event: "SessionStart", ...w }] }, AG);
  await api("POST", "/sessions/hook", { events: [{ event: "UserPromptSubmit", ...w, prompt }] }, AG);
  await api("POST", "/sessions/hook", { events: [{ event: "UserPromptSubmit", ...w, prompt: "a short one" }] }, AG);
  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  const id = db.prepare(`SELECT id FROM agent_sessions WHERE cli_id = ?`).get(cli).id;
  db.close();

  await page.goto(`${base}/agents/sessions/${id}`);
  const line = page.getByRole("button", { name: /^Look into why the guard refused this/ });
  await line.waitFor();
  ok((await page.getByText(tail).count()) === 0, `${tag} the line holds only the prompt's start`);
  ok((await page.getByRole("button", { name: "a short one" }).count()) === 0, `${tag} a short prompt doesn't open`);
  await page.screenshot({ path: `${out}/line-${width}-${scheme}.png`, fullPage: true });
  await line.click();
  const whole = page.locator("pre", { hasText: tail });
  await whole.waitFor();
  ok((await whole.innerText()).includes("Look into why the guard refused this:\n\nnpx tsc"), `${tag} the whole prompt keeps its lines`);
  ok(!(await overflow()), `${tag} no sideways scroll with the prompt open`);
  await page.waitForTimeout(250);
  await page.screenshot({ path: `${out}/open-${width}-${scheme}.png`, fullPage: true });
  ok((await line.getAttribute("aria-expanded")) === "true", `${tag} the line says it's open`);
  await line.click();
  await whole.waitFor({ state: "detached" });
  ok((await line.getAttribute("aria-expanded")) === "false", `${tag} a second click closes it`);
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
