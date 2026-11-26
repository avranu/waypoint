import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, Plus } from 'lucide-react'
import { q } from '@/lib/queries'
import { localISO } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { PageHeader, Skeleton } from '@/components/ui/misc'
import { TimerBar } from './time/TimerBar'
import { EntryList } from './time/Entries'
import { AddTimeDialog } from './time/AddTime'
import { AgentTimePanel, SummaryPanel, WeekStrip } from './time/Summary'
import { weekLabel, weekRange } from './time/util'

export function TimePage() {
  const me = useQuery(q.me()).data
  const weekStart = me?.prefs.week_start ?? 1
  const [offset, setOffset] = useState(0)
  const [adding, setAdding] = useState(false)
  useEffect(() => {
    document.title = 'Time · Waypoint'
  }, [])

  const { from, fromISO, toISO } = useMemo(() => weekRange(weekStart, offset), [weekStart, offset])

  const timer = useQuery(q.timer()).data
  const entries = useQuery({ ...q.time(fromISO, toISO), placeholderData: (p) => p })
  const report = useQuery({
    ...q.timeReport(fromISO, toISO),
    placeholderData: (p) => p,
    refetchInterval: timer ? 60_000 : false,
  })

  const list = entries.data ?? []
  const thisWeek = offset === 0
  const defaultDate = thisWeek ? undefined : localISO(from)

  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-5xl">
        <PageHeader
          title="Time"
          actions={
            <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setAdding(true)}>
              Add time
            </Button>
          }
        />
        <div className="space-y-4 px-4 sm:px-6">
          <TimerBar timer={timer} />
          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-1">
              <IconButton
                label="Previous week"
                onClick={() => setOffset(offset - 1)}
                icon={<ChevronLeft className="size-4" />}
              />
              <IconButton
                label="Next week"
                onClick={() => setOffset(offset + 1)}
                disabled={offset >= 0}
                icon={<ChevronRight className="size-4" />}
              />
              <h2 className="ml-1 text-[15px] font-semibold">{weekLabel(from)}</h2>
            </div>
            {!thisWeek && (
              <Button size="sm" variant="ghost" onClick={() => setOffset(0)}>
                This week
              </Button>
            )}
          </div>
          <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_300px]">
            <div className="min-w-0 space-y-4">
              <WeekStrip
                from={from}
                report={report.data}
                onPickDay={(k) =>
                  document
                    .getElementById(`day-${k}`)
                    ?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                }
              />
              {entries.isPending ? (
                <div className="space-y-2">
                  <Skeleton className="h-12" />
                  <Skeleton className="h-12" />
                  <Skeleton className="h-12" />
                </div>
              ) : (
                <>
                  {list.length >= 500 && (
                    <p className="text-xs text-subtle">
                      Showing the latest 500 entries of the week; the day totals below count only
                      these.
                    </p>
                  )}
                  <EntryList entries={list} />
                </>
              )}
            </div>
            <aside className="min-w-0 space-y-4">
              <SummaryPanel report={report.data} />
              <AgentTimePanel report={report.data} />
            </aside>
          </div>
        </div>
      </div>
      <AddTimeDialog open={adding} onClose={() => setAdding(false)} defaultDate={defaultDate} />
    </div>
  )
}
