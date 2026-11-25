import { toastError } from '@/components/ui/toast'

type EditorModule = typeof import('./module')

let loaded: EditorModule | null = null
let loading: Promise<EditorModule> | null = null

/** Starts loading the editor (once) and resolves with it. */
export function loadEditor(): Promise<EditorModule> {
  // A failed load is forgotten, so the next try fetches the chunk again.
  loading ??= import('./module').then(
    (m) => (loaded = m),
    (err) => {
      loading = null
      throw err
    },
  )
  return loading
}

const EDITOR_FAILED = "The formatted editor didn't load; editing as markdown"

/** loadEditor, but a failure is a toast and null: the caller edits as markdown. */
let toldAt = 0
export async function tryLoadEditor(): Promise<EditorModule | null> {
  try {
    return await loadEditor()
  } catch {
    // Several boxes on a page fail together: one toast.
    if (Date.now() - toldAt > 6000) toastError(new Error(EDITOR_FAILED))
    toldAt = Date.now()
    return null
  }
}

/** The editor if it has loaded already. */
export const loadedEditor = () => loaded

/** Starts loading when the browser is idle, so the click that opens an editor rarely waits. */
export function preloadEditor() {
  if (loaded || loading) return
  const w = window as Window & { requestIdleCallback?: (cb: () => void) => number }
  if (w.requestIdleCallback) w.requestIdleCallback(() => void loadEditor().catch(() => {}))
  else setTimeout(() => void loadEditor().catch(() => {}), 200)
}
