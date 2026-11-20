// Holds (WP-269): a task no one takes, from wp next or by name, until a
// date or until someone lifts the hold.

import { useEffect, useState } from 'react'
import { Hourglass } from 'lucide-react'
import clsx from 'clsx'
import type { Hold } from '@/lib/types'
import { useHoldIssue } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { dateTime, localISO, shortDate, todayISO } from '@/lib/format'
import { Badge } from '@/components/ui/misc'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { toast, toastError } from '@/components/ui/toast'

/** "until Nov 3", "until Nov 3 and WP-1 is done", "until R-146 is answered", or "until someone lifts it". */
export function holdSpan(h: Hold): string {
  if (!h.until) return h.waits ? 'until ' + h.waits : 'until someone lifts it'
  const d = new Date(h.until)
  const date = d.getHours() || d.getMinutes() ? dateTime(h.until) : shortDate(h.until)
  return 'until ' + date + (h.waits ? ' and ' + h.waits : '')
}

/** The "On hold" marker on rows and cards. */
export function HoldBadge({ hold, className }: { hold: Hold; className?: string }) {
  return (
    <span
      title={`On hold ${holdSpan(hold)}${hold.note ? `: ${hold.note}` : ''}. No one takes it until then.`}
      className={clsx('shrink-0', className)}
    >
      <Badge color="var(--wp-warn)">
        <Hourglass className="size-3" aria-hidden />
        On hold
      </Badge>
    </span>
  )
}

/** Says what holds the task, with a way to lift it. */
export function HoldBanner({
  issueKey,
  hold,
  editable,
  className,
}: {
  issueKey: string
  hold: Hold
  editable: boolean
  className?: string
}) {
  const lift = useHoldIssue()
  const by = hold.agent ? agentLabel(hold.agent) : hold.by
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
      <Hourglass className="size-4 shrink-0" style={{ color: 'var(--wp-warn)' }} aria-hidden />
      <span className="min-w-0 flex-1">
        <span className="font-medium">On hold {holdSpan(hold)}.</span>{' '}
        {hold.note && <span>{hold.note}. </span>}
        <span className="text-muted">
          No one takes it, from wp next or by name, until then{by ? ` · held by ${by}` : ''}.
        </span>
      </span>
      {editable && (
        <Button
          size="sm"
          loading={lift.isPending}
          onClick={() =>
            lift.mutate(
              { key: issueKey, hold: null },
              { onSuccess: () => toast(`${issueKey} is off hold`), onError: toastError },
            )
          }
        >
          Lift hold
        </Button>
      )}
    </div>
  )
}

/** Puts a task on hold, or changes its hold: until a day, or until lifted. */
export function HoldDialog({
  open,
  onClose,
  issueKey,
  hold,
}: {
  open: boolean
  onClose: () => void
  issueKey: string
  hold?: Hold
}) {
  const save = useHoldIssue()
  const [until, setUntil] = useState('')
  const [note, setNote] = useState('')
  useEffect(() => {
    if (!open) return
    setUntil(hold?.until ? localISO(new Date(hold.until)) : '')
    setNote(hold?.note ?? '')
  }, [open, hold])
  const submit = () =>
    save.mutate(
      { key: issueKey, hold: { until, note, keep_waits: !!hold?.waits } },
      {
        onSuccess: (d) => {
          toast(`${issueKey} is on hold ${d.hold ? holdSpan(d.hold) : ''}`)
          onClose()
        },
        onError: toastError,
      },
    )
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={hold ? `Change the hold on ${issueKey}` : `Put ${issueKey} on hold`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} loading={save.isPending}>
            {hold ? 'Save' : 'Put on hold'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          No one takes it, from wp next or by name, until the hold ends. An agent working on it lets
          it go.
        </p>
        {hold?.waits && (
          <p className="text-[13px]">
            It also waits until {hold.waits}. Lift the hold to drop that.
          </p>
        )}
        <Field
          label="Until"
          hint={
            hold?.waits
              ? 'Blank: no date. It ends at the start of that day.'
              : 'Blank: until someone lifts it. It ends at the start of that day.'
          }
        >
          <Input
            type="date"
            min={todayISO(1)}
            value={until}
            onChange={(e) => setUntil(e.target.value)}
          />
        </Field>
        <Field label="Why it waits">
          <Input
            autoFocus
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="After the model’s quota refreshes"
          />
        </Field>
      </form>
    </Dialog>
  )
}
