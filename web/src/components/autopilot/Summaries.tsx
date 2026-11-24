import { useEffect, useMemo, useRef, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Check,
  CheckCheck,
  Circle,
  ExternalLink,
  List,
  MessageCircleQuestion,
  Plane,
  Terminal,
  Undo2,
} from 'lucide-react'
import { autopilotSummariesQuery, useReviewSummaries } from '@/lib/queries'
import type { TaskSummary } from '@/lib/autopilot'
import { relativeTime } from '@/lib/format'
import { renderMarkdown } from '@/lib/markdown'
import { AppLink } from '@/components/AppLink'
import { TypeIcon } from '@/components/issue/icons'
import { Button, IconButton } from '@/components/ui/button'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toastError } from '@/components/ui/toast'

/** The nearest ancestor that scrolls, or null for the page itself. */
function scroller(el: Element | null): Element | null {
  for (let p = el?.parentElement; p; p = p.parentElement) {
    const y = getComputedStyle(p).overflowY
    if (y === 'auto' || y === 'scroll') return p
  }
  return null
}

/**
 * Calls `more` when the returned element comes within `margin` px of the
 * bottom of whatever scrolls it, so the next page is there before the end is
 * reached. It looks again after each page, so a short page that still leaves
 * the end in view loads the next.
 */
