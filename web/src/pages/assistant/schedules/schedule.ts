// Schedule times are wall-clock in Hermes's own timezone, which need not be the browser's; parsing mirrors Hermes's parse_schedule.

/** Hermes's timezone: an IANA name from its config, else a fixed offset (minutes east of UTC) read off a job's timestamps, else the browser's. */
export interface Zone {
  iana?: string
  offset?: number
}

interface Wall {
  y: number
  mo: number
  d: number
  h: number
  mi: number
  dow: number
}

const WEEKDAY_SHORT = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

function wall(at: Date, z: Zone): Wall {
  if (z.iana) {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: z.iana,
      hourCycle: 'h23',
      year: 'numeric',
      month: 'numeric',
      day: 'numeric',
      hour: 'numeric',
      minute: 'numeric',
      weekday: 'short',
    }).formatToParts(at)
    const get = (t: string) => parts.find((p) => p.type === t)?.value ?? ''
    return {
      y: +get('year'),
      mo: +get('month'),
      d: +get('day'),
      h: +get('hour') % 24,
      mi: +get('minute'),
      dow: WEEKDAY_SHORT.indexOf(get('weekday')),
    }
  }
  if (z.offset !== undefined) {
    const s = new Date(at.getTime() + z.offset * 60_000)
    return {
      y: s.getUTCFullYear(),
      mo: s.getUTCMonth() + 1,
      d: s.getUTCDate(),
      h: s.getUTCHours(),
      mi: s.getUTCMinutes(),
      dow: s.getUTCDay(),
    }
  }
  return {
    y: at.getFullYear(),
    mo: at.getMonth() + 1,
    d: at.getDate(),
    h: at.getHours(),
    mi: at.getMinutes(),
    dow: at.getDay(),
  }
}

/** The instant a wall-clock time in the zone names. */
function fromWall(y: number, mo: number, d: number, h: number, mi: number, z: Zone): Date {
  if (z.offset !== undefined && !z.iana)
    return new Date(Date.UTC(y, mo - 1, d, h, mi) - z.offset * 60_000)
  if (!z.iana) return new Date(y, mo - 1, d, h, mi)
  // One correction can land on the wrong side of a DST edge; the second pass settles it.
  const want = Date.UTC(y, mo - 1, d, h, mi)
  let t = want
  for (let i = 0; i < 2; i++) {
    const w = wall(new Date(t), z)
    t += want - Date.UTC(w.y, w.mo - 1, w.d, w.h, w.mi)
  }
  return new Date(t)
}

/** Minutes east of UTC at that moment. */
function offsetAt(z: Zone, at = new Date()): number {
  if (z.offset !== undefined && !z.iana) return z.offset
  if (!z.iana) return -at.getTimezoneOffset()
  const minute = Math.floor(at.getTime() / 60_000) * 60_000
  const w = wall(new Date(minute), z)
  return Math.round((Date.UTC(w.y, w.mo - 1, w.d, w.h, w.mi) - minute) / 60_000)
}

/** The zone from Hermes's configured timezone or, failing that, a timestamp it wrote. */
export function hermesZone(
  configTz: string | undefined,
  stamps: (string | null | undefined)[],
): Zone {
  if (configTz) {
    try {
      new Intl.DateTimeFormat('en-US', { timeZone: configTz })
      return { iana: configTz }
    } catch {}
  }
  for (const s of stamps) {
    const m = s && /([+-])(\d{2}):?(\d{2})$/.exec(s)
    if (m) return { offset: (m[1] === '-' ? -1 : 1) * (+m[2] * 60 + +m[3]) }
    if (s?.endsWith('Z')) return { offset: 0 }
  }
  return {}
}

/** "UTC−4" or "America/New_York" when Hermes's clock differs from this browser's, else ''. */
export function zoneNote(z: Zone, at = new Date()): string {
  const off = offsetAt(z, at)
  if (off === -at.getTimezoneOffset()) return ''
  if (z.iana) return z.iana.replace(/_/g, ' ')
  const h = Math.floor(Math.abs(off) / 60)
  const m = Math.abs(off) % 60
  return `UTC${off < 0 ? '−' : '+'}${h}${m ? `:${String(m).padStart(2, '0')}` : ''}`
}

/** A time of day in this browser's style ("9:00 AM" or "09:00"). */
export function clockLabel(h: number, mi: number): string {
  return new Date(2000, 0, 1, h, mi).toLocaleTimeString(undefined, {
    hour: 'numeric',
    minute: '2-digit',
  })
}

