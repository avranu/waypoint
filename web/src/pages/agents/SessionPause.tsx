import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { CirclePause, CirclePlay } from 'lucide-react'
import { pausesQuery, usePauseAgents, useResumeAgents, type AgentPause } from '@/lib/queue'
import { relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Popover, usePopover } from '@/components/ui/popover'
import { toast } from '@/components/ui/toast'

/** Your pauses that include running sessions. */
export function useSessionPauses(): AgentPause[] | undefined {
  return useQuery(pausesQuery()).data?.filter((p) => p.sessions)
}

const covers = (p: AgentPause) => (p.spec === 'any' ? 'Your agents' : `Agents matching “${p.spec}”`)

/** A banner per pause that includes running sessions, with Resume. */
export function SessionPauseBanners({ pauses }: { pauses: AgentPause[] }) {
  const resume = useResumeAgents()
  if (!pauses.length) return null
  return (
    <div className="space-y-2">
      {pauses.map((p) => (
        <div
          key={p.id}
          role="status"
          className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-warn/40 bg-warn/10 px-3 py-2.5"
        >
          <CirclePause className="size-4 shrink-0 text-warn" />
          <div className="min-w-0 flex-1 text-[13px]">
            <span className="font-medium">{covers(p)} are paused, running sessions included</span>
            <span className="text-muted">
              {' '}
              {p.by ? `by ${p.by.display_name || p.by.username} ` : ''}
              {relativeTime(p.created_at)}
              {p.note ? `: ${p.note}` : ''}
            </span>
            <div className="text-xs text-muted">
              Sessions finish their step, write a hand-off and stop. Nothing new starts, and
              Waypoint doesn't wake them until you resume.
            </div>
          </div>
          <Button
            size="sm"
            icon={<CirclePlay className="size-3.5" />}
            loading={resume.isPending}
            onClick={() =>
              resume.mutate(p.spec, { onSuccess: () => toast(`${covers(p)} can work again`) })
            }
          >
            Resume
          </Button>
        </div>
      ))}
    </div>
  )
}

/** The Pause button: confirms in a popover, with an optional note agents see. */
export function PauseSessionsButton({ label = 'Pause sessions' }: { label?: string }) {
  const pop = usePopover()
  const pause = usePauseAgents()
  const [note, setNote] = useState('')
  return (
    <>
      <Button
        ref={pop.anchor}
        size="sm"
        icon={<CirclePause className="size-3.5" />}
        onClick={pop.toggle}
      >
        {label}
      </Button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end" width={320}>
        <form
          className="space-y-2.5 p-2"
          onSubmit={(e) => {
            e.preventDefault()
            pause.mutate(
              { spec: 'any', note, sessions: true },
              {
                onSuccess: () => {
                  toast('Agents paused, running sessions included')
                  setNote('')
                  pop.close()
                },
              },
            )
          }}
        >
          <div className="text-[13px] font-medium">Pause all your agents and sessions?</div>
          <p className="text-xs text-muted">
            Running sessions start no agents, finish the step they're on, write a hand-off and stop.
            No new work starts until you resume.
          </p>
          <Input
            autoFocus
            value={note}
            maxLength={300}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Why (optional, agents see it)"
            aria-label="Why"
          />
          <div className="flex justify-end gap-2">
            <Button type="button" size="sm" variant="ghost" onClick={pop.close}>
              Cancel
            </Button>
            <Button type="submit" size="sm" variant="primary" loading={pause.isPending}>
              Pause
            </Button>
          </div>
        </form>
      </Popover>
    </>
  )
}
