// The assistant's Tools tab: what the agent can reach and how. Connections
// are the gate's (services whose credentials it holds); MCP servers and
// toolsets are Hermes's own.

import type { Assistant } from '@/lib/assistant'
import { ConnectionsSection } from './tools/Connections'
import { McpServersSection } from './tools/McpServers'
import { ToolsetsSection } from './tools/Toolsets'

export function ToolsTab({ assistant }: { assistant: Assistant }) {
  return (
    <div className="space-y-5">
      <ConnectionsSection assistant={assistant} />
      <McpServersSection assistant={assistant} />
      <ToolsetsSection assistant={assistant} />
    </div>
  )
}
