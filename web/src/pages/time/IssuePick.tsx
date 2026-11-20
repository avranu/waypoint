import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { Hash, X } from 'lucide-react'
import { q, usePrefetch } from '@/lib/queries'
import { useDebounced } from '@/lib/hooks'
import type { IssueRef } from '@/lib/types'
import { Popover, Picker, usePopover, type PickItem } from '@/components/ui/popover'
import { TypeIcon } from '@/components/issue/icons'
import { issuePickItem } from '@/components/issue/IssuePeek'

/** Pick an issue (or none) by searching. */
export function IssuePick({
  value,
  onChange,
  className,
  placeholder = 'Issue',
}: {
  value: IssueRef | null
  onChange: (i: IssueRef | null) => void
  className?: string
  placeholder?: string
}) {
  const p = usePopover()
  const [text, setText] = useState('')
  const debounced = useDebounced(text.trim(), 150)
  const query = debounced || 'assignee:me is:open order:-updated'
  usePrefetch(q.search('assignee:me is:open order:-updated', { limit: 30 }))
  const search = useQuery({
    ...q.search(query, { limit: 30 }),
    enabled: p.open,
    placeholderData: (prev) => prev,
  })
  const results = search.data
  const byKey = useMemo(() => new Map((results?.issues ?? []).map((i) => [i.key, i])), [results])
  const items: PickItem<string | null>[] = [
    ...(value
      ? [
          {
            value: null,
            fixed: true,
            label: 'No issue',
            icon: <X className="size-3.5 text-subtle" />,
          },
        ]
      : []),
    ...(results?.issues ?? []).map(issuePickItem),
  ]
  const close = () => {
    p.close()
    setText('')
  }
  return (
    <>
      <button
        ref={p.anchor}
        type="button"
        onClick={p.toggle}
        title={value ? `${value.key} ${value.title}` : 'Pick an issue'}
        className={clsx(
          'inline-flex h-9 min-w-0 items-center gap-1.5 rounded-lg border border-line bg-panel px-2.5 text-[13px] transition-colors hover:bg-hover',
          className,
        )}
      >
        {value ? (
          <>
            <TypeIcon type={value.type} />
            <span className="shrink-0 font-medium">{value.key}</span>
            <span className="truncate text-muted">{value.title}</span>
          </>
        ) : (
          <>
            <Hash className="size-3.5 shrink-0 text-subtle" />
            <span className="truncate text-subtle">{placeholder}</span>
          </>
        )}
      </button>
      <Popover open={p.open} onClose={close} anchor={p.anchor}>
        <div>
          <Picker
            items={items}
            selected={[value?.key ?? null]}
            placeholder="Search issues…"
            onSearch={setText}
            searching={text.trim() !== debounced || search.isPlaceholderData}
            wide
            onClose={close}
            onPick={(k) => {
              if (k === null) return onChange(null)
              const i = byKey.get(k)
              if (i) onChange({ id: i.id, key: i.key, title: i.title, type: i.type })
            }}
          />
          {!debounced && results && results.issues.length === 0 && (
            <div className="px-3 pb-2 text-center text-xs text-subtle">
              Type to search all issues
            </div>
          )}
        </div>
      </Popover>
    </>
  )
}
