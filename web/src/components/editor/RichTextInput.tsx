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
  Link as LinkIcon,
  List,
  ListOrdered,
  ListTodo,
  Quote,
  Redo2,
  SquareCode,
  Strikethrough,
  Undo2,
} from 'lucide-react'
import { getMarkRange, type Editor } from '@tiptap/core'
import { DOMParser, type Schema } from '@tiptap/pm/model'
import { Selection, TextSelection } from '@tiptap/pm/state'
import { EditorContent, useEditor, useEditorState } from '@tiptap/react'
import { q } from '@/lib/queries'
import { Button, IconButton } from '@/components/ui/button'
import { fieldBoxLook, Input } from '@/components/ui/input'
import { useEscapeLayer } from '@/components/ui/layers'
import { Popover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import {
  issueTarget,
  refreshAttachments,
  uploadAttachment,
  uploadPlaceholder,
} from '@/lib/attachments'
import { toEditorHtml, fromEditorHtml } from '@/lib/editor-html'
import { looksLikeMarkdown } from '@/lib/looksLikeMarkdown'
import { hasRawHtml, pieceHtml } from '@/lib/markdown'
import { htmlToMarkdown, SAFE_HREF } from '@/lib/richtext'
import type { UserRef } from '@/lib/types'
import { MENTION_RE, MentionList, useMentionMatches } from '../issue/userMention'
import { editorExtensions } from './schema'
import { DESCRIPTION_BOX } from './styles'

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
  /**
   * 'inline' is the editor in place of rendered text, its toolbar floating
   * above; 'field' is a form field with the toolbar inside its border.
   */
  look?: 'inline' | 'field'
  /** The 'field' look's least height, in lines. */
  minRows?: number
  disabled?: boolean
  /**
   * For a form with no attachment target yet: a pasted or dropped file is not
   * uploaded; this returns the address (an object URL) it shows at, and the
   * caller uploads it later.
   */
  deferUploads?: (file: File) => string
  /** On the outer box. */
  className?: string
}

type Mention = { from: number; to: number; text: string }
type LinkDraft = { from: number; to: number; href: string; text: string | null }

/** Markdown parsed into the schema as a slice, the way a paste of it is. */
function markdownSlice(schema: Schema, md: string, blob = false) {
  const t = document.createElement('template')
  t.innerHTML = toEditorHtml(md, { blob })
  return DOMParser.fromSchema(schema).parseSlice(t.content)
}

/**
 * Whether copied HTML says more than its plain text (links, emphasis, headings,
 * lists, tables). A code editor's coloured spans around markdown source don't.
 */
function hasFormatting(html: string) {
  if (!html) return false
  const { body } = new window.DOMParser().parseFromString(html, 'text/html')
  return !!body.querySelector('a[href], strong, b, em, i, h1, h2, h3, h4, h5, h6, ul, ol, table')
}

/** Plain text as htmlToMarkdown writes it, escapes included. */
function asWritten(text: string) {
  const p = document.createElement('p')
  p.textContent = text
  return htmlToMarkdown(p.outerHTML)
}

