// An assistant (WP-ADR1): a Hermes agent behind its gate, as its owner sees
// it in Waypoint, at desktop and phone widths, light and dark. Needs a real
// Hermes behind the fake gate (WP design #340, "Your assistant"):
//   ASSISTANT_DOOR=https://127.0.0.1:19443 ASSISTANT_CA=door-ca.pem
//   ASSISTANT_KEY=devkey ASSISTANT_PASSWORD=… (Hermes's dashboard login)
//   ASSISTANT_KEY_HEADER=… (optional: the header the door reads the key
//   from, when it isn't X-Gate-Key; door -header)
// with the scripted model (hermestest.FakeLLM). Without them it skips.
// Connects it; chats (streaming, a question it asks, edit); every tab
// loads without errors or sideways scroll; a gate approval shows; an issue
// starts a conversation and lists it; an automation asks it; freeze.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import https from "node:https";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
const NEED = ["ASSISTANT_DOOR", "ASSISTANT_CA", "ASSISTANT_KEY", "ASSISTANT_PASSWORD"];
const missing = NEED.filter((k) => !process.env[k]);
if (missing.length === NEED.length) {
  console.log(`skip: set ${NEED.join(", ")} to run it`);
  process.exit(0);
}
if (missing.length) {
  console.log(`FAIL set ${missing.join(", ")} too (or none of them, to skip)`);
  process.exit(1);
}
if (!fs.existsSync(process.env.ASSISTANT_CA)) {
  console.log(`FAIL ASSISTANT_CA: no file ${process.env.ASSISTANT_CA}`);
  process.exit(1);
}
const door = process.env.ASSISTANT_DOOR;
fs.mkdirSync(out, { recursive: true });
const ca = fs.readFileSync(process.env.ASSISTANT_CA, "utf8");
const key = process.env.ASSISTANT_KEY;
const keyHeader = process.env.ASSISTANT_KEY_HEADER || "X-Gate-Key";
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const TABS = ["chat", "activity", "schedules", "skills", "memory", "tools", "files", "usage", "settings"];

// The fake gate's own controls, as Signal would act.
const gate = (path) =>
  new Promise((resolve, reject) => {
    const req = https.request(new URL(path, door), { method: "POST", ca, headers: { [keyHeader]: key } }, (res) => {
      let body = "";
      res.on("data", (d) => (body += d));
      res.on("end", () => resolve(body));
    });
    req.on("error", reject);
    req.end();
  });

const browser = await chromium.launch();
let id;

