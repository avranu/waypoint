// The agent kit (WP-ADR2 §4), in a real browser at desktop and phone
// widths: an admin makes a repo on a fake Forgejo the kit in Admin → Forges;
// Waypoint reads its kit.toml and marketplace (plugins waypoint and demo);
// a checkout reports through the API, one behind and drifted. Agents → Kit
// shows the plugins with what they hold and where they go, the notes, the
// checkouts, and Roll out (no runners here: it says so).
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
import http from "node:http";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const KIT = "Robin/agent-kit";
const HEAD = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";

// The fixture kit: two plugins, one sent everywhere, one to kitco repos
// (a namespace of its own: other suites attach acme/* repos to the same server)
// with .indexed; a note for the WP project.
const files = {
  "kit.toml": `[kit]
marketplace = "agent-kit"

[plugins.waypoint]
repos = ["*"]

[plugins.demo]
repos = ["kitco/*", "!kitco/secret"]
when = ["exists:.indexed"]

[[notes]]
repos = ["KIT"]
text = "Wait on CI with wp pr wait; never sleep-poll."
`,
  ".claude-plugin/marketplace.json": JSON.stringify({
    name: "agent-kit",
    owner: { name: "Robin" },
    plugins: [
      { name: "waypoint", source: "./plugins/waypoint", description: "The PR workflow: ship, wait, babysit" },
      { name: "demo", source: "./plugins/demo", description: "A demo plugin" },
    ],
  }),
  "plugins/waypoint/.claude-plugin/plugin.json": JSON.stringify({ name: "waypoint", version: "1.0.0" }),
  "plugins/waypoint/skills/ship/SKILL.md": "---\nname: ship\ndescription: Ship a change\n---\n",
  "plugins/waypoint/skills/wait/SKILL.md": "---\nname: wait\ndescription: Wait on CI\n---\n",
  "plugins/waypoint/hooks/hooks.json": JSON.stringify({ hooks: { PreToolUse: [{ matcher: "Bash", hooks: [] }] } }),
  "plugins/waypoint/bin/wt": "#!/bin/sh\n",
  "plugins/demo/commands/hello.md": "---\ndescription: Say hello\n---\n",
  "plugins/demo/.mcp.json": JSON.stringify({ mcpServers: { demo: { command: "demo" } } }),
};

