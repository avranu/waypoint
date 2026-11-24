// The sandbox escape drill (WP-473, PL7 §13): Settings → Runners → Sandbox
// shows each ready machine's last drill and runs one now. The runner's poll
// takes the drill job with three signed Contained start specs; what it
// reports decides the drill. A try that got through fails it, tells the
// owner, and lists the case; a clean one passes. A token can't start one.
// Desktop and 390px. Run after 01-signup-flow (user robin, an admin).
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

const kase = (name, group, want, got, detail) => ({
  name,
  group,
  want,
  got,
  detail,
});
const clean = [
  kase("control: write /work", "files", "allowed", "allowed"),
  kase("read /etc/shadow", "files", "refused", "refused", "permission denied"),
  kase(
    "reach 127.0.0.1:28690",
    "network",
    "refused",
    "refused",
    "connection refused",
  ),
  kase(
    "fork bomb",
    "resources",
    "refused",
    "refused",
    "stopped after 31 processes (TasksMax=32)",
  ),
  kase(
    "overrun the time limit",
    "resources",
    "refused",
    "refused",
    "stopped at 5s",
  ),
  kase("control: the probe got to its end", "resources", "allowed", "allowed"),
];
const escaped = [
  ...clean,
  kase("list /projects", "files", "refused", "allowed", "12 entries"),
];

let n = 0;
let runnerId = 0;
let T = {};
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  n++;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then(async (r) => ({
        status: r.status(),
        body: await r.json().catch(() => null),
      }));
  const overflow = async (what) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} ${what}: no sideways scroll`,
    );

  if (n === 1) {
    T = {
      Authorization: `Bearer ${(await api("POST", "/me/tokens", { name: "drill e2e" })).body.token}`,
    };
    const r = await page.request.post(`${base}/api/v1/runner/register`, {
      headers: T,
      data: {
        name: "drillbox",
        hostname: "drillbox.local",
        os: "linux",
        arch: "amd64",
        version: "v1",
        labels: [],
        allowed_dirs: ["/work"],
        clis: [],
        features: ["sandbox_drill"],
      },
    });
    ok(r.status() === 200, `register a runner: ${r.status()}`);
    runnerId = (await r.json()).id;
    const fp = (await api("GET", "/sandbox/key")).body.fingerprint;
    const now = Math.floor(Date.now() / 1000);
    const polled = await page.request.post(`${base}/api/v1/runner/poll`, {
      headers: T,
      data: {
        runner: runnerId,
        wait: 0,
        free: 0,
        running: [],
        sandbox: {
          installed: true,
          status: {
            version: "v3",
            key: fp,
            runner_uid: 1000,
            kvm: false,
            retired: 0,
            pending: 0,
            drift: [],
            store: { path: "/var/lib/waypoint/store", ok: true, mounted: true },
            users: [
              {
                name: "wpr-robin-app-a1b2c3",
                uid: 40000,
                kind: "repo",
                key: "1/7",
                slug: "robin/app",
                created: now,
                last_used: now,
                present: true,
              },
            ],
          },
        },
      },
    });
    ok(
      polled.status() === 200,
      `a poll reports a ready helper: ${polled.status()}`,
    );
    const refused = await page.request.post(
      `${base}/api/v1/sandbox/machines/${runnerId}/drill`,
      { headers: T, data: {} },
    );
    ok(
      refused.status() === 403,
      `a token can't start a drill: ${refused.status()}`,
    );
  }

  await page.goto(`${base}/settings?tab=runners`);
  const sb = page.locator("section", {
    has: page.getByRole("heading", { name: "Sandbox", exact: true }),
  });
  await sb.waitFor();
  // The machine list may hold machines from other specs, so find drillbox's row by name.
  const row = sb.locator("li", { hasText: "drillbox" });
  const unfold = async () => {
    const d = row
      .locator("details", {
        has: page.locator("summary", { hasText: "Escape drill" }),
      })
      .first();
    if ((await d.count()) && !(await d.evaluate((e) => e.open)))
      await d.locator("summary").first().click();
  };
  if (n === 1) {
    await row
      .getByText("Escape drill: none yet. It runs on its own within the hour.")
      .waitFor();
    await sb.screenshot({ path: `${out}/none-${width}-${scheme}.png` });
  }
  await unfold();
  await row.getByRole("button", { name: "Run drill" }).click();
  await page.getByText("Escape drill queued on drillbox").waitFor();
  await row.getByText("Escape drill queued…").waitFor();
  await unfold();
  ok(
    await row.getByRole("button", { name: "Run drill" }).isDisabled(),
    `${tag} no second drill while one waits`,
  );

  const take = await page.request.post(`${base}/api/v1/runner/poll`, {
    headers: T,
    data: { runner: runnerId, wait: 0, free: 1, running: [] },
  });
  const job = (await take.json()).job;
  ok(
    job?.kind === "sandbox_drill" &&
      job.sessions?.length === 3 &&
      job.sessions.every((s) => s.payload),
    `${tag} the claim carries three signed specs`,
  );
  await page.reload();
  await row.getByText("Escape drill running…").waitFor();

  const cases = n === 1 ? escaped : clean;
  const fin = await page.request.post(
    `${base}/api/v1/runner/jobs/${job.id}/finish`,
    { headers: T, data: { state: "done", result: { cases } } },
  );
  ok(fin.status() === 200, `${tag} the runner reports: ${fin.status()}`);
  await page.reload();
  await sb.waitFor();
  if (n === 1) {
    await row.getByText("Escape drill failed").click();
    await row.getByText("list /projects: allowed, should be refused").waitFor();
    ok(
      await row.getByText("1 got through").isVisible(),
      `${tag} says how many got through`,
    );
    ok(
      await row.getByText("6 as they should be").isVisible(),
      `${tag} folds the rest away`,
    );
    const notes = (await api("GET", "/notifications")).body;
    const list = Array.isArray(notes)
      ? notes
      : (notes?.notifications ?? notes?.items ?? []);
    ok(
      list.some((x) => x.title === "Sandbox escape drill failed on drillbox"),
      `${tag} robin is told the drill failed`,
    );
  } else {
    await row.getByText("Escape drill passed").click();
    ok(
      await row.getByText(/· 6 tries/).isVisible(),
      `${tag} says how many tries`,
    );
  }
  await row.getByText("Not tried yet").click();
  ok(
    await row.getByText(/no swap limit by decision/).isVisible(),
    `${tag} lists what the drill doesn't try`,
  );
  ok(
    await row
      .getByText(/^Run by @robin · .* · as wpr-robin-app-a1b2c3/)
      .isVisible(),
    `${tag} names the machine user it ran as`,
  );
  await overflow("drill");
  await sb.screenshot({ path: `${out}/drill-${width}-${scheme}.png` });
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