async function setup(page) {
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  const list = await (await call("GET", "/assistants")).json();
  const have = list.find((a) => a.name === "Sage");
  if (have) return have.id;
  const r = await call("POST", "/assistants", {
    name: "Sage", url: door, ca_pem: ca, key, key_header: process.env.ASSISTANT_KEY_HEADER || "",
    username: "waypoint", password: process.env.ASSISTANT_PASSWORD,
  });
  ok(r.ok(), `connect: ${r.status()}`);
  const a = await r.json();
  const test = await (await call("POST", `/assistants/${a.id}/test`)).json();
  ok(test.every((s) => s.ok), `connect: the door, the gate and Hermes answer (${JSON.stringify(test)})`);
  return a.id;
}

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text())) fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.keyboard.press("Enter");
  await page.waitForURL((u) => !u.pathname.startsWith("/login"));
  id ??= await setup(page);

  // ---- chat ----
  await page.goto(`${base}/assistant/${id}`);
  const box = page.getByLabel("Message Sage");
  await box.waitFor();
  const word = `Hello ${width} ${scheme}`;
  await box.fill(word);
  await page.keyboard.press("Enter");
  await page.waitForURL(/[?&]s=/);
  await page.getByText("Anything else?").last().waitFor();
  ok(true, `${tag} a reply streams in`);
  await shot("assistant-chat");
  await overflow("chat");
  await page.reload();
  await page.getByText(word).first().waitFor();
  ok(true, `${tag} the conversation is still there after a reload`);

  await box.fill("Please ask me which day");
  await page.keyboard.press("Enter");
  await page.getByRole("button", { name: "Friday" }).click();
  await page.getByRole("button", { name: "Answer" }).click();
  await page.getByText("Done. The tool said:").last().waitFor();
  ok(true, `${tag} it asks a question in the chat, and gets the answer`);

  await page.getByText("Please ask me which day").first().hover();
  await page.getByLabel("Edit and send again").click();
  await box.fill(`Edited ${word}`);
  await page.keyboard.press("Enter");
  await page.getByText(`Edited ${word}`).first().waitFor();
  ok(true, `${tag} the last question can be edited and sent again`);

  // ---- every tab ----
  for (const t of TABS.slice(1)) {
    await page.goto(`${base}/assistant/${id}?tab=${t}`);
    await page.getByRole("tab", { name: new RegExp(t, "i") }).first().waitFor();
    await page.waitForTimeout(1200);
    const text = await page.getByRole("tabpanel").innerText();
    ok(text.trim().length > 20, `${tag} the ${t} tab shows something`);
    await overflow(t);
    await shot(`assistant-${t}`);
  }

  // ---- the gate holds an action for Signal ----
  const code = await gate(`/dev/ask?tool=send_email&summary=${encodeURIComponent(`email the plumber (${tag})`)}`);
  await call("POST", `/assistants/${id}/check`);
  await page.goto(`${base}/assistant/${id}`);
  await page.getByText(code).first().waitFor();
  ok(true, `${tag} an approval waiting on Signal shows (${code})`);
  await gate(`/dev/decide?code=${code}&approve=0`);
  await call("POST", `/assistants/${id}/check`);

  // ---- an issue starts a conversation and lists it ----
  const projects = await (await call("GET", "/projects")).json();
  if (!projects.some((p) => p.key === "ASST")) await call("POST", "/projects", { name: "Errands", key: "ASST" });
  const issue = await (await call("POST", "/issues", { project: "ASST", title: `Book the plumber ${tag}` })).json();
  await page.goto(`${base}/issue/${issue.key}`);
  await page.getByRole("link", { name: "Ask Sage" }).click();
  await box.waitFor();
  ok((await box.inputValue()).includes(issue.key), `${tag} asking from an issue names it`);
  await box.press("End");
  await box.type("please book it");
  await page.keyboard.press("Enter");
  await page.waitForURL(/[?&]s=/);
  await page.getByText("Anything else?").last().waitFor();
  await page.goto(`${base}/issue/${issue.key}`);
  await page.getByRole("link", { name: new RegExp(issue.key) }).first().waitFor();
  ok((await page.locator("section", { hasText: "Assistant" }).getByRole("link").count()) >= 2, `${tag} the issue lists its conversation`);

  await ctx.close();
}

async function automation() {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.keyboard.press("Enter");
  await page.waitForURL((u) => !u.pathname.startsWith("/login"));
  const call = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
  const a = await (await call("POST", "/projects/ASST/automations", {
    name: "Ask Sage", enabled: true,
    rule: { trigger: { event: "manual" }, actions: [{ type: "ask_assistant", id: "ask", prompt: "hello from an automation" }] },
  })).json();
  await call("POST", `/automations/${a.id}/run`, {});
  let run;
  for (let i = 0; i < 60; i++) {
    run = (await (await call("GET", `/automations/${a.id}/runs`)).json())[0]; // newest first
    if (run && !["queued", "running", "waiting"].includes(run.status)) break;
    await new Promise((r) => setTimeout(r, 500));
  }
  if (!run) throw new Error("the automation never started a run");
  const detail = await (await call("GET", `/automation-runs/${run.id}`)).json();
  const step = detail.steps?.find((s) => s.step_id === "ask");
  ok(run.status === "succeeded" && /hello from an automation/.test(step?.outputs?.Text ?? ""), `an automation asks the assistant and keeps its reply (${run.status})`);

  // ---- freeze, then Signal thaws it ----
  await page.goto(`${base}/assistant/${id}`);
  await page.getByRole("button", { name: "Freeze" }).click();
  await page.getByRole("button", { name: "Freeze now" }).click();
  await page.getByText("is frozen.").waitFor();
  ok(await page.getByLabel("Message Sage").isDisabled(), "frozen: the chat can't send");
  await page.screenshot({ path: `${out}/assistant-frozen.png` });
  await gate("/dev/thaw");
  await call("POST", `/assistants/${id}/check`);
  await ctx.close();
}

try {
  for (const [w, s] of looks([[1280, "light"], [390, "dark"]])) await run(w, s);
  await automation();
} finally {
  await browser.close();
}
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("assistant: all good");
