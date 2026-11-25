import { type Editor, Extension, markInputRule, Node, wrappingInputRule } from '@tiptap/core'
import { InputRule } from '@tiptap/core'
import { Fragment, type Node as PMNode } from '@tiptap/pm/model'
import { closeHistory } from '@tiptap/pm/history'
import { Plugin, TextSelection } from '@tiptap/pm/state'
import type { EditorView } from '@tiptap/pm/view'
import { Bold } from '@tiptap/extension-bold'
import { Code } from '@tiptap/extension-code'
import { CodeBlock } from '@tiptap/extension-code-block'
import { OrderedList } from '@tiptap/extension-ordered-list'
import { HardBreak } from '@tiptap/extension-hard-break'
import { Italic } from '@tiptap/extension-italic'
import { Link } from '@tiptap/extension-link'
import { Strike } from '@tiptap/extension-strike'
import Image from '@tiptap/extension-image'
import { TableKit } from '@tiptap/extension-table'
import TaskItem from '@tiptap/extension-task-item'
import TaskList from '@tiptap/extension-task-list'
import { Placeholder } from '@tiptap/extensions'
import StarterKit from '@tiptap/starter-kit'
import { ATTACHED_VIDEO, isPieceSrc, pieceHtml, PIECE_SRC, pieceText } from '@/lib/markdown'
import { SAFE_HREF, SAFE_SRC } from '@/lib/richtext'

// The editor's schema is what Waypoint's markdown renders: it is also what
// survivesRichEdit round-trips text through, so what the schema can't hold
// opens in markdown instead of being lost.

/** A file the person added to a form that has no attachment target yet (deferUploads). */
export const BLOB_URL = /^blob:\S+$/i

/** An uploaded video, which markdown writes as an image of an attachment. */
const video = (blob: boolean) =>
  Node.create({
    name: 'video',
    group: 'inline',
    inline: true,
    atom: true,
    draggable: true,
    addAttributes() {
      return { src: { default: null }, alt: { default: '' } }
    },
    parseHTML() {
      return [
        {
          tag: 'video[src]',
          getAttrs: (el) => {
            const src = (el as HTMLElement).getAttribute('src') ?? ''
            return ATTACHED_VIDEO.test(src) || (blob && BLOB_URL.test(src))
              ? { src, alt: (el as HTMLElement).getAttribute('aria-label') ?? '' }
              : false
          },
        },
      ]
    },
    renderHTML({ node }) {
      return [
        'video',
        {
          src: node.attrs.src,
          controls: '',
          preload: 'metadata',
          playsinline: '',
          'aria-label': node.attrs.alt,
        },
      ]
    },
  })

/**
 * A wikilink or @mention: drawn as the view draws it, not editable, and kept
 * as its markdown (`src`). The element is the one markdown.ts makes, so it is
 * the same width in the editor and out of it.
 */
export const Piece = Node.create({
  name: 'piece',
  group: 'inline',
  inline: true,
  atom: true,
  // Arrow keys step over it in one press and Backspace takes it whole.
  selectable: false,
  addAttributes() {
    return { src: { default: '' } }
  },
  renderText: ({ node }) => node.attrs.src as string,
  parseHTML() {
    return [
      {
        tag: `[${PIECE_SRC}]`,
        priority: 200,
        // Only one that shows what its markdown shows: markup pasted in with a
        // data-md of its own must not save a link or name other than the words
        // on screen.
        getAttrs: (el) => {
          const src = (el as HTMLElement).getAttribute(PIECE_SRC)
          return src !== null && isPieceSrc(src) && el.textContent === pieceText(src)
            ? { src }
            : false
        },
      },
    ]
  },
  renderHTML({ node }) {
    const holder = document.createElement('template')
    holder.innerHTML = pieceHtml(node.attrs.src as string) ?? ''
    return holder.content.firstElementChild ?? ['span', node.attrs.src as string]
  },
})

