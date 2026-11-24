// Typed markdown in the rich editor when the text arrives by composition
// (WP-1764): Android keyboards, IMEs and dictation send ordinary typing as
// compositionstart / insertCompositionText / compositionend, and Enter and
// Backspace as keydown 229 followed by beforeinput insertParagraph /
// deleteContentBackward. The rules must run once the composition has ended and
// never change the DOM while one is active.
// Run after 01-signup-flow (user robin).
//
// What stands in for a phone here: Chrome DevTools Protocol input on desktop
// Chromium. Input.imeSetComposition starts or updates a composition and
// Input.insertText commits it (the events seen are in the WP-1764 report:
// compositionstart, compositionupdate, beforeinput/input insertCompositionText
// with isComposing, compositionend). It sends no keydown 229 for composed
// characters, unlike Gboard. Enter and Backspace go as Input.dispatchKeyEvent
// with keyCode 229 / key "Unidentified" and an editing command, which makes
// the browser itself send beforeinput insertParagraph / deleteContentBackward,
// so the default action really happens. No event is synthesised with
// dispatchEvent. This is not a device test.
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
  const cdp = await ctx.newCDPSession(page);
  const make = async (title) =>
    (
      await page.request.post(base + "/api/v1/issues", {
        headers: H,
        data: { project: "RICH", title: `${title} ${tag}` },
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
  const html = () => richBox.evaluate((el) => el.innerHTML);

  // Composition, as Gboard does it. `comp` sets the composition text (still
  // open: isComposing), `commit` ends it with that text, `cc` types each
  // character as its own composition (symbols and spaces), `word` grows one
  // composition letter by letter and commits it (swipe typing sends it whole).
  const comp = (text) =>
    cdp.send("Input.imeSetComposition", {
      text,
      selectionStart: text.length,
      selectionEnd: text.length,
    });
  const commit = (text) => cdp.send("Input.insertText", { text });
  const cc = async (s) => {
    for (const ch of s) {
      await comp(ch);
      await commit(ch);
    }
  };
  const word = async (w) => {
    for (let i = 1; i <= w.length; i++) await comp(w.slice(0, i));
    await commit(w);
  };
  const swipe = async (w) => {
    await comp(w);
    await commit(w);
  };
  // Enter and Backspace from a soft keyboard: keydown 229, then the editing
  // command's own beforeinput.
  const imeKey = async (command) => {
    const k = { windowsVirtualKeyCode: 229, key: "Unidentified", code: "" };
    await cdp.send("Input.dispatchKeyEvent", { type: "rawKeyDown", ...k, commands: [command] });
    await cdp.send("Input.dispatchKeyEvent", { type: "keyUp", ...k });
  };
  const aEnter = () => imeKey("insertNewline");
  const aBack = () => imeKey("deleteBackward");
  const type = (s) => page.keyboard.type(s, { delay: KEY_DELAY });
  const press = (k) => page.keyboard.press(k);

  const typed = async (name, steps, md, dom) => {
    const issue = await make(name);
    await page.goto(`${base}/issue/${issue.key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
    await page.getByRole("button", { name: "Add a description…" }).click();
    await richBox.waitFor();
    await steps();
    const h = await html();
    if (dom) {
      const good = typeof dom === "function" ? dom(h) : dom.test(h);
      ok(good, `${tag} ${name}: editor DOM (${h.replace(/﻿/g, "·").slice(0, 160)})`);
    }
    await done.click();
    if (md === null) {
      await richBox.waitFor({ state: "detached" });
      return issue;
    }
    const got = await waitDesc(issue.key, md);
    ok(got === md, `${tag} ${name}: saved ${JSON.stringify(md)}${got === md ? "" : ` got ${JSON.stringify(got)}`}`);
    return issue;
  };

  // 1. Block rules, composed one character at a time.
  for (const n of [1, 2, 3, 4, 5, 6])
    await typed(
      `composed heading ${n}`,
      async () => {
        await cc("#".repeat(n) + " ");
        await word("Title");
        await aEnter();
        await word("plain");
      },
      `${"#".repeat(n)} Title\n\nplain`,
      new RegExp(`<h${n}>Title</h${n}><p>plain</p>`),
    );
  await typed(
    "composed bullets",
    async () => {
      await cc("- ");
      await word("one");
      await aEnter();
      await word("two");
      await aEnter();
      await aEnter();
      await word("after");
    },
    "- one\n- two\n\nafter",
    /<ul><li>one<\/li><li>two<\/li><\/ul><p>after<\/p>/,
  );
  for (const [mark, md] of [
    ["* ", "- a"],
    ["+ ", "- a"],
  ])
    await typed(`composed bullet ${mark.trim()}`, async () => {
      await cc(mark);
      await word("a");
    }, md, /^<ul><li>a<\/li><\/ul>$/);
  await typed(
    "composed numbers",
    async () => {
      await cc("1. ");
      await word("a");
      await aEnter();
      await word("b");
    },
    "1. a\n2. b",
    /<ol><li>a<\/li><li>b<\/li><\/ol>/,
  );
  await typed("composed numbers from 3", async () => {
    await cc("3) ");
    await word("c");
  }, "3. c", /<ol start="3"><li>c<\/li><\/ol>/);
  await typed(
    "composed task",
    async () => {
      await cc("- [ ] ");
      await word("buy");
      await aEnter();
      await word("milk");
      await aEnter();
      await aEnter();
      await word("after");
    },
    "- [ ] buy\n- [ ] milk\n\nafter",
    /<li><input[^>]*class="task"[^>]*>(?:&nbsp;| )?buy<\/li><li><input[^>]*>(?:&nbsp;| )?milk<\/li><\/ul><p>after/,
  );
  await typed("composed checked task", async () => {
    await cc("[x] ");
    await word("done");
  }, "- [x] done", /<input[^>]*checked/);
  await typed("composed empty-box task", async () => {
    await cc("[] ");
    await word("a");
  }, "- [ ] a", /<input[^>]*class="task"/);
  await typed(
    "composed quote",
    async () => {
      await cc("> ");
      await word("quoted");
      await aEnter();
      await aEnter();
      await word("out");
    },
    "> quoted\n\nout",
    /<blockquote><p>quoted<\/p><\/blockquote><p>out<\/p>/,
  );
  await typed(
    "composed fence with language, Enter by beforeinput",
    async () => {
      await cc("```");
      await word("js");
      await aEnter();
      await word("const");
      await cc(" ");
      await word("a");
      await aEnter();
      await cc("```");
      await aEnter();
      await word("after");
    },
    "```js\nconst a\n```\n\nafter",
    /<pre><code class="language-js">const a(<br>)?<\/code><\/pre><p>after<\/p>/,
  );
  await typed(
    "composed bare fence on space",
    async () => {
      await cc("``` ");
      await cc("# not a heading");
      await aEnter();
      await aEnter();
      await aEnter();
      await word("after");
    },
    "```\n# not a heading\n```\n\nafter",
    /<pre><code>[^<]*(<br>)?<\/code><\/pre><p>after<\/p>/,
  );
  for (const r of ["---", "***", "___"])
    await typed(
      `composed rule ${r}`,
      async () => {
        await cc(r);
        await aEnter();
        await word("below");
      },
      "---\n\nbelow",
      /<hr><p>below<\/p>/,
    );
  await typed(
    "composed heading 6 exit by beforeinput Enter",
    async () => {
      await cc("###### ");
      await word("six");
      await aEnter();
      await word("next");
    },
    "###### six\n\nnext",
    /<h6>six<\/h6><p>next<\/p>/,
  );

  // 2. Inline rules, composed.
  for (const [src, md, want] of [
    ["use `npm run x` now", "use `npm run x` now", /use(?:&nbsp;| )<code>npm run x<\/code>[^<]* now/],
    ["a **bold** b", "a **bold** b", /<strong>bold<\/strong>[^<]* b/],
    ["a *it* b", "a *it* b", /<em>it<\/em>[^<]* b/],
    ["a __bold__ b", "a **bold** b", /<strong>bold<\/strong>[^<]* b/],
    ["a _it_ b", "a *it* b", /<em>it<\/em>[^<]* b/],
    ["a ~~gone~~ b", "a ~~gone~~ b", /<s>gone<\/s>[^<]* b/],
    ["[the site](https://example.com) x", "[the site](https://example.com) x", /<a href="https:\/\/example.com">the site<\/a>[^<]* x/],
  ])
    await typed(
      `composed inline ${src}`,
      async () => {
        // Words as words, everything else a character at a time.
        for (const part of src.match(/[A-Za-z]+|[^A-Za-z]/g)) {
          if (/^[A-Za-z]+$/.test(part)) await word(part);
          else await cc(part);
        }
      },
      md,
      (h) => want.test(h) && !/<(strong|em|s|code)>[^<]* b<\//.test(h),
    );

  // 3. A word committed whole (swipe typing, autocorrect suggestion) and then
  // a composed space: the rule reads the text before the caret.
  await typed("swiped word then composed space stays literal mid-line", async () => {
    await swipe("hello");
    await cc("# ");
  }, "hello#", (h) => !/<h1/.test(h));
  await typed("composed # then a swiped word after the space", async () => {
    await cc("# ");
    await swipe("hello");
  }, "# hello", /<h1>hello<\/h1>/);
  await typed("one composition committing \"# \" converts", async () => {
    await comp("# ");
    await commit("# ");
    await word("Title");
  }, "# Title", /<h1>Title<\/h1>/);
  await typed("a word then a composition ending in the closing backtick", async () => {
    await cc("`");
    await swipe("code");
    await cc("`");
    await cc(" x");
  }, "`code` x", /<code>code<\/code>[^<]* x/);

  // 4. Nothing changes while a composition is active.
  {
    const issue = await make("mid-composition");
    await page.goto(`${base}/issue/${issue.key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
    await page.getByRole("button", { name: "Add a description…" }).click();
    await richBox.waitFor();
    await cc("#");
    await comp(" ");
    let h = await html();
    ok(/^<p>#(&nbsp;| )<\/p>$/.test(h), `${tag} the space is still composing: no heading yet (${h})`);
    await commit(" ");
    h = await html();
    ok(/<h1><br><\/h1>/.test(h), `${tag} the heading appears once the composition ends (${h})`);
    // A closing backtick still composing does not convert.
    await word("x");
    await aEnter();
    await cc("`");
    await swipe("a");
    await comp("`");
    h = await html();
    ok(!/<code>/.test(h) && /`a`/.test(h), `${tag} an open composition ending in a backtick leaves the text alone (${h})`);
    await commit("`");
    h = await html();
    ok(/<code>a<\/code>/.test(h), `${tag} the code appears once it is committed (${h})`);
    // A candidate window cycling through text: never a rule, never a change
    // outside the composing text.
    await aEnter();
    for (const t of ["n", "ni", "ni h", "你好"]) await comp(t);
    h = await html();
    ok((h.match(/<h\d/g) ?? []).length === 1 && !/<ul|<blockquote/.test(h) && h.endsWith("你好</p>"), `${tag} candidates replace each other in place (${h})`);
    await commit("你好");
    await cc("＃ ");
    h = await html();
    ok(!/<h1[^>]*>[^<]*<\/h1>$/.test(h) && /＃/.test(h) && h.endsWith("</p>"), `${tag} a full-width ＃ is not a heading marker (${h})`);
    await done.click();
    await richBox.waitFor({ state: "detached" });
  }

  // 5. A composition that is cancelled leaves nothing and converts nothing.
  await typed("cancelled composition", async () => {
    await comp("#");
    await comp("");
    await cc("x");
  }, "x", (h) => !/<h1/.test(h));

  // 6. Backspace by beforeinput.
  await typed("backspace by beforeinput turns a heading back into a paragraph", async () => {
    await type("# ");
    await aBack();
    await word("x");
  }, "x", /^<p>x<\/p>$/);
  await typed("backspace by beforeinput removes the box of a task", async () => {
    await type("[ ] t");
    await press("Home");
    await press("ArrowRight");
    await press("ArrowRight");
    await aBack();
  }, "- t", (h) => !/<input/.test(h));
  await typed("backspace by beforeinput at the start of a code block", async () => {
    await type("```");
    await press("Enter");
    await aBack();
    await word("x");
  }, "x", (h) => !/<pre/.test(h));
  await typed("backspace by beforeinput at the start of a quote", async () => {
    await type("> ");
    await aBack();
    await word("x");
  }, "x", (h) => !/<blockquote/.test(h));
  await typed("backspace by beforeinput after inline code deletes one visible character", async () => {
    await type("`ab`");
    await type("cd");
    await aBack();
    await aBack();
    await aBack();
    await cc("X");
  }, "`aX`", (h) => /<code>aX<\/code>/.test(h) || /<code>a<\/code>[^<]*X/.test(h));
  await typed("backspace by beforeinput in an empty task leaves an empty bullet item", async () => {
    await type("[ ] ");
    await aBack();
    await type("x");
  }, "- x", /^<ul><li>x<\/li><\/ul>$/);
  await typed("a plain backspace by beforeinput still deletes a character", async () => {
    await word("abc");
    await aBack();
  }, "ab", /^<p>ab<\/p>$/);

  // 6b. Enter by beforeinput after markdown typed on a desktop keyboard: the
  // Enter is the only thing that differs from suite 85.
  await typed("Enter by beforeinput after a typed fence", async () => {
    await type("```");
    await aEnter();
    await type("x");
    await aEnter();
    await aEnter();
    await aEnter();
    await type("after");
  }, "```\nx\n```\n\nafter", /^<pre><code>x(<br>)?<\/code><\/pre><p>after<\/p>$/);
  await typed("Enter by beforeinput after a typed rule", async () => {
    await type("---");
    await aEnter();
    await type("below");
  }, "---\n\nbelow", /^<hr><p>below<\/p>$/);
  await typed("Enter by beforeinput in a typed task list", async () => {
    await type("- [ ] a");
    await aEnter();
    await type("b");
    await aEnter();
    await aEnter();
    await type("c");
  }, "- [ ] a\n- [ ] b\n\nc", (h) => (h.match(/<input/g) ?? []).length === 2 && /<\/ul><p>c<\/p>$/.test(h));
  await typed("Enter by beforeinput leaves a typed quote and heading", async () => {
    await type("> q");
    await aEnter();
    await aEnter();
    await type("out");
    await aEnter();
    await type("###### h");
    await aEnter();
    await type("next");
  }, "> q\n\nout\n\n###### h\n\nnext", /<blockquote><p>q<\/p><\/blockquote><p>out<\/p><h6>h<\/h6><p>next<\/p>/);

  // 6c. Enter by beforeinput with the mention menu open picks the highlighted
  // person and inserts no paragraph, as Enter on a desktop does.
  {
    const menu = page.getByRole("listbox", { name: "Mention someone" });
    for (const [name, steps] of [
      ["typed", async () => type("hi @rob")],
      [
        "composed",
        async () => {
          await word("hi");
          await cc(" @");
          await word("rob");
        },
      ],
    ]) {
      const issue = await make(`mention ${name}`);
      await page.goto(`${base}/issue/${issue.key}`);
      await page.getByRole("tab", { name: "Details" }).click({ timeout: 2000 }).catch(() => {});
      await page.getByRole("button", { name: "Add a description…" }).click();
      await richBox.waitFor();
      await steps();
      await menu.waitFor();
      await aEnter();
      const h = await html();
      ok(/@robin/.test(h) && (h.match(/<p>/g) ?? []).length === 1, `${tag} ${name}: Enter by beforeinput picks the mention, no new paragraph (${h})`);
      ok(!(await menu.isVisible()), `${tag} ${name}: the menu closed after the pick`);
      await done.click();
      await richBox.waitFor({ state: "detached" });
      const got = await waitDesc(issue.key, "hi @robin");
      ok(/^hi @robin\s*$/.test(got ?? ""), `${tag} ${name}: saved ${JSON.stringify(got)}`);
    }
  }

  // 7. Multi-character inserts that are not compositions stay literal.
  for (const [src, md] of [
    ["# ", "\\#"],
    ["- ", "-"],
    ["> ", ">"],
    ["1. ", "1\\."],
    ["[ ] ", "\\[ \\]"],
    ["a `b`", "a \\`b\\`"],
    ["**b**", "\\*\\*b\\*\\*"],
    ["a ~~b~~", "a \\~\\~b\\~\\~"],
  ])
    await typed(
      `inserted ${JSON.stringify(src)} stays literal`,
      async () => {
        // Input.insertText with no composition: inputType insertText, many
        // characters, as a drop or an autocorrect that replaces text arrive.
        await page.keyboard.insertText(src);
      },
      null,
      (h) => !/<(h\d|ul|ol|strong|em|s|code|blockquote|input)\b/.test(h),
    );
  await typed("inserted fence stays literal", async () => {
    await page.keyboard.insertText("```");
    await word("x");
  }, null, (h) => !/<pre/.test(h));
  // 8. Undo after a composed conversion gives the typed text back.
  await typed("undo a composed heading", async () => {
    await cc("# ");
    await press("Control+z");
    const t = await html();
    ok(/^<p>#(&nbsp;| )<\/p>$/.test(t), `${tag} undo after a composed "# " is the literal text (${t})`);
    await press("End");
    await type("x");
  }, "\\# x", /<p>#[^<]*x<\/p>/);
  await typed("undo composed inline code", async () => {
    await word("a");
    await cc(" `code`");
    await press("Control+z");
    const t = await richBox.evaluate((el) => el.innerText.replace(/﻿/g, ""));
    ok(t.trim() === "a `code`", `${tag} undo after a composed closing backtick is the literal text (${JSON.stringify(t)})`);
  }, null, (h) => !/<code>/.test(h));

  // 9. Desktop typing converts once.
  await typed("desktop heading", async () => {
    await type("# Title");
    await press("Enter");
    await type("plain");
  }, "# Title\n\nplain", /^<h1>Title<\/h1><p>plain<\/p>$/);
  await typed("desktop bold", async () => {
    await type("a **b** c");
  }, "a **b** c", (h) => (h.match(/<strong>/g) ?? []).length === 1 && /<strong>b<\/strong>/.test(h));
  await typed("desktop fence and Enter once", async () => {
    await type("```");
    await press("Enter");
    await type("x");
    await press("Enter");
    await press("Enter");
    await press("Enter");
    await type("after");
  }, "```\nx\n```\n\nafter", /^<pre><code>x(<br>)?<\/code><\/pre><p>after<\/p>$/);
  await typed("desktop rule and Enter once", async () => {
    await type("---");
    await press("Enter");
    await type("below");
  }, "---\n\nbelow", /^<hr><p>below<\/p>$/);
  await typed("desktop task Enter makes one item", async () => {
    await type("- [ ] a");
    await press("Enter");
    await type("b");
  }, "- [ ] a\n- [ ] b", (h) => (h.match(/<input/g) ?? []).length === 2 && (h.match(/<li>/g) ?? []).length === 2);
  await typed("desktop heading Backspace once", async () => {
    await type("# ab");
    await press("Home");
    await press("Backspace");
    await press("End");
    await type("c");
  }, "abc", /^<p>abc<\/p>$/);
  await typed("desktop Shift+Enter in a paragraph is not a rule", async () => {
    await type("a");
    await press("Shift+Enter");
    await type("b");
  }, null, (h) => /a<br>b|<p>a<\/p><p>b<\/p>/.test(h));

  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("no errors");
