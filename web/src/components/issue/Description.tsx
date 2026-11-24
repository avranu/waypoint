import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import clsx from 'clsx'
import { Button } from '@/components/ui/button'
import { useQuery } from '@tanstack/react-query'
import { renderMarkdown, toggleTask } from '@/lib/markdown'
import { refsQuery } from '@/lib/knowledge'
import type { TextRef } from '@/lib/types'
import { MarkdownInput } from './MarkdownInput'
import { RichTextInput } from './RichTextInput'
import { survivesRichEdit } from '@/lib/richtext'
import { clearDraft, readDraft, tracked, useUnsaved, writeDraft } from '@/lib/drafts'

// The editor a description opens in is remembered (WP-628).
const MODE_KEY = 'wp.editor.mode'
// A choice whose write failed, with what storage held then: it applies only
// while storage still holds that, so another tab's later choice shows.
let chosen: { mode: 'rich' | 'md'; stored: string | null } | null = null
const stored = (): string | null => {
  try {
    return localStorage.getItem(MODE_KEY)
  } catch {
    return null
  }
}
const readMode = (): 'rich' | 'md' => {
  const now = stored()
  if (chosen && chosen.stored === now) return chosen.mode
  chosen = null
  return now === 'md' ? 'md' : 'rich'
}
const writeMode = (m: 'rich' | 'md') => {
  try {
    localStorage.setItem(MODE_KEY, m)
    chosen = null
  } catch {
    chosen = { mode: m, stored: stored() }
  }
}

// How long typing pauses before an autosave goes out.
const AUTOSAVE_MS = 1800

type SaveState = 'idle' | 'saving' | 'saved' | 'error'

