import { useQueryClient } from '@tanstack/react-query'

/** Local midnight at the start of the week containing d. */
export function startOfWeek(d: Date, weekStart: number): Date {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  const diff = (x.getDay() - weekStart + 7) % 7
  x.setDate(x.getDate() - diff)
  return x
}

/** The week shown on the Time page (offset in weeks from this one), as RFC 3339 bounds. */
export function weekRange(weekStart: number, offset: number) {
  const from = addDays(startOfWeek(new Date(), weekStart), offset * 7)
  return { from, fromISO: rfc3339(from), toISO: rfc3339(addDays(from, 7)) }
}

export function addDays(d: Date, n: number): Date {
  const x = new Date(d)
  x.setDate(x.getDate() + n)
  return x
}

/** RFC 3339 without milliseconds, in UTC. */
export function rfc3339(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

/** "9:05 AM" in the user's locale. */
export function timeOfDay(iso: string | Date): string {
  return new Date(iso).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
}

/** "HH:MM" for <input type=time>. */
export function hhmm(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}`
}

/** Parse "1h30m", "45m", "1:30", "1.5h", "90" (minutes) to seconds; null if unreadable. */
export function parseDuration(v: string): number | null {
  const s = v.toLowerCase().replace(/\s+/g, '')
  if (!s) return null
  let m = /^(\d+):(\d{1,2})$/.exec(s)
  if (m) return Number(m[1]) * 3600 + Number(m[2]) * 60
  if (/^\d+(\.\d+)?$/.test(s)) return Math.round(Number(s) * 60)
  m = /^(?:(\d+(?:\.\d+)?)h)?(?:(\d+(?:\.\d+)?)m)?(?:(\d+)s)?$/.exec(s)
  if (m && (m[1] || m[2] || m[3]))
    return Math.round(Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0))
  return null
}

/** Seconds to an editable "1h30m". */
export function durationInput(seconds: number): string {
  const mins = Math.max(1, Math.round(seconds / 60))
  const h = Math.floor(mins / 60)
  const m = mins % 60
  return h ? `${h}h${m ? m + 'm' : ''}` : `${m}m`
}

/** Header label for a day group. */
export function dayHeading(d: Date): string {
  const today = new Date()
  const yest = addDays(today, -1)
  if (d.toDateString() === today.toDateString()) return 'Today'
  if (d.toDateString() === yest.toDateString()) return 'Yesterday'
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' })
}

export function weekLabel(from: Date): string {
  const to = addDays(from, 6)
  const sameYear =
    from.getFullYear() === new Date().getFullYear() && to.getFullYear() === from.getFullYear()
  const fmt = (d: Date, year: boolean) =>
    d.toLocaleDateString(undefined, {
      month: 'short',
      day: 'numeric',
      ...(year ? { year: 'numeric' } : {}),
    })
  return `${fmt(from, false)} – ${fmt(to, !sameYear)}`
}

/** Refetch everything that shows tracked time. */
export function useInvalidateTime() {
  const qc = useQueryClient()
  return () => {
    // 'issue' too: the issue page shows total time spent.
    for (const k of ['time', 'timer', 'timeReport', 'issueTime', 'issue'])
      qc.invalidateQueries({ queryKey: [k] })
  }
}
