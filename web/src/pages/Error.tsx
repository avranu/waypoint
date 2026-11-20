import { useRouter, type ErrorComponentProps } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'

/** Shown when a page throws; the rest of the app keeps working. */
export function ErrorPage({ error, reset }: ErrorComponentProps) {
  const router = useRouter()
  const qc = useQueryClient()
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
      <TriangleAlert className="size-8 text-warn" />
      <div className="text-[15px] font-medium">Something broke on this page</div>
      <div className="max-w-md break-words font-mono text-xs text-muted">
        {error instanceof Error ? error.message : String(error)}
      </div>
      <div className="flex gap-2">
        <Button
          variant="primary"
          onClick={() => {
            qc.invalidateQueries()
            reset()
            router.invalidate()
          }}
        >
          Try again
        </Button>
        <Button onClick={() => router.navigate({ to: '/' })}>Go home</Button>
      </div>
    </div>
  )
}