/** Markdown text that becomes an editor on click; task checkboxes toggle in place. */
export function MarkdownEditor({
  value,
  onSave,
  placeholder = 'Add a description…',
  editable = true,
  minRows = 4,
  autoEdit = false,
  onCancel,
  issueKey,
  attachTo,
  project,
  draftKey,
  autosave = false,
  batchedRefs,
  rich = false,
  ariaLabel,
}: {
  value: string
  onSave: (v: string) => Promise<unknown> | void
  placeholder?: string
  editable?: boolean
  minRows?: number
  autoEdit?: boolean
  onCancel?: () => void
  /** Lets files pasted or dropped while editing upload to this issue. */
  issueKey?: string
  /** Same for a page or design note: page:12, design:3. */
  attachTo?: string
  /**
   * Edits with formatting shown in place (WP-628), for a task description.
   * The value is still markdown, so everything that reads it is unchanged.
   */
  rich?: boolean
  /** Names the editor for assistive tech, e.g. "Description". */
  ariaLabel?: string
  /** The project [[wikilinks]] are looked up in. */
  project?: string
  /** Keeps an open edit in this browser until it's saved (WP-68). */
  draftKey?: string
  /**
   * Saves after a pause in typing and when focus leaves, instead of on a Save
   * button; the draft stays until the server has it (WP-81). For in-place
   * edits of something that already exists: editing it again is cheap.
   */
  autosave?: boolean
  /**
   * The parent resolves the codes of many editors in one call (WP-184); pass
   * its answer (undefined while loading) instead of asking per editor.
   */
  batchedRefs?: { refs: TextRef[] | undefined }
}) {
  const navigate = useNavigate()
  // An edit left open (a refresh, a closed tab) comes back open.
  const [stored] = useState(() => (draftKey ? readDraft<string>(draftKey) : null))
  const [restored, setRestored] = useState(stored !== null && stored !== value)
  const [editing, setEditing] = useState(autoEdit || restored)
  // What's on screen: the saved text plus any checkbox toggles still being
  // saved, so a quick second toggle builds on the first instead of undoing it.
  const [shown, setShown] = useState(value)
  useEffect(() => setShown(value), [value])
  const [draft, setDraftState] = useState(restored ? stored! : value)
  const [kept, setKept] = useState(true)
  useEffect(() => {
    if (!editing) setDraftState(shown)
  }, [shown, editing])
  const setDraft = (v: string) => {
    // An upload can end after Done/Escape closed the editor. Nothing autosaves
    // then, so the link (or the removal of a failed upload's placeholder)
    // would stay in local state; send it now. `latest` is a ref because the
    // upload's closure is stale. Without autosave, the text is kept as a
    // draft and the next edit session shows it.
    if (autosave && !latest.current.editing) {
      latest.current.draft = v
      setDraftState(v)
      void flush()
      return
    }
    setDraftState(v)
    if (draftKey) setKept(writeDraft(draftKey, v !== shown ? v : null))
  }
  const forget = () => {
    if (draftKey) clearDraft(draftKey)
    setKept(true)
    setRestored(false)
  }
  useUnsaved(editing && draft !== shown && !kept)
  const own = useQuery({
    ...refsQuery(shown, project),
    placeholderData: (p) => p,
    enabled: !batchedRefs,
  }).data
  const refs = batchedRefs ? batchedRefs.refs : own
  const html = useMemo(() => renderMarkdown(shown, { project, refs }), [shown, project, refs])

  // Close right away (the change shows optimistically); if the save fails,
  // reopen the editor with the text so nothing typed is lost.
  const save = async () => {
    const text = draft
    setEditing(false)
    if (text === shown) return (forget(), onCancel?.())
    setShown(text)
    try {
      await onSave(text)
      forget()
    } catch {
      setShown(value)
      setDraft(text)
      setEditing(true)
    }
  }

  const cancel = () => {
    forget()
    setDraftState(shown)
    setEditing(false)
    onCancel?.()
  }

  // ---- autosave ----
  const [state, setState] = useState<SaveState>('idle')
  const latest = useRef({ draft, onSave, draftKey, editing })
  latest.current = { draft, onSave, draftKey, editing }
  // The text the server is known to hold, and every text sent from this edit.
  const base = useRef(value)
  const sent = useRef(new Set<string>())
  const queue = useRef<Promise<unknown>>(Promise.resolve())
  const wasEditing = useRef(editing)
  if (editing && !wasEditing.current) ((base.current = value), sent.current.clear())
  wasEditing.current = editing
  // The text changed on the server since editing began, by someone else:
  // saving would overwrite it, so autosave waits for a choice.
  const conflict =
    autosave && editing && value !== base.current && !sent.current.has(value) && value !== draft

  // Sends the draft; resolves true once the server has it (or has all of it).
  const flush = (): Promise<boolean> => {
    const run = async () => {
      const text = latest.current.draft
      if (text === base.current) return true
      setState('saving')
      sent.current.add(text)
      setShown(text)
      try {
        await tracked(Promise.resolve(latest.current.onSave(text)))
        base.current = text
        if (latest.current.draft === text) {
          if (latest.current.draftKey) clearDraft(latest.current.draftKey)
          setKept(true)
          setRestored(false)
        }
        // Whatever was typed meanwhile is the next autosave's job; never
        // leave this at 'saving', which held that autosave back.
        setState('saved')
        return true
      } catch {
        setShown(base.current)
        setState('error')
        return false
      }
    }
    const p = queue.current.then(run)
    queue.current = p
    return p
  }
  const settle = () => flush().then((ok) => ok && (setEditing(false), onCancel?.()))
  const dirty = autosave && editing && draft !== base.current
  useEffect(() => {
    if (!dirty || conflict) return
    const t = setTimeout(() => void flush(), AUTOSAVE_MS)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- flush reads refs
  }, [draft, dirty, conflict, state])
  // Leaving the page (or this issue) sends what's pending.
  useEffect(
    () => () => {
      const { draft: d, onSave: f, editing: on } = latest.current
      if (autosave && on && d !== base.current && !sent.current.has(d))
        void Promise.resolve(f(d)).catch(() => {})
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only on unmount
    [],
  )

  // Which editor is open (WP-628): the remembered one, unless the text has
  // formatting the rich editor can't keep, which opens as markdown with a note.
  const opening = useMemo(
    () => {
      if (!rich || !editing) return null
      if (readMode() === 'md') return { mode: 'md' as const, note: false }
      return survivesRichEdit(draft)
        ? { mode: 'rich' as const, note: false }
        : { mode: 'md' as const, note: true }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- decided once per edit
    [rich, editing],
  )
  const [picked, setPicked] = useState<{ mode: 'rich' | 'md'; note: boolean } | null>(null)
  useEffect(() => {
    if (!editing) setPicked(null)
  }, [editing])
  const editor = picked ?? opening
  const toRich = () => {
    if (survivesRichEdit(draft)) {
      writeMode('rich')
      setPicked({ mode: 'rich', note: false })
    } else setPicked({ mode: 'md', note: true })
  }
  const toMarkdown = () => {
    writeMode('md')
    setPicked({ mode: 'md', note: false })
  }

  const editorKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      if (autosave) void settle()
      else save()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      e.stopPropagation()
      if (autosave && !conflict) void settle()
      else cancel()
    }
  }

  const uploads = !!(issueKey || attachTo)
  if (editing) {
    return (
      <div className="space-y-2" onBlur={autosave && !conflict ? () => void flush() : undefined}>
        {rich && editor?.mode === 'rich' ? (
          <RichTextInput
            autoFocus
            value={draft}
            onValueChange={setDraft}
            issueKey={issueKey}
            attachTo={attachTo}
            onKeyDown={editorKeyDown}
            ariaLabel={ariaLabel}
            getValue={() => latest.current.draft}
            placeholder={`Write here. Type @ to mention someone${uploads ? '; paste or drop files to attach them.' : '.'}`}
            onSource={toMarkdown}
          />
        ) : (
          <MarkdownInput
            autoFocus
            autoGrow
            rows={minRows}
            value={draft}
            onValueChange={setDraft}
            issueKey={issueKey}
            attachTo={attachTo}
            onKeyDown={editorKeyDown}
            aria-label={ariaLabel}
            placeholder={`Markdown supported: **bold**, - [ ] tasks, \`code\`, links, WAY-12 references, [[wikilinks]], @mentions${uploads ? '; paste or drop files' : ''}`}
            className="font-[450]"
          />
        )}
        {rich && editor?.mode === 'md' && (
          <div className="flex flex-wrap items-center gap-2 text-xs">
            {editor.note && (
              <span className="text-subtle">
                Opened as markdown: this text has formatting the editor can't keep.
              </span>
            )}
            <button
              type="button"
              onClick={toRich}
              className="ml-auto rounded-md px-1.5 py-0.5 text-subtle hover:bg-hover hover:text-fg"
            >
              Edit formatted
            </button>
          </div>
        )}
        {autosave ? (
          <div className="flex items-center gap-2">
            {conflict ? (
              <>
                <span className="text-xs text-warn">Changed elsewhere since you started</span>
                <Button
                  size="sm"
                  variant="primary"
                  onClick={() => {
                    base.current = value
                    void flush()
                  }}
                >
                  Overwrite
                </Button>
                <Button size="sm" variant="ghost" onClick={cancel}>
                  Discard mine
                </Button>
              </>
            ) : (
              <>
                <button
                  type="button"
                  onClick={() => void settle()}
                  className="rounded-md px-1.5 py-0.5 text-xs text-subtle hover:bg-hover hover:text-fg"
                >
                  Done
                </button>
                {restored && state === 'idle' && (
                  <span className="text-xs text-muted">Unsaved edit restored</span>
                )}
                {state === 'error' && (
                  <button
                    type="button"
                    onClick={() => void flush()}
                    className="text-xs text-danger underline"
                  >
                    Not saved: retry
                  </button>
                )}
              </>
            )}
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <Button size="sm" variant="primary" onClick={save}>
              Save
            </Button>
            <Button size="sm" variant="ghost" onClick={cancel}>
              Cancel
            </Button>
            {restored && <span className="text-xs text-muted">Unsaved edit restored</span>}
            {draft !== shown && (
              <span className="text-xs text-subtle">
                {kept && draftKey ? 'Not saved yet · draft kept here' : 'Not saved yet'}
              </span>
            )}
          </div>
        )}
      </div>
    )
  }

  const onClick = (e: React.MouseEvent) => {
    const t = e.target as HTMLElement
    const a = t.closest('a')
    if (a) {
      // Issue links stay in the app instead of reloading the page.
      const href = a.getAttribute('href') ?? ''
      if (href.startsWith('/') && !href.startsWith('//') && !e.metaKey && !e.ctrlKey) {
        e.preventDefault()
        navigate({ to: href })
      }
      return
    }
    if (t instanceof HTMLInputElement && t.type === 'checkbox') {
      e.preventDefault()
      if (!editable) return
      const boxes = Array.from((e.currentTarget as HTMLElement).querySelectorAll('input.task'))
      const next = toggleTask(shown, boxes.indexOf(t), boxes.length)
      if (next === null) {
        setEditing(true) // can't map the box to the source safely
        return
      }
      setShown(next)
      // The save path reports errors; show the saved text again.
      Promise.resolve(onSave(next)).catch(() => setShown(value))
      return
    }
    if (editable && !window.getSelection()?.toString()) setEditing(true)
  }

  if (!shown) {
    return (
      <button
        disabled={!editable}
        onClick={() => setEditing(true)}
        className="w-full rounded-lg px-1 py-2 text-left text-sm text-subtle hover:bg-hover disabled:hover:bg-transparent"
      >
        {editable ? placeholder : 'No description.'}
      </button>
    )
  }
  return (
    <div
      onClick={onClick}
      tabIndex={editable ? 0 : undefined}
      onKeyDown={(e) => {
        if (editable && e.key === 'Enter' && e.target === e.currentTarget) {
          e.preventDefault()
          setEditing(true)
        }
      }}
      title={editable ? 'Click or press Enter to edit' : undefined}
      className={clsx(
        'prose-wp -mx-1 rounded-lg px-1 py-1 text-sm leading-relaxed',
        editable && 'cursor-text hover:bg-hover/50',
      )}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
}
