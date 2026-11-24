// The rich editor takes markdown typed on the keyboard (WP-1749): "# ", "- ",
// "[ ] ", "> ", fences, rules and `code`, **bold**, *italic*, ~~strike~~ and
// [links](url) apply as they are typed, undo gives back the typed characters,
// and opening the editor with a click moves nothing on the page.
// Run after 01-signup-flow (user robin). RICH_TYPING_BROWSER=firefox (or
// webkit) runs it under that browser, which the other suites don't use. There
// the typed-markdown rules are switched off (richdom.ts, WP-1758), so the
// cases that expect a conversion FAIL by design: that mode exists to measure
// them for WP-1758, not to pass. Only Chromium is expected to pass.
import { chromium, firefox, webkit } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await (
  { firefox, webkit }[process.env.RICH_TYPING_BROWSER ?? ""] ?? chromium
).launch();
// Milliseconds between key presses. Found by trial, not derived: with no delay
// the typing outran the editor's input handling and cases flaked; 12 was the
// smallest value that did not, and still keeps the suite short.
const KEY_DELAY = 12;

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

// A long description that mixes every kind of block, with a unique plain word
// in each paragraph to click on. `ref` is a real issue key.
const longDescription = (ref) => {
  const parts = [];
  for (let i = 0; i < 6; i++) {
    parts.push(
      `## Section ${i}`,
      "",
      `Paragraph ${i} has plainwordnumber${i}x text with \`inline code\`, a mention @robin, a reference ${ref} and a [[Some Page]] wikilink that run long enough to wrap onto a second line of the paragraph.`,
      "",
      "- bullet one",
      "- bullet two with more words",
      "",
      "Tasks follow.",
      "",
      "- [ ] open task",
      "- [x] done task",
      "",
      "```js",
      `const value${i} = 1 * 2`,
      "```",
      "",
      "> quoted words",
      "",
      "See [the site](https://example.com) for more.",
      "",
      "| a | b |",
      "| --- | --- |",
      "| 1 | 2 |",
      "",
    );
  }
  return parts.join("\n");
};

