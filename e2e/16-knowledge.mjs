// The glossary, design notes, links, wikilinks and search everything (ADR
// 0010) in a real browser at desktop and phone widths: add terms and lock
// one (agents then can't change it), review an agent's proposed note,
// additions and edit (approve, reject, merge), the review queue, links
// added and removed, [[wikilinks]] that resolve or offer to create, search
// across kinds and the ⌘K palette. Agents' work is made through the API with
// an agent token, so it waits for a person.
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
  const mobile = width < 500;
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
  page.on("response", (r) => {
    if (
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      !r.request().headers()["authorization"]
    )
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`);
  });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const settle = () => page.waitForTimeout(400);

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const n = `${width}${scheme[0]}`.toUpperCase();
  const org = `KO${n}`;
  const repo = `KR${n}`;
  await api("POST", "/projects", { name: `Org ${tag}`, key: org });
  await api("POST", "/projects", { name: `Repo ${tag}`, key: repo });
  await api("PATCH", `/projects/${repo}`, { parent: org });
  const { token } = await api("POST", "/me/tokens", {
    name: `bot ${tag}`,
    agent: "anthropic/claude-haiku-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  const agent = (m, p, d) => call(m, p, d, AG);
  const issue = await api("POST", "/issues", {
    project: repo,
    title: `Lease work ${tag}`,
  });
  const word = `zephyr${n.toLowerCase()}`;

  await api("POST", "/glossary", {
    term: "Claim",
    definition: "Org-wide meaning.",
    project: org,
  });
  await page.goto(`${base}/p/${repo}/glossary`);
  await page.getByRole("button", { name: /New term/ }).click();
  await page.getByLabel("Term", { exact: true }).fill("Lease");
  await page.getByLabel("Aliases").fill("hold, reservation");
  await page.getByLabel("Definition").fill(`How long a claim lasts. ${word}`);
  await page.getByRole("button", { name: "Add term" }).click();
  await page.getByRole("link", { name: /Lease/ }).first().waitFor();
  await page.getByRole("button", { name: "Close" }).click();
  const full = await api("GET", `/glossary?project=${repo}`);
  const lease = full.find((e) => e.term === "Lease");
  ok(
    lease?.project_key === repo && lease.aliases.length === 2,
    `${tag} term added with aliases`,
  );
  ok(
    full.some((e) => e.term === "Claim" && e.inherited),
    `${tag} full glossary includes the org's terms`,
  );
  await page.getByRole("switch", { name: "Only this project's terms" }).click();
  await page.getByText(org, { exact: true }).first().waitFor({ timeout: 5000 }).catch(() => {});
  ok(
    await page.getByText(org, { exact: true }).first().isVisible(),
    `${tag} inherited term shows its scope`,
  );
  await shot("glossary");

  await page.getByRole("link", { name: /Lease/ }).first().click();
  await page.waitForURL(/\/glossary\/\d+$/);
  await page.getByRole("button", { name: "Add an alias" }).click();
  const saved = () =>
    page.waitForResponse(
      (r) =>
        r.request().method() === "PATCH" &&
        r.url().endsWith(`/api/v1/glossary/${lease.id}`),
    );
  const aliasSaved = saved();
  await page.getByLabel("Add an alias").fill("tenure");
  await page.getByLabel("Add an alias").press("Enter");
  await aliasSaved;
  const lockSaved = saved();
  await page.getByRole("button", { name: "Lock term" }).click();
  await lockSaved;
  await page.getByRole("button", { name: "Unlock term" }).waitFor();
  let e = await api("GET", `/glossary/${lease.id}`);
  ok(
    e.locked && e.aliases.includes("tenure"),
    `${tag} alias added and term locked`,
  );
  const refused = await agent("PATCH", `/glossary/${lease.id}`, {
    definition: "changed",
  });
  ok(
    refused.status() === 403,
    `${tag} agents can't change a locked term (${refused.status()})`,
  );
  ok((e.history?.length ?? 0) >= 2, `${tag} entry history kept`);
  await shot("glossary-entry");

  const note = await api("POST", "/design", {
    title: `Leases ${tag}`,
    kind: "feature",
    project: repo,
    body: `Tasks are held by a [[Lease]]. Later: [[Grace period ${n}]].\n\n${word} first line`,
  });
  await agent("POST", `/design/${note.id}/additions`, {
    body: "Renew on every commit.",
  });
  await agent("POST", `/design/${note.id}/additions`, {
    body: `Merge me ${word}.`,
  });
  await agent("PATCH", `/design/${note.id}`, {
    title: `Leases ${tag} (renewable)`,
    body: `Tasks are held by a [[Lease]], renewed on commit. Later: [[Grace period ${n}]].\n\n${word} first line`,
  });
  const proposed = await (
    await agent("POST", "/design", {
      title: `Retry claims ${tag}`,
      kind: "requirement",
      project: repo,
      parent: note.id,
      body: "Try the next task when one is taken.",
    })
  ).json();
  ok(!proposed.approved, `${tag} an agent's note starts proposed`);

  await page.goto(`${base}/design/${note.id}`);
  await page.getByRole("heading", { name: "Proposed edits" }).waitFor();
  ok(
    await page.getByText("unapproved agent addition").first().isVisible(),
    `${tag} pending additions are marked`,
  );
  await shot("design-note");
  await page.getByRole("button", { name: "Approve edit" }).click();
  await page.waitForFunction(
    (want) =>
      document.querySelector('textarea[aria-label="Note title"]')?.value === want,
    `Leases ${tag} (renewable)`,
  );
  await page
    .getByRole("button", { name: "Reject", exact: true })
    .first()
    .click();
  await page.getByLabel("Reason for rejecting").fill("Too chatty");
  await page
    .getByRole("button", { name: "Reject", exact: true })
    .last()
    .click();
  await settle();
  await page.getByRole("button", { name: "Merge into body" }).first().click();
  await settle();
  let d = await api("GET", `/design/${note.id}`);
  for (let i = 0; i < 25 && d.pending !== 0; i++) {
    await page.waitForTimeout(200);
    d = await api("GET", `/design/${note.id}`);
  }
  ok(d.title.endsWith("(renewable)"), `${tag} approved edit applied`);
  ok(
    d.body.includes(`Merge me ${word}.`) && d.pending === 0,
    `${tag} addition merged; nothing pending (${d.pending})`,
  );
  ok(
    d.history.some(
      (r) => r.state === "rejected" && r.review_note === "Too chatty",
    ),
    `${tag} rejection kept in the history with its note`,
  );
  await page.getByRole("heading", { name: "History" }).scrollIntoViewIfNeeded();
  await shot("design-history");

  await page.getByRole("tab", { name: "Spec" }).click();
  await page.getByText(`Retry claims ${tag}`).locator("visible=true").first().waitFor({ timeout: 5000 }).catch(() => {});
  ok(
    await page.getByText(`Retry claims ${tag}`).first().isVisible(),
    `${tag} spec lists the parts`,
  );
  await shot("design-spec");
  await page.getByRole("tab", { name: "Note" }).click();

  await page.getByRole("button", { name: "Add a link" }).click();
  await page.getByLabel("Find something to link").fill(issue.key);
  await page.getByRole("option", { name: `Link ${issue.key}` }).click();
  await page.getByRole("link", { name: new RegExp(issue.title) }).waitFor();
  await page
    .getByLabel("Find something to link")
    .fill("https://github.com/acme/rocket/pull/7");
  await page.getByRole("option", { name: /Link https/ }).click();
  await page.getByText("acme/rocket PR #7").waitFor();
  await page.keyboard.press("Escape");
  let links = await api("GET", `/links?type=design&id=${note.id}`);
  ok(
    links.some((l) => l.item.key === issue.key) &&
      links.some((l) => l.item.type === "url") &&
      links.some((l) => l.auto && l.item.type === "glossary"),
    `${tag} links: issue, PR, and the wikilink's automatic one`,
  );
  await page
    .getByRole("button", { name: "Remove link to acme/rocket PR #7" })
    .click();
  await settle();
  links = await api("GET", `/links?type=design&id=${note.id}`);
  ok(!links.some((l) => l.item.type === "url"), `${tag} link removed`);
  await page.goto(`${base}/issue/${issue.key}`);
  await page.getByRole("heading", { name: "Knowledge" }).waitFor();
  await page
    .getByRole("link", { name: new RegExp(`Leases ${tag}`) })
    .first()
    .waitFor();
  ok(true, `${tag} the issue lists the note linked to it`);

  await page.goto(`${base}/design/${proposed.id}`);
  await page.getByText(/not approved yet/).waitFor();
  await shot("design-proposed");
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await page.getByText(/Approved by/).waitFor();
  ok(
    (await api("GET", `/design/${proposed.id}`)).approved,
    `${tag} proposed note approved`,
  );

  await agent("POST", `/design/${note.id}/additions`, {
    body: `Queue item ${tag}`,
  });
  await page.goto(`${base}/design/review?project=${repo}`);
  const card = page.locator("article").filter({ hasText: `Queue item ${tag}` });
  await card.waitFor();
  await shot("review");
  await card.getByRole("button", { name: "Approve" }).click();
  await card.waitFor({ state: "detached" });
  let approved = false;
  for (let i = 0; i < 25 && !approved; i++) {
    if (i) await page.waitForTimeout(200);
    d = await api("GET", `/design/${note.id}`);
    approved = d.additions.some(
      (a) => a.body === `Queue item ${tag}` && a.state === "approved",
    );
  }
  ok(approved, `${tag} approved from the review queue`);

  await page.goto(`${base}/design/${note.id}`);
  await page.locator("a.wikilink", { hasText: "Lease" }).first().click();
  await page.waitForURL(new RegExp(`/glossary/${lease.id}$`));
  ok(true, `${tag} [[Lease]] opens the term`);
  await page.goto(`${base}/design/${note.id}`);
  await page.locator("a.wikilink", { hasText: `Grace period ${n}` }).click();
  await page.getByText(`Nothing called “Grace period ${n}” yet`).waitFor();
  await shot("wikilink-unresolved");
  await page.getByRole("button", { name: "Create glossary term" }).click();
  await page.waitForURL(/\/glossary\/\d+$/);
  e = await api("GET", `/glossary/${page.url().split("/").pop()}`);
  ok(
    e.term === `Grace period ${n}` && e.project_key === repo,
    `${tag} term created from a wikilink`,
  );
  await page.getByRole("button", { name: "Term actions" }).click();
  await page.getByRole("menuitem", { name: "Delete…" }).click();
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.waitForURL(new RegExp(`/p/${repo}/glossary$`));
  ok(
    (await call("GET", `/glossary/${e.id}`)).status() === 404,
    `${tag} term deleted`,
  );

  await page.goto(`${base}/search?q=${word}`);
  await page.getByRole("heading", { name: "Design notes" }).waitFor();
  ok(
    await page.getByRole("heading", { name: "Glossary" }).isVisible(),
    `${tag} search finds design notes and glossary terms`,
  );
  ok((await page.locator("mark").count()) > 0, `${tag} matches highlighted`);
  await shot("search");

  if (!mobile) {
    await page.goto(base + "/");
    await page.keyboard.press("Control+k");
    await page.keyboard.type("Lease");
    await page.getByText("Glossary", { exact: true }).last().waitFor();
    ok(true, `${tag} glossary terms in the command palette`);
    await page.keyboard.press("Escape");
    ok(
      await page
        .locator("aside")
        .getByRole("link", { name: /Design/ })
        .isVisible(),
      `${tag} Design in the sidebar`,
    );
  } else {
    await page.goto(`${base}/p/${repo}/design`);
    await page
      .getByRole("link", { name: new RegExp(`Leases ${tag}`) })
      .waitFor();
    const over = await page.evaluate(
      () => document.documentElement.scrollWidth - innerWidth,
    );
    ok(over <= 1, `${tag} no sideways scroll on the design list (${over}px)`);
    await shot("project-design");
  }

  await page.goto(`${base}/design/${proposed.id}`);
  await page.getByRole("button", { name: "Note actions" }).click();
  await page.getByRole("menuitem", { name: "Delete…" }).click();
  await page.getByText("Retiring keeps the record").waitFor();
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.waitForURL(new RegExp(`/p/${repo}/design$`));
  ok(
    (await call("GET", `/design/${proposed.id}`)).status() === 404,
    `${tag} note deleted`,
  );
  await ctx.close();
}

for (const [w, s] of looks([
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
