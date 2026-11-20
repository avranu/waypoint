// The chip that says where a session's adversarial review is (WP-752,
// WP-761): blue with a running clock while it is under way, solid once
// something is really working on it, amber when it went wrong.

import { clock } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { reviewFaulted, reviewLive, reviewWorking, type SessionReview } from '@/lib/sessions'

function ReviewClock({ since }: { since: string }) {
  useTick(1000)
  return (
    <span className="tabular-nums" data-testid="session-review-clock">
      {clock((Date.now() - new Date(since).getTime()) / 1000)}
    </span>
  )
}

function chipColor(r: SessionReview): string {
  if (reviewFaulted(r)) return 'bg-warn/15 text-warn'
  if (reviewWorking(r)) return 'bg-info text-info-fg'
  if (reviewLive(r)) return 'bg-info/15 text-info'
  return 'bg-sunken text-muted'
}

/** withTask: the session page's longer form, with the task and the detail beside it. */
export function ReviewChip({
  review,
  title,
  withTask,
}: {
  review: SessionReview
  title?: string
  withTask?: boolean
}) {
  const chip = (
    <span
      className={`inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 text-[11px] font-medium whitespace-nowrap ${chipColor(review)}`}
    >
      {review.label}
      {withTask && ` on ${review.task}`}
      {reviewLive(review) && <ReviewClock since={review.since} />}
    </span>
  )
  if (!withTask)
    return (
      <span title={title} data-testid="session-review">
        {chip}
      </span>
    )
  return (
    <span
      className="inline-flex flex-wrap items-center gap-x-1.5 text-xs"
      data-testid="session-review"
    >
      {chip}
      {review.detail && (
        <span className={reviewFaulted(review) ? 'text-warn' : 'text-muted'}>{review.detail}</span>
      )}
    </span>
  )
}