function findText(editor: Editor, text: string): number {
  let at = -1
  editor.state.doc.descendants((node, pos) => {
    if (at >= 0) return false
    if (node.isText) {
      const i = node.text!.indexOf(text)
      if (i >= 0) at = pos + i
    }
    return true
  })
  return at
}

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
  look = 'inline',
  minRows = 3,
  disabled,
  deferUploads,
  className,
}: Props) {
  const qc = useQueryClient()
  const target = attachTo ?? (issueKey ? issueTarget(issueKey) : undefined)
  const field = look === 'field'
  const box = useRef<HTMLDivElement | null>(null)
  const scrolled = useRef<[Element, number][] | null>(null)
  if (scrolled.current === null) {
    scrolled.current = []
    const hit = caretAt && document.elementFromPoint(caretAt.x, caretAt.y)
    for (let n = hit || null; n; n = n.parentElement) {
      if (n.scrollTop > 0) scrolled.current.push([n, n.scrollTop])
    }
  }
  // The newest markdown this editor has given its caller (or been given): a
  // value that differs from it came from elsewhere.
  const last = useRef(value)
  const mounted = useRef(true)
  const props = useRef({ onValueChange, onKeyDown, target, getValue, deferUploads })
  props.current = { onValueChange, onKeyDown, target, getValue, deferUploads }

  // The formatting toolbar's row scrolls sideways when it doesn't fit; which
  // edge has more buttons past it shows as a fade.
  const bar = useRef<HTMLDivElement | null>(null)
  const [fade, setFade] = useState({ left: false, right: false })
  const measureBar = () => {
    const b = bar.current
    if (!b) return
    const left = b.scrollLeft > 1
    const right = b.scrollLeft + b.clientWidth < b.scrollWidth - 1
    setFade((f) => (f.left === left && f.right === right ? f : { left, right }))
  }
  useLayoutEffect(measureBar)
  useEffect(() => {
    const b = bar.current
    if (!b) return
    const ro = new ResizeObserver(measureBar)
    ro.observe(b)
    window.addEventListener('resize', measureBar)
    return () => {
      ro.disconnect()
      window.removeEventListener('resize', measureBar)
    }
  }, [disabled])

  const users = useQuery(q.users()).data ?? []
  const [mention, setMention] = useState<Mention | null>(null)
  const [active, setActive] = useState(0)
  const [menuPos, setMenuPos] = useState<{ top: number; left: number } | null>(null)
  const [dragging, setDragging] = useState(false)
  const [linking, setLinking] = useState<LinkDraft | null>(null)
  const matches = useMentionMatches(users, mention?.text)
  const menuOpen = !!mention && matches.length > 0
  const live = useRef({ menuOpen, matches, active, mention })
  live.current = { menuOpen, matches, active, mention }
  useEscapeLayer(menuOpen, () => setMention(null))
  useEffect(() => setActive(0), [mention?.text])

  const editorRef = useRef<Editor | null>(null)
  const pasteLiteral = useRef(false)

  const openLink = () => {
    const ed = editorRef.current
    if (!ed || !ed.isEditable) return
    const { selection, schema, doc } = ed.state
    let { from, to } = selection
    let href = ''
    if (from === to) {
      const r = getMarkRange(selection.$from, schema.marks.link)
      if (r) {
        from = r.from
        to = r.to
      }
    }
    doc.nodesBetween(from, to, (n) => {
      const m = n.marks.find((x) => x.type === schema.marks.link)
      if (m && !href) href = m.attrs.href as string
    })
    setMention(null)
    setLinking({ from, to, href, text: from === to ? '' : null })
  }

  const pick = (u: UserRef) => {
    const ed = editorRef.current
    const m = live.current.mention
    if (!ed || !m) return
    const src = `@${u.username}`
    // A space after it, unless one already follows.
    const doc = ed.state.doc
    const gap = /^\s/.test(doc.textBetween(m.to, Math.min(m.to + 1, doc.content.size), ' ', ' '))
      ? []
      : [{ type: 'text', text: ' ' }]
    const content = pieceHtml(src)
      ? [{ type: 'piece', attrs: { src } }, ...gap]
      : [{ type: 'text', text: gap.length ? `${src} ` : src }]
    ed.chain().focus().insertContentAt({ from: m.from, to: m.to }, content).run()
    setMention(null)
  }

  const detect = (ed: Editor) => {
    const { selection } = ed.state
    let next: Mention | null = null
    if (ed.isFocused && selection.empty && selection.$from.parent.isTextblock) {
      const { $from } = selection
      const before = $from.parent.textBetween(0, $from.parentOffset, '', '\ufffc')
      const m = MENTION_RE.exec(before)
      if (m) next = { from: selection.from - m[2].length - 1, to: selection.from, text: m[2] }
    }
    setMention((prev) =>
      prev && next && prev.from === next.from && prev.to === next.to && prev.text === next.text
        ? prev
        : next,
    )
  }

  const upload = (files: File[]) => {
    const ed = editorRef.current
    const to = props.current.target
    const defer = props.current.deferUploads
    if (!files.length || !ed) return
    if (!to) {
      if (!defer) return
      const content = files.flatMap((f, i) => {
        const src = defer(f)
        const name = f.name || 'pasted file'
        const node = f.type.startsWith('image/')
          ? { type: 'image', attrs: { src, alt: name } }
          : f.type.startsWith('video/')
            ? { type: 'video', attrs: { src, alt: name } }
            : { type: 'text', text: name, marks: [{ type: 'link', attrs: { href: src } }] }
        return i ? [{ type: 'hardBreak' }, node] : [node]
      })
      ed.chain().focus().insertContent(content).run()
      return
    }
    const held = files.map((f) => uploadPlaceholder(f.name || 'pasted image'))
    const placeholders = held.map((h) => h.text)
    ed.chain()
      .focus()
      .insertContent(
        placeholders.flatMap((p, i) =>
          i ? [{ type: 'hardBreak' }, { type: 'text', text: p }] : [{ type: 'text', text: p }],
        ),
      )
      .run()
    const finish = (i: number, markdown: string | null) => {
      const ph = placeholders[i]
      const cur = editorRef.current
      const at = mounted.current && cur && !cur.isDestroyed ? findText(cur, ph) : -1
      if (cur && at >= 0) {
        const tr = cur.state.tr
        if (markdown === null) tr.delete(at, at + ph.length)
        else tr.replaceRange(at, at + ph.length, markdownSlice(cur.schema, markdown))
        cur.view.dispatch(tr)
        return true
      }
      const v = props.current.getValue ? props.current.getValue() : last.current
      const found = [ph, asWritten(ph)].find((p) => v.includes(p))
      if (!found) return false
      const next = v.replace(found, () => markdown ?? '')
      last.current = next
      props.current.onValueChange(next)
      return true
    }
    files.forEach((f, i) => {
      uploadAttachment(to, f).then(
        (a) => {
          refreshAttachments(qc, to)
          if (!finish(i, a.markdown))
            toast(`${f.name || 'The file'} was attached but not inserted: its place was deleted.`)
          held[i].done()
        },
        (err) => {
          finish(i, null)
          held[i].done()
          toastError(err)
        },
      )
    })
  }
  const uploadRef = useRef(upload)
  uploadRef.current = upload

  const editor = useEditor({
    extensions: editorExtensions({ placeholder, openLink, blob: !!deferUploads }),
    content: toEditorHtml(value, { blob: !!deferUploads }),
    editable: !disabled,
    parseOptions: { preserveWhitespace: false },
    editorProps: {
      attributes: {
        role: 'textbox',
        'aria-multiline': 'true',
        'aria-label': ariaLabel ?? '',
        ...(placeholder ? { 'data-placeholder': placeholder } : {}),
        class: clsx(
          'rich-editor outline-none',
          field
            ? 'prose-wp px-3 py-2 text-sm leading-relaxed'
            : clsx(DESCRIPTION_BOX, 'transition-shadow focus:ring-2 focus:ring-accent/25'),
        ),
        ...(field ? { style: `min-height: calc(${minRows} * 1.625 * 0.875rem + 1rem)` } : {}),
      },
      handleKeyDown: (_view, e) => {
        pasteLiteral.current = (e.ctrlKey || e.metaKey) && e.shiftKey && e.key.toLowerCase() === 'v'
        if (e.isComposing || e.keyCode === 229) return false
        const l = live.current
        if (!l.menuOpen) return false
        if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
          const d = e.key === 'ArrowDown' ? 1 : -1
          setActive((a) => (a + d + l.matches.length) % l.matches.length)
          return true
        }
        if ((e.key === 'Enter' && !e.metaKey && !e.ctrlKey && !e.shiftKey) || e.key === 'Tab') {
          pick(l.matches[l.active])
          return true
        }
        return false
      },
      handlePaste: (view, event) => {
        const data = event.clipboardData
        if (!data) return false
        const files = [...data.files]
        if ((props.current.target || props.current.deferUploads) && files.length) {
          event.preventDefault()
          uploadRef.current(files)
          return true
        }
        // In a block of code, and on Shift+Ctrl/Cmd+V, what was copied goes in
        // as the text it is: ProseMirror's own plain paste.
        if (view.state.selection.$from.parent.type.spec.code || pasteLiteral.current) return false
        const html = data.getData('text/html')
        if (html.includes('data-pm-slice')) return false
        const text = data.getData('text/plain')
        if (text && looksLikeMarkdown(text) && !hasFormatting(html)) {
          event.preventDefault()
          // Markup in it would not survive the sanitiser: it goes in as the text it is.
          if (hasRawHtml(text)) {
            pasteLiteral.current = true
            try {
              return view.pasteText(text, event)
            } finally {
              pasteLiteral.current = false
            }
          }
          const slice = markdownSlice(view.state.schema, text, !!props.current.deferUploads)
          view.dispatch(
            view.state.tr
              .replaceSelection(slice)
              .scrollIntoView()
              .setMeta('paste', true)
              .setMeta('uiEvent', 'paste'),
          )
          return true
        }
        return false
      },
      handleDrop: (view, event) => {
        const files = [...(event.dataTransfer?.files ?? [])]
        if (!(props.current.target || props.current.deferUploads) || !files.length) return false
        event.preventDefault()
        setDragging(false)
        const at = view.posAtCoords({ left: event.clientX, top: event.clientY })
        if (at)
          view.dispatch(view.state.tr.setSelection(Selection.near(view.state.doc.resolve(at.pos))))
        uploadRef.current(files)
        return true
      },
      handleClickOn: (view, _pos, node, nodePos, event) => {
        if (node.type.name !== 'piece') return false
        // A wikilink piece is a link, and must not take the person away from
        // their text: the click puts the caret on its nearer side.
        event.preventDefault()
        const el = (event.target as HTMLElement).closest('[data-md]') ?? (event.target as Element)
        const r = el.getBoundingClientRect()
        const at = event.clientX < r.left + r.width / 2 ? nodePos : nodePos + node.nodeSize
        view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, at)))
        return true
      },
      handleDOMEvents: {
        click: (_view, e) => {
          if ((e.target as HTMLElement).closest?.('a')) e.preventDefault()
          return false
        },
        keyup: () => {
          pasteLiteral.current = false
          return false
        },
        // A click into an @name reopens its menu. prosemirror-view 1.44
        // settles a click's caret during mouse selection, where the
        // transaction's check can miss it, so look again once it is done.
        mouseup: () => {
          window.setTimeout(() => {
            const ed = editorRef.current
            if (ed && !ed.isDestroyed) detect(ed)
          })
          return false
        },
        blur: () => {
          window.setTimeout(() => {
            const ed = editorRef.current
            if (ed && !ed.isDestroyed && !ed.isFocused) setMention(null)
          }, 150)
          return false
        },
      },
    },
    onUpdate: ({ editor: ed }) => {
      const md = fromEditorHtml(ed.getHTML())
      if (md === last.current) return
      last.current = md
      props.current.onValueChange(md)
    },
    onTransaction: ({ editor: ed }) => detect(ed),
  })
  editorRef.current = editor

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  useEffect(() => {
    editor.setEditable(!disabled)
  }, [editor, disabled])

  // A value from elsewhere (a draft refreshed under the editor) replaces the
  // content, the caret staying near where it was.
  useEffect(() => {
    if (value === last.current) return
    last.current = value
    const { from } = editor.state.selection
    editor
      .chain()
      .command(({ tr }) => {
        tr.setMeta('addToHistory', false)
        return true
      })
      .setContent(toEditorHtml(value, { blob: !!props.current.deferUploads }), {
        emitUpdate: false,
        parseOptions: { preserveWhitespace: false },
      })
      .run()
    const { doc } = editor.state
    editor.view.dispatch(
      editor.state.tr.setSelection(
        TextSelection.near(doc.resolve(Math.min(from, doc.content.size))),
      ),
    )
    setMention(null)
  }, [value, editor])

  useLayoutEffect(() => {
    for (const [n, top] of scrolled.current ?? []) if (n.isConnected) n.scrollTop = top
    if (!autoFocus || disabled) return
    const { view } = editor
    // Focusing scrolls the caret into view, which is what opening by the
    // keyboard wants; a click opened the editor where the person already is.
    view.dom.focus({ preventScroll: !!caretAt })
    const at = caretAt && view.posAtCoords({ left: caretAt.x, top: caretAt.y })
    const tr = view.state.tr.setSelection(
      at ? Selection.near(view.state.doc.resolve(at.pos)) : Selection.atEnd(view.state.doc),
    )
    // Opened by the keyboard, the caret lands after the text, not in it: what
    // is typed there is plain, not an extension of a trailing code or link.
    view.dispatch(caretAt ? tr : tr.setStoredMarks([]).scrollIntoView())
    for (const [n, top] of scrolled.current ?? []) if (n.isConnected) n.scrollTop = top
    // eslint-disable-next-line react-hooks/exhaustive-deps -- starting state only
  }, [])

  // Where the menu opens: under the "@" it is for.
  useLayoutEffect(() => {
    if (!mention || !editor.isFocused) return
    try {
      const r = editor.view.coordsAtPos(mention.from)
      const left = Math.max(8, Math.min(r.left, window.innerWidth - 248))
      let top = r.bottom + 4
      if (top + 220 > window.innerHeight) top = Math.max(8, r.top - 220)
      setMenuPos({ top, left })
    } catch {
      /* the position no longer exists; the menu stays where it was */
    }
  }, [mention, editor])

  const state = useEditorState({
    editor,
    selector: ({ editor: ed }) => ({
      bold: ed.isActive('bold'),
      italic: ed.isActive('italic'),
      strike: ed.isActive('strike'),
      code: ed.isActive('code'),
      link: ed.isActive('link'),
      h1: ed.isActive('heading', { level: 1 }),
      h2: ed.isActive('heading', { level: 2 }),
      h3: ed.isActive('heading', { level: 3 }),
      bullet: ed.isActive('bulletList'),
      ordered: ed.isActive('orderedList'),
      task: ed.isActive('taskList'),
      quote: ed.isActive('blockquote'),
      codeBlock: ed.isActive('codeBlock'),
    }),
  })

  const run = () => editor.chain().focus()
  const tool = (label: string, icon: React.ReactNode, onClick: () => void, pressed?: boolean) => (
    <IconButton
      size="xs"
      label={label}
      aria-pressed={pressed}
      className={clsx(pressed && 'bg-accent/15 text-accent')}
      onMouseDown={(e) => e.preventDefault()}
      onClick={onClick}
    >
      {icon}
    </IconButton>
  )
  const sep = <span className="mx-1 h-4 w-px shrink-0 bg-line" />

  const toolbar = (
    <>
      <div
        role="toolbar"
        aria-label="Formatting"
        ref={bar}
        onScroll={measureBar}
        data-fade={
          fade.left && fade.right ? 'both' : fade.left ? 'left' : fade.right ? 'right' : undefined
        }
        style={fadeStyle(fade)}
        // contain: the buttons' width must not widen the box it sits in (a
        // comment box in a flex row grew past a 390px screen); the row scrolls.
        className="no-scrollbar flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto px-1 [contain:inline-size]"
      >
        {tool('Bold', <Bold className="size-3.5" />, () => run().toggleBold().run(), state.bold)}
        {tool(
          'Italic',
          <Italic className="size-3.5" />,
          () => run().toggleItalic().run(),
          state.italic,
        )}
        {tool(
          'Strikethrough',
          <Strikethrough className="size-3.5" />,
          () => run().toggleStrike().run(),
          state.strike,
        )}
        {tool(
          'Inline code',
          <Code className="size-3.5" />,
          () => run().toggleCode().run(),
          state.code,
        )}
        {tool('Link', <LinkIcon className="size-3.5" />, openLink, state.link)}
        {sep}
        {tool(
          'Heading 1',
          <Heading1 className="size-3.5" />,
          () => run().toggleHeading({ level: 1 }).run(),
          state.h1,
        )}
        {tool(
          'Heading 2',
          <Heading2 className="size-3.5" />,
          () => run().toggleHeading({ level: 2 }).run(),
          state.h2,
        )}
        {tool(
          'Heading 3',
          <Heading3 className="size-3.5" />,
          () => run().toggleHeading({ level: 3 }).run(),
          state.h3,
        )}
        {sep}
        {tool(
          'Bulleted list',
          <List className="size-3.5" />,
          () => run().toggleBulletList().run(),
          state.bullet,
        )}
        {tool(
          'Numbered list',
          <ListOrdered className="size-3.5" />,
          () => run().toggleOrderedList().run(),
          state.ordered,
        )}
        {tool(
          'Task list',
          <ListTodo className="size-3.5" />,
          () => run().toggleTaskList().run(),
          state.task,
        )}
        {tool(
          'Quote',
          <Quote className="size-3.5" />,
          () => run().toggleBlockquote().run(),
          state.quote,
        )}
        {tool(
          'Code block',
          <SquareCode className="size-3.5" />,
          () => run().toggleCodeBlock().run(),
          state.codeBlock,
        )}
        {sep}
        {tool('Undo', <Undo2 className="size-3.5" />, () => run().undo().run())}
        {tool('Redo', <Redo2 className="size-3.5" />, () => run().redo().run())}
        {onSource && (
          <>
            {sep}
            {tool('Edit as markdown', <FileCode className="size-3.5" />, onSource)}
          </>
        )}
      </div>
      {trailing && (
        <div className="flex shrink-0 items-center gap-2 border-l border-line px-2">{trailing}</div>
      )}
    </>
  )

  const anchor = useRef<HTMLSpanElement | null>(null)
  const [anchorAt, setAnchorAt] = useState({ top: 0, left: 0 })
  useLayoutEffect(() => {
    if (!linking || !box.current) return
    const r = editor.view.coordsAtPos(linking.from)
    const b = box.current.getBoundingClientRect()
    setAnchorAt({ top: r.bottom - b.top, left: r.left - b.left })
  }, [linking, editor])

  const closeLink = () => {
    setLinking(null)
    editor.commands.focus()
  }

  return (
    <div
      ref={box}
      className={clsx('relative', field && [fieldBoxLook, disabled && 'opacity-60'], className)}
      data-dragging={dragging || undefined}
      onDragOver={(e) => {
        if (!(target || deferUploads) || !e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={() => setDragging(false)}
      onKeyDown={(e) => {
        // Keys the editor left alone are the caller's (Mod+Enter saves,
        // Escape closes), but not those typed in the link popover, and never
        // one that is part of composing text.
        if (!editor.view.dom.contains(e.target as Node)) return
        if (e.nativeEvent.isComposing || e.keyCode === 229) return
        // ProseMirror marks Enter and Escape handled whatever they do, so these
        // two are the caller's even then: no binding of the editor's uses them.
        const caller = e.key === 'Escape' || (e.key === 'Enter' && (e.metaKey || e.ctrlKey))
        if (e.defaultPrevented && !caller) return
        props.current.onKeyDown?.(e)
      }}
    >
      {/* The toolbar takes no room: a zero-height wrapper that sticks under
          the page's header (3rem) with the toolbar (2.25rem and a gap) above
          its line, so it floats over what is above the text and follows it
          down a long description. */}
      {!disabled &&
        (field ? (
          <div className="flex h-9 items-center border-b border-line">{toolbar}</div>
        ) : (
          <div className="sticky top-[5.5rem] z-20 h-0">
            <div className="absolute inset-x-0 bottom-full mb-1 flex h-9 items-center rounded-lg border border-line bg-panel shadow-pop">
              {toolbar}
            </div>
          </div>
        ))}
      <div
        className={clsx(
          '[&_.ProseMirror]:rounded-lg',
          dragging && '[&_.ProseMirror]:ring-2 [&_.ProseMirror]:ring-accent/40',
        )}
      >
        <EditorContent editor={editor} />
      </div>
      {menuOpen && (
        <MentionList
          users={matches}
          active={active}
          pos={menuPos}
          onHover={setActive}
          onPick={pick}
        />
      )}
      {linking && (
        <>
          <span
            ref={anchor}
            aria-hidden
            className="pointer-events-none absolute size-0"
            style={anchorAt}
          />
          <LinkForm
            draft={linking}
            anchor={anchor}
            onClose={closeLink}
            onApply={(href, text) => {
              const { from, to } = linking
              const c = editor.chain().focus()
              const link = editor.schema.marks.link
              if (from === to) {
                if (href)
                  c.insertContentAt(from, {
                    type: 'text',
                    text: text || href,
                    marks: [{ type: 'link', attrs: { href } }],
                  }).run()
                else c.run()
              } else if (href)
                c.setTextSelection({ from, to }).setLink({ href }).setTextSelection(to).run()
              else
                c.setTextSelection({ from, to })
                  .command(({ tr }) => {
                    tr.removeMark(from, to, link)
                    return true
                  })
                  .setTextSelection(to)
                  .run()
              setLinking(null)
            }}
          />
        </>
      )}
    </div>
  )
}

function LinkForm({
  draft,
  anchor,
  onClose,
  onApply,
}: {
  draft: LinkDraft
  anchor: React.RefObject<HTMLSpanElement | null>
  onClose: () => void
  onApply: (href: string, text: string) => void
}) {
  const [href, setHref] = useState(draft.href)
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const submit = () => {
    const h = href.trim()
    if (h && !SAFE_HREF.test(h)) {
      setError('A link starts with https://, http://, mailto:, / or #.')
      return
    }
    onApply(h, text.trim())
  }
  return (
    <Popover open onClose={onClose} anchor={anchor} width={288} className="p-2">
      <form
        className="flex flex-col gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
        onKeyDown={(e) => {
          // Enter applies, and only here: not the caller's save.
          if (e.key === 'Enter') e.stopPropagation()
        }}
      >
        {draft.text !== null && (
          <Input
            aria-label="Link text"
            placeholder="Text"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        )}
        <Input
          autoFocus
          aria-label="Link URL"
          placeholder="https://"
          value={href}
          aria-invalid={!!error || undefined}
          onChange={(e) => {
            setHref(e.target.value)
            setError('')
          }}
        />
        {error && (
          <p role="alert" className="text-xs text-danger">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" size="sm" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" size="sm">
            {draft.href || draft.text === null ? 'Apply' : 'Add link'}
          </Button>
        </div>
      </form>
    </Popover>
  )
}

// A gradient over the toolbar row's edge that has more buttons beyond it.
function fadeStyle({ left, right }: { left: boolean; right: boolean }): React.CSSProperties {
  if (!left && !right) return {}
  const edge = '1.5rem'
  const mask = `linear-gradient(to right, ${left ? 'transparent' : '#000'} 0, #000 ${left ? edge : '0px'}, #000 calc(100% - ${right ? edge : '0px'}), ${right ? 'transparent' : '#000'} 100%)`
  return { maskImage: mask, WebkitMaskImage: mask }
}
