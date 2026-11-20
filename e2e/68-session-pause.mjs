// Pausing running sessions (WP-146) in a real browser at desktop and phone
// widths: Agents → Sessions has a Pause sessions button that confirms with a
// note; once paused, a banner says so with Resume, the session's hooks get
// the pause (the hook answer and GET /sessions/pause), and Resume clears it.
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
  const overflow = () =>
    page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth + 1,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const { token } = await api("POST", "/me/tokens", {
    name: `paused agent ${tag}`,
    agent: "anthropic/claude-opus-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const sid = `e2e-pause-${width}`;
  const hook = (event) =>
    call(
      "POST",
      "/sessions/hook",
      {
        events: [
          { event, session_id: sid, machine: "box", cwd: "/work/app" },
        ],
      },
      AG,
    ).then((r) => r.json());
  ok((await hook("SessionStart")).pause === null, `${tag} no pause yet`);

  await page.goto(base + "/agents?tab=sessions&state=all");
  const pauseBtn = page.getByRole("button", { name: "Pause sessions" });
  await pauseBtn.waitFor();
  ok(!(await overflow()), `${tag} sessions has no sideways scroll`);
  await pauseBtn.click();
  await page.getByLabel("Why").fill(`quota climbing ${tag}`);
  await shot("confirm");
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  const banner = page.getByRole("status").filter({
    hasText: "are paused, running sessions included",
  });
  await banner.waitFor();
  ok(
    (await banner.textContent()).includes(`quota climbing ${tag}`),
    `${tag} the banner shows the note`,
  );
  ok(
    (await pauseBtn.count()) === 0,
    `${tag} no Pause button while paused`,
  );
  ok(!(await overflow()), `${tag} paused sessions has no sideways scroll`);
  await shot("paused");

  const r = await hook("PostToolUse");
  ok(
    r.pause?.sessions === true && r.pause?.note === `quota climbing ${tag}`,
    `${tag} the hook answer carries the pause`,
  );
  const pulled = await (await call("GET", "/sessions/pause", undefined, AG)).json();
  ok(pulled.pause?.id === r.pause?.id, `${tag} the pull hook gets it too`);
  const stop = await hook("Stop");
  ok(
    /Paused \(quota climbing/.test(stop.note ?? ""),
    `${tag} a paused stop answers a note (${stop.note})`,
  );

  await banner.getByRole("button", { name: "Resume" }).click();
  await pauseBtn.waitFor();
  ok((await banner.count()) === 0, `${tag} Resume clears the banner`);
  const after = await (await call("GET", "/sessions/pause", undefined, AG)).json();
  ok(after.pause === null, `${tag} resumed: the hooks see no pause`);
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
