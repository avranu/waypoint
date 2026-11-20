// Drafts (WP-42): issues and pages a person is still thinking through.
// Agents can't see them anywhere until a person publishes them.

import { Globe, PenLine } from 'lucide-react'
import clsx from 'clsx'
import { Badge } from '@/components/ui/misc'
import { Button } from '@/components/ui/button'

const HINT = "A draft: agents can't see it, anywhere, until you publish it"
const OUTSIDE_HINT =
  "From outside: someone who isn't a Waypoint person wrote it on a forge. Agents can't see it until a person approves it"

/** The "Draft" (or "Outside", WP-279) marker on rows, cards and tree items. */
export function DraftBadge({ className, outside }: { className?: string; outside?: boolean }) {
  return (
    <span title={outside ? OUTSIDE_HINT : HINT} className={clsx('shrink-0', className)}>
      <Badge color="var(--wp-warn)">
        {outside ? (
          <Globe className="size-3" aria-hidden />
        ) : (
          <PenLine className="size-3" aria-hidden />
        )}
        {outside ? 'Outside' : 'Draft'}
      </Badge>
    </span>
  )
}

/** Says what a draft is, with the person's way to publish it. */
export function DraftBanner({
  what,
  onPublish,
  publishing,
  outside,
  className,
}: {
  what: 'issue' | 'page'
  /** absent when the viewer can't edit it */
  onPublish?: () => void
  publishing?: boolean
  /** it's a draft because a forge user who isn't a Waypoint person wrote it (WP-279) */
  outside?: { by?: string; forge?: string }
  className?: string
}) {
  return (
    <div
      role="note"
      className={clsx(
        'flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-dashed px-3 py-2 text-[13px]',
        className,
      )}
      style={{
        borderColor: 'color-mix(in oklab, var(--wp-warn) 55%, transparent)',
        background: 'color-mix(in oklab, var(--wp-warn) 8%, transparent)',
      }}
    >
      {outside ? (
        <Globe className="size-4 shrink-0" style={{ color: 'var(--wp-warn)' }} aria-hidden />
      ) : (
        <PenLine className="size-4 shrink-0" style={{ color: 'var(--wp-warn)' }} aria-hidden />
      )}
      <span className="min-w-0 flex-1">
        {outside ? (
          <>
            <span className="font-medium">From outside.</span>{' '}
            <span className="text-muted">
              {outside.by ? <span className="font-mono">{outside.by}</span> : 'Someone'}
              {outside.forge ? ` wrote it on ${outside.forge}` : ' wrote it on a forge'}, and isn't
              a Waypoint person. No agent or model sees it until you've read it and approve it.
            </span>
          </>
        ) : (
          <>
            <span className="font-medium">Draft.</span>{' '}
            <span className="text-muted">
              Agents can't see this {what}
              {what === 'issue' ? ' or its sub-issues' : ' or its sub-pages'} until you publish it.
            </span>
          </>
        )}
      </span>
      {onPublish && (
        <Button size="sm" variant="primary" loading={publishing} onClick={onPublish}>
          {outside ? 'Approve' : 'Publish'}
        </Button>
      )}
    </div>
  )
}
