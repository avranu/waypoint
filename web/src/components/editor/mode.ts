// The editor a text opens in is remembered (WP-628).
const MODE_KEY = 'wp.editor.mode'
// A choice whose write failed, with what storage held then: it applies only
// while storage still holds that, so another tab's later choice shows.
let chosen: { mode: 'rich' | 'md'; stored: string | null } | null = null
const stored = (): string | null => {
  try {
    return localStorage.getItem(MODE_KEY)
  } catch {
    return null
  }
}
export const readMode = (): 'rich' | 'md' => {
  const now = stored()
  if (chosen && chosen.stored === now) return chosen.mode
  chosen = null
  return now === 'md' ? 'md' : 'rich'
}
export const writeMode = (m: 'rich' | 'md') => {
  try {
    localStorage.setItem(MODE_KEY, m)
    chosen = null
  } catch {
    chosen = { mode: m, stored: stored() }
  }
}
