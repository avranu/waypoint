// A markdown box as a person meets it, whichever editor is showing: the
// formatted editor (a contenteditable textbox, the default) or the markdown
// textarea (the person's "Edit as markdown" choice, localStorage wp.editor.mode).
// Suites whose subject is not the editor go through these two helpers.

/**
 * The box, by its accessible name (label, a string matched as a substring or a
 * RegExp) or by its placeholder. root is a page or a locator to search in.
 *   mdBox(page, { label: "Comment" })
 *   mdBox(dialog, { placeholder: "Add a description" })
 */
export function mdBox(root, { label, placeholder, exact }) {
  if (label !== undefined) {
    return root.getByRole("textbox", { name: label, exact }).filter({ visible: true });
  }
  // CSS can't match an attribute by pattern: a RegExp of plain words (or ^words) is taken as that text.
  let op = "*=";
  let text = placeholder;
  if (placeholder instanceof RegExp) {
    text = placeholder.source;
    if (text.startsWith("^")) (op = "^="), (text = text.slice(1));
    if (/[\\[\]()|.*+?{}$]/.test(text)) throw new Error(`mdBox: pattern ${placeholder} is not plain text`);
  }
  const q = `${op}${JSON.stringify(String(text))} i`;
  return root.locator(`textarea[placeholder${q}], .rich-editor[data-placeholder${q}]`);
}

/**
 * Replaces the box's text with md. A textarea is filled; the formatted editor
 * gets a paste of text/plain over its whole content, so markdown becomes
 * formatting and prose goes in literally, as when a person pastes.
 */
export async function fillMarkdown(box, md) {
  const el = box.first();
  if ((await el.evaluate((n) => n.tagName)) === "TEXTAREA") {
    await el.fill(md);
    return;
  }
  await el.focus();
  await el.evaluate((n, text) => {
    const sel = getSelection();
    const r = document.createRange();
    r.selectNodeContents(n);
    sel.removeAllRanges();
    sel.addRange(r);
    const dt = new DataTransfer();
    dt.setData("text/plain", text);
    n.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
  }, md);
  // The editor hands its text up in the transaction; two frames let React and
  // the caller's state take it before the next step reads either.
  await el.evaluate(
    () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
  );
}

/**
 * The box's text: a textarea's markdown, or the formatted editor's text as a
 * person reads it, without markdown syntax (check shape() for formatting).
 */
export async function markdownOf(box) {
  const el = box.first();
  return (await el.evaluate((n) => n.tagName)) === "TEXTAREA"
    ? el.inputValue()
    : el.evaluate((n) => n.innerText);
}

/**
 * The formatting toolbar of one formatted editor. A page can hold several
 * (a description and the new-comment box), so a button looked up on the page
 * alone is ambiguous.
 *   toolbarOf(richBox).getByRole("button", { name: "Bold" })
 */
export function toolbarOf(editor) {
  return editor
    .locator("xpath=ancestor::div[contains(concat(' ',normalize-space(@class),' '),' relative ')][1]")
    .getByRole("toolbar", { name: "Formatting" });
}

/**
 * Pastes text/plain (and html when given) into the focused formatted editor
 * with a synthetic ClipboardEvent. literal sends Shift+Ctrl+V first, which
 * is how a person pastes the text as it is: the keydown goes to the editor,
 * then the paste.
 */
export async function pasteText(box, text, { html, literal } = {}) {
  await box.focus();
  await box.evaluate(
    (el, [t, h, lit, mac]) => {
      if (lit)
        el.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: "V",
            code: "KeyV",
            keyCode: 86,
            shiftKey: true,
            ctrlKey: !mac,
            metaKey: mac,
            bubbles: true,
            cancelable: true,
          }),
        );
      const dt = new DataTransfer();
      if (t !== null) dt.setData("text/plain", t);
      if (h) dt.setData("text/html", h);
      el.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
      if (lit) el.dispatchEvent(new KeyboardEvent("keyup", { key: "V", code: "KeyV", bubbles: true }));
    },
    [text, html ?? null, !!literal, false],
  );
}

/** The "Edit formatted" button under one markdown textarea that stands in for the editor. */
export function formattedSwitch(textarea) {
  return textarea
    .locator("xpath=ancestor::div[contains(concat(' ',normalize-space(@class),' '),' space-y-2 ')][1]")
    .getByRole("button", { name: "Edit formatted" });
}

/**
 * The editor's HTML with what only the editor needs taken off, so a check can
 * ask what a person sees (a heading, a list item, a checked task) without the
 * editor's wrappers: a lone paragraph inside a list item or cell, the task item's
 * label and box, the placeholder and trailing break, link targets, editing flags.
 */
export function shape(editor) {
  return editor.evaluate((el) => {
    const c = el.cloneNode(true);
    const unwrap = (n) => n.replaceWith(...n.childNodes);
    c.querySelectorAll("br.ProseMirror-trailingBreak, img.ProseMirror-separator, colgroup").forEach((n) => n.remove());
    c.querySelectorAll("p[data-placeholder]").forEach((p) => {
      p.removeAttribute("data-placeholder");
      p.removeAttribute("class");
    });
    c.querySelectorAll('ul[data-type="taskList"]').forEach((u) => u.removeAttribute("data-type"));
    c.querySelectorAll("li[data-checked]").forEach((li) => {
      const on = li.getAttribute("data-checked") === "true";
      li.removeAttribute("data-checked");
      li.querySelector(":scope > label")?.remove();
      const box = document.createElement("input");
      box.setAttribute("type", "checkbox");
      box.className = "task";
      if (on) box.setAttribute("checked", "");
      const body = li.querySelector(":scope > div");
      if (body) unwrap(body);
      li.prepend(box);
    });
    c.querySelectorAll("li, td, th").forEach((n) => {
      const ps = [...n.children].filter((x) => x.tagName === "P");
      if (ps.length === 1) unwrap(ps[0]);
    });
    c.querySelectorAll(".tableWrapper").forEach(unwrap);
    c.querySelectorAll("table").forEach((t) => t.removeAttribute("style"));
    c.querySelectorAll("a").forEach((a) => (a.removeAttribute("target"), a.removeAttribute("rel")));
    c.querySelectorAll("[contenteditable], [draggable]").forEach((n) => (n.removeAttribute("contenteditable"), n.removeAttribute("draggable")));
    return c.innerHTML;
  });
}

/**
 * Ctrl+Home in the formatted editor, repeated until the caret is at the start:
 * the editor puts its caret at the end as it opens, and a key pressed in the
 * first moments is read before that settles (a person cannot press that fast).
 */
export async function caretToStart(box) {
  const atStart = () =>
    box.evaluate((el) => {
      const s = getSelection();
      if (!s.rangeCount || !el.contains(s.anchorNode)) return false;
      const r = document.createRange();
      r.selectNodeContents(el);
      r.setEnd(s.anchorNode, s.anchorOffset);
      return r.toString().replace(/﻿/g, "") === "";
    });
  for (let i = 0; i < 20; i++) {
    await box.press("Control+Home");
    // wplint:ignore e2e-wait polling a value: the caret settles a moment after the editor opens
    await box.page().waitForTimeout(100);
    if (await atStart()) return;
  }
}

// WebKit reports a fetch cancelled by a navigation (page.goto, reload) as a
// pageerror, "<url> due to access control checks." Under Playwright's WebKit each
// one comes with a requestfailed "Load request cancelled" within ms of the
// navigation; no other engine raises it. Only that message, only under WebKit.
export const navigationAbort = (message) =>
  process.env.RICH_TYPING_BROWSER === "webkit" && /due to access control checks\.?$/.test(message);
