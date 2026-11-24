// Markdown typed in the rich editor applies as it is typed (WP-1749). This is
// the matching only, on plain strings: richdom.ts finds the text before the
// caret and does the DOM work for what is matched here.

/**
 * The caret-parking character: U+FEFF, the zero-width no-break space that
 * ProseMirror uses for its cursor wrapper. In the middle of text it means
 * nothing, unlike a zero-width space, which people paste in. After an inline
 * conversion the caret needs a text node of its own outside the new element,
 * or the browser types into the element; this is that node's content.
 * htmlToMarkdown drops it, and nothing else.
 */
export const PARK = '\ufeff'

/** Stands for an element (inline code, a checkbox, an image) in the text. */
export const OPAQUE = '\ufffc'

export type BlockRule =
  | { kind: 'heading'; level: number }
  | { kind: 'ul' }
  | { kind: 'ol'; start: number }
  | { kind: 'task'; checked: boolean }
  | { kind: 'quote' }

/**
 * What a block becomes when Space is typed after `before`, the block's text up
 * to the caret including that space. The marker is all of that text. In a
 * list item only a task box converts, since a bullet there would be a
 * different thing (a nested list) that the typist did not ask for.
 */
export function blockRule(before: string, inItem: boolean): BlockRule | null {
  if (!/[ \u00a0]$/.test(before)) return null
  const m = before.slice(0, -1)
  const task = /^\[( |x|X)?\]$/.exec(m)
  if (task) return { kind: 'task', checked: task[1] === 'x' || task[1] === 'X' }
  if (inItem) return null
  if (/^#{1,6}$/.test(m)) return { kind: 'heading', level: m.length }
  if (/^[-*+]$/.test(m)) return { kind: 'ul' }
  const num = /^(\d{1,9})[.)]$/.exec(m)
  if (num) return { kind: 'ol', start: Number(num[1]) }
  if (m === '>') return { kind: 'quote' }
  return null
}

/** The language of a block that is exactly a code fence, or null. */
export function fenceRule(text: string, bareOnly = false): { lang: string } | null {
  const m = (bareOnly ? /^```()$/ : /^```([\w+#.-]*)$/).exec(text.trim())
  return m ? { lang: m[1] } : null
}

/** A block that is exactly a thematic break. */
export function ruleLine(text: string): boolean {
  return /^(?:-{3}|\*{3}|_{3})$/.test(text.trim())
}

export type InlineKind = 'code' | 'strong' | 'em' | 's' | 'link'

/** Indices into the text passed to inlineRule. */
export type InlineMatch = {
  kind: InlineKind
  /** Where the opening delimiter starts; the match runs to the caret. */
  from: number
  contentFrom: number
  contentTo: number
  href?: string
}

const space = (c: string | undefined) => c !== undefined && (c === PARK || /[\s\u00a0]/.test(c))
// An underscore opens or closes only at a word edge: nothing, white space or
// punctuation beside it, never a letter or digit (snake_case, file_name).
const edge = (c: string | undefined) => c === undefined || !/[\p{L}\p{N}]/u.test(c)

// A root-relative path never starts "//" or "/\": browsers read a backslash
// there as a slash, and the link would leave the site.
const SAFE_HREF = /^(?:https?:\/\/|mailto:|\/(?![/\\])|#)\S+$/i

/**
 * The inline markdown just completed by the character typed last, or null.
 * `before` is the text of the block up to and including that character,
 * `after` the text that follows the caret. Content must be non-empty with no
 * white space just inside either delimiter; the nearest opener that is valid
 * wins.
 */
export function inlineRule(before: string, after: string): InlineMatch | null {
  const end = before.length
  const c = before[end - 1]
  if (c === ')') return linkRule(before)
  if (c === '`') {
    if (before[end - 2] === '`' || after[0] === '`') return null
    const i = before.lastIndexOf('`', end - 2)
    if (i < 0 || before[i - 1] === '`' || before[i + 1] === '`') return null
    // A backslash makes the opening backtick literal; inside a code span it
    // escapes nothing, so the closing one closes whatever precedes it.
    if (backslashed(before, i)) return null
    return wrapped('code', before, i, 1, end - 1)
  }
  if (c !== '*' && c !== '_' && c !== '~') return null
  const delim = c.repeat(2)
  if (c === '~') {
    if (before.slice(end - 2) !== delim || before[end - 3] === c || after[0] === c) return null
    return opener('s', before, after, c, 2)
  }
  if (before.slice(end - 2) === delim && before[end - 3] !== c) {
    // The second half of a closing "**": strong, or nothing.
    if (after[0] === c) return null
    return opener('strong', before, after, c, 2)
  }
  // A lone "*" next to another is half of a pair and closes nothing yet.
  if (before[end - 2] === c || after[0] === c) return null
  return opener('em', before, after, c, 1)
}

// Whether the character at i follows an odd run of backslashes.
function backslashed(s: string, i: number) {
  let n = 0
  while (i - n - 1 >= 0 && s[i - n - 1] === '\\') n++
  return n % 2 === 1
}

function wrapped(
  kind: InlineKind,
  text: string,
  open: number,
  openLen: number,
  close: number,
): InlineMatch | null {
  const contentFrom = open + openLen
  const content = text.slice(contentFrom, close)
  if (!content || space(content[0]) || space(content[content.length - 1])) return null
  return { kind, from: open, contentFrom, contentTo: close }
}

// Looks back from the closing delimiter (of length n, ending at the caret) for
// the nearest opener that is valid.
function opener(
  kind: InlineKind,
  before: string,
  after: string,
  c: string,
  n: number,
): InlineMatch | null {
  const close = before.length - n
  const word = c === '_'
  if (word && !edge(after[0])) return null
  const delim = c.repeat(n)
  // A backslash before a delimiter makes it literal.
  if (backslashed(before, close)) return null
  for (let i = close - n; i >= 0; i--) {
    if (before.slice(i, i + n) !== delim) continue
    // The opener is not part of a longer run.
    if (before[i - 1] === c || before[i + n] === c) continue
    if (word && !edge(before[i - 1])) continue
    if (backslashed(before, i)) continue
    const m = wrapped(kind, before, i, n, close)
    if (m) return m
  }
  return null
}

function linkRule(before: string): InlineMatch | null {
  const m = /\[([^[\]]+)\]\(([^()\s]+)\)$/.exec(before)
  if (!m || before[m.index - 1] === '!') return null
  // Each of "[", "]" and ")" is literal after an odd run of backslashes.
  const close = m.index + 1 + m[1].length
  if ([m.index, close, before.length - 1].some((i) => backslashed(before, i))) return null
  if (!SAFE_HREF.test(m[2]) || !m[1].trim()) return null
  const contentFrom = m.index + 1
  return {
    kind: 'link',
    from: m.index,
    contentFrom,
    contentTo: contentFrom + m[1].length,
    href: m[2],
  }
}
