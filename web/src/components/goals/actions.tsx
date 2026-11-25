import type { ReactNode } from 'react'
import { Check, Target, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Picker, Popover, usePopover } from '@/components/ui/popover'

export interface GoalOption {
  id: number
  title: string
}

/** ✓ Link / ✕ Drop, and `other` (a GoalPicker) after them. */
export function ChoiceButtons({
  onYes,
  onNo,
  yesLabel = 'Link',
  noLabel = 'Drop',
  yesHint,
  noHint,
  busy,
  other,
}: {
  onYes?: () => void
  onNo?: () => void
  yesLabel?: string
  noLabel?: string
  /** says what the check does, for a tooltip */
  yesHint?: string
  noHint?: string
  busy?: boolean
  other?: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {onYes && (
        <Button
          size="xs"
          variant="primary"
          icon={<Check className="size-3" />}
          disabled={busy}
          title={yesHint}
          onClick={onYes}
        >
          {yesLabel}
        </Button>
      )}
      {onNo && (
        <Button
          size="xs"
          variant="ghost"
          icon={<X className="size-3" />}
          disabled={busy}
          title={noHint}
          onClick={onNo}
        >
          {noLabel}
        </Button>
      )}
      {other}
    </div>
  )
}

/**
 * "Other goal…" next to a suggestion, or "Link to a goal…" when there is none:
 * pick any approved goal, or name a new one.
 */
export function GoalPicker({
  goals,
  label,
  onPick,
  onCreate,
  busy,
}: {
  goals: GoalOption[]
  label: 'Other goal…' | 'Link to a goal…'
  onPick: (goal: number) => void
  onCreate?: (title: string) => void
  busy?: boolean
}) {
  const pick = usePopover()
  return (
    <>
      <Button
        ref={pick.anchor}
        size="xs"
        variant="secondary"
        disabled={busy}
        onClick={pick.toggle}
        title={label === 'Other goal…' ? 'Link it to a different goal' : 'Pick the goal it serves'}
      >
        {label}
      </Button>
      <Popover open={pick.open} onClose={pick.close} anchor={pick.anchor} align="end" width={320}>
        <Picker
          items={goals.map((g) => ({
            value: g.id,
            label: g.title,
            icon: <Target className="size-3.5 text-subtle" />,
          }))}
          selected={[]}
          placeholder="Which goal does it serve?"
          onPick={(id) => {
            pick.close()
            if (id !== null) onPick(id)
          }}
          onCreate={
            onCreate
              ? (title) => {
                  pick.close()
                  onCreate(title)
                }
              : undefined
          }
          createLabel={(t) => `Add goal: “${t}”`}
          onClose={pick.close}
        />
      </Popover>
    </>
  )
}
