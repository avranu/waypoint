// Files on anything and Copy buttons on code (WP-D16), in a real browser at
// desktop and phone widths, light and dark. Code blocks in rendered Markdown
// have a Copy button that copies the code, says so, and never opens the
// editor (mouse, Enter or Space). Pages and design notes have an Attachments
// panel: thumbnails open a lightbox, text previews in a dialog (Markdown is
// rendered), other files download, video plays in place, captions edit,
// files delete; files pasted into the page editor upload and embed.
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
const KEY = "FILES";
const CODE = "const answer = 42\nconsole.log(answer)";
// 1x1 and 2x2 PNGs.
const PNG1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
  "base64",
);
const PNG2 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFElEQVR42mNk+M/wn4EIwDiqEAAMUgIBd8zrSQAAAABJRU5ErkJggg==",
  "base64",
);

const browser = await chromium.launch();

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const phone = width < 600;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
    hasTouch: phone,
    isMobile: phone,
  });
  await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: base });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, extra = {}) =>
    page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d, ...extra });
  const api = (m, p, d) => call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const upload = (target, name, buffer, mimeType, caption) =>
    call("POST", `/attachments?target=${encodeURIComponent(target)}`, undefined, {
      multipart: { file: { name, mimeType, buffer }, ...(caption ? { caption } : {}) },
    }).then(async (r) => {
      if (!r.ok()) throw new Error(`upload ${name}: ${r.status()} ${await r.text()}`);
      return r.json();
    });
  const shot = async (name) => {
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: false });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)),
      `${tag} no sideways scroll (${where})`,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const projects = await api("GET", "/projects");
  if (!projects.some((p) => p.key === KEY)) {
    await api("POST", "/projects", { name: "Files", key: KEY });
    await api("POST", "/issues", {
      project: KEY,
      title: "Code with a copy button",
      description: `Run this:\n\n\`\`\`js\n${CODE}\n\`\`\`\n\nThen inline \`code\` stays plain. Click this text to edit.`,
    });
    await api("POST", "/pages", {
      title: "Layout plan",
      category: "plan",
      project: KEY,
      body: "The plan.",
    });
    await api("POST", "/design", { title: "Board screen", kind: "screen", body: "The board.", project: KEY });
  }
  const pages = await api("GET", `/pages?project=${KEY}`);
  const planPage = pages.find((p) => p.title === "Layout plan");
  const notes = await api("GET", `/design?project=${KEY}`);
  const note = notes.find((n) => n.title === "Board screen");

  await page.goto(`${base}/issue/${KEY}-1`);
  const desc = page.locator(".prose-wp").first();
  await desc.getByText("Then inline").waitFor();
  const editing = () => page.getByRole("button", { name: /^(Save|Done)$/ }).count();
  const copy = desc.getByRole("button", { name: "Copy code" });
  ok((await copy.count()) === 1, `${tag} one fenced block, one Copy button (inline code has none)`);
  const opacity = () => copy.evaluate((b) => getComputedStyle(b).opacity);
  if (phone) {
    ok((await opacity()) === "1", `${tag} Copy is always visible on a touch screen`);
  } else {
    ok((await opacity()) === "0", `${tag} Copy is hidden until you point at the block`);
    await desc.locator("pre").hover();
    // On a loaded CI runner the fade-in can land after a fixed pause, so wait for the opacity instead of reading it once.
    const copyHandle = await copy.elementHandle();
    const shown = await page
      .waitForFunction((b) => getComputedStyle(b).opacity === "1", copyHandle, { timeout: 3000 })
      .then(
        () => true,
        () => false,
      );
    if (!shown) console.log(`     opacity stayed ${await opacity()}`);
    ok(shown, `${tag} Copy shows on hover`);
  }
  await shot("copy-button");
  await copy.click();
  await page.waitForFunction(
    () => document.querySelector(".code-copy")?.textContent?.includes("Copied"),
  );
  ok(true, `${tag} the button says Copied`);
  ok(
    (await page.evaluate(() => navigator.clipboard.readText())) === CODE,
    `${tag} the code, without its trailing newline, is on the clipboard`,
  );
  ok(
    (await editing()) === 0,
    `${tag} clicking Copy didn't open the editor`,
  );
  await shot("copied");
  for (const key of ["Enter", "Space"]) {
    await page.evaluate(() => navigator.clipboard.writeText("something else"));
    await copy.focus();
    ok(
      await copy.evaluate((b) => b === document.activeElement),
      `${tag} the button takes focus`,
    );
    await page.keyboard.press(key);
    await page.waitForFunction(async () => (await navigator.clipboard.readText()).includes("answer"));
    ok(
      (await editing()) === 0,
      `${tag} ${key} on Copy copies without opening the editor`,
    );
  }
  if (!phone) {
    await page.waitForTimeout(1800);
    ok(
      !(await copy.textContent())?.includes("Copied"),
      `${tag} the Copied message goes away by itself`,
    );
  }
  await desc.getByText("Click this text to edit.").click();
  await page.getByRole("button", { name: "Done", exact: true }).waitFor();
  ok(true, `${tag} clicking the text still opens the editor`);
  await page.keyboard.press("Escape");
  await overflow("code block");

  const target = `page:${planPage.id}`;
  const existing = await api("GET", `/attachments?target=${encodeURIComponent(target)}`);
  if (existing.length === 0) {
    await upload(target, "before.png", PNG1, "image/png", "Before the change");
    await upload(target, "after.png", PNG2, "image/png");
    await upload(target, "run.log", Buffer.from("started\nrow 1 ok\nfinished\n"), "text/plain");
    await upload(
      target,
      "NOTES.md",
      Buffer.from("# Findings\n\nIt **works**.\n\n```\nwp attach\n```\n"),
      "text/markdown",
    );
    await upload(target, "data.json", Buffer.from('{"rows":[1,2,3]}'), "application/json");
    await upload(target, "report.zip", Buffer.from("PK\x03\x04zipbytes"), "application/zip");
    await upload(
      target,
      "demo.webm",
      Buffer.from([0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 0x01, 0x42, 0xf7, 0x81, 0x01]),
      "video/webm",
      "A recording",
    );
    // SVG never renders from our origin.
    await upload(
      target,
      "logo.svg",
      Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>'),
      "image/svg+xml",
    );
  }
  await api("PATCH", `/pages/${planPage.id}`, { body: "The plan." });
  const files = await api("GET", `/attachments?target=${encodeURIComponent(target)}`);
  await page.goto(`${base}/pages/${planPage.id}`);
  const panel = page.locator(`[data-attachments="${target}"]`);
  await panel.waitFor();
  await page.getByText(new RegExp(`^Attachments · ${files.length}$`)).waitFor();
  ok(files.length >= 8, `${tag} the page lists its ${files.length} files`);
  ok((await panel.locator("video").count()) === 1, `${tag} the recording is a video player`);
  ok(
    (await panel.locator("video").getAttribute("src")).startsWith("/api/v1/attachments/"),
    `${tag} the player loads from the attachment URL`,
  );
  ok(
    (await panel.getByText("Before the change").count()) > 0,
    `${tag} a caption shows under its thumbnail`,
  );
  await shot("page-attachments");
  await overflow("attachments");

  await panel.getByRole("button", { name: /View Before the change/ }).click();
  const lb = page.getByRole("dialog", { name: "Before the change" });
  await lb.waitFor();
  await shot("lightbox");
  await page.keyboard.press("ArrowRight");
  await page.getByRole("dialog", { name: "after.png" }).waitFor();
  ok(true, `${tag} the lightbox steps to the next image`);
  await page.keyboard.press("Escape");
  await page.getByRole("dialog").waitFor({ state: "detached" });
  ok(true, `${tag} Escape closes the lightbox`);

  await panel.getByRole("button", { name: "run.log", exact: true }).click();
  const dlg = page.getByRole("dialog", { name: "run.log" });
  await dlg.getByText("row 1 ok").waitFor();
  ok((await dlg.locator("pre").count()) === 1, `${tag} a log previews as plain text`);
  ok(
    (await dlg.getByRole("link", { name: /Download/ }).getAttribute("href")).endsWith("?download=1"),
    `${tag} the preview offers a download`,
  );
  await page.keyboard.press("Escape");
  await panel.getByRole("button", { name: "NOTES.md", exact: true }).click();
  const md = page.getByRole("dialog", { name: "NOTES.md" });
  await md.getByRole("heading", { name: "Findings" }).waitFor();
  ok((await md.locator("strong").innerText()) === "works", `${tag} Markdown previews rendered`);
  ok(
    (await md.getByRole("button", { name: "Copy code" }).count()) === 1,
    `${tag} a code block in the preview has its Copy button`,
  );
  await shot("markdown-preview");
  await page.keyboard.press("Escape");
  await panel.getByRole("button", { name: "data.json", exact: true }).click();
  const js = page.getByRole("dialog", { name: "data.json" });
  await js.locator("pre").waitFor();
  ok((await js.locator("pre").innerText()).includes('"rows": [\n'), `${tag} JSON is pretty-printed`);
  await page.keyboard.press("Escape");
  for (const name of ["report.zip", "logo.svg"]) {
    const href = await panel.getByRole("link", { name, exact: true }).getAttribute("href");
    ok(href.endsWith("?download=1"), `${tag} ${name} is a download link`);
  }
  const svg = files.find((f) => f.name === "logo.svg");
  const svgRes = await page.request.get(base + svg.url);
  ok(
    svgRes.headers()["content-type"] === "application/octet-stream" &&
      svgRes.headers()["content-disposition"].startsWith("attachment"),
    `${tag} the SVG is served as a download, not as an image`,
  );

  const before = (await api("GET", `/attachments?target=${encodeURIComponent(target)}`)).length;
  await page.getByLabel("Choose files to attach").setInputFiles({
    name: `extra-${tag.replace("/", "-")}.txt`,
    mimeType: "text/plain",
    buffer: Buffer.from("extra file"),
  });
  await page.getByText(new RegExp(`^Attachments · ${before + 1}$`)).waitFor();
  ok(true, `${tag} Add file uploads to the page`);
  const row = panel.locator("li", { hasText: `extra-${tag.replace("/", "-")}.txt` });
  await row.getByRole("button", { name: /Edit caption/ }).dispatchEvent("click"); // not .click(): on a phone a floating button can sit over the row
  await page.getByRole("dialog", { name: "Caption" }).getByLabel("Caption").fill("Extra notes");
  await page.keyboard.press("Enter");
  await panel.getByText("Extra notes").waitFor();
  ok(true, `${tag} a caption can be added`);
  await row.getByRole("button", { name: /^Remove / }).dispatchEvent("click");
  await page.getByRole("dialog").getByRole("button", { name: "Remove", exact: true }).click();
  await page.getByText(new RegExp(`^Attachments · ${before}$`)).waitFor();
  ok(true, `${tag} a file can be removed`);

  await page.getByText("The plan.").click();
  const ta = page.locator("textarea:visible").first();
  await ta.waitFor();
  await ta.evaluate((el, b64) => {
    const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    const dt = new DataTransfer();
    dt.items.add(new File([bin], "pasted.png", { type: "image/png" }));
    el.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
  }, PNG1.toString("base64"));
  await page.waitForFunction(() =>
    /!\[pasted\.png\]\(\/api\/v1\/attachments\/\d+\/pasted\.png\)/.test(
      document.querySelector("textarea")?.value ?? "",
    ),
  );
  ok(true, `${tag} a pasted image uploads to the page and becomes Markdown`);
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page.locator('.prose-wp img[alt="pasted.png"]').first().waitFor();
  ok(true, `${tag} the saved page shows the pasted image`);
  await shot("page-pasted");

  const dTarget = `design:${note.id}`;
  await page.goto(`${base}/design/${note.id}`);
  const dPanel = page.locator(`[data-attachments="${dTarget}"]`);
  await dPanel.waitFor();
  ok(
    (await dPanel.getByRole("button", { name: "Drop files here or click to attach" }).count()) === 1 ||
      (await dPanel.locator("img").count()) > 0,
    `${tag} the design note has an Attachments panel`,
  );
  if ((await dPanel.locator("img").count()) === 0) {
    await page.getByLabel("Choose files to attach").setInputFiles({
      name: "mock.png",
      mimeType: "image/png",
      buffer: PNG2,
    });
    await dPanel.locator("img").first().waitFor();
  }
  ok(true, `${tag} a file can be attached to a design note`);
  await shot("design-attachments");
  await overflow("design note");

  const demo = files.find((f) => f.name === "demo.webm");
  await api("PATCH", `/pages/${planPage.id}`, { body: `Watch:\n\n${demo.markdown}\n` });
  await page.goto(`${base}/pages/${planPage.id}`);
  await page.locator(".prose-wp video").first().waitFor();
  ok(true, `${tag} an attached video in Markdown becomes a player`);
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [1280, "dark"],
  [390, "light"],
  [390, "dark"],
], [0, 2])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
