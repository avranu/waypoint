import { useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { Cable, Cpu, KeyRound, MessageCircle, Server, SlidersHorizontal } from 'lucide-react'
import type { Assistant } from '@/lib/assistant'
import { TabNav, type TabDef } from '../settings/common'
import type { AssistantSearch } from './search'
import { ModelSection } from './settings/Model'
import { ConfigSection } from './settings/Config'
import { KeysSection } from './settings/Keys'
import { ChannelsSection } from './settings/Channels'
import { SystemSection } from './settings/System'
import { ConnectionSection } from './settings/Connection'

type View = 'model' | 'config' | 'keys' | 'channels' | 'system' | 'connection'

const VIEWS: TabDef<View>[] = [
  { id: 'model', label: 'Model', icon: <Cpu /> },
  { id: 'config', label: 'Configuration', icon: <SlidersHorizontal /> },
  { id: 'keys', label: 'Keys', icon: <KeyRound /> },
  { id: 'channels', label: 'Channels', icon: <MessageCircle /> },
  { id: 'system', label: 'System', icon: <Server /> },
  { id: 'connection', label: 'Connection', icon: <Cable /> },
]

export function SettingsTab({ assistant }: { assistant: Assistant }) {
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const view = VIEWS.find((v) => v.id === search.view)?.id ?? 'model'
  const go = (v: View) =>
    navigate({
      search: (s: AssistantSearch) => ({ ...s, view: v === 'model' ? undefined : v }),
      replace: true,
    })
  const [draft, setDraft] = useState<Record<string, unknown>>({})
  const id = assistant.id
  return (
    <div className="flex flex-col gap-4 lg:flex-row lg:gap-8">
      <TabNav tabs={VIEWS} active={view} onSelect={go} />
      <div className="min-w-0 flex-1 space-y-4">
        {view === 'model' ? (
          <ModelSection id={id} />
        ) : view === 'config' ? (
          <ConfigSection id={id} draft={draft} setDraft={setDraft} />
        ) : view === 'keys' ? (
          <KeysSection id={id} onConnections={() => navigate({ search: { tab: 'tools' } })} />
        ) : view === 'channels' ? (
          <ChannelsSection id={id} />
        ) : view === 'system' ? (
          <SystemSection id={id} />
        ) : (
          <ConnectionSection assistant={assistant} />
        )}
      </div>
    </div>
  )
}
