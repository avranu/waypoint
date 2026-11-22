// The rich editor's markdown writer (WP-628): what is typed as literal text
// is escaped so it renders as typed and nothing else is; text it can't keep
// (raw HTML, comments) opens as markdown; lists, code fences and emphasis
// are written in a form that renders back the same.
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
  // The saved text once it equals want, polled for up to 10s (autosave).
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
  // Opens the description editor of a new issue (empty, or with text).
  const open = async (title, description) => {
    const issue = await make(title, description);
    await page.goto(`${base}/issue/${issue.key}`);
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
  const typeInto = async (title, typed, want) => {
    const issue = await open(title);
    await richBox.waitFor();
    await field.pressSequentially(typed);
    const got = await saved(issue.key, want);
    ok(got === want, `${tag} typed ${JSON.stringify(typed)} saves ${JSON.stringify(want)}: ${JSON.stringify(got)}`);
    await settleClose();
  };

  // Literal text typed into the editor is escaped, and no more than needed.
  await typeInto("Esc star", "*emph* and a*b", "\\*emph\\* and a\\*b");
  await typeInto("Esc tick", "a `code` b", "a \\`code\\` b");
  await typeInto("Esc under", "snake_case and _x_", "snake_case and \\_x\\_");
  await typeInto("Esc tilde", "~/bin and ~x~", "~/bin and \\~x\\~");
  await typeInto("Esc tag", "<div> and 1 < 2", "\\<div> and 1 < 2");
  await typeInto("Esc link", "[x](y) and [a][b]", "[x\\](y) and [a\\][b]");
  await typeInto("Esc image", "![a](b)", "![a\\](b)");
  await typeInto("Esc entity", "&amp; and R&D and &#65;", "\\&amp; and R&D and \\&#65;");
  await typeInto("Esc wiki", "see [[Some Page]] and [[A|b]]", "see [[Some Page]] and [[A|b]]");
  await typeInto("Esc heading", "# h", "\\# h");
  await typeInto("Esc heading root", "# not a heading", "\\# not a heading");
  await typeInto("Esc number", "1. n", "1\\. n");
  await typeInto("Esc number paren", "2) n", "2\\) n");
  await typeInto("Esc quote", "> q", "\\> q");
  await typeInto("Esc dash", "- d", "\\- d");
  await typeInto("Esc bullet star", "* d", "\\* d");
  await typeInto("Esc spaced star", "2 * 3 * 4", "2 * 3 * 4");
  await typeInto("Esc line star", "a" + "\n".repeat(0) + " * b", "a * b");

  // What the editor writes for a DOM it holds (as a paste or the browser can
  // leave it): link labels, alt text and table cells escape what would end
  // them, a heading or table row stays one line, and inline text next to a
  // block stays its own paragraph.
  const writes = async (title, html, want) => {
    const issue = await open(title);
    await richBox.waitFor();
    await richBox.evaluate((el, h) => {
      el.innerHTML = h;
      el.dispatchEvent(new InputEvent("input", { bubbles: true }));
    }, html);
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
  // Everything else written into a cell escapes its pipes too: link and
  // image destinations, a link shown by its address, alt text, a list or
  // code block.
  await writes(
    "Table cell targets",
    '<table><tbody><tr><th>h</th></tr><tr><td><a href="/x?a|b">l</a> <img src="/i|.png" alt="p|q"> <a href="/y|z"></a> <a href="https://e.com/a|b">https://e.com/a|b</a></td></tr></tbody></table>',
    "| h |\n| --- |\n| [l](/x?a\\|b) ![p\\|q](/i\\|.png) [/y\\|z](/y\\|z) <https://e.com/a\\|b> |",
  );
  // A link that reads as its own address stays a link: bare where nothing
  // next to it changes where it ends, <angled> where something would, and
  // [text](target) where it isn't an absolute address at all.
  await writes(
    "Autolinks",
    '<p>see <a href="https://example.com/a_b">https://example.com/a_b</a>. (<a href="https://example.com/">https://example.com/</a>)</p>' +
      '<p><a href="https://example.com/">https://example.com/</a>more <a href="https://example.com/x.">https://example.com/x.</a> <em><a href="https://example.com/">https://example.com/</a></em></p>' +
      '<p><a href="/issue/a b">/issue/a b</a> <a href="mailto:a@example.com">mailto:a@example.com</a> <a href="https://example.com/"></a></p>',
    "see https://example.com/a_b. (https://example.com/)\n\n" +
      "<https://example.com/>more <https://example.com/x.> *<https://example.com/>*\n\n" +
      "[/issue/a b](</issue/a b>) <mailto:a@example.com> https://example.com/",
  );
  await writes(
    "Table cell blocks",
    "<table><tbody><tr><th>h</th></tr><tr><td><ul><li>a|b</li></ul><pre><code>c|d\ne</code></pre></td></tr></tbody></table>",
    "| h |\n| --- |\n| - a\\|b `c\\|d e` |",
  );
  // The table reader splits at a pipe after an even run of backslashes, so a
  // code span's own backslash before a pipe must not leave it at two.
  await writes(
    "Table cell code backslash",
    "<table><tbody><tr><th>h</th><th>k</th></tr><tr><td><code>c\\|d</code></td><td>e</td></tr></tbody></table>",
    "| h | k |\n| --- | --- |\n| `c\\|d` | e |",
  );
  await writes("Script and style", "<p>a<style>p{}</style><script>x()</script>b</p>", "ab");
  await writes("Heading break", "<h2>a<br>b</h2>", "## a b");
  await writes("Heading closer", "<h2>Issue #</h2>", "## Issue \\#");
  // A paragraph's start, on its first line or after a blank one: an indent
  // that would make code is dropped, and any number or three dashes is a
  // block; inside it, a dash-and-pipe row would make a table.
  await writes("Indent", "<p>&nbsp;&nbsp;&nbsp;&nbsp;a<br><br>&nbsp;&nbsp;&nbsp;&nbsp;b<br>&nbsp;&nbsp;&nbsp;&nbsp;c</p>", "a\n\nb\n    c");
  await writes("Number after blank", "<p>a<br><br>2. b<br>3. c</p>", "a\n\n2\\. b\n3. c");
  await writes("Spaced rule", "<p>-- -<br><br>---</p>", "\\-- -\n\n\\---");
  await writes("Table row", "<p>a | b<br>--- | ---</p>", "a | b\n--- \\| ---");
  // escapeText sees one node: where a node meets the next, "!" before a link
  // and "]" before "(" are escaped too.
  await writes("Bang link", '<p><span>wow!</span><a href="https://example.com/">x</a></p>', "wow\\![x](https://example.com/)");
  await writes("Bracket span", "<p><span>[a]</span>(b)</p>", "[a\\](b)");
  // A list item's later lines are indented, so its blocks stay in it.
  await writes(
    "Item blocks",
    "<ul><li><p>a</p><p>b</p><pre><code># c</code></pre></li><li>d</li></ul>",
    "- a\n\n  b\n\n  ```\n  # c\n  ```\n- d",
  );

  // A literal task box at the start of a list item stays literal.
  {
    const i = await open("Esc task", "- first");
    await richBox.waitFor();
    await field.press("Control+End");
    await field.press("Enter");
    await field.pressSequentially("[ ] foo");
    const want = "- first\n- \\[ ] foo";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} a typed [ ] in a bullet is escaped: ${JSON.stringify(got)}`);
    await settleClose();
  }

  // Raw HTML the sanitiser strips can't be kept: it opens as markdown.
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

  // Raw HTML that would run script is neither run nor kept.
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

  // Emphasis keeps its edge spaces outside the markers.
  {
    const i = await open("Bold edge", "word next");
    await richBox.waitFor();
    await field.press("Control+Home");
    for (let k = 0; k < 5; k++) await field.press("Shift+ArrowRight");
    await page.getByRole("button", { name: "Bold" }).click();
    const want = "**word** next";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} bold on "word " saves **word** next: ${JSON.stringify(got)}`);
    await settleClose();
  }

  // Ordered lists keep their start, nested lists their indent.
  {
    const text = "3. three\n4. four";
    const i = await open("Ol start", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a list starting at 3 opens rich`);
    await field.press("Control+End");
    await field.pressSequentially("!");
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
    await field.press("Control+End");
    await field.pressSequentially("!");
    const want = "1. one\n   - child\n2. two!";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} nested indent is the marker width + 1: ${JSON.stringify(got)}`);
    await settleClose();
  }

  // A code body holding a fence line gets a longer fence.
  {
    const text = "````\na\n```\nb\n````";
    const i = await open("Fence", text);
    await richBox.waitFor();
    ok((await note.count()) === 0, `${tag} a code block holding a fence opens rich`);
    await field.press("Control+Home");
    await field.pressSequentially("z");
    const want = "````\nza\n```\nb\n````";
    const got = await saved(i.key, want);
    ok(got === want, `${tag} the fence outgrows the body: ${JSON.stringify(got)}`);
    await settleClose();
  }
  await ctx.close();
}

if (errors.length) {
  console.log(`${errors.length} failed`);
  process.exit(1);
}
await browser.close();
