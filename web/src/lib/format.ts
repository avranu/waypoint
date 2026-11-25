const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

export function relativeTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const diff = (new Date(iso).getTime() - Date.now()) / 1000
  const abs = Math.abs(diff)
  if (abs < 45) return 'just now'
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour')
  if (abs < 86400 * 7) return rtf.format(Math.round(diff / 86400), 'day')
  return shortDate(iso)
}

/** "Oct 12" this year, "Oct 12, 2025" otherwise. Accepts YYYY-MM-DD or ISO. */
export function shortDate(d: string | null | undefined): string {
  if (!d) return ''
  const date = d.length === 10 ? new Date(d + 'T00:00:00') : new Date(d)
  const sameYear = date.getFullYear() === new Date().getFullYear()
  return date.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
  })
}

export function dateTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  })
}

/** "Nov 2, 2:05 PM" this year, "Nov 2, 2025, 2:05 PM" otherwise. */
export function dateTimeYear(iso: string): string {
  const date = new Date(iso)
  return date.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(date.getFullYear() === new Date().getFullYear() ? {} : { year: 'numeric' }),
    hour: 'numeric',
    minute: '2-digit',
  })
}

export function todayISO(offsetDays = 0): string {
  const d = new Date()
  d.setDate(d.getDate() + offsetDays)
  return localISO(d)
}

export function localISO(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export type DueState = 'overdue' | 'today' | 'soon' | 'later' | null

export function dueState(due: string | null, closed = false): DueState {
  if (!due || closed) return null
  const today = todayISO()
  if (due < today) return 'overdue'
  if (due === today) return 'today'
  if (due <= todayISO(3)) return 'soon'
  return 'later'
}

/** 1h 23m, 45m, 30s */
export function duration(seconds: number): string {
  if (seconds < 60) return `${Math.max(0, Math.floor(seconds))}s`
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (h === 0) return `${m}m`
  return m ? `${h}h ${m}m` : `${h}h`
}

/** 1:23:45 for running timers. */
export function clock(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const p = (n: number) => String(n).padStart(2, '0')
  return h ? `${h}:${p(m)}:${p(sec)}` : `${m}:${p(sec)}`
}

export function hours(seconds: number): string {
  const h = seconds / 3600
  return h >= 10 ? h.toFixed(0) : h.toFixed(1)
}

export function initials(name: string): string {
  const parts = name.trim().split(/\s+/)
  if (parts.length >= 2) return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
  return name.slice(0, 2).toUpperCase()
}

/** A stable pleasant color for a string. */
export function hashColor(s: string): string {
  let h = 0
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) | 0
  return `hsl(${Math.abs(h) % 360} 55% 50%)`
}

export function plural(n: number, word: string, pluralWord = word + 's'): string {
  return `${n} ${n === 1 ? word : pluralWord}`
}

/** Hover text for an issue in a list or on a card: its title and the start of its description (WP-67). */
export function issueHover(i: { title: string; excerpt?: string }): string | undefined {
  return i.excerpt ? `${i.title}\n\n${i.excerpt}` : undefined
}

/** Markdown read as plain text, for one-line previews: no **, `, # or list marks, links as their text. */
export function plainText(md: string): string {
  return md
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/(\*\*|__)(.+?)\1/g, '$2')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/^\s{0,3}(#{1,6}\s+|>\s?|[-*+]\s+(\[[ xX]\]\s+)?)/gm, '')
}

export function points(n: number | null | undefined): string {
  if (n === null || n === undefined) return ''
  return Number.isInteger(n) ? String(n) : n.toFixed(1)
}

/** The URL if it is http(s), else undefined: hrefs from stored data never run script. */
export function safeUrl(u: string | null | undefined): string | undefined {
  if (!u) return undefined
  try {
    const p = new URL(u, location.origin)
    return p.protocol === 'https:' || p.protocol === 'http:' ? p.href : undefined
  } catch {
    return undefined
  }
}

/** The last 30 days including today, as local-midnight ISO instants [from, to). */
export function last30Days(): [string, string] {
  const now = new Date()
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 29)
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
  return [start.toISOString(), end.toISOString()]
}
