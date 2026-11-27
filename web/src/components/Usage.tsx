import { useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { AtSign, ExternalLink, Eye, TriangleAlert } from 'lucide-react'
import { itemUsageQuery, type KnowledgeUse, type UsedType } from '@/lib/usage'
import { itemHref } from '@/lib/knowledge'
import { dateTime, plural, relativeTime, safeUrl } from '@/lib/format'
import type { KItem } from '@/lib/types'
import { BlockTitle, WhoAvatar } from './knowledge'

const SHOWN = 5

export function Usage({ type, id, name }: { type: UsedType; id: number; name: string }) {
  const q = useQuery(itemUsageQuery(type, id))
  const [all, setAll] = useState(false)
  const u = q.data
  if (!u) return null
  const { counts } = u
  const parts = [
    counts.lookups > 0 && `Looked up ${times(counts.lookups)}`,
    counts.mentions > 0 &&
      `${counts.lookups > 0 ? 'named' : 'Named'} in ${plural(counts.mentions, 'place')}`,
  ].filter(Boolean)
  const shown = all ? u.uses : u.uses.slice(0, SHOWN)
  return (
    <section aria-label="Usage">
      <BlockTitle>Usage</BlockTitle>
      {u.uses.length === 0 ? (
        <p className="text-[13px] text-subtle">
          Not used yet. Lookups by agents and the CLI, and text that names it, show here.
        </p>
      ) : (
        <>
          <p className="mb-2 text-[13px] text-muted">
            {parts.join(' · ')}
            {counts.avoided > 0 && (
              <span className="text-warn">
                {parts.length > 0 ? ' · a' : 'A'} word to avoid written {times(counts.avoided)}
              </span>
            )}
            {u.last_used_at && (
              <span className="text-subtle" title={dateTime(u.last_used_at)}>
                {' '}
                · last {relativeTime(u.last_used_at)}
              </span>
            )}
          </p>
          <ol className="overflow-hidden rounded-lg border border-line">
            {shown.map((x, i) => (
              <UseRow key={i} use={x} name={name} />
            ))}
          </ol>
          {u.uses.length > SHOWN && (
            <button onClick={() => setAll(!all)} className="mt-1 text-xs text-subtle hover:text-fg">
              {all ? 'Show fewer' : `Show all ${u.uses.length}`}
            </button>
          )}
        </>
      )}
    </section>
  )
}

/** One use as a sentence: who did what, where, when. */
export function UseRow({
  use: x,
  name,
  item,
}: {
  use: KnowledgeUse
  /** the item's own name; a word that differs from it is shown */
  name?: string
  /** overviews: the item used, linked */
  item?: KItem
}) {
  const word = x.word && (!name || x.word.toLowerCase() !== name.toLowerCase()) ? x.word : undefined
  const who = x.user || x.agent ? <WhoAvatar user={x.user} agent={x.agent} /> : null
  let text: ReactNode
  switch (x.kind) {
    case 'lookup':
      text = (
        <>
          {item ? <SourceLink item={item} /> : 'Looked up'}
          {word && <> as “{word}”</>}
        </>
      )
      break
    case 'avoided':
      text = (
        <>
          <b className="font-medium text-warn">“{x.word}”</b>
          {item && (
            <>
              {' '}
              instead of <SourceLink item={item} />
            </>
          )}{' '}
          in <Where use={x} />
        </>
      )
      break
    default:
      text = (
        <>
          {item && (
            <>
              <SourceLink item={item} />
              {' in '}
            </>
          )}
          {word && <>“{word}” in </>}
          <Where use={x} />
        </>
      )
  }
  return (
    <li className="flex items-start gap-2 border-b border-line px-3 py-2 text-[13px] last:border-b-0">
      <UseIcon kind={x.kind} />
      {who && <span className="mt-px">{who}</span>}
      <span className="min-w-0 flex-1 break-words text-muted">{text}</span>
      <time className="shrink-0 text-xs text-subtle" title={dateTime(x.at)}>
        {relativeTime(x.at)}
      </time>
    </li>
  )
}

export function UseIcon({ kind, className }: { kind: KnowledgeUse['kind']; className?: string }) {
  const c = clsx('mt-0.5 size-3.5 shrink-0', className)
  if (kind === 'lookup') return <Eye className={clsx(c, 'text-subtle')} aria-label="Looked up" />
  if (kind === 'avoided')
    return <TriangleAlert className={clsx(c, 'text-warn')} aria-label="Word to avoid" />
  return <AtSign className={clsx(c, 'text-subtle')} aria-label="Named" />
}

function Where({ use: x }: { use: KnowledgeUse }) {
  if (!x.source) return <>text you can't see</>
  return (
    <>
      {x.comment && 'a comment on '}
      <SourceLink item={x.source} />
    </>
  )
}

/** An item named inline: in the app, or out to the forge for a pull request. */
export function SourceLink({ item }: { item: KItem }) {
  const href = itemHref(item)
  const label = (
    <>
      {(item.type === 'issue' || item.type === 'page') && item.key && (
        <span className="text-subtle">{item.key} </span>
      )}
      {item.type === 'url' && item.repo && (
        <span className="text-subtle">
          {item.repo}#{item.number}{' '}
        </span>
      )}
      <span className="text-fg">{item.title}</span>
    </>
  )
  if (href)
    return (
      <Link to={href} className="hover:underline">
        {label}
      </Link>
    )
  const url = item.ref ? safeUrl(item.ref) : undefined
  if (url)
    return (
      <a href={url} target="_blank" rel="noopener noreferrer" className="hover:underline">
        {label}
        <ExternalLink className="ml-0.5 inline size-3 align-[-1px] text-subtle" />
      </a>
    )
  return label
}

/** "once", "twice", "5 times" */
export function times(n: number) {
  return n === 1 ? 'once' : n === 2 ? 'twice' : `${n} times`
}
