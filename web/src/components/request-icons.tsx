import { HelpCircle, ListChecks, ShieldQuestion, SquarePen, Terminal } from 'lucide-react'
import type { RequestKind } from '@/lib/requests'

/** An icon for each kind of request (WP-D16). */
export const requestIcon: Record<RequestKind, React.ReactNode> = {
  question: <HelpCircle />,
  change: <SquarePen />,
  command: <Terminal />,
  permission: <ShieldQuestion />,
  action: <ListChecks />,
}