function useNearEnd(more: () => void, enabled: boolean, version: unknown, margin = 1200) {
  const ref = useRef<HTMLDivElement>(null)
  const latest = useRef(more)
  latest.current = more
  useEffect(() => {
    const el = ref.current
    if (!el || !enabled) return
    const io = new IntersectionObserver(
      (es) => es.some((e) => e.isIntersecting) && latest.current(),
      { root: scroller(el), rootMargin: `0px 0px ${margin}px 0px` },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [enabled, margin, version])
  return ref
}

const CLAMP_CHARS = 700

export function Summaries({
  project,
  unreviewed: overall,
  className,
}: {
  project: string
  /** Across every project; the badge and "Mark all read" follow the project shown instead. */
  unreviewed: number
  className?: string
}) {
  const [view, setView] = useState<'unread' | 'all'>('unread')
  const list = useInfiniteQuery(
    autopilotSummariesQuery({ project: project || undefined, all: view === 'all' }),
  )
  const review = useReviewSummaries()
  const unreviewed = list.data?.pages[0]?.unreviewed ?? (project ? 0 : overall)
  const items = list.data?.pages.flatMap((p) => p.summaries) ?? []
  const idle = !list.isFetchingNextPage && !list.isFetching
  const end = useNearEnd(
    () => void list.fetchNextPage(),
    !!list.hasNextPage && idle && !list.isFetchNextPageError,
    items.length,
  )
  return (
    <Section
      title={
        <span className="inline-flex items-center gap-2">
          <span>Tasks</span>
          {unreviewed > 0 && (
            <Badge
              color="var(--wp-accent)"
              title={`${unreviewed} unread task ${unreviewed === 1 ? 'summary' : 'summaries'}`}
            >
              {unreviewed}
            </Badge>
          )}
          {unreviewed > 0 && (
            <IconButton
              size="xs"
              label="Mark all read"
              icon={<CheckCheck className="size-3.5" />}
              loading={review.isPending && !!review.variables?.all}
              onClick={() =>
                review.mutate({ all: true, project: project || undefined }, { onError: toastError })
              }
            />
          )}
        </span>
      }
      className={className}
      actions={
        <SegmentedControl
          label="Summaries shown"
          value={view}
          onChange={setView}
          options={[
            { value: 'unread', label: 'Unread', icon: <Circle className="size-3" /> },
            { value: 'all', label: 'All', icon: <List className="size-3" /> },
          ]}
        />
      }
    >
      {list.isLoading ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
        </div>
      ) : list.isError && items.length === 0 ? (
        <p className="p-4 text-[13px] text-danger">{list.error.message}</p>
      ) : items.length === 0 ? (
        <Empty
          icon={<Check />}
          title={view === 'unread' ? 'Nothing unread' : 'No task summaries yet'}
        />
      ) : (
        <ul className="divide-y divide-line">
          {items.map((s) => (
            <SummaryCard
              key={s.id}
              s={s}
              showProject={!project}
              busy={review.isPending && review.variables?.ids?.includes(s.id)}
              onReview={(unread) => review.mutate({ ids: [s.id], unread }, { onError: toastError })}
            />
          ))}
        </ul>
      )}
      {list.hasNextPage && (
        <div ref={end} className="border-t border-line p-3 text-center">
          <Button
            size="sm"
            variant="ghost"
            loading={list.isFetchingNextPage}
            onClick={() => list.fetchNextPage()}
          >
            {list.isFetchingNextPage
              ? 'Loading'
              : list.isFetchNextPageError
                ? 'Couldn’t load more: try again'
                : 'Load more'}
          </Button>
        </div>
      )}
    </Section>
  )
}

function SummaryCard({
  s,
  showProject,
  busy,
  onReview,
}: {
  s: TaskSummary
  showProject: boolean
  busy?: boolean
  onReview: (unread: boolean) => void
}) {
  const [open, setOpen] = useState(false)
  const html = useMemo(() => renderMarkdown(s.body, { untrusted: true }), [s.body])
  const long = s.body.length > CLAMP_CHARS
  const read = !!s.reviewed_at
  return (
    <li
      data-testid="task-summary"
      data-read={read}
      className={clsx('relative px-4 py-3', read && 'bg-sunken/30')}
    >
      {!read && (
        <span aria-hidden className="absolute inset-y-3 left-0 w-0.5 rounded-r bg-accent" />
      )}
      <header className="flex items-start gap-x-2">
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-1.5 text-[13px]">
            {s.issue ? (
              <>
                <TypeIcon type={s.issue.type} />
                <AppLink
                  to={`/issue/${s.issue.key}`}
                  className="shrink-0 font-mono text-xs text-muted hover:text-fg hover:underline"
                >
                  {s.issue.key}
                </AppLink>
                <span className={clsx('truncate', !read && 'font-medium')} title={s.issue.title}>
                  {s.issue.title}
                </span>
              </>
            ) : (
              <span className="text-muted">Task</span>
            )}
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-subtle">
            {showProject && s.project && <Badge>{s.project}</Badge>}
            {s.machine && <span>{s.machine}</span>}
            {s.agent && <span className="truncate">· {s.agent.replace(/^anthropic\//, '')}</span>}
            <span aria-hidden>·</span>
            <time dateTime={s.updated_at} title={new Date(s.updated_at).toLocaleString()}>
              {relativeTime(s.updated_at)}
            </time>
            {s.autopilot && (
              <Badge
                color="var(--wp-accent)"
                title="Autopilot moved the session on after this task"
                icon
              >
                <Plane className="size-3" />
              </Badge>
            )}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-0.5">
          <AppLink
            to={s.session_url}
            aria-label="Open the session"
            title="Open the session"
            className="inline-flex size-7 items-center justify-center rounded-md text-muted hover:bg-hover hover:text-fg"
          >
            <Terminal className="size-3.5" />
          </AppLink>
          <IconButton
            label={read ? 'Mark unread' : 'Mark read'}
            loading={busy}
            icon={read ? <Undo2 className="size-3.5" /> : <Check className="size-3.5" />}
            onClick={() => onReview(read)}
          />
        </div>
      </header>
      <div className={clsx('relative mt-2', long && !open && 'max-h-52 overflow-hidden')}>
        <div
          className="prose-wp text-[13px] [overflow-wrap:anywhere]"
          // sanitised by renderMarkdown
          dangerouslySetInnerHTML={{ __html: html }}
        />
        {long && !open && (
          <div className="pointer-events-none absolute inset-x-0 bottom-0 h-12 bg-gradient-to-t from-panel to-transparent" />
        )}
      </div>
      {long && (
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="mt-1 text-xs text-muted hover:text-fg hover:underline"
        >
          {open ? 'Show less' : 'Show all'}
        </button>
      )}
      {s.asks.length > 0 && (
        <div
          data-testid="summary-asks"
          className="mt-2.5 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-[13px]"
        >
          <div className="flex items-center gap-1.5 font-medium">
            <MessageCircleQuestion className="size-3.5 text-warn" />
            Possibly waiting on you
          </div>
          <ul className="mt-1 list-disc space-y-0.5 pl-5 text-muted">
            {s.asks.map((a) => (
              <li key={a}>{a}</li>
            ))}
          </ul>
          {s.request_url && (
            <AppLink
              to={s.request_url}
              className="mt-1.5 inline-flex items-center gap-1 text-xs font-medium text-fg hover:underline"
            >
              Filed as request {s.request}
              <ExternalLink className="size-3" />
            </AppLink>
          )}
        </div>
      )}
    </li>
  )
}
