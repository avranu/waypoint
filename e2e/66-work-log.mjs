// The work log (WP-285) in a real browser at desktop and phone widths, light
// and dark: autopilot's Adversarial review limit and its prompt on Agents →
// Autopilot; the issue page's Work log block with a counted review round (its
// issues and report open on a click), a note added from the page that counts
// as an issue found, the stats the rounds add up to, and a ship's test
// evidence and a statement it contradicts (WP-715).
// Run after 01-signup-flow (user robin, an admin).
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

async function run(width, scheme, n) {
  const tag = `${width}/${scheme}`;
  const KEY = `WL${"ABCDEFGH"[n]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(250);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );
  const pick = async (scope, name, option) => {
    await scope.getByRole("combobox", { name, exact: true }).click();
    await page.getByRole("option", { name: option, exact: true }).click();
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  await api("POST", "/projects", { name: `Work log ${n}`, key: KEY });

  await page.goto(`${base}/agents?tab=autopilot`);
  const row = page.getByTestId(`autopilot-${KEY}`);
  await row.waitFor();
  await page.getByRole("switch", { name: `Autopilot in ${KEY}` }).click();
  await row.getByText("No sessions of yours open here").waitFor();
  await row.getByRole("button", { name: `Limits for ${KEY}` }).click();
  const dlg = page.getByRole("dialog", { name: "Autopilot settings" });
  await dlg.waitFor();
  const prompt = page.locator(`#p-${KEY}-review_prompt`);
  ok((await prompt.count()) === 0, `${tag} no review prompt while the review is off`);
  await pick(dlg, "Adversarial review", "On");
  await prompt.waitFor();
  ok(
    ((await prompt.getAttribute("placeholder")) || "").startsWith("Run an adversarial review. Assess and fix any legitimate issues"),
    `${tag} the prompt field shows the default`,
  );
  await prompt.fill("Attack this change and fix what breaks.");
  await prompt.blur();
  await page.waitForFunction(async ([b, k]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return p && p.limits.review === true && p.limits.review_prompt === "Attack this change and fix what breaks.";
  }, [base, KEY]);
  ok(true, `${tag} the project's review and prompt are saved`);

  const brief = page.locator(`#p-${KEY}-review_text`);
  await brief.waitFor();
  ok(
    ((await brief.getAttribute("placeholder")) || "").includes("Burden of proof"),
    `${tag} the brief field shows the default brief`,
  );
  await pick(dlg, "Reviewer", "The same session");
  await page.waitForFunction(async ([b, k]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return p && p.limits.review_model === "session";
  }, [base, KEY]);
  ok(true, `${tag} the reviewer choice is saved`);
  await dlg.getByRole("button", { name: "Start from it" }).click();
  ok(((await brief.inputValue()) || "").includes("Burden of proof"), `${tag} Start from it fills in the default brief`);
  await brief.fill("Attack the parser. Name the input that breaks it.");
  await brief.blur();
  await page.waitForFunction(async ([b, k]) => {
    const o = await (await fetch(b + "/api/v1/autopilot")).json();
    const p = o.projects.find((x) => x.project_key === k);
    return p && p.limits.review_text === "Attack the parser. Name the input that breaks it.";
  }, [base, KEY]);
  ok(true, `${tag} the reviewer's brief is saved`);
  await shot("autopilot-review");
  await overflow("autopilot review limits");

  const briefIs = (want) =>
    page.waitForFunction(async ([b, k, w]) => {
      const o = await (await fetch(b + "/api/v1/autopilot")).json();
      const p = o.projects.find((x) => x.project_key === k);
      return p && (w === null ? p.limits.review_text === undefined : p.limits.review_text === w);
    }, [base, KEY, want]);
  const mine = (await api("GET", "/autopilot")).defaults;
  const house = "House brief 🔥🔥: attack the migrations first.";
  await api("PUT", "/autopilot", { limits: { review_text: house }, merge: true });
  await page.reload();
  await page.getByTestId(`autopilot-${KEY}`).getByRole("button", { name: `Limits for ${KEY}` }).click();
  await dlg.waitFor();
  await brief.fill(house + "!");
  // The server counts this limit in code points (utf8.RuneCountInString), so the client must count the same way.
  await dlg.getByText(`${[...house].length + 1} of 12,000 characters`).waitFor();
  ok(true, `${tag} the brief's length is counted in characters, as the server counts it`);
  await brief.fill("");
  await brief.blur();
  await briefIs(null);
  ok((await brief.getAttribute("placeholder")) === house, `${tag} an empty brief inherits the custom default, shown in grey`);
  await dlg.getByRole("button", { name: "Use the built-in" }).click();
  await briefIs("");
  await dlg.getByText("Set to Waypoint’s built-in brief, shown in grey.").waitFor();
  ok(
    ((await brief.getAttribute("placeholder")) || "").includes("Burden of proof"),
    `${tag} Use the built-in sets the project back to Waypoint's brief under a custom default`,
  );
  await shot("autopilot-builtin-brief");
  await overflow("the built-in brief chosen");
  await page.reload();
  await page.getByTestId(`autopilot-${KEY}`).getByRole("button", { name: `Limits for ${KEY}` }).click();
  await dlg.getByText("Set to Waypoint’s built-in brief, shown in grey.").waitFor();
  ok(true, `${tag} the choice is still shown after a reload`);
  await dlg.getByRole("button", { name: "Inherit instead" }).click();
  await briefIs(null);
  ok((await brief.getAttribute("placeholder")) === house, `${tag} Inherit instead goes back to the default's brief`);
  // Later tests in this suite run against these autopilot limits, so restore them here.
  await api("PUT", "/autopilot", { limits: mine });
  await brief.fill("Attack the parser. Name the input that breaks it.");
  await brief.blur();
  await briefIs("Attack the parser. Name the input that breaks it.");

  const issue = await api("POST", "/issues", { project: KEY, title: `Parser of ${KEY}` });
  const empty = await api("GET", `/issues/${issue.key}/worklog`);
  ok(empty.entries.length === 0, `${tag} a new task's work log is empty`);
  const loaded = page.waitForResponse((r) => r.url().endsWith(`/issues/${issue.key}/worklog`));
  await page.goto(`${base}/issue/${issue.key}`);
  await loaded;
  await page.waitForTimeout(300);
  ok((await page.getByRole("heading", { name: "Work log" }).count()) === 0, `${tag} the issue page has no Work log block for an empty log`);

  const db = new DatabaseSync(`${process.env.DIR}/wp.db`);
  db.exec("PRAGMA busy_timeout = 5000");
  const now = Date.now();
  const items = [
    { claim: "A nil map in merge.go panics when the base has no labels", file: "internal/merge.go", outcome: "fixed" },
    { claim: "The retry loop might double-post", file: "", outcome: "rejected" },
  ];
  db.prepare(
    `INSERT INTO work_log(issue_id, kind, state, agent, prompt, body, found, fixed, rejected, items, heads, read_state, read_by, started_at, ended_at, created_at, updated_at)
     VALUES (?, 'review', 'done', 'claude', ?, ?, 2, 1, 1, ?, ?, 'done', 'free/model', ?, ?, ?, ?)`,
  ).run(
    issue.id,
    "Attack this change and fix what breaks.",
    "Reviewed the parser change.\n\n1. Fixed: a nil map in merge.go panicked.\n2. Rejected: the retry loop can't double-post; the key is unique.",
    JSON.stringify(items),
    JSON.stringify({ "a/b#1": { start: "aaa", end: "bbb" } }),
    now - 600_000,
    now - 60_000,
    now - 600_000,
    now - 60_000,
  );
  db.close();

  await page.reload();
  const block = page.locator("section", { has: page.getByRole("heading", { name: "Work log" }) });
  await block.waitFor();
  const head = await block.innerText();
  ok(/1 review round: 2 found, 1 fixed, 1 rejected/.test(head), `${tag} the block sums the round (${head.replace(/\s+/g, " ").slice(0, 160)})`);
  const round = block.getByRole("button", { name: /Adversarial review: found 2 issues, fixed 1/ });
  await round.click();
  await block.getByText("A nil map in merge.go panics").waitFor();
  ok((await block.getByText("The agent’s report:").count()) === 1, `${tag} the round opens to its issues and report`);
  ok((await block.getByText("Counted by free/model.").count()) === 1, `${tag} it says which model counted it`);
  await shot("issue-round");
  await overflow("issue round open");

  await block.getByRole("button", { name: "+ Note" }).click();
  await block.getByLabel("Note").fill("The review missed the empty-input case; caught in QA.");
  await pick(block, "Count it as", "An issue found");
  await shot("issue-note-form");
  await overflow("note form");
  await block.getByRole("button", { name: "Add note" }).click();
  await block.getByText("The review missed the empty-input case").waitFor();
  await block.getByText("1 note", { exact: true }).waitFor();
  ok(/1 found, 0 fixed, 0 rejected in notes/.test(await block.innerText()) && !/in 0 rounds/.test(await block.innerText()), `${tag} a note tagged found counts, apart from the rounds`);
  const log = await api("GET", `/issues/${issue.key}/worklog`);
  const note = log.entries.find((e) => e.kind === "note");
  ok(note && note.tag === "found" && note.author?.username === "robin", `${tag} the note is stored, tagged and attributed`);
  await shot("issue-note");
  await overflow("note added");

  const stats = await api("GET", `/worklog/stats?project=${KEY}&days=30`);
  ok(stats.total.rounds === 1 && stats.total.model_read.rounds === 1 && stats.total.model_read.found === 2 && stats.total.measured.rounds === 0 && stats.total.pushed === 1, `${tag} the stats count the round (${JSON.stringify(stats.total)})`);

  const long = "go test -run " + "VeryLongTestNameWithoutAnyBreak".repeat(8) + " ./...";
  const failed = { at: Date.now(), command: long, outcome: "fail", passed: 118, failed: 2, skipped: 0, units: 3, unit_name: "packages", failures: ["TestA", "TestB"], seconds: 4, duration: 5 };
  const sent = await api("POST", `/projects/${KEY}/test-evidence`, {
    repo: "acme/app", sha: "abc1234def", tree: "tree1", issue: issue.key, summary: "All tests pass.", runs: [failed],
  });
  ok(sent.outcome === "fail" && sent.notices?.length === 1, `${tag} a false statement comes back as a notice (${JSON.stringify(sent.notices)})`);
  await page.reload();
  await block.getByText(/Tests on this tree: 2 failed, 118 passed/).waitFor();
  await block.getByText("Said: All tests pass.").waitFor();
  await block.getByText("1 statement the records contradict", { exact: true }).waitFor();
  await block.getByRole("button", { name: /Said: All tests pass/ }).click();
  await block.getByText(/Waypoint's record at abc1234/).waitFor();
  await shot("issue-evidence");
  await overflow("test evidence with a long command");
  const after = await api("GET", `/worklog/stats?project=${KEY}&days=30`);
  ok(after.evidence?.length === 1 && after.evidence[0].ships === 1 && after.evidence[0].mismatches === 1, `${tag} the week counts the ship and the mismatch (${JSON.stringify(after.evidence)})`);
  await ctx.close();
}

for (const [n, [w, s]] of looks([[1280, "light"], [390, "dark"]]).entries()) await run(w, s, n);

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("\nwork log suite passed");
