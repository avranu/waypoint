import { useEffect } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  BotMessageSquare,
  FlaskConical,
  GitBranch,
  Mail,
  MailPlus,
  Server,
  ShieldAlert,
  Sparkles,
  Users,
  Webhook,
  Zap,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { Empty, PageHeader, Skeleton } from '@/components/ui/misc'
import { TabbedLayout, TabNav, type TabDef } from './settings/common'
import { UsersTab } from './admin/Users'
import { InvitesTab } from './admin/Invites'
import { ForgesTab } from './admin/Forges'
import { WebhooksTab } from './admin/Webhooks'
import { AITab } from './admin/AI'
import { RunnersTab } from './settings/Runners'
import { AutomationsAdminTab } from './admin/Automations'
import { AssistantsTab } from './admin/Assistants'
import { EmailTab } from './admin/Email'
import { ExperimentsTab } from './admin/Experiments'

type Tab =
  | 'users'
  | 'invites'
  | 'forges'
  | 'ai'
  | 'assistants'
  | 'automations'
  | 'experiments'
  | 'runners'
  | 'email'
  | 'webhooks'

const TABS: TabDef<Tab>[] = [
  { id: 'users', label: 'People', icon: <Users /> },
  { id: 'invites', label: 'Invites', icon: <MailPlus /> },
  { id: 'forges', label: 'Forges', icon: <GitBranch /> },
  { id: 'ai', label: 'AI', icon: <Sparkles /> },
  { id: 'assistants', label: 'Assistants', icon: <BotMessageSquare /> },
  { id: 'automations', label: 'Automations', icon: <Zap /> },
  { id: 'experiments', label: 'Experiments', icon: <FlaskConical /> },
  { id: 'runners', label: 'Runners', icon: <Server /> },
  { id: 'email', label: 'Email', icon: <Mail /> },
  { id: 'webhooks', label: 'Global webhooks', icon: <Webhook /> },
]

export function AdminPage() {
  const { tab: raw } = useSearch({ from: '/app/admin' })
  const navigate = useNavigate()
  const me = useQuery(q.me()).data
  const tab: Tab = TABS.some((t) => t.id === raw) ? (raw as Tab) : 'users'
  const go = (t: Tab) =>
    navigate({ to: '/admin', search: { tab: t === 'users' ? undefined : t }, replace: true })
  useEffect(() => {
    document.title = 'Admin · Waypoint'
  }, [])
  if (me && !me.is_admin) {
    return (
      <div className="flex-1 overflow-y-auto">
        <Empty
          icon={<ShieldAlert />}
          title="Admins only"
          action={
            <Link to="/" className="text-[13px] font-medium text-accent hover:underline">
              Back to home
            </Link>
          }
        >
          This area is for managing people and integrations. Ask an admin if you need something
          changed.
        </Empty>
      </div>
    )
  }
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-4xl">
        <PageHeader title="Admin" />
        <TabbedLayout nav={<TabNav tabs={TABS} active={tab} onSelect={go} />}>
          {!me ? (
            <Skeleton className="h-64" />
          ) : tab === 'users' ? (
            <UsersTab me={me} />
          ) : tab === 'invites' ? (
            <InvitesTab />
          ) : tab === 'forges' ? (
            <ForgesTab />
          ) : tab === 'ai' ? (
            <AITab />
          ) : tab === 'assistants' ? (
            <AssistantsTab />
          ) : tab === 'runners' ? (
            <RunnersTab all />
          ) : tab === 'automations' ? (
            <AutomationsAdminTab />
          ) : tab === 'experiments' ? (
            <ExperimentsTab />
          ) : tab === 'email' ? (
            <EmailTab />
          ) : (
            <WebhooksTab />
          )}
        </TabbedLayout>
      </div>
    </div>
  )
}
