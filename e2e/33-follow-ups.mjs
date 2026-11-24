// Follow-ups (WP-D22) in a real browser at desktop and phone widths, light
// and dark: on an agent's session page, queue "start a fresh agent" (model and
// prompt) for when it finishes; a stop with a background task still running
// holds it; the stop where the agent truly finishes runs it (without a runner
// on that machine it fails and says so); then queue "tell it what's next",
// which reaches the agent at its next stop and waits for it to finish again.
// The Sessions tab shows what is queued.
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
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d, headers) =>
    call(m, p, d, headers).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );
  const dialogFits = async (where) =>
    ok(
      await page.evaluate(() => {
        const d = document.querySelector('[role="dialog"]');
        return !d || d.scrollWidth <= d.clientWidth + 1;
      }),
      `${tag} the dialog fits its width (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const { token } = await api("POST", "/me/tokens", { name: `haiku ${n}`, agent: "anthropic/claude-haiku-5-5" });
  const agent = { ...H, Authorization: `Bearer ${token}` };
  const where = { session_id: `cli-fu-${n}`, machine: `fu-box-${n}`, cwd: "/work/parser", branch: "feat/parser" };
  const hook = (event, extra = {}) => api("POST", "/sessions/hook", { events: [{ event, ...where, ...extra }] }, agent);
  const { session } = await hook("SessionStart", { model: "claude-haiku-5-5", source: "startup" });
  await hook("UserPromptSubmit", { prompt: "Build the parser", title: `Parser ${n}` });

  await page.goto(`${base}/agents/sessions/${session}`);
  const panel = page.getByTestId("follow-up");
  await panel.getByRole("button", { name: "Start a fresh agent" }).waitFor();
  ok(true, `${tag} the session page offers to queue a fresh agent`);
  await shot("empty");
  await overflow("empty panel");

  await panel.getByRole("button", { name: "Start a fresh agent" }).click();
  const dlg = page.getByRole("dialog", { name: "When this agent finishes" });
  await dlg.waitFor();
  await dlg.getByLabel("Model", { exact: true }).fill("opus");
  const prompt = dlg.getByLabel("Prompt", { exact: true });
  await prompt.click();
  await prompt.fill("Look over what the Haiku agent did on feat/parser, then add the error recovery.");
  await shot("editor");
  await dialogFits("editor");
  await dlg.getByRole("button", { name: "Queue" }).click();
  await dlg.waitFor({ state: "hidden" });
  await panel.getByText("Waits for the agent to finish.").waitFor();
  ok(/fresh agent/i.test(await panel.innerText()), `${tag} the queued step is listed (${(await panel.innerText()).slice(0, 120)})`);
  await shot("armed");
  await overflow("armed");

  await hook("Stop", {
    last_message: "Started the test suite in the background.",
    pending: { tasks: [{ type: "shell", status: "running", description: "go test ./..." }], crons: [] },
  });
  await panel.getByText("Still running a background task.").waitFor();
  await page.getByTestId("session-pending").getByText("still running a background task").waitFor();
  ok(true, `${tag} a background task holds it, and the header says so`);
  await shot("held");

  // No runner is on this machine, so the step runs and fails.
  const fin = await hook("Stop", { last_message: "The parser is done; tests pass.", pending: { tasks: [], crons: [] } });
  ok(!fin.await, `${tag} no hook wait for a step that doesn't talk to the agent`);
  await panel.getByText("Failed").first().waitFor({ timeout: 20000 });
  ok(/no runner named fu-box/i.test(await panel.innerText()), `${tag} the run says it needs a runner on that machine`);
  await shot("ran");
  await overflow("ran");

  await page.getByRole("button", { name: "Queue again" }).click();
  const dlg2 = page.getByRole("dialog", { name: "When this agent finishes" });
  await dlg2.waitFor();
  await dlg2.getByRole("button", { name: /^Remove/ }).first().click();
  await dlg2.getByRole("button", { name: "Tell it what’s next" }).click();
  const msg = dlg2.getByLabel("Message", { exact: true });
  await msg.click();
  await msg.fill("Now write the README section for the parser.");
  await dlg2.getByRole("button", { name: "Queue" }).click();
  await dlg2.waitFor({ state: "hidden" });
  await panel.getByText("Waiting for the agent to take it and finish").first().waitFor({ timeout: 20000 });
  ok(true, `${tag} queuing on a finished agent runs at once`);
  await hook("UserPromptSubmit", { prompt: "one more thing" });
  const told = await hook("Stop", { last_message: "Done with that.", pending: { tasks: [{ type: "subagent", status: "running" }], crons: [] } });
  ok(/README section/.test(told.text ?? ""), `${tag} the agent's next stop carries the message (${(told.text ?? "").slice(0, 80)})`);
  await hook("Stop", { last_message: "README written.", pending: { tasks: [], crons: [] } });
  await panel.getByText("Succeeded").first().waitFor({ timeout: 20000 });
  ok(/README written/.test(await panel.innerText()), `${tag} the step has the agent's answer`);
  await shot("told");

  await page.getByRole("button", { name: "Queue again" }).click();
  await page.getByRole("dialog", { name: "When this agent finishes" }).getByRole("button", { name: "Queue" }).click();
  await hook("UserPromptSubmit", { prompt: "keep going" });
  await page.goto(`${base}/agents?tab=sessions`);
  const row = page.getByRole("link").filter({ hasText: `Parser ${n}` }).first();
  await row.waitFor();
  await row.getByText(/then 1 step|running its follow-up/).waitFor();
  ok(true, `${tag} the sessions list shows the queued step`);
  await shot("sessions");
  await overflow("sessions");
  await ctx.close();
}

let n = 0;
for (const [w, s] of looks([
  [1280, "light"],
  [1280, "dark"],
  [390, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s, ++n);
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
