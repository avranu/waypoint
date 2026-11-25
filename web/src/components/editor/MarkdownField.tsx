import { useEffect, useRef, useState } from 'react'
import clsx from 'clsx'
import { fieldBoxLook } from '@/components/ui/input'
import { MarkdownInput } from '../issue/MarkdownInput'
import { loadedEditor, preloadEditor, tryLoadEditor } from './lazy'
import { readMode, writeMode } from './mode'

type Props = {
  value: string
  onValueChange: (v: string) => void
  /** Files pasted or dropped upload to this issue. */
  issueKey?: string
  /** Same for a page, design note or anything else: page:12, design:3. */
  attachTo?: string
  /** For a form with no target yet: see RichTextInput's deferUploads. */
  deferUploads?: (file: File) => string
  placeholder?: string
  ariaLabel?: string
  'aria-label'?: string
  autoFocus?: boolean
  /** The box's least height, in lines of text. */
  rows?: number
  onKeyDown?: (e: React.KeyboardEvent) => void
  className?: string
  disabled?: boolean
  id?: string
  /**
   * Whether the formatted editor keeps this text as it is: false opens the
   * markdown, as text it can't hold does (a memory file's § line, say).
   */
  keeps?: (md: string) => boolean
}

// The toolbar's row (2.25rem, border included) and the text's lines and padding,
// as RichTextInput's 'field' look sizes them; the markdown box and the box
// shown while the editor loads take the same height so switching moves nothing.
const CANT_KEEP = "Opened as markdown: this text has formatting the editor can't keep."

const boxHeight = (rows: number) => `calc(2.25rem + ${rows} * 1.625 * 0.875rem + 1rem + 2px)`

/**
 * The form-field counterpart of MarkdownEditor: formatted text in a box with a
 * toolbar, or the markdown source, whichever the person last chose (the
 * setting MarkdownEditor shares). Text the formatted editor can't keep opens
 * as markdown.
 */
export function MarkdownField({
  value,
  onValueChange,
  issueKey,
  attachTo,
  deferUploads,
  placeholder,
  ariaLabel,
  'aria-label': ariaLabelAttr,
  autoFocus,
  rows = 3,
  onKeyDown,
  className,
  disabled,
  id,
  keeps,
}: Props) {
  const label = ariaLabel ?? ariaLabelAttr
  const [mod, setMod] = useState(loadedEditor)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    if (readMode() === 'md' || mod) return
    void tryLoadEditor().then((m) => (m ? setMod(m) : setFailed(true)))
  }, [mod])
  const blob = !!deferUploads
  const holds = (m: NonNullable<typeof mod>, md: string) =>
    m.survivesRichEdit(md, { blob }) && (keeps?.(md) ?? true)
  useEffect(() => {
    if (readMode() === 'rich') preloadEditor()
  }, [])

  // Decided once, when the text is first seen: a text the editor can't keep
  // opens as markdown with a note; after that the person's switches rule.
  const first = useRef(value)
  const [picked, setPicked] = useState<{ mode: 'rich' | 'md'; note: string | null } | null>(null)
  const opening =
    readMode() === 'md'
      ? { mode: 'md' as const, note: null }
      : mod
        ? holds(mod, first.current)
          ? { mode: 'rich' as const, note: null }
          : { mode: 'md' as const, note: CANT_KEEP }
        : failed
          ? { mode: 'md' as const, note: null }
          : null
  const settled = useRef(opening)
  if (!settled.current && opening) settled.current = opening
  const shown = picked ?? settled.current
  const latest = useRef(value)
  latest.current = value

  const toRich = async () => {
    const m = mod ?? (await tryLoadEditor())
    if (!m) return
    setMod(m)
    if (holds(m, latest.current)) {
      writeMode('rich')
      setPicked({ mode: 'rich', note: null })
    } else setPicked({ mode: 'md', note: CANT_KEEP })
  }
  const toMarkdown = () => {
    writeMode('md')
    setPicked({ mode: 'md', note: null })
  }

  const minHeight = boxHeight(rows)
  if (!shown || (shown.mode === 'rich' && !mod)) {
    return (
      <div
        id={id}
        aria-hidden
        className={clsx(fieldBoxLook, 'opacity-60', className)}
        style={{ minHeight }}
      />
    )
  }
  if (shown.mode === 'rich' && mod) {
    return (
      // Inside a <label> (a Field), a click on the text would otherwise be
      // handed to the label's first control, the toolbar's first button.
      <div
        onClick={(e) => {
          if (!(e.target as Element).closest('button, input, a, textarea, select'))
            e.preventDefault()
        }}
      >
        <mod.RichTextInput
          look="field"
          value={value}
          onValueChange={onValueChange}
          issueKey={issueKey}
          attachTo={attachTo}
          deferUploads={deferUploads}
          placeholder={placeholder}
          ariaLabel={label}
          autoFocus={autoFocus}
          minRows={rows}
          onKeyDown={onKeyDown}
          onSource={toMarkdown}
          getValue={() => latest.current}
          disabled={disabled}
          className={className}
        />
      </div>
    )
  }
  return (
    <div className={className}>
      <MarkdownInput
        id={id}
        autoFocus={autoFocus}
        value={value}
        onValueChange={onValueChange}
        issueKey={issueKey}
        attachTo={attachTo}
        placeholder={placeholder}
        aria-label={label}
        onKeyDown={onKeyDown}
        disabled={disabled}
        rows={rows}
        autoGrow
        style={{ minHeight }}
        className="font-[450]"
      />
      <div className="mt-1 flex flex-wrap items-center gap-2 text-xs">
        {shown.note && <span className="text-subtle">{shown.note}</span>}
        <button
          type="button"
          onClick={() => void toRich()}
          className="ml-auto rounded-md px-1.5 py-0.5 text-subtle hover:bg-hover hover:text-fg"
        >
          Edit formatted
        </button>
      </div>
    </div>
  )
}