const imageNode = (blob: boolean) =>
  Image.extend({
    // Typing ![alt](url) is Typing's rule, which checks the address.
    addInputRules() {
      return []
    },
    parseHTML() {
      return [
        {
          tag: 'img[src]',
          getAttrs: (el) =>
            SAFE_SRC.test((el as HTMLElement).getAttribute('src') ?? '') ||
            (blob && BLOB_URL.test((el as HTMLElement).getAttribute('src') ?? ''))
              ? null
              : false,
        },
      ]
    },
  })

// Mod+Enter belongs to the caller (it saves); the editor's own use of it would
// insert a line break or leave a block of code.
const hardBreak = HardBreak.extend({
  addKeyboardShortcuts() {
    return { 'Shift-Enter': () => this.editor.commands.setHardBreak() }
  },
})
const codeBlock = CodeBlock.extend({
  // The rules for ``` are Typing's: they also work after a line break.
  addInputRules() {
    return []
  },
  addKeyboardShortcuts() {
    const { 'Mod-Enter': _exit, ...rest } = this.parent?.() ?? {}
    return rest
  },
})

// A mark without Tiptap's own input and paste rules, which let a space follow
// the opener ("2 * 3 * 4" becomes 2 3 4 in italics): Typing has the rules.
const spaceless = <T extends typeof Bold | typeof Italic | typeof Strike>(mark: T) =>
  mark.extend({
    addInputRules() {
      return []
    },
    addPasteRules() {
      return []
    },
  })

/** The schema, with nothing that only acts on keys and typing. */
export function schemaExtensions({ blob }: { blob: boolean }) {
  return [
    StarterKit.configure({
      underline: false,
      bold: false,
      italic: false,
      strike: false,
      code: false,
      orderedList: false,
      trailingNode: false,
      hardBreak: false,
      codeBlock: false,
      link: false,
    }),
    // Text typed right after a link is not part of it (Tiptap makes an
    // autolinking mark inclusive).
    Link.extend({ inclusive: () => false }).configure({
      openOnClick: false,
      autolink: true,
      linkOnPaste: true,
      isAllowedUri: (url) => SAFE_HREF.test(url) || (blob && BLOB_URL.test(url)),
    }),
    ...[Bold, Italic, Strike].map(spaceless),
    // Pasted `text` stays the characters it is; typing the backticks is a rule.
    // Nor does pasted "1. n" turn into a list behind Shift+Ctrl+V: pasting
    // markdown is the editor's own paste rule.
    OrderedList.extend({
      addProseMirrorPlugins() {
        return []
      },
      // "3. " and "3) " both start a numbered list, at that number.
      addInputRules() {
        return [
          wrappingInputRule({
            find: /^(\d+)[.)]\s$/,
            type: this.type,
            getAttributes: (m) => ({ start: +m[1] }),
            joinPredicate: (m, node) => node.childCount + node.attrs.start === +m[1],
          }),
        ]
      },
    }),
    Code.extend({
      addPasteRules() {
        return []
      },
    }),
    hardBreak,
    codeBlock.configure({
      languageClassPrefix: 'language-',
      enableTabIndentation: true,
      tabSize: 2,
    }),
    TaskList,
    TaskItem.configure({ nested: true }),
    imageNode(blob).configure({ inline: true, allowBase64: false }),
    TableKit.configure({ table: { resizable: false } }),
    video(blob),
    Piece,
  ]
}

// A mark's rule where the start of the text or a line break or a piece
// stands before it. Tiptap's own rules need white space there, which a line
// break is not, so "**bold**" after Shift+Enter would stay as typed.
const EDGE = String.raw`(?<![^\s\ufffc])`
const rule = (src: string) => new RegExp(EDGE + src + '$')

// An address may hold parentheses in pairs, two deep ("/wiki/Foo_(bar)").
const ADDRESS = String.raw`((?:[^()\s]|\((?:[^()\s]|\([^()\s]*\))*\))+)`
const LINK = String.raw`\[([^[\]]+)\]\(` + ADDRESS + String.raw`\)`

