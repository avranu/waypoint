// The assistant's files (WP-ADR1): what it keeps under its root, browsed
// and managed from here. The folder is on the left and the file open in
// it on the right; on a phone, one at a time. Where you are is the URL's
// `path` (a folder or a file), so back, forward and links work.

import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { FolderOpen } from 'lucide-react'
import type { Assistant } from '@/lib/assistant'
import { useMediaQuery } from '@/lib/hooks'
import { Empty } from '@/components/ui/misc'
import { FolderPane } from './files/Listing'
import { FilePreview } from './files/Preview'
import {
  listingQuery,
  parentOf,
  seenEntry,
  seenRoot,
  trimSlash,
  type FileEntry,
} from './files/model'
import type { AssistantSearch } from './search'

export function FilesTab({ assistant }: { assistant: Assistant }) {
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const qc = useQueryClient()
  const wide = useMediaQuery('(min-width: 768px)')
  const id = assistant.id
  const up = assistant.state.reachable && !assistant.state.frozen

  const root = seenRoot(qc, id)?.root
  const path = search.path && trimSlash(search.path) !== root ? trimSlash(search.path) : undefined
  // Is `path` a file? A listing already fetched may say; otherwise the
  // folder above's listing does.
  const seen = path ? seenEntry(qc, id, path) : undefined
  const above = path && parentOf(path) !== root ? parentOf(path) : undefined
  const parent = useQuery({ ...listingQuery(id, above), enabled: up && !!path && !seen })
  const known = seen ?? parent.data?.entries.find((e) => e.path === path)
  const isFile = !!path && !!known && !known.is_directory
  const resolved = !path || !!seen || !parent.isPending
  const dir = isFile ? parentOf(path) : path
  const folder = isFile ? above : dir
  const listing = useQuery({ ...listingQuery(id, folder), enabled: up && resolved })
  const entry = isFile ? listing.data?.entries.find((e) => e.path === path) : undefined

  const go = (p?: string, replace = false) =>
    navigate({ search: (s: AssistantSearch) => ({ ...s, path: p }), replace })
  const toFolder = (p?: string) => go(p && p !== listing.data?.root ? p : undefined)
  const onDeleted = (e: FileEntry) => {
    if (path && (path === e.path || path.startsWith(e.path + '/'))) {
      const up = parentOf(e.path)
      go(up === listing.data?.root ? undefined : up, true)
    }
  }

  if (!up)
    return (
      <Empty icon={<FolderOpen />} title={`${assistant.name}'s files are out of reach`}>
        {assistant.state.frozen
          ? 'It is frozen at the gate.'
          : "It isn't answering right now. They'll be back when it is."}
      </Empty>
    )
  return (
    <div className="flex min-h-0 flex-1">
      {(wide || !isFile) && (
        <section
          aria-label="Folder"
          className={clsx(
            'flex min-h-0 min-w-0 flex-col',
            isFile ? 'w-[380px] shrink-0 border-r border-line lg:w-[420px]' : 'flex-1',
          )}
        >
          <FolderPane
            assistant={assistant}
            listing={listing}
            selected={isFile ? path : undefined}
            onGo={toFolder}
            onDeleted={onDeleted}
          />
        </section>
      )}
      {isFile && (
        <section aria-label="File" className="flex min-h-0 min-w-0 flex-1 flex-col">
          <FilePreview
            key={path}
            id={id}
            path={path}
            entry={entry}
            loading={listing.isPending}
            wide={wide}
            onClose={() => toFolder(dir)}
            onDeleted={onDeleted}
          />
        </section>
      )}
    </div>
  )
}
