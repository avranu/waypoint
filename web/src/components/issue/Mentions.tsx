import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { GitCommit, GitPullRequest, CircleDot } from 'lucide-react'
import { safeUrl } from '@/lib/format'
import { itemHref, linksQuery } from '@/lib/knowledge'
import type { ForgeMention, KLink } from '@/lib/types'
import { ItemIcon } from '@/components/Links'

/** Links made from text, not by a person choosing them. */
export const isMention = (l: KLink) => !!l.auto && l.rel === 'mentions'

const chipClass =
  'inline-flex h-6 items-center gap-1 rounded-full border border-line bg-sunken px-2 text-[12px] text-muted'

function Chip({ link }: { link: KLink }) {
  const it = link.item
  const label = it.type === 'issue' || it.type === 'page' ? it.key || it.title : it.title
  const href = itemHref(it)
  const body = (
    <>
      <ItemIcon item={it} className="size-3" />
      <span className="max-w-40 truncate">{label}</span>
    </>
  )
  const cls = chipClass
  return href ? (
    <Link to={href} title={it.title} className={cls + ' hover:bg-hover hover:text-fg'}>
      {body}
    </Link>
  ) : (
    <span title={it.title} className={cls}>
      {body}
    </span>
  )
}

function Group({ label, links }: { label: string; links: KLink[] }) {
  if (links.length === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-1">
      <span className="mr-1 text-[11px] font-medium text-subtle">
        {label} {links.length}
      </span>
      {links.map((l) => (
        <Chip key={`${l.id}:${l.item.type}:${l.item.id}`} link={l} />
      ))}
    </div>
  )
}

const VERB: Record<string, string> = {
  part_of: 'part of',
  depends_on: 'depends on',
  follows: 'follow-up to',
  relates: 'refers to',
  not: 'says not',
}

function forgeLabel(m: ForgeMention) {
  if (m.kind === 'commit') return m.url.replace(/.*\/commit\//, '').slice(0, 7)
  const repo = m.repo.split('/').pop()
  return m.number ? `${repo}#${m.number}` : m.url.replace(/^https?:\/\//, '')
}

function ForgeChip({ m }: { m: ForgeMention }) {
  const Icon = m.kind === 'pull' ? GitPullRequest : m.kind === 'commit' ? GitCommit : CircleDot
  const verb = m.verb && m.verb !== 'issue' ? (VERB[m.verb] ?? m.verb) : ''
  const where = `${verb ? verb + ', ' : ''}in its ${m.places.join(', ')}`
  const tip = [
    `${m.repo} ${m.kind === 'pull' ? 'pull request' : m.kind}${m.state ? ` (${m.state})` : ''}`,
    m.title,
    where,
    m.excerpt && `“${m.excerpt}”`,
  ]
    .filter(Boolean)
    .join('\n')
  return (
    <a
      href={safeUrl(m.source_url || m.url)}
      target="_blank"
      rel="noopener noreferrer"
      title={tip}
      className={chipClass + ' hover:bg-hover hover:text-fg'}
    >
      <Icon className="size-3 shrink-0 text-subtle" />
      <span className="max-w-40 truncate">{forgeLabel(m)}</span>
      {verb && <span className="text-[11px] text-subtle">{verb}</span>}
    </a>
  )
}

export function Mentions({
  issueKey,
  forge = [],
  related = [],
}: {
  issueKey: string
  forge?: ForgeMention[]
  /** Keys in the Relations section, not repeated here (WP-781). */
  related?: string[]
}) {
  const links = useQuery(linksQuery({ type: 'issue', key: issueKey }))
  const list = (links.data ?? []).filter(
    (l) => isMention(l) && !(l.item.type === 'issue' && related.includes(l.item.key ?? '')),
  )
  const out = list.filter((l) => l.direction === 'out')
  const into = list.filter((l) => l.direction === 'in')
  if (list.length === 0 && forge.length === 0) return null
  return (
    <section aria-label="Mentions" className="space-y-1">
      <Group label="Mentions" links={out} />
      <Group label="Mentioned by" links={into} />
      {forge.length > 0 && (
        <div className="flex flex-wrap items-center gap-1">
          <span
            className="mr-1 text-[11px] font-medium text-subtle"
            title="Pull requests, commits and forge issues that name this issue without being its work. Not linked: they don't hold it in review or finish it."
          >
            On the forge {forge.length}
          </span>
          {forge.map((m) => (
            <ForgeChip key={m.url} m={m} />
          ))}
        </div>
      )}
    </section>
  )
}
