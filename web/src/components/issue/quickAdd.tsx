import { useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { q, useCreateIssue, type IssueInput } from '@/lib/queries'
import { parseQuickAdd, splitLongTitle, titleTooLong, type Parsed } from '@/lib/quickadd'
import { useDraft } from '@/lib/drafts'
import { PRIORITIES, type IssueDetail } from '@/lib/types'

export function useQuickAdd(draftKey: string | null) {
  const create = useCreateIssue()
  const users = useQuery(q.users()).data ?? []
  const [text, setDraft] = useDraft(draftKey, '')
  const [error, setError] = useState<string | null>(null)
  const sending = useRef(false)
  // The latest text, read at submit time: a second Enter queued behind the
  // first can run before the re-render that shows the cleared box.
  const latest = useRef(text)
  latest.current = text
  const parse = (t: string) =>
    parseQuickAdd(
      t,
      users.map((u) => u.username),
    )

  /**
   * Sends the text as built by build; resolves to the new issue, or to
   * undefined when there was nothing to send or the create failed (the
   * text then stays, with error set).
   */
  const submit = async (
    build: (p: Parsed) => IssueInput | null,
  ): Promise<IssueDetail | undefined> => {
    // One at a time: the text stays until the server has it, so a second
    // Enter while the first is on its way would add a duplicate. A ref, not
    // create.isPending: a second Enter before the re-render still sees false.
    if (sending.current) return
    const p = parse(latest.current)
    if (!p.title.trim()) return
    const input = build(p)
    if (!input) return
    setError(null)
    sending.current = true
    try {
      const d = await create.mutateAsync(input)
      latest.current = ''
      setDraft('')
      return d
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      sending.current = false
    }
  }
  /**
   * What the new-issue dialog opens with (WP-125): the typed text as the
   * dialog would read it, text too long for a title split as a create would
   * split it, plus extra (where the box adds to). The draft is cleared: the
   * dialog keeps what's written from here on.
   */
  const details = (extra: Partial<IssueInput> = {}): Partial<IssueInput> => {
    const p = parse(latest.current)
    const { title, description } = splitLongTitle(p.title)
    const { priority, ...rest } = p.input
    latest.current = ''
    setDraft('')
    setError(null)
    return {
      ...extra,
      ...rest,
      ...(typeof priority === 'string' && { priority: PRIORITIES.indexOf(priority as never) }),
      title,
      description,
    }
  }
  return {
    text,
    details,
    setText: (t: string) => ((latest.current = t), setDraft(t), setError(null)),
    submit,
    error,
    pending: create.isPending,
    long: titleTooLong(parse(text).title),
  }
}

/** Said under a title field holding more than a title can. */
export function LongTitleHint({ show, className }: { show: boolean; className?: string }) {
  if (!show) return null
  return (
    <p className={className ?? 'text-[11px] text-muted'}>
      Too long for a title: the first sentence becomes the title and the full text goes in the
      description.
    </p>
  )
}

/** Why a save failed, next to the text that's still there. */
export function SaveError({ error, className }: { error: string | null; className?: string }) {
  if (!error) return null
  return (
    <p role="alert" className={className ?? 'text-[11px] text-danger'}>
      Not saved: {error}
    </p>
  )
}