/** An instant as Hermes's wall clock reads it, for the "once" picker: YYYY-MM-DDTHH:MM. */
export function wallInput(at: Date, z: Zone): string {
  const w = wall(at, z)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${w.y}-${p(w.mo)}-${p(w.d)}T${p(w.h)}:${p(w.mi)}`
}

/** An instant, written in Hermes's clock: "Thu, Oct 23, 9:00 AM". */
export function wallLabel(at: Date, z: Zone, withDay = true): string {
  const w = wall(at, z)
  const local = new Date(w.y, w.mo - 1, w.d, w.h, w.mi)
  const sameYear = w.y === new Date().getFullYear()
  return local.toLocaleString(undefined, {
    ...(withDay ? { weekday: 'short' } : {}),
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
    hour: 'numeric',
    minute: '2-digit',
  })
}

export type Parsed =
  | { kind: 'interval'; minutes: number }
  | { kind: 'once'; at: Date; inMinutes?: number }
  | { kind: 'cron'; expr: string }
  | { kind: 'error'; error: string }

const UNIT: Record<string, number> = { m: 1, h: 60, d: 1440 }

function parseDuration(s: string): number | null {
  const m = /^(\d*)\s*(m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days)$/.exec(
    s.trim().toLowerCase(),
  )
  if (!m) return null
  return (m[1] ? +m[1] : 1) * UNIT[m[2][0]]
}

const DOW: Record<string, string> = {
  sunday: '0',
  sun: '0',
  monday: '1',
  mon: '1',
  tuesday: '2',
  tue: '2',
  tues: '2',
  wednesday: '3',
  wed: '3',
  weds: '3',
  thursday: '4',
  thu: '4',
  thur: '4',
  thurs: '4',
  friday: '5',
  fri: '5',
  saturday: '6',
  sat: '6',
}
const DAYSPEC: Record<string, string> = {
  day: '*',
  daily: '*',
  everyday: '*',
  weekday: '1-5',
  weekdays: '1-5',
  weekend: '0,6',
  weekends: '0,6',
}

function parseClock(text: string): [number, number] | null {
  const t = text.trim().toLowerCase().replace(/ /g, '')
  if (!t) return null
  if (t === 'noon' || t === 'midday') return [12, 0]
  if (t === 'midnight') return [0, 0]
  const m = /^(\d{1,2})(?::(\d{2}))?(am|pm)?$/.exec(t)
  if (!m) return null
  let h = +m[1]
  const mi = +(m[2] ?? 0)
  if (m[3]) {
    if (h < 1 || h > 12) return null
    h = (h % 12) + (m[3] === 'pm' ? 12 : 0)
  }
  if (h > 23 || mi > 59) return null
  return [h, mi]
}

function naturalToCron(rest: string): string | null {
  const tokens = rest.toLowerCase().replace(/,/g, ' ').split(/\s+/).filter(Boolean)
  if (!tokens.length) return null
  let dow = DAYSPEC[tokens[0]]
  let idx = 1
  if (dow === undefined) {
    const days: string[] = []
    idx = tokens.length
    for (let i = 0; i < tokens.length; i++) {
      if (tokens[i] === 'and') continue
      const d = DOW[tokens[i]]
      if (d === undefined) {
        idx = i
        break
      }
      if (!days.includes(d)) days.push(d)
    }
    if (!days.length) return null
    dow = days.join(',')
  }
  let time = tokens.slice(idx)
  if (time[0] === 'at') time = time.slice(1)
  if (!time.length) return null
  const c = parseClock(time.join(' '))
  return c ? `${c[1]} ${c[0]} * * ${dow}` : null
}

/** What Hermes will make of a schedule string; `now` and the zone resolve one-shots. */
export function parseSchedule(input: string, z: Zone, now = new Date()): Parsed {
  const s = input.trim()
  if (!s) return { kind: 'error', error: 'Say when it runs' }
  const lower = s.toLowerCase()
  const every = lower.startsWith('every ')
  const rest = every ? s.slice(6).trim() : lower
  const cron = naturalToCron(rest)
  if (cron) return checkCron(cron)
  if (every) {
    const m = parseDuration(rest)
    return m
      ? { kind: 'interval', minutes: m }
      : { kind: 'error', error: `Hermes can't read “${s}”` }
  }
  const parts = s.split(/\s+/)
  if (parts.length >= 5 && parts.slice(0, 5).every((p) => /^[A-Za-z\d*\-,/]+$/.test(p)))
    return checkCron(s)
  if (s.includes('T') || /^\d{4}-\d{2}-\d{2}/.test(s)) {
    const m =
      /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?)?(Z|[+-]\d{2}:?\d{2})?$/.exec(
        s,
      )
    if (!m) return { kind: 'error', error: `“${s}” isn't a date and time` }
    let at: Date
    if (m[7]) at = new Date(s)
    else at = fromWall(+m[1], +m[2], +m[3], +(m[4] ?? 0), +(m[5] ?? 0), z)
    if (isNaN(at.getTime())) return { kind: 'error', error: `“${s}” isn't a date and time` }
    if (at.getTime() < now.getTime() - 120_000)
      return { kind: 'error', error: 'That time has passed' }
    return { kind: 'once', at }
  }
  if (lower.startsWith('in ')) {
    const m = parseDuration(s.slice(3))
    return m
      ? { kind: 'once', at: new Date(now.getTime() + m * 60_000), inMinutes: m }
      : { kind: 'error', error: 'Write it like “in 30m” or “in 2h”' }
  }
  const m = parseDuration(s)
  if (m) return { kind: 'interval', minutes: m }
  return { kind: 'error', error: `Hermes can't read “${s}”` }
}

