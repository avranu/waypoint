import { useState } from 'react'
import clsx from 'clsx'
import { Sparkles } from 'lucide-react'
import { type Breakdown, useAITask, useBreakdown, useEstimate } from '@/lib/ai'
import { useCreateIssue } from '@/lib/queries'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Badge } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

const confidenceColor = { low: 'var(--wp-warn)', medium: 'var(--wp-accent)', high: 'var(--wp-ok)' }

/**
 * Inside the points picker: asks the estimate model for points and offers to apply them.
 * Hidden unless a model is set up. Lives in the picker so the points control stays one button.
 */
export function EstimatePanel({
  issueKey,
  onApply,
}: {
  issueKey: string
  onApply: (points: number) => unknown
}) {
  const enabled = useAITask('estimate')
  const est = useEstimate(issueKey)
  if (!enabled) return null
  const e = est.data
  return (
    <div className="mt-1.5 border-t border-line pt-1.5 text-[13px]" aria-live="polite">
      {est.isPending ? (
        <div className="flex h-8 items-center gap-2 px-1 text-subtle">
          <Spinner className="size-3.5" /> Thinking…
        </div>
      ) : e ? (
        <div className="space-y-2 px-1">
          <div className="flex items-center gap-2">
            <span className="text-2xl font-semibold tabular-nums">{e.points}</span>
            <span className="text-subtle">points</span>
            <Badge color={confidenceColor[e.confidence]} className="ml-auto">
              {e.confidence}
            </Badge>
          </div>
          <p className="text-muted">{e.reason}</p>
          <div className="truncate text-[11px] text-subtle" title={e.model}>
            {e.model}
          </div>
          <Button size="sm" variant="primary" className="w-full" onClick={() => onApply(e.points)}>
            Use {e.points}
          </Button>
        </div>
      ) : (
        <button
          onClick={() => est.mutate(undefined, { onError: toastError })}
          className="flex h-8 w-full items-center gap-2 rounded-lg px-1.5 text-muted hover:bg-hover hover:text-fg"
        >
          <Sparkles className="size-3.5" /> Estimate with AI
        </button>
      )}
    </div>
  )
}

/** Proposes sub-tasks; the person picks which to add. */
export function SuggestSubtasks({ issueKey, childType }: { issueKey: string; childType: string }) {
  const enabled = useAITask('breakdown')
  const suggest = useBreakdown(issueKey)
  const create = useCreateIssue()
  const [open, setOpen] = useState(false)
  const [picked, setPicked] = useState<boolean[]>([])
  const [adding, setAdding] = useState(false)
  if (!enabled) return null
  const run = () => {
    setOpen(true)
    suggest.mutate(undefined, {
      onSuccess: (b: Breakdown) => setPicked(b.subtasks.map(() => true)),
      onError: (e) => (setOpen(false), toastError(e)),
    })
  }
  const list = suggest.data?.subtasks ?? []
  const count = picked.filter(Boolean).length
  const add = async () => {
    setAdding(true)
    try {
      for (const [i, s] of list.entries()) {
        if (!picked[i]) continue
        await create.mutateAsync({
          type: childType,
          title: s.title,
          points: s.points,
          parent: issueKey,
        })
      }
      toast(`Added ${count} ${count === 1 ? 'sub-task' : 'sub-tasks'}`)
      setOpen(false)
    } catch (e) {
      toastError(e)
    } finally {
      setAdding(false)
    }
  }
  return (
    <>
      <IconButton
        label="Suggest sub-tasks"
        size="xs"
        onClick={run}
        disabled={suggest.isPending}
        icon={<Sparkles className="size-3.5" />}
      />
      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        title="Suggested sub-tasks"
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={!count || suggest.isPending}
              loading={adding}
              onClick={add}
            >
              Add {count || ''} {count === 1 ? 'sub-task' : 'sub-tasks'}
            </Button>
          </>
        }
      >
        <div className="p-4">
          {suggest.isPending ? (
            <div className="flex items-center gap-2 text-[13px] text-subtle">
              <Spinner className="size-3.5" /> Thinking…
            </div>
          ) : (
            <>
              <ul className="space-y-1">
                {list.map((s, i) => (
                  <li key={i}>
                    <label
                      className={clsx(
                        'flex cursor-pointer items-center gap-2.5 rounded-lg px-2 py-1.5 text-[13px] hover:bg-hover',
                        !picked[i] && 'text-subtle',
                      )}
                    >
                      <input
                        type="checkbox"
                        checked={!!picked[i]}
                        onChange={(e) =>
                          setPicked(picked.map((p, j) => (j === i ? e.target.checked : p)))
                        }
                        className="size-4 accent-[var(--wp-accent)]"
                      />
                      <span className="flex-1">{s.title}</span>
                      {s.points != null && <Badge>{s.points} pts</Badge>}
                    </label>
                  </li>
                ))}
              </ul>
              {suggest.data && (
                <div className="mt-3 truncate text-[11px] text-subtle">{suggest.data.model}</div>
              )}
            </>
          )}
        </div>
      </Dialog>
    </>
  )
}