/** Typing markdown applies it: the rules StarterKit does not have, and the keys. */
export const Typing = Extension.create<{ openLink: () => void }>({
  name: 'markdownTyping',
  priority: 1000,
  addOptions() {
    return { openLink: () => {} }
  },
  addInputRules() {
    const { schema } = this.editor
    const mark = (name: string) => schema.marks[name]
    return [
      markInputRule({
        find: rule(String.raw`(\*\*(?!\s)((?:[^*]*[^*\s]))\*\*)`),
        type: mark('bold'),
      }),
      markInputRule({ find: rule(String.raw`(__(?!\s)((?:[^_]*[^_\s]))__)`), type: mark('bold') }),
      markInputRule({
        find: rule(String.raw`(\*(?!\s)((?:[^*]*[^*\s]))\*)`),
        type: mark('italic'),
      }),
      markInputRule({ find: rule(String.raw`(_(?!\s)((?:[^_]*[^_\s]))_)`), type: mark('italic') }),
      markInputRule({
        find: rule(String.raw`(~~(?!\s)((?:[^~]*[^~\s]))~~)`),
        type: mark('strike'),
      }),
      // "[ ] " in a bullet item makes that item a task: the list is split
      // around it, since a list is all bullets or all tasks here.
      new InputRule({
        find: /^\[( |x|X)?\]\s$/,
        handler: ({ state, range, match }) => {
          const { tr } = state
          const $from = state.doc.resolve(range.from)
          if ($from.parent.type.name !== 'paragraph' || $from.depth < 2) return null
          if ($from.node(-1).type.name !== 'listItem' || $from.index(-1) !== 0) return null
          if ($from.node(-2).type.name !== 'bulletList') return null
          tr.delete(range.from, range.to)
          const $p = tr.doc.resolve(range.from)
          const list = $p.node(-2)
          const at = $p.index(-2)
          const items: PMNode[] = []
          list.forEach((n) => items.push(n))
          const task = schema.nodes.taskList.create(
            null,
            schema.nodes.taskItem.create({ checked: /x/i.test(match[1] ?? '') }, items[at].content),
          )
          const out: PMNode[] = []
          if (at > 0) out.push(list.type.create(list.attrs, items.slice(0, at)))
          out.push(task)
          if (at < items.length - 1) out.push(list.type.create(list.attrs, items.slice(at + 1)))
          const listPos = $p.before(-2)
          tr.replaceWith(listPos, listPos + list.nodeSize, out)
          const caret = listPos + (at > 0 ? out[0].nodeSize : 0) + 3
          tr.setSelection(TextSelection.create(tr.doc, caret))
        },
      }),
      new InputRule({
        find: new RegExp(String.raw`(?<![\\!])` + LINK + '$'),
        handler: ({ state, range, match }) => {
          const [, label, href] = match
          if (!SAFE_HREF.test(href) || !label.trim()) return null
          const link = schema.marks.link
          state.tr
            .replaceWith(range.from, range.to, schema.text(label, [link.create({ href })]))
            .removeStoredMark(link)
        },
      }),
      new InputRule({
        find: new RegExp(String.raw`(?<!\\)!\[([^[\]]*)\]\(` + ADDRESS + String.raw`\)$`),
        handler: ({ state, range, match }) => {
          const [, alt, src] = match
          if (!SAFE_SRC.test(src)) return null
          state.tr.replaceWith(range.from, range.to, schema.nodes.image.create({ src, alt }))
        },
      }),
      new InputRule({
        find: /\[\[([^[\]|\n]{1,200})(?:\|([^[\]\n]{0,200}))?\]\]$/,
        handler: ({ state, range, match }) => {
          const src = match[0]
          if (!pieceHtml(src)) return null
          state.tr.replaceWith(range.from, range.to, schema.nodes.piece.create({ src }))
        },
      }),
    ]
  },
  addKeyboardShortcuts() {
    const { editor } = this
    return {
      'Mod-k': () => {
        this.options.openLink()
        return true
      },
      // There is no markdown for underline; the browser's own would add one.
      'Mod-u': () => true,
      // In a list Tab nests and Shift+Tab un-nests, and where nothing can move
      // the key is swallowed: it must not take focus out of the editor.
      Tab: () => listTab(editor, true),
      'Shift-Tab': () => listTab(editor, false),
      'Mod-z': () =>
        editor.commands.first(({ commands }) => [
          () => commands.undoInputRule(),
          () => commands.undo(),
        ]),
      // Backspace first takes back a rule just applied; at the start of a
      // block it then turns the block into a paragraph.
      Backspace: () => {
        if (editor.commands.undoInputRule()) return true
        const { selection } = editor.state
        if (!selection.empty) return false
        const { $from } = selection
        if ($from.parentOffset !== 0) return false
        const name = $from.parent.type.name
        if (name === 'heading' || name === 'codeBlock') return editor.commands.setNode('paragraph')
        if (name !== 'paragraph' || $from.depth < 2) return false
        const outer = $from.node(-1).type.name
        if ($from.index(-1) !== 0) return false
        if (outer === 'listItem' || outer === 'taskItem') return editor.commands.liftListItem(outer)
        if (outer === 'blockquote') return editor.commands.lift('blockquote')
        return false
      },
    }
  },
  addProseMirrorPlugins() {
    return [
      new Plugin({
        props: {
          handleDOMEvents: {
            // ProseMirror reads the composed text into the document a moment
            // after compositionend; Tiptap's own pass at the end of the
            // composition can come before that, so the rules run once more.
            compositionend: (view, e) => {
              window.setTimeout(() => {
                if (view.isDestroyed) return
                const rules = view.state.plugins.find(
                  (p) => (p.spec as { isInputRules?: boolean }).isInputRules,
                )
                rules?.props.handleDOMEvents?.compositionend?.call(rules, view, e)
                composedFence(view)
              }, 30)
              return false
            },
            // A soft keyboard sends Enter and Backspace as keydown 229 and then
            // the editing command's own beforeinput; ProseMirror ignores the
            // keydown, so the key's handlers (list, heading, fence) run here.
            beforeinput: (view, e) => {
              const ie = e as InputEvent
              if (view.composing || ie.isComposing) return false
              if (ie.inputType === 'insertText' && ie.data && [...ie.data].length > 1) {
                // Put in as one piece, not typed: no rule reads it. (Tiptap's
                // rules plugin sees handleTextInput before this plugin does.)
                const { from, to } = view.state.selection
                view.dispatch(view.state.tr.insertText(ie.data, from, to).scrollIntoView())
                e.preventDefault()
                return true
              }
              const key =
                ie.inputType === 'insertParagraph'
                  ? { key: 'Enter', code: 'Enter', keyCode: 13 }
                  : ie.inputType === 'deleteContentBackward'
                    ? { key: 'Backspace', code: 'Backspace', keyCode: 8 }
                    : null
              if (!key) return false
              const down = new KeyboardEvent('keydown', { ...key, bubbles: true, cancelable: true })
              if (!view.someProp('handleKeyDown', (f) => f(view, down))) return false
              e.preventDefault()
              return true
            },
          },
          handleTextInput: (view, from, to, text) => {
            // Text put in as one piece (an insertText from a keyboard's
            // suggestion, a dictation) is not keys typed: no rule reads it.
            if (!view.composing && [...text].length > 1) {
              view.dispatch(view.state.tr.insertText(text, from, to))
              return true
            }
            if (from === to && inCellBlockRule(view, text)) {
              view.dispatch(view.state.tr.insertText(text, from, to))
              return true
            }
            return text === ' ' && from === to && !view.composing ? openFence(view, true) : false
          },
          handleKeyDown: (view, e) => {
            if (e.key !== 'Enter' || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return false
            if (e.isComposing || e.keyCode === 229) return false
            return closeFence(view) || openFence(view, false)
          },
        },
      }),
    ]
  },
})

const CELL_BLOCK = /^(?:#{1,6}|[-+*]|\d+[.)]|>|\[[ xX]?\]|```[\w+#.-]*)$/
const CELL_RULE = /^(?:--|\*\*|__)$/

/**
 * A table cell holds one line of text in markdown, so the typing that starts a
 * block (a list, a heading, a quote, a fence, a rule) is plain text there.
 */
function inCellBlockRule(view: EditorView, text: string): boolean {
  const { $from } = view.state.selection
  let cell = false
  for (let d = $from.depth; d > 0; d--)
    if (['tableCell', 'tableHeader'].includes($from.node(d).type.name)) cell = true
  if (!cell) return false
  const before = $from.parent.textBetween(0, $from.parentOffset, '', '\ufffc')
  return (
    (text === ' ' && CELL_BLOCK.test(before)) ||
    (text.length === 1 && CELL_RULE.test(before) && text === before[0])
  )
}

const LIST_ITEMS = ['listItem', 'taskItem']

/** Tab or Shift+Tab in a list item; false elsewhere. Each is its own undo step. */
function listTab(editor: Editor, nest: boolean): boolean {
  const { $from } = editor.state.selection
  let depth = $from.depth
  while (depth > 0 && !LIST_ITEMS.includes($from.node(depth).type.name)) depth--
  if (depth === 0) return false
  const item = $from.node(depth).type.name
  const history = () => editor.commands.command(({ tr }) => (closeHistory(tr), true))
  history()
  if (nest) {
    if (!editor.commands.sinkListItem(item)) nestUnderPrevious(editor, depth)
  } else if (depth > 2) editor.commands.liftListItem(item)
  history()
  return true
}

/**
 * A first item whose list follows another list (a task typed under a bullet
 * item starts its own list) goes inside that list's last item.
 */
function nestUnderPrevious(editor: Editor, itemDepth: number): boolean {
  const { state } = editor
  const { $from } = state.selection
  const listDepth = itemDepth - 1
  if (listDepth < 1 || $from.index(listDepth) !== 0) return false
  const list = $from.node(listDepth)
  const prev = $from.node(listDepth - 1).maybeChild($from.index(listDepth - 1) - 1)
  const last = prev?.lastChild
  if (!prev || !last || !LIST_ITEMS.includes(last.type.name) || !list.firstChild) return false
  const item = list.firstChild
  const listPos = $from.before(listDepth)
  const tr = state.tr
  if (list.childCount === 1) tr.delete(listPos, listPos + list.nodeSize)
  else tr.delete(listPos + 1, listPos + 1 + item.nodeSize)
  const at = listPos - 2
  tr.insert(at, list.type.create(list.attrs, item))
  tr.setSelection(TextSelection.near(tr.doc.resolve(at + 3 + $from.parentOffset)))
  editor.view.dispatch(tr.scrollIntoView())
  return true
}

const FENCE_LINE = /^```([\w+#.-]*)$/

/**
 * A line that is exactly ``` or ```lang, at the start of a paragraph or after
 * a line break in one, becomes a block of code when Space or Enter is typed
 * after it: the text before the break stays a paragraph, the text after the
 * caret goes into the block.
 */
function openFence(view: EditorView, space: boolean): boolean {
  const { state } = view
  const { selection, schema } = state
  if (!selection.empty) return false
  const { $from } = selection
  const para = $from.parent
  if (para.type.name !== 'paragraph') return false
  let lineStart = 0
  para.forEach((child, offset) => {
    if (child.type.name === 'hardBreak' && offset + child.nodeSize <= $from.parentOffset)
      lineStart = offset + child.nodeSize
  })
  const line = para.textBetween(lineStart, $from.parentOffset, '', '\ufffc')
  const m = FENCE_LINE.exec(line)
  if (!m) return false
  let before = para.content.cut(0, Math.max(0, lineStart - 1))
  while (before.lastChild?.type.name === 'hardBreak')
    before = before.cut(0, before.size - before.lastChild.nodeSize)
  const rest = para.content.cut($from.parentOffset)
  const text = rest.textBetween(0, rest.size, undefined, '\n')
  const block = schema.nodes.codeBlock.create(
    { language: m[1] || null },
    text ? schema.text(text) : null,
  )
  const nodes: PMNode[] = []
  if (before.size) nodes.push(para.type.create(para.attrs, before))
  nodes.push(block)
  const parent = $from.node(-1)
  if (!parent.canReplace($from.index(-1), $from.indexAfter(-1), Fragment.from(nodes))) return false
  const tr = state.tr.replaceWith($from.before(), $from.after(), nodes)
  const start = $from.before() + (before.size ? nodes[0].nodeSize : 0) + 1
  tr.setSelection(TextSelection.create(tr.doc, start)).scrollIntoView()
  // Backspace and Mod+Z give back the line as typed (and the space, if one was).
  const rules = state.plugins.find((p) => (p.spec as { isInputRules?: boolean }).isInputRules)
  if (rules)
    tr.setMeta(rules, { transform: tr, from: $from.pos, to: $from.pos, text: space ? ' ' : '' })
  view.dispatch(tr)
  return true
}

/**
 * A composed space has already gone into the text when the composition ends,
 * so a line of "``` " is a fence whose space is taken out first.
 */
function composedFence(view: EditorView): void {
  const { selection } = view.state
  const { $from } = selection
  if (!selection.empty || $from.parent.type.name !== 'paragraph') return
  const para = $from.parent
  let lineStart = 0
  para.forEach((child, offset) => {
    if (child.type.name === 'hardBreak' && offset + child.nodeSize <= $from.parentOffset)
      lineStart = offset + child.nodeSize
  })
  const line = para.textBetween(lineStart, $from.parentOffset, '', '\ufffc')
  if (!/^```[\w+#.-]* $/.test(line)) return
  view.dispatch(view.state.tr.delete($from.pos - 1, $from.pos))
  openFence(view, true)
}

/** Enter on a last line that is exactly ``` takes it out and leaves the block. */
function closeFence(view: EditorView): boolean {
  const { state } = view
  const { selection, schema } = state
  const { $from } = selection
  const block = $from.parent
  if (!selection.empty || block.type.name !== 'codeBlock') return false
  if ($from.parentOffset !== block.content.size) return false
  const text = block.textContent
  if (!(text === '```' || text.endsWith('\n```'))) return false
  const tr = state.tr
  const start = $from.start()
  const keep = text.length - 3 > 0 ? text.length - 4 : 0
  tr.delete(start + keep, start + text.length)
  if (!keep) {
    tr.setBlockType($from.before(), tr.mapping.map($from.after()), schema.nodes.paragraph)
    tr.setSelection(TextSelection.create(tr.doc, start))
  } else {
    const after = tr.mapping.map($from.after())
    tr.insert(after, schema.nodes.paragraph.create())
    tr.setSelection(TextSelection.create(tr.doc, after + 1))
  }
  view.dispatch(tr.scrollIntoView())
  return true
}

/** Everything the editor has: the schema, typing and the placeholder. */
export function editorExtensions(opts: {
  placeholder?: string
  openLink: () => void
  blob: boolean
}) {
  return [
    ...schemaExtensions({ blob: opts.blob }),
    Typing.configure({ openLink: opts.openLink }),
    Placeholder.configure({
      placeholder: opts.placeholder ?? '',
      showOnlyCurrent: false,
    }),
  ]
}
