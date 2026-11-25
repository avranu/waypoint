import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { copyText } from './clipboard'
import type { TextRef } from './types'

marked.setOptions({ gfm: true, breaks: true })

// Our own uploaded video, written as an image in the source, plays in place
// (WP-D16). Anything else stays an image, so markdown can't embed other media.
const ATTACHED_VIDEO = /^\/api\/v1\/attachments\/\d+\/[^?#]+\.(?:mp4|webm)$/i
const attr = (s: string) =>
  s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

// Task checkboxes are marked so raw <input> in the source can be told apart.
marked.use({
  renderer: {
    checkbox: ({ checked }) => `<input type="checkbox" class="task"${checked ? ' checked' : ''}> `,
    image: ({ href, text }) =>
      ATTACHED_VIDEO.test(href)
        ? `<video src="${attr(href)}" controls preload="metadata" playsinline aria-label="${attr(text)}"></video>`
        : false,
  },
})

// Issue keys and @mentions become links, except inside code and links.
const TOKEN = /(^|[\s(])(?:([A-Z][A-Z0-9]{1,9}-\d+)\b|@([a-zA-Z0-9][\w.-]{0,38}))/g

DOMPurify.addHook('uponSanitizeElement', (node, data) => {
  // Only our own task checkboxes survive; raw form controls could overlay the UI.
  if (
    data.tagName === 'input' &&
    !(
      (node as Element).getAttribute('type') === 'checkbox' &&
      (node as Element).classList.contains('task')
    )
  ) {
    node.parentNode?.removeChild(node)
  }
})
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A' && node.getAttribute('href')?.startsWith('http')) {
    node.setAttribute('target', '_blank')
    node.setAttribute('rel', 'noopener noreferrer')
  }
})

const SANITIZE = {
  ADD_ATTR: ['target'],
  FORBID_TAGS: ['style', 'form', 'button', 'select', 'textarea', 'option'],
  FORBID_ATTR: ['style'],
}

// [[Target]] and [[Target|label]] point at a glossary term, design note,
// page or issue; /go looks the target up (in the project, when known).
const WIKILINK = /\[\[([^[\]|\n]{1,200})(?:\|([^[\]\n]{0,200}))?\]\]/g

function textNodes(root: DocumentFragment) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) =>
      n.parentElement?.closest('a, code, pre')
        ? NodeFilter.FILTER_REJECT
        : NodeFilter.FILTER_ACCEPT,
  })
  const nodes: Text[] = []
  for (let n = walker.nextNode(); n; n = walker.nextNode()) nodes.push(n as Text)
  return nodes
}

// The pieces below are drawn by the description view and, as uneditable
// copies, by the rich editor (editorPieces), so both come from these two.
function wikilinkEl(target: string, label: string | undefined, project?: string) {
  const a = document.createElement('a')
  const params = new URLSearchParams({ ref: target })
  if (project) params.set('project', project)
  a.setAttribute('href', `/go?${params}`)
  a.className = 'wikilink'
  a.textContent = (label ?? '').trim() || target
  return a
}

// `name` is without the "@"; trailing punctuation isn't part of it.
const mentionName = (name: string) => name.replace(/[.-]+$/, '')

function mentionEl(user: string) {
  const el = document.createElement('span')
  el.className = 'mention'
  el.textContent = '@' + user
  return el
}

function wikilinks(root: DocumentFragment, project?: string) {
  for (const n of textNodes(root)) {
    const text = n.data
    if (!text.includes('[[')) continue
    const out = document.createDocumentFragment()
    let last = 0
    for (const m of text.matchAll(WIKILINK)) {
      const target = m[1].trim()
      if (!target) continue
      out.append(text.slice(last, m.index), wikilinkEl(target, m[2], project))
      last = m.index + m[0].length
    }
    if (last === 0) continue
    out.append(text.slice(last))
    n.replaceWith(out)
  }
}

