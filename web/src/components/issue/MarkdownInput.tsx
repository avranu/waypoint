import { forwardRef, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { q } from '@/lib/queries'
import { Textarea } from '@/components/ui/input'
import { useEscapeLayer } from '@/components/ui/layers'
import { toastError } from '@/components/ui/toast'
import { issueTarget, refreshAttachments, uploadAttachment } from '@/lib/attachments'
import type { UserRef } from '@/lib/types'
import { MENTION_RE, MentionList, useMentionMatches } from './userMention'

type Props = Omit<React.TextareaHTMLAttributes<HTMLTextAreaElement>, 'value' | 'onChange'> & {
  value: string
  onValueChange: (v: string) => void
  /** Enables paste/drop uploads to this issue. */
  issueKey?: string
  /** Same for anything files attach to: page:12, design:3 (wins over issueKey). */
  attachTo?: string
  autoGrow?: boolean
}

export const MarkdownInput = forwardRef<HTMLTextAreaElement, Props>(function MarkdownInput(
  { value, onValueChange, issueKey, attachTo, onKeyDown, className, ...rest },
  outerRef,
) {
  const qc = useQueryClient()
  const target = attachTo ?? (issueKey ? issueTarget(issueKey) : undefined)
  const ref = useRef<HTMLTextAreaElement | null>(null)
  const latest = useRef(value)
  latest.current = value
  const setValue = (v: string) => {
    latest.current = v
    onValueChange(v)
  }
  const users = useQuery(q.users()).data ?? []
  const [mention, setMention] = useState<{ start: number; text: string } | null>(null)
  const [active, setActive] = useState(0)
  const [dragging, setDragging] = useState(false)

  const matches = useMentionMatches(users, mention?.text)
  const open = !!mention && matches.length > 0
  useEscapeLayer(open, () => setMention(null))
  useEffect(() => setActive(0), [mention?.text])

  const detect = (el: HTMLTextAreaElement) => {
    if (el.selectionStart !== el.selectionEnd) return setMention(null)
    const m = MENTION_RE.exec(el.value.slice(0, el.selectionStart))
    setMention(m ? { start: el.selectionStart - m[2].length - 1, text: m[2] } : null)
  }

  const pick = (u: UserRef) => {
    const el = ref.current
    if (!el || !mention) return
    const end = el.selectionStart
    const insert = '@' + u.username + ' '
    const next = value.slice(0, mention.start) + insert + value.slice(end)
    onValueChange(next)
    setMention(null)
    const caret = mention.start + insert.length
    requestAnimationFrame(() => {
      el.focus()
      el.setSelectionRange(caret, caret)
    })
  }

  const insertAtCaret = (text: string) => {
    const el = ref.current
    const v = latest.current
    const s = el?.selectionStart ?? v.length
    const e = el?.selectionEnd ?? v.length
    setValue(v.slice(0, s) + text + v.slice(e))
    requestAnimationFrame(() => el?.setSelectionRange(s + text.length, s + text.length))
  }

  const upload = (files: File[]) => {
    if (!target || !files.length) return
    const placeholders = files.map((f) => `[Uploading ${f.name || 'pasted image'}…]()`)
    const v = latest.current
    const caret = ref.current?.selectionStart ?? v.length
    const lead = caret > 0 && !v.slice(0, caret).endsWith('\n') ? '\n' : ''
    insertAtCaret(lead + placeholders.join('\n') + '\n')
    files.forEach((f, i) => {
      uploadAttachment(target, f).then(
        (a) => {
          setValue(latest.current.replace(placeholders[i], a.markdown))
          refreshAttachments(qc, target)
        },
        (err) => {
          setValue(latest.current.replace(placeholders[i] + '\n', '').replace(placeholders[i], ''))
          toastError(err)
        },
      )
    })
  }

  return (
    <div
      className="relative"
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
        ref.current?.focus()
        upload([...e.dataTransfer.files])
      }}
    >
      <Textarea
        {...rest}
        ref={(el) => {
          ref.current = el
          if (typeof outerRef === 'function') outerRef(el)
          else if (outerRef) outerRef.current = el
        }}
        value={value}
        className={clsx(className, dragging && 'border-accent ring-2 ring-accent/20')}
        onChange={(e) => {
          onValueChange(e.target.value)
          detect(e.target)
        }}
        onClick={(e) => detect(e.currentTarget)}
        onBlur={() => setTimeout(() => setMention(null), 150)}
        onPaste={(e) => {
          const files = [...e.clipboardData.files]
          if (!target || !files.length) return
          e.preventDefault()
          upload(files)
        }}
        onKeyDown={(e) => {
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
          }
          onKeyDown?.(e)
        }}
        onKeyUp={(e) => {
          if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) detect(e.currentTarget)
        }}
      />
      {open && ref.current && (
        <MentionMenu
          anchor={ref.current}
          caret={mention.start}
          users={matches}
          active={active}
          onHover={setActive}
          onPick={pick}
        />
      )}
    </div>
  )
})

function MentionMenu({
  anchor,
  caret,
  users,
  active,
  onHover,
  onPick,
}: {
  anchor: HTMLTextAreaElement
  caret: number
  users: UserRef[]
  active: number
  onHover: (i: number) => void
  onPick: (u: UserRef) => void
}) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  useLayoutEffect(() => {
    const c = caretCoords(anchor, caret)
    const r = anchor.getBoundingClientRect()
    const left = Math.max(8, Math.min(r.left + c.left, window.innerWidth - 248))
    let top = r.top + c.top + c.height + 4
    if (top + 220 > window.innerHeight) top = Math.max(8, r.top + c.top - 220)
    setPos({ top, left })
  }, [anchor, caret])
  return <MentionList users={users} active={active} pos={pos} onHover={onHover} onPick={onPick} />
}

/** Where a character index sits inside a textarea, relative to its box. */
function caretCoords(el: HTMLTextAreaElement, index: number) {
  const style = getComputedStyle(el)
  const div = document.createElement('div')
  for (const p of [
    'boxSizing',
    'width',
    'paddingTop',
    'paddingRight',
    'paddingBottom',
    'paddingLeft',
    'borderTopWidth',
    'borderLeftWidth',
    'fontFamily',
    'fontSize',
    'fontWeight',
    'lineHeight',
    'letterSpacing',
    'tabSize',
  ] as const) {
    div.style[p] = style[p]
  }
  div.style.position = 'absolute'
  div.style.visibility = 'hidden'
  div.style.whiteSpace = 'pre-wrap'
  div.style.overflowWrap = 'break-word'
  div.textContent = el.value.slice(0, index)
  const span = document.createElement('span')
  span.textContent = el.value.slice(index) || '.'
  div.appendChild(span)
  document.body.appendChild(div)
  const top = span.offsetTop - el.scrollTop
  const left = span.offsetLeft
  const height = parseFloat(style.lineHeight) || 20
  div.remove()
  return { top, left, height }
}
