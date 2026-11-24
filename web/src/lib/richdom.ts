// The DOM half of typed markdown in the rich editor (WP-1749). richinput.ts
// decides what a line of text means; this finds that line in the editor and
// makes the change with execCommand, never by editing nodes directly, so the
// browser's undo gives back what was typed (Ctrl+Z after "# " is "# ").

import {
  blockRule,
  fenceRule,
  inlineRule,
  OPAQUE,
  PARK,
  ruleLine,
  type BlockRule,
  type InlineMatch,
} from '@/lib/richinput'

// A table cell is a block of its own: the caret in one is never in "the table".
const TEXT_BLOCKS = new Set(['P', 'DIV', 'LI', 'TD', 'TH', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6'])
const ANY_BLOCKS = new Set([...TEXT_BLOCKS, 'PRE', 'BLOCKQUOTE', 'UL', 'OL', 'TABLE', 'HR'])
const TASK_BOX = '<input class="task" type="checkbox" contenteditable="false">'

/** A paragraph-like block: an element, or a bare run of nodes at the root. */
type Block = { node: Element | null; parent: Node; from: number; to: number }

function caretIn(el: HTMLElement): Range | null {
  const sel = window.getSelection()
  if (!sel || sel.rangeCount === 0) return null
  const r = sel.getRangeAt(0)
  return r.collapsed && el.contains(r.startContainer) ? r : null
}

function elementOf(n: Node): Element | null {
  return n.nodeType === Node.ELEMENT_NODE ? (n as Element) : n.parentElement
}

function blockAt(el: HTMLElement, caret: Range): Block | null {
  let n: Node = caret.startContainer
  while (n !== el) {
    if (n.nodeType === Node.ELEMENT_NODE && TEXT_BLOCKS.has((n as Element).tagName)) {
      return { node: n as Element, parent: n.parentNode!, from: 0, to: 0 }
    }
    const p = n.parentNode
    if (!p) return null
    if (p === el) {
      const kids = Array.from(el.childNodes)
      const stop = (k: Node) =>
        k.nodeName === 'BR' ||
        (k.nodeType === Node.ELEMENT_NODE && ANY_BLOCKS.has((k as Element).tagName))
      let from = kids.indexOf(n as ChildNode)
      let to = from + 1
      while (from > 0 && !stop(kids[from - 1])) from--
      while (to < kids.length && !stop(kids[to])) to++
      return { node: null, parent: el, from, to }
    }
    n = p
  }
  return null
}

function contentOf(b: Block): Range {
  const r = document.createRange()
  if (b.node) r.selectNodeContents(b.node)
  else {
    r.setStart(b.parent, b.from)
    r.setEnd(b.parent, b.to)
  }
  return r
}

/** A text node and where its part before the caret starts in `Scanned.before`. */
type TextRun = { node: Text; start: number; len: number }
type Scanned = { before: string; after: string; texts: TextRun[] }

const OPAQUE_TAGS = new Set(['CODE', 'PRE', 'INPUT', 'IMG', 'VIDEO'])

// The block's text before and after the caret. Elements the rules never look
// inside (code, a checkbox, an image) are one OPAQUE character, a <br> a
// newline, a no-break space a space.
function scan(b: Block, caret: Range): Scanned {
  let before = ''
  let after = ''
  const texts: TextRun[] = []
  const walk = (node: Node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      const t = node as Text
      let cut = t.data.length
      if (t === caret.startContainer) cut = caret.startOffset
      else if (caret.comparePoint(t, 0) > 0) cut = 0
      const head = t.data.slice(0, cut).replace(/\u00a0/g, ' ')
      if (head) texts.push({ node: t, start: before.length, len: head.length })
      before += head
      if (after.length < 2) after += t.data.slice(cut).replace(/\u00a0/g, ' ')
      return
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return
    const e = node as Element
    const isBefore = caret.comparePoint(e.parentNode!, indexIn(e)) < 0
    if (OPAQUE_TAGS.has(e.tagName) || e.tagName === 'BR') {
      const ch = e.tagName === 'BR' ? '\n' : OPAQUE
      if (isBefore) before += ch
      else if (after.length < 2) after += ch
      return
    }
    for (const k of Array.from(e.childNodes)) walk(k)
  }
  if (b.node) for (const k of Array.from(b.node.childNodes)) walk(k)
  else for (let i = b.from; i < b.to; i++) walk(b.parent.childNodes[i])
  return { before, after, texts }
}

const indexIn = (n: Node) => Array.prototype.indexOf.call(n.parentNode!.childNodes, n) as number

function pointAt(s: Scanned, i: number): [Node, number] | null {
  for (const t of s.texts) if (i >= t.start && i <= t.start + t.len) return [t.node, i - t.start]
  return null
}

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
const escAttr = (s: string) => esc(s).replace(/"/g, '&quot;')

function select(r: Range) {
  const sel = window.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(r)
}

function caretAt(node: Node, offset: number) {
  const r = document.createRange()
  r.setStart(node, offset)
  r.collapse(true)
  select(r)
}

// The first place text can go in a block: past a task box and its space.
function caretAtStart(node: Element) {
  const walk = document.createTreeWalker(node, NodeFilter.SHOW_TEXT)
  const t = walk.nextNode() as Text | null
  const box = node.querySelector('input.task')
  if (box && (!t || t.compareDocumentPosition(box) & Node.DOCUMENT_POSITION_PRECEDING)) {
    // Past the box; a text node right after it may start with its space.
    const after = box.nextSibling
    if (after?.nodeType === Node.TEXT_NODE && (after as Text).data.startsWith(GAP))
      return caretAt(after, 1)
    return caretAt(box.parentNode!, indexIn(box) + 1)
  }
  if (!t) return caretAt(node, 0)
  caretAt(t, 0)
}

function html(frag: DocumentFragment): string {
  const box = document.createElement('div')
  box.append(frag)
  return box.innerHTML
}

const exec = (cmd: string, arg?: string) => document.execCommand(cmd, false, arg)

// Firefox keeps a <br> inserted into an empty block as a real line break
// (typing then lands after it) and supplies its own placeholder; Blink and
// WebKit need the <br> for the empty block to have a line.
const GECKO = typeof navigator !== 'undefined' && /Firefox\//.test(navigator.userAgent)
const EMPTY_LINE = GECKO ? '' : '<br>'
// The space after a task box. Firefox drops a plain one at the end of inserted
// markup, and then the caret has no place after the box; a no-break one stays.
const GAP = GECKO ? '\u00a0' : ' '

const nodeRange = (n: Node) => {
  const r = document.createRange()
  r.selectNode(n)
  return r
}

// A paragraph or div directly in the editor, which a block rule replaces whole.
const wholeOrContents = (b: Block) => (replaceBlock(b) ? nodeRange(b.node!) : contentOf(b))
const replaceBlock = (b: Block) => b.node?.tagName === 'P' || b.node?.tagName === 'DIV'

// On in Blink and Gecko, where e2e/85-rich-typing.mjs passes all its checks
// (WP-1758). Off in WebKit (Safari, every iOS browser), where typed markdown
// stays literal text, as before WP-1749: in Playwright's WebKit that suite
// fails 63 of its checks beyond the "access control checks" page errors WebKit
// raises when a navigation cancels a fetch (undo takes two steps after a
// rule, insertHTML at the end of a paragraph adds inline styles and style
// spans, Enter in code splits the <pre>, the task box loses
// contenteditable=false, outdent leaves a <font>). Firefox on iOS is WebKit
// and says "FxiOS", not "Firefox/".
const SUPPORTED = typeof navigator === 'undefined' || /Chrome\/|Firefox\//.test(navigator.userAgent)

/**
 * Called after a character was typed. Converts "# ", "- ", "[ ] " and the
 * like at the start of a block, and `code`, **bold**, *italic*, ~~strike~~
 * and [text](url) as the closing delimiter is typed. True if it changed
 * anything.
 */
export function inputRules(el: HTMLElement, e: InputEvent): boolean {
  if (!SUPPORTED) return false
  // A composition commit that arrives as an input event with isComposing
  // already false (Safari ends the composition first) is judged like a
  // composition end; one still composing is left alone.
  if (e.isComposing) return false
  if (e.inputType === 'insertCompositionText') return composedRules(el, e.data ?? '')
  if (e.inputType !== 'insertText' || !e.data || e.data.length !== 1) return false
  return typedRules(el, e.data, false)
}

/**
 * Called when a composition has ended (Android keyboards, IMEs, dictation
 * send ordinary typing this way) with the text it committed. The DOM is never
 * changed while a composition is active, so the rules run here, judging from
 * the text before the caret as ProseMirror does: the trigger is the last
 * character committed, whatever the length of what was committed. A paste or
 * autocorrect is not a composition and never comes through here.
 */
export function composedRules(el: HTMLElement, data: string): boolean {
  if (!SUPPORTED || !data) return false
  return typedRules(el, data.slice(-1).replace(/\u00a0/g, ' '), true)
}

function typedRules(el: HTMLElement, ch: string, composed: boolean): boolean {
  const caret = caretIn(el)
  if (!caret) return false
  const at = elementOf(caret.startContainer)
  if (!at || at.closest('pre, code') || !el.contains(at)) return false
  const b = blockAt(el, caret)
  if (!b) return false
  const s = scan(b, caret)
  // The committed text may already have been acted on, or the caret moved on.
  if (composed && !s.before.endsWith(ch)) return false
  if (ch === ' ') {
    const bare = b.node ? b.node.tagName === 'P' || b.node.tagName === 'DIV' : true
    const inItem = b.node?.tagName === 'LI'
    if (!bare && !inItem) return false
    if (bare && b.parent !== el) return false
    // A fence replaces the whole paragraph, so only when nothing follows it.
    const fence = fenceRule(s.before.trim(), true)
    if (fence && bare && s.after === '') return codeBlock(el, b, fence.lang)
    const rule = blockRule(s.before, !!inItem)
    return rule ? convert(el, b, caret, rule) : false
  }
  const m = inlineRule(s.before, s.after)
  return m ? inline(s, caret, m) : false
}

function convert(el: HTMLElement, b: Block, caret: Range, rule: BlockRule): boolean {
  const content = contentOf(b)
  const tail = document.createRange()
  tail.setStart(caret.startContainer, caret.startOffset)
  tail.setEnd(content.endContainer, content.endOffset)
  const rest = html(tail.cloneContents())
  const body = rest || EMPTY_LINE
  const box = (checked: boolean) => `${TASK_BOX.replace('>', checked ? ' checked>' : '>')}${GAP}`
  let out: string
  switch (rule.kind) {
    case 'heading':
      out = `<h${rule.level}>${body}</h${rule.level}>`
      break
    case 'ul':
      out = `<ul><li>${body}</li></ul>`
      break
    case 'ol':
      out = `<ol${rule.start === 1 ? '' : ` start="${rule.start}"`}><li>${body}</li></ol>`
      break
    case 'quote':
      out = `<blockquote><p>${body}</p></blockquote>`
      break
    case 'task':
      out = box(rule.checked) + rest
      if (b.node?.tagName !== 'LI') out = `<ul><li>${out}</li></ul>`
      break
  }
  // A whole paragraph is replaced as a node: insertHTML over a paragraph's
  // contents leaves an empty <p> behind in Firefox.
  select(replaceBlock(b) ? nodeRange(b.node!) : content)
  exec('insertHTML', out)
  const now = caretIn(el)
  const nb = now && blockAt(el, now)
  if (nb?.node) caretAtStart(nb.node)
  return true
}

function codeBlock(el: HTMLElement, b: Block, lang: string): boolean {
  select(wholeOrContents(b))
  const cls = lang ? ` class="language-${escAttr(lang)}"` : ''
  exec('insertHTML', `<pre><code${cls}>${EMPTY_LINE}</code></pre>`)
  const now = caretIn(el)
  const code = now && elementOf(now.startContainer)?.closest('pre')?.querySelector('code')
  if (code) caretAt(code, 0)
  return true
}

function inline(s: Scanned, caret: Range, m: InlineMatch): boolean {
  const open = pointAt(s, m.from)
  const from = pointAt(s, m.contentFrom)
  const to = pointAt(s, m.contentTo)
  if (!open || !from || !to) return false
  const inner = document.createRange()
  inner.setStart(...from)
  inner.setEnd(...to)
  const frag = inner.cloneContents()
  const full = document.createRange()
  full.setStart(...open)
  full.setEnd(caret.startContainer, caret.startOffset)
  let out: string
  if (m.kind === 'code') out = `<code>${esc(frag.textContent ?? '')}</code>`
  else if (m.kind === 'link') out = `<a href="${escAttr(m.href!)}">${html(frag)}</a>`
  else {
    const tag = { strong: 'strong', em: 'em', s: 's' }[m.kind]
    out = `<${tag}>${html(frag)}</${tag}>`
  }
  select(full)
  // The parking character after the element is where the caret goes: the
  // browser types into the element it follows, so the next characters need a
  // node of their own. htmlToMarkdown drops it.
  exec('insertHTML', out + PARK)
  return true
}

// A code block's text. The browser writes a newline as <br>, and one <br> at
// the very end only holds the last line open.
function codeText(node: Node): string {
  if (node.nodeType === Node.TEXT_NODE) return (node as Text).data
  if (node.nodeName === 'BR') return '\n'
  return Array.from(node.childNodes).map(codeText).join('')
}

function codeSplit(pre: Element, caret: Range) {
  const head = document.createRange()
  head.selectNodeContents(pre)
  head.setEnd(caret.startContainer, caret.startOffset)
  const tail = document.createRange()
  tail.selectNodeContents(pre)
  tail.setStart(caret.startContainer, caret.startOffset)
  const text = (r: Range) => Array.from(r.cloneContents().childNodes).map(codeText).join('')
  let after = text(tail)
  const last = pre.querySelector('code')?.lastChild ?? pre.lastChild
  if (last?.nodeName === 'BR' && after.endsWith('\n')) after = after.slice(0, -1)
  return { before: text(head), after }
}

function paragraphAfter(node: Node) {
  const r = document.createRange()
  r.setStart(node.parentNode!, indexIn(node) + 1)
  r.collapse(true)
  select(r)
  exec('insertHTML', `<p>${EMPTY_LINE}</p>`)
}

// Moves the caret over a task box right beside it; false if there is none.
function stepOverBox(caret: Range, forward: boolean): boolean {
  const { startContainer: c, startOffset: o } = caret
  let next: Node | null | undefined
  if (c.nodeType === Node.TEXT_NODE) {
    if (forward ? o === (c as Text).length : o === 0)
      next = forward ? c.nextSibling : c.previousSibling
  } else next = c.childNodes[forward ? o : o - 1]
  if (!(next instanceof HTMLInputElement) || !next.matches('input.task')) return false
  caretAt(next.parentNode!, indexIn(next) + (forward ? 1 : 0))
  return true
}

function eraseBack(n: number) {
  const sel = window.getSelection()
  for (let i = 0; i < n; i++) sel?.modify('extend', 'backward', 'character')
  exec('delete')
}

/** The character the caret would cross moving one step, without moving it. */
function peek(dir: 'backward' | 'forward'): string {
  const sel = window.getSelection()
  if (!sel || sel.rangeCount === 0) return ''
  const keep = sel.getRangeAt(0).cloneRange()
  sel.modify('extend', dir, 'character')
  const crossed = sel.toString()
  select(keep)
  return crossed
}

// Backspace with the caret just after a parking character: it goes together
// with the one visible character before it (a whole grapheme, an emoji too),
// or alone when nothing precedes it in its block.
function eraseParked(caret: Range) {
  const sel = window.getSelection()
  if (!sel) return
  const { startContainer, startOffset } = caret
  sel.modify('extend', 'backward', 'character')
  const parked = sel.toString()
  sel.modify('extend', 'backward', 'character')
  const both = sel.toString()
  if (both.length === parked.length || both.includes('\n')) {
    caretAt(startContainer, startOffset)
    sel.modify('extend', 'backward', 'character')
  }
  exec('delete')
}

/** What the key rules read of an event: a keydown, or a beforeinput standing in for one. */
type KeyLike = Pick<
  KeyboardEvent,
  'key' | 'isComposing' | 'shiftKey' | 'ctrlKey' | 'metaKey' | 'altKey' | 'preventDefault'
>

const plain = (e: KeyLike) => !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey

/**
 * Keys the browser handles badly in markdown-shaped content: Enter and Tab
 * in code, Enter after "```" or "---", Backspace at the start of a heading,
 * quote, code or task, and arrows past the parking character after an inline
 * conversion. True if it was handled (the event is then cancelled).
 */
export function keyRules(el: HTMLElement, e: KeyLike): boolean {
  if (!SUPPORTED || e.isComposing) return false
  if (e.key === 'Tab' && !e.ctrlKey && !e.metaKey && !e.altKey && tabInList(el, e)) return true
  const caret = caretIn(el)
  if (!caret) return false
  if (e.key === 'Backspace' && plain(e)) {
    if (peek('backward') === PARK) {
      e.preventDefault()
      eraseParked(caret)
      return true
    }
    return backspace(el, e, caret)
  }
  if ((e.key === 'ArrowLeft' || e.key === 'ArrowRight') && plain(e)) {
    // Firefox leaves the caret where it is when the next thing is a task box;
    // Blink steps over the box itself, and doing it here would move it twice.
    if (GECKO && stepOverBox(caret, e.key === 'ArrowRight')) {
      e.preventDefault()
      return true
    }
    // Only the step over a parking character is the editor's: it takes two
    // moves to get past it. Every other arrow press is the browser's.
    // Without one in the editor there is nothing to look for, and looking
    // moves the selection and puts it back, which the browser may not undo
    // exactly (next to a checkbox).
    if (!el.textContent?.includes(PARK)) return false
    const dir = e.key === 'ArrowLeft' ? 'backward' : 'forward'
    if (peek(dir) !== PARK) return false
    e.preventDefault()
    const sel = window.getSelection()
    sel?.modify('move', dir, 'character')
    sel?.modify('move', dir, 'character')
    return true
  }
  const at = elementOf(caret.startContainer)
  const pre = at?.closest('pre')
  if (pre && el.contains(pre)) return inCode(el, e, caret, pre)
  if (e.key !== 'Enter' || !plain(e)) return false
  const b = blockAt(el, caret)
  if (!b) return false
  if (b.node?.tagName === 'H6' && b.parent === el && scan(b, caret).after === '') {
    // Blink starts the line after a heading in a new paragraph for h1 to h5
    // but stays in an h6; the other levels are the browser's.
    e.preventDefault()
    exec('insertParagraph')
    exec('formatBlock', 'p')
    return true
  }
  if (b.node?.tagName === 'LI') {
    // An empty nested item steps out one level; only a top-level one ends the list.
    if (isNested(b.node, el) && b.node.textContent!.replace(PARK, '').trim() === '') {
      e.preventDefault()
      exec('outdent')
      return true
    }
    return taskEnter(el, e, b.node, caret)
  }
  if (b.node?.parentElement?.tagName === 'BLOCKQUOTE') return quoteEnter(el, e, b.node)
  if (b.parent !== el || (b.node && b.node.tagName !== 'P' && b.node.tagName !== 'DIV'))
    return false
  const s = scan(b, caret)
  if (s.after !== '') return false
  const fence = fenceRule(s.before.trim())
  if (fence) {
    e.preventDefault()
    return codeBlock(el, b, fence.lang)
  }
  if (ruleLine(s.before)) {
    e.preventDefault()
    select(wholeOrContents(b))
    exec('insertHTML', `<hr><p>${EMPTY_LINE}</p>`)
    return true
  }
  return false
}

const isList = (n: Element | null) => n?.tagName === 'UL' || n?.tagName === 'OL'

// An item inside another list: Chromium's indent puts the new list in the
// previous item or, as often, straight inside the outer list.
function isNested(li: Element, el: HTMLElement): boolean {
  const list = li.parentElement
  const up = list?.parentElement
  return !!list && isList(list) && !!up && up !== el && (up.tagName === 'LI' || isList(up))
}

// Tab nests the items under the caret (or in the selection) beneath their
// previous item, Shift+Tab lifts them one level, both with execCommand so one
// undo reverses one press. Where neither can happen in a list the key is still
// swallowed, or focus would leave the editor mid-list; Escape is the way out.
// Outside lists, and in code, Tab is the browser's or the code rule's.
function tabInList(el: HTMLElement, e: KeyLike): boolean {
  const sel = window.getSelection()
  if (!sel || sel.rangeCount === 0) return false
  const r = sel.getRangeAt(0)
  if (!el.contains(r.startContainer) || !el.contains(r.endContainer)) return false
  const start = elementOf(r.startContainer)
  // Code is the code rule's (below), which takes Tab as two spaces.
  if (!start || start.closest('pre')) return false
  const li = start.closest('li')
  if (!li || !el.contains(li)) return false
  e.preventDefault()
  if (e.shiftKey) {
    if (isNested(li, el)) exec('outdent')
    return true
  }
  // Chromium indents an item with no previous sibling into a list of its own.
  const prev = li.previousElementSibling
  if (prev && (prev.tagName === 'LI' || isList(prev))) exec('indent')
  return true
}

/**
 * Android keyboards send Enter and Backspace as keydown 229 with no key, then
 * beforeinput insertParagraph / insertLineBreak / deleteContentBackward. This
 * runs the key rules for those, only when the keydown did not carry the key
 * (the caller checks), so a desktop press is not handled twice.
 */
export function beforeInputRules(el: HTMLElement, e: InputEvent): boolean {
  if (!SUPPORTED || e.isComposing || !e.cancelable) return false
  const key =
    e.inputType === 'insertParagraph' || e.inputType === 'insertLineBreak'
      ? 'Enter'
      : e.inputType === 'deleteContentBackward'
        ? 'Backspace'
        : ''
  if (!key) return false
  return keyRules(el, {
    key,
    isComposing: false,
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    altKey: false,
    preventDefault: () => e.preventDefault(),
  })
}

function inCode(el: HTMLElement, e: KeyLike, caret: Range, pre: Element): boolean {
  if (!plain(e)) return false
  const { before, after } = codeSplit(pre, caret)
  if (e.key === 'Tab') {
    e.preventDefault()
    exec('insertText', '  ')
    return true
  }
  if (e.key === 'ArrowDown') {
    if (after.includes('\n') || pre.parentNode !== el || pre.nextElementSibling) return false
    e.preventDefault()
    paragraphAfter(pre)
    return true
  }
  if (e.key !== 'Enter') return false
  e.preventDefault()
  if (after === '') {
    // A closing fence, or a second empty line, ends the block.
    const fence = /(^|\n)```$/.exec(before)
    const gone = fence ? fence[0].length : before.endsWith('\n\n') ? 2 : 0
    if (gone) {
      eraseBack(gone)
      paragraphAfter(pre)
      return true
    }
  }
  exec('insertText', '\n')
  return true
}

function quoteEnter(el: HTMLElement, e: KeyLike, p: Element): boolean {
  if (p.textContent !== '' || p.parentElement!.lastElementChild !== p) return false
  e.preventDefault()
  // Outdent lifts the empty line out of the quote; with other lines left in
  // the quote it comes out as bare text, which is made a paragraph.
  exec('outdent')
  // The browser rebuilds the quote, so the line is found again from the caret.
  const now = caretIn(el)
  let bare: Node | null = now ? now.startContainer : null
  while (bare && bare.parentNode !== el) bare = bare.parentNode
  if (bare && (bare.nodeName === 'FONT' || bare.nodeName === 'BR')) {
    const r = document.createRange()
    r.selectNode(bare)
    select(r)
    exec('insertHTML', `<p>${EMPTY_LINE}</p>`)
  }
  return true
}

function taskEnter(el: HTMLElement, e: KeyLike, li: Element, caret: Range): boolean {
  const box = Array.from(li.children).find((c) => c.matches('input.task'))
  if (!box) return false
  // Caret before the box: leave it to the browser.
  if (caret.comparePoint(box.parentNode!, indexIn(box)) > 0) return false
  if (li.textContent!.trim() === '') {
    // An empty task is no task: the box goes and Enter leaves the list.
    removeBox(li)
    return false
  }
  e.preventDefault()
  exec('insertParagraph')
  const now = caretIn(el)
  const next = now && blockAt(el, now)
  if (!next?.node) return true
  // The new item starts with a box in front of what followed the caret, which
  // stays; with nothing after the caret the item is only the box.
  if (next.node.textContent === '') select(contentOf(next))
  exec('insertHTML', `${TASK_BOX}${GAP}`)
  caretAtStart(next.node)
  if (GECKO) dropStrandedGap(next.node.previousElementSibling)
  return true
}

// Firefox splits a task between its box and its text with the gap left behind
// in the first item; Chromium collapses it. An item of only box and gap is an
// empty task, so the gap goes too (through execCommand, so undo sees it).
function dropStrandedGap(li: Element | null) {
  const box = li && Array.from(li.children).find((c) => c.matches('input.task'))
  const gap = box?.nextSibling
  if (!li || gap?.nodeType !== Node.TEXT_NODE || (gap as Text).data !== GAP || gap.nextSibling)
    return
  const keep = caretIn(li.parentElement!.closest('[contenteditable]') as HTMLElement)
  const r = document.createRange()
  r.selectNode(gap)
  select(r)
  exec('delete')
  if (keep) select(keep)
}

// `keepItem`: Backspace in a task with nothing after its box leaves an empty
// bullet item, where a plain delete would take the whole item (and the list)
// with it.
function removeBox(li: Element, keepItem = false) {
  const box = Array.from(li.children).find((c) => c.matches('input.task'))
  if (!box) return
  const r = document.createRange()
  r.setStartBefore(box)
  const next = box.nextSibling
  if (next?.nodeType === Node.TEXT_NODE && (next as Text).data.startsWith(GAP)) r.setEnd(next, 1)
  else r.setEndAfter(box)
  if (keepItem && li.textContent!.trim() === '') {
    // Taking the box out of an item with nothing else in it takes the whole
    // item (and the list). With a placeholder character in it, the item stays;
    // backspacing that last, visible character leaves the empty item.
    caretAt(li, 0)
    exec('insertText', '.')
    const dot = li.firstChild
    const gone = document.createRange()
    gone.setStartAfter(dot!)
    gone.setEnd(r.endContainer, r.endOffset)
    select(gone)
    exec('delete')
    if (dot?.nodeType === Node.TEXT_NODE) caretAt(dot, 1)
    exec('delete')
    return
  }
  select(r)
  exec('delete')
}

function backspace(el: HTMLElement, e: KeyLike, caret: Range): boolean {
  const at = elementOf(caret.startContainer)
  const pre = at?.closest('pre')
  if (pre && el.contains(pre)) {
    if (codeSplit(pre, caret).before !== '') return false
    e.preventDefault()
    exec('formatBlock', 'p')
    return true
  }
  const b = blockAt(el, caret)
  if (!b?.node) return false
  const s = scan(b, caret)
  const tag = b.node.tagName
  if (tag === 'LI') {
    // At the start of a nested item (before a task's box too) it steps out one
    // level; a top-level task then loses its box as before.
    if (isNested(b.node, el) && (s.before === '' || /^\ufffc ?$/.test(s.before))) {
      e.preventDefault()
      exec('outdent')
      return true
    }
    if (!/^\ufffc ?$/.test(s.before) || !b.node.querySelector(':scope > input.task')) return false
    e.preventDefault()
    removeBox(b.node, true)
    return true
  }
  if (s.before !== '') return false
  if (/^H[1-6]$/.test(tag)) {
    e.preventDefault()
    exec('formatBlock', 'p')
    return true
  }
  const quote = b.node.parentElement
  if (quote?.tagName === 'BLOCKQUOTE' && quote.firstElementChild === b.node) {
    e.preventDefault()
    exec('outdent')
    return true
  }
  return false
}
