// The agent queue (WP-D15), in a real browser: delegated work shows in the
// order wp next hands it out, with why; work that waits says why and offers
// its fix; pinning (row menu and drag) holds one task in a place of its own
// while the rest keeps the automatic order, and wp next then takes it; pausing stops an agent with the person's note, and resuming
// lets it work again; the issue's Agent picker says which agents a spec
// reaches; the project setting pauses agents there. At 1440 and 390 wide,
// light and dark. Run after 01-signup-flow (user robin, an admin).
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
  const api = (m, p, d, headers = H) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers, data: d })
      .then(async (r) => {
        const t = await r.text();
        if (r.status() >= 300) throw new Error(`${m} ${p}: ${r.status()} ${t}`);
        try {
          return JSON.parse(t);
        } catch {
          return t;
        }
      });
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  const shot = async (name, top = true) => {
    if (top)
      await page.evaluate(() =>
        document
          .querySelectorAll(".overflow-y-auto")
          .forEach((e) => (e.scrollTop = 0)),
      );
    await page.waitForTimeout(400);
    await page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `Q${width === 1440 ? "W" : "N"}${scheme === "dark" ? "D" : "L"}`;
  await api("POST", "/projects", { name: `Queue ${tag}`, key });
  const mk = (title, extra = {}) =>
    api("POST", "/issues", { project: key, title, ...extra });
  const parser = await mk("Refactor the parser", {
    agent: "claude",
    priority: "high",
    due_date: "2099-01-14",
  });
  const docs = await mk("Write the API docs", {
    agent: "claude",
    priority: "low",
  });
  const tidy = await mk("Tidy the lint config", {
    agent: "claude",
    priority: "low",
  });
  const flaky = await mk("Fix the flaky upload test", {
    agent: "claude",
    priority: "urgent",
  });
  const someday = await mk("Try a new layout engine", {
    agent: "any",
    status: "backlog",
  });
  const blocked = await mk("Ship the importer", {
    agent: "any",
    priority: "medium",
  });
  const blocker = await mk("Agree on the import format", { agent: "none" });
  await api("POST", `/issues/${blocker.key}/relations`, {
    kind: "blocks",
    issue: blocked.key,
  });
  await mk("Call the plumber", { priority: "urgent", agent: "none" });
  const shipped = await mk("Speed up the search index", {
    agent: "claude",
    priority: "medium",
  });
  const prURL = "https://github.com/me/web/pull/7";
  await api("POST", `/issues/${shipped.key}/links`, { url: prURL });
  await api("POST", `/issues/${shipped.key}/transition`, {
    status: "In Review",
    via_link: prURL,
  });
  const opusTok = (
    await api("POST", "/me/tokens", {
      name: `opus ${tag}`,
      agent: "anthropic/claude-opus-5-5",
    })
  ).token;
  const haikuTok = (
    await api("POST", "/me/tokens", {
      name: `haiku ${tag}`,
      agent: "anthropic/claude-haiku-5-5",
    })
  ).token;
  const as = (tok) => ({ Authorization: `Bearer ${tok}` });
  const took = await api(
    "POST",
    "/agent/next?format=json",
    { project: key },
    as(haikuTok),
  );
  ok(took.task?.key === flaky.key, `${tag} haiku took the urgent task`);

  await page.goto(`${base}/agents?tab=queue&project=${key}`);
  await page.getByRole("heading", { name: "Every agent’s queue" }).waitFor();
  await page.getByText("Working now · 1").waitFor();
  const upNext = page.getByRole("list", { name: "Up next, in order" });
  const rows = upNext
    .locator("li:not([aria-hidden])")
    .filter({ hasText: key + "-" });
  ok(
    (await rows.first().innerText()).includes(parser.key),
    `${tag} the high-priority task is first`,
  );
  ok(
    await page.getByText("High priority · due Jan 14").first().isVisible(),
    `${tag} each row says why it's there`,
  );
  await page.getByText("In the backlog").first().waitFor();
  ok(
    await page
      .getByRole("button", { name: "Move to To Do" })
      .first()
      .isVisible(),
    `${tag} backlog work offers Move to To Do`,
  );
  ok(
    await page.getByRole("link", { name: blocker.key }).first().isVisible(),
    `${tag} blocked work names its blocker`,
  );
  const review = page
    .locator("li")
    .filter({ hasText: shipped.key })
    .filter({ hasText: "me/web#7 is open" });
  ok(
    (await page.getByText("Out for review").first().isVisible()) &&
      (await review.first().isVisible()),
    `${tag} shipped work waits out for review, naming its pull request`,
  );
  ok(
    !(await upNext.innerText()).includes(shipped.key),
    `${tag} shipped work isn't up next`,
  );
  await shot("queue-all");
  await overflow("queue");

  const keys = async () =>
    (await api("GET", `/agents/queue?project=${key}`)).next.map(
      (i) => i.issue.key,
    );
  const pinnedKeys = async () =>
    (await api("GET", `/agents/queue?project=${key}`)).next
      .filter((i) => i.pinned)
      .map((i) => i.issue.key);
  const settle = async (want) => {
    for (let n = 0; n < 30 && (await pinnedKeys()).sort().join() !== want; n++)
      await page.waitForTimeout(150);
  };
  const moveTop = async (k) => {
    if (width >= 640) {
      const b = page.getByRole("button", { name: `Move ${k} to top` });
      ok(
        (await b.getAttribute("title")) === "Move to top",
        `${tag} the row's button says Move to top on hover`,
      );
      await b.click();
    } else {
      await page.getByRole("button", { name: `More for ${k}` }).click();
      await page.getByRole("menuitem", { name: "Move to top" }).click();
    }
  };
  const auto = await keys();
  ok(
    auto[0] === parser.key &&
      auto.length === 3 &&
      (await pinnedKeys()).length === 0,
    `${tag} nothing is pinned to begin with: ${auto}`,
  );
  const last = auto[2];
  const mid = auto[1];
  await page.getByRole("button", { name: `More for ${last}` }).click();
  const offered = (await page.getByRole("menuitem").allInnerTexts()).map((n) =>
    n.split("\n")[0].trim(),
  );
  ok(
    !offered.some((n) => ["Move up", "Move down", "Pin here"].includes(n)) &&
      offered.includes("Move to top") === width < 640 &&
      offered.includes("Prioritize") === width >= 640,
    `${tag} with no pins the menu offers one way to the top: Prioritize, or Move to top on a phone: ${offered}`,
  );
  await page.keyboard.press("Escape");
  await moveTop(last);
  await page.getByText("Pinned at #1 by").first().waitFor();
  ok(
    (await rows.first().innerText()).includes(last),
    `${tag} the pinned task moved to the top`,
  );
  ok(
    (await keys()).join() === [last, parser.key, mid].join() &&
      (await pinnedKeys()).join() === last,
    `${tag} pinning one task leaves the others in the automatic order: ${await keys()}`,
  );

  if (width >= 1024) {
    const from = await rows
      .filter({ hasText: parser.key })
      .getByText("High priority")
      .boundingBox();
    const target = await rows.filter({ hasText: mid }).boundingBox();
    await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
    await page.mouse.down();
    await page.mouse.move(from.x + from.width / 2, from.y + 10, { steps: 6 });
    await page.mouse.move(
      from.x + from.width / 2,
      target.y + target.height - 4,
      { steps: 12 },
    );
    await page.mouse.up();
    await page.getByText("Pinned at #3 by").first().waitFor();
    ok(
      (await keys()).join() === [last, mid, parser.key].join() &&
        (await pinnedKeys()).sort().join() === [last, parser.key].sort().join(),
      `${tag} placing a task pins only it: ${await keys()} pinned ${await pinnedKeys()}`,
    );
    await page.getByRole("button", { name: `More for ${parser.key}` }).click();
    await page.getByRole("menuitem", { name: /Prioritize/ }).click();
    await settle(last);
    ok(
      (await keys()).join() === [last, parser.key, mid].join() &&
        (await pinnedKeys()).join() === last,
      `${tag} prioritized back to its automatic place, it is unpinned: pinned ${await pinnedKeys()}`,
    );
  }

  await page.getByRole("button", { name: `More for ${mid}` }).click();
  const prio = page.getByRole("menuitem", { name: /Prioritize/ });
  ok(
    (await prio.innerText()).includes("after 1 pin"),
    `${tag} Prioritize says where the task goes`,
  );
  await prio.click();
  await page.getByText("Pinned at #2 by").first().waitFor();
  ok(
    (await keys()).join() === [last, mid, parser.key].join() &&
      (await pinnedKeys()).sort().join() === [last, mid].sort().join(),
    `${tag} prioritized after the pin at the top: ${await keys()} pinned ${await pinnedKeys()}`,
  );
  await shot("queue-pinned");
  await page.getByRole("button", { name: `Unpin ${mid}` }).click();
  await settle(last);
  ok(
    (await keys()).join() === [last, parser.key, mid].join(),
    `${tag} unpinned, it goes back to the automatic order: ${await keys()}`,
  );
  await page.getByRole("button", { name: `Unpin ${last}` }).click();
  await settle("");
  ok((await pinnedKeys()).length === 0, `${tag} the pin button unpins`);
  await page.getByRole("button", { name: `More for ${parser.key}` }).click();
  await page.getByRole("menuitem", { name: /Prioritize/ }).click();
  await settle(parser.key);
  ok(
    (await pinnedKeys()).join() === parser.key &&
      (await keys())[0] === parser.key,
    `${tag} Prioritize pins the first task at the top: pinned ${await pinnedKeys()}`,
  );
  await page.getByRole("button", { name: `Unpin ${parser.key}` }).click();
  await settle("");
  await moveTop(last);
  await page.getByText("Pinned at #1 by").first().waitFor();

  await page.goto(
    `${base}/agents?tab=queue&project=${key}&agent=anthropic/claude-opus-5-5`,
  );
  await page
    .getByRole("heading", { name: "claude-opus-5-5’s queue" })
    .waitFor();
  const peek = await api(
    "GET",
    `/agent/next?format=json&project=${key}`,
    undefined,
    as(opusTok),
  );
  ok(
    peek.task?.key === last && /Pinned at #1/.test(peek.why),
    `${tag} wp next agrees: ${peek.task?.key} (${peek.why})`,
  );

  await page.goto(`${base}/agents?tab=queue&project=${key}&agent=claude`);
  await page.getByRole("heading", { name: "Queue for claude" }).waitFor();
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await page.getByLabel("Why").fill("reviewing yesterday's work");
  await page.getByRole("button", { name: "Pause", exact: true }).last().click();
  await page
    .getByText(/Agents matching “claude” paused/)
    .first()
    .waitFor();
  const paused = await api(
    "POST",
    "/agent/next",
    { project: key },
    as(opusTok),
  );
  ok(
    /^Paused: .+ stopped agents matching claude/.test(paused) &&
      paused.includes("reviewing yesterday's work"),
    `${tag} a paused agent is told why`,
  );
  await shot("queue-paused");
  await page.getByRole("button", { name: "Resume" }).first().click();
  await page.getByRole("button", { name: "Pause", exact: true }).waitFor();
  ok(true, `${tag} resumed`);

  await page.getByRole("button", { name: "Add work" }).first().click();
  await page.getByLabel("Find an issue").fill("plumber");
  const dialog = page.getByRole("dialog", { name: "Add work for agents" });
  await dialog.getByText("Call the plumber").waitFor();
  await shot("queue-add", false);
  await dialog.getByRole("button", { name: "Add" }).first().click();
  await page
    .getByText(/is for claude, first in the queue/)
    .first()
    .waitFor();
  await page.keyboard.press("Escape");
  let after;
  for (let i = 0; i < 20; i++) {
    after = await api("GET", `/agents/queue?project=${key}&agent=claude`);
    if (
      after.next[0]?.pinned &&
      after.next[0].issue.title === "Call the plumber"
    )
      break;
    await page.waitForTimeout(150);
  }
  ok(
    after.next[0]?.issue.title === "Call the plumber",
    `${tag} added work is first`,
  );

  await page.goto(`${base}/agents?tab=queue&project=${key}`);
  await page.getByRole("button", { name: "Take back" }).first().click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Take back" })
    .click();
  await page.getByText(`${flaky.key} is back in the queue`).waitFor();
  ok(true, `${tag} a person took the task back`);

  const box = `qbox-${key.toLowerCase()}`;
  const start = (k) =>
    page.getByRole("button", { name: `Start a session for ${k}` });
  await start(docs.key).waitFor();
  ok(
    await start(docs.key).isDisabled(),
    `${tag} Start is off while no runner can take it`,
  );
  ok(
    /^None of your runners can start/.test(
      await start(docs.key).locator("..").getAttribute("title"),
    ),
    `${tag} the off Start says why`,
  );
  const sTok = (
    await api("POST", "/me/tokens", {
      name: `session ${tag}`,
      agent: "anthropic/claude-code",
    })
  ).token;
  const sAs = { ...H, Authorization: `Bearer ${sTok}` };
  const cli = `cli-q-${key}`;
  await api(
    "POST",
    "/sessions/hook",
    {
      events: [
        {
          event: "SessionStart",
          session_id: cli,
          machine: box,
          cwd: "/work/queue",
        },
      ],
    },
    sAs,
  );
  const held = await api(
    "POST",
    "/agent/next?format=json",
    { project: key },
    { ...sAs, "X-Waypoint-Session": cli, "X-Waypoint-Machine": box },
  );
  const runner = await api("POST", "/runner/register", {
    name: box,
    hostname: box,
    os: "linux",
    arch: "amd64",
    version: "e2e",
    allowed_dirs: ["/work"],
    clis: [{ name: "claude", signed_in: true }],
    features: ["task_kickoff"],
  });
  let job;
  try {
    await page.goto(`${base}/agents?tab=queue&project=${key}`);
    await start(docs.key).waitFor();
    for (let i = 0; i < 20 && (await start(docs.key).isDisabled()); i++)
      await page.waitForTimeout(150);
    ok(
      !(await start(docs.key).isDisabled()),
      `${tag} Start is on once a runner can take it`,
    );
    await shot("queue-start");
    await overflow("queue with Start");
    await start(docs.key).click();
    const dlg = page.getByRole("dialog", {
      name: `Start a session for ${docs.key}?`,
    });
    await dlg.getByText("/work/queue").waitFor();
    ok(
      (await dlg.innerText()).includes(box),
      `${tag} the dialog says where it starts`,
    );
    await shot("queue-start-dialog", false);
    await dlg.getByRole("button", { name: "Start session" }).click();
    await page.getByText(`Starting ${docs.key} on ${box}`).waitFor();
    ok(true, `${tag} a toast says it's starting`);
    const jobs = await api("GET", `/runner-jobs?source=manual&state=queued`);
    job = (Array.isArray(jobs) ? jobs : (jobs.jobs ?? [])).find((j) =>
      j.title?.includes(docs.key),
    );
    const detail = job && (await api("GET", `/runner-jobs/${job.id}`));
    ok(
      detail?.kind === "session" &&
        detail.input?.task === docs.key &&
        detail.input?.dir === "/work/queue" &&
        detail.input?.kickoff &&
        !detail.input?.prompt,
      `${tag} a session job bound to ${docs.key} is queued: ${JSON.stringify(detail?.input)}`,
    );
  } finally {
    if (job) await api("POST", `/runner-jobs/${job.id}/cancel`).catch(() => {});
    await api("DELETE", `/runners/${runner.id}`).catch(() => {});
    if (held.task)
      await api("POST", `/agents/release/${held.task.key}`).catch(() => {});
  }

  const anyone = await mk("Sweep the changelog", { priority: "urgent" });
  ok(!anyone.agent, `${tag} a new issue is open to anyone`);
  const qn = (await api("GET", `/agents/queue?project=${key}`)).next;
  const qa = qn.map((i) => i.issue.key);
  const at = qa.indexOf(anyone.key);
  ok(
    at >= 0 &&
      qn
        .slice(0, at)
        .every((i) => i.pinned || i.issue.priority >= 4 || i.issue.due_date),
    `${tag} an urgent Anyone task comes before less pressing delegated ones: ${qa}`,
  );
  const humans = await mk("Water the plants", { agent: "none" });
  ok(
    !(await api("GET", `/agents/queue?project=${key}`)).next.some(
      (i) => i.issue.key === humans.key,
    ),
    `${tag} humans-only work isn't in the queue`,
  );
  await page.goto(`${base}/agents?tab=queue&project=${key}`);
  await page.getByText(anyone.title).first().waitFor();
  ok(
    await page.getByTitle("Anyone can take this").first().isVisible(),
    `${tag} an Anyone row says so`,
  );
  await shot("queue-anyone");

  const epic = await mk("Rework the importer", {
    agent: "claude",
    type: "epic",
    priority: "urgent",
  });
  const slice = await mk("Importer slice one", {
    agent: "claude",
    parent: epic.key,
  });
  const fam = await api("GET", `/agents/queue?project=${key}`);
  ok(
    fam.next.some((i) => i.issue.key === slice.key) &&
      !fam.next.some((i) => i.issue.key === epic.key) &&
      fam.waiting.some(
        (i) => i.issue.key === epic.key && i.reason === "subtasks",
      ),
    `${tag} a parent waits for its sub-issues: ${fam.next.map((i) => i.issue.key)}`,
  );
  await page.goto(`${base}/agents?tab=queue&project=${key}`);
  await page.getByRole("heading", { name: /Sub-issues first/ }).waitFor();
  ok(
    await page
      .getByText("Its sub-issues come first (1 of 1 open for agents)")
      .first()
      .isVisible(),
    `${tag} the parent says what it waits for`,
  );
  await shot("queue-sub-issues");
  await overflow("sub-issues");

  await page.goto(`${base}/issue/${someday.key}`);
  await page
    .getByTitle(/^For agents: any/)
    .locator("visible=true")
    .first()
    .click();
  const anyAgent = page.getByRole("option", { name: /Any agent/ }).first();
  await anyAgent.waitFor();
  ok(
    /\d+ agents?/.test(await anyAgent.innerText()) &&
      (await page
        .getByRole("option", { name: /claude-opus-5-5/ })
        .first()
        .isVisible()),
    `${tag} the picker says which agents a spec reaches`,
  );
  await shot("agent-picker", false);
  await page.keyboard.press("Escape");

  await page.goto(`${base}/p/${key}/settings`);
  await page
    .getByRole("switch", { name: "Agents may take new work here" })
    .click();
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await page.getByText("Settings saved").waitFor();
  await page.goto(`${base}/agents?tab=queue&project=${key}`);
  await page
    .getByText(/Agents are paused in/)
    .first()
    .waitFor();
  await page.getByRole("heading", { name: /In a paused project/ }).waitFor();
  ok(true, `${tag} a paused project's work waits`);
  await shot("queue-project-paused");
  await overflow("paused project");
  const long = key + "L";
  await api("POST", "/projects", { name: `Long queue ${tag}`, key: long });
  for (let n = 0; n < 12; n++)
    await api("POST", "/issues", {
      project: long,
      title: `Chore ${n}`,
      agent: "claude",
      priority: "low",
    });
  await page.setViewportSize({ width, height: 600 });
  await page.goto(`${base}/p/${long}/agents`);
  const longRows = page
    .getByRole("list", { name: "Up next, in order" })
    .locator("li:not([aria-hidden])");
  await longRows.nth(11).waitFor();
  const far = (await longRows.nth(10).innerText()).match(
    new RegExp(`${long}-\\d+`),
  )[0];
  await longRows.nth(10).scrollIntoViewIfNeeded();
  const scrolled = () =>
    page.evaluate(() => {
      let el = document.querySelector('[aria-label="Up next, in order"]');
      while (el && getComputedStyle(el).overflowY !== "auto")
        el = el.parentElement;
      return el ? el.scrollTop : -1;
    });
  const top0 = await scrolled();
  await moveTop(far);
  await page.getByText("Pinned at #1 by").first().waitFor();
  await page.waitForTimeout(400);
  const top1 = await scrolled();
  ok(
    top0 > 100 && Math.abs(top1 - top0) < 2,
    `${tag} moving a task to the top leaves the page where it was: ${top0} → ${top1}`,
  );
  await page.setViewportSize({ width, height: 900 });
  await ctx.close();
}

try {
  for (const [w, s] of looks([
    [1440, "light"],
    [1440, "dark"],
    [390, "light"],
    [390, "dark"],
  ]))
    await run(w, s);
} catch (e) {
  fail(String(e.stack || e));
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
