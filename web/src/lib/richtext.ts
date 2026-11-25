import { isPieceSrc, pieceText, PIECE_SRC } from '@/lib/markdown'

// A root-relative path never starts "//" or "/\": browsers read a backslash
// there as a slash, and the link would leave the site.
export const SAFE_HREF = /^(?:https?:\/\/|mailto:|\/(?![/\\])|#)\S+$/i
// An image is fetched, so only a web address or a path on this site.
export const SAFE_SRC = /^(?:https?:\/\/|\/(?![/\\]))\S+$/i

// `label`: text inside a link label or image alt, where any bracket would end
// it early. `cell`: inside a table cell, which is one line (its pipes are
// escaped once, by table()).
type Opts = { code?: boolean; label?: boolean; cell?: boolean }

// Descriptions are read raw by agents, so only what would change meaning when
// the markdown is rendered again is escaped: backticks and `*` (except a
// lone `*` with a space on both sides); `_` unless it sits inside a word
// (snake_case); `\` only before punctuation (or at the end, where it would
// join a hard break); `~` unless it can't open or close a strikethrough (a
// lone "~" or a leading "~/path"); `<` only where it would start HTML or an
// autolink; `]` only before `(` or `[` (a link or image the typist didn't
// mean; [[wikilinks]] stay as typed); `&` only where it starts an entity.
// In a link label or alt text every bracket is escaped. Edges of a text node
// count as non-word and non-space.
const ENTITY = /^&(?:#\d+|#[xX][\da-fA-F]+|[A-Za-z][A-Za-z0-9]*);/
function escapeText(s: string, opts: Opts = {}) {
  return s.replace(/[\\`*_~<[\]&]/g, (c, i: number) => {
    const prev = s[i - 1]
    const next = s[i + 1]
    switch (c) {
      case '\\':
        return next === undefined || next === '\n' || /[!-/:-@[-`{-~]/.test(next) ? '\\\\' : c
      case '*':
        return prev && next && /\s/.test(prev) && /\s/.test(next) ? c : '\\*'
      case '_':
        return prev && next && /\w/.test(prev) && /\w/.test(next) ? c : '\\_'
      case '~': {
        const opensOnly = !prev || /\s/.test(prev)
        const inert = next === undefined || next === '/' || /\s/.test(next)
        return opensOnly && inert ? c : '\\~'
      }
      case '<':
        return next && /[A-Za-z/!?]/.test(next) ? '\\<' : c
      case '[':
        return opts.label ? '\\[' : c
      case ']':
        return opts.label || next === '(' || next === '[' ? '\\]' : c
      case '&':
        return ENTITY.test(s.slice(i)) ? '\\&' : c
      default:
        return `\\${c}`
    }
  })
}

// A line that begins with block syntax would turn into that block when the
// markdown is rendered, so a literal one is escaped. Only what would really
// become a block. A paragraph starts on a run's first line and after a blank
// line; there, an indent of four columns would make it code, so it is
// dropped (the renderer drops a shorter one anyway), any number can start a
// list, and three dashes are a rule. Inside a paragraph a number starts a
// list only as "1." (its `.` or `)` is escaped, the digits aren't), a run of
// "-" or "=" is a setext underline, and a dash-and-pipe row under a line of
// cells makes a table. A "[label]:" is a link definition.
function escapeLineStarts(s: string) {
  let start = true
  return s
    .split('\n')
    .map((raw) => {
      const first = start
      start = !raw.trim()
      let line = raw
      if (first) {
        const indent = /^[ \t]*/.exec(line)![0]
        if (columns(indent) >= 4) line = line.slice(indent.length)
      }
      const m = /^(\s*)(?:(>)|(#{1,6}|[+*-])(?=\s|$)|(\d+)([.)])(?=\s|$))/.exec(line)
      if (m) {
        if (m[4] !== undefined) {
          if (!first && Number(m[4]) !== 1) return line
          return `${m[1]}${m[4]}\\${m[5]}${line.slice(m[0].length)}`
        }
        return `${m[1]}\\${m[2] ?? m[3]}${line.slice(m[0].length)}`
      }
      const rule = /^(\s*)([-=])(?:\s*\2)*\s*$/.exec(line)
      const dashes = line.split('-').length - 1
      if (rule && (!first || (rule[2] === '-' && dashes >= 3))) {
        return `${rule[1]}\\${line.slice(rule[1].length)}`
      }
      if (
        !first &&
        line.includes('|') &&
        /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/.test(line)
      ) {
        return line.replace(/[\\|]/g, (c) => (c === '|' ? '\\|' : c))
      }
      const def = /^(\s*)\[[^\]]+\]:/.exec(line)
      if (def) return `${def[1]}\\${line.slice(def[1].length)}`
      return line
    })
    .join('\n')
}

// How wide an indent is, with a tab reaching the next multiple of four.
function columns(indent: string) {
  let n = 0
  for (const c of indent) n = c === '\t' ? n + 4 - (n % 4) : n + 1
  return n
}

// Whether the character at i is escaped by the backslashes before it.
function escapedAt(s: string, i: number) {
  let n = 0
  while (i - n - 1 >= 0 && s[i - n - 1] === '\\') n++
  return n % 2 === 1
}

// Pieces of serialized markdown put side by side. escapeText sees one text
// node at a time, so what one piece ends with can combine with what the next
// starts with: "!" before a link makes it an image, "]" before "(" or "[" a
// link, "<" before a letter a tag, and "&" before the rest of a name an
// entity. That character of the first piece is escaped.
function join(a: string, b: string) {
  if (!a || !b) return a + b
  const last = a.length - 1
  const c = a[last]
  if (
    !escapedAt(a, last) &&
    ((c === '!' && b[0] === '[') ||
      (c === ']' && (b[0] === '(' || b[0] === '[')) ||
      (c === '<' && /[A-Za-z/!?]/.test(b[0])))
  ) {
    return `${a.slice(0, last)}\\${c}${b}`
  }
  const amp = /&[#A-Za-z0-9]*$/.exec(a)
  if (amp && !escapedAt(a, amp.index) && ENTITY.test(amp[0] + b.slice(0, 40))) {
    return `${a.slice(0, amp.index)}\\${a.slice(amp.index)}${b}`
  }
  return a + b
}

// Emphasis markers hug the text: edge whitespace goes outside them, and an
// all-whitespace run is no emphasis at all.
function wrap(mark: string, s: string) {
  const m = /^(\s*)([\s\S]*?)(\s*)$/.exec(s) as RegExpExecArray
  return m[2] ? `${m[1]}${mark}${m[2]}${mark}${m[3]}` : s
}

function codeSpan(text: string) {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((r) => r.length))
  const ticks = '`'.repeat(longest + 1)
  const pad = text.startsWith('`') || text.endsWith('`') ? ' ' : ''
  return `${ticks}${pad}${text}${pad}${ticks}`
}

// A link or image destination. A backslash before punctuation would be read
// as an escape, a line break can't be in one, and `<` or `>` would end the
// angle form, so those are escaped or encoded.
function target(url: string) {
  const u = url.replace(/\\(?=[!-/:-@[-`{-~])/g, '\\\\').replace(/[\r\n]/g, encodeURIComponent)
  return /[()\s<>]/.test(u) ? `<${u.replace(/[\\<>]/g, (c) => (c === '\\' ? c : `\\${c}`))}>` : u
}

// A code block's text. The browser writes a newline typed in one as <br>, and
// a <br> at the very end only holds the last line open, so it is no text; the
// rendered form of a block likewise ends in a newline. Exactly one trailing
// newline goes, so a blank last line the person made is kept.
function codeText(node: Node): string {
  const text = (n: Node): string =>
    n.nodeType === Node.TEXT_NODE
      ? (n as Text).data
      : n.nodeName === 'BR'
        ? '\n'
        : (pieceOf(n) ?? Array.from(n.childNodes).map(text).join(''))
  const all = text(node)
  return all.endsWith('\n') ? all.slice(0, -1) : all
}

// The markdown of a wikilink or @mention the editor drew, or null. Only one
// that shows what its markdown shows: markup pasted or dropped in with its own
// attribute must not save a link or name other than the words on screen.
function pieceOf(node: Node): string | null {
  if (node.nodeType !== Node.ELEMENT_NODE) return null
  const el = node as HTMLElement
  const piece = el.getAttribute(PIECE_SRC)
  return piece !== null &&
    el.getAttribute('contenteditable') === 'false' &&
    isPieceSrc(piece) &&
    el.textContent === pieceText(piece)
    ? piece
    : null
}

// What follows a wikilink or mention must not become part of it when the
// markdown is read again: a "(" after "]]" would make the wikilink a link's
// label and the parenthesis its address, so it is escaped; a letter, digit or
// "_" (with any "." or "-" before it) after "@name" would lengthen the name,
// so a space goes between them.
function afterPiece(a: string, piece: string, b: string) {
  if (piece.startsWith('[[')) return b.startsWith('(') ? `${a}\\${b}` : a + b
  const name = (s: string) => s.replace(/[.-]+$/, '')
  const glued = /^@[a-zA-Z0-9][\w.-]{0,38}/.exec(piece + b.slice(0, 41).replaceAll('\\_', '_'))
  return glued && name(glued[0]) !== name(piece) ? `${a} ${b}` : a + b
}

// An element's text for a code span, a piece being its markdown, not its label.
function plainText(node: Node): string {
  const piece = pieceOf(node)
  if (piece !== null) return piece
  if (node.nodeType === Node.TEXT_NODE) return (node as Text).data
  return Array.from(node.childNodes).map(plainText).join('')
}

function serialize(node: Node, opts: Opts = {}): string {
  if (node.nodeType === Node.TEXT_NODE) {
    const data = (node as Text).data.replace(/\u00a0/g, ' ')
    return opts.code ? data : escapeText(data, opts)
  }
  if (node.nodeType === Node.DOCUMENT_FRAGMENT_NODE) return blocks(node, opts)
  if (node.nodeType !== Node.ELEMENT_NODE) return ''

  const el = node as HTMLElement
  const tag = el.tagName.toLowerCase()
  const inner = (o: Opts = opts) => children(el, o)

  // A wikilink or @mention the editor drew: its markdown, as it was.
  const piece = pieceOf(el)
  if (piece !== null) return piece

  switch (tag) {
    case 'br':
      return '\n'
    case 'strong':
    case 'b':
      return wrap('**', inner())
    case 'em':
    case 'i':
      return wrap('*', inner())
    case 's':
    case 'strike':
    case 'del':
      return wrap('~~', inner())
    case 'code':
      if (el.parentElement?.tagName.toLowerCase() === 'pre') return plainText(el)
      return codeSpan(plainText(el))
    case 'pre': {
      const code = el.querySelector('code')
      const lang = (code?.className.match(/(?:^|\s)language-(\S+)/) ?? [])[1] ?? ''
      const body = codeText(code ?? el)
      if (opts.cell) return codeSpan(oneLine(body))
      const longest = Math.max(0, ...(body.match(/`+/g) ?? []).map((r) => r.length))
      const fence = '`'.repeat(Math.max(3, longest + 1))
      return `\n${fence}${lang}\n${body}\n${fence}\n`
    }
    case 'h1':
    case 'h2':
    case 'h3':
    case 'h4':
    case 'h5':
    case 'h6':
      return `\n${'#'.repeat(Number(tag[1]))} ${oneLine(inner()).replace(/(^|\s)(#+)$/, '$1\\$2')}\n`
    case 'p':
    case 'div':
      return `\n${blocks(el, opts)}\n`
    case 'blockquote': {
      const body = blocks(el, opts).trim()
      const quoted = body
        .split('\n')
        .map((l) => (l ? `> ${l}` : '>'))
        .join('\n')
      return `\n${quoted}\n`
    }
    case 'ul':
    case 'ol':
      return `\n${list(el, opts)}\n`
    case 'a': {
      const href = el.getAttribute('href') ?? ''
      if (!href) return inner().trim()
      const own = escapeText(href, { label: true })
      const label = inner({ ...opts, label: true }).trim() || own
      if (label === own && AUTOLINK.test(href)) return `\0${href}\0`
      return `[${label}](${target(href)})`
    }
    case 'img': {
      const alt = el.getAttribute('alt') ?? ''
      const src = el.getAttribute('src') ?? ''
      return `![${altText(alt)}](${target(src)})`
    }
    case 'video': {
      const alt = el.getAttribute('aria-label') ?? ''
      const src = el.getAttribute('src') ?? ''
      return `![${altText(alt)}](${target(src)})`
    }
    case 'hr':
      return '\n---\n'
    case 'table':
      return `\n${table(el)}\n`
    case 'input':
    case 'script':
    case 'style':
    case 'template':
    case 'noscript':
      return ''
    default:
      return inner()
  }
}

// Alt text is plain text, read back as markdown inline content.
function altText(alt: string) {
  return escapeText(oneLine(alt), { label: true })
}

// A heading or table row is one line: a line break inside it becomes a space.
function oneLine(md: string) {
  return md.replace(/\s*\n\s*/g, ' ').trim()
}

function children(node: Node, opts: Opts = {}) {
  let out = ''
  let piece: string | null = null
  for (const child of Array.from(node.childNodes)) {
    const md = serialize(child, opts)
    // A wikilink or mention is whole as it is: no "]" of it is escaped.
    out = piece !== null ? afterPiece(out, piece, md) : join(out, md)
    piece = pieceOf(child)
  }
  return out
}

const BLOCK = new Set([
  'p',
  'div',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'ul',
  'ol',
  'blockquote',
  'pre',
  'table',
  'hr',
])

// A container's children, with each run of inline content (a bare text node
// at the editor's root, say, as well as a paragraph's) line-start escaped.
function blocks(node: Node, opts: Opts = {}) {
  return blocksOf(Array.from(node.childNodes), opts)
}

function blocksOf(nodes: Node[], opts: Opts = {}) {
  let out = ''
  let run = ''
  let afterBlock = false
  let piece: string | null = null
  const text = (beforeBlock: boolean) =>
    run.trim()
      ? `${afterBlock ? '\n' : ''}${escapeLineStarts(run)}${beforeBlock ? '\n' : ''}`
      : escapeLineStarts(run)
  for (const child of nodes) {
    if (
      child.nodeType === Node.ELEMENT_NODE &&
      BLOCK.has((child as Element).tagName.toLowerCase())
    ) {
      out += text(true) + serialize(child, opts)
      run = ''
      afterBlock = true
      piece = null
    } else {
      const md = serialize(child, opts)
      run = piece !== null ? afterPiece(run, piece, md) : join(run, md)
      piece = pieceOf(child)
    }
  }
  return out + text(false)
}

const isListTag = (e: Element) => ['ul', 'ol'].includes(e.tagName.toLowerCase())

function list(el: HTMLElement, opts: Opts = {}): string {
  const ordered = el.tagName.toLowerCase() === 'ol'
  let out = ''
  let n = ordered ? Number.parseInt(el.getAttribute('start') ?? '', 10) : 1
  if (!Number.isFinite(n)) n = 1
  const kids = Array.from(el.children)
  for (const [at, child] of kids.entries()) {
    if (child.tagName.toLowerCase() !== 'li') {
      // A sub-list with no item before it has no parent to nest under.
      if (isListTag(child) && !kids.slice(0, at).some((k) => k.tagName.toLowerCase() === 'li'))
        out += list(child as HTMLElement, opts)
      continue
    }
    const li = child as HTMLElement
    const box = Array.from(li.children).find(
      (c) => c.tagName.toLowerCase() === 'input' && (c as HTMLInputElement).type === 'checkbox',
    ) as HTMLInputElement | undefined
    const nested: HTMLElement[] = []
    const own: Node[] = []
    for (const c of Array.from(li.childNodes)) {
      if (c.nodeType === Node.ELEMENT_NODE) {
        const t = (c as HTMLElement).tagName.toLowerCase()
        if (t === 'ul' || t === 'ol') {
          nested.push(c as HTMLElement)
          continue
        }
        if (t === 'input') continue
      }
      own.push(c)
    }
    let body = blocksOf(own, opts).trim()
    if (!box && /^\[[ xX]\]/.test(body)) body = `\\${body}`
    const bare = ordered ? `${n++}.` : '-'
    const marker = box ? `${bare} [${box.checked ? 'x' : ' '}]` : bare
    for (let k = at + 1; k < kids.length && isListTag(kids[k]); k++)
      nested.push(kids[k] as HTMLElement)
    const pad = ' '.repeat(bare.length + 1)
    body = body
      .split('\n')
      .map((l, i) => (i && l ? pad + l : l))
      .join('\n')
    out += `${marker} ${body}\n`
    for (const sub of nested)
      out += list(sub, opts)
        .split('\n')
        .map((l) => (l ? pad + l : l))
        .join('\n')
  }
  return out
}

// A table cell's markdown, made safe to sit between pipes. The table reader
// (marked) splits a row at every pipe after an even run of backslashes, then
// takes one backslash off each pipe it kept, and only then reads the cell as
// inline markdown. So a pipe after an even run (none, or escaped backslashes)
// gets one more backslash, and the cell reads back as written. A pipe after
// an odd run is already escaped and is left alone: as inline text it meant a
// plain pipe anyway (inside a code span one backslash before it is lost;
// there is no way to write that in a cell).
function cellPipes(md: string) {
  return md.replace(/(\\*)\|/g, (_m, bs: string) => (bs.length % 2 ? `${bs}|` : `${bs}\\|`))
}

function table(el: HTMLElement): string {
  const rows = Array.from(el.querySelectorAll('tr'))
  if (!rows.length) return ''
  const cells = (tr: Element) =>
    Array.from(tr.children).map((c) => cellPipes(oneLine(children(c, { cell: true }))))
  const head = cells(rows[0])
  const lines = [`| ${head.join(' | ')} |`, `| ${head.map(() => '---').join(' | ')} |`]
  for (const row of rows.slice(1)) lines.push(`| ${cells(row).join(' | ')} |`)
  return lines.join('\n')
}

// Blank-line runs and trailing spaces are tidied outside fenced code, where
// they are part of the code. A fence may open a list item.
function tidy(md: string) {
  const out: string[] = []
  let fence = 0
  for (const line of md.split('\n')) {
    if (fence) {
      const f = /^(?:>\s?)*\s*(`{3,})\s*$/.exec(line)
      if (f && f[1].length >= fence) fence = 0
      out.push(line)
      continue
    }
    const f = /^(?:>\s?)*\s*(?:(?:[-+*]|\d+[.)])\s+)*(`{3,})(.*)$/.exec(line)
    if (f && !f[2].includes('`')) fence = f[1].length
    const l = line.replace(/[ \t]+$/, '')
    if (!l && out.length && !out[out.length - 1]) continue
    out.push(l)
  }
  return out.join('\n').replace(/^\n+/, '').replace(/\n+$/, '')
}

// A CommonMark autolink's address: a scheme, then no space, `<` or `>`.
const AUTOLINK = /^[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>\x00-\x1f]*$/
// A web address that reads back whole when written bare: nothing the reader
// would stop at, take as markup, decode or trim off its end.
const BARE = /^(?:https?|ftp):\/\/[A-Za-z0-9-][^\s<>()[\]\\`*~"']*$/

// Autolinks come out of serialize() between NULs (the HTML parser drops or
// replaces any NUL in the input, so no text holds one). Written bare, an
// address runs into what follows it, and the reader trims closing
// punctuation off its end; so it is bare only after the start, a space or
// "(", and before the end, a space, or closing punctuation and then one of
// those. Anywhere else it is <angled>, which nothing next to it changes.
function autolinks(md: string) {
  return md.replace(/\0([^\0]*)\0/g, (m, url: string, i: number) => {
    const before = md[i - 1]
    const after = md.slice(i + m.length, i + m.length + 40)
    const bare =
      BARE.test(url) &&
      !/[?!.,:;_]$/.test(url) &&
      !/&[#A-Za-z0-9]+;/.test(url) &&
      (before === undefined || /[\s(]/.test(before)) &&
      /^[.,:;!?)]*(?:\s|$)/.test(after)
    return bare ? url : `<${url}>`
  })
}

/** The markdown behind a rich editor's HTML. */
export function htmlToMarkdown(html: string): string {
  const tpl = document.createElement('template')
  tpl.innerHTML = html
  return autolinks(tidy(serialize(tpl.content)))
}

/**
 * Whether markdown starts with a YAML front-matter block (a `---` line,
 * `key: value` lines, a closing `---`), as skills and memory files do. The
 * editor would read it as a rule and a heading, so such text opens as source.
 */
export function hasFrontMatter(md: string): boolean {
  const lines = md.replace(/^\uFEFF/, '').split(/\r?\n/)
  if (!/^---[ \t]*$/.test(lines[0])) return false
  let keys = 0
  for (const line of lines.slice(1)) {
    if (/^---[ \t]*$/.test(line)) return keys > 0
    if (/^[\w.-]+[ \t]*:/.test(line)) keys++
  }
  return false
}
