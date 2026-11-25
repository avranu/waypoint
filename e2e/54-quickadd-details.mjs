// Quick add opens the full new-issue dialog (WP-125): the button and Ctrl+Enter
// carry the typed text over (long text split into title + description), and
// the description editor says when an edit isn't saved yet (WP-127).
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
  if (!projects.some((p) => p.key === "QAD"))
    await page.request.post(base + "/api/v1/projects", {
      headers: H,
      data: { name: "Quick details", key: "QAD" },
    });
  await ctx.close();
}

const long =
  "Make the importer cope with a stale head. " +
  "The webhook arrives before the push is visible, so the review runs on the old commit. ".repeat(
    8,
  );

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
  const dialog = page.getByRole("dialog");

  await page.goto(`${base}/p/QAD/backlog`);
  const box = page.getByLabel("Add to backlog");
  await box.fill(`Details from the button ${tag} !high ~3`);
  await page.getByRole("button", { name: /^Add details/ }).click();
  await dialog.waitFor();
  const title = dialog.getByLabel("Issue title");
  ok(
    (await title.inputValue()).startsWith(`Details from the button ${tag}`),
    `${tag} button opens the dialog with the typed title`,
  );
  ok(
    (await box.inputValue()) === "",
    `${tag} the box is empty once handed over`,
  );
  await page.screenshot({ path: `${out}/dialog-${tag}.png` });
  await dialog.getByRole("button", { name: "Create issue" }).click();
  await dialog.waitFor({ state: "detached" });
  const list = await (
    await page.request.get(
      base + "/api/v1/issues?project=QAD&q=button+" + tag,
      {
        headers: H,
      },
    )
  ).json();
  const made = (list.issues ?? list).find?.((i) =>
    i.title.startsWith(`Details from the button ${tag}`),
  );
  ok(made && made.points === 3, `${tag} tokens carried over (points)`);

  await box.fill(long);
  await box.press("Control+Enter");
  await dialog.waitFor();
  const t = await title.inputValue();
  const d = await markdownOf(mdBox(dialog, { placeholder: /Add a description/ }));
  ok(
    t.length < 200 && t.startsWith("Make the importer"),
    `${tag} long text gives a short title`,
  );
  ok(d.includes("webhook arrives"), `${tag} long text goes in the description`);
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });

  await page.goto(`${base}/p/QAD/board`);
  await page.getByRole("button", { name: "Add to To Do", exact: true }).click();
  const col = page.getByLabel("New issue title");
  await col.fill(`From the board ${tag}`);
  await col.press("Control+Enter");
  await dialog.waitFor();
  ok(
    (await title.inputValue()) === `From the board ${tag}`,
    `${tag} board column opens details`,
  );
  ok(
    await dialog.getByText("To Do", { exact: true }).first().isVisible(),
    `${tag} board column's status carried over`,
  );
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });
  await page.goto(base + "/");
  await page.getByLabel("Capture an issue").fill(`From home ${tag}`);
  await page.getByRole("button", { name: /^Add details/ }).click();
  await dialog.waitFor();
  ok(
    (await title.inputValue()) === `From home ${tag}`,
    `${tag} home capture opens details`,
  );
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });
  await ctx.close();
}

if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
await browser.close();
