import { toastError } from '@/components/ui/toast'
import { uploadAttachment } from './attachments'

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** A markdown image or link whose address is `url` (written bare or in angle brackets). */
const tokenRe = (url: string) =>
  new RegExp(`!?\\[(?:\\\\.|[^\\]\\\\])*\\]\\(<?${escapeRe(url)}>?\\)`, 'g')

/** Markdown with every image and link that points at an object URL taken out. */
export const stripBlobTokens = (md: string) =>
  md.replace(/!?\[(?:\\.|[^\]\\])*\]\(<?blob:[^)\s>]*>?\)/g, '')

/**
 * Files added to text before anything exists to attach them to (a new issue):
 * each shows at an object URL, and once the target exists the ones the text
 * still points at are uploaded and their tokens swapped for the attachment's
 * markdown.
 */
export function deferredFiles() {
  const files = new Map<string, File>()
  const drop = (url: string) => {
    URL.revokeObjectURL(url)
    files.delete(url)
  }
  return {
    /** The address a file shows at; pass as an editor's deferUploads. */
    add(file: File) {
      const url = URL.createObjectURL(file)
      files.set(url, file)
      return url
    },
    // A file whose token leaves the text is kept until attach or clear: undo
    // can bring the token back, and its image must still show and upload.
    /** Revokes what is left, when the text is abandoned. */
    clear() {
      for (const url of [...files.keys()]) drop(url)
    },
    /**
     * Uploads the files `text` still points at to `target`; resolves with the
     * text, each token replaced by the attachment's markdown (a failed upload's
     * token goes, with a toast). Takes the files, so later calls find none.
     */
    async attach(target: string, text: string) {
      const todo = [...files]
      files.clear()
      let out = text
      for (const [url, file] of todo) {
        if (!out.includes(url)) continue
        try {
          const a = await uploadAttachment(target, file)
          out = out.replace(tokenRe(url), () => a.markdown)
        } catch (err) {
          toastError(err)
          out = out.replace(tokenRe(url), '')
        }
      }
      for (const [url] of todo) URL.revokeObjectURL(url)
      return out
    },
  }
}
