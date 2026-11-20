import type { ReactNode } from 'react'
import { useRouter } from '@tanstack/react-router'

/**
 * A link to an app path the server gave as a string (/issue/WP-1,
 * /agents?tab=pulls&pr=3), navigating without a reload. Modified clicks keep
 * their browser meaning (new tab).
 */
export function AppLink({
  to,
  className,
  title,
  children,
  onClick,
}: {
  to: string
  className?: string
  title?: string
  children: ReactNode
  onClick?: (e: React.MouseEvent) => void
}) {
  const router = useRouter()
  return (
    <a
      href={to}
      title={title}
      className={className}
      onClick={(e) => {
        onClick?.(e)
        if (
          e.defaultPrevented ||
          e.button !== 0 ||
          e.metaKey ||
          e.ctrlKey ||
          e.shiftKey ||
          e.altKey
        )
          return
        e.preventDefault()
        router.history.push(to)
      }}
    >
      {children}
    </a>
  )
}
