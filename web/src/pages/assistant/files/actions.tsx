import { useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Copy, Download, FolderOpen, Trash2 } from 'lucide-react'
import { hermes } from '@/lib/assistant'
import { copyText } from '@/lib/clipboard'
import { Confirm } from '@/components/ui/dialog'
import type { MenuItem } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { downloadURL, listingsKey, type FileEntry } from './model'

/** Starts a download the way a link would, for menus. */
export function download(id: number, path: string) {
  const a = document.createElement('a')
  a.href = downloadURL(id, path)
  a.download = ''
  document.body.appendChild(a)
  a.click()
  a.remove()
}

export function copyPath(path: string) {
  copyText(path).then(() => toast('Path copied'), toastError)
}

export function useFileActions(id: number, onDeleted: (e: FileEntry) => void) {
  const qc = useQueryClient()
  const [doomed, setDoomed] = useState<FileEntry | null>(null)
  const remove = async (e: FileEntry) => {
    try {
      await hermes(id, 'DELETE', 'files', { path: e.path, recursive: e.is_directory })
      toast(`Deleted ${e.name}`)
      onDeleted(e)
    } catch (err) {
      toastError(err)
    } finally {
      qc.invalidateQueries({ queryKey: listingsKey(id) })
    }
  }
  const items = (e: FileEntry, open?: () => void): MenuItem[] => [
    ...(open && e.is_directory ? [{ label: 'Open', icon: <FolderOpen />, onSelect: open }] : []),
    ...(!e.is_directory && !e.broken_link
      ? [{ label: 'Download', icon: <Download />, onSelect: () => download(id, e.path) }]
      : []),
    { label: 'Copy path', icon: <Copy />, onSelect: () => copyPath(e.path) },
    { separator: true, label: '' },
    { label: 'Delete…', icon: <Trash2 />, danger: true, onSelect: () => setDoomed(e) },
  ]
  const dialog: ReactNode = (
    <Confirm
      open={!!doomed}
      onClose={() => setDoomed(null)}
      onConfirm={() => doomed && remove(doomed)}
      title={doomed?.is_directory ? 'Delete this folder?' : 'Delete this file?'}
      body={
        doomed && (
          <>
            <span className="break-all font-medium text-fg">{doomed.name}</span>
            {doomed.is_directory
              ? ' and everything in it will be deleted from the assistant.'
              : ' will be deleted from the assistant.'}{' '}
            This can't be undone.
          </>
        )
      }
    />
  )
  return { items, askDelete: setDoomed, dialog }
}
