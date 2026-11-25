// The editor a text opens in is remembered (WP-628).
const MODE_KEY = 'wp.editor.mode'
export const readMode = (): 'rich' | 'md' => {
  try {
    return localStorage.getItem(MODE_KEY) === 'md' ? 'md' : 'rich'
  } catch {
    return 'rich'
  }
}
export const writeMode = (m: 'rich' | 'md') => {
  try {
    localStorage.setItem(MODE_KEY, m)
  } catch {}
}
