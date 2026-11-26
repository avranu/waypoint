import { hermesQuery } from '@/lib/assistant'

export interface McpServer {
  name: string
  transport: 'http' | 'stdio' | 'unknown'
  url: string | null
  command: string | null
  args: string[]
  env: Record<string, string>
  auth: 'oauth' | 'header' | null
  enabled: boolean
  /** the tools picked from it; null means all of them */
  tools: string[] | null
  source: 'config' | 'plugin'
  plugin: string | null
}

export interface McpTest {
  ok: boolean
  error?: string
  tools: { name: string; description: string; schema_chars?: number }[]
  prompts?: number
  resources?: number
}

export interface Toolset {
  name: string
  label: string
  description: string
  platform: string
  platform_label: string
  enabled: boolean
  available: boolean
  configured: boolean
  tools: string[]
}

export const mcpServersQuery = (id: number) =>
  hermesQuery<{ servers: McpServer[] }>(id, 'mcp/servers')

export const toolsetsQuery = (id: number) => hermesQuery<Toolset[]>(id, 'tools/toolsets')

/** Hermes's config; only agent.disabled_toolsets is read here. */
export const configQuery = (id: number) =>
  hermesQuery<{ agent?: { disabled_toolsets?: string[] } }>(id, 'config')