for (const [width, scheme] of looks(
  [
    [1280, "light"],
    [390, "dark"],
  ],
  [0, 1],
)) {
  const tag = `w${width}`;
  const height = width < 500 ? 844 : 800;
  const ctx = await browser.newContext({ viewport: { width, height }, colorScheme: scheme });
  const page = await login(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const make = async (title, description) =>
    (
      await page.request.post(base + "/api/v1/issues", {
        headers: H,
        data: { project: "RICH", title: `${title} ${tag}`, description: description || undefined },
      })
    ).json();
  const desc = async (key) =>
    (await (await page.request.get(base + `/api/v1/issues/${key}`, { headers: H })).json())
      .description;
  const waitDesc = async (key, want, ms = 10000) => {
    const end = Date.now() + ms;
    let d = await desc(key);
    while (d !== want && Date.now() < end) {
      // wplint:ignore e2e-wait polling a value or proving a negative: there is nothing to wait on
      await page.waitForTimeout(250);
      d = await desc(key);
    }
    return d;
  };
  const richBox = page.locator('[contenteditable="true"][aria-label="Description"]');
  const done = page.getByRole("button", { name: "Done", exact: true });

  // Opens a new task's empty description and runs `steps` on the editor, then
  // checks the editor's HTML (`dom` is a regex or a function) and, after Done,
  // the saved markdown.
  const typed = async (name, steps, md, dom) => {
    const issue = await make(name, "");
    await page.goto(`${base}/issue/${issue.key}`);
    // The description lives on the Details tab, which a fresh browser opens second.
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
    await page.getByRole("button", { name: "Add a description…" }).click();
    await richBox.waitFor();
    await steps();
    const html = await richBox.evaluate((el) => el.innerHTML);
    if (dom) {
      const good = typeof dom === "function" ? dom(html) : dom.test(html);
      ok(good, `${tag} ${name}: editor DOM (${html.replace(/​/g, "·").slice(0, 160)})`);
    }
    await done.click();
    if (md === null) {
      // Only the editor's DOM is checked; the editor closes once it has saved.
      await richBox.waitFor({ state: "detached" });
      return issue;
    }
    const got = await waitDesc(issue.key, md);
    ok(got === md, `${tag} ${name}: saved ${JSON.stringify(md)}${got === md ? "" : ` got ${JSON.stringify(got)}`}`);
    return issue;
  };
  const type = (s) => page.keyboard.type(s, { delay: KEY_DELAY });
  const press = (k) => page.keyboard.press(k);

  // 1. Headings.
  for (const n of [1, 2, 3, 4, 5, 6])
    await typed(
      `heading ${n}`,
      async () => {
        await type(`${"#".repeat(n)} Title`);
        await press("Enter");
        await type("plain");
      },
      `${"#".repeat(n)} Title\n\nplain`,
      new RegExp(`<h${n}>Title</h${n}><p>plain</p>`),
    );

  // 2. Lists.
  await typed(
    "bullets",
    async () => {
      await type("- one");
      await press("Enter");
      await type("two");
      await press("Enter");
      await press("Enter");
      await type("after");
    },
    "- one\n- two\n\nafter",
    /<ul><li>one<\/li><li>two<\/li><\/ul><p>after<\/p>/,
  );
  await typed(
    "numbers",
    async () => {
      await type("1. a");
      await press("Enter");
      await type("b");
    },
    "1. a\n2. b",
    /<ol><li>a<\/li><li>b<\/li><\/ol>/,
  );
  await typed(
    "numbers from 3",
    async () => {
      await type("3) c");
      await press("Enter");
      await type("d");
    },
    "3. c\n4. d",
    /<ol start="3">/,
  );
  await typed(
    "star and plus bullets",
    async () => {
      await type("* a");
      await press("Enter");
      await press("Enter");
      await type("+ b");
    },
    "- a\n\n- b",
    /^<ul><li>a<\/li><\/ul><ul><li>b<\/li><\/ul>$/,
  );

  // 3. Tasks.
  await typed(
    "task",
    async () => {
      await type("- [ ] buy");
      await press("Enter");
      await type("milk");
      await press("Enter");
      await press("Enter");
      await type("after");
    },
    "- [ ] buy\n- [ ] milk\n\nafter",
    /<li><input[^>]*class="task"[^>]*>(?:&nbsp;| )?buy<\/li><li><input[^>]*>(?:&nbsp;| )?milk<\/li><\/ul><p>after/,
  );
  await typed(
    "checked task",
    async () => {
      await type("[x] done");
      await press("Enter");
      await press("Enter");
      await type("x");
    },
    "- [x] done\n\nx",
    /<input[^>]*checked/,
  );
  await typed(
    "empty box task",
    async () => {
      await type("[] a");
    },
    "- [ ] a",
    /<input[^>]*class="task"/,
  );

  // 4. Quote.
  await typed(
    "quote",
    async () => {
      await type("> quoted");
      await press("Enter");
      await press("Enter");
      await type("out");
    },
    "> quoted\n\nout",
    /<blockquote><p>quoted<\/p><\/blockquote><p>out<\/p>/,
  );

  // 5. Code fences.
  await typed(
    "fence with language",
    async () => {
      await type("```js");
      await press("Enter");
      await type("const a_b = 1 * 2");
      await press("Enter");
      await type("x()");
      await press("Enter");
      await type("```");
      await press("Enter");
      await type("after");
    },
    "```js\nconst a_b = 1 * 2\nx()\n```\n\nafter",
    /<pre><code class="language-js">const a_b = 1 \* 2<br>x\(\)(<br>)?<\/code><\/pre><p>after<\/p>/,
  );
  await typed(
    "bare fence on space",
    async () => {
      await type("``` ");
      await type("# not a heading `x` **y**");
      await press("Enter");
      await press("Enter");
      await press("Enter");
      await type("after");
    },
    "```\n# not a heading `x` **y**\n```\n\nafter",
    /<pre><code>[^<]*(<br>)?<\/code><\/pre><p>after<\/p>/,
  );
  await typed(
    "code block Tab and ArrowDown",
    async () => {
      await type("```");
      await press("Enter");
      await press("Tab");
      await type("a");
      await press("ArrowDown");
      await type("below");
    },
    "```\n  a\n```\n\nbelow",
    /<pre>.*<\/pre><p>below<\/p>/,
  );

  // 6. Rule.
  await typed(
    "rule",
    async () => {
      await type("---");
      await press("Enter");
      await type("below");
    },
    "---\n\nbelow",
    /<hr><p>below<\/p>/,
  );

  for (const r of ["***", "___"])
    await typed(
      `rule ${r}`,
      async () => {
        await type(r);
        await press("Enter");
        await type("below");
      },
      "---\n\nbelow",
      /<hr><p>below<\/p>/,
    );

  // 7. Inline code, with typing continuing outside it.
  await typed(
    "inline code",
    async () => {
      await type("use `npm run x` now");
    },
    "use `npm run x` now",
    (h) => /<code>npm run x<\/code>[^<]*now/.test(h) && !/<code>[^<]*now/.test(h),
  );
  await typed(
    "inline code at the end then Enter",
    async () => {
      await type("`a`");
      await press("Enter");
      await type("b");
    },
    "`a`\n\nb",
    /<p><code>a<\/code>[^<]*<\/p><p>b<\/p>/,
  );
  await typed(
    "code in a list item, then more",
    async () => {
      await type("- run `x`");
      await type(" and more");
    },
    "- run `x` and more",
    (h) => /<li>run(?:&nbsp;| )<code>x<\/code>[^<]* and more<\/li>/.test(h),
  );

  // 8. Emphasis.
  for (const [src, md, want] of [
    ["a **bold** b", "a **bold** b", /<strong>bold<\/strong>[^<]* b/],
    ["a *it* b", "a *it* b", /<em>it<\/em>[^<]* b/],
    ["a __bold__ b", "a **bold** b", /<strong>bold<\/strong>[^<]* b/],
    ["a _it_ b", "a *it* b", /<em>it<\/em>[^<]* b/],
    ["a ~~gone~~ b", "a ~~gone~~ b", /<s>gone<\/s>[^<]* b/],
    ["**a** and *b* and `c` end", "**a** and *b* and `c` end", /<strong>a<\/strong>[^<]* and(?:&nbsp;| )<em>b<\/em>[^<]* and(?:&nbsp;| )<code>c<\/code>[^<]* end/],
  ])
    await typed(
      `emphasis ${src}`,
      async () => {
        await type(src);
      },
      md,
      (h) => want.test(h) && !/<(strong|em|s|code)>[^<]* b<\//.test(h),
    );

  // 9. Things that stay as typed.
  for (const [src, md] of [
    ["snake_case_name and file_name.txt", "snake_case_name and file_name.txt"],
    ["2 * 3 * 4", "2 * 3 * 4"],
    ["a `", "a \\`"],
    ["#hashtag", "#hashtag"],
    ["-5 degrees", "-5 degrees"],
    ["a_b_ c", "a_b\\_ c"],
    ["**", "\\*\\*"],
  ]) {
    await typed(
      `literal ${src}`,
      async () => {
        await type(src);
      },
      md,
      (h) => !/<(h\d|ul|ol|strong|em|s|code|blockquote)\b/.test(h),
    );
  }

  // 10. Links.
  await typed(
    "link",
    async () => {
      await type("[the site](https://example.com) x");
    },
    "[the site](https://example.com) x",
    /<a href="https:\/\/example.com">the site<\/a>[^<]* x/,
  );
  await typed(
    "link with an unsafe address stays text",
    async () => {
      await type("[x](javascript:alert) y");
    },
    "[x\\](javascript:alert) y",
    (h) => !/<a /.test(h),
  );

  for (const src of [
    "![x](https://example.com)",
    "[](https://example.com)",
    "[x](data:text/html,x)",
    "[x](//host)",
  ])
    await typed(
      `link negative ${src}`,
      async () => {
        await type(`${src} y`);
      },
      null,
      (h) => !/<a /.test(h),
    );

  // 11. Undo gives back what was typed.
  await typed(
    "undo a heading",
    async () => {
      await type("# ");
      await press("Control+z");
      const t = await richBox.evaluate((el) => el.innerHTML);
      ok(/^<p>#(&nbsp;| )<\/p>$/.test(t), `${tag} undo after "# " is the literal text (${t})`);
      // The restored text is selected, so typing would replace it.
      await press("End");
      await type("x");
    },
    "\\# x",
    /<p>#[^<]*x<\/p>/,
  );
  await typed(
    "undo inline code",
    async () => {
      await type("a `code`");
      await press("Control+z");
      const t = await richBox.evaluate((el) => el.innerText.replace(/​/g, ""));
      ok(t.trim() === "a `code`", `${tag} undo after the closing backtick is the literal text (${JSON.stringify(t)})`);
      await press("End");
      await type("!");
    },
    "a \\`code\\`!",
    (h) => !/<code>/.test(h),
  );
  await typed(
    "undo bold then redo",
    async () => {
      await type("**b**");
      await press("Control+z");
      const t = await richBox.evaluate((el) => el.innerText.replace(/\ufeff/g, ""));
      ok(t.trim() === "**b**", `${tag} undo after the closing ** is the literal text (${JSON.stringify(t)})`);
      await press("Control+Shift+z");
    },
    "**b**",
    /<strong>b<\/strong>/,
  );

  await typed(
    "undo a bullet",
    async () => {
      await type("- ");
      await press("Control+z");
      const t = await richBox.evaluate((el) => el.innerHTML);
      ok(/^<p>-(&nbsp;| )<\/p>$/.test(t), `${tag} undo after "- " is the literal text (${t})`);
      await press("End");
      await type("x");
    },
    "\\- x",
    /<p>-[^<]*x<\/p>/,
  );
  await typed(
    "undo a fence",
    async () => {
      await type("```");
      await press("Enter");
      await press("Control+z");
      const t = await richBox.evaluate((el) => el.innerHTML);
      ok(/^<p>```<\/p>$/.test(t), `${tag} undo after Enter on a fence is the literal text (${t})`);
      await press("End");
      await type("x");
    },
    "\\`\\`\\`x",
    (h) => !/<pre/.test(h),
  );

  // 12. Backspace turns a block back into a paragraph.
  await typed(
    "backspace in a heading",
    async () => {
      await type("# Title");
      for (let i = 0; i < 5; i++) await press("ArrowLeft");
      await press("Home");
      await press("Backspace");
    },
    "Title",
    /<p>Title<\/p>/,
  );
  await typed(
    "backspace in a quote",
    async () => {
      await type("> q");
      await press("Home");
      await press("Backspace");
    },
    "q",
    (h) => !/blockquote/.test(h),
  );
  await typed(
    "backspace at a task box",
    async () => {
      await type("[ ] t");
      await press("Home");
      await press("ArrowRight");
      await press("ArrowRight");
      await press("Backspace");
    },
    "- t",
    (h) => !/<input/.test(h),
  );
  await typed(
    "backspace deletes one visible character after inline code",
    async () => {
      await type("`ab`");
      await type("cd");
      await press("Backspace");
      await press("Backspace");
      await press("Backspace");
      await type("X");
    },
    "`aX`",
    (h) => /<code>aX<\/code>/.test(h) || /<code>a<\/code>[^<]*X/.test(h),
  );
  await typed(
    "arrows over the caret parking space move one character",
    async () => {
      await type("`ab`cd");
      await press("ArrowLeft");
      await press("ArrowLeft");
      await press("ArrowLeft");
      await type("X");
    },
    "`aXb`cd",
    null,
  );

  // Opens an existing description for editing by clicking its text.
  const openDesc = async (name, initial) => {
    const issue = await make(name, initial);
    await page.goto(`${base}/issue/${issue.key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
    return issue;
  };
  const viewBox = page.locator('.prose-wp[title="Click or press Enter to edit"]').first();
  const closeAndCheck = async (issue, name, md) => {
    await done.click();
    const got = await waitDesc(issue.key, md);
    ok(got === md, `${tag} ${name}: saved ${JSON.stringify(md)}${got === md ? "" : ` got ${JSON.stringify(got)}`}`);
  };
  // Whether the last key press was default-prevented once it had gone through
  // every handler (the listener on window runs after React's).
  const watchKeys = () =>
    page.evaluate(() => {
      window.__dp = [];
      window.addEventListener("keydown", (e) => window.__dp.push(e.defaultPrevented));
    });

  // 14. Rules that must not eat text.
  await typed(
    "fence on space with text after the caret",
    async () => {
      await type("foo");
      await press("Home");
      await type("``` ");
    },
    "\\`\\`\\` foo",
    (h) => /foo/.test(h) && !/<pre/.test(h),
  );
  await typed(
    "Enter in the middle of a task keeps the text",
    async () => {
      await type("[ ] buy milk");
      for (let i = 0; i < 4; i++) await press("ArrowLeft");
      await press("Enter");
    },
    "- [ ] buy\n- [ ] milk",
    /buy(?:&nbsp;| )?<\/li><li><input[^>]*>[^<]*milk<\/li>/,
  );
  await typed(
    "Enter at the start of a task's text keeps the text",
    async () => {
      await type("[ ] milk");
      await press("Home");
      await press("ArrowRight");
      await press("ArrowRight");
      await press("Enter");
    },
    "- [ ]\n- [ ] milk",
    /<li><input[^>]*>(?:<br>)?<\/li><li><input[^>]*>[^<]*milk<\/li>/,
  );
  {
    const i = await openDesc("Task in a bullet", "- first");
    await viewBox.click();
    await richBox.waitFor();
    await press("Control+End");
    await press("Enter");
    await type("[ ] ");
    await type("t");
    const h = await richBox.evaluate((el) => el.innerHTML);
    ok(/<li>first<\/li><li><input[^>]*class="task"[^>]*>[^<]*t<\/li>/.test(h), `${tag} "[ ] " typed at the start of a bullet makes it a task (${h.slice(0, 160)})`);
    await closeAndCheck(i, "task in a bullet", "- first\n- [ ] t");
  }

  // Tables: the caret in a cell is in the cell, not in "the table".
  {
    const table = "| a | b |\n| --- | --- |\n| 1 | 2 |";
    const i = await openDesc("Table", table);
    await page.locator(".prose-wp th").first().click();
    await richBox.waitFor();
    await press("Home");
    await type("- ");
    await type("# ");
    await type("``` ");
    await press("End");
    await type(" `x` y");
    const h = await richBox.evaluate((el) => el.innerHTML);
    ok(/<table/.test(h) && !/<(ul|h1|pre)\b/.test(h), `${tag} block rules typed in a table cell leave the table alone (${h.slice(0, 200)})`);
    ok(/<th>[^<]*<code>x<\/code>/.test(h) && !/<code>x<\/code>[^<]*y<\/code>/.test(h), `${tag} inline code typed in a cell converts, inside that cell (${h.slice(0, 200)})`);
    await done.click();
    await richBox.waitFor({ state: "detached" });
    const got = await desc(i.key);
    ok(/^\| .*\|\n\| --- \| --- \|\n\| 1 \| 2 \|$/.test(got) && got.includes("`x`") && got.includes("- # \\`\\`\\` "), `${tag} the table is still a table in the saved markdown (${JSON.stringify(got)})`);
  }

  // Opening by keyboard puts the caret after a trailing link or code.
  for (const [name, src, want] of [
    ["link", "See [the docs](https://example.com)", "See [the docs](https://example.com) more"],
    ["code", "Run `npm test`", "Run `npm test` more"],
  ]) {
    const i = await openDesc(`Trailing ${name}`, src);
    await viewBox.focus();
    await press("Enter");
    await richBox.waitFor();
    await type(" more");
    await closeAndCheck(i, `typing after a trailing ${name}`, want);
  }

  // A zero-width space the person's own text holds is kept.
  await typed(
    "pasted U+200B in a paragraph",
    async () => {
      await page.keyboard.insertText("a​b");
    },
    "a​b",
    null,
  );
  await typed(
    "pasted U+200B in a code block",
    async () => {
      await type("```");
      await press("Enter");
      await page.keyboard.insertText("x​y");
    },
    "```\nx​y\n```",
    null,
  );
  {
    const i = await openDesc("Stored U+200B", "one​two");
    await viewBox.click();
    await richBox.waitFor({ timeout: 3000 });
    ok((await page.locator('textarea[aria-label="Description"]').count()) === 0, `${tag} a description with a U+200B opens in the rich editor`);
    await closeAndCheck(i, "stored U+200B", "one​two");
  }

  // Arrows: only the step over the caret parking space is the editor's.
  await typed(
    "arrow keys elsewhere are left to the browser",
    async () => {
      await type("`ab`cd");
      await watchKeys();
      await press("ArrowLeft");
      const dp = await page.evaluate(() => window.__dp);
      ok(dp.length === 1 && dp[0] === false, `${tag} an arrow press away from the parking space is not default-prevented (${dp})`);
      await press("ArrowLeft");
      await press("ArrowLeft");
      await type("X");
    },
    "`aXb`cd",
    null,
  );
  await typed(
    "Tab outside a code block is not eaten",
    async () => {
      await type("a");
      await watchKeys();
      await press("Tab");
      const dp = await page.evaluate(() => window.__dp);
      ok(dp.length === 1 && dp[0] === false, `${tag} Tab in a paragraph is not default-prevented (${dp})`);
      const inside = await richBox.evaluate((el) => el.contains(document.activeElement));
      ok(!inside, `${tag} Tab in a paragraph moves focus out of the editor`);
    },
    "a",
    null,
  );

  // Backspace over the parking space takes one visible character, an emoji too.
  await typed(
    "backspace over the parking space after an emoji",
    async () => {
      await type("`a\u{1F600}`");
      await press("Backspace");
      await type("X");
    },
    "`aX`",
    (h) => /<code>aX<\/code>/.test(h) || /<code>a<\/code>[^<]*X/.test(h),
  );

  // A rule that throws must not lose the typed text.
  await typed(
    "a throwing rule still saves",
    async () => {
      await page.evaluate(() => {
        const real = Element.prototype.closest;
        window.__restore = () => (Element.prototype.closest = real);
        Element.prototype.closest = function (sel) {
          if (sel === "pre, code") throw new Error("boom");
          return real.call(this, sel);
        };
      });
      await type("ab");
      await page.evaluate(() => window.__restore());
    },
    "ab",
    null,
  );

  // A drag over the text selects it; it does not open the editor.
  {
    await openDesc("Drag select", "Some selectable words here");
    await viewBox.waitFor();
    const r = await viewBox.evaluate((el) => {
      const range = document.createRange();
      range.selectNodeContents(el.querySelector("p"));
      const b = range.getBoundingClientRect();
      return { l: b.left + 2, r: b.right - 2, y: b.top + b.height / 2 };
    });
    await page.mouse.move(r.l, r.y);
    await page.mouse.down();
    await page.mouse.move((r.l + r.r) / 2, r.y, { steps: 5 });
    await page.mouse.move(r.r, r.y, { steps: 5 });
    await page.mouse.up();
    const sel = await page.evaluate(() => getSelection().toString());
    ok(sel.length > 5, `${tag} the drag selected text (${JSON.stringify(sel)})`);
    ok((await richBox.count()) === 0, `${tag} a drag-select over the description does not open the editor`);
  }

  // The toolbar clears the page header in the issue sheet too.
  {
    const i = await openDesc("Sheet toolbar", "Words in a sheet.\n\nMore words.");
    await page.goto(`${base}/p/RICH/board?issue=${i.key}`);
    const dialog = page.getByRole("dialog", { name: i.key });
    await dialog.waitFor();
    await dialog.locator('.prose-wp[title="Click or press Enter to edit"]').first().click();
    await richBox.waitFor();
    const tb = page.getByRole("toolbar", { name: "Formatting" });
    const box = await tb.boundingBox();
    ok(box && box.y >= 0 && box.y + box.height <= height && box.x >= 0 && box.x + box.width <= width + 0.5, `${tag} the toolbar is on screen in the sheet (${JSON.stringify(box)})`);
    const hit = await tb.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const top = document.elementFromPoint(r.left + 12, r.top + r.height / 2);
      return !!top && !!top.closest('[role="toolbar"]');
    });
    ok(hit, `${tag} the toolbar is not covered in the sheet`);
    const ed = await richBox.boundingBox();
    ok(box && ed && box.y + box.height <= ed.y + 0.5, `${tag} the sheet's toolbar sits above the text (${JSON.stringify(box)} / ${JSON.stringify(ed)})`);
    await page.screenshot({ path: `${out}/rich-sheet-${tag}.png` });
    await press("Escape");
    await page.goto(`${base}/`);
  }

  // The toolbar's row scrolls sideways on a phone, and says so.
  // (At 390 the buttons still fit; the row overflows from about 330px.)
  if (width < 500) {
    await page.setViewportSize({ width: 320, height });
    const i = await openDesc("Toolbar cue", "Words.");
    await viewBox.click();
    await richBox.waitFor();
    const tb = page.getByRole("toolbar", { name: "Formatting" });
    const state = () =>
      tb.evaluate((el) => ({
        fade: el.getAttribute("data-fade"),
        mask: getComputedStyle(el).maskImage || getComputedStyle(el).webkitMaskImage,
        left: el.scrollLeft,
        max: el.scrollWidth - el.clientWidth,
      }));
    const a = await state();
    ok(a.max > 0 && a.left === 0, `${tag} the toolbar row overflows at 320 (${JSON.stringify(a)})`);
    ok(a.fade === "right" && a.mask && a.mask !== "none", `${tag} a fade on the right edge says there are more buttons (${JSON.stringify(a)})`);
    await page.screenshot({ path: `${out}/rich-toolbar-cue-start-${tag}.png` });
    await tb.evaluate((el) => (el.scrollLeft = el.scrollWidth));
    await tb.evaluate((el) => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
    const b = await state();
    ok(b.fade === "left" && b.mask && b.mask !== "none", `${tag} at the end the fade is on the left only (${JSON.stringify(b)})`);
    await page.screenshot({ path: `${out}/rich-toolbar-cue-end-${tag}.png` });
    await page.getByRole("button", { name: "Edit as markdown" }).click();
    await page.locator('textarea[aria-label="Description"]').waitFor();
    ok(true, `${tag} "Edit as markdown" is reachable by scrolling the toolbar`);
    await page.getByRole("button", { name: "Edit formatted" }).click();
    await richBox.waitFor();
    await press("Escape");
    await page.setViewportSize({ width, height });
  }

  // An empty (or blank) description opens into the same box: nothing moves.
  for (const blank of [undefined, "   "]) {
    const i = await openDesc(blank ? "Blank description" : "Empty description", blank);
    const btn = page.getByRole("button", { name: "Add a description…", exact: true });
    await btn.waitFor();
    const measure = (sel) =>
      page.evaluate((sel) => {
        const el = sel
          ? document.querySelector(sel)
          : Array.from(document.querySelectorAll("button")).find((b) => b.textContent === "Add a description…");
        if (!window.__below) {
          let n = el;
          while (n && !n.nextElementSibling) n = n.parentElement;
          window.__below = n?.nextElementSibling;
        }
        const box = (e) => {
          const r = e.getBoundingClientRect();
          return [r.left, r.top, r.width, r.height].map((v) => Math.round(v * 100) / 100);
        };
        const cs = getComputedStyle(el);
        return {
          self: box(el),
          below: window.__below ? box(window.__below) : null,
          scrollY: window.scrollY,
          style: [cs.paddingTop, cs.paddingLeft, cs.fontSize, cs.lineHeight, cs.fontWeight, cs.fontFamily].join("|"),
        };
      }, sel);
    const before = await measure(null);
    await btn.click();
    await richBox.waitFor();
    const after = await measure('[contenteditable="true"][aria-label="Description"]');
    const same = (a, b) => a.length === b.length && a.every((v, k) => Math.abs(v - b[k]) <= 0.5);
    const what = blank ? "a blank description" : "an empty description";
    ok(same(before.self, after.self), `${tag} ${what}: the placeholder's box is where it was (${before.self} -> ${after.self})`);
    ok(before.below && same(before.below, after.below), `${tag} ${what}: what is below did not move (${before.below} -> ${after.below})`);
    ok(before.scrollY === after.scrollY, `${tag} ${what}: nothing scrolled (${before.scrollY} -> ${after.scrollY})`);
    ok(before.style === after.style, `${tag} ${what}: same padding and font (${before.style} / ${after.style})`);
    await press("Escape");
  }

  // 13. Clicking to edit moves nothing.
  {
    const ref = await make("Ref", "Referenced.");
    const long = await make("Long", longDescription(ref.key));
    await page.goto(`${base}/issue/${long.key}`);
    const view = page.locator('.prose-wp[title="Click or press Enter to edit"]').first();
    await view.waitFor();
    // Section 3 (of 6) is the middle of the page.
    const target = await view.evaluate((el, vh) => {
      const scroller = (() => {
        for (let n = el.parentElement; n; n = n.parentElement) {
          const o = getComputedStyle(n).overflowY;
          if ((o === "auto" || o === "scroll") && n.scrollHeight > n.clientHeight) return n;
        }
        return document.scrollingElement;
      })();
      const p = Array.from(el.querySelectorAll("p")).find((x) => x.textContent.includes("plainwordnumber3x"));
      const r0 = p.getBoundingClientRect();
      scroller.scrollTop += r0.top + r0.height / 2 - vh / 2;
      return { tall: el.getBoundingClientRect().height };
    }, height);
    ok(target.tall > height * 2, `${tag} the description is taller than two screens (${Math.round(target.tall)}px)`);
    // wplint:ignore e2e-wait the scroll has to settle before positions are recorded
    await page.waitForTimeout(300);
    const measure = () =>
      page.evaluate(() => {
        const el =
          document.querySelector('[contenteditable="true"][aria-label="Description"]') ??
          document.querySelector('.prose-wp[title="Click or press Enter to edit"]');
        // The neighbours are picked once, from the view, and measured again as the
        // same elements after the editor opens.
        if (!window.__above) {
          let n = el;
          while (n && !n.previousElementSibling) n = n.parentElement;
          window.__above = n?.previousElementSibling;
          n = el;
          while (n && !n.nextElementSibling) n = n.parentElement;
          window.__below = n?.nextElementSibling;
        }
        const scrollers = [];
        for (let n = el.parentElement; n; n = n.parentElement) scrollers.push(n.scrollTop);
        const box = (e) => {
          if (!e) return null;
          const r = e.getBoundingClientRect();
          return [r.left, r.top, r.width, r.height].map((v) => Math.round(v * 100) / 100);
        };
        const sel = "p,h1,h2,h3,li,ul,ol,pre,blockquote,table,hr";
        // The code blocks' copy button is not part of the text.
        const blocks = Array.from(el.querySelectorAll(sel)).filter((e) => !e.closest(".code-copy"));
        return {
          scrollY: window.scrollY,
          scrollers,
          self: box(el),
          above: box(window.__above),
          below: box(window.__below),
          blocks: blocks.map((e) => `${e.tagName} ${JSON.stringify(box(e))}`),
        };
      });
    const before = await measure();
    // The first word of a paragraph in the middle, and where it is.
    const word = await view.evaluate((el) => {
      const p = Array.from(el.querySelectorAll("p")).find((x) => x.textContent.includes("plainwordnumber3x"));
      const t = p.firstChild;
      const at = t.data.indexOf("plainwordnumber3x");
      const range = document.createRange();
      range.setStart(t, at);
      range.setEnd(t, at + "plainwordnumber3x".length);
      const r = range.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
    });
    ok(word.y > 0 && word.y < height, `${tag} the word to click is on screen (y ${Math.round(word.y)})`);
    await page.mouse.click(word.x, word.y);
    await richBox.waitFor().catch(async (e) => {
      await page.screenshot({ path: `${out}/click-${tag}.png` });
      console.log("click at", word, "hit", await page.evaluate(([x, y]) => document.elementFromPoint(x, y)?.outerHTML.slice(0, 200), [word.x, word.y]));
      throw e;
    });
    const after = await measure();
    ok(JSON.stringify(after.scrollers.filter(Boolean)) === JSON.stringify(before.scrollers.filter(Boolean)) && after.scrollY === before.scrollY, `${tag} nothing scrolled (${before.scrollers.filter(Boolean)} -> ${after.scrollers.filter(Boolean)})`);
    ok(JSON.stringify(after.self) === JSON.stringify(before.self), `${tag} the description box is where it was (${before.self} -> ${after.self})`);
    ok(JSON.stringify(after.above) === JSON.stringify(before.above), `${tag} what is above did not move`);
    ok(JSON.stringify(after.below) === JSON.stringify(before.below), `${tag} what is below did not move (${JSON.stringify(before.below)} -> ${JSON.stringify(after.below)})`);
    const moved = [];
    const kinds = (list) => list.map((s) => s.split(" ")[0]);
    ok(JSON.stringify(kinds(before.blocks)) === JSON.stringify(kinds(after.blocks)), `${tag} the same blocks are shown (${before.blocks.length} -> ${after.blocks.length})`);
    for (let i = 0; i < Math.min(before.blocks.length, after.blocks.length); i++) {
      const a = JSON.parse(before.blocks[i].replace(/^\S+ /, ""));
      const b = JSON.parse(after.blocks[i].replace(/^\S+ /, ""));
      // Every block's box must match to within half a pixel.
      if (a.some((v, k) => Math.abs(v - b[k]) > 0.5)) moved.push(`${before.blocks[i]} -> ${after.blocks[i]}`);
    }
    ok(moved.length === 0, `${tag} every block is at the same pixel position${moved.length ? `: ${moved.slice(0, 4).join("; ")}` : ""}`);
    const caret = await page.evaluate(() => {
      const s = getSelection();
      const n = s.anchorNode;
      return { collapsed: s.isCollapsed, text: n.nodeType === 3 ? n.data : null, offset: s.anchorOffset };
    });
    const w0 = caret.text?.indexOf("plainwordnumber3x") ?? -1;
    ok(
      caret.collapsed && w0 >= 0 && caret.offset >= w0 && caret.offset <= w0 + "plainwordnumber3x".length,
      `${tag} the caret is inside the clicked word (offset ${caret.offset}, word at ${w0})`,
    );
    const tb = page.getByRole("toolbar", { name: "Formatting" });
    const tbBox = await tb.boundingBox();
    ok(tbBox && tbBox.y >= 0 && tbBox.y + tbBox.height <= height && tbBox.x >= 0 && tbBox.x + tbBox.width <= width + 0.5, `${tag} the toolbar is on screen (${JSON.stringify(tbBox)})`);
    const hit = await tb.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const top = document.elementFromPoint(r.left + 12, r.top + r.height / 2);
      return !!top && !!top.closest('[role="toolbar"]');
    });
    ok(hit, `${tag} the toolbar is not covered by the page header`);
    if (width < 500) {
      const dims = await tb.evaluate((el) => [el.scrollWidth, el.clientWidth, el.getBoundingClientRect().height]);
      ok(dims[2] < 40, `${tag} the toolbar is one row, and scrolls sideways if it does not fit (scrollWidth ${dims[0]}, width ${dims[1]}, height ${dims[2]})`);
    }
    await page.screenshot({ path: `${out}/rich-long-${tag}.png` });
    // Typing goes in at the click.
    await type("ZZ");
    const text = await page.evaluate(() => getSelection().anchorNode.data);
    const w = "plainwordnumber3x";
    ok(
      Array.from({ length: w.length + 1 }, (_, k) => w.slice(0, k) + "ZZ" + w.slice(k)).some((s) => text.includes(s)),
      `${tag} typing after the click lands in the clicked word (${text.slice(0, 40)})`,
    );
    // Still reachable after scrolling on through the description.
    await page.mouse.wheel(0, height / 2);
    // wplint:ignore e2e-wait the scroll has to settle before the toolbar is measured
    await page.waitForTimeout(300);
    const tb2 = await tb.boundingBox();
    ok(tb2 && tb2.y >= 0 && tb2.y + tb2.height <= height, `${tag} the toolbar stays on screen while scrolling the description (${JSON.stringify(tb2)})`);
    await page.screenshot({ path: `${out}/rich-long-scrolled-${tag}.png` });
    await page.keyboard.press("Escape");
  }
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("no errors");
