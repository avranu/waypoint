// A what-you-see-is-what-you-get editor for task descriptions (WP-628): the
// markdown renders in place while it is edited, and goes back to markdown on
// every change, so the stored text is unchanged. @mentions and pasted or
// dropped files work as they do in the plain textarea.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
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
import { Avatar } from '@/components/ui/misc'
import { IconButton } from '@/components/ui/button'
import { useEscapeLayer } from '@/components/ui/layers'
import { toastError } from '@/components/ui/toast'
import { issueTarget, refreshAttachments, uploadAttachment } from '@/lib/attachments'
import { renderMarkdownForEdit } from '@/lib/markdown'
import { htmlToMarkdown } from '@/lib/richtext'
import type { UserRef } from '@/lib/types'

type Props = {
  value: string
  onValueChange: (v: string) => void
  issueKey?: string
  attachTo?: string
  placeholder?: string
  autoFocus?: boolean
  ariaLabel?: string
  onKeyDown?: (e: React.KeyboardEvent) => void
  /** Switches to the markdown source; a button at the toolbar's end. */
  onSource?: () => void
}

type Mention = { node: Text; start: number; end: number; text: string }

const MENTION_RE = /(^|[\s(])@([\w.-]{0,39})$/

export function RichTextInput({
  value,
  onValueChange,
  issueKey,
  attachTo,
  placeholder,
  autoFocus,
  ariaLabel,
  onKeyDown,
  onSource,
}: Props) {
  const qc = useQueryClient()
  const target = attachTo ?? (issueKey ? issueTarget(issueKey) : undefined)
  const editor = useRef<HTMLDivElement | null>(null)
  // The markdown the DOM currently shows. An outside change that differs from
  // this is the only reason to replace the DOM under the person typing.
  const shown = useRef(value)
  const users = useQuery(q.users()).data ?? []
  const [mention, setMention] = useState<Mention | null>(null)
  const [active, setActive] = useState(0)
  const [dragging, setDragging] = useState(false)

  const matches = useMemo(() => {
    if (!mention) return []
    const t = mention.text.toLowerCase()
    return users
      .filter(
        (u) =>
          u.username.toLowerCase().startsWith(t) ||
          u.display_name
            .toLowerCase()
            .split(/\s+/)
            .some((w) => w.startsWith(t)),
      )
      .slice(0, 6)
  }, [mention, users])
  const open = !!mention && matches.length > 0
  useEscapeLayer(open, () => setMention(null))
  useEffect(() => setActive(0), [mention?.text])

  const sync = () => {
    const el = editor.current
    if (!el) return
    if (empty(el) && el.innerHTML !== '') el.innerHTML = ''
    // A ticked box changes the property, not the attribute innerHTML shows.
    for (const box of Array.from(el.querySelectorAll<HTMLInputElement>('input[type=checkbox]')))
      box.toggleAttribute('checked', box.checked)
    const md = htmlToMarkdown(el.innerHTML)
    shown.current = md
    onValueChange(md)
  }

  // Render the starting markdown once, and again only when it changes while
  // the editor isn't focused (a draft restored, a conflict overwritten).
  useLayoutEffect(() => {
    const el = editor.current
    if (!el) return
    try {
      document.execCommand('defaultParagraphSeparator', false, 'p')
      document.execCommand('styleWithCSS', false, 'false')
    } catch {
      // Older browsers ignore these; the browser's own blocks still serialize.
    }
    el.innerHTML = renderMarkdownForEdit(value)
    shown.current = value
    if (autoFocus) {
      el.focus()
      placeCaretAtEnd(el)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- starting value only
  }, [])

  useEffect(() => {
    const el = editor.current
    if (!el || value === shown.current) return
    if (el.contains(document.activeElement)) return
    el.innerHTML = renderMarkdownForEdit(value)
    shown.current = value
  }, [value])

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
    files.forEach((f, i) => {
      if (i > 0) {
        const br = document.createElement('br')
        range!.insertNode(br)
        range!.setStartAfter(br)
      }
      const placeholder = document.createTextNode(`[Uploading ${f.name || 'pasted image'}…]()`)
      range!.insertNode(placeholder)
      range!.setStartAfter(placeholder)
      nodes.push(placeholder)
    })
    range.collapse(false)
    sel?.removeAllRanges()
    sel?.addRange(range)
    sync()
    files.forEach((f, i) => {
      uploadAttachment(target, f).then(
        (a) => {
          const holder = document.createElement('span')
          holder.innerHTML = renderMarkdownForEdit(a.markdown)
          nodes[i].replaceWith(...Array.from(holder.childNodes))
          refreshAttachments(qc, target)
          sync()
        },
        (err) => {
          nodes[i].remove()
          toastError(err)
          sync()
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
      className={clsx(
        'overflow-hidden rounded-lg border border-line bg-panel transition-colors focus-within:border-accent focus-within:ring-2 focus-within:ring-accent/20',
        dragging && 'border-accent ring-2 ring-accent/20',
      )}
      onDragOver={(e) => {
        if (!target || !e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        if (!target || !e.dataTransfer.files.length) return
        e.preventDefault()
        setDragging(false)
        upload([...e.dataTransfer.files])
      }}
    >
      <div
        role="toolbar"
        aria-label="Formatting"
        className="flex flex-wrap items-center gap-0.5 border-b border-line px-1 py-1"
      >
        {tool('Bold', <Bold className="size-3.5" />, () => run('bold'))}
        {tool('Italic', <Italic className="size-3.5" />, () => run('italic'))}
        {tool('Strikethrough', <Strikethrough className="size-3.5" />, () => wrap('s'))}
        {tool('Inline code', <Code className="size-3.5" />, () => wrap('code'))}
        <span className="mx-1 h-4 w-px bg-line" />
        {tool('Heading 1', <Heading1 className="size-3.5" />, () => heading(1))}
        {tool('Heading 2', <Heading2 className="size-3.5" />, () => heading(2))}
        {tool('Heading 3', <Heading3 className="size-3.5" />, () => heading(3))}
        <span className="mx-1 h-4 w-px bg-line" />
        {tool('Bulleted list', <List className="size-3.5" />, () => run('insertUnorderedList'))}
        {tool('Numbered list', <ListOrdered className="size-3.5" />, () =>
          run('insertOrderedList'),
        )}
        {tool('Task list', <ListTodo className="size-3.5" />, taskList)}
        {tool('Quote', <Quote className="size-3.5" />, quote)}
        {tool('Code block', <SquareCode className="size-3.5" />, codeBlock)}
        <span className="mx-1 h-4 w-px bg-line" />
        {tool('Undo', <Undo2 className="size-3.5" />, () => run('undo'))}
        {tool('Redo', <Redo2 className="size-3.5" />, () => run('redo'))}
        {onSource && (
          <span className="ml-auto">
            {tool('Edit as markdown', <FileCode className="size-3.5" />, onSource)}
          </span>
        )}
      </div>
      <div
        ref={editor}
        role="textbox"
        aria-multiline="true"
        aria-label={ariaLabel}
        data-placeholder={placeholder}
        contentEditable
        suppressContentEditableWarning
        spellCheck
        className="rich-editor prose-wp min-h-24 px-3 py-2 text-sm leading-relaxed focus:outline-none"
        onInput={() => {
          sync()
          detect()
        }}
        onClick={(e) => {
          const t = e.target as HTMLElement
          if (t instanceof HTMLInputElement && t.type === 'checkbox') {
            // The browser toggles the box; save it after it does.
            window.setTimeout(sync, 0)
          }
        }}
        onKeyUp={(e) => {
          if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) detect()
        }}
        onKeyDown={(e) => {
          if (open) {
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
              e.preventDefault()
              const d = e.key === 'ArrowDown' ? 1 : -1
              setActive((a) => (a + d + matches.length) % matches.length)
              return
            }
            if ((e.key === 'Enter' && !e.metaKey && !e.ctrlKey) || e.key === 'Tab') {
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

function placeCaretAtEnd(el: HTMLElement) {
  const range = document.createRange()
  range.selectNodeContents(el)
  range.collapse(false)
  const sel = window.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(range)
}

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
  return createPortal(
    <div
      role="listbox"
      aria-label="Mention someone"
      style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999 }}
      className="animate-pop fixed z-[70] w-60 overflow-hidden rounded-xl border border-line bg-raised p-1 shadow-pop"
      onMouseDown={(e) => e.preventDefault()}
    >
      {users.map((u, i) => (
        <button
          key={u.id}
          role="option"
          aria-selected={i === active}
          onMouseEnter={() => onHover(i)}
          onClick={() => onPick(u)}
          className={clsx(
            'flex h-8 w-full items-center gap-2 rounded-lg px-2 text-left text-[13px]',
            i === active && 'bg-hover',
          )}
        >
          <Avatar user={u} size={18} />
          <span className="truncate">{u.display_name}</span>
          <span className="ml-auto shrink-0 text-xs text-subtle">@{u.username}</span>
        </button>
      ))}
    </div>,
    document.body,
  )
}
