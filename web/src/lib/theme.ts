import { useSyncExternalStore } from 'react'

export type Theme = '' | 'light' | 'dark'

const media = () => matchMedia('(prefers-color-scheme: dark)')

export function storedTheme(): Theme {
  try {
    return (localStorage.getItem('wp:theme') as Theme) ?? ''
  } catch {
    return ''
  }
}

export function applyTheme(t: Theme) {
  const dark = t === 'dark' || (t === '' && media().matches)
  document.documentElement.classList.toggle('dark', dark)
  // Both media-specific tags, so an explicit choice wins over the system scheme.
  for (const m of document.querySelectorAll('meta[name=theme-color]'))
    m.setAttribute('content', dark ? '#15171b' : '#ffffff')
}

export function setTheme(t: Theme) {
  try {
    localStorage.setItem('wp:theme', t)
  } catch {}
  applyTheme(t)
}

export function watchSystemTheme() {
  media().addEventListener('change', () => applyTheme(storedTheme()))
}

/** Pick readable text for a background color. */
export function contrastText(hex: string): string {
  const m = hex.replace('#', '')
  const r = parseInt(m.slice(0, 2), 16)
  const g = parseInt(m.slice(2, 4), 16)
  const b = parseInt(m.slice(4, 6), 16)
  return (r * 299 + g * 587 + b * 114) / 1000 > 150 ? '#18181b' : '#ffffff'
}

/** Background presets for projects; the value is stored on the project. */
export const BACKGROUNDS: { id: string; label: string; css: string }[] = [
  { id: '', label: 'None', css: 'transparent' },
  {
    id: 'dawn',
    label: 'Dawn',
    css: 'linear-gradient(135deg, #fde68a55, #fca5a555 45%, #c4b5fd55)',
  },
  {
    id: 'ocean',
    label: 'Ocean',
    css: 'linear-gradient(135deg, #67e8f944, #3b82f644 50%, #6366f144)',
  },
  {
    id: 'forest',
    label: 'Forest',
    css: 'linear-gradient(135deg, #86efac44, #14b8a644 55%, #0f766e44)',
  },
  {
    id: 'dusk',
    label: 'Dusk',
    css: 'linear-gradient(135deg, #f472b644, #a855f744 50%, #312e8144)',
  },
  {
    id: 'sand',
    label: 'Sand',
    css: 'linear-gradient(160deg, #fef3c766, #fed7aa55 60%, #e7e5e466)',
  },
  {
    id: 'graphite',
    label: 'Graphite',
    css: 'linear-gradient(160deg, #94a3b833, #47556933 60%, #1e293b33)',
  },
  {
    id: 'grid',
    label: 'Grid',
    css: 'radial-gradient(circle at 1px 1px, color-mix(in oklab, var(--wp-fg) 14%, transparent) 1px, transparent 0) 0 0 / 18px 18px',
  },
]

// Unsaved look from the project settings form, so color and background
// changes preview live behind the form before they're saved.
type Look = { color: string; background: string }
let preview: { key: string; look: Look } | null = null
const lookListeners = new Set<() => void>()

export function setProjectPreview(key: string, look: Look | null) {
  if (look === null && preview?.key !== key) return
  preview = look ? { key, look } : null
  lookListeners.forEach((l) => l())
}

/** The project's look, with any unsaved preview from its settings form applied. */
export function useProjectLook(key: string, saved: Look): Look {
  const p = useSyncExternalStore(
    (cb) => {
      lookListeners.add(cb)
      return () => lookListeners.delete(cb)
    },
    () => preview,
  )
  return p?.key === key ? p.look : saved
}

export function backgroundCSS(bg: string): string {
  if (!bg) return 'transparent'
  const preset = BACKGROUNDS.find((b) => b.id === bg)
  if (preset) return preset.css
  if (/^https?:\/\//.test(bg)) return `url("${bg.replace(/"/g, '%22')}") center / cover fixed`
  if (/^#[0-9a-f]{6}$/i.test(bg)) return bg + '22'
  return 'transparent'
}

export const PROJECT_COLORS = [
  '#6366f1',
  '#8b5cf6',
  '#ec4899',
  '#ef4444',
  '#f97316',
  '#eab308',
  '#22c55e',
  '#14b8a6',
  '#0ea5e9',
  '#64748b',
]
