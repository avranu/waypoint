// Turning the rich editor's HTML back into markdown (WP-628). Only the tags a
// contenteditable editor makes are handled; anything else falls back to its
// text, so pasted markup can't smuggle itself into the source.

import { renderMarkdownForEdit, hasRawHtml } from '@/lib/markdown'

type Opts = { code?: boolean }

// Descriptions are read raw by agents, so only what would change meaning when
// the markdown is rendered again is escaped: backticks and `*` (except a
// lone `*` with a space on both sides); `_` unless it sits inside a word
// (snake_case); `\` only before punctuation (or at the end, where it would
// join a hard break); `~` unless it can't open or close a strikethrough (a
// lone "~" or a leading "~/path"); `<` only where it would start HTML or an
// autolink; `]` only before `(` or `[` (a link or image the typist didn't
// mean; [[wikilinks]] stay as typed); `&` only where it starts an entity.
// Edges of a text node count as non-word and non-space.
const ENTITY = /^&(?:#\d+|#[xX][\da-fA-F]+|[A-Za-z][A-Za-z0-9]*);/
function escapeText(s: string) {
  return s.replace(/[\\`*_~<\]&]/g, (c, i: number) => {
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
      case ']':
        return next === '(' || next === '[' ? '\\]' : c
      case '&':
        return ENTITY.test(s.slice(i)) ? '\\&' : c
      default:
        return `\\${c}`
    }
  })
}

// A line that begins with block syntax would turn into that block when the
// markdown is rendered, so a literal one is escaped. Only what would really
// become a block: a number starts a list only on a paragraph's first line or
// as "1." (its `.` or `)` is escaped, the digits aren't), and a run of "-" or
// "=" matters as a rule or a setext underline. A "[label]:" is a link
// definition.
function escapeLineStarts(s: string) {
  return s
    .split('\n')
    .map((line, i) => {
      const m = /^(\s*)(?:(#{1,6}|[>+*-])|(\d+)([.)]))(?=\s|$)/.exec(line)
      if (m) {
        if (m[3] !== undefined) {
          if (i > 0 && Number(m[3]) !== 1) return line
          return `${m[1]}${m[3]}\\${m[4]}${line.slice(m[0].length)}`
        }
        return `${m[1]}\\${m[2]}${line.slice(m[0].length)}`
      }
      const rule = /^(\s*)([-=])\2*\s*$/.exec(line)
      if (rule && (i > 0 || (rule[2] === '-' && line.trim().length >= 3))) {
        return `${rule[1]}\\${line.slice(rule[1].length)}`
      }
      if (/^\s*\[[^\]]+\]:/.test(line)) return line.replace('[', '\\[')
      return line
    })
    .join('\n')
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

function target(url: string) {
  return /[()\s]/.test(url) ? `<${url}>` : url
}

function serialize(node: Node, opts: Opts = {}): string {
  if (node.nodeType === Node.TEXT_NODE) {
    const data = (node as Text).data.replace(/\u00a0/g, ' ')
    return opts.code ? data : escapeText(data)
  }
  if (node.nodeType === Node.DOCUMENT_FRAGMENT_NODE) return blocks(node, opts)
  if (node.nodeType !== Node.ELEMENT_NODE) return ''

  const el = node as HTMLElement
  const tag = el.tagName.toLowerCase()
  const inner = (o: Opts = opts) => children(el, o)

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
      if (el.parentElement?.tagName.toLowerCase() === 'pre') return el.textContent ?? ''
      return codeSpan(el.textContent ?? '')
    case 'pre': {
      const code = el.querySelector('code')
      const lang = (code?.className.match(/language-([\w-]+)/) ?? [])[1] ?? ''
      const body = (code ?? el).textContent?.replace(/\n$/, '') ?? ''
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
      return `\n${'#'.repeat(Number(tag[1]))} ${inner().trim()}\n`
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
      return `\n${list(el)}\n`
    case 'a': {
      const href = el.getAttribute('href') ?? ''
      const label = inner().trim()
      if (!href) return label
      if (!label || label === href) return href
      return `[${label}](${target(href)})`
    }
    case 'img': {
      const alt = el.getAttribute('alt') ?? ''
      const src = el.getAttribute('src') ?? ''
      return `![${alt}](${target(src)})`
    }
    case 'video': {
      const alt = el.getAttribute('aria-label') ?? ''
      const src = el.getAttribute('src') ?? ''
      return `![${alt}](${target(src)})`
    }
    case 'hr':
      return '\n---\n'
    case 'table':
      return `\n${table(el)}\n`
    case 'input':
      return ''
    default:
      return inner()
  }
}

function children(node: Node, opts: Opts = {}) {
  let out = ''
  for (const child of Array.from(node.childNodes)) out += serialize(child, opts)
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
  let out = ''
  let run = ''
  for (const child of Array.from(node.childNodes)) {
    if (
      child.nodeType === Node.ELEMENT_NODE &&
      BLOCK.has((child as Element).tagName.toLowerCase())
    ) {
      out += escapeLineStarts(run) + serialize(child, opts)
      run = ''
    } else run += serialize(child, opts)
  }
  return out + escapeLineStarts(run)
}

function list(el: HTMLElement): string {
  const ordered = el.tagName.toLowerCase() === 'ol'
  let out = ''
  let n = ordered ? Number.parseInt(el.getAttribute('start') ?? '', 10) : 1
  if (!Number.isFinite(n)) n = 1
  for (const child of Array.from(el.children)) {
    if (child.tagName.toLowerCase() !== 'li') continue
    const li = child as HTMLElement
    const box = Array.from(li.children).find(
      (c) => c.tagName.toLowerCase() === 'input' && (c as HTMLInputElement).type === 'checkbox',
    ) as HTMLInputElement | undefined
    const nested: HTMLElement[] = []
    let body = ''
    for (const c of Array.from(li.childNodes)) {
      if (c.nodeType === Node.ELEMENT_NODE) {
        const t = (c as HTMLElement).tagName.toLowerCase()
        if (t === 'ul' || t === 'ol') {
          nested.push(c as HTMLElement)
          continue
        }
        if (t === 'input') continue
      }
      body += serialize(c)
    }
    body = escapeLineStarts(body.trim())
    // A literal "[ ]" opening a bullet would become a task box.
    if (!box && /^\[[ xX]\]/.test(body)) body = `\\${body}`
    const bare = ordered ? `${n++}.` : '-'
    const marker = box ? `${bare} [${box.checked ? 'x' : ' '}]` : bare
    out += `${marker} ${body}\n`
    // Nested content lines up with the parent's text: marker width + 1.
    const pad = ' '.repeat(bare.length + 1)
    for (const sub of nested)
      out += list(sub)
        .split('\n')
        .map((l) => (l ? pad + l : l))
        .join('\n')
  }
  return out
}

function table(el: HTMLElement): string {
  const rows = Array.from(el.querySelectorAll('tr'))
  if (!rows.length) return ''
  const cells = (tr: Element) =>
    Array.from(tr.children).map((c) => children(c).trim().replace(/\|/g, '\\|'))
  const head = cells(rows[0])
  const lines = [`| ${head.join(' | ')} |`, `| ${head.map(() => '---').join(' | ')} |`]
  for (const row of rows.slice(1)) lines.push(`| ${cells(row).join(' | ')} |`)
  return lines.join('\n')
}

// Blank-line runs and trailing spaces are tidied outside fenced code, where
// they are part of the code.
function tidy(md: string) {
  const out: string[] = []
  let fence = 0
  for (const line of md.split('\n')) {
    const f = /^(?:>\s?)*\s*(`{3,})(.*)$/.exec(line)
    if (fence) {
      if (f && f[1].length >= fence && !f[2].trim()) fence = 0
      out.push(line)
      continue
    }
    if (f && !f[2].includes('`')) fence = f[1].length
    const l = line.replace(/[ \t]+$/, '')
    if (!l && out.length && !out[out.length - 1]) continue
    out.push(l)
  }
  return out.join('\n').replace(/^\n+/, '').replace(/\n+$/, '')
}

/** The markdown behind a rich editor's HTML. */
export function htmlToMarkdown(html: string): string {
  const tpl = document.createElement('template')
  tpl.innerHTML = html
  return tidy(serialize(tpl.content))
}

const norm = (html: string) => html.replace(/\s+/g, ' ').trim()

/**
 * Whether markdown can be opened in the rich editor without losing anything.
 * Editing re-spells untouched markdown into the editor's canonical form
 * (`__x__` becomes `**x**`, a `+` bullet `-`, and so on), so this checks
 * what the text renders as, not its bytes: it must render the same after a
 * round trip. Raw HTML (comments included) fails outright, since the
 * sanitiser would drop it from both sides of that comparison and the first
 * edit would delete it; footnotes and the like fail by the comparison.
 */
export function survivesRichEdit(md: string): boolean {
  if (hasRawHtml(md)) return false
  const html = renderMarkdownForEdit(md)
  return norm(renderMarkdownForEdit(htmlToMarkdown(html))) === norm(html)
}
