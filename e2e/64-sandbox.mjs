// Sandbox policy (WP-418, WP-ADR28): Project settings → Sandbox sets the
// levels, asks why before loosening and for the password when the server
// says so, then retries; the change log reverts. Settings → Runners pauses
// sandboxing on a machine (the kill switch) with a reason. A token can read
// but not change any of it. A machine's sandbox helper, reported through
// the runner's poll, shows as ready with its machine users; removing one
// and finishing interrupted work go to the runner on its next poll, and
// show done once it answers (WP-448). Desktop and 390px.
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

const choose = async (page, label, option) => {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
};

let n = 0;
let runnerId = 0;
let T = {};
// What the machine's sandbox helper reports through the runner's poll.
const helperReport = (fp, now) => ({
  installed: true,
  status: {
    version: "v1", key: fp, runner_uid: 1000, kvm: true, retired: 0, pending: 1, drift: [],
    store: { path: "/var/lib/waypoint/store", ok: true, mounted: true, fs: "xfs", free: 1e11 },
    users: [
      { name: "wpr-robin-waypoint-a1b2c3", uid: 40000, kind: "repo", key: "1/7", slug: "robin/waypoint", created: now, last_used: now, present: true },
      { name: "wpo-acme-d4e5f6", uid: 40001, kind: "org", key: "1/acme", slug: "acme", created: now, last_used: now - 86400 * 30, present: true },
    ],
    removed: [{ name: "wpr-old-thing-0a0b0c", uid: 39999, kind: "repo", key: "1/3", slug: "robin/old-thing", created: now - 86400 * 90, last_used: now - 86400 * 60, at: now - 3600, archive: "/var/lib/waypoint/store/trash/wpr-old-thing-0a0b0c-1" }],
  },
});
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const key = `SBX${++n}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then(async (r) => ({ status: r.status(), body: await r.json().catch(() => null) }));
  await api("POST", "/projects", { name: `Sandbox ${n}`, key });
  const overflow = async (what) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);

  // A token reads the policy and can't change it.
  const tok = (await api("POST", "/me/tokens", { name: `sandbox e2e ${n}` })).body.token;
  if (n === 1) T = { Authorization: `Bearer ${tok}` };
  ok((await page.request.get(`${base}/api/v1/projects/${key}/sandbox`, { headers: T })).status() === 200, `${tag} a token reads the policy`);
  const refused = await page.request.patch(`${base}/api/v1/projects/${key}/sandbox`, { headers: T, data: { level: "off", reason: "x" } });
  ok(refused.status() === 403 && (await refused.text()).includes("Waypoint UI"), `${tag} a token can't change it`);

  // Tighten: no reason asked.
  await page.goto(`${base}/p/${key}/settings?tab=sandbox`);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Sandbox", exact: true }) });
  await section.waitFor();
  ok(await section.getByText("doesn’t enforce them yet").isVisible(), `${tag} says levels aren't enforced yet`);
  await choose(page, "Runner jobs", "VM");
  ok(!(await section.getByLabel("Why loosen it?").isVisible()), `${tag} tightening asks no reason`);
  await section.getByRole("button", { name: "Save" }).click();
  await page.getByText("Sandbox saved").waitFor();
  ok((await api("GET", `/projects/${key}/sandbox`)).body.level === "vm", `${tag} saved VM`);

  // Loosen: a reason, and the password when the server asks for it. Signing
  // in just now counts as entering it, so make the server ask once.
  await choose(page, "Runner jobs", "Off");
  ok(await section.getByText("Runs as you: everything you can reach").isVisible(), `${tag} Off says what it opens up`);
  const loosen = section.getByRole("button", { name: "Loosen sandbox" });
  ok(await loosen.isDisabled(), `${tag} loosening waits for a reason`);
  await section.getByLabel("Why loosen it?").fill("the build needs docker");
  let asked = false;
  await page.route(`**/api/v1/projects/${key}/sandbox`, async (route) => {
    if (route.request().method() === "PATCH" && !asked) {
      asked = true;
      return route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "enter your password again to loosen the sandbox", code: "reauth" }) });
    }
    return route.continue();
  });
  await loosen.click();
  const dlg = page.getByRole("dialog", { name: "Enter your password" });
  await dlg.waitFor();
  await page.screenshot({ path: `${out}/reauth-${width}-${scheme}.png` });
  await dlg.getByLabel("Password").fill("wrong password");
  await dlg.getByRole("button", { name: "Confirm" }).click();
  await dlg.getByText("wrong password").waitFor();
  ok(true, `${tag} a wrong password is refused in the dialog`);
  await dlg.getByLabel("Password").fill("correct horse battery");
  await dlg.getByRole("button", { name: "Confirm" }).click();
  await page.getByText("Sandbox loosened; admins are told").waitFor();
  await page.unroute(`**/api/v1/projects/${key}/sandbox`);
  ok((await api("GET", `/projects/${key}/sandbox`)).body.level === "off", `${tag} loosened after the password`);

  const log = page.locator("section", { has: page.getByRole("heading", { name: "Changes" }) });
  await log.getByText("“the build needs docker”").waitFor();
  await overflow("project tab");
  await section.screenshot({ path: `${out}/project-${width}-${scheme}.png` });
  await log.screenshot({ path: `${out}/project-log-${width}-${scheme}.png` });

  // Revert the loosening: it tightens, so no reason is needed.
  await log.getByRole("button", { name: "Revert" }).first().click();
  const rd = page.getByRole("dialog", { name: /Revert change/ });
  ok(await rd.getByText("Why? (optional)").isVisible(), `${tag} reverting a loosening needs no reason`);
  await rd.getByRole("button", { name: "Revert" }).click();
  await page.getByText("Change reverted").waitFor();
  ok((await api("GET", `/projects/${key}/sandbox`)).body.level === "vm", `${tag} reverted to VM`);
  await log.getByText("revert of #").waitFor();

  // The kill switch, on a machine of robin's.
  if (n === 1) {
    const r = await page.request.post(`${base}/api/v1/runner/register`, {
      headers: T,
      data: { name: "laptop", hostname: "laptop.local", os: "linux", arch: "amd64", version: "v1", labels: [], allowed_dirs: ["/work"], clis: [] },
    });
    ok(r.status() === 200, `register a runner: ${r.status()}`);
    // Its sandbox helper reports through the runner's poll: ready once it
    // pins this server's key and its store is sound.
    runnerId = (await r.json()).id;
    const fp = (await api("GET", "/sandbox/key")).body.fingerprint;
    ok(/^SHA256:/.test(fp), `the server's spec key: ${fp}`);
    const polled = await page.request.post(`${base}/api/v1/runner/poll`, {
      headers: T,
      data: { runner: runnerId, wait: 0, free: 0, running: [], sandbox: helperReport(fp, Math.floor(Date.now() / 1000)) },
    });
    ok(polled.status() === 200, `a poll reports the helper: ${polled.status()}`);
  }
  await page.goto(`${base}/settings?tab=runners`);
  const sb = page.locator("section", { has: page.getByRole("heading", { name: "Sandbox", exact: true }) });
  await sb.waitFor();
  await sb.getByText("Sandbox helper ready").click();
  await sb.getByText("wpr-robin-waypoint-a1b2c3").waitFor();
  ok(await sb.getByText(/repo robin\/waypoint · uid 40000/).isVisible(), `${tag} lists the helper's machine users`);
  const showHelp = sb.getByRole("button", { name: "Show help: Sandbox" });
  if (await showHelp.isVisible()) await showHelp.click();
  await sb.getByText(/Helpers pin this server’s key/).waitFor();
  ok(await sb.locator("code", { hasText: /^SHA256:/ }).isVisible(), `${tag} shows the key's fingerprint`);
  await sb.screenshot({ path: `${out}/helper-${width}-${scheme}.png` });

  // Remove a machine user: confirmed in the page, sent on the runner's
  // next poll as a signed spec, done once the runner answers.
  ok(await sb.getByText("wpr-old-thing-0a0b0c", { exact: true }).isVisible(), `${tag} shows the removed history`);
  ok(await sb.getByText(/trash\/wpr-old-thing/).isVisible(), `${tag} says where a removed user's files wait`);
  ok((await sb.getByText("no project here uses it now").count()) === 2, `${tag} flags users no project uses`);
  await sb.getByRole("button", { name: "Remove wpo-acme-d4e5f6" }).click();
  const rm = page.getByRole("dialog", { name: "Remove wpo-acme-d4e5f6 from laptop?" });
  await rm.waitFor();
  ok(await rm.getByText(/moves its home, work and cache to the store’s trash/).isVisible(), `${tag} the confirmation says what happens`);
  await page.screenshot({ path: `${out}/remove-${width}-${scheme}.png` });
  await rm.getByRole("button", { name: "Remove" }).click();
  await page.getByText("Asked laptop to remove wpo-acme-d4e5f6").waitFor();
  await sb.getByText("removing…").waitFor();
  await sb.getByText("waiting for the runner").first().waitFor();
  await sb.getByRole("button", { name: "Finish interrupted work" }).click();
  await page.getByText("Asked laptop to finish its interrupted work").waitFor();
  const take = await page.request.post(`${base}/api/v1/runner/poll`, { headers: T, data: { runner: runnerId, wait: 0, free: 0, running: [] } });
  const work = (await take.json()).sandbox ?? [];
  ok(work.length === 2 && work.every((o) => o.spec && o.spec.payload), `${tag} the runner's poll carries both, signed: ${JSON.stringify(work.map((o) => o.op))}`);
  const rmOp = work.find((o) => o.op === "remove_user");
  await sb.getByText("the runner has it").first().waitFor({ timeout: 20000 });
  ok(true, `${tag} the page follows the action to the runner`);
  await page.request.post(`${base}/api/v1/runner/poll`, {
    headers: T,
    data: { runner: runnerId, wait: 0, free: 0, running: [], sandbox_done: work.map((o) => ({ id: o.id, error: o === rmOp ? "" : `userdel: user is logged in (${n})` })) },
  });
  await sb.getByText(`userdel: user is logged in (${n})`).waitFor({ timeout: 20000 });
  ok(!(await sb.getByText("removing…").isVisible()) && (await sb.getByText(/^done$/).count()) === n, `${tag} the removal shows done`);
  await overflow("helper actions");
  await sb.screenshot({ path: `${out}/helper-ops-${width}-${scheme}.png` });
  const sw = sb.getByRole("switch", { name: "Pause sandboxing on laptop" });
  const paused = await sw.getAttribute("aria-checked");
  if (paused === "true") {
    await sw.click(); // unpausing tightens: no questions
    await page.getByText("Sandboxing back on for laptop").waitFor();
  }
  await sw.click();
  const pd = page.getByRole("dialog", { name: "Pause sandboxing on laptop?" });
  await pd.waitFor();
  ok(await pd.getByRole("button", { name: "Pause sandboxing" }).isDisabled(), `${tag} pausing waits for a reason`);
  await pd.getByLabel("Why?").fill("jobs fail under the sandbox");
  await pd.getByRole("button", { name: "Pause sandboxing" }).click();
  await page.getByText("Sandboxing paused on laptop").waitFor();
  const ms = (await api("GET", "/sandbox/machines")).body;
  ok(ms.some((m) => m.runner.name === "laptop" && m.paused), `${tag} the machine is paused`);
  await sb.getByText("paused", { exact: true }).waitFor();
  await overflow("runners");
  await sb.screenshot({ path: `${out}/machines-${width}-${scheme}.png` });
  const changes = page.locator("section", { has: page.getByRole("heading", { name: "Sandbox changes" }) });
  await changes.getByText("“jobs fail under the sandbox”").first().waitFor();
  await changes.screenshot({ path: `${out}/machine-log-${width}-${scheme}.png` });
  ok(true, `${tag} the log shows the pause and its reason`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
