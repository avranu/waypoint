import { useEffect, useRef } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import clsx from 'clsx'
import {
  Bot,
  FileStack,
  ListChecks,
  Plug,
  Server,
  Settings2,
  ShieldCheck,
  Tag,
  Users,
  Workflow,
  Zap,
} from 'lucide-react'
import { useProject } from './Project'
import { GeneralTab } from './project-settings/General'
import { WorkflowTab } from './project-settings/Workflow'
import { AgentWorkflowsTab } from './project-settings/AgentWorkflows'
import { LabelsTab } from './project-settings/Labels'
import { MembersTab } from './project-settings/Members'
import { AutomationsTab } from './project-settings/Automations'
import { BotsTab } from './project-settings/Bots'
import { IntegrationsTab } from './project-settings/Integrations'
import { EnvironmentsTab } from './project-settings/Environments'
import { TemplatesTab } from './project-settings/Templates'
import { SandboxTab } from './project-settings/Sandbox'
import type { Project } from '@/lib/types'

const TABS = [
  { id: 'general', label: 'General', icon: Settings2, Component: GeneralTab },
  { id: 'workflow', label: 'Statuses', icon: ListChecks, Component: WorkflowTab },
  { id: 'workflows', label: 'Workflows', icon: Workflow, Component: AgentWorkflowsTab },
  { id: 'labels', label: 'Tags', icon: Tag, Component: LabelsTab },
  { id: 'templates', label: 'Templates', icon: FileStack, Component: TemplatesTab },
  { id: 'members', label: 'Members', icon: Users, Component: MembersTab },
  { id: 'automations', label: 'Automations', icon: Zap, Component: AutomationsTab },
  { id: 'bots', label: 'Bots', icon: Bot, Component: BotsTab },
  { id: 'integrations', label: 'Integrations', icon: Plug, Component: IntegrationsTab },
  { id: 'environments', label: 'Environments', icon: Server, Component: EnvironmentsTab },
  { id: 'sandbox', label: 'Sandbox', icon: ShieldCheck, Component: SandboxTab },
] as const satisfies readonly {
  id: string
  label: string
  icon: unknown
  Component: (p: { project: Project }) => unknown
}[]

export function ProjectSettingsPage() {
  const project = useProject()
  const search = useSearch({ from: '/app/p/$key/settings' })
  const navigate = useNavigate()
  const current = TABS.find((t) => t.id === search.tab) ?? TABS[0]
  const scroller = useRef<HTMLDivElement>(null)
  const go = (tab: string) =>
    navigate({
      to: '/p/$key/settings',
      params: { key: project.key },
      search: { tab: tab === 'general' ? undefined : tab },
      replace: true,
    })
  useEffect(() => {
    scroller.current?.scrollTo({ top: 0 })
    document
      .getElementById(`settings-tab-${current.id}`)
      ?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [current.id])
  if (!project) return null
  const Tab = current.Component
  return (
    <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex w-full max-w-5xl flex-col lg:flex-row lg:gap-8 lg:px-6 lg:py-6">
        <nav
          aria-label="Settings sections"
          role="tablist"
          className="wp-surface-blur no-scrollbar sticky top-0 z-[2] flex shrink-0 gap-1 overflow-x-auto border-b border-line bg-panel/90 px-3 py-2 backdrop-blur lg:w-48 lg:flex-col lg:self-start lg:overflow-visible lg:rounded-xl lg:border-0 lg:bg-transparent lg:p-0 lg:backdrop-blur-none lg:sticky lg:top-6"
        >
          {TABS.map((t) => (
            <button
              key={t.id}
              id={`settings-tab-${t.id}`}
              role="tab"
              aria-selected={t.id === current.id}
              onClick={() => go(t.id)}
              className={clsx(
                'flex h-8 shrink-0 items-center gap-2 rounded-lg px-2.5 text-[13px] transition-colors',
                t.id === current.id
                  ? 'bg-accent-soft font-medium text-fg'
                  : 'text-muted hover:bg-hover hover:text-fg',
              )}
            >
              <t.icon
                className={clsx('size-4', t.id === current.id ? 'text-accent' : 'text-subtle')}
              />
              {t.label}
            </button>
          ))}
        </nav>
        <main
          role="tabpanel"
          className="wp-surface wp-pad-lg min-w-0 max-w-3xl flex-1 px-3 py-4 sm:px-5 lg:p-0"
        >
          <Tab key={current.id} project={project} />
        </main>
      </div>
    </div>
  )
}