const MONTH_NAMES = [
  'jan',
  'feb',
  'mar',
  'apr',
  'may',
  'jun',
  'jul',
  'aug',
  'sep',
  'oct',
  'nov',
  'dec',
]
const DOW_NAMES = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat']

interface CronField {
  values: Set<number>
  /** begins with *: matters for how day-of-month and day-of-week combine */
  star: boolean
}

function field(src: string, lo: number, hi: number, names?: string[], base = 0): CronField | null {
  const values = new Set<number>()
  const name = (t: string) => {
    const i = names?.indexOf(t.toLowerCase()) ?? -1
    return i >= 0 ? i + base : /^\d+$/.test(t) ? +t : NaN
  }
  for (const part of src.split(',')) {
    const [range, stepS] = part.split('/')
    const step = stepS === undefined ? 1 : /^\d+$/.test(stepS) ? +stepS : NaN
    if (!step) return null
    let a: number
    let b: number
    if (range === '*') [a, b] = [lo, hi]
    else if (range.includes('-')) {
      const [x, y] = range.split('-')
      ;[a, b] = [name(x), name(y)]
    } else {
      a = name(range)
      b = stepS === undefined ? a : hi
    }
    if (isNaN(a) || isNaN(b) || a < lo || b > hi || a > b) return null
    for (let v = a; v <= b; v += step) values.add(hi === 7 && v === 7 ? 0 : v)
  }
  return { values, star: src.startsWith('*') }
}

interface Cron {
  minute: CronField
  hour: CronField
  dom: CronField
  month: CronField
  dow: CronField
}

function parseCron(expr: string): Cron | null {
  const p = expr.trim().split(/\s+/)
  if (p.length !== 5) return null
  const minute = field(p[0], 0, 59)
  const hour = field(p[1], 0, 23)
  const dom = field(p[2], 1, 31)
  const month = field(p[3], 1, 12, MONTH_NAMES, 1)
  const dow = field(p[4], 0, 7, DOW_NAMES)
  if (!minute || !hour || !dom || !month || !dow) return null
  return { minute, hour, dom, month, dow }
}

function checkCron(expr: string): Parsed {
  const n = expr.trim().split(/\s+/).length
  if (n === 6) return { kind: 'cron', expr } // a seconds field: Hermes takes it; we only describe five
  if (!parseCron(expr))
    return { kind: 'error', error: `“${expr}” isn't a cron expression Hermes accepts` }
  return { kind: 'cron', expr }
}

