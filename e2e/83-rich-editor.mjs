// The rich description editor (WP-628): opening and closing it untouched
// leaves the text byte for byte as it was; the toolbar and Shift+Enter write
// markdown; text it can't keep (raw HTML) opens as markdown with a note; the
// toggle switches editors and keeps the text.
// Run after 01-signup-flow (user robin).
import { launchEngine } from "./engine.mjs";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await launchEngine();

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
  // The description is on the Details tab, which a fresh browser may not show.
  const details = () =>
    page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
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
  // Polls the API until the description satisfies `want` (a string or a
  // predicate); returns the last value either way.
  const waitDesc = async (key, want, ms = 10000) => {
    const hit = typeof want === "function" ? want : (d) => d === want;
    const end = Date.now() + ms;
    let d = await desc(key);
    while (!hit(d) && Date.now() < end) {
      // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
      await page.waitForTimeout(250);
      d = await desc(key);
    }
    return d;
  };
  const field = page.getByRole("textbox", { name: "Description" });
  const richBox = page.locator('[contenteditable="true"][aria-label="Description"]');
  const mdBox = page.locator("textarea[aria-label=Description]");
  const note = page.getByText("Opened as markdown: this text has formatting");
  const settleClose = async () => {
    if ((await field.count()) > 0) await field.press("Escape");
    await page.waitForFunction(
      () => !document.querySelector('[aria-label="Description"]'),
    );
  };

  // An untouched open and close changes nothing.
  const a = await make("Mixed", MIXED);
  await page.goto(`${base}/issue/${a.key}`);
  await details();
  await page.getByText("Intro with snake_case").click();
  await richBox.waitFor();
  ok((await note.count()) === 0, `${tag} mixed markdown opens in the rich editor`);
  await page.screenshot({ path: `${out}/rich-open-${tag}.png` });
  // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
  await page.waitForTimeout(2200); // longer than the autosave pause: it would have gone out
  await settleClose();
  ok((await desc(a.key)) === MIXED, `${tag} an untouched open and close keeps the text byte for byte`);

  // Bold from the toolbar; Shift+Enter is one newline, Enter a new paragraph.
  const b = await make("Bold", "alpha beta_gamma");
  await page.goto(`${base}/issue/${b.key}`);
  await details();
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
  const bd = await waitDesc(b.key, "**alpha beta_gamma word\nnext**\n\n**para**");
  ok(bd === "**alpha beta_gamma word\nnext**\n\n**para**", `${tag} toolbar bold and breaks save markdown: ${JSON.stringify(bd)}`);
  await settleClose();

  const c = await make("Word", "plain text");
  await page.goto(`${base}/issue/${c.key}`);
  await details();
  await page.getByText("plain text").click();
  await richBox.waitFor();
  await field.press("Control+End");
  await field.pressSequentially(" word");
  await field.press("Shift+Control+ArrowLeft");
  await page.getByRole("button", { name: "Bold" }).click();
  const cd = await waitDesc(c.key, "plain text **word**");
  ok(cd === "plain text **word**", `${tag} bold on a typed word saves **word**: ${JSON.stringify(cd)}`);

  // Task boxes toggle and save.
  await settleClose();
  const t = await make("Tasks", "- [ ] first\n- [x] second");
  await page.goto(`${base}/issue/${t.key}`);
  await details();
  await page.getByText("first").click({ position: { x: 40, y: 5 } });
  await richBox.waitFor();
  await richBox.locator("input[type=checkbox]").first().click();
  const td = await waitDesc(t.key, "- [x] first\n- [x] second");
  ok(td === "- [x] first\n- [x] second", `${tag} a task box toggles in the editor: ${JSON.stringify(td)}`);
  await settleClose();

  // Text the editor can't keep opens as markdown.
  const h = await make("Html", HTML);
  await page.goto(`${base}/issue/${h.key}`);
  await details();
  await page.locator("summary").click();
  await mdBox.waitFor();
  ok((await note.count()) === 1, `${tag} raw HTML opens as markdown with a note`);
  ok((await mdBox.inputValue()) === HTML, `${tag} the markdown editor holds the text as stored`);
  await page.screenshot({ path: `${out}/html-note-${tag}.png` });
  await page.getByRole("button", { name: "Edit formatted" }).click();
  await mdBox.waitFor();
  ok((await note.count()) === 1, `${tag} switching back refuses while the text can't be kept`);
  ok(
    (await page.evaluate(() => localStorage.getItem("wp.editor.mode"))) !== "rich",
    `${tag} a refused switch does not change the remembered editor`,
  );
  await settleClose();
  ok((await desc(h.key)) === HTML, `${tag} nothing changed`);

  // The toggle switches editors, keeps the draft and the choice.
  const m = await make("Toggle", "**bold** text");
  await page.goto(`${base}/issue/${m.key}`);
  await details();
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
  const md = await waitDesc(m.key, "**bold** text more");
  ok(md === "**bold** text more", `${tag} the draft saved: ${JSON.stringify(md)}`);
  await settleClose();
  const m2 = await make("Remembered", "plain");
  await page.goto(`${base}/issue/${m2.key}`);
  await details();
  await page.getByText("plain", { exact: true }).click();
  await mdBox.waitFor();
  ok((await note.count()) === 0, `${tag} the chosen editor is remembered, without a note`);
  await page.getByRole("button", { name: "Edit formatted" }).click();
  await richBox.waitFor();


  // ---- @mentions in the rich editor ----
  const menu = page.getByRole("listbox", { name: "Mention someone" });
  const openRich = async (key, text) => {
    await page.goto(`${base}/issue/${key}`);
    await details();
    await page.getByText(text, { exact: true }).click();
    await richBox.waitFor();
    await field.press("Control+End");
  };
  const mn = await make("Mention", "hello");
  await openRich(mn.key, "hello");
  await field.pressSequentially(" @rob");
  await menu.waitFor();
  ok(await menu.isVisible(), `${tag} typing @rob opens the mention menu`);
  await field.press("Enter");
  const mnd = await waitDesc(mn.key, (d) => d.includes("@robin"));
  ok(/^hello @robin\s*$/.test(mnd), `${tag} Enter picks the user: ${JSON.stringify(mnd)}`);
  await settleClose();

  const mc = await make("Mention click", "hello");
  await openRich(mc.key, "hello");
  await field.pressSequentially(" @rob");
  await menu.waitFor();
  await field.press("Escape");
  ok(!(await menu.isVisible()), `${tag} Escape closes the menu, not the editor`);
  const bb = await richBox.boundingBox();
  await richBox.click({ position: { x: bb.width - 12, y: 14 } });
  await menu.waitFor({ timeout: 3000 }).catch(() => {});
  ok(await menu.isVisible(), `${tag} clicking the caret into @rob reopens the menu`);
  await field.press("Escape");
  await settleClose();

  const ms = await make("Mention shift", "hello");
  await openRich(ms.key, "hello");
  await field.pressSequentially(" @rob");
  await menu.waitFor();
  await field.press("Shift+Enter");
  await field.pressSequentially("z");
  const msd = await waitDesc(ms.key, (d) => d.includes("z"));
  ok(msd === "hello @rob\nz", `${tag} Shift+Enter breaks the line instead of picking: ${JSON.stringify(msd)}`);
  await settleClose();

  const mo = await make("Mention dot", "hello");
  await openRich(mo.key, "hello");
  await field.pressSequentially(" @.");
  // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
  await page.waitForTimeout(300);
  ok(!(await menu.isVisible()), `${tag} "@." opens no menu`);
  await settleClose();

  // ---- uploads in the rich editor ----
  const PNG =
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==";
  const pastePng = (name) =>
    richBox.evaluate(
      (el, [b64, n]) => {
        const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
        const dt = new DataTransfer();
        dt.items.add(new File([bytes], n, { type: "image/png" }));
        el.dispatchEvent(
          new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }),
        );
      },
      [PNG, name],
    );
  const slow = async (ms) =>
    page.route(/\/api\/v1\/attachments\?/, async (route) => {
      await new Promise((r) => setTimeout(r, ms));
      await route.continue();
    });
  const link = (n) => new RegExp(`!\\[${n}\\]\\(/api/v1/attachments/\\d+/${n}\\)`);

  const up = await make("Upload", "pic");
  await openRich(up.key, "pic");
  await slow(500);
  await pastePng("shot.png");
  ok((await richBox.textContent()).includes("Uploading shot.png"), `${tag} a pasted file shows a placeholder`);
  const upd = await waitDesc(up.key, (d) => link("shot.png").test(d));
  ok(link("shot.png").test(upd) && !upd.includes("Uploading"), `${tag} a pasted file ends as an attachment link: ${JSON.stringify(upd)}`);
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await settleClose();

  // The editor goes away while the file is still uploading.
  const us = await make("Upload switch", "pic");
  await openRich(us.key, "pic");
  await slow(1500);
  await pastePng("late.png");
  await page.getByRole("button", { name: "Edit as markdown" }).click();
  await mdBox.waitFor();
  await page.waitForFunction(
    () => /late\.png\]\(\/api\/v1\/attachments\/\d+\//.test(document.querySelector("textarea[aria-label=Description]")?.value ?? ""),
    null,
    { timeout: 10000 },
  ).catch(() => {});
  const mv = await mdBox.inputValue();
  ok(link("late.png").test(mv) && !mv.includes("Uploading"), `${tag} an upload finishing after the switch to markdown lands in the text: ${JSON.stringify(mv)}`);
  const usd = await waitDesc(us.key, (d) => link("late.png").test(d));
  ok(link("late.png").test(usd), `${tag} ...and is saved: ${JSON.stringify(usd)}`);
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await settleClose();
  // Back to the rich editor for what follows.
  await page.evaluate(() => localStorage.setItem("wp.editor.mode", "rich"));

  // Done/Escape pressed while the file is still uploading: the editor is gone
  // when it ends, and the link (or the failed upload's cleanup) must still be
  // saved rather than left as the placeholder (WP-628).
  const ue = await make("Upload escape", "pic");
  await openRich(ue.key, "pic");
  await slow(1500);
  await pastePng("esc.png");
  await field.press("Escape");
  const ued = await waitDesc(ue.key, (d) => link("esc.png").test(d));
  ok(link("esc.png").test(ued) && !ued.includes("Uploading"), `${tag} an upload finishing after Escape is saved: ${JSON.stringify(ued)}`);
  await page.reload();
  await page.getByRole("img").first().waitFor({ timeout: 8000 }).catch(() => {});
  ok(
    (await page.locator('img[src*="esc.png"]').count()) > 0 && !(await page.content()).includes("Uploading esc.png"),
    `${tag} ...and shows after a reload`,
  );
  await page.unrouteAll({ behavior: "ignoreErrors" });

  // The same upload failing after Escape: the placeholder goes, and is saved.
  const uf = await make("Upload escape fail", "pic");
  await openRich(uf.key, "pic");
  await page.route(/\/api\/v1\/attachments\?/, async (route) => {
    await new Promise((r) => setTimeout(r, 1500));
    await route.fulfill({ status: 500, contentType: "application/json", body: '{"error":"nope"}' });
  });
  await pastePng("bad.png");
  await field.press("Escape");
  // Escape saves the placeholder first; the failure then has to take it out.
  await waitDesc(uf.key, (d) => d.includes("Uploading bad.png"));
  const ufd = await waitDesc(uf.key, (d) => !d.includes("Uploading"));
  ok(!ufd.includes("Uploading") && !ufd.includes("bad.png"), `${tag} an upload failing after Escape leaves no placeholder saved: ${JSON.stringify(ufd)}`);
  await page.unrouteAll({ behavior: "ignoreErrors" });

  // The placeholder deleted mid-upload: the file is attached, say so.
  const ud = await make("Upload deleted", "pic");
  await openRich(ud.key, "pic");
  await slow(1200);
  await pastePng("gone.png");
  await field.press("Control+a");
  await field.press("Delete");
  await page.getByText(/attached but not inserted/).waitFor({ timeout: 8000 }).catch(() => {});
  ok(
    (await page.getByText(/attached but not inserted/).count()) > 0,
    `${tag} a placeholder deleted mid-upload says the file was attached but not inserted`,
  );
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await settleClose();

  // The placeholder's text node remade mid-upload (the browser can swap it
  // for an equal one): the link goes into the draft and must show in the
  // editor too, or the next keystroke writes the editor's old text over it.
  const ur = await make("Upload remade", "pic");
  await openRich(ur.key, "pic");
  await slow(1200);
  await pastePng("remade.png");
  await richBox.evaluate((el) => {
    const w = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    for (let t = w.nextNode(); t; t = w.nextNode())
      if (t.data.includes("Uploading remade.png")) {
        t.replaceWith(document.createTextNode(t.data));
        break;
      }
  });
  await page
    .waitForFunction(() => !!document.querySelector('[aria-label="Description"] img[src*="remade.png"]'), null, { timeout: 10000 })
    .catch(() => {});
  ok(await richBox.evaluate((el) => !!el.querySelector('img[src*="remade.png"]')), `${tag} an upload whose placeholder was remade shows in the editor`);
  await field.pressSequentially("!");
  const urd = await waitDesc(ur.key, (d) => link("remade.png").test(d) && d.includes("!"));
  ok(link("remade.png").test(urd) && !urd.includes("Uploading"), `${tag} ...and keeps its link after the next keystroke: ${JSON.stringify(urd)}`);
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await settleClose();

  // ---- toolbar buttons, empty and not ----
  const tools = [
    ["Italic", /^[*_]TXT[*_]$/],
    ["Heading 1", /^# TXT$/],
    ["Quote", /^> TXT$/],
    ["Bulleted list", /^[-*] TXT$/],
    ["Numbered list", /^1\. TXT$/],
    ["Code block", /^```\nTXT\n```$/],
  ];
  for (const [name, re] of tools) {
    const want = (d) => new RegExp(re.source.replace("TXT", "hi")).test(d ?? "");
    const ei = await make(`Empty ${name}`, undefined);
    await page.goto(`${base}/issue/${ei.key}`);
    await details();
    await page.getByRole("button", { name: "Add a description…", exact: true }).click();
    await richBox.waitFor();
    await page.getByRole("button", { name, exact: true }).click();
    await field.pressSequentially("hi");
    const got = await waitDesc(ei.key, want, 6000);
    ok(want(got), `${tag} ${name} in an empty editor: ${JSON.stringify(got)}`);
    await settleClose();

    const wantFull = (d) => new RegExp(re.source.replace("TXT", "plain")).test(d ?? "");
    const ni = await make(`Full ${name}`, "plain");
    await openRich(ni.key, "plain");
    await field.press("Control+a");
    await page.getByRole("button", { name, exact: true }).click();
    const got2 = await waitDesc(ni.key, wantFull, 6000);
    ok(wantFull(got2), `${tag} ${name} on existing text: ${JSON.stringify(got2)}`);
    await settleClose();
  }

  // Underline has no markdown: the shortcut does nothing.
  const un = await make("Underline", "plain");
  await openRich(un.key, "plain");
  await field.press("Control+a");
  await field.press("Control+u");
  ok(!(await richBox.evaluate((el) => !!el.querySelector("u"))), `${tag} Ctrl+U adds no underline`);
  await settleClose();

  // Text dragged in from outside is plain text, like a paste.
  const dr = await make("Drop", "plain");
  await openRich(dr.key, "plain");
  await richBox.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const dt = new DataTransfer();
    dt.setData("text/plain", "dropped words");
    dt.setData("text/html", "<h1 style='color:red'>dropped words</h1>");
    el.dispatchEvent(
      new DragEvent("drop", { dataTransfer: dt, clientX: r.right - 8, clientY: r.top + 12, bubbles: true, cancelable: true }),
    );
  });
  ok(!(await richBox.evaluate((el) => !!el.querySelector("h1"))), `${tag} dropped HTML is not inserted as HTML`);
  const drd = await waitDesc(dr.key, (d) => d.includes("dropped words"));
  ok(/^plain\s*dropped words$/.test(drd), `${tag} dropped text is inserted as text: ${JSON.stringify(drd)}`);
  await settleClose();

  // An empty description shows the prompt and nothing overflows.
  await settleClose();
  const e = await make("Empty", undefined);
  await page.goto(`${base}/issue/${e.key}`);
  await details();
  const emptyBtn = page.getByRole("button", { name: "Add a description…", exact: true });
  await emptyBtn.waitFor();
  ok(
    (await page.getByRole("button", { name: /^Add a description/ }).count()) === 1 &&
      (await emptyBtn.count()) === 1,
    `${tag} the empty-state button is just the short label`,
  );
  await page.getByRole("button", { name: "Add a description…", exact: true }).click();
  await richBox.waitFor();
  // An empty editor holds one empty paragraph, and the hint is that paragraph's ::before.
  const prompt = await richBox.evaluate((el) => getComputedStyle(el.firstElementChild, "::before").content);
  ok(prompt.length > 2, `${tag} an empty editor shows its prompt: ${prompt}`);
  ok(
    !prompt.replace(/^["']/, "").startsWith("Add a description"),
    `${tag} the in-field hint does not depend on the empty-state label: ${prompt}`,
  );
  ok(
    prompt.includes("@") && prompt.includes("paste or drop files"),
    `${tag} the in-field hint mentions @ and files: ${prompt}`,
  );
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
