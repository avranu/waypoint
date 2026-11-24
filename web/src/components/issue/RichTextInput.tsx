// A what-you-see-is-what-you-get editor for task descriptions (WP-628): the
// markdown renders in place while it is edited, and goes back to markdown on
// every change. Opening and closing it leaves the text as it was; an edit
// writes the whole description in the editor's canonical markdown, which
// renders the same. @mentions and pasted or dropped files work as they do in
// the plain textarea.

import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bold,
  Code,
  FileCode,
  Heading1,
  Heading2,
  Heading3,
  Italic,
  List,
  ListOrdered,
  ListTodo,
  Quote,
  Redo2,
  SquareCode,
  Strikethrough,
  Undo2,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { IconButton } from '@/components/ui/button'
import { useEscapeLayer } from '@/components/ui/layers'
import { toast, toastError } from '@/components/ui/toast'
import { issueTarget, refreshAttachments, uploadAttachment } from '@/lib/attachments'
import { renderMarkdownForEdit } from '@/lib/markdown'
import { htmlToMarkdown } from '@/lib/richtext'
import { inputRules, keyRules } from '@/lib/richdom'
import type { UserRef } from '@/lib/types'
import { MENTION_RE, MentionList, useMentionMatches } from './userMention'

type Props = {
  value: string
  onValueChange: (v: string) => void
  issueKey?: string
  attachTo?: string
  placeholder?: string
  autoFocus?: boolean
  ariaLabel?: string
  /**
   * The newest text of the draft, wherever it is edited now. An upload that
   * finishes after this editor is gone (the person switched to markdown, or
   * closed it) puts its link in the draft through this and onValueChange.
   */
  getValue?: () => string
  onKeyDown?: (e: React.KeyboardEvent) => void
  /** Switches to the markdown source; a button at the toolbar's end. */
  onSource?: () => void
  /** Shown at the toolbar's right end: Done and the save state. */
  trailing?: React.ReactNode
  /**
   * Where the person clicked to open the editor over the text it replaces
   * (viewport coordinates): the caret goes there, and nothing scrolls.
   */
  caretAt?: { x: number; y: number }
}

// What an editor holds when it is empty: typing needs a paragraph to go in, or
// the browser leaves the first line as bare text that no rule can see.
const EMPTY = '<p><br></p>'

type Mention = { node: Text; start: number; end: number; text: string }