/** The next `n` times a cron expression fires after `from`, in the zone. */
export function cronNext(expr: string, z: Zone, from = new Date(), n = 3): Date[] {
  const c = parseCron(expr)
  if (!c) return []
  const out: Date[] = []
  const start = wall(from, z)
  const hours = [...c.hour.values].sort((a, b) => a - b)
  const minutes = [...c.minute.values].sort((a, b) => a - b)
  // The search stops five years out, so a 29 February eight years away (2096 to 2104) can return fewer than n.
  const day = new Date(Date.UTC(start.y, start.mo - 1, start.d))
  for (let i = 0; i < 366 * 5 && out.length < n; i++, day.setUTCDate(day.getUTCDate() + 1)) {
    const y = day.getUTCFullYear()
    const mo = day.getUTCMonth() + 1
    const d = day.getUTCDate()
    if (!c.month.values.has(mo)) continue
    const domOk = c.dom.values.has(d)
    const dowOk = c.dow.values.has(day.getUTCDay())
    const dayOk = c.dom.star || c.dow.star ? domOk && dowOk : domOk || dowOk
    if (!dayOk) continue
    for (const h of hours) {
      for (const mi of minutes) {
        const at = fromWall(y, mo, d, h, mi, z)
        if (at.getTime() > from.getTime()) {
          out.push(at)
          if (out.length >= n) return out
        }
      }
    }
  }
  return out
}

const DAY_LONG = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
/** Monday first, as a week reads. */
export const WEEK = [1, 2, 3, 4, 5, 6, 0]

export function ordinal(n: number): string {
  const t = n % 100
  if (t >= 11 && t <= 13) return `${n}th`
  return `${n}${['th', 'st', 'nd', 'rd'][n % 10] ?? 'th'}`
}

export function everyLabel(minutes: number): string {
  if (minutes % 1440 === 0) return minutes === 1440 ? 'Every day' : `Every ${minutes / 1440} days`
  if (minutes % 60 === 0) return minutes === 60 ? 'Every hour' : `Every ${minutes / 60} hours`
  return minutes === 1 ? 'Every minute' : `Every ${minutes} minutes`
}

function daysLabel(days: number[]): string {
  const set = [...new Set(days)].sort((a, b) => WEEK.indexOf(a) - WEEK.indexOf(b))
  const key = set.join(',')
  if (key === '1,2,3,4,5') return 'Weekdays'
  if (key === '6,0') return 'Weekends'
  if (set.length === 7) return 'Every day'
  if (set.length === 1) return `Every ${DAY_LONG[set[0]]}`
  return set.map((d) => WEEKDAY_SHORT[d]).join(', ')
}

/** A cron expression in words, or null when it is too intricate to say briefly. */
export function cronLabel(expr: string): string | null {
  const c = parseCron(expr)
  if (!c) return null
  const p = expr.trim().split(/\s+/)
  const one = (f: CronField) => (f.values.size === 1 ? [...f.values][0] : null)
  const m = one(c.minute)
  const h = one(c.hour)
  const allDays = c.dom.star && c.dow.star && p[2] === '*' && p[4] === '*'
  const allMonths = p[3] === '*'
  if (allMonths && allDays) {
    const step = (s: string) => /^\*\/(\d+)$/.exec(s)?.[1]
    if (step(p[0]) && p[1] === '*') return everyLabel(+step(p[0])!)
    if (m !== null && step(p[1]))
      return `${everyLabel(+step(p[1])! * 60)}${m ? ` at :${String(m).padStart(2, '0')}` : ''}`
    if (m !== null && p[1] === '*')
      return m === 0 ? 'Every hour' : `Every hour at :${String(m).padStart(2, '0')}`
  }
  if (m === null || h === null || !allMonths) return null
  const at = clockLabel(h, m)
  if (allDays) return `Every day at ${at}`
  if (p[2] === '*' && !c.dow.star) return `${daysLabel([...c.dow.values])} at ${at}`
  if (p[4] === '*' && /^\d+$/.test(p[2])) return `Monthly on the ${ordinal(+p[2])} at ${at}`
  return null
}

/** What a parsed schedule means, in a short phrase. */
export function describe(p: Parsed, z: Zone): string {
  switch (p.kind) {
    case 'interval':
      return everyLabel(p.minutes)
    case 'once':
      return p.inMinutes !== undefined
        ? `Once, in ${everyLabel(p.inMinutes)
            .replace(/^Every hour$/, 'an hour')
            .replace(/^Every (day|minute)$/, 'a $1')
            .replace(/^Every /, '')}`
        : `Once, ${wallLabel(p.at, z)}`
    case 'cron':
      return cronLabel(p.expr) ?? `Cron: ${p.expr}`
    case 'error':
      return p.error
  }
}

/** When a parsed schedule next fires (for a new job: from now). */
export function upcoming(p: Parsed, z: Zone, now = new Date(), n = 3): Date[] {
  switch (p.kind) {
    case 'interval':
      return Array.from(
        { length: n },
        (_, i) => new Date(now.getTime() + (i + 1) * p.minutes * 60_000),
      )
    case 'once':
      return [p.at]
    case 'cron':
      return cronNext(p.expr, z, now, n)
    default:
      return []
  }
}

