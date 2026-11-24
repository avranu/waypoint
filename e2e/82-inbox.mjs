// The Inbox (WP-1141): what needs the person now, one row per thing with
// its notifications counted, summary counts by kind and the oldest waiting,
// and what went stale by itself (closed issues, old alerts) folded away,
// with bulk actions that mark a group or all the stale read. Each look
// signs in as its own new user, so the counts are exact.
// Desktop, wide and 390px. Needs user robin, an admin (run 01-signup-flow first).
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

async function login(ctx, username, password) {
  const page = await ctx.newPage();
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill(username);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

let n = 0;
for (const [width, scheme] of looks(
  [
    [1440, "light"],
    [1920, "dark"],
    [390, "light"],
    [390, "dark"],
  ],
  [0, 3],
)) {
  const tag = `${width}/${scheme}`;
  const id = `${"abcd"[n++]}${width}`;
  const KEY = `IBX${id}`.toUpperCase();
  const me = `inbox${id}`;
  const password = "inbox check long password";
  try {
    const robinCtx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const robin = await login(robinCtx, "robin", "correct horse battery");
    const rcall = (m, p, d) => robin.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d });
    const rapi = (m, p, d) => rcall(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
    const made = await rcall("POST", "/admin/users", { username: me, password, email: `${me}@example.com`, display_name: `Inbox ${id}`, is_admin: true });
    ok(made.status() < 300, `${tag} a new user (${made.status()})`);
    await rapi("POST", "/projects", { name: `Inbox ${tag}`, key: KEY });

    const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
    const page = await login(ctx, me, password);
    page.setDefaultTimeout(10000);
    page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
    const shot = (name) => page.screenshot({ path: `${out}/inbox-${name}-${width}-${scheme}.png`, fullPage: false });
    const overflow = () => page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
    const api = (p) => page.request.fetch(base + "/api/v1" + p, { headers: H }).then((r) => r.json());

    await page.goto(base + "/inbox");
    await page.getByText("Nothing needs you").waitFor();
    ok(await page.getByText("All caught up").first().isVisible(), `${tag} a new inbox says it is caught up`);
    ok(!(await overflow()), `${tag} empty: no sideways scroll`);
    await shot("empty");

    const open = [];
    const closed = [];
    for (let i = 0; i < 9; i++) {
      const k = (await rapi("POST", "/issues", { project: KEY, title: `Fix the ${["parser", "cache", "login", "export", "sidebar", "importer", "search", "billing", "docs"][i]} thing`, assignees: [me] })).key;
      await rcall("POST", `/issues/${k}/comments`, { body: `First look at ${k}: the repro is in the description.` });
      await rcall("POST", `/issues/${k}/comments`, { body: `@${me} can you check the second case?` });
      await rcall("POST", `/issues/${k}/comments`, { body: "Pushed a fix, waiting on CI." });
      (i < 6 ? open : closed).push(k);
    }
    for (const k of closed) await rcall("POST", `/issues/${k}/transition`, { status: "Done" });
    const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
    db.exec("PRAGMA busy_timeout = 10000");
    const uid = db.prepare("SELECT id FROM users WHERE username = ?").get(me).id;
    const ins = db.prepare("INSERT INTO notifications(user_id, kind, title, body, url, created_at) VALUES (?, ?, ?, ?, '', ?)");
    const week = Date.now() - 6 * 86400000;
    for (let i = 0; i < 400; i++) ins.run(uid, "automation", "Rule: label new bugs", `Labelled bug ${i}`, week - i * 1000);
    ins.run(uid, "quota", "Quota alert: 5h window at 90%", "Agents are using the quota quickly.", Date.now() - 60000);
    db.close();

    const inbox = await api("/inbox");
    const liveCount = inbox.summary.live_unread;
    ok(inbox.live.filter((g) => g.issue_key).length === 6 && inbox.live.every((g) => !closed.includes(g.issue_key)), `${tag} API: one live group per open issue, none for closed ones`);
    ok(inbox.summary.stale_unread >= 400 && inbox.stale.some((g) => g.reason === "Issue closed") && inbox.stale.some((g) => g.reason === "Over 3 days old"), `${tag} API: ${inbox.summary.stale_unread} stale, with reasons`);
    ok(inbox.live[0].tier === "you" && inbox.live.at(-1).kind === "quota", `${tag} API: asked-of-you first, alerts after`);

    await page.goto(base + "/inbox");
    const live = page.locator("[data-inbox-tier] > ul > li");
    await live.first().waitFor();
    ok((await live.count()) === 7, `${tag} one row per thing: ${await live.count()} rows for ${liveCount} notifications`);
    const row = live.filter({ hasText: open[0] }).first();
    ok(/comments?/.test(await row.innerText()) && (await row.innerText()).includes("assignment"), `${tag} a row says what happened to it: ${JSON.stringify((await row.innerText()).split("\n").slice(0, 3))}`);
    const summary = page.getByRole("complementary", { name: "Inbox summary" });
    ok((await summary.innerText()).includes(String(liveCount)) && /Oldest waiting/.test(await summary.innerText()), `${tag} the summary counts ${liveCount} and says what waited longest`);
    ok(/comments/.test(await summary.getByRole("list", { name: "By kind" }).innerText()), `${tag} the summary counts by kind`);
    const badge = await page.locator("aside a[href='/inbox'], nav a[href='/inbox']").filter({ visible: true }).first().innerText();
    // Other suites leave requests waiting on you open, so the badge expectation adds them.
    const waitingTotal = (await api("/waiting")).total ?? 0;
    ok(badge.match(/\d+/)?.[0] === String(liveCount + waitingTotal), `${tag} the badge counts what matters (${JSON.stringify(badge)}: ${liveCount} live + ${waitingTotal} waiting), not the ${inbox.summary.unread} unread`);
    ok(await page.getByRole("heading", { name: "Asked of you" }).isVisible(), `${tag} sorted into tiers`);
    await shot("many");
    ok(!(await overflow()), `${tag} many: no sideways scroll`);

    await row.getByRole("button", { name: /Show all 4 notifications/ }).click();
    await row.getByText("Pushed a fix, waiting on CI.").first().waitFor();
    ok(true, `${tag} a row opens into its notifications`);
    await shot("grouped");

    await summary.getByRole("button", { name: /assignments?/ }).click();
    ok((await live.count()) === 6, `${tag} the assignments chip keeps ${await live.count()} things`);
    await summary.getByRole("button", { name: /assignments?/ }).click();

    const stale = page.locator("[data-inbox-section=stale]");
    ok((await stale.locator("li").count()) === 0, `${tag} stale items start folded`);
    await shot("stale-collapsed");
    await stale.getByRole("button", { expanded: false }).first().click();
    await stale.locator("li").first().waitFor();
    ok(/Issue closed|Over 3 days old/.test(await stale.innerText()), `${tag} stale items say why`);
    await shot("stale-open");
    ok(!(await overflow()), `${tag} stale open: no sideways scroll`);

    await row.getByRole("button", { name: /Mark .* read/ }).click();
    await page.waitForFunction(() => document.querySelectorAll('[data-inbox-tier] > ul > li').length === 6);
    ok((await live.count()) === 6, `${tag} marking a group read takes its row away`);
    let one = await api("/inbox");
    for (let i = 0; i < 20 && one.summary.live_unread !== liveCount - 4; i++) {
      await page.waitForTimeout(250);
      one = await api("/inbox");
    }
    ok(one.summary.live_unread === liveCount - 4, `${tag} and only its 4 notifications`);
    await shot("bulk");
    await stale.getByRole("button", { name: /stale read/ }).click();
    await page.waitForFunction(() => !document.querySelector("[data-inbox-section=stale]"));
    let after = await api("/inbox");
    for (let i = 0; i < 20 && after.summary.stale_unread; i++) {
      await page.waitForTimeout(250);
      after = await api("/inbox");
    }
    ok(after.summary.stale_unread === 0 && after.summary.live_unread === liveCount - 4, `${tag} marking stale read leaves the live ones (${after.summary.live_unread})`);
    await page.getByRole("button", { name: /^Earlier/ }).click();
    ok((await page.locator("[data-inbox-section=earlier] li").count()) > 0, `${tag} what was read is under Earlier`);
    await shot("earlier");

    const quota = live.filter({ hasText: "Quota alert" });
    await quota.getByRole("button").first().click();
    await page.waitForTimeout(400);
    ok((await quota.count()) === 1 && (await quota.getByRole("button", { name: "Pause sessions" }).isVisible()), `${tag} opening an alert with nothing to open leaves it, and its actions, in place`);
    const raced = live.filter({ hasText: open[1] }).first();
    await raced.waitFor();
    const db2 = new DatabaseSync(`${process.env.DIR}/wp.db`);
    db2.exec("PRAGMA busy_timeout = 10000");
    const iid = db2.prepare("SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.key || '-' || i.number = ?").get(open[1]).id;
    db2.prepare("INSERT INTO notifications(user_id, kind, title, body, url, created_at, issue_id) VALUES (?, 'mentioned', ?, 'Robin Ames mentioned you: late news', '', ?, ?)").run(uid, `${open[1]} late`, Date.now(), iid);
    db2.close();
    await raced.getByRole("button", { name: /Mark .* read/ }).click();
    let late = await api("/inbox");
    const lateGroup = () => late.live.find((g) => g.key === `i:${open[1]}`);
    for (let i = 0; i < 20 && lateGroup()?.unread !== 1; i++) {
      await page.waitForTimeout(250);
      late = await api("/inbox");
    }
    ok(lateGroup()?.unread === 1, `${tag} a notification that arrived after the view stays unread (${lateGroup()?.unread})`);
    ok((await api("/inbox/counts")).live_unread === late.summary.live_unread, `${tag} the badge's counts match the page's`);

    // Mark all read marks only what the view showed; a row that lands while the request is in flight stays unread.
    await page.reload();
    await page.getByRole("button", { name: "Mark all read" }).waitFor();
    await page.route("**/api/v1/notifications/read", async (r) => {
      const db3 = new DatabaseSync(`${process.env.DIR}/wp.db`);
      db3.exec("PRAGMA busy_timeout = 10000");
      db3.prepare("INSERT INTO notifications(user_id, kind, title, body, url, created_at, issue_id) VALUES (?, 'mentioned', ?, 'Robin Ames mentioned you: later still', '', ?, ?)").run(uid, `${open[1]} later`, Date.now(), iid);
      db3.close();
      await r.continue();
    });
    await page.getByRole("button", { name: "Mark all read" }).click();
    const readState = () => {
      const db4 = new DatabaseSync(`${process.env.DIR}/wp.db`);
      db4.exec("PRAGMA busy_timeout = 10000");
      const row = db4.prepare("SELECT read_at FROM notifications WHERE user_id = ? AND title = ?").get(uid, `${open[1]} later`);
      const others = db4.prepare("SELECT count(*) AS n FROM notifications WHERE user_id = ? AND read_at IS NULL AND title != ? AND kind != 'request'").get(uid, `${open[1]} later`).n;
      db4.close();
      return { row, others };
    };
    let st = readState();
    for (let i = 0; i < 20 && st.others !== 0; i++) {
      await page.waitForTimeout(250);
      st = readState();
    }
    ok(st.others === 0 && st.row && st.row.read_at === null, `${tag} Mark all read marks what the view showed and leaves one that arrived after (others unread ${st.others}, late read_at ${st.row?.read_at})`);
    await page.unroute("**/api/v1/notifications/read");

    let release;
    const gate = new Promise((res) => (release = res));
    await page.route("**/api/v1/inbox", async (r) => {
      await gate;
      await r.continue();
    });
    await page.reload();
    await page.getByLabel("Loading the inbox").waitFor();
    await shot("loading");
    release();
    await page.waitForSelector("[data-inbox-section=earlier]");
    await page.unrouteAll({ behavior: "wait" });
    await page.route("**/api/v1/inbox", (r) => r.fulfill({ status: 500, contentType: "application/json", body: '{"error":"boom"}' }));
    await page.reload();
    await page.getByRole("alert").getByText(/Couldn’t load the inbox/).waitFor();
    ok(true, `${tag} a failed load says so`);
    await shot("error");
    await page.unrouteAll({ behavior: "wait" });
    await page.getByRole("button", { name: "Try again" }).click();
    await page.getByText("Nothing needs you").or(page.locator("[data-inbox-tier]").first()).waitFor();
    ok(true, `${tag} Try again loads it`);
    await ctx.close();
    await robinCtx.close();
  } catch (e) {
    fail(`${tag}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
