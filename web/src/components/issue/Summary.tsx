import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import { Bot, Check, Hourglass, Link2 } from 'lucide-react'
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { useLocal } from '@/lib/hooks'
import { agentLabel } from '@/lib/agents'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { relLabel } from '@/lib/relations'
import { Lifecycle } from './Lifecycle'
import type { IssueDetail, Project, Relation, RelationKind } from '@/lib/types'

export type IssueTab = 'summary' | 'details'

/** The tab a person last chose, for every task (per browser). */
export function useIssueTab() {
  return useLocal<IssueTab>('issue-tab', 'summary')
}

export function IssueTabs({ tab, setTab }: { tab: IssueTab; setTab: (t: IssueTab) => void }) {
  const tabs: { id: IssueTab; label: string; hint: string }[] = [
    { id: 'summary', label: 'Summary', hint: 'What it’s for and its life so far' },
    {
      id: 'details',
      label: 'Details',
      hint: 'Everything: the full write-up, links, files, agent runs and the work log',
    },
  ]
  return (
    <div role="tablist" aria-label="Task view" className="flex gap-4 border-b border-line">
      {tabs.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          aria-selected={tab === t.id}
          title={t.hint}
          onClick={() => setTab(t.id)}
          className={clsx(
            '-mb-px border-b-2 pb-1.5 text-[13px]',
            tab === t.id
              ? 'border-accent font-medium text-fg'
              : 'border-transparent text-muted hover:text-fg',
          )}
        >
          {t.label}
        </button>
      ))}
    </div>
  )
}

const WAITS: RelationKind[] = ['blocked_by']
/** Work that goes with this one; duplicates, replacements and tensions stay in Details. */
const GOES_WITH: RelationKind[] = ['relates', 'follows', 'followed_by']
const CONFIDENT = (r: Relation) => r.basis === 'person' || (r.score ?? 0) >= 0.8

function Fact({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <li className="flex min-w-0 items-start gap-2 text-[13px]">
      <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3.5">{icon}</span>
      <span className="min-w-0">{children}</span>
    </li>
  )
}

/**
 * What it's for, its life so far, and what it waits on (WP-1196). The intent
 * is its own short text, apart from the write-up in Details, so editing one
 * never changes the other.
 */
export function IssueSummary({
  issue,
  project,
  editable,
  onIntent,
  onDetails,
}: {
  issue: IssueDetail
  project: Project
  editable: boolean
  onIntent: (text: string) => Promise<unknown>
  onDetails: () => void
}) {
  const waits = issue.relations.filter(
    (r) => WAITS.includes(r.kind) && r.state !== 'done' && r.state !== 'canceled',
  )
  const related = issue.relations
    .filter((r) => GOES_WITH.includes(r.kind) && CONFIDENT(r))
    .slice(0, 3)
  return (
    <section aria-label="Summary" className="space-y-4 rounded-xl border border-line bg-panel p-4">
      <Intent issue={issue} editable={editable} onSave={onIntent} onDetails={onDetails} />
      <Lifecycle issue={issue} project={project} />
      {(waits.length > 0 || related.length > 0) && (
        <ul className="space-y-1.5">
          {waits.length > 0 && (
            <Fact icon={<Hourglass />}>
              Waits on{' '}
              {waits.map((r, i) => (
                <span key={r.id}>
                  {i > 0 && ', '}
                  <Link to="/issue/$key" params={{ key: r.issue.key }} className="hover:underline">
                    {r.issue.title}
                  </Link>
                </span>
              ))}
            </Fact>
          )}
          {related.map((r) => (
            <Fact key={r.id} icon={<Link2 />}>
              {relLabel[r.kind]}{' '}
              <Link to="/issue/$key" params={{ key: r.issue.key }} className="hover:underline">
                {r.issue.title}
              </Link>
              {r.subkind && (
                <>
                  {' '}
                  <Badge color={r.subkind === 'fix' ? 'var(--wp-warn)' : 'var(--wp-info)'}>
                    {r.subkind}
                  </Badge>
                </>
              )}
            </Fact>
          ))}
        </ul>
      )}
    </section>
  )
}

/**
 * The intent, edited in place: click and type, Enter or leaving saves, Esc
 * puts it back. One an agent wrote reads "Suggested by …" until a person
 * keeps it (the check) or rewrites it. With none yet, the write-up's first
 * paragraph stands in, read-only.
 */
/** The intent's limit, in characters (internal/core issues.go maxIntentLen). */
const MAX_INTENT = 1000

