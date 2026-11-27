// Wikilinks, @mentions and issue references look the same in the rich editor
// as in the description view, so clicking to edit shifts nothing (WP-1763,
// WP-1749). Run after 01-signup-flow (user robin). RICH_TYPING_BROWSER=
// firefox|webkit runs it under that engine (engine.mjs).
import { launchEngine } from "./engine.mjs";
import fs from "node:fs";
import { looks } from "./looks.mjs";
import { shape, navigationAbort, caretToStart } from "./editor.mjs";
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
  const projects = await (await page.request.get(base + "/api/v1/projects", { headers: H })).json();
  if (!projects.some((p) => p.key === "RICH"))
    await page.request.post(base + "/api/v1/projects", { headers: H, data: { name: "Rich editor", key: "RICH" } });
  await ctx.close();
}

const WORDS = "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty".split(" ");
const STEPS = 24;
// One paragraph per amount of leading text, so some of them have the piece
// close to where the line wraps at any width. The marker word follows it.
const sweep = (kind, piece) =>
  Array.from({ length: STEPS }, (_, i) => {
    const lead = Array.from({ length: 4 + i }, (_, k) => WORDS[(k + i) % WORDS.length]).join(" ");
    return `${lead} ${piece} mk${kind}${i}x then enough words after it to fill the end of the line and a bit more.`;
  }).join("\n\n");

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
  page.on("pageerror", (e) => navigationAbort(e.message) || fail(`${tag} pageerror: ${e.message}`));
  const make = async (title, description) =>
    (
      await page.request.post(base + "/api/v1/issues", {
        headers: H,
        data: { project: "RICH", title: `${title} ${tag}`, description: description || undefined },
      })
    ).json();
  const richBox = page.locator('[contenteditable="true"][aria-label="Description"]');
  const viewSel = '.prose-wp[title="Click or press Enter to edit"]';

  const ref = await make("Referenced", "Referenced.");
  const pieces = {
    wikilink: "[[Some Page]]",
    mention: "@robin",
    issue: ref.key,
  };
  // Where the marker word of each paragraph is: the first client rect of its text,
  // from its paragraph's top so one paragraph reflowing does not blame the next.
  const markers = () =>
    page.evaluate(() => {
      const root =
        document.querySelector('[contenteditable="true"][aria-label="Description"]') ??
        document.querySelector('.prose-wp[title="Click or press Enter to edit"]');
      const out = {};
      const dump = {};
      for (const p of root.querySelectorAll("p")) {
        const m = /mk([a-z]+\d+)x/.exec(p.textContent);
        if (!m) continue;
        const walk = document.createTreeWalker(p, NodeFilter.SHOW_TEXT);
        for (let t = walk.nextNode(); t; t = walk.nextNode()) {
          const at = t.data.indexOf(m[0]);
          if (at < 0) continue;
          const r = document.createRange();
          r.setStart(t, at);
          r.setEnd(t, at + m[0].length);
          const q = r.getClientRects()[0];
          const top = p.getBoundingClientRect().top;
          out[m[1]] = [Math.round(q.left * 100) / 100, Math.round((q.top - top) * 100) / 100, Math.round(p.getBoundingClientRect().height * 100) / 100];
          break;
        }
        // Where each line breaks, to read a shift without guessing.
        let line = "";
        let last = null;
        const w2 = document.createTreeWalker(p, NodeFilter.SHOW_TEXT);
        for (let t = w2.nextNode(); t; t = w2.nextNode())
          for (const mm of t.data.matchAll(/\S+|\s+/g)) {
            const rr = document.createRange();
            rr.setStart(t, mm.index);
            rr.setEnd(t, mm.index + mm[0].length);
            const q2 = rr.getClientRects()[0];
            if (q2 && last !== null && Math.abs(q2.top - last) > 2) line += "|";
            if (q2) last = q2.top;
            line += mm[0].replace(/\ufeff/g, "~").replace(/\u00a0/g, "_");
          }
        dump[m[1]] = line + "\n      html: " + p.innerHTML.replace(/\u00a0/g, "_").replace(/\ufeff/g, "~").slice(-260);
      }
      const b = root.getBoundingClientRect();
      return { markers: out, dump, box: [b.left, b.top, b.width, b.height] };
    });

  const description = Object.entries(pieces).map(([k, p]) => sweep(k, p)).join("\n\n");
  const issue = await make("Inline refs", description);
  await page.goto(`${base}/issue/${issue.key}`);
  await page.getByRole("tab", { name: "Details" }).click({ timeout: 15000 }).catch(() => {});
  const view = page.locator(viewSel).first();
  await view.waitFor();
  // The reference becomes a link once the server has resolved it.
  await view.locator(`a[href="/issue/${ref.key}"]`).first().waitFor();
  await page.evaluate(() => document.fonts.ready);
  const before = await markers();
  ok(Object.keys(before.markers).length === 3 * STEPS, `${tag} the view shows every marker (${Object.keys(before.markers).length})`);
  const word = await view.evaluate((el) => {
    const p = el.querySelector("p");
    const t = p.firstChild;
    const r = document.createRange();
    r.setStart(t, 0);
    r.setEnd(t, 3);
    const q = r.getBoundingClientRect();
    return { x: q.left + q.width / 2, y: q.top + q.height / 2 };
  });
  await page.mouse.click(word.x, word.y);
  await richBox.waitFor();
  const after = await markers();
  // Playwright's WebKit places a text run at a whole pixel and a link or piece at a
  // fraction: text after one sits within a pixel of where the view put it
  // (measured: the view's reference starts at x 291.52, the editor's same text at
  // 291.00, the text after both at 340.00). Chromium matches to 0.5.
  const dxTol = process.env.RICH_TYPING_BROWSER === "webkit" ? 1.01 : 0.5;
  for (const kind of Object.keys(pieces)) {
    const rows = [];
    for (let i = 0; i < STEPS; i++) {
      const a = before.markers[kind + i];
      const b = after.markers[kind + i];
      if (!a || !b) {
        rows.push(`#${i} missing`);
        continue;
      }
      if (Math.abs(a[0] - b[0]) > dxTol || Math.abs(a[1] - b[1]) > 0.5 || Math.abs(a[2] - b[2]) > 0.5)
        rows.push(`#${i} dx ${(b[0] - a[0]).toFixed(1)} dy ${(b[1] - a[1]).toFixed(1)} dh ${(b[2] - a[2]).toFixed(1)}`);
    }
    ok(rows.length === 0, `${tag} text after a ${kind} stays put when the editor opens: ${rows.length} of ${STEPS} moved${rows.length ? ` (${rows.slice(0, 6).join("; ")})` : ""}`);
  }
  ok(JSON.stringify(before.box) === JSON.stringify(after.box), `${tag} the description box is the same (${before.box} -> ${after.box})`);
  await page.screenshot({ path: `${out}/inline-${tag}.png` });
  await page.keyboard.press("Escape");

  // Behaviour of the pieces in the editor, each on a small description.
  const desc = async (key) =>
    (await (await page.request.get(base + `/api/v1/issues/${key}`, { headers: H })).json()).description;
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
  const done = page.getByRole("button", { name: "Done", exact: true });
  const openSmall = async (title, text) => {
    const i = await make(title, text);
    await page.goto(`${base}/issue/${i.key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 15000 }).catch(() => {});
    const v = page.locator(viewSel).first();
    await v.waitFor();
    const at = await v.evaluate((el) => {
      const r = document.createRange();
      const t = el.querySelector("p").firstChild;
      r.setStart(t, 0);
      r.setEnd(t, 1);
      const q = r.getBoundingClientRect();
      return { x: q.left + q.width / 2, y: q.top + q.height / 2 };
    });
    await page.mouse.click(at.x, at.y);
    await richBox.waitFor();
    // ProseMirror re-applies a stale selection a moment after the first key
    // (WebKit most); settle it before the checks press keys.
    await caretToStart(richBox);
    return i;
  };
  // The text of the first paragraph up to the caret (PARK dropped).
  const caretText = () =>
    page.evaluate(() => {
      const s = getSelection();
      const n = s.anchorNode;
      const p = (n.nodeType === 3 ? n.parentElement : n).closest("p");
      const r = document.createRange();
      r.setStart(p, 0);
      r.setEnd(n, s.anchorOffset);
      return r.toString().replace(/﻿/g, "");
    });
  const pieceCount = () => richBox.evaluate((el) => el.querySelectorAll("[data-md]").length);
  const type = (s) => page.keyboard.type(s, { delay: 12 });
  const press = (k) => page.keyboard.press(k);

  // Round trip: opening and closing changes nothing; typing elsewhere keeps every piece.
  const src = "See [[Some Page]], [[Target|shown label]] and (@robin). Also **bold @robin** and `@code [[x]]` ok";
  {
    const i = await openSmall("Round trip", src);
    const n = await pieceCount();
    ok(n === 4, `${tag} the editor holds the pieces as uneditable elements (${n} of 4)`);
    const html = await shape(richBox);
    ok(/data-md="\[\[Target\|shown label\]\]"/.test(html), `${tag} each piece carries its markdown (${html.slice(0, 200)})`);
    await press("Escape");
    await richBox.waitFor({ state: "detached" });
    ok((await desc(i.key)) === src, `${tag} open and close without a change leaves the markdown byte-identical`);
  }
  {
    const i = await openSmall("Type elsewhere", src);
    // Control+End: at 390 wide End goes to the end of the line, not of the text.
    await press("Control+End");
    await type("!");
    await done.click();
    const got = await waitDesc(i.key, src + "!");
    ok(got === src + "!", `${tag} typing after the pieces saves them as they were: ${JSON.stringify(got)}`);
  }

  // Caret before and after a piece: arrows, then clicks.
  for (const [name, piece, text] of [
    ["wikilink", "[[Some Page]]", "Some Page"],
    ["mention", "@robin", "@robin"],
  ]) {
    await openSmall(`Caret ${name}`, `ab ${piece} cd`);
    await press("Home");
    for (let k = 0; k < 3; k++) await press("ArrowRight");
    const beforePiece = await caretText();
    ok(beforePiece === "ab ", `${tag} ${name}: three Right arrows reach the spot before it (${JSON.stringify(beforePiece)})`);
    await press("ArrowRight");
    const afterPiece = await caretText();
    ok(afterPiece === `ab ${text}`, `${tag} ${name}: one more steps over it (${JSON.stringify(afterPiece)})`);
    await press("ArrowLeft");
    const back = await caretText();
    ok(back === "ab ", `${tag} ${name}: Left steps back before it (${JSON.stringify(back)})`);
    // Text typed beside it goes outside it.
    await type("X");
    await press("ArrowRight");
    await type("Y");
    const inside = await richBox.evaluate((el) => el.querySelector("[data-md]")?.textContent);
    ok(inside === text, `${tag} ${name}: text typed beside it stays outside (${JSON.stringify(inside)})`);
    await press("Escape");
    // By click: on its left edge, then its right edge.
    await openSmall(`Click ${name}`, `ab ${piece} cd`);
    const box = await richBox.locator("[data-md]").boundingBox();
    await page.mouse.click(box.x + 2, box.y + box.height / 2);
    const l = await caretText();
    ok(l === "ab ", `${tag} ${name}: a click on its left edge puts the caret before it (${JSON.stringify(l)})`);
    await page.mouse.click(box.x + box.width - 2, box.y + box.height / 2);
    const r = await caretText();
    ok(r === `ab ${text}`, `${tag} ${name}: a click on its right edge puts the caret after it (${JSON.stringify(r)})`);
    await press("Escape");
  }

  // Backspace right after a piece deletes it whole; one undo restores it.
  for (const [name, piece, text] of [
    ["wikilink", "[[Some Page]]", "Some Page"],
    ["mention", "@robin", "@robin"],
  ]) {
    await openSmall(`Backspace ${name}`, `ab ${piece} cd`);
    await press("Home");
    for (let k = 0; k < 4; k++) await press("ArrowRight");
    const at = await caretText();
    ok(at === `ab ${text}`, `${tag} ${name}: caret is right after it (${JSON.stringify(at)})`);
    await press("Backspace");
    const gone = await richBox.evaluate((el) => el.textContent);
    ok((await pieceCount()) === 0 && gone.replace(/\s+/g, " ").trim() === "ab cd", `${tag} ${name}: Backspace deletes the whole piece (${JSON.stringify(gone)})`);
    await press("Control+z");
    const restored = await richBox.evaluate((el) => el.textContent);
    ok((await pieceCount()) === 1 && restored.trim() === `ab ${text} cd`, `${tag} ${name}: one undo restores it (${JSON.stringify(restored)})`);
    await press("Escape");
  }

  // New [[x]] and @name typed in the editor are literal markdown, and render once saved.
  {
    const i = await openSmall("Typed refs", "Start");
    await press("End");
    await type(" [[Fresh Page]] @robin and done");
    await done.click();
    const want = "Start [[Fresh Page]] @robin and done";
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} typed [[x]] and @name save as literal markdown: ${JSON.stringify(got)}`);
    await page.goto(`${base}/issue/${i.key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 15000 }).catch(() => {});
    const v = page.locator(viewSel).first();
    await v.waitFor();
    const links = (await v.locator("a.wikilink").allTextContents()).join();
    const mentions = (await v.locator(".mention").allTextContents()).join();
    ok(links === "Fresh Page" && mentions === "@robin", `${tag} they render as a wikilink and a mention once saved (${links} / ${mentions})`);
  }

  // The mention menu opens after a piece, and picking writes the mention.
  {
    const i = await openSmall("Menu", "x [[Some Page]]");
    await press("End");
    await type(" @ro");
    const menu = page.getByRole("listbox", { name: "Mention someone" });
    const opened = await menu.waitFor({ timeout: 5000 }).then(() => true, () => false);
    ok(opened && (await menu.getByRole("option").count()) >= 1, `${tag} the mention menu opens after a piece`);
    if (opened) await press("Enter");
    await done.click();
    const want = "x [[Some Page]] @robin";
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} picking from the menu writes the mention: ${JSON.stringify(got)}`);
  }

  // Saving shifts nothing either: a mention picked from the menu and a
  // wikilink typed by hand are pieces at once, so the editor and the view
  // that replaces it put the text after them in the same place.
  {
    const N = 12;
    const kinds = ["menu", "typed"];
    const text = kinds
      .flatMap((k) =>
        Array.from({ length: N }, (_, i) => {
          const lead = Array.from({ length: 4 + i }, (_, j) => WORDS[(j + i) % WORDS.length]).join(" ");
          return `${lead} HERE mk${k}${i}x then enough words after it to fill the end of the line and a bit more.`;
        }),
      )
      .join("\n\n");
    const i = await openSmall("Save shifts nothing", text);
    const menu = page.getByRole("listbox", { name: "Mention someone" });
    for (const k of kinds)
      for (let n = 0; n < N; n++) {
        await richBox.evaluate(
          (el, id) => {
            const p = Array.from(el.querySelectorAll("p")).find((x) => x.textContent.includes(id));
            const walk = document.createTreeWalker(p, NodeFilter.SHOW_TEXT);
            for (let t = walk.nextNode(); t; t = walk.nextNode()) {
              const at = t.data.indexOf("HERE");
              if (at < 0) continue;
              getSelection().setBaseAndExtent(t, at, t, at + 4);
              return;
            }
          },
          `mk${k}${n}x`,
        );
        if (k === "menu") {
          await type("@ro");
          await menu.waitFor();
          await press("Enter");
          await menu.waitFor({ state: "hidden" });
        } else await type("[[Fresh Page]]");
      }
    const made = await richBox.evaluate((el) => el.querySelectorAll("[data-md]").length);
    ok(made === 2 * N, `${tag} every picked mention and typed wikilink is a piece in the editor (${made} of ${2 * N})`);
    const editing = await markers();
    await done.click();
    await richBox.waitFor({ state: "detached" });
    await page.locator(viewSel).first().waitFor();
    await page.evaluate(() => document.fonts.ready);
    const saved = await markers();
    for (const k of kinds) {
      const rows = [];
      for (let n = 0; n < N; n++) {
        const a = editing.markers[k + n];
        const b = saved.markers[k + n];
        if (!a || !b) rows.push(`#${n} missing`);
        else if (Math.abs(a[0] - b[0]) > 0.5 || Math.abs(a[1] - b[1]) > 0.5 || Math.abs(a[2] - b[2]) > 0.5)
          {
            rows.push(`#${n} dx ${(b[0] - a[0]).toFixed(1)} dy ${(b[1] - a[1]).toFixed(1)} dh ${(b[2] - a[2]).toFixed(1)}`);
            console.log(`info ${tag} #${n} editor: ${editing.dump[k + n]}\ninfo ${tag} #${n} view:   ${saved.dump[k + n]}`);
          }
      }
      ok(rows.length === 0, `${tag} text after a ${k === "menu" ? "picked mention" : "typed wikilink"} stays put when the editor saves: ${rows.length} of ${N} moved${rows.length ? ` (${rows.slice(0, 6).join("; ")})` : ""}`);
    }
    const md = await desc(i.key);
    ok(md.includes("@robin ") && md.includes("[[Fresh Page]] mk"), `${tag} the saved markdown holds them literally (${JSON.stringify(md.slice(0, 120))})`);
  }

  // What the description view draws for an issue: its wikilinks, mentions and links.
  const viewed = async (key) => {
    await page.goto(`${base}/issue/${key}`);
    await page.getByRole("tab", { name: "Details" }).click({ timeout: 15000 }).catch(() => {});
    const v = page.locator(viewSel).first();
    await v.waitFor();
    return v.evaluate((el) => ({
      wikilinks: Array.from(el.querySelectorAll("a.wikilink"), (a) => a.textContent),
      mentions: Array.from(el.querySelectorAll(".mention"), (m) => m.textContent),
      links: Array.from(el.querySelectorAll("a:not(.wikilink)"), (a) => a.getAttribute("href")),
      text: el.textContent.trim(),
    }));
  };

  // Typed [[x]] and @name become pieces as they are completed; undo gives the typed text back.
  {
    await openSmall("Typed pieces", "Start");
    await press("Control+End");
    await type(" [[Fresh]]");
    ok((await pieceCount()) === 1, `${tag} typing the closing ]] makes a piece`);
    await press("Control+z");
    const lit = await richBox.evaluate((el) => el.textContent.trim());
    ok((await pieceCount()) === 0 && lit === "Start [[Fresh]]", `${tag} one undo gives back the typed text (${JSON.stringify(lit)})`);
  }
  {
    await openSmall("Typed piece Backspace", "Start");
    await press("Control+End");
    await type(" [[Again]]");
    await press("Backspace");
    const left = await richBox.evaluate((el) => el.textContent.replace(/[\ufeff\u00a0]/g, " ").trim());
    // Backspace takes back the rule that just applied, leaving the typed text.
    ok((await pieceCount()) === 0 && left === "Start [[Again]]", `${tag} Backspace right after a converted wikilink gives back the typed [[Again]] (${JSON.stringify(left)}, ${await pieceCount()} piece(s))`);
  }
  {
    const i = await openSmall("Typed mention", "Start");
    await press("Control+End");
    await type(" and @robin.");
    // Two frames on, so a conversion put off to the next frame would show.
    await richBox.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
    ok((await pieceCount()) === 0, `${tag} a name followed by "." is not converted yet (it may go on: @a.b)`);
    // A typed name stays text in the editor (only the menu makes a piece); it is saved as typed and the view shows it as a mention.
    await done.click();
    const want = "Start and @robin.";
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} a typed @robin is saved as typed (${JSON.stringify(got)})`);
    const v = await viewed(i.key);
    ok(JSON.stringify(v.mentions) === '["@robin"]', `${tag} the view shows the typed @robin as a mention (${JSON.stringify(v.mentions)})`);
  }

  // What follows a piece is not part of it, and stays apart from it when the
  // markdown is read again: "(" after "]]" would make the wikilink a link.
  for (const [tail, want] of [
    ["(draft)", "Start [[Foo]]\\(draft)"],
    ["[[Bar]]", "Start [[Foo]][[Bar]]"],
  ]) {
    const i = await openSmall(`Piece then ${tail}`, "Start");
    await press("Control+End");
    await type(` [[Foo]]${tail}`);
    await done.click();
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} [[Foo]] directly followed by ${tail} saves as ${JSON.stringify(want)} (${JSON.stringify(got)})`);
    const v = await viewed(i.key);
    ok(v.wikilinks[0] === "Foo" && v.links.length === 0 && v.text === `Start Foo${tail.replace("[[Bar]]", "Bar")}`, `${tag} the view draws [[Foo]] then ${tail} as typed (${JSON.stringify(v)})`);
  }

  // A piece with a letter typed straight after it, and a piece at the end of a paragraph.
  {
    const i = await openSmall("Letter after", "ab @robin cd");
    await press("Home");
    for (let k = 0; k < 4; k++) await press("ArrowRight");
    await type("x");
    await done.click();
    // "@robinx" would be another person: a space keeps the mention who it was.
    const want = "ab @robin x cd";
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} a letter typed right after @robin is kept apart from the name (${JSON.stringify(got)})`);
    const v = await viewed(i.key);
    ok(JSON.stringify(v.mentions) === '["@robin"]', `${tag} the view still mentions @robin (${JSON.stringify(v.mentions)})`);
  }
  // Markup pasted or dropped in that looks like a piece but shows other words
  // saves the words shown, not the attribute's link.
  {
    const i = await openSmall("Forged piece", "Start");
    await press("Control+End");
    await richBox.evaluate(() => document.execCommand("insertHTML", false, ' <span contenteditable="false" data-md="[[Other Page]]">OK</span>'));
    await done.click();
    // Chromium may put the inserted span in a block of its own.
    const got = await waitDesc(i.key, "Start OK");
    ok(/^Start\s+OK$/.test(got ?? ""), `${tag} a pasted span carrying another wikilink saves its visible text (${JSON.stringify(got)})`);
  }
  {
    const i = await openSmall("Piece at the end", "ab [[Some Page]]");
    const box = await richBox.locator("[data-md]").boundingBox();
    await page.mouse.click(box.x + box.width - 2, box.y + box.height / 2);
    await type("z");
    await press("Enter");
    await type("w");
    await done.click();
    const want = "ab [[Some Page]]z\n\nw";
    const got = await waitDesc(i.key, want);
    ok(got === want, `${tag} click after a piece at the end, type, Enter, type: ${JSON.stringify(got)}`);
  }
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`\n${errors.length} failure(s)`);
  process.exit(1);
}
console.log("no errors");
