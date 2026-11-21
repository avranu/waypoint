import { memo, useMemo } from 'react'
import clsx from 'clsx'
import { renderMarkdown } from '@/lib/markdown'

/** Markdown from a model or another person: rendered as untrusted. */
export const Markdown = memo(function Markdown({
  text,
  className,
}: {
  text: string
  className?: string
}) {
  const html = useMemo(() => renderMarkdown(text, { untrusted: true }), [text])
  return (
    <div
      className={clsx('prose-wp break-words text-[14px] leading-relaxed', className)}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
})
