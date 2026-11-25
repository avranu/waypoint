import { api, enc } from './api'

export interface AutopilotText {
  prompt: string
  done: string
  default_prompt: string
  default_done: string
  /** worded where asked: the project's own, or the admins' for every project */
  custom_prompt: boolean
  custom_done: boolean
  /** a project's only: what applies without its own */
  inherited_prompt?: string
  inherited_done?: string
}

export const saveAutopilotText = (v: { prompt?: string; done?: string }) =>
  api.put<AutopilotText>('/admin/autopilot-text', v)

/** A project's own wording; "" falls back to every project's. */
export const saveProjectAutopilotText = (key: string, v: { prompt: string; done: string }) =>
  api.patch(`/projects/${enc(key)}`, {
    settings: { autopilot_prompt: v.prompt, autopilot_done: v.done },
  })

const EXAMPLE_NEXT = `Autopilot assigned WP-123 to you: "Fix the export button" by Sam

The export button does nothing on Safari.

Assess and complete the task as appropriate. When done, run \`wp ship WP-123 -m "what you did" --summary "your reply to the person"\`; if it ships cleanly, that reply ends your turn (add \`--check "CMD"\` to run your checks first, and \`--shot URL\` for each new frontend change). File questions with \`wp ask "question"\`, and if you need to give the task back, use \`wp release WP-123 -m "why"\`.`

/** The prompt as a session sees it, for an example task. */
export const renderAutopilotPrompt = (tmpl: string) =>
  tmpl
    .replaceAll('{key}', 'WP-123')
    .replaceAll('{title}', 'Fix the export button')
    .replaceAll('{done}', 'WP-122')
    .replaceAll('{next}', EXAMPLE_NEXT)
    .trim()

/** The note as a session sees it, for an example task. */
export const renderAutopilotDone = (tmpl: string) =>
  tmpl
    .replaceAll('{key}', 'WP-123')
    .replaceAll('{status}', 'The next task starts in about a minute.')
    .trim()

/** A prompt that never names its task leaves the session without one. */
export const promptNamesTask = (v: string) =>
  !v.trim() || v.includes('{key}') || v.includes('{next}')
