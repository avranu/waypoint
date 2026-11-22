// The rich description editor (WP-628): opening and closing it untouched
// leaves the text byte for byte as it was; the toolbar and Shift+Enter write
// markdown; text it can't keep (raw HTML) opens as markdown with a note; the
// toggle switches editors and keeps the text.
// Run after 01-signup-flow (user robin).
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

const MIXED = [
  "## Plan",
  "",
  "Intro with snake_case, file_name.txt, WP-12, @robin and [[Some Page]]; C:\\temp\\x and ~/bin.",
  "",
  "- one",
  "- two",
  "",
  "Tasks:",
  "",
  "- [ ] todo",
  "- [x] done",
  "",
  "```js",
  "const a_b = 1 * 2",
  "```",
  "",
  "See [the site](https://example.com) for more.",
].join("\n");
const HTML = "<details><summary>x</summary>y</details>";

{
  const ctx = await browser.newContext();
  const page = await login(ctx);
  const projects = await (
    await page.request.get(base + "/api/v1/projects", { headers: H })
  ).json();
  if (!projects.some((p) => p.key === "RICH"))
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Rich editor", key: "RICH" },
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
  const make = async (title, description) =>
    (
      await page.request.post(base + "/api/v1/issues", {
        headers: H,
        data: { project: "RICH", title: `${title} ${tag}`, description },
      })
    ).json();
  const desc = async (key) =>
    (await (await page.request.get(base + `/api/v1/issues/${key}`, { headers: H })).json())
      .description;
  const field = page.getByRole("textbox", { name: "Description" });
  const richBox = page.locator('[contenteditable="true"][aria-label="Description"]');
  const mdBox = page.locator("textarea[aria-label=Description]");
  const note = page.getByText("Opened as markdown: this text has formatting");
  const settleClose = async () => {
    await field.press("Escape");
    await page.waitForFunction(
      () => !document.querySelector('[aria-label="Description"]'),
    );
  };

  // An untouched open and close changes nothing.
  const a = await make("Mixed", MIXED);
  await page.goto(`${base}/issue/${a.key}`);
  await page.getByText("Intro with snake_case").click();
  await richBox.waitFor();
  ok((await note.count()) === 0, `${tag} mixed markdown opens in the rich editor`);
  await page.screenshot({ path: `${out}/rich-open-${tag}.png` });
  await page.waitForTimeout(2800); // an autosave would have gone out
  await settleClose();
  ok((await desc(a.key)) === MIXED, `${tag} an untouched open and close keeps the text byte for byte`);

  // Bold from the toolbar; Shift+Enter is one newline, Enter a new paragraph.
  const b = await make("Bold", "alpha beta_gamma");
  await page.goto(`${base}/issue/${b.key}`);
  await page.getByText("alpha beta_gamma").click();
  await richBox.waitFor();
  await field.press("Control+End");
  await field.pressSequentially(" word");
  await field.press("Shift+Enter");
  await field.pressSequentially("next");
  await field.press("Enter");
  await field.pressSequentially("para");
  await field.press("Control+a");
  await page.getByRole("button", { name: "Bold" }).click();
  await page.waitForTimeout(2800);
  ok((await desc(b.key)) === "**alpha beta_gamma word\nnext**\n\n**para**", `${tag} toolbar bold and breaks save markdown: ${JSON.stringify(await desc(b.key))}`);
  await settleClose();

  const c = await make("Word", "plain text");
  await page.goto(`${base}/issue/${c.key}`);
  await page.getByText("plain text").click();
  await richBox.waitFor();
  await field.press("Control+End");
  await field.pressSequentially(" word");
  await field.press("Shift+Control+ArrowLeft");
  await page.getByRole("button", { name: "Bold" }).click();
  await page.waitForTimeout(2800);
  ok((await desc(c.key)) === "plain text **word**", `${tag} bold on a typed word saves **word**: ${JSON.stringify(await desc(c.key))}`);

  // Task boxes toggle and save.
  await settleClose();
  const t = await make("Tasks", "- [ ] first\n- [x] second");
  await page.goto(`${base}/issue/${t.key}`);
  await page.getByText("first").click({ position: { x: 40, y: 5 } });
  await richBox.waitFor();
  await richBox.locator("input[type=checkbox]").first().click();
  await page.waitForTimeout(2800);
  ok((await desc(t.key)) === "- [x] first\n- [x] second", `${tag} a task box toggles in the editor: ${JSON.stringify(await desc(t.key))}`);
  await settleClose();

  // Text the editor can't keep opens as markdown.
  const h = await make("Html", HTML);
  await page.goto(`${base}/issue/${h.key}`);
  await page.locator("summary").click();
  await mdBox.waitFor();
  ok((await note.count()) === 1, `${tag} raw HTML opens as markdown with a note`);
  ok((await mdBox.inputValue()) === HTML, `${tag} the markdown editor holds the text as stored`);
  await page.screenshot({ path: `${out}/html-note-${tag}.png` });
  await page.getByRole("button", { name: "Edit formatted" }).click();
  await mdBox.waitFor();
  ok((await note.count()) === 1, `${tag} switching back refuses while the text can't be kept`);
  await settleClose();
  ok((await desc(h.key)) === HTML, `${tag} nothing changed`);

  // The toggle switches editors, keeps the draft and the choice.
  const m = await make("Toggle", "**bold** text");
  await page.goto(`${base}/issue/${m.key}`);
  await page.locator(".prose-wp strong").click();
  await richBox.waitFor();
  await page.getByRole("button", { name: "Edit as markdown" }).click();
  await mdBox.waitFor();
  ok((await mdBox.inputValue()) === "**bold** text", `${tag} markdown mode shows the source`);
  await mdBox.press("Control+End");
  await mdBox.pressSequentially(" more");
  await page.getByRole("button", { name: "Edit formatted" }).click();
  await richBox.waitFor();
  ok(
    (await richBox.evaluate((el) => el.innerHTML)).includes("<strong>bold</strong>") &&
      (await richBox.textContent()).includes("text more"),
    `${tag} switching back keeps the draft`,
  );
  await page.getByRole("button", { name: "Edit as markdown" }).click();
  await mdBox.waitFor();
  await page.waitForTimeout(2800);
  ok((await desc(m.key)) === "**bold** text more", `${tag} the draft saved: ${JSON.stringify(await desc(m.key))}`);
  await settleClose();
  const m2 = await make("Remembered", "plain");
  await page.goto(`${base}/issue/${m2.key}`);
  await page.getByText("plain", { exact: true }).click();
  await mdBox.waitFor();
  ok((await note.count()) === 0, `${tag} the chosen editor is remembered, without a note`);
  await page.getByRole("button", { name: "Edit formatted" }).click();
  await richBox.waitFor();

  // An empty description shows the prompt and nothing overflows.
  await settleClose();
  const e = await make("Empty", undefined);
  await page.goto(`${base}/issue/${e.key}`);
  await page.getByRole("button", { name: "Add a description…" }).click();
  await richBox.waitFor();
  const prompt = await richBox.evaluate((el) => getComputedStyle(el, "::before").content);
  ok(prompt.includes("Add a description"), `${tag} an empty editor shows its prompt: ${prompt}`);
  await page.screenshot({ path: `${out}/empty-${tag}.png` });
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
  ok(!overflow, `${tag} no horizontal scroll`);
  await ctx.close();
}

if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
await browser.close();
