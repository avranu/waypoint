import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Copy, Download } from 'lucide-react'
import { downloadURL, fileSize, PREVIEW_BYTES } from '@/lib/attachments'
import { copyText } from '@/lib/clipboard'
import { renderMarkdown } from '@/lib/markdown'
import type { Attachment } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

/** The start of a text file (files never change, so it's fetched once). */
function useFileText(f: Attachment | null) {
  return useQuery({
    queryKey: ['attachmentText', f?.id],
    enabled: !!f,
    staleTime: Infinity,
    queryFn: async () => {
      // The server answers ranges, so a big log costs only what's shown.
      const res = await fetch(f!.url, { headers: { Range: `bytes=0-${PREVIEW_BYTES - 1}` } })
      if (!res.ok) throw new Error('Could not load the file')
      return res.text()
    },
  })
}

/** A dialog that shows a text file: rendered for Markdown, monospace for the rest. */
export function TextPreview({ file, onClose }: { file: Attachment | null; onClose: () => void }) {
  const text = useFileText(file)
  const truncated = !!file && file.size > PREVIEW_BYTES
  const shown = useMemo(() => {
    const t = text.data
    if (t === undefined || !file) return ''
    if (file.preview === 'json' && !truncated) {
      try {
        return JSON.stringify(JSON.parse(t), null, 2)
      } catch {
        return t
      }
    }
    return t
  }, [text.data, file, truncated])
  const html = useMemo(
    () => (file?.preview === 'markdown' && text.data !== undefined ? renderMarkdown(shown) : ''),
    [file?.preview, text.data, shown],
  )
  return (
    <Dialog
      open={!!file}
      onClose={onClose}
      wide
      title={<span className="block max-w-[60vw] truncate">{file?.name}</span>}
      footer={
        file && (
          <>
            <span className="mr-auto text-xs text-subtle">
              {fileSize(file.size)}
              {truncated && ` · showing the first ${fileSize(PREVIEW_BYTES)}`}
            </span>
            <Button
              size="sm"
              icon={<Copy className="size-3.5" />}
              disabled={text.data === undefined}
              onClick={() => copyText(shown).then(() => toast('Copied'), toastError)}
            >
              Copy
            </Button>
            <a
              href={downloadURL(file)}
              className="inline-flex h-7 items-center gap-1.5 rounded-md border border-line bg-panel px-2.5 text-[13px] font-medium shadow-card hover:bg-hover"
            >
              <Download className="size-3.5" /> Download
            </a>
          </>
        )
      }
    >
      <div className="p-4" data-preview={file?.preview}>
        {text.isPending ? (
          <Skeleton className="h-40" />
        ) : text.isError ? (
          <p className="text-sm text-danger">Could not load the file. Download it instead.</p>
        ) : file?.preview === 'markdown' ? (
          <div
            className="prose-wp text-sm leading-relaxed"
            dangerouslySetInnerHTML={{ __html: html }}
          />
        ) : (
          <pre className="max-h-[60vh] overflow-auto whitespace-pre rounded-lg bg-sunken p-3 font-mono text-xs leading-relaxed">
            {shown}
          </pre>
        )}
      </div>
    </Dialog>
  )
}
