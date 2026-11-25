// The rich editor's markdown writer (WP-628): what is typed as literal text
// is escaped so it renders as typed and nothing else is; text it can't keep
// (raw HTML, comments) opens as markdown; lists, code fences and emphasis
// are written in a form that renders back the same.
// Run after 01-signup-flow (user robin).
import { launchEngine } from "./engine.mjs";
import fs from "node:fs";
import { looks } from "./looks.mjs";
import { formattedSwitch, pasteText, shape, toolbarOf, caretToStart, navigationAbort } from "./editor.mjs";
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

for (const [width, scheme] of looks([[1280, "light"]])) {
  const tag = `w${width}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await login(ctx);
  page.on("pageerror", (e) => navigationAbort(e.message) || fail(`${tag} pageerror: ${e.message}`));
  // The description is on the Details tab, which a fresh browser may not show.
  const details = () =>
    page.getByRole("tab", { name: "Details" }).click({ timeout: 15000 }).catch(() => {});
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
  const saved = async (key, want) => {
    let got;
    for (let i = 0; i < 40; i++) {
      got = await desc(key);
      if (got === want) return got;
      // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
      await page.waitForTimeout(250);
    }
    return got;
  };
  const field = page.getByRole("textbox", { name: "Description" });
  const richBox = page.locator('[contenteditable="true"][aria-label="Description"]');
  const mdBox = page.locator("textarea[aria-label=Description]");
  const note = page.getByText("Opened as markdown: this text has formatting");
  const settleClose = async () => {
    await field.press("Escape");
    await page.waitForFunction(() => !document.querySelector('[aria-label="Description"]'));
  };
  const open = async (title, description) => {
    const issue = await make(title, description);
    await page.goto(`${base}/issue/${issue.key}`);
    await details();
    if (description === undefined) {
      const add = page.getByRole("button", { name: "Add a description…" });
      await add.waitFor().catch(async (e) => {
        await page.screenshot({ path: `${out}/open-${issue.key}.png` });
        console.log("page text:", (await page.locator("body").innerText()).slice(0, 600));
        throw e;
      });
      await add.click();
    } else {
      await page.locator('.prose-wp[title^="Click or press"]').first().focus();
      await page.keyboard.press("Enter");
    }
    await field.waitFor();
    return issue;
  };
  const insertInto = async (title, text, want) => {
    const issue = await open(title);
    await richBox.waitFor();
    // Pasted with Shift+Ctrl+V, which puts text in as the characters it is: this
    // suite is about how literal text is written, not about what typing or
    // pasting a marker does (suites 85 and below).
    await pasteText(richBox, text, { literal: true });
    const got = await saved(issue.key, want);
    ok(got === want, `${tag} inserting ${JSON.stringify(text)} saves ${JSON.stringify(want)}: ${JSON.stringify(got)}`);
    await settleClose();
  };

  await insertInto("Esc star", "*emph* and a*b", "\\*emph\\* and a\\*b");
  await insertInto("Esc tick", "a `code` b", "a \\`code\\` b");
  await insertInto("Esc under", "snake_case and _x_", "snake_case and \\_x\\_");
  await insertInto("Esc tilde", "~/bin and ~x~", "~/bin and \\~x\\~");
  await insertInto("Esc tag", "<div> and 1 < 2", "\\<div> and 1 < 2");
  await insertInto("Esc link", "[x](y) and [a][b]", "[x\\](y) and [a\\][b]");
  await insertInto("Esc image", "![a](b)", "![a\\](b)");
  await insertInto("Esc entity", "&amp; and R&D and &#65;", "\\&amp; and R&D and \\&#65;");
  await insertInto("Esc wiki", "see [[Some Page]] and [[A|b]]", "see [[Some Page]] and [[A|b]]");
  await insertInto("Esc heading", "# h", "\\# h");
  await insertInto("Esc heading root", "# not a heading", "\\# not a heading");
  await insertInto("Esc number", "1. n", "1\\. n");
  await insertInto("Esc number paren", "2) n", "2\\) n");
  await insertInto("Esc quote", "> q", "\\> q");
  await insertInto("Esc dash", "- d", "\\- d");
  await insertInto("Esc bullet star", "* d", "\\* d");
  await insertInto("Esc spaced star", "2 * 3 * 4", "2 * 3 * 4");
  await insertInto("Esc line star", "a" + "\n".repeat(0) + " * b", "a * b");

  const writes = async (title, html, want) => {
    const issue = await open(title);
    await richBox.waitFor();
    // Pasted as HTML, the way a web page or document is copied in.
    await pasteText(richBox, null, { html });
    const got = await saved(issue.key, want);
    ok(got === want, `${tag} ${title} writes ${JSON.stringify(want)}: ${JSON.stringify(got)}`);
    await settleClose();
  };
  await writes("Label brackets", '<p><a href="https://example.com/">a]b \\] [c</a></p>', "[a\\]b \\\\\\] \\[c](https://example.com/)");
  await writes("Alt text", '<p><img src="/i.png" alt="a*b [c]"></p>', "![a\\*b \\[c\\]](/i.png)");
  await writes("Def and wiki", "<p>[a]: see [[Page]]</p>", "\\[a]: see [[Page]]");
  await writes("Rule under text", "text<hr>", "text\n\n---");
  await writes("Text under paragraph", "<p>a</p>b", "a\n\nb");
  await writes(
    "Table cell",
    "<table><tbody><tr><th>h</th></tr><tr><td>a|b \\| <code>c|d</code><br>e</td></tr></tbody></table>",
    "| h |\n| --- |\n| a\\|b \\\\\\| `c\\|d` e |",
  );
  // An empty link (no text) is dropped by the editor: there is nothing to see or click.
  await writes(
    "Table cell targets",
    '<table><tbody><tr><th>h</th></tr><tr><td><a href="/x?a|b">l</a> <img src="/i|.png" alt="p|q"> <a href="/y|z"></a> <a href="https://e.com/a|b">https://e.com/a|b</a></td></tr></tbody></table>',
    "| h |\n| --- |\n| [l](/x?a\\|b) ![p\\|q](/i\\|.png) <https://e.com/a\\|b> |",
  );
  // Links whose address is a refused one (a space) lose the link, keep the text; links
  // are made by the editor's autolinking, so a link followed by letters is one link.
  await writes(
    "Autolinks",
    '<p>see <a href="https://example.com/a_b">https://example.com/a_b</a>. (<a href="https://example.com/">https://example.com/</a>)</p>' +
      '<p><em><a href="https://example.com/">https://example.com/</a></em></p>' +
      '<p><a href="/issue/a b">/issue/a b</a> <a href="mailto:a@example.com">mailto:a@example.com</a> <a href="https://example.com/"></a></p>',
    "see https://example.com/a_b. (https://example.com/)\n\n" +
      "[*https://example.com/*](https://example.com/)\n\n" +
      "/issue/a b <mailto:a@example.com>",
  );
  {
    // A link to an address with a space is held, and saved with the space
    // encoded (a serializer normalisation: the same link).
    const text = "[/issue/a b](</issue/a b>)";
    const i = await open("Space in a link target", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a link to an address with a space opens rich`);
    ok((await richBox.locator('a[href="/issue/a%20b"]').count()) === 1, `${tag} and shows as a link`);
    await settleClose();
    const got = await saved(i.key, "[/issue/a b](/issue/a%20b)");
    ok(got === "[/issue/a b](/issue/a%20b)", `${tag} and saves the address encoded: ${JSON.stringify(got)}`);
  }
  await writes(
    "Table cell blocks",
    "<table><tbody><tr><th>h</th></tr><tr><td><ul><li>a|b</li></ul><pre><code>c|d\ne</code></pre></td></tr></tbody></table>",
    "| h |\n| --- |\n| - a\\|b `c\\|d e` |",
  );
  await writes(
    "Table cell code backslash",
    "<table><tbody><tr><th>h</th><th>k</th></tr><tr><td><code>c\\|d</code></td><td>e</td></tr></tbody></table>",
    "| h | k |\n| --- | --- |\n| `c\\|d` | e |",
  );
  await writes("Script and style", "<p>a<style>p{}</style><script>x()</script>b</p>", "ab");
  await writes("Heading break", "<h2>a<br>b</h2>", "## a b");
  await writes("Heading closer", "<h2>Issue #</h2>", "## Issue \\#");
  await writes("Indent", "<p>&nbsp;&nbsp;&nbsp;&nbsp;a<br><br>&nbsp;&nbsp;&nbsp;&nbsp;b<br>&nbsp;&nbsp;&nbsp;&nbsp;c</p>", "a\n\nb\n    c");
  await writes("Number after blank", "<p>a<br><br>2. b<br>3. c</p>", "a\n\n2\\. b\n3. c");
  await writes("Spaced rule", "<p>-- -<br><br>---</p>", "\\-- -\n\n\\---");
  await writes("Table row", "<p>a | b<br>--- | ---</p>", "a | b\n--- \\| ---");
  await writes("Bang link", '<p><span>wow!</span><a href="https://example.com/">x</a></p>', "wow\\![x](https://example.com/)");
  await writes("Bracket span", "<p><span>[a]</span>(b)</p>", "[a\\](b)");
  await writes(
    "Item blocks",
    "<ul><li><p>a</p><p>b</p><pre><code># c</code></pre></li><li>d</li></ul>",
    "- a\n\n  b\n\n  ```\n  # c\n  ```\n- d",
  );

  {
    const i = await open("Esc task", "- first");
    await richBox.waitFor();
    await field.press("Control+End");
    await field.press("Enter");
    await page.keyboard.insertText("[ ] foo");
    const want = "- first\n- \\[ ] foo";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} an inserted [ ] in a bullet is escaped: ${JSON.stringify(got)}`);
    await settleClose();
  }

  for (const [name, text] of [
    ["comment", "hello\n\n<!-- keep me -->"],
    ["style", "hello\n\n<style>p { color: red }</style>"],
    ["inline", "a <span>b</span> c"],
  ]) {
    const i = await open(`Html ${name}`, text);
    await mdBox.waitFor();
    ok((await note.count()) === 1, `${tag} ${name} opens as markdown with a note`);
    ok((await mdBox.inputValue()) === text, `${tag} ${name} text is held as stored`);
    await settleClose();
    ok((await desc(i.key)) === text, `${tag} ${name} is not deleted by opening and closing`);
  }

  {
    await page.addInitScript(() => {
      window.__pwned = 0;
    });
    const text = '<img src=x onerror="window.__pwned=1"> <script>window.__pwned=2</script>';
    const i = await open("Html script", text);
    await mdBox.waitFor();
    // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
    await page.waitForTimeout(500);
    ok((await page.evaluate(() => window.__pwned)) === 0, `${tag} script and onerror do not run`);
    ok((await note.count()) === 1, `${tag} script text opens as markdown with a note`);
    await settleClose();
    ok((await desc(i.key)) === text, `${tag} script text is unchanged`);
  }

  {
    const i = await open("Bold edge", "word next");
    await richBox.waitFor();
    await caretToStart(richBox);
    for (let k = 0; k < 5; k++) await richBox.press("Shift+ArrowRight");
    await toolbarOf(richBox).getByRole("button", { name: "Bold" }).click();
    const want = "**word** next";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} bold on "word " saves **word** next: ${JSON.stringify(got)}`);
    await settleClose();
  }

  {
    const text = "3. three\n4. four";
    const i = await open("Ol start", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a list starting at 3 opens rich`);
    await richBox.press("Control+End");
    await richBox.pressSequentially("!");
    const want = "3. three\n4. four!";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} ol start is kept: ${JSON.stringify(got)}`);
    await settleClose();
  }
  {
    const text = "1. one\n   - child\n2. two";
    const i = await open("Nested ol", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a list nested under a number opens rich`);
    await richBox.press("Control+End");
    await richBox.pressSequentially("!");
    const want = "1. one\n   - child\n2. two!";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} nested indent is the marker width + 1: ${JSON.stringify(got)}`);
    await settleClose();
  }

  {
    const text = "````\na\n```\nb\n````";
    const i = await open("Fence", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a code block holding a fence opens rich`);
    await caretToStart(richBox);
    await richBox.pressSequentially("z");
    const want = "````\nza\n```\nb\n````";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} the fence outgrows the body: ${JSON.stringify(got)}`);
    await settleClose();
  }
  // Paste (WP-1791): text that reads as markdown becomes formatting, prose is
  // literal, Shift+Ctrl+V is always literal, and HTML keeps what the schema holds.
  const pasted = async (name, text, opts, want, dom) => {
    const i = await open(`Paste ${name}`);
    await richBox.waitFor();
    await pasteText(richBox, text, opts);
    const html = await shape(richBox);
    if (dom) ok(typeof dom === "function" ? dom(html) : dom.test(html), `${tag} paste ${name}: editor DOM (${html.slice(0, 200)})`);
    const hit = want instanceof RegExp ? (d) => want.test(d) : (d) => d === want;
    let got = await desc(i.key);
    for (let k = 0; k < 40 && !hit(got); k++) {
      // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
      await page.waitForTimeout(250);
      got = await desc(i.key);
    }
    ok(hit(got), `${tag} paste ${name} saves ${want}: ${JSON.stringify(got)}`);
    await settleClose();
  };
  await pasted("fence", "```\nx\n```", {}, "```\nx\n```", /<pre><code>x/);
  await pasted("fence with language", "```js\nlet a = 1\n```", {}, "```js\nlet a = 1\n```", /language-js/);
  await pasted(
    "heading, list and task",
    "# T\n\n- a\n- [ ] b",
    {},
    /^# T\n\n- a\n\n?- \[ \] b$/,
    (h) => /<h1>T<\/h1>/.test(h) && /<li>a<\/li>/.test(h) && /type="checkbox"|data-type="taskItem"|class="task"/.test(h),
  );
  await pasted("inline markup", "some **bold** and `code`", {}, "some **bold** and `code`", /<strong>bold<\/strong>.*<code>code<\/code>/);
  await pasted("prose with snake_case", "use snake_case and file_name.txt here", {}, "use snake_case and file_name.txt here");
  await pasted("one line of prose", "just some words", {}, "just some words");
  await pasted(
    "with Shift+Ctrl+V",
    "# T\n\n- a\n\n```\nx\n```",
    { literal: true },
    // ProseMirror's plain paste makes one paragraph per line.
    "\\# T\n\n\\- a\n\n\\`\\`\\`\n\nx\n\n\\`\\`\\`",
    (h) => !/<h1>|<ul>|<pre>/.test(h),
  );
  await pasted(
    "HTML link",
    "label",
    { html: '<a href="https://example.com">label</a>' },
    /^\[label\]\(https:\/\/example\.com\/?\)$/,
    /<a [^>]*href="https:\/\/example\.com\/?"[^>]*>label<\/a>/,
  );
  await pasted(
    "HTML with styles, classes and scripts",
    "hi there",
    {
      html:
        '<p class="Mso x" style="color:red;font-family:Comic Sans" onclick="alert(1)"><span style="font-weight:700" class="y">hi</span> there<script>window.__x=1</script><style>p{}</style></p>',
    },
    // The bold the schema holds is kept; no style, class or script reaches the text.
    "**hi** there",
    (h) => !/style=|onclick|<script|<style|Mso|Comic|class=/.test(h) && /<strong>hi<\/strong> there/.test(h),
  );
  await ctx.close();
}

if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
await browser.close();