function linkify(root: DocumentFragment, keys: boolean) {
  for (const n of textNodes(root)) {
    const text = n.data
    TOKEN.lastIndex = 0
    if (!TOKEN.test(text)) continue
    TOKEN.lastIndex = 0
    const out = document.createDocumentFragment()
    let last = 0
    for (const m of text.matchAll(TOKEN)) {
      const at = m.index + m[1].length
      let el: HTMLElement
      let len: number
      if (m[2] && !keys) {
        continue // the server decides which codes are references
      } else if (m[2]) {
        el = document.createElement('a')
        el.setAttribute('href', `/issue/${m[2]}`)
        el.textContent = m[2]
        len = m[2].length
      } else {
        const user = mentionName(m[3])
        el = mentionEl(user)
        len = user.length + 1
      }
      out.append(text.slice(last, at), el)
      last = at + len
    }
    out.append(text.slice(last))
    n.replaceWith(out)
  }
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/**
 * Links what the server resolved from the text (POST /refs): codes as
 * written, terms in any case. The server decides what is a reference; this
 * only places the links, so a code nothing has stays plain text.
 */
function linkRefs(root: DocumentFragment, refs: TextRef[]) {
  const codes = new Map<string, TextRef>()
  const terms = new Map<string, TextRef>()
  for (const r of refs) {
    if (r.kind === 'glossary') terms.set(r.match.toLowerCase(), r)
    else codes.set(r.match, r)
  }
  const pass = (map: Map<string, TextRef>, flags: string, lower: boolean) => {
    if (map.size === 0) return
    const alts = [...map.keys()].sort((a, b) => b.length - a.length).map(escapeRe)
    const re = new RegExp(`(?<![\\w/:#@.-])(?:${alts.join('|')})(?![\\w-])`, flags)
    for (const n of textNodes(root)) {
      const text = n.data
      re.lastIndex = 0
      if (!re.test(text)) continue
      re.lastIndex = 0
      const out = document.createDocumentFragment()
      let last = 0
      for (const m of text.matchAll(re)) {
        const r = map.get(lower ? m[0].toLowerCase() : m[0])
        if (!r) continue
        const a = document.createElement('a')
        a.setAttribute('href', r.url)
        a.className = `ref ref-${r.kind}`
        a.title = [r.key, r.title].filter(Boolean).join(' · ')
        if (r.external) {
          a.setAttribute('target', '_blank')
          a.setAttribute('rel', 'noopener noreferrer')
        }
        a.textContent = m[0]
        out.append(text.slice(last, m.index), a)
        last = m.index + m[0].length
      }
      out.append(text.slice(last))
      n.replaceWith(out)
    }
  }
  pass(codes, 'g', false)
  pass(terms, 'gi', true)
}

const COPY_ICON =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect width="14" height="14" x="8" y="8" rx="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>'

// Fenced code gets a Copy button. It is added here, after DOMPurify (which
// forbids <button>), and works through the one delegated listener below.
function copyButtons(root: DocumentFragment) {
  for (const pre of Array.from(root.querySelectorAll('pre'))) {
    const wrap = document.createElement('div')
    wrap.className = 'code-block'
    pre.replaceWith(wrap)
    const b = document.createElement('button')
    b.type = 'button'
    b.className = 'code-copy'
    b.title = 'Copy code'
    b.setAttribute('aria-label', 'Copy code')
    b.innerHTML = COPY_ICON + '<span class="code-copy-label" aria-live="polite"></span>'
    wrap.append(pre, b)
  }
}

const copyTimers = new WeakMap<HTMLElement, number>()

function flash(btn: HTMLElement, state: 'copied' | 'failed') {
  const label = btn.querySelector('.code-copy-label')
  if (label) label.textContent = state === 'copied' ? 'Copied' : 'Copy failed'
  btn.dataset.state = state
  window.clearTimeout(copyTimers.get(btn))
  copyTimers.set(
    btn,
    window.setTimeout(() => {
      delete btn.dataset.state
      if (label) label.textContent = ''
    }, 1600),
  )
}

// Capture phase on the document: the click ends here, before the editor
// wrapper around rendered markdown can take it as "edit this text". A
// keyboard Enter or Space on the focused button arrives as this same click.
if (typeof document !== 'undefined') {
  document.addEventListener(
    'click',
    (e) => {
      const btn = (e.target as Element | null)?.closest?.<HTMLElement>('button.code-copy')
      if (!btn) return
      e.preventDefault()
      e.stopPropagation()
      const code = btn.parentElement?.querySelector('pre')?.textContent ?? ''
      copyText(code.replace(/\n$/, '')).then(
        () => flash(btn, 'copied'),
        () => flash(btn, 'failed'),
      )
    },
    true,
  )
}

// Text from an agent outside Waypoint (an assistant, WP-ADR1) may try to
// make the browser fetch a URL that carries what it read: its images become
// links, unless they are inline data. Links say where they go.
function untrustedImages(root: DocumentFragment) {
  for (const img of Array.from(root.querySelectorAll('img'))) {
    const src = img.getAttribute('src') ?? ''
    if (src.startsWith('data:image/') && !src.startsWith('data:image/svg')) continue
    const a = document.createElement('a')
    a.textContent = '🖼 ' + (img.getAttribute('alt') || src)
    if (/^https?:/i.test(src)) {
      a.setAttribute('href', src)
      a.setAttribute('target', '_blank')
      a.setAttribute('rel', 'noopener noreferrer nofollow')
    }
    img.replaceWith(a)
  }
  for (const a of Array.from(root.querySelectorAll('a[href^="http"]'))) {
    a.setAttribute('rel', 'noopener noreferrer nofollow')
    const href = a.getAttribute('href') ?? ''
    if (a.textContent?.trim() !== href) a.setAttribute('title', href)
  }
}

// The parse, template and sanitise steps both renderers share, so a SANITIZE
// change applies to both.
function sanitized(src: string, addAttr: string[] = []) {
  const html = marked.parse(src, { async: false }) as string
  const tpl = document.createElement('template')
  tpl.innerHTML = DOMPurify.sanitize(html, {
    ...SANITIZE,
    ADD_ATTR: [...SANITIZE.ADD_ATTR, ...addAttr],
  })
  return tpl
}

/**
 * Whether the markdown holds raw HTML (a block or inline `html` token, comments
 * included), which the sanitiser may strip. Conservative: any counts.
 */
export function hasRawHtml(src: string): boolean {
  const walk = (t: unknown): boolean => {
    if (Array.isArray(t)) return t.some(walk)
    if (!t || typeof t !== 'object') return false
    const o = t as Record<string, unknown>
    return o.type === 'html' || walk(o.tokens) || walk(o.items) || walk(o.header) || walk(o.rows)
  }
  return walk(marked.lexer(src))
}

/**
 * Markdown as sanitised HTML; `project` scopes [[wikilinks]]. `untrusted`
 * is for text an outside agent wrote: no remote images, no wikilinks.
 * `refs` is what the server resolved the text's codes to (refsQuery); once
 * given, only those are linked, before it any issue key is.
 */
export function renderMarkdown(
  src: string,
  opts: { project?: string; untrusted?: boolean; refs?: TextRef[] } = {},
): string {
  const tpl = sanitized(src)
  if (opts.untrusted) untrustedImages(tpl.content)
  else wikilinks(tpl.content, opts.project)
  if (opts.refs) linkRefs(tpl.content, opts.refs)
  linkify(tpl.content, !opts.refs)
  copyButtons(tpl.content)
  return tpl.innerHTML
}

// Editor copies of wikilinks and @mentions carry their markdown in this
// attribute, which htmlToMarkdown writes back as it was.
export const PIECE_SRC = 'data-md'
const PIECE_SRC_OK = /^(?:\[\[[^[\]\n]{1,420}\]\]|@[a-zA-Z0-9][\w.-]{0,38})$/

// [[wikilinks]] and @mentions in the editor are drawn as the view draws them
// (same elements, so the same width and the text after them stays put when
// the editor opens), but uneditable: the caret goes before or after one, and
// Backspace removes it whole. Issue keys are links in the view only, and a
// link is as wide as its text, so they stay text here.
function pieceEl(src: string, el: HTMLElement) {
  el.setAttribute('contenteditable', 'false')
  el.setAttribute(PIECE_SRC, src)
  return el
}

/**
 * The editor's piece for `src`, a wikilink's or a mention's markdown, as HTML
 * (null if it is neither). The one place a piece's markup is made, for text
 * already in the description and for what is typed or picked in the editor.
 */
export function pieceHtml(src: string): string | null {
  const w = new RegExp(`^${WIKILINK.source}$`).exec(src)
  if (w) {
    const target = w[1].trim()
    return target ? pieceEl(src, wikilinkEl(target, w[2])).outerHTML : null
  }
  return /^@[a-zA-Z0-9][\w.-]{0,38}$/.test(src)
    ? pieceEl(src, mentionEl(src.slice(1))).outerHTML
    : null
}

function editorPieces(root: DocumentFragment) {
  const re = new RegExp(`${WIKILINK.source}|${TOKEN.source}`, 'g')
  for (const n of textNodes(root)) {
    const text = n.data
    if (!text.includes('[[') && !text.includes('@')) continue
    const out = document.createDocumentFragment()
    let last = 0
    for (const m of text.matchAll(re)) {
      let at = m.index
      let src: string
      if (m[1] !== undefined) src = m[0]
      else if (m[5]) {
        src = '@' + mentionName(m[5])
        at += m[3].length
      } else continue // an issue key
      const html = pieceHtml(src)
      if (!html) continue
      const holder = document.createElement('template')
      holder.innerHTML = html
      out.append(text.slice(last, at), holder.content)
      last = at + src.length
    }
    if (last === 0) continue
    out.append(text.slice(last))
    n.replaceWith(out)
  }
}

/** The text a piece for `src` shows: a wikilink's label or target, a mention's "@name". */
export function pieceText(src: string): string {
  const w = new RegExp(`^${WIKILINK.source}$`).exec(src)
  return w ? (w[2] ?? '').trim() || w[1].trim() : src
}

/** Whether `src` is what an editor piece may carry (see PIECE_SRC). */
export const isPieceSrc = (src: string) => PIECE_SRC_OK.test(src)

/**
 * Markdown as HTML for a rich editor: formatting renders in place. Codes
 * stay as written; [[wikilinks]] and @mentions are uneditable pieces that
 * keep their markdown, so the text round-trips back to the same markdown.
 * Task checkboxes are the only form controls kept.
 */
export function renderMarkdownForEdit(src: string): string {
  const tpl = sanitized(src, ['contenteditable'])
  for (const input of Array.from(tpl.content.querySelectorAll('input'))) {
    if (input.getAttribute('type') === 'checkbox' && input.classList.contains('task')) {
      input.setAttribute('contenteditable', 'false')
    } else {
      input.remove()
    }
  }
  editorPieces(tpl.content)
  return tpl.innerHTML
}

const TASK = /^(\s*(?:>\s?)*\s*(?:[-*+]|\d+[.)])\s+)\[( |x|X)\]/
const FENCE = /^\s*(`{3,}|~{3,})/

/**
 * Toggle the nth task checkbox in markdown source. Tasks inside fenced code
 * don't render as checkboxes, so they are skipped. Returns null if the source
 * and the rendered checkboxes (count) disagree, rather than flip the wrong one.
 */
export function toggleTask(src: string, n: number, count: number): string | null {
  const lines = src.split('\n')
  const hits: number[] = []
  let fence = ''
  lines.forEach((l, i) => {
    const f = FENCE.exec(l)
    if (f) {
      if (!fence) fence = f[1][0]
      else if (f[1][0] === fence) fence = ''
      return
    }
    if (!fence && TASK.test(l)) hits.push(i)
  })
  if (hits.length !== count || n < 0 || n >= count) return null
  const i = hits[n]
  lines[i] = lines[i].replace(TASK, (_m, pre, mark) => `${pre}[${mark === ' ' ? 'x' : ' '}]`)
  return lines.join('\n')
}
