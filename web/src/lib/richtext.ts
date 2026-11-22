// Turning the rich editor's HTML back into markdown (WP-628). Only the tags a
// contenteditable editor makes are handled; anything else falls back to its
// text, so pasted markup can't smuggle itself into the source.

import { renderMarkdownForEdit } from '@/lib/markdown'

type Opts = { code?: boolean }

// Descriptions are read raw by agents, so only what would change meaning when
// the markdown is rendered again is escaped: `*` and backticks always; `_`
// unless it sits inside a word (snake_case); `\` only before punctuation (or
// at the end, where it would join a hard break); `~` unless it can't open or
// close a strikethrough (a lone "~" or a leading "~/path"); `<` only where it
// would start HTML or an autolink. Edges of a text node count as non-word.
function escapeText(s: string) {
  return s.replace(/[\\`*_~<]/g, (c, i: number) => {
    const prev = s[i - 1]
    const next = s[i + 1]
    switch (c) {
      case '\\':
        return next === undefined || next === '\n' || /[!-/:-@[-`{-~]/.test(next) ? '\\\\' : c
      case '_':
        return prev && next && /\w/.test(prev) && /\w/.test(next) ? c : '\\_'
      case '~': {
        const opensOnly = !prev || /\s/.test(prev)
        const inert = next === undefined || next === '/' || /\s/.test(next)
        return opensOnly && inert ? c : '\\~'
      }
      case '<':
        return next && /[A-Za-z/!?]/.test(next) ? '\\<' : c
      default:
        return `\\${c}`
    }
  })
}

// A line that begins with block syntax would turn into that block when the
// markdown is rendered, so a literal one is escaped. Only what would really
// become a block: a number starts a list only on a paragraph's first line or
// as "1.", and a run of "-" or "=" matters as a rule or a setext underline.
function escapeLineStarts(s: string) {
  return s
    .split('\n')
    .map((line, i) => {
      const m = /^(\s*)(#{1,6}|[>+-]|(\d+)[.)])(?=\s|$)/.exec(line)
      if (m) {
        if (m[3] !== undefined && i > 0 && Number(m[3]) !== 1) return line
        return `${m[1]}\\${m[2]}${line.slice(m[1].length + m[2].length)}`
      }
      const rule = /^(\s*)([-=])\2*\s*$/.exec(line)
      if (rule && (i > 0 || (rule[2] === '-' && line.trim().length >= 3))) {
        return `${rule[1]}\\${line.slice(rule[1].length)}`
      }
      return line
    })
    .join('\n')
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
  if (node.nodeType === Node.DOCUMENT_FRAGMENT_NODE) return children(node, opts)
  if (node.nodeType !== Node.ELEMENT_NODE) return ''

  const el = node as HTMLElement
  const tag = el.tagName.toLowerCase()
  const inner = (o: Opts = opts) => children(el, o)

  switch (tag) {
    case 'br':
      return '\n'
    case 'strong':
    case 'b':
      return `**${inner()}**`
    case 'em':
    case 'i':
      return `*${inner()}*`
    case 's':
    case 'strike':
    case 'del':
      return `~~${inner()}~~`
    case 'code':
      if (el.parentElement?.tagName.toLowerCase() === 'pre') return el.textContent ?? ''
      return codeSpan(el.textContent ?? '')
    case 'pre': {
      const code = el.querySelector('code')
      const lang = (code?.className.match(/language-([\w-]+)/) ?? [])[1] ?? ''
      const body = (code ?? el).textContent?.replace(/\n$/, '') ?? ''
      return `\n\`\`\`${lang}\n${body}\n\`\`\`\n`
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
      return `\n${escapeLineStarts(inner())}\n`
    case 'blockquote': {
      const body = inner().trim()
      const quoted = body
        .split('\n')
        .map((l) => (l ? `> ${l}` : '>'))
        .join('\n')
      return `\n${quoted}\n`
    }
    case 'ul':
    case 'ol':
      return `\n${list(el, 0)}\n`
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

function list(el: HTMLElement, depth: number): string {
  const ordered = el.tagName.toLowerCase() === 'ol'
  let out = ''
  let n = 1
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
    const marker = box ? `- [${box.checked ? 'x' : ' '}]` : ordered ? `${n++}.` : '-'
    out += `${'  '.repeat(depth)}${marker} ${body}\n`
    for (const sub of nested) out += list(sub, depth + 1)
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
  return md
    .split(/(```[\s\S]*?```)/g)
    .map((part, i) => (i % 2 ? part : part.replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n')))
    .join('')
    .replace(/^\n+/, '')
    .replace(/\n+$/, '')
}

/** The markdown behind a rich editor's HTML. */
export function htmlToMarkdown(html: string): string {
  const tpl = document.createElement('template')
  tpl.innerHTML = html
  return tidy(serialize(tpl.content))
}

const norm = (html: string) => html.replace(/\s+/g, ' ').trim()

/**
 * Whether markdown comes back from the rich editor unchanged in what it shows:
 * raw HTML, footnotes and the like don't, and opening them there would lose
 * them silently (WP-628).
 */
export function survivesRichEdit(md: string): boolean {
  const html = renderMarkdownForEdit(md)
  return norm(renderMarkdownForEdit(htmlToMarkdown(html))) === norm(html)
}
