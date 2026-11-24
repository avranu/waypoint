// Autopilot's wording (WP-308): Admin → AI words the prompt autopilot pastes
// and the note at the Stop for every project; a project's settings word its
// own, which wins and falls back when cleared. The previews fill in an
// example task. Desktop and 390px.
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

let n = 0;
for (const [width, scheme] of looks([
  [1440, "light"],
  [390, "dark"],
])) {
  const tag = `${width}/${scheme}`;
  const KEY = `AW${"ABCD"[n++]}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  await api("POST", "/projects", { name: `Wording ${tag}`, key: KEY });
  const noSideways = async (what) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} ${what}: no sideways scroll`);

  await page.goto(`${base}/admin?tab=ai`);
  let section = page.locator("section", { has: page.getByRole("heading", { name: "Autopilot’s wording" }) });
  let [prompt, done] = [section.getByRole("textbox").nth(0), section.getByRole("textbox").nth(1)];
  await prompt.waitFor();
  ok((await prompt.inputValue()) === "{next}", `${tag} shows the default prompt`);
  ok((await done.inputValue()) === "Autopilot: {key} is done. {status} Type `keep` to keep it.", `${tag} shows the default note`);
  ok(await section.getByText("Autopilot: WP-123 is done. The next task starts in about a minute.").isVisible(), `${tag} the note's preview fills in an example`);

  await prompt.fill("Do the next task.");
  ok(await section.getByText("Name the task with {key} or {next}").isVisible(), `${tag} a prompt without its task is refused`);
  await page.waitForTimeout(2800); // Autosave fires 900 ms after typing pauses; wait well past it so a save that should not happen has time to show.
  ok(!(await api("GET", "/autopilot/text")).custom_prompt, `${tag} and isn't saved`);
  await prompt.fill("Take {key} ({title}).\n\n{next}");
  await done.fill("{key} done. {status}");
  ok(await section.getByText("Take WP-123 (Fix the export button).").isVisible(), `${tag} the prompt's preview follows the edit`);
  await section.getByText("Saved", { exact: true }).waitFor();
  const saved = await api("GET", "/autopilot/text");
  ok(saved.custom_prompt && saved.prompt === "Take {key} ({title}).\n\n{next}" && saved.done === "{key} done. {status}", `${tag} saved: ${JSON.stringify(saved)}`);
  await noSideways("Admin → AI");
  await section.scrollIntoViewIfNeeded();
  await section.screenshot({ path: `${out}/admin-${width}-${scheme}.png` });

  await page.goto(`${base}/p/${KEY}/settings?tab=general`);
  section = page.locator("section", { has: page.getByRole("heading", { name: "Autopilot’s wording" }) });
  [prompt, done] = [section.getByRole("textbox").nth(0), section.getByRole("textbox").nth(1)];
  await prompt.waitFor();
  ok((await prompt.inputValue()) === "Take {key} ({title}).\n\n{next}", `${tag} the project shows every project's prompt`);
  ok(!(await section.getByRole("button", { name: "Use every project’s" }).isVisible()), `${tag} nothing of its own to clear yet`);
  await done.fill("{key} is finished here. {status}");
  await section.getByText("Saved", { exact: true }).waitFor();
  let own = await api("GET", `/projects/${KEY}/autopilot-text`);
  ok(own.custom_done && !own.custom_prompt && own.done === "{key} is finished here. {status}" && own.inherited_done === "{key} done. {status}", `${tag} the project keeps its own note only: ${JSON.stringify(own)}`);
  ok(!(await page.getByText("You have unsaved changes").isVisible()), `${tag} saving the wording leaves the rest of the settings alone`);
  await noSideways("project settings");
  await section.scrollIntoViewIfNeeded();
  await section.screenshot({ path: `${out}/project-${width}-${scheme}.png` });
  await section.getByRole("button", { name: "Use every project’s" }).click();
  await page.waitForFunction(
    async ([b, k]) => !(await (await fetch(`${b}/api/v1/projects/${k}/autopilot-text`, { headers: { "X-Waypoint-Client": "web" } })).json()).custom_done,
    [base, KEY],
  );
  own = await api("GET", `/projects/${KEY}/autopilot-text`);
  ok(own.done === "{key} done. {status}", `${tag} cleared, the project falls back to every project's note`);

  await api("PUT", "/admin/autopilot-text", { prompt: "", done: "" });
  ok(!(await api("GET", "/autopilot/text")).custom_prompt, `${tag} back to the default`);
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
