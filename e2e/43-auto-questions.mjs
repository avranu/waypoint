// Questions that end an agent's turn (WP-96) in a real browser at desktop and
// phone widths: the Stop hook files one as a request marked "auto"; it waits
// under "Waiting on you"; "Not a question" dismisses it and records why. A
// request the agent moved on from is out of the Inbox until "Expired" is on.
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
let agent;

async function run(width) {
  const tag = `${width}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const sid = `e2e-auto-${width}`;
  if (!agent) {
    const { token } = await api("POST", "/me/tokens", { name: "auto agent", agent: "anthropic/claude-code" });
    agent = { ...H, Authorization: `Bearer ${token}`, "X-Waypoint-Machine": "box" };
  }
  const hook = (event, extra = {}) =>
    call("POST", "/sessions/hook", { events: [{ event, session_id: sid, machine: "box", cwd: "/work/app", repo: "acme/app", branch: "main", ...extra }] }, agent);
  await hook("SessionStart");
  await hook("UserPromptSubmit", { prompt: "Fix the matcher", title: "Matcher" });
  const denied = `Allow Bash: rm -rf build ${width}?`;
  await call("POST", "/requests", { kind: "permission", title: denied, tool: "Bash", tool_input: { command: `rm -rf build ${width}` } }, { ...agent, "X-Waypoint-Session": sid });
  await hook("UserPromptSubmit", { prompt: "Never mind, go on" });
  await new Promise((r) => setTimeout(r, 50));
  const question = "Should a pin with no containing building fall back to the nearest one within 5 m?";
  await hook("Stop", { last_message: `The depot's pin is 1.7 m outside the Admin Building. ${question}` });

  await page.goto(base + "/inbox");
  const section = page.getByRole("region", { name: "Waiting on you" });
  await section.waitFor();
  const row = section.getByRole("button", { name: new RegExp(question.slice(0, 30)) });
  ok(await row.isVisible(), `${tag} the auto-filed question waits on robin`);
  ok(await row.getByText("auto", { exact: true }).isVisible(), `${tag} the row says it was filed automatically`);
  ok((await page.getByText(denied).count()) === 0, `${tag} the expired request is out of the Inbox`);
  const expiredBtn = page.getByRole("button", { name: "Expired" });
  ok(await expiredBtn.isVisible(), `${tag} an Expired filter is there`);
  await page.screenshot({ path: `${out}/inbox-${width}.png` });
  await expiredBtn.click();
  ok(await page.getByText(new RegExp(denied.slice(0, 20))).first().isVisible(), `${tag} Expired brings it back`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  await page.screenshot({ path: `${out}/inbox-expired-${width}.png` });

  await row.click();
  const sheet = page.getByRole("dialog", { name: /Request/ });
  await sheet.waitFor();
  await sheet.getByText(/Filed automatically from the end of the agent/).waitFor();
  await sheet.getByText("auto", { exact: true }).first().waitFor();
  ok(await sheet.getByText("auto", { exact: true }).first().isVisible(), `${tag} the sheet marks it auto`);
  ok(await sheet.getByText(/Filed automatically from the end of the agent/).isVisible(), `${tag} the body says where it came from`);
  await page.screenshot({ path: `${out}/sheet-${width}.png` });
  await sheet.getByRole("button", { name: "Not a question" }).click();
  await sheet.getByText(/dismissed/i).first().waitFor();
  const list = await api("GET", "/requests?state=dismissed");
  const got = list.requests.find((r) => r.title === question);
  ok(got && got.note === "not a question" && got.auto, `${tag} the dismissal is recorded: ${got?.state} ${got?.note}`);
  await ctx.close();
}

for (const w of looks([1440, 390])) await run(w);
await browser.close();
process.exit(errors.length ? 1 : 0);
