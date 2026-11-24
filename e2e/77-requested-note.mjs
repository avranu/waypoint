// A decision an agent writes for a task a person filed counts on that
// request, but nobody approved it (WP-409, WP-ADR96): its page says which
// task it was written for and who filed it, never "Approved by" that person;
// the agent's addition is applied, not approved, and a person can merge it;
// the Spec tab says "written for" too. Desktop and 390px.
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
  const KEY = `REQ${"ABCD"[n++]}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      !/Failed to load resource/.test(m.text()) &&
      fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  const shot = (name) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  const call = (m, p, d, headers = H) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers, data: d });
  const api = async (m, p, d, headers) => (await call(m, p, d, headers)).json();

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("POST", "/projects", { name: `Requests ${tag}`, key: KEY });
  const { token } = await api("POST", "/me/tokens", {
    name: `requested ${tag}`,
    agent: "anthropic/claude-opus-5-5",
  });
  const agent = { ...H, Authorization: `Bearer ${token}` };

  const task = await api("POST", "/issues", {
    project: KEY,
    title: "Pick the export format",
  });
  const note = await api(
    "POST",
    "/design",
    {
      title: "Export as CSV",
      kind: "decision",
      body: "Because spreadsheets.",
      project: KEY,
      for: task.key,
    },
    agent,
  );
  ok(
    note.approved && note.requested?.issue === task.key,
    `${tag} the note counts on ${task.key}: ${JSON.stringify(note.requested)}`,
  );
  ok(
    !note.approved_by,
    `${tag} nobody is recorded as its approver: ${JSON.stringify(note.approved_by)}`,
  );
  ok(
    /^Saved for /.test(note.notice ?? "") && !/^Accepted/.test(note.notice),
    `${tag} the agent isn't told it was accepted: ${note.notice}`,
  );
  const added = await call(
    "POST",
    `/design/${note.id}/additions`,
    { body: "Also quote every field." },
    agent,
  );
  ok(added.ok(), `${tag} the agent's addition applies: ${added.status()}`);

  const specResp = page.waitForResponse(
    (r) =>
      r.url().includes("/api/v1/spec?") && !r.url().includes("format=text"),
  );
  await page.goto(`${base}/design/${note.id}`);
  const spec = await (await specResp).json();
  const banner = page.getByText("Written for", { exact: false }).first();
  await banner.waitFor();
  ok(
    await page.getByRole("link", { name: task.key }).first().isVisible(),
    `${tag} the banner links ${task.key}`,
  );
  ok(
    (await page.getByText(/Approved by/).count()) === 0,
    `${tag} the page never says "Approved by"`,
  );
  ok(
    await page.getByText("Also quote every field.").first().isVisible(),
    `${tag} the addition shows`,
  );
  ok(
    !(await page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth,
    )),
    `${tag} no sideways scroll`,
  );
  ok(
    await page
      .getByRole("button", { name: "Merge into body" })
      .first()
      .isVisible(),
    `${tag} a person can merge the applied addition into the body`,
  );
  await shot("requested");

  const specTab = page.getByRole("tab", { name: "Spec" });
  const specAdds = [
    "parts",
    "related",
    "terms",
    "issues",
    "changes",
    "files",
    "others",
  ].some((k) => (spec[k] ?? []).length > 0);
  if (!specAdds) {
    ok(
      (await specTab.count()) === 0,
      `${tag} the Spec tab is hidden: the spec would only repeat the note`,
    );
  } else {
    await specTab.click();
    await page
      .getByText(
        /^(Parts|Related|Terms|Tasks|Some PRs and commits|Files|Other notes that matched)$/,
      )
      .first()
      .waitFor();
    ok(
      (await page.getByText("Also quote every field.").count()) === 0,
      `${tag} the Spec tab doesn't repeat the applied addition (it shows on the note)`,
    );
  }
  ok(
    (await page.getByText(/approved by|^approved/i).count()) === 0,
    `${tag} the spec never says "approved"`,
  );
  await shot("requested-spec");
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
