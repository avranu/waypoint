// Autosave (WP-81, WP-127): the issue description saves after a pause and
// when focus leaves, with no Save button; a failed save keeps the text and
// offers a retry; a refresh mid-typing brings the text back; a run of
// autosaves is one history entry; the comment box keeps its draft.
// Run after 01-signup-flow (user robin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
import { fillMarkdown, markdownOf, mdBox } from "./editor.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();

async function login(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

{
  const ctx = await browser.newContext();
  const page = await login(ctx);
  const projects = await (
    await page.request.get(base + "/api/v1/projects", { headers: H })
  ).json();
  if (!projects.some((p) => p.key === "AUTO"))
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Autosave", key: "AUTO" },
    });
  await ctx.close();
}

for (const [width, scheme] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  const tag = `w${width}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await login(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = async (path) =>
    (await page.request.get(base + "/api/v1" + path, { headers: H })).json();
  const issue = await (
    await page.request.post(base + "/api/v1/issues", {
      headers: H,
      data: { project: "AUTO", title: `Autosave ${tag}` },
    })
  ).json();
  const desc = () => api(`/issues/${issue.key}`).then((i) => i.description);
  const field = page.getByRole("textbox", { name: "Description" });
  const save = page.getByRole("button", { name: "Save", exact: true });

  await page.goto(`${base}/issue/${issue.key}`);
  await page.getByRole("button", { name: "Add a description…" }).click();
  await field.pressSequentially("First thoughts", { delay: 20 });
  ok((await save.count()) === 0, `${tag} the editor has no Save button`);
  await page.waitForTimeout(2800);
  ok((await desc()) === "First thoughts", `${tag} a pause in typing saves`);
  await page.screenshot({ path: `${out}/saved-${tag}.png` });

  await field.pressSequentially(" and more", { delay: 20 });
  await page.reload();
  await field.waitFor();
  ok(
    (await field.evaluate((el) => (el.textContent ?? "").trim())) === "First thoughts and more",
    `${tag} a refresh mid-typing brings the text back`,
  );
  await page.waitForTimeout(2800);
  ok(
    (await desc()) === "First thoughts and more",
    `${tag} the restored text autosaves`,
  );

  await field.press("Control+End");
  for (const t of [" one", " two", " three"]) {
    await field.pressSequentially(t, { delay: 10 });
    await page.waitForTimeout(2800);
  }
  const hist = await api(`/issues/${issue.key}/history`);
  const edits = (hist.events ?? hist).filter(
    (e) => e.field === "description",
  );
  ok(edits.length === 1, `${tag} autosaves coalesce into one history entry (${edits.length})`);

  await page.route("**/api/v1/issues/*", (r) =>
    r.request().method() === "PATCH" ? r.abort() : r.continue(),
  );
  await field.pressSequentially(" offline", { delay: 10 });
  const retry = page.getByRole("button", { name: "Not saved: retry" });
  await retry.waitFor({ timeout: 8000 });
  ok(
    (await field.evaluate((el) => (el.textContent ?? "").trim())).endsWith(" offline"),
    `${tag} a failed save keeps the text: ${JSON.stringify(await field.evaluate((el) => el.textContent ?? ""))}`,
  );
  await page.screenshot({ path: `${out}/failed-${tag}.png` });
  await page.unroute("**/api/v1/issues/*");
  await retry.click();
  await page.waitForTimeout(2800);
  ok((await desc()).endsWith(" offline"), `${tag} retry saves it`);

  await field.pressSequentially("!");
  await field.press("Control+Enter");
  await page.waitForFunction(
    () => !document.querySelector('[contenteditable="true"][aria-label="Description"]'),
  );
  ok((await desc()).endsWith("offline!"), `${tag} Ctrl+Enter saves now and closes`);

  const box = mdBox(page, { label: "Comment", exact: true });
  await fillMarkdown(box, `Half a thought ${tag}`);
  await page.reload();
  await box.waitFor();
  ok(
    (await markdownOf(box)) === `Half a thought ${tag}`,
    `${tag} a comment draft survives a refresh`,
  );
  await page.getByRole("button", { name: "Comment", exact: true }).click();
  await page.waitForFunction(() =>
    [...document.querySelectorAll('[aria-label=Comment]')].every((n) => (n.value ?? n.innerText).trim() === ""),
  );
  await page.reload();
  await box.waitFor();
  ok((await markdownOf(box)).trim() === "", `${tag} a posted comment leaves no draft: ${JSON.stringify(await markdownOf(box))}`);

  const body = page.locator(".rich-editor[contenteditable=true]");
  const pg = await (
    await page.request.post(base + "/api/v1/pages", {
      headers: H,
      data: { title: `Autosave page ${tag}`, body: "Start." },
    })
  ).json();
  await page.goto(`${base}/pages/${pg.id}`);
  await page.locator(".prose-wp").first().click({ position: { x: 5, y: 5 } });
  await body.press("Control+End");
  await body.pressSequentially(" More.", { delay: 10 });
  await page.waitForTimeout(2800);
  const got = await api(`/pages/${pg.id}`);
  ok(got.body === "Start. More.", `${tag} a page body autosaves: ${JSON.stringify(got.body)}`);

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth > innerWidth,
  );
  ok(!overflow, `${tag} no horizontal scroll`);
  await ctx.close();
}

if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
await browser.close();