export function RichTextInput({
  value,
  onValueChange,
  issueKey,
  attachTo,
  placeholder,
  autoFocus,
  ariaLabel,
  getValue,
  onKeyDown,
  onSource,
  trailing,
  caretAt,
}: Props) {
  const qc = useQueryClient()
  const target = attachTo ?? (issueKey ? issueTarget(issueKey) : undefined)
  const editor = useRef<HTMLDivElement | null>(null)
  // The editor is full when React puts it in the page. React puts focus back on
  // the element that had it right after that, before layout effects run, and
  // with an empty editor the page is short for that moment: the focus clamps
  // the scroll position to the top and the click would move everything.
  const initial = useRef<string | null>(null)
  if (initial.current === null) initial.current = renderMarkdownForEdit(value) || EMPTY
  // The markdown the DOM currently shows. An outside change that differs from
  // this is the only reason to replace the DOM under the person typing.
  const shown = useRef(value)
  // An empty description gets room to write in; one with text is the size of
  // its text, as the page showed it, so opening it moves nothing.
  const [roomy] = useState(() => !value.trim())
  const users = useQuery(q.users()).data ?? []
  const [mention, setMention] = useState<Mention | null>(null)
  const [active, setActive] = useState(0)
  const [dragging, setDragging] = useState(false)
  const internalDrag = useRef(false)

  const matches = useMentionMatches(users, mention?.text)
  const open = !!mention && matches.length > 0
  useEscapeLayer(open, () => setMention(null))
  useEffect(() => setActive(0), [mention?.text])

  const sync = () => {
    const el = editor.current
    if (!el) return
    // A browser leaves a <br> or an empty <p> behind when everything is
    // deleted; that goes. An empty heading, quote or list is the block the
    // person just asked for, and stays.
    if (blank(el) && el.innerHTML !== EMPTY) {
      el.innerHTML = EMPTY
      if (el.contains(document.activeElement)) placeCaretAtEnd(el)
    }
    // A ticked box changes the property, not the attribute innerHTML shows.
    for (const box of Array.from(el.querySelectorAll<HTMLInputElement>('input[type=checkbox]')))
      box.toggleAttribute('checked', box.checked)
    const md = htmlToMarkdown(el.innerHTML)
    shown.current = md
    latest.current = md
    onValueChange(md)
  }

  // Render the starting markdown once, and again whenever it changes from
  // outside (an upload's link put in the draft).
  useLayoutEffect(() => {
    const el = editor.current
    if (!el) return
    shown.current = value
    try {
      document.execCommand('defaultParagraphSeparator', false, 'p')
      document.execCommand('styleWithCSS', false, 'false')
    } catch {
      // Older browsers ignore these; the browser's own blocks still serialize.
    }
    if (autoFocus) {
      // Focusing scrolls the caret into view, which is what opening by the
      // keyboard wants; a click opened the editor where the person already is.
      el.focus({ preventScroll: !!caretAt })
      const at = caretAt && pointRange(caretAt.x, caretAt.y)
      if (at && el.contains(at.startContainer)) {
        const sel = window.getSelection()
        sel?.removeAllRanges()
        sel?.addRange(at)
      } else placeCaretAtEnd(el)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- starting value only
  }, [])

  // The editor's own changes come back equal to `shown`. Anything else is
  // newer than the DOM, so it is shown even under the caret: left out, the
  // next keystroke would serialize the old DOM over it. The caret keeps its
  // place in the text.
  useEffect(() => {
    const el = editor.current
    if (!el || value === shown.current) return
    const caret = el.contains(document.activeElement) ? caretOffset(el) : null
    el.innerHTML = renderMarkdownForEdit(value) || EMPTY
    shown.current = value
    if (caret !== null) placeCaretAt(el, caret)
  }, [value])

  // What the editor last wrote out; the draft's newest text while mounted.
  const latest = useRef(value)
  latest.current = value
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const draftNow = () => (getValue ? getValue() : latest.current)

  const detect = () => {
    const sel = window.getSelection()
    if (!sel || sel.rangeCount === 0) return setMention(null)
    const range = sel.getRangeAt(0)
    if (!range.collapsed) return setMention(null)
    const node = range.startContainer
    if (node.nodeType !== Node.TEXT_NODE) return setMention(null)
    const before = (node as Text).data.slice(0, range.startOffset)
    const m = MENTION_RE.exec(before)
    if (!m) return setMention(null)
    setMention({
      node: node as Text,
      start: range.startOffset - m[2].length - 1,
      end: range.startOffset,
      text: m[2],
    })
  }

  const pick = (u: UserRef) => {
    if (!mention) return
    const { node, start, end } = mention
    const insert = `@${u.username} `
    node.data = node.data.slice(0, start) + insert + node.data.slice(end)
    const caret = start + insert.length
    const range = document.createRange()
    range.setStart(node, caret)
    range.collapse(true)
    const sel = window.getSelection()
    sel?.removeAllRanges()
    sel?.addRange(range)
    setMention(null)
    sync()
    editor.current?.focus()
  }

  const block = (selector: string): HTMLElement | null => {
    const sel = window.getSelection()
    if (!sel || sel.rangeCount === 0) return null
    const node = sel.getRangeAt(0).startContainer
    const start = node.nodeType === Node.ELEMENT_NODE ? (node as Element) : node.parentElement
    const found = start?.closest(selector) as HTMLElement | null
    return found && editor.current?.contains(found) ? found : null
  }

  const run = (cmd: string, arg?: string) => {
    editor.current?.focus()
    document.execCommand(cmd, false, arg)
    sync()
    detect()
  }

  // Inline code and strikethrough have no execCommand; wrap the selection.
  const wrap = (tag: 'code' | 's') => {
    const el = editor.current
    if (!el) return
    el.focus()
    const sel = window.getSelection()
    if (!sel || sel.rangeCount === 0) return
    const range = sel.getRangeAt(0)
    if (range.collapsed) return
    let node: Node | null = range.commonAncestorContainer
    let existing: HTMLElement | null = null
    while (node && node !== el) {
      if (
        node.nodeType === Node.ELEMENT_NODE &&
        (node as HTMLElement).tagName.toLowerCase() === tag
      ) {
        existing = node as HTMLElement
        break
      }
      node = node.parentNode
    }
    if (existing) {
      const parent = existing.parentNode
      if (parent) {
        while (existing.firstChild) parent.insertBefore(existing.firstChild, existing)
        parent.removeChild(existing)
      }
    } else {
      const wrapEl = document.createElement(tag)
      try {
        range.surroundContents(wrapEl)
      } catch {
        wrapEl.appendChild(range.extractContents())
        range.insertNode(wrapEl)
      }
      sel.removeAllRanges()
      const after = document.createRange()
      after.selectNodeContents(wrapEl)
      sel.addRange(after)
    }
    sync()
  }

  const heading = (n: number) => {
    const current = block('h1,h2,h3,h4,h5,h6')
    run('formatBlock', current?.tagName.toLowerCase() === `h${n}` ? 'p' : `h${n}`)
  }

  const quote = () => run('formatBlock', block('blockquote') ? 'p' : 'blockquote')

  const codeBlock = () => run('formatBlock', block('pre') ? 'p' : 'pre')

  const taskList = () => {
    const el = editor.current
    if (!el) return
    el.focus()
    let li = block('li')
    if (!li) {
      document.execCommand('insertUnorderedList')
      li = block('li')
    }
    if (!li) return
    if (!Array.from(li.children).some((c) => c.tagName.toLowerCase() === 'input')) {
      const box = document.createElement('input')
      box.type = 'checkbox'
      box.className = 'task'
      box.contentEditable = 'false'
      li.insertBefore(box, li.firstChild)
      li.insertBefore(document.createTextNode(' '), box.nextSibling)
    }
    sync()
  }

  const upload = (files: File[]) => {
    const el = editor.current
    if (!target || !files.length || !el) return
    el.focus()
    const sel = window.getSelection()
    let range = sel && sel.rangeCount ? sel.getRangeAt(0) : null
    if (!range || !el.contains(range.commonAncestorContainer)) {
      range = document.createRange()
      range.selectNodeContents(el)
      range.collapse(false)
    }
    const nodes: Text[] = []
    const placeholders = files.map((f) => `[Uploading ${f.name || 'pasted image'}…]()`)
    files.forEach((_f, i) => {
      if (i > 0) {
        const br = document.createElement('br')
        range!.insertNode(br)
        range!.setStartAfter(br)
      }
      const placeholder = document.createTextNode(placeholders[i])
      range!.insertNode(placeholder)
      range!.setStartAfter(placeholder)
      nodes.push(placeholder)
    })
    range.collapse(false)
    sel?.removeAllRanges()
    sel?.addRange(range)
    sync()
    // The editor may be gone when an upload ends (switched to markdown, done,
    // Escape), or the placeholder deleted; then the draft is what to edit.
    const finish = (i: number, markdown: string | null) => {
      const ph = placeholders[i]
      const node = nodes[i]
      // Text typed beside the placeholder joins its node: cut the
      // placeholder out and leave the rest alone. If it was edited, fall
      // through to the draft.
      const at0 = mounted.current && node.isConnected ? node.data.indexOf(ph) : -1
      if (at0 >= 0) {
        const mid = at0 > 0 ? node.splitText(at0) : node
        if (mid.data.length > ph.length) mid.splitText(ph.length)
        if (markdown === null) mid.remove()
        else {
          const holder = document.createElement('span')
          holder.innerHTML = renderMarkdownForEdit(markdown)
          mid.replaceWith(...Array.from(holder.childNodes))
        }
        sync()
        return true
      }
      // The draft holds the placeholder as the serializer wrote it (escaped),
      // or as typed if markdown mode has rewritten it. While this editor is
      // open, the change comes back as `value` and the effect above shows it.
      const v = draftNow()
      const at = [ph, asWritten(ph)].find((p) => v.includes(p))
      if (!at) return false
      const next = v.replace(at, () => markdown ?? '')
      latest.current = next
      onValueChange(next)
      return true
    }
    files.forEach((f, i) => {
      uploadAttachment(target, f).then(
        (a) => {
          refreshAttachments(qc, target)
          if (!finish(i, a.markdown))
            toast(`${f.name || 'The file'} was attached but not inserted: its place was deleted.`)
        },
        (err) => {
          finish(i, null)
          toastError(err)
        },
      )
    })
  }

  const tool = (label: string, icon: React.ReactNode, onClick: () => void) => (
    <IconButton size="xs" label={label} onMouseDown={(e) => e.preventDefault()} onClick={onClick}>
      {icon}
    </IconButton>
  )

  return (
    <div
      className="relative"
      onDragOver={(e) => {
        if (!target || !e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDragStart={() => (internalDrag.current = true)}
      onDragEnd={() => (internalDrag.current = false)}
      onDrop={(e) => {
        const inside = internalDrag.current
        internalDrag.current = false
        setDragging(false)
        if (target && e.dataTransfer.files.length) {
          e.preventDefault()
          upload([...e.dataTransfer.files])
          return
        }
        // Text dragged in from elsewhere is plain text, as a paste is; moving
        // text around inside the editor is the browser's.
        const el = editor.current
        const text = e.dataTransfer.getData('text/plain')
        if (inside || !text || !el || !el.contains(e.target as Node)) return
        e.preventDefault()
        const at = pointRange(e.clientX, e.clientY)
        if (at && el.contains(at.startContainer)) {
          const sel = window.getSelection()
          sel?.removeAllRanges()
          sel?.addRange(at)
        }
        el.focus()
        document.execCommand('insertText', false, text)
        sync()
      }}
    >
      {/* The toolbar takes no room: a zero-height wrapper that sticks under
          the page's header (3rem) with the toolbar (2.25rem and a gap) above
          its line, so it floats over what is above the text and follows it
          down a long description. */}
      <div className="sticky top-[5.5rem] z-20 h-0">
        <div className="absolute inset-x-0 bottom-full mb-1 flex h-9 items-center rounded-lg border border-line bg-panel shadow-pop">
          <div
            role="toolbar"
            aria-label="Formatting"
            className="no-scrollbar flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto px-1"
          >
            {tool('Bold', <Bold className="size-3.5" />, () => run('bold'))}
            {tool('Italic', <Italic className="size-3.5" />, () => run('italic'))}
            {tool('Strikethrough', <Strikethrough className="size-3.5" />, () => wrap('s'))}
            {tool('Inline code', <Code className="size-3.5" />, () => wrap('code'))}
            <span className="mx-1 h-4 w-px shrink-0 bg-line" />
            {tool('Heading 1', <Heading1 className="size-3.5" />, () => heading(1))}
            {tool('Heading 2', <Heading2 className="size-3.5" />, () => heading(2))}
            {tool('Heading 3', <Heading3 className="size-3.5" />, () => heading(3))}
            <span className="mx-1 h-4 w-px shrink-0 bg-line" />
            {tool('Bulleted list', <List className="size-3.5" />, () => run('insertUnorderedList'))}
            {tool('Numbered list', <ListOrdered className="size-3.5" />, () =>
              run('insertOrderedList'),
            )}
            {tool('Task list', <ListTodo className="size-3.5" />, taskList)}
            {tool('Quote', <Quote className="size-3.5" />, quote)}
            {tool('Code block', <SquareCode className="size-3.5" />, codeBlock)}
            <span className="mx-1 h-4 w-px shrink-0 bg-line" />
            {tool('Undo', <Undo2 className="size-3.5" />, () => run('undo'))}
            {tool('Redo', <Redo2 className="size-3.5" />, () => run('redo'))}
            {onSource && (
              <>
                <span className="mx-1 h-4 w-px shrink-0 bg-line" />
                {tool('Edit as markdown', <FileCode className="size-3.5" />, onSource)}
              </>
            )}
          </div>
          {trailing && (
            <div className="flex shrink-0 items-center gap-2 border-l border-line px-2">
              {trailing}
            </div>
          )}
        </div>
      </div>
      <div
        ref={editor}
        role="textbox"
        aria-multiline="true"
        aria-label={ariaLabel}
        data-placeholder={placeholder}
        contentEditable
        suppressContentEditableWarning
        dangerouslySetInnerHTML={{ __html: initial.current }}
        spellCheck
        style={{ '--rich-placeholder': JSON.stringify(placeholder ?? '') } as React.CSSProperties}
        // The same box as the rendered text it replaces (Description.tsx), so
        // opening it moves nothing; focus and dragging show as a ring, which
        // takes no room.
        className={clsx(
          'rich-editor prose-wp -mx-1 rounded-lg px-1 py-1 text-sm leading-relaxed outline-none transition-shadow focus:ring-2 focus:ring-accent/25',
          roomy && 'min-h-24',
          dragging && 'ring-2 ring-accent/40',
        )}
        onInput={(e) => {
          const el = editor.current
          if (el) inputRules(el, e.nativeEvent as InputEvent)
          sync()
          detect()
        }}
        onClick={(e) => {
          const t = e.target as HTMLElement
          if (t instanceof HTMLInputElement && t.type === 'checkbox') {
            // The browser toggles the box; save it after it does.
            window.setTimeout(sync, 0)
            return
          }
          detect()
        }}
        onKeyUp={(e) => {
          if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) detect()
        }}
        onKeyDown={(e) => {
          // Markdown has no underline; the shortcut would make one that
          // vanishes on save.
          if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'u') {
            e.preventDefault()
            return
          }
          if (open) {
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
              e.preventDefault()
              const d = e.key === 'ArrowDown' ? 1 : -1
              setActive((a) => (a + d + matches.length) % matches.length)
              return
            }
            if ((e.key === 'Enter' && !e.metaKey && !e.ctrlKey && !e.shiftKey) || e.key === 'Tab') {
              e.preventDefault()
              pick(matches[active])
              return
            }
            if (e.key === 'Escape') {
              e.preventDefault()
              e.stopPropagation()
              setMention(null)
              return
            }
          }
          const el = editor.current
          if (el && keyRules(el, e.nativeEvent)) {
            sync()
            detect()
            return
          }
          onKeyDown?.(e)
        }}
        onBlur={() => setTimeout(() => setMention(null), 150)}
        onPaste={(e) => {
          const files = [...e.clipboardData.files]
          if (target && files.length) {
            e.preventDefault()
            upload(files)
            return
          }
          const text = e.clipboardData.getData('text/plain')
          if (text) {
            e.preventDefault()
            document.execCommand('insertText', false, text)
            sync()
          }
        }}
      />
      {open && editor.current && (
        <MentionMenu
          anchor={editor.current}
          mention={mention}
          users={matches}
          active={active}
          onHover={setActive}
          onPick={pick}
        />
      )}
    </div>
  )
}

function empty(el: HTMLElement) {
  return !el.textContent?.trim() && !el.querySelector('img,video,hr,input,pre,table')
}

/** Plain text as htmlToMarkdown writes it, escapes included. */
function asWritten(text: string) {
  const p = document.createElement('p')
  p.textContent = text
  return htmlToMarkdown(p.outerHTML)
}

/** True when nothing is in the editor, not even an empty heading or list. */
function blank(el: HTMLElement) {
  return empty(el) && !el.querySelector('h1,h2,h3,h4,h5,h6,blockquote,ul,ol,li,pre,table,hr')
}

function pointRange(x: number, y: number): Range | null {
  const doc = document as Document & {
    caretRangeFromPoint?: (x: number, y: number) => Range | null
    caretPositionFromPoint?: (x: number, y: number) => { offsetNode: Node; offset: number } | null
  }
  if (doc.caretRangeFromPoint) return doc.caretRangeFromPoint(x, y)
  const pos = doc.caretPositionFromPoint?.(x, y)
  if (!pos) return null
  const r = document.createRange()
  r.setStart(pos.offsetNode, pos.offset)
  r.collapse(true)
  return r
}

/** Characters of text before the caret, or null if it isn't in `el`. */
function caretOffset(el: HTMLElement): number | null {
  const sel = window.getSelection()
  if (!sel || sel.rangeCount === 0) return null
  const r = sel.getRangeAt(0)
  if (!el.contains(r.startContainer)) return null
  const before = document.createRange()
  before.selectNodeContents(el)
  before.setEnd(r.startContainer, r.startOffset)
  return before.toString().length
}

/** Puts the caret `n` characters of text into `el`, or at its end. */
function placeCaretAt(el: HTMLElement, n: number) {
  const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT)
  let left = n
  for (let t = walk.nextNode() as Text | null; t; t = walk.nextNode() as Text | null) {
    if (left <= t.data.length) {
      const range = document.createRange()
      range.setStart(t, left)
      range.collapse(true)
      const sel = window.getSelection()
      sel?.removeAllRanges()
      sel?.addRange(range)
      return
    }
    left -= t.data.length
  }
  placeCaretAtEnd(el)
}

