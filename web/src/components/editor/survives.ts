import { generateHTML, generateJSON } from '@tiptap/core'
import { hasRawHtml, renderMarkdownForEdit } from '@/lib/markdown'
import { fromEditorHtml, toEditorHtml } from '@/lib/editor-html'
import { hasFrontMatter } from '@/lib/richtext'
import { schemaExtensions } from './schema'

/** A link reference or footnote definition, which renders as nothing and so would be dropped unseen. */
function hasDefinitions(md: string): boolean {
  let fence = ''
  for (const line of md.split('\n')) {
    const f = /^ {0,3}(`{3,}|~{3,})/.exec(line)
    if (f) {
      if (!fence) fence = f[1]
      else if (f[1][0] === fence[0] && f[1].length >= fence.length) fence = ''
      continue
    }
    if (!fence && /^ {0,3}\[[^\]]+\]:/.test(line)) return true
  }
  return false
}

const norm = (html: string) => html.replace(/\s+/g, ' ').trim()

/**
 * Whether markdown can be opened in the formatted editor without losing
 * anything. Editing re-spells untouched markdown into the editor's canonical
 * form (`__x__` becomes `**x**`, a `+` bullet `-`, and so on), so this checks
 * what the text renders as, not its bytes: it must render the same after a
 * round trip through the editor's schema. Raw HTML (comments included) fails
 * outright, since the sanitiser would drop it from both sides of that
 * comparison and the first edit would delete it; whatever else the schema
 * can't hold (footnotes, definition lists, an image that isn't on the web or
 * this site) fails by the comparison.
 */
export function survivesRichEdit(md: string, { blob = false } = {}): boolean {
  if (hasRawHtml(md) || hasFrontMatter(md) || hasDefinitions(md)) return false
  const extensions = schemaExtensions({ blob })
  const kept = generateHTML(generateJSON(toEditorHtml(md, { blob }), extensions), extensions)
  return (
    norm(renderMarkdownForEdit(fromEditorHtml(kept), { blob })) ===
    norm(renderMarkdownForEdit(md, { blob }))
  )
}