const srv = http.createServer((req, res) => {
  const send = (code, body, type = "application/json") => {
    res.writeHead(code, { "Content-Type": type });
    res.end(body === undefined ? "" : typeof body === "string" ? body : JSON.stringify(body));
  };
  const u = new URL(req.url, "http://x");
  const m = u.pathname.match(/^\/api\/v1\/repos\/([^/]+\/[^/]+)(\/.*)?$/);
  if (!m) return send(404, { message: "Not Found" });
  const [, repo, rest = ""] = m;
  if (rest.startsWith("/hooks")) return req.method === "GET" ? send(200, []) : send(201, { id: 1 });
  if (repo !== KIT) return rest === "" ? send(200, { default_branch: "main" }) : send(404, { message: "Not Found" });
  if (rest === "") return send(200, { default_branch: "main" });
  if (rest === "/branches/main") return send(200, { commit: { id: HEAD } });
  if (rest.startsWith("/git/trees/"))
    return send(200, { tree: Object.keys(files).map((path) => ({ path, type: "blob" })), truncated: false });
  if (rest.startsWith("/raw/")) {
    const p = decodeURIComponent(rest.slice(5));
    return p in files ? send(200, files[p], "text/plain") : send(404, { message: "Not Found" });
  }
  return send(404, { message: "Not Found" });
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const fake = `http://127.0.0.1:${srv.address().port}`;
const browser = await chromium.launch();

async function signIn(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

let everyRepo = "";

// ---- set up: a forge, a project with the kit repo and two others, the kit picked ----
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await signIn(ctx);
  page.on("pageerror", (e) => fail(`setup pageerror: ${e.message}`));
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then(async (r) => {
      if (!r.ok()) throw new Error(`${m} ${p}: ${r.status()} ${await r.text()}`);
      return r.status() === 204 ? null : r.json();
    });
  await api("POST", "/projects", { name: "Kit", key: "KIT" });
  const forge = await api("POST", "/forges", { kind: "forgejo", name: "Fake Forgejo kit", base_url: fake, token: "tok" });
  const kitRepo = await api("POST", `/projects/KIT/repos`, { forge_id: forge.id, repo: KIT, sync_issues: false });
  await api("POST", `/projects/KIT/repos`, { forge_id: forge.id, repo: "kitco/app", sync_issues: false });
  await api("POST", `/projects/KIT/repos`, { forge_id: forge.id, repo: "kitco/secret", sync_issues: false });

  // Admin → Forges: pick the kit repo.
  await page.goto(`${base}/admin?tab=forges`);
  const picker = page.getByLabel("Kit repository");
  await picker.waitFor();
  await picker.click();
  await page.getByRole("option", { name: new RegExp(`^${KIT}`) }).click();
  let kit;
  for (let i = 0; i < 50; i++) {
    kit = await api("GET", "/kit");
    if (kit.commit) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  ok(kit.repo_id === kitRepo.id && kit.commit === HEAD && kit.errors.length === 0, `kit read: ${kit.commit} ${kit.errors}`);
  ok(kit.kit?.plugins?.length === 2, "two plugins");
  ok(["kitco/app", "kitco/secret", KIT].every((r) => kit.reach.waypoint?.includes(r)), `waypoint goes to every repo: ${kit.reach.waypoint}`);
  everyRepo = kit.reach.waypoint.slice(0, 3).join(", ");
  ok(JSON.stringify(kit.reach.demo) === JSON.stringify(["kitco/app"]), `demo goes to kitco/app only: ${kit.reach.demo}`);
  const res = await api("GET", "/kit/resolve?repo=kitco/app");
  ok(res.plugins.map((p) => p.name).join() === "waypoint,demo" && res.notes.length === 1, "kitco/app gets both plugins and the note");

  // Two checkouts report: one current, one behind with drift.
  await api("POST", "/kit/reports", {
    machine: "devhost", path: "/home/robin/projects/app", repo: "kitco/app", kit_commit: HEAD,
    plugins: ["waypoint@agent-kit", "demo@agent-kit"], drift: [], wp_version: "0.12.0",
  });
  await api("POST", "/kit/reports", {
    machine: "laptop", path: "/Users/robin/src/agent-kit", repo: KIT, kit_commit: "0123456789abcdef",
    plugins: ["waypoint@agent-kit"], drift: [{ plugin: "waypoint", what: "disabled here by hand after wp enabled it" }], wp_version: "0.11.0",
  });
  await ctx.close();
}

// ---- the Kit tab at desktop and phone widths ----
for (const [width, scheme] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await signIn(ctx);
  const tag = `${width}`;
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(`${base}/agents?tab=kit`);
  await page.getByRole("heading", { name: "Agent kit" }).waitFor();
  const wp = page.getByRole("region", { name: "Plugin waypoint" });
  await wp.waitFor();
  ok(await wp.getByText("The PR workflow: ship, wait, babysit").isVisible(), `${tag} waypoint's description`);
  ok(await wp.getByText("2 skills").isVisible(), `${tag} waypoint counts its skills`);
  ok(await wp.getByText("ship").first().isVisible(), `${tag} names the skills`);
  ok(await wp.getByText(everyRepo).isVisible(), `${tag} waypoint lists the repos it goes to`);
  const demo = page.getByRole("region", { name: "Plugin demo" });
  ok(await demo.getByText(/kitco\/app · with .indexed/).isVisible(), `${tag} demo's targeting and condition`);
  ok(await demo.getByText("1 MCP server").isVisible(), `${tag} demo's MCP server`);
  ok(await page.getByText("Wait on CI with wp pr wait; never sleep-poll.").isVisible(), `${tag} the note`);
  ok(await page.getByText("behind 4b825dc").isVisible(), `${tag} the laptop checkout is behind`);
  ok(await page.getByText(/Drift: waypoint disabled here by hand/).isVisible(), `${tag} drift shows`);
  ok((await page.getByText("behind", { exact: false }).count()) === 1, `${tag} only one checkout is behind`);
  ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll`);
  ok((await page.getByRole("button", { name: "Roll out now" }).count()) === 1, `${tag} one Roll out button`);
  await page.screenshot({ path: `${out}/kit-${width}.png`, fullPage: true });

  if (width === 1280) {
    // Roll out: confirmed first; with no runners the server says so.
    await page.getByRole("button", { name: "Roll out now" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Roll out" }).click();
    await page.getByText(/you have no runners/).waitFor();
    ok(true, "roll out without runners says so");
  }
  await ctx.close();
}

await browser.close();
srv.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
