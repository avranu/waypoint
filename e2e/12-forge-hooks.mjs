// Waypoint adds a linked repo's forge webhook itself, in a real browser,
// against a fake GitHub that keeps webhooks and pings Waypoint (signed)
// when one is made, as GitHub does. A second fake refuses (403), so the
// failure, its reason, "Try again" and the by-hand steps show too. Then an
// organisation webhook covers a repo (no repo webhook of its own), and an
// outbound webhook's delivery log shows a failure that "Send again"
// redelivers to a local receiver.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

// A fake GitHub API under /api/v3. `refuse` answers every call with 403.
function fakeGitHub(refuse, orgs = []) {
  const hooks = new Map();
  const seen = { calls: 0 };
  let next = 1;
  const srv = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", async () => {
      const send = (code, body) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(body === undefined ? "" : JSON.stringify(body));
      };
      seen.calls++;
      if (refuse)
        return send(403, {
          message: "Resource not accessible by personal access token",
        });
      if (req.url.startsWith("/api/v3/user/orgs"))
        return send(
          200,
          orgs.map((login) => ({ login })),
        );
      const m =
        req.url.match(
          /^\/api\/v3\/repos\/([^/]+\/[^/]+)\/hooks(?:\/(\d+))?(\/pings)?/,
        ) ||
        req.url.match(/^\/api\/v3\/(orgs\/[^/]+)\/hooks(?:\/(\d+))?(\/pings)?/);
      if (!m) return send(404, { message: "Not Found" });
      const [, repo, id, pings] = m;
      if (pings) return send(204);
      if (!hooks.has(repo)) hooks.set(repo, new Map());
      const list = hooks.get(repo);
      const body = raw ? JSON.parse(raw) : null;
      if (req.method === "GET")
        return send(
          200,
          [...list].map(([i, b]) => ({
            id: +i,
            config: { url: b.config.url },
          })),
        );
      if (req.method === "POST") {
        const hid = next++;
        list.set(String(hid), body);
        send(201, { id: hid });
        const org = repo.startsWith("orgs/") && repo.slice(5);
        const ping = JSON.stringify(
          org
            ? {
                zen: "Keep it logically awesome.",
                hook: { type: "Organization" },
                organization: { login: org },
              }
            : {
                zen: "Keep it logically awesome.",
                repository: { full_name: repo, owner: { type: "User" } },
              },
        );
        const sig = crypto
          .createHmac("sha256", body.config.secret)
          .update(ping)
          .digest("hex");
        await fetch(body.config.url, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-GitHub-Event": "ping",
            "X-Hub-Signature-256": "sha256=" + sig,
          },
          body: ping,
        }).catch((e) => fail(`ping: ${e.message}`));
        return;
      }
      if (req.method === "PATCH" && list.has(id))
        return (list.set(id, body), send(200, { id: +id }));
      if (req.method === "DELETE" && list.delete(id)) return send(204);
      send(404, { message: "Not Found" });
    });
  });
  return new Promise((r) =>
    srv.listen(0, "127.0.0.1", () => r({ srv, hooks, seen })),
  );
}

const good = await fakeGitHub(false, ["orgx", "orgy"]);

// An outbound webhook receiver: answers `status`, keeps what it got. It
// listens on this machine's LAN address: Waypoint refuses loopback for
// webhooks project owners add (private networks are allowed).
const lan = Object.values(os.networkInterfaces())
  .flat()
  .find((a) => a.family === "IPv4" && !a.internal)?.address;
const receiver = { status: 418, bodies: [] };
const recv = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    receiver.bodies.push(raw);
    res.writeHead(receiver.status, { "Content-Type": "text/plain" });
    res.end(receiver.status === 200 ? "thanks" : "I'm a teapot");
  });
});
if (lan) await new Promise((r) => recv.listen(0, lan, r));
else console.log("skip delivery log checks (no LAN address to listen on)");
const bad = await fakeGitHub(true);
const browser = await chromium.launch();