function placeCaretAtEnd(el: HTMLElement) {
  // Inside the last block, where typing goes: a caret between blocks leaves
  // the typing rules unable to tell what is being typed in.
  let n: Node = el
  while (n.lastChild && !VOID.has(n.lastChild.nodeName)) n = n.lastChild
  const range = document.createRange()
  if (n.nodeType === Node.TEXT_NODE) range.setStart(n, (n as Text).data.length)
  else
    range.setStart(
      n,
      n.lastChild?.nodeName === 'BR' ? n.childNodes.length - 1 : n.childNodes.length,
    )
  range.collapse(true)
  const sel = window.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(range)
}

const VOID = new Set(['BR', 'HR', 'IMG', 'INPUT', 'VIDEO', 'TABLE'])

function MentionMenu({
  anchor,
  mention,
  users,
  active,
  onHover,
  onPick,
}: {
  anchor: HTMLElement
  mention: Mention
  users: UserRef[]
  active: number
  onHover: (i: number) => void
  onPick: (u: UserRef) => void
}) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  useLayoutEffect(() => {
    const range = document.createRange()
    range.setStart(mention.node, mention.start)
    range.setEnd(mention.node, mention.end)
    const r = range.getBoundingClientRect()
    const box = anchor.getBoundingClientRect()
    const left = Math.max(8, Math.min(r.left || box.left, window.innerWidth - 248))
    let top = (r.bottom || box.top) + 4
    if (top + 220 > window.innerHeight) top = Math.max(8, (r.top || box.top) - 220)
    setPos({ top, left })
  }, [anchor, mention])
  return <MentionList users={users} active={active} pos={pos} onHover={onHover} onPick={onPick} />
}
