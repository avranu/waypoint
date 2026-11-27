// Goals (WP-382, WP-ADR50): a person adds a goal in a line on the Goals tab;
// an agent's goal shows as suggested with a wording flag, an agent's link
// and Waypoint's guess wait for a person to confirm (or to stop asking, which
// is no confirm), and untraced work links
// to a goal (or a new one) from a picker. An issue shows the goal it serves
// under its title. An agent's other words for a goal wait for "Use it" or
// "Keep mine", and Suggest goals answers. A goal links many tasks at once
// from a short list; a task picks its goal (or asks for one) on its page;
// unapproved decisions are one line, not a list of would-be goals (WP-679).
// Desktop and 390px.
// Needs user robin (run 01-signup-flow first).
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

let n = 0;
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const KEY = `GOL${"ABCD"[n++]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  const overflow = async (what) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);
  const call = (m, p, d, headers = H) => page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = async (m, p, d, headers) => (await call(m, p, d, headers)).json();

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("POST", "/projects", { name: `Atlas ${tag}`, key: KEY });
  const { token } = await api("POST", "/me/tokens", { name: `goals ${tag}`, agent: "anthropic/claude-opus-5-5" });
  const agent = { ...H, Authorization: `Bearer ${token}` };

  await page.goto(`${base}/p/${KEY}/goals`);
  const first = page.getByLabel("Add goal");
  await first.waitFor();
  ok(/An outcome/.test((await first.getAttribute("placeholder")) ?? ""), `${tag} the empty tab asks for an outcome`);
  await shot("empty");
  await overflow("the empty Goals tab");
  await first.fill("Travellers find a route in one tap");
  await page.getByRole("button", { name: "Add goal" }).click();
  const rows = page.locator("[data-testid=goal-row]");
  await rows.filter({ hasText: "Travellers find a route in one tap" }).waitFor();
  const goals = await api("GET", `/goals?project=${KEY}`);
  const goal = goals.goals[0]?.note;
  ok(goal?.approved && goal.kind === "goal", `${tag} the person's goal is approved: ${JSON.stringify(goal)?.slice(0, 200)}`);

  await api("POST", "/design", { title: "Use Elasticsearch for route search", kind: "goal", project: KEY }, agent);
  const linked = await api("POST", "/issues", { project: KEY, title: "Draw the route list" }, agent);
  const stray = await api("POST", "/issues", { project: KEY, title: "Recolour the footer" }, agent);
  const extra = [];
  for (const title of ["Show the route on the map", "Cache the route tiles"]) extra.push(await api("POST", "/issues", { project: KEY, title }, agent));
  for (const t of [linked, ...extra]) {
    const l = await call("POST", "/links", { from: { ref: t.key }, to: { ref: `design:${goal.id}` }, rel: "serves" }, agent);
    ok(l.ok(), `${tag} the agent links ${t.key}: ${l.status()}`);
  }
  const near = await api("POST", "/issues", { project: KEY, title: "Find a route in one tap from the map" });
  const rw = await call("PATCH", `/design/${goal.id}`, { title: "Travellers reach a route in one tap" }, agent);
  ok(rw.ok(), `${tag} the agent proposes a rewording: ${rw.status()}`);

  await page.reload();
  const suggested = rows.filter({ hasText: "Use Elasticsearch for route search" });
  await suggested.waitFor();
  ok(await suggested.getByText("Suggested").isVisible(), `${tag} the agent's goal shows as suggested`);
  ok(await suggested.getByText(/Elasticsearch/).nth(1).isVisible(), `${tag} its wording is flagged`);
  ok(await suggested.getByRole("button", { name: "Reword" }).isVisible(), `${tag} with a Reword button`);
  const rewording = page.locator("[data-testid=goal-rewording]");
  ok(await rewording.getByText("“Travellers reach a route in one tap”").isVisible(), `${tag} the proposed words wait on the goal`);
  ok(await rows.filter({ hasText: "Travellers find a route in one tap" }).count() === 1, `${tag} the goal keeps its words until a person takes the new ones`);
  const waiting = page.locator("section", { has: page.getByRole("heading", { name: /Links to check/ }) });
  ok(await waiting.getByText("Draw the route list").isVisible(), `${tag} the agent's link waits for a person`);
  const untraced = page.locator("section", { has: page.getByRole("heading", { name: /Not traced to an approved goal/ }) });
  ok(await untraced.getByText("Recolour the footer").isVisible(), `${tag} the stray task is untraced`);
  const nearRow = waiting.locator("li", { hasText: near.title });
  ok((await nearRow.locator("[title^=\"Waypoint's guess\"]").count()) === 1 && await nearRow.getByText(/^Both are about/).isVisible(), `${tag} Waypoint guesses the goal of the near task`);
  await shot("goals");
  ok(await page.getByTestId("goals-legend").isVisible(), `${tag} the legend names the colors`);
  ok(!(await page.getByText("Guessed goal").count()) && !(await page.getByText("Suggested goal").count()), `${tag} the agent kinds are one group`);
  ok(!(await page.getByRole("heading", { name: /Worth a look/ }).count()), `${tag} no wall of questions`);
  const warn = suggested.locator("span[title]").filter({ has: page.getByRole("button", { name: "Dismiss this warning" }) }).first();
  await warn.waitFor();
  const dismissedBefore = (await api("GET", "/me")).prefs.dismissed_warnings ?? [];
  await warn.getByRole("button", { name: "Dismiss this warning" }).click();
  await warn.waitFor({ state: "detached" });
  await page.reload();
  await suggested.waitFor();
  ok(!(await suggested.getByRole("button", { name: "Dismiss this warning" }).count()), `${tag} the dismissed warning stays gone after a reload`);
  const dismissedAfter = (await api("GET", "/me")).prefs.dismissed_warnings ?? [];
  ok(dismissedAfter.length === dismissedBefore.length + 1, `${tag} the dismissal is on the profile: ${JSON.stringify(dismissedAfter.slice(-2))}`);
  await api("POST", "/design", { title: "Routes load in a second", kind: "goal", project: KEY, parent: goal.id });
  await page.reload();
  const subRow = rows.filter({ hasText: "Routes load in a second" });
  await subRow.waitFor();
  await rows.filter({ hasText: "Travellers find a route in one tap" }).first().getByRole("button", { name: /Fold the goals under it/ }).click();
  await subRow.waitFor({ state: "detached" });
  await page.reload();
  await rows.filter({ hasText: "Travellers find a route in one tap" }).first().waitFor();
  ok(!(await subRow.count()), `${tag} the fold is remembered`);
  await rows.filter({ hasText: "Travellers find a route in one tap" }).first().getByRole("button", { name: /Show 1 goal under it/ }).click();
  await subRow.waitFor();
  await overflow("the folded goals");
  await waiting.scrollIntoViewIfNeeded();
  await shot("links");
  await overflow("the Goals tab");

  await rewording.scrollIntoViewIfNeeded();
  await shot("rewording");
  await rewording.getByRole("button", { name: "Keep mine" }).click();
  await rewording.waitFor({ state: "detached" });
  const linkedRow = waiting.locator("li", { hasText: linked.title });
  await linkedRow.getByRole("button", { name: "Link", exact: true }).click();
  await linkedRow.waitFor({ state: "detached" });
  await nearRow.getByRole("button", { name: "Link", exact: true }).click();
  await nearRow.waitFor({ state: "detached" });
  await waiting.getByRole("button", { name: "Stop asking" }).click();
  await page.getByText("Stopped asking about 2 links; they still count as unconfirmed").waitFor();
  await waiting.waitFor({ state: "detached" });
  await suggested.getByRole("button", { name: "Drop", exact: true }).click();
  await suggested.waitFor({ state: "detached" });
  const strayRow = untraced.locator("li", { hasText: stray.title });
  await strayRow.getByRole("button", { name: /Link to a goal|Other/ }).click();
  await page.getByPlaceholder("Which goal does it serve?").fill("Pages look like the brand");
  await page.getByText("Add goal: “Pages look like the brand”").click();
  await strayRow.waitFor({ state: "detached" });
  await rows.filter({ hasText: "Pages look like the brand" }).waitFor();
  const after = await api("GET", `/goals?project=${KEY}`);
  ok(
    after.totals.person_goal === 3 && after.totals.agent_goal === 2 && !after.totals.none && after.links.length === 0,
    `${tag} confirmed and hand-linked tasks are the person's, the ones no one asks about stay the agent's: ${JSON.stringify(after.totals)}`,
  );
  ok(await page.getByText("Everything open traces to an approved goal.").isVisible(), `${tag} the tab says so`);
  await shot("traced");
  await page.getByRole("button", { name: "Suggest goals" }).click();
  await page.getByText(/Admin → AI|Waypoint suggests|Nothing to suggest|found nothing/).first().waitFor();
  ok(true, `${tag} Suggest goals answers`);

  await api("POST", "/design", { title: "Route search runs on a nightly index", kind: "decision", project: KEY }, agent);
  const fast = await api("POST", "/issues", { project: KEY, title: "Travellers find the fastest route between two stops" });
  const header = await api("POST", "/issues", { project: KEY, title: "Recolour the header" });
  await page.reload();
  const pending = page.locator("[data-testid=goals-unapproved]");
  await pending.waitFor();
  ok(/1 proposed note awaits/.test(await pending.textContent()), `${tag} the unapproved decision is one line: ${await pending.textContent()}`);
  ok(!(await page.getByRole("heading", { name: /How no person approved/ }).count()), `${tag} no list of unapproved notes`);
  const routeRow = rows.filter({ hasText: "Travellers find a route in one tap" });
  await routeRow.getByRole("button", { name: "Link tasks…" }).click();
  const work = page.locator("[data-testid=goal-work]");
  await work.getByText(fast.title).waitFor();
  ok(!(await work.getByText(header.title).count()), `${tag} unrelated work stays out of the list`);
  await shot("goal-work");
  await overflow("the goal's work list");
  await work.getByLabel(`${fast.key} ${fast.title}`).check();
  await work.getByPlaceholder("Find a task by key or words").fill(header.key);
  await work.getByText(header.title).waitFor();
  await work.getByLabel(`${header.key} ${header.title}`).check();
  await work.getByRole("button", { name: "Link 2 tasks" }).click();
  await page.getByText("Linked 2 tasks to the goal").waitFor();
  const linkedNow = await api("GET", `/goals?project=${KEY}`);
  ok(linkedNow.totals.person_goal === 5, `${tag} both tasks serve the goal as the person's: ${JSON.stringify(linkedNow.totals)}`);

  const loose = await api("POST", "/issues", { project: KEY, title: "Print the timetable" });
  await page.goto(`${base}/issue/${loose.key}`);
  const looseTrace = page.locator("[data-testid=goal-trace] >> visible=true");
  await looseTrace.getByRole("button", { name: "Suggest" }).waitFor();
  await shot("issue-unlinked");
  await looseTrace.getByRole("button", { name: "Suggest" }).click();
  await page.getByText(/Admin → AI/).first().waitFor();
  ok(true, `${tag} Suggest on a task says where to set up a model`);
  await looseTrace.getByRole("button", { name: "Link to a goal…" }).click();
  await page.getByPlaceholder("Which goal does it serve?").fill("in one tap");
  await page.getByRole("option", { name: "Travellers find a route in one tap", exact: true }).click();
  await looseTrace.getByText("Approved goal").waitFor();
  ok((await looseTrace.textContent()).includes("Travellers find a route in one tap"), `${tag} the task serves the goal picked on its page`);
  await shot("issue-pick");
  await overflow("the issue with a goal picker");

  await page.goto(`${base}/issue/${linked.key}`);
  const trace = page.locator("[data-testid=goal-trace] >> visible=true");
  await trace.waitFor();
  ok((await trace.textContent()).includes("Travellers find a route in one tap"), `${tag} the issue names its goal`);
  ok(await trace.getByText("Approved goal").isVisible(), `${tag} at an approved goal`);
  await trace.getByRole("button", { name: "how" }).click();
  ok(await trace.getByText(/serves/).first().isVisible(), `${tag} the route opens`);
  ok(await trace.getByText("by Robin Ames").isVisible(), `${tag} the confirmed link counts as the person's`);
  await shot("issue-trace");
  await overflow("the issue");

  const text = await (await call("GET", `/trace?ref=${linked.key}&format=text`, undefined, agent)).text();
  ok(text.includes("Travellers find a route in one tap"), `${tag} the agent's trace names the goal: ${text.slice(0, 300)}`);

  const noFlag = await call("POST", "/goals/drift/999999/answer", { answer: "intent" });
  ok(noFlag.status() === 404, `${tag} an unknown flag is a 404 from the API: ${noFlag.status()}`);
  const byAgent = await call("POST", "/goals/drift/999999/answer", { answer: "intent" }, agent);
  ok(byAgent.status() === 403, `${tag} an agent can't answer: ${byAgent.status()}`);
  let flags = [
    {
      id: 7001,
      kind: "contradicts",
      headline: "Drifts from “Travellers find a route in one tap”",
      goal: "Travellers find a route in one tap",
      reason: "Sends each traveller's searches to a hosted analytics service, which the goal has no need of and a very long explanation keeps going so it wraps at phone width.",
      model: "opencode/big-pickle",
      pr: { repo: "me/app", number: 12, title: "Send route searches to a hosted analytics service", url: "https://git.example.com/me/app/pulls/12", merged: true },
      items: [{ type: "issue", id: linked.id, key: linked.key, title: linked.title }],
      created_at: new Date().toISOString(),
    },
  ];
  const answers = [];
  await page.route(/\/api\/v1\/goals\?project=/, async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, json: { ...(await res.json()), drift_flags: flags } });
  });
  await page.route(/\/api\/v1\/goals\/drift\/7001\/answer/, async (route) => {
    const { answer } = route.request().postDataJSON();
    answers.push(answer);
    flags = answer === "stop" ? flags.map((f) => ({ ...f, answer })) : [];
    await route.fulfill({ json: {} });
  });
  await page.goto(`${base}/p/${KEY}/goals`);
  const drift = page.locator("[data-testid=goal-drift]");
  await drift.waitFor();
  ok(await drift.getByText("Drifts from “Travellers find a route in one tap”").isVisible(), `${tag} the flag names the goal`);
  ok(await drift.getByText("me/app#12", { exact: false }).isVisible(), `${tag} and the pull request`);
  ok(await drift.getByText("· merged", { exact: false }).isVisible(), `${tag} and that it already merged`);
  ok(await drift.getByText("A model's read (opencode/big-pickle)").isVisible(), `${tag} and says it is only a model's read`);
  await drift.scrollIntoViewIfNeeded();
  await shot("drift");
  await overflow("the drift list");
  await drift.getByRole("button", { name: "Stop this" }).click();
  await drift.getByText(/You said to stop this/).waitFor();
  ok(!(await drift.getByRole("button", { name: "Stop this" }).count()), `${tag} a stopped flag offers only the other answer`);
  await drift.getByRole("button", { name: "Fine", exact: true }).click();
  await drift.waitFor({ state: "detached" });
  ok(answers.join() === "stop,intent", `${tag} both answers went to the API: ${answers}`);
  await page.unrouteAll({ behavior: "wait" });

  const ask = await api("POST", "/issues", { project: KEY, title: "Make routes easy to share" });
  await page.route(/\/api\/v1\/goals\?project=/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const g of body.goals) {
      if (g.note.title === "Travellers find a route in one tap") {
        g.note.requested = { issue: ask.key, by: { id: 1, username: "robin", display_name: "Robin Ames" } };
      }
    }
    await route.fulfill({ response: res, json: body });
  });
  await page.goto(`${base}/p/${KEY}/goals`);
  const reqRow = page.getByTestId("goal-row").filter({ hasText: "Travellers find a route in one tap" });
  await reqRow.waitFor();
  ok(await reqRow.getByText(/^requested in .* by Robin Ames$/).isVisible(), `${tag} the goal says who requested it, in which task: ${await reqRow.textContent()}`);
  ok((await reqRow.getByRole("link", { name: ask.key }).getAttribute("href"))?.endsWith(`/issue/${ask.key}`), `${tag} and links the task`);
  await reqRow.scrollIntoViewIfNeeded();
  await shot("goal-requested");
  await overflow("a requested goal");
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