async function run(width, scheme) {
  const tag = `${width}${scheme[0]}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `H${width}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Hooks ${tag}`, key });
  const port = (s) => s.srv.address().port;
  const g = await api("POST", "/forges", {
    kind: "github",
    name: `Good ${tag}`,
    base_url: `http://127.0.0.1:${port(good)}`,
    token: "tok",
  });
  await api("POST", "/forges", {
    kind: "github",
    name: `Locked ${tag}`,
    base_url: `http://127.0.0.1:${port(bad)}`,
    token: "tok",
  });
  ok(g && g.id, `${tag} forge created`);

  await page.goto(`${base}/p/${key}/settings?tab=integrations`);
  const link = async (forge, repo) => {
    await page.getByRole("combobox", { name: "Forge" }).click();
    await page.getByRole("option", { name: forge }).click();
    await page.getByLabel("Repository").fill(repo);
    await page.getByRole("button", { name: "Link repo" }).click();
  };
  await link(`Good ${tag}`, `acme/app${tag}`);
  await page.getByText(/Webhook added · last delivery/).waitFor();
  ok(true, `${tag} webhook added and its ping came through`);
  const made = good.hooks.get(`acme/app${tag}`);
  ok(
    made?.size === 1 &&
      [...made.values()][0].config.url === `${base}/api/v1/hooks/forge/${g.id}`,
    `${tag} the fake forge has one webhook pointing at Waypoint`,
  );

  await link(`Locked ${tag}`, `acme/locked${tag}`);
  await page.getByText("Webhook not added.").waitFor();
  ok(
    await page.getByText(/admin:repo_hook/).isVisible(),
    `${tag} the failure says which scope the token needs`,
  );
  ok(
    await page
      .getByText("Adding the webhook by hand")
      .or(page.getByText("Payload URL"))
      .first()
      .isVisible(),
    `${tag} the by-hand steps show for the repo that failed`,
  );
  await page.screenshot({
    path: `${out}/integrations-${width}-${scheme}.png`,
    fullPage: true,
  });
  const before = bad.seen.calls;
  await page.getByRole("button", { name: "Try again" }).click();
  for (let i = 0; i < 50 && bad.seen.calls === before; i++)
    await new Promise((r) => setTimeout(r, 100));
  await page.getByText("Webhook not added.").waitFor();
  ok(bad.seen.calls > before, `${tag} try again asks the forge again`);

  for (const where of ["/agents", "/inbox"]) {
    await page.goto(base + where);
    const row = page.getByRole("link", { name: /Webhooks? not working on/ });
    await row.waitFor();
    ok(
      (await row.textContent()).includes(`acme/locked${tag}`) ||
        /\d+ more/.test(await row.textContent()),
      `${tag} ${where} names the repo whose webhook failed`,
    );
    await page.screenshot({
      path: `${out}/waiting-hooks${where.replace("/", "-")}-${width}-${scheme}.png`,
      fullPage: true,
    });
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where} with a broken webhook)`,
    );
  }
  await page.getByRole("link", { name: /Webhooks? not working on/ }).click();
  await page.waitForURL(/\/settings\?tab=integrations/);
  ok(true, `${tag} the row opens Integrations`);
  await page.goto(`${base}/p/${key}/settings?tab=integrations`);
  await page.getByText("Webhook not added.").waitFor();

  await page.getByRole("button", { name: `Remove acme/app${tag}` }).click();
  await page.getByRole("button", { name: "Disconnect" }).click();
  for (let i = 0; i < 50 && made.size > 0; i++)
    await new Promise((r) => setTimeout(r, 100));
  ok(made.size === 0, `${tag} detaching removed the webhook`);

  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  await overflow("integrations");

  const org = `org${tag}`;
  await page.goto(`${base}/admin?tab=forges`);
  const row = page
    .locator("li")
    .filter({ hasText: `Good ${tag}` })
    .first();
  ok(
    !(await row.getByText("Organisation webhooks").isVisible()),
    `${tag} no organisation webhook section until there is one`,
  );
  await row.getByRole("button", { name: `Actions for Good ${tag}` }).click();
  await page
    .getByRole("menuitem", { name: "Add organisation webhook" })
    .click();
  await row.getByLabel("organisation name").fill(org);
  await row.getByRole("button", { name: "Add", exact: true }).click();
  await row.getByText(/Added · last delivery/).waitFor();
  ok(
    good.hooks.get(`orgs/${org}`)?.size === 1,
    `${tag} the org webhook was added and its ping came through`,
  );
  await page.screenshot({
    path: `${out}/forges-${width}-${scheme}.png`,
    fullPage: true,
  });
  await overflow("forges");

  await page.goto(`${base}/p/${key}/settings?tab=integrations`);
  await link(`Good ${tag}`, `${org}/svc`);
  await page
    .getByText(/organisation webhook/)
    .first()
    .waitFor();
  ok(
    !good.hooks.get(`${org}/svc`)?.size,
    `${tag} the covered repo got no repo webhook`,
  );

  await page.goto(`${base}/admin?tab=forges`);
  await row.getByRole("button", { name: `Remove the ${org} webhook` }).click();
  for (let i = 0; i < 50 && !good.hooks.get(`${org}/svc`)?.size; i++)
    await new Promise((r) => setTimeout(r, 100));
  ok(
    good.hooks.get(`orgs/${org}`)?.size === 0 &&
      good.hooks.get(`${org}/svc`)?.size === 1,
    `${tag} removing the org webhook moved the repo onto its own`,
  );

  if (!lan) return ctx.close();
  receiver.status = 418;
  const sent = receiver.bodies.length;
  await api("POST", `/projects/${key}/webhooks`, {
    name: `Receiver ${tag}`,
    url: `http://${lan}:${recv.address().port}/hook`,
    events: "issue.created",
  });
  await api("POST", "/issues", { project: key, title: "Delivered" });
  for (let i = 0; i < 50 && receiver.bodies.length === sent; i++)
    await new Promise((r) => setTimeout(r, 100));
  await page.goto(`${base}/p/${key}/settings?tab=integrations`);
  await page.getByRole("button", { name: "Deliveries" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByText("issue.created").waitFor();
  ok(
    await dialog.getByText("HTTP 418").isVisible(),
    `${tag} the log shows the failed delivery`,
  );
  await dialog.getByRole("button", { name: "issue.created" }).click();
  ok(
    await dialog.getByText("I'm a teapot").isVisible(),
    `${tag} the receiver's answer is kept`,
  );
  await page.screenshot({
    path: `${out}/deliveries-${width}-${scheme}.png`,
    fullPage: true,
  });
  receiver.status = 200;
  await dialog.getByRole("button", { name: "Send again" }).first().click();
  await dialog.getByText("sent again").waitFor();
  const got = receiver.bodies.slice(sent);
  ok(
    got.length === 2 && got[0] === got[1],
    `${tag} sending again redelivered the same body`,
  );
  await overflow("deliveries");
  await ctx.close();
}

for (const [w, s] of looks([[1440, "light"], [390, "light"], [1440, "dark"], [390, "dark"]])) await run(w, s);
await browser.close();
good.srv.close();
bad.srv.close();
if (lan) recv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
console.log("all good");
