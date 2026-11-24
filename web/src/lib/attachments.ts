// Attachment targets are written 'kind:id', e.g. 'page:12', 'issue:WP-12'.

import { queryOptions, type QueryClient } from '@tanstack/react-query'
import { api, qs } from './api'
import type { Attachment } from './types'

export const issueTarget = (key: string) => `issue:${key}`
export const pageTarget = (id: number) => `page:${id}`
export const designTarget = (id: number) => `design:${id}`

export const attachmentsQuery = (target: string) =>
  queryOptions({
    queryKey: ['attachments', target],
    queryFn: () => api.get<Attachment[]>(`/attachments${qs({ target })}`),
    staleTime: 30_000,
  })

/** Uploads one file to a target. */
export function uploadAttachment(target: string, file: File, caption?: string) {
  const form = new FormData()
  form.append('file', file, file.name || 'pasted-image.png')
  if (caption) form.append('caption', caption)
  return api.post<Attachment>(`/attachments${qs({ target })}`, form)
}

/** Refetch what shows a target's files (an issue's come with the issue). */
export function refreshAttachments(qc: QueryClient, target: string) {
  qc.invalidateQueries({ queryKey: ['attachments', target] })
  if (target.startsWith('issue:')) {
    const key = target.slice('issue:'.length)
    qc.invalidateQueries({ queryKey: ['issue', key] })
    qc.invalidateQueries({ queryKey: ['comments', key] })
  }
}

export function fileSize(n: number) {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

/** Text previews load at most this much of a file. */
export const PREVIEW_BYTES = 256 * 1024

export const downloadURL = (a: Attachment) => `${a.url}?download=1`