export type Mode = 'interval' | 'daily' | 'weekdays' | 'weekly' | 'monthly' | 'once' | 'custom'

export interface Builder {
  mode: Mode
  every: number
  unit: 'm' | 'h' | 'd'
  time: string
  days: number[]
  dom: number
  at: string
  raw: string
}

export function defaultBuilder(z: Zone, now = new Date()): Builder {
  const tomorrow = new Date(now.getTime() + 86_400_000)
  return {
    mode: 'daily',
    every: 30,
    unit: 'm',
    time: '09:00',
    days: [1],
    dom: 1,
    at: wallInput(tomorrow, z).slice(0, 11) + '09:00',
    raw: '',
  }
}

function hm(time: string): [number, number] | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(time)
  if (!m || +m[1] > 23 || +m[2] > 59) return null
  return [+m[1], +m[2]]
}

/** The string sent to Hermes, or '' while the builder is incomplete. */
export function buildSchedule(b: Builder): string {
  const t = hm(b.time)
  switch (b.mode) {
    case 'interval':
      return b.every >= 1 && Number.isInteger(b.every) ? `every ${b.every}${b.unit}` : ''
    case 'daily':
      return t ? `${t[1]} ${t[0]} * * *` : ''
    case 'weekdays':
      return t ? `${t[1]} ${t[0]} * * 1-5` : ''
    case 'weekly':
      return t && b.days.length
        ? `${t[1]} ${t[0]} * * ${[...b.days].sort((x, y) => x - y).join(',')}`
        : ''
    case 'monthly':
      return t && b.dom >= 1 && b.dom <= 31 ? `${t[1]} ${t[0]} ${b.dom} * *` : ''
    case 'once':
      return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(b.at) ? `${b.at}:00` : ''
    case 'custom':
      return b.raw.trim()
  }
}

export interface StoredSchedule {
  kind?: string
  expr?: string
  minutes?: number
  run_at?: string
  display?: string
}

/** The builder for a saved job's schedule. */
export function builderFrom(s: StoredSchedule | undefined, display: string, z: Zone): Builder {
  const b = defaultBuilder(z)
  if (!s) return { ...b, mode: 'custom', raw: display }
  if (s.kind === 'interval' && s.minutes) {
    const unit = s.minutes % 1440 === 0 ? 'd' : s.minutes % 60 === 0 ? 'h' : 'm'
    return { ...b, mode: 'interval', unit, every: s.minutes / UNIT[unit] }
  }
  if (s.kind === 'once' && s.run_at)
    return { ...b, mode: 'once', at: wallInput(new Date(s.run_at), z) }
  if (s.kind === 'cron' && s.expr) {
    const p = s.expr.trim().split(/\s+/)
    const time = (mi: string, h: string) =>
      /^\d+$/.test(mi) && /^\d+$/.test(h) ? `${h.padStart(2, '0')}:${mi.padStart(2, '0')}` : null
    const t = p.length === 5 && p[3] === '*' ? time(p[0], p[1]) : null
    if (t && p[2] === '*' && p[4] === '*') return { ...b, mode: 'daily', time: t }
    if (t && p[2] === '*' && p[4] === '1-5') return { ...b, mode: 'weekdays', time: t }
    if (t && p[2] === '*' && /^[0-7](,[0-7])*$/.test(p[4]))
      return {
        ...b,
        mode: 'weekly',
        time: t,
        days: [...new Set(p[4].split(',').map((d) => +d % 7))],
      }
    if (t && /^\d+$/.test(p[2]) && p[4] === '*')
      return { ...b, mode: 'monthly', time: t, dom: +p[2] }
    return { ...b, mode: 'custom', raw: s.display && s.display !== s.expr ? s.display : s.expr }
  }
  return { ...b, mode: 'custom', raw: s.display || display }
}

/** A saved job's schedule in words. */
export function storedLabel(s: StoredSchedule | undefined, display: string, z: Zone): string {
  if (s?.kind === 'interval' && s.minutes) return everyLabel(s.minutes)
  if (s?.kind === 'once' && s.run_at) return `Once, ${wallLabel(new Date(s.run_at), z)}`
  if (s?.kind === 'cron' && s.expr) return cronLabel(s.expr) ?? s.display ?? s.expr
  return display || s?.display || 'No schedule'
}
