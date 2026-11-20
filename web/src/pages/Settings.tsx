import { useEffect } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Bell,
  Bot,
  KeyRound,
  Lock,
  MonitorSmartphone,
  Palette,
  Plug,
  Server,
  UserRound,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { PageHeader, Skeleton } from '@/components/ui/misc'
import { TabbedLayout, TabNav, type TabDef } from './settings/common'
import { AppearanceTab, PasswordTab, ProfileTab } from './settings/Profile'
import { NotificationsTab } from './settings/Notifications'
import { TokensTab } from './settings/Tokens'
import { ConnectionsTab } from './settings/Connections'
import { SessionsTab } from './settings/Sessions'
import { ClaudeCodeTab } from './settings/ClaudeCode'
import { RunnersTab } from './settings/Runners'

type Tab =
  | 'profile'
  | 'appearance'
  | 'password'
  | 'sessions'
  | 'claude'
  | 'notifications'
  | 'tokens'
  | 'connections'
  | 'runners'

const TABS: TabDef<Tab>[] = [
  { id: 'profile', label: 'Profile', icon: <UserRound /> },
  { id: 'appearance', label: 'Appearance', icon: <Palette /> },
  { id: 'password', label: 'Password', icon: <Lock /> },
  { id: 'sessions', label: 'Sessions', icon: <MonitorSmartphone /> },
  { id: 'claude', label: 'Claude Code', icon: <Bot /> },
  { id: 'notifications', label: 'Notifications', icon: <Bell /> },
  { id: 'tokens', label: 'API tokens', icon: <KeyRound /> },
  { id: 'connections', label: 'Connections', icon: <Plug /> },
  { id: 'runners', label: 'Runners', icon: <Server /> },
]

export function SettingsPage() {
  const { tab: raw } = useSearch({ from: '/app/settings' })
  const navigate = useNavigate()
  const me = useQuery(q.me()).data
  const tab: Tab = TABS.some((t) => t.id === raw) ? (raw as Tab) : 'profile'
  const go = (t: Tab) =>
    navigate({ to: '/settings', search: { tab: t === 'profile' ? undefined : t }, replace: true })
  useEffect(() => {
    document.title = 'Settings · Waypoint'
  }, [])
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-4xl">
        <PageHeader title="Settings" subtitle={me ? `Signed in as @${me.username}` : undefined} />
        <TabbedLayout nav={<TabNav tabs={TABS} active={tab} onSelect={go} />}>
          {!me ? (
            <Skeleton className="h-64" />
          ) : tab === 'profile' ? (
            <ProfileTab me={me} />
          ) : tab === 'appearance' ? (
            <AppearanceTab me={me} />
          ) : tab === 'password' ? (
            <PasswordTab />
          ) : tab === 'sessions' ? (
            <SessionsTab />
          ) : tab === 'claude' ? (
            <ClaudeCodeTab me={me} />
          ) : tab === 'notifications' ? (
            <NotificationsTab me={me} />
          ) : tab === 'tokens' ? (
            <TokensTab />
          ) : tab === 'runners' ? (
            <RunnersTab />
          ) : (
            <ConnectionsTab onCreateToken={() => go('tokens')} />
          )}
        </TabbedLayout>
      </div>
    </div>
  )
}
