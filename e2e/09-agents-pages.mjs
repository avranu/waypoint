// Agents, workflows and pages (WP-D7) in the UI: issue agent/category/
// workflow, agent-made hiding, pages (create, rename, edit, nest, link),
// the Workflows settings tab, agent tokens, device login approval, nested
// projects and pages in ⌘K. Run after 01-signup-flow (user robin, project WAYP).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const browser = await chromium.launch();
const H = { "X-Waypoint-Client": "web" };

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      !r.url().includes("/cli/login/") &&
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`),
  );
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  const until = async (get, done) => {
    let v;
    for (let i = 0; i < 100; i++) {
      v = await get();
      if (done(v)) break;
      await page.waitForTimeout(100);
    }
    return v;
  };
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const mobile = width < 500;

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const issue = await api("POST", "/issues", {
    project: "WAYP",
    title: `Agent work ${tag}`,
  });
  await page.goto(`${base}/issue/${issue.key}`);
  const props = mobile ? page : page.locator("aside");
  await props.getByTitle("Assignees").locator("visible=true").first().click();
  await page.getByRole("option", { name: /^Any agent/ }).last().click();
  await page.keyboard.press("Escape");
  const openField = async (name) => {
    const more = props.getByRole("button", { name: "More fields" }).locator("visible=true");
    if (await more.count()) {
      await more.first().click();
      await page.getByRole("menuitem", { name, exact: true }).click();
    } else {
      await props.getByTitle(name, { exact: true }).locator("visible=true").first().click();
    }
  };
  await openField("Category");
  await page
    .getByRole("option", { name: "Feature", exact: true })
    .last()
    .click();
  await openField("Workflow");
  await page.getByRole("button", { name: /Agent pipeline/ }).click();
  let d = await until(
    () => api("GET", `/issues/${issue.key}`),
    (x) =>
      x.agent === "any" && x.category === "feature" && x.workflow_id && x.step,
  );
  ok(d.agent === "any", `${tag} agent set from the picker`);
  ok(d.category === "feature", `${tag} category set`);
  ok(!!d.workflow_id && !!d.step, `${tag} on a workflow (step ${d.step})`);
  await props.getByTitle("Agent pipeline: plan").first().click();
  await page
    .getByRole("button", { name: /implement Make the change/ })
    .last()
    .click();
  d = await until(
    () => api("GET", `/issues/${issue.key}`),
    (x) => x.step?.toLowerCase() === "implement",
  );
  ok(
    d.step?.toLowerCase() === "implement",
    `${tag} step moved with the stepper`,
  );
  await page.getByRole("button", { name: "Add knowledge" }).click();
  ok(
    await page.getByRole("heading", { name: "Knowledge" }).isVisible(),
    `${tag} knowledge (pages, notes, terms) block on the issue`,
  );
  await shot("issue");

  await page.goto(`${base}/p/WAYP/list`);
  await page.getByText(issue.title).first().waitFor();
  if (!mobile)
    ok(
      await page.getByTitle("For agents: any").first().isVisible(),
      `${tag} robot chip on the row`,
    );
  const toggle = page.getByRole("button", { name: "Show issues agents made" });
  ok(
    (await toggle.getAttribute("aria-pressed")) === "false",
    `${tag} agent-made hidden by default`,
  );
  await shot("toolbar-off");
  await toggle.click();
  ok(
    (await toggle.getAttribute("aria-pressed")) === "true",
    `${tag} agent-made toggle`,
  );
  await page.mouse.move(700, 500);
  await page.waitForTimeout(400);
  await shot("toolbar-on");
  await page.goto(`${base}/p/WAYP/board`);
  await toggle.waitFor();
  ok(
    (await toggle.getAttribute("aria-pressed")) === "true",
    `${tag} board toolbar shows the state`,
  );
  await page.waitForTimeout(400);
  await shot("board-toolbar-on");
  await toggle.click();
  ok(
    (await toggle.getAttribute("aria-pressed")) === "false",
    `${tag} board toggle turns it off`,
  );
  await page.mouse.move(700, 500);
  await page.waitForTimeout(400);
  await shot("board-toolbar-off");
  await page.goto(`${base}/p/WAYP/list`);
  await page.getByText(issue.title).first().waitFor();
  await shot("list");

  await page.goto(`${base}/pages`);
  await page.getByRole("button", { name: "New", exact: true }).click();
  await page.waitForURL(/\/pages\/\d+$/);
  const title = page.getByLabel("Page title");
  const name = `Plan ${tag} ${Date.now() % 100000}`;
  await title.fill(name);
  await title.press("Enter");
  await page
    .getByRole("button", { name: "Write the page… (markdown)" })
    .click();
  await page.locator("textarea").first().fill("## Goal\n\nShip the thing.");
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page.getByRole("heading", { name: "Goal" }).waitFor();
  await page.getByRole("button", { name: /^Category$/ }).click();
  await page.getByRole("option", { name: "Plan", exact: true }).last().click();
  const id = Number(page.url().split("/").pop());
  let pg = await until(
    () => api("GET", `/pages/${id}`),
    (x) => x.category === "plan",
  );
  ok(
    pg.title === name && pg.body.includes("Ship") && pg.category === "plan",
    `${tag} page renamed, written, categorised`,
  );

  await page.getByRole("button", { name: "Add a link" }).click();
  await page.getByLabel("Find something to link").fill(issue.key);
  await page.getByRole("option", { name: `Link ${issue.key}` }).click();
  await page.getByRole("link", { name: new RegExp(issue.key) }).waitFor();
  ok(true, `${tag} issue linked to the page`);
  await page.keyboard.press("Escape");

  await page.getByRole("button", { name: "Page actions" }).click();
  await page.getByRole("menuitem", { name: "New sub-page" }).click();
  await page.waitForURL((u) => !u.pathname.endsWith(`/${id}`));
  const child = Number(page.url().split("/").pop());
  await page.waitForFunction(
    () =>
      document.querySelector('[aria-label="Page title"]')?.value === "Untitled",
  );
  if (!mobile)
    ok(
      await page
        .getByRole("navigation", { name: "Breadcrumbs" })
        .getByRole("link", { name })
        .waitFor()
        .then(
          () => true,
          () => false,
        ),
      `${tag} breadcrumbs show the parent`,
    );
  await shot("page");
  await page.getByRole("button", { name: "Page actions" }).click();
  await page.getByRole("menuitem", { name: "Delete…" }).click();
  await page.getByRole("button", { name: "Delete" }).click();
  await page.waitForURL(base + "/pages");
  ok(
    (await api("GET", `/pages/${id}`)).child_count === 0,
    `${tag} sub-page ${child} deleted`,
  );
  if (!mobile) {
    await page.getByLabel("Search pages").fill("Ship the thing");
    await page.getByRole("link", { name }).waitFor();
    ok(true, `${tag} full-text page search`);
  }
  await shot("pages");

  d = await api("GET", `/issues/${issue.key}`);
  ok(
    d.pages?.some((p) => p.id === id),
    `${tag} issue shows the linked page`,
  );

  if (!mobile) {
    await page.goto(base + "/");
    await page
      .getByRole("button", { name: /^Search/ })
      .first()
      .click();
    await page.keyboard.type(name);
    await page.getByText(name).last().waitFor();
    await shot("palette");
    ok(true, `${tag} pages in the command palette`);
    await page.keyboard.press("Escape");
  }

  await page.goto(`${base}/p/WAYP/settings?tab=workflows`);
  await page.getByText("Agent pipeline").first().waitFor();
  await page.getByRole("button", { name: "New workflow" }).click();
  await page
    .getByRole("textbox", { name: "Name", exact: true })
    .fill(`Review flow ${tag}`);
  await page.getByLabel("Step 1 name").fill("Write");
  await page.getByLabel("Step 2 name").fill("Review");
  await page.getByRole("combobox", { name: "Step 2 agent" }).click();
  await page
    .getByRole("option", { name: "A different agent than before" })
    .click();
  await page.getByRole("button", { name: "Save workflow" }).click();
  await page.getByText(`Review flow ${tag}`).waitFor();
  const wfs = await api("GET", "/workflows?project=WAYP");
  const wf = wfs.find((w) => w.name === `Review flow ${tag}`);
  ok(wf?.steps[1]?.agent === "different", `${tag} project workflow saved`);
  await shot("workflows");
  await api("DELETE", `/workflows/${wf.id}`);

  await page.goto(`${base}/settings?tab=tokens`);
  await page.getByRole("button", { name: "New token" }).click();
  await page.getByPlaceholder("Claude Code").fill(`bot ${tag}`);
  await page.getByRole("switch", { name: "Agent token" }).click();
  await page
    .getByPlaceholder("anthropic/claude-opus-5-5")
    .fill("anthropic/claude-haiku-5-5");
  await page.getByRole("button", { name: "Create token" }).click();
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByText("claude-haiku-5-5").first().waitFor();
  ok(true, `${tag} agent token listed with its agent`);

  const start = await (
    await page.request.fetch(base + "/api/v1/cli/login", {
      method: "POST",
      headers: H,
      data: { name: "e2e laptop", agent: "anthropic/claude-sonnet-5-5" },
    })
  ).json();
  await page.goto(`${base}/device`);
  await page.getByLabel("Login code").fill(start.user_code);
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByText("e2e laptop").waitFor();
  await shot("device");
  await page.getByRole("button", { name: "Approve" }).click();
  await page.getByText("Approved").waitFor();
  const poll = await (
    await page.request.fetch(base + "/api/v1/cli/login/poll", {
      method: "POST",
      headers: H,
      data: { device_code: start.device_code },
    })
  ).json();
  ok(poll.status === "approved", `${tag} device login approved`);

  const sub = `S${width}${scheme[0].toUpperCase()}`.slice(0, 10);
  if (!(await api("GET", "/projects/" + sub))?.key)
    await api("POST", "/projects", { name: `Sub ${tag}`, key: sub });
  await api("PATCH", "/projects/" + sub, { parent: "WAYP" });
  await page.goto(`${base}/p/${sub}/settings`);
  ok(
    await page
      .getByRole("combobox", { name: "Parent project" })
      .filter({ hasText: "Waypoint" })
      .waitFor()
      .then(
        () => true,
        () => false,
      ),
    `${tag} parent project shown in settings`,
  );
  if (!mobile) {
    const link = page
      .locator("aside")
      .getByRole("link", { name: `Sub ${tag}` });
    const pad = await link.evaluate((e) =>
      parseInt(getComputedStyle(e).paddingLeft),
    );
    ok(pad > 8, `${tag} sub-project indented in the sidebar`);
    await page
      .getByRole("button", { name: "Hide Waypoint's sub-projects" })
      .click();
    ok(!(await link.isVisible()), `${tag} sub-projects collapse`);
    await page
      .getByRole("button", { name: "Show Waypoint's sub-projects" })
      .click();
  }
  await shot("nested");
  await ctx.close();
}

for (const [w, s] of looks([
  [1440, "dark"],
  [390, "light"],
  [1440, "light"],
  [390, "dark"],
]))
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }
await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
