// Tab and Shift+Tab nest and un-nest a list item in the rich editor (WP-1765):
// bullets, numbers and tasks, one level at a time; Enter on an empty nested item
// and Backspace at its start un-nest it; undo reverses one Tab; and the saved
// markdown (and a reopened one) shows the same nesting. RICH_TYPING_BROWSER=
// firefox|webkit runs it under that engine (engine.mjs); the rules are on only
// where richdom.ts SUPPORTED says (WP-1758).
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
// Milliseconds between key presses; the same value 85-rich-typing found.
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

// Saved markdown with three levels, and what it is after "!" is typed at the
// end of the deepest item: the same, so the round trip keeps the nesting.
const nestedMd = {
  bullets: ["- a\n  - b\n    - deep\n- d", "- a\n  - b\n    - deep!\n- d"],
  numbers: ["1. a\n   1. b\n      1. deep\n2. d", "1. a\n   1. b\n      1. deep!\n2. d"],
  mixed: ["- a\n  1. b\n     - deep\n  2. c\n- d", "- a\n  1. b\n     - deep!\n  2. c\n- d"],
  tasks: [
    "- [ ] a\n  - [x] b\n    - [ ] deep\n- [x] d",
    "- [ ] a\n  - [x] b\n    - [ ] deep!\n- [x] d",
  ],
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
  const type = (s) => page.keyboard.type(s, { delay: KEY_DELAY });
  const press = (k) => page.keyboard.press(k);
  const html = () => richBox.evaluate((el) => el.innerHTML);
  const focused = () => richBox.evaluate((el) => el === document.activeElement);

  const openNew = async (name) => {
    const issue = await make(name, "");
    await page.goto(`${base}/issue/${issue.key}`);
    await page.getByRole("button", { name: "Add a description…" }).click();
    await richBox.waitFor();
    return issue;
  };
  // Runs `steps` in a new task's editor, then Done, and checks the saved markdown.
  const typed = async (name, steps, md) => {
    const issue = await openNew(name);
    await steps();
    const h = await html();
    await done.click();
    const got = await waitDesc(issue.key, md);
    ok(
      got === md,
      `${tag} ${name}: saved ${JSON.stringify(md)}${got === md ? "" : ` got ${JSON.stringify(got)} (${h.replace(/​/g, "·").slice(0, 200)})`}`,
    );
    return issue;
  };
  // The caret as the text node it is in and the offset.
  const caret = () =>
    page.evaluate(() => ({ text: getSelection().anchorNode.data, at: getSelection().anchorOffset }));
  // "- a", then "b" nested under it, caret at the end of b.
  const aThenNestedB = async () => {
    await type("- a");
    await press("Enter");
    await type("b");
    await press("Tab");
  };

  // 1. Tab nests under the previous item, keeping the list type.
  await typed(
    "tab nests a bullet",
    async () => {
      await aThenNestedB();
      ok(await focused(), `${tag} the editor keeps focus after Tab in a list`);
    },
    "- a\n  - b",
  );
  await typed(
    "tab nests a number",
    async () => {
      await type("1. a");
      await press("Enter");
      await type("b");
      await press("Tab");
    },
    "1. a\n   1. b",
  );
  await typed(
    "tab nests a task",
    async () => {
      await type("- [ ] a");
      await press("Enter");
      await type("b");
      await press("Tab");
    },
    "- [ ] a\n  - [ ] b",
  );
  await typed(
    "tab nests a checked task",
    async () => {
      await type("- a");
      await press("Enter");
      await type("[x] b");
      await press("Tab");
    },
    "- a\n  - [x] b",
  );
  await typed(
    "tab twice nests two levels",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await type("c");
      await press("Tab");
    },
    "- a\n  - b\n    - c",
  );
  await typed(
    "tab nests every selected item",
    async () => {
      await type("- a");
      await press("Enter");
      await type("b");
      await press("Enter");
      await type("c");
      await press("Shift+ArrowUp");
      await press("Tab");
    },
    "- a\n  - b\n  - c",
  );
  await typed(
    "tab nests under the previous sibling",
    async () => {
      await type("- a");
      await press("Enter");
      await type("b");
      await press("Enter");
      await type("c");
      await press("Tab");
    },
    "- a\n- b\n  - c",
  );

  // 2. Tab where nothing can nest is swallowed; elsewhere it is the browser's.
  await typed(
    "tab in the first item does nothing",
    async () => {
      await type("- a");
      await press("Tab");
      ok(await focused(), `${tag} focus stays in the editor after Tab in a first item`);
      await type("x");
    },
    "- ax",
  );
  await typed(
    "tab outside a list leaves the editor",
    async () => {
      await type("para");
      await press("Tab");
      ok(!(await focused()), `${tag} Tab in a paragraph moves focus out of the editor`);
    },
    "para",
  );
  {
    await openNew("tab in code");
    await type("```");
    await press("Enter");
    await type("a");
    await press("Tab");
    // The code is "a" and two spaces, no tab character, and the caret after them.
    const code = await richBox.evaluate((el) => {
      const c = el.querySelector("pre code");
      const s = getSelection();
      const at = s.rangeCount ? s.getRangeAt(0).cloneRange() : null;
      if (at && c) at.setStart(c, 0);
      return { text: c?.textContent.replace(/\u00a0/g, " ").replace(/\ufeff/g, ""), before: at && c?.contains(at.endContainer) ? at.toString().replace(/\u00a0/g, " ").replace(/\ufeff/g, "") : null };
    });
    ok(/^a {2}\n?$/.test(code.text ?? "") && code.before === "a  " && (await focused()), `${tag} Tab in a code block inserts two spaces (${JSON.stringify(code)})`);
    await done.click();
    await richBox.waitFor({ state: "detached" });
  }

  // 3. Shift+Tab un-nests one level.
  await typed(
    "shift+tab un-nests a bullet",
    async () => {
      await aThenNestedB();
      await press("Shift+Tab");
    },
    "- a\n- b",
  );
  await typed(
    "shift+tab un-nests one level of two",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await type("c");
      await press("Tab");
      await press("Shift+Tab");
    },
    "- a\n  - b\n  - c",
  );
  await typed(
    "shift+tab un-nests a number",
    async () => {
      await type("1. a");
      await press("Enter");
      await type("b");
      await press("Tab");
      await press("Shift+Tab");
    },
    "1. a\n2. b",
  );
  await typed(
    "shift+tab keeps a task's box",
    async () => {
      await type("- [ ] a");
      await press("Enter");
      await type("b");
      await press("Tab");
      await press("Shift+Tab");
    },
    "- [ ] a\n- [ ] b",
  );
  // What Chromium's outdent does with the items after the one lifted out.
  await typed(
    "shift+tab on a middle sub-item",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await type("c");
      await press("Enter");
      await type("d");
      await press("ArrowUp");
      await press("Shift+Tab");
    },
    "- a\n  - b\n- c\n  - d",
  );
  await typed(
    "shift+tab at the top level does nothing",
    async () => {
      await type("- a");
      await press("Shift+Tab");
      ok(await focused(), `${tag} focus stays after Shift+Tab at the top level`);
      await type("x");
    },
    "- ax",
  );

  // 4. Enter and Backspace un-nest.
  await typed(
    "enter on an empty nested item un-nests it",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await press("Enter");
      await type("c");
    },
    "- a\n  - b\n- c",
  );
  await typed(
    "enter on an empty item two deep un-nests one level",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await type("c");
      await press("Tab");
      await press("Enter");
      await press("Enter");
      await type("d");
    },
    "- a\n  - b\n    - c\n  - d",
  );
  await typed(
    "enter on an empty top-level item still ends the list",
    async () => {
      await type("- a");
      await press("Enter");
      await press("Enter");
      await type("x");
    },
    "- a\n\nx",
  );
  await typed(
    "backspace at the start of a nested item un-nests it",
    async () => {
      await aThenNestedB();
      await press("Home");
      await press("Backspace");
      await type("c");
    },
    "- a\n- cb",
  );
  await typed(
    "backspace at the start of a nested task un-nests it first",
    async () => {
      await type("- [ ] a");
      await press("Enter");
      await type("b");
      await press("Tab");
      await press("Home");
      await press("Backspace");
      await type("c");
    },
    "- [ ] a\n- [ ] cb",
  );

  // 5. The caret stays where it was in the text.
  await typed(
    "caret stays through tab and shift+tab",
    async () => {
      await type("- a");
      await press("Enter");
      await type("bcde");
      await press("ArrowLeft");
      await press("ArrowLeft");
      await press("Tab");
      let c = await caret();
      ok(c.text === "bcde" && c.at === 2, `${tag} caret after Tab is at 2 of bcde (${JSON.stringify(c)})`);
      await press("Shift+Tab");
      c = await caret();
      ok(c.text === "bcde" && c.at === 2, `${tag} caret after Shift+Tab is at 2 of bcde (${JSON.stringify(c)})`);
      await type("X");
    },
    "- a\n- bcXde",
  );

  // 6. Undo reverses one Tab.
  await typed(
    "undo reverses a tab",
    async () => {
      await aThenNestedB();
      await press("Control+z");
    },
    "- a\n- b",
  );
  await typed(
    "undo reverses one of two tabs",
    async () => {
      await aThenNestedB();
      await press("Enter");
      await type("c");
      await press("Tab");
      await press("Control+z");
    },
    "- a\n  - b\n  - c",
  );
  await typed(
    "undo reverses a shift+tab",
    async () => {
      await aThenNestedB();
      await press("Shift+Tab");
      await press("Control+z");
    },
    "- a\n  - b",
  );

  // 7. Escape leaves the editor from inside a list, with no mouse.
  {
    const issue = await openNew("escape in a list");
    await aThenNestedB();
    await press("Escape");
    await richBox.waitFor({ state: "detached" });
    const got = await waitDesc(issue.key, "- a\n  - b");
    ok(got === "- a\n  - b", `${tag} Escape in a nested list saves and closes the editor (${JSON.stringify(got)})`);
  }

  // 8. A saved nested list opens in the rich editor, nested, and saves the same.
  for (const [kind, [md, edited]] of Object.entries(nestedMd)) {
    const issue = await make(`reopen ${kind}`, md);
    await page.goto(`${base}/issue/${issue.key}`);
    const view = page.locator('.prose-wp[title="Click or press Enter to edit"]').first();
    await view.waitFor();
    const spot = await view.evaluate((el) => {
      const li = Array.from(el.querySelectorAll("li")).find((l) =>
        Array.from(l.childNodes).some((n) => n.nodeType === 3 && n.data.includes("deep")),
      );
      const t = Array.from(li.childNodes).find((n) => n.nodeType === 3 && n.data.includes("deep"));
      const r = document.createRange();
      r.setStart(t, t.data.indexOf("deep"));
      r.setEnd(t, t.data.indexOf("deep") + 4);
      const b = r.getBoundingClientRect();
      return { x: b.left + b.width / 2, y: b.top + b.height / 2 };
    });
    await page.mouse.click(spot.x, spot.y);
    await richBox.waitFor();
    const depth = await richBox.evaluate((el) => {
      let max = 0;
      for (const li of el.querySelectorAll("li")) {
        let d = 0;
        for (let n = li.parentElement; n && n !== el; n = n.parentElement)
          if (n.tagName === "UL" || n.tagName === "OL") d++;
        max = Math.max(max, d);
      }
      return max;
    });
    ok(depth === 3, `${tag} ${kind}: opens three deep in the rich editor, not in markdown mode (depth ${depth})`);
    await press("End");
    await type("!");
    await done.click();
    const got = await waitDesc(issue.key, edited);
    ok(got === edited, `${tag} ${kind}: edited and saved ${JSON.stringify(edited)}${got === edited ? "" : ` got ${JSON.stringify(got)}`}`);
  }

  // 9. A nested list sits inside the editor box, at any width.
  {
    await openNew("nested fits");
    await type("- a very long first item that goes on and on and wraps around at a narrow width");
    await press("Enter");
    await type("second");
    await press("Tab");
    await press("Enter");
    await type("third");
    await press("Tab");
    const fit = await richBox.evaluate((el) => {
      const box = el.getBoundingClientRect();
      const item = (t) => Array.from(el.querySelectorAll("li")).find((l) => l.textContent.startsWith(t));
      const li = item("third");
      const r = li.getBoundingClientRect();
      // Lists around the item, up to the editor: three for an item nested twice.
      let depth = 0;
      for (let n = li.parentElement; n && n !== el; n = n.parentElement) if (n.tagName === "UL" || n.tagName === "OL") depth++;
      const lefts = ["a very long", "second", "third"].map((t) => item(t)?.getBoundingClientRect().left);
      return { box: [box.left, box.right], li: [r.left, r.right], depth, lefts };
    });
    ok(fit.depth === 3 && fit.lefts[0] < fit.lefts[1] && fit.lefts[1] < fit.lefts[2], `${tag} the third item is nested twice, each level further in (${JSON.stringify(fit)})`);
    ok(
      fit.li[0] > fit.box[0] && fit.li[1] <= fit.box[1] + 0.5,
      `${tag} the twice-nested item is inside the editor box (${JSON.stringify(fit)})`,
    );
    await page.screenshot({ path: `${out}/nested-${tag}.png` });
    await press("Escape");
  }
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("no errors");