function Intent({
  issue,
  editable,
  onSave,
  onDetails,
}: {
  issue: IssueDetail
  editable: boolean
  onSave: (text: string) => Promise<unknown>
  onDetails: () => void
}) {
  const saved = issue.intent ?? ''
  const [text, setText] = useState(saved)
  const [focused, setFocused] = useState(false)
  const [held, setHeld] = useState(false)
  const ref = useRef<HTMLTextAreaElement>(null)
  useEffect(() => {
    if (!focused && !held) setText(saved)
  }, [saved, focused, held])
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = el.scrollHeight + 'px'
  }, [text])
  const over = [...text.trim()].length - MAX_INTENT
  const commit = () => {
    const t = text.trim()
    if (t === saved) {
      setHeld(false)
      return
    }
    if (over > 0) {
      setHeld(true)
      return
    }
    setHeld(true)
    onSave(t).then(
      () => setHeld(false),
      () => {},
    )
  }
  const excerpt = !saved && issue.description ? firstProse(issue.description) : ''
  const suggested = !!saved && !!issue.intent_agent
  return (
    <div className="space-y-1.5">
      {editable ? (
        <textarea
          ref={ref}
          rows={1}
          value={text}
          aria-invalid={over > 0 || undefined}
          aria-label="Intent"
          placeholder="What’s this for? One or two sentences."
          onChange={(e) => setText(e.target.value)}
          onFocus={() => setFocused(true)}
          onBlur={() => {
            setFocused(false)
            commit()
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault()
              e.currentTarget.blur()
            } else if (e.key === 'Escape') {
              e.preventDefault()
              setText(saved)
              setHeld(false)
              requestAnimationFrame(() => ref.current?.blur())
            }
          }}
          className="-mx-1 block w-[calc(100%+0.5rem)] resize-none overflow-hidden rounded-md bg-transparent px-1 py-0.5 text-[15px] leading-relaxed outline-none placeholder:text-subtle hover:bg-hover focus:bg-hover"
        />
      ) : (
        saved && <p className="whitespace-pre-line text-[15px] leading-relaxed">{saved}</p>
      )}
      {over > 0 && (
        <p role="alert" className="text-xs text-danger">
          {over} character{over === 1 ? '' : 's'} too long to save: a sentence or two, {MAX_INTENT}{' '}
          characters at most.
        </p>
      )}
      {suggested && (
        <div className="flex items-center gap-1.5 text-xs text-muted">
          <Bot className="size-3.5" />
          Suggested by {agentLabel(issue.intent_agent!)}
          {editable && (
            <Button
              size="xs"
              variant="ghost"
              icon={<Check className="size-3.5" />}
              title="Keep it as the intent; editing it keeps it too"
              onClick={() => onSave(saved).catch(() => {})}
            >
              Keep
            </Button>
          )}
        </div>
      )}
      {excerpt && (
        <p className="line-clamp-3 whitespace-pre-line text-[13px] text-muted">
          {excerpt}{' '}
          <button
            type="button"
            onClick={onDetails}
            className="text-xs text-subtle underline-offset-2 hover:text-fg hover:underline"
          >
            Details
          </button>
        </p>
      )}
    </div>
  )
}

const HEADING = /^\s{0,3}#{1,6}(\s|$)/
const NOT_PROSE = /^\s*([-*+]\s|\d+[.)]\s|\||<|!\[)/

/**
 * The first paragraph of prose in markdown, as plain words: headings, code
 * blocks, lists, tables and images are skipped, and inline marks dropped
 * without touching the words (so `my_file.go` stays my_file.go).
 */
export function firstProse(text: string): string {
  const kept: string[] = []
  let fence = ''
  for (const line of text.split('\n')) {
    const f = /^\s*(`{3,}|~{3,})/.exec(line)?.[1]
    if (fence) {
      if (f && f[0] === fence[0] && f.length >= fence.length) fence = ''
      continue
    }
    if (f) {
      fence = f
      kept.push('')
      continue
    }
    kept.push(HEADING.test(line) ? '' : line)
  }
  for (const block of kept.join('\n').split(/\n\s*\n/)) {
    const lines = block.split('\n').filter((l) => l.trim())
    if (!lines.length || NOT_PROSE.test(lines[0])) continue
    return lines
      .map((l) => l.replace(/^\s*>\s?/, '').trim())
      .join('\n')
      .replace(/!\[[^\]]*\]\([^)]*\)/g, '')
      .replace(/\[([^\]]*)\]\(([^)]*)\)/g, (_m, text: string, url: string) =>
        text.trim() === url.trim() ? text : `${text} (${url.trim()})`,
      )
      .replace(/`([^`]*)`/g, '$1')
      .replace(/\*\*(?=\S)(.+?)(?<=\S)\*\*/g, '$1')
      .replace(/(^|[\s(])\*(?=\S)(.+?)(?<=\S)\*(?=$|[\s.,;:!?)])/g, '$1$2')
      .trim()
  }
  return ''
}
