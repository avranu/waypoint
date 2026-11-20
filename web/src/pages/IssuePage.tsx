import { useEffect } from 'react'
import { useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { q } from '@/lib/queries'
import { IssueDetailView } from '@/components/issue/IssueDetail'
import { projectStyle } from './Project'

export function IssuePage() {
  const { key } = useParams({ from: '/app/issue/$key' })
  const issue = useQuery(q.issue(key)).data
  const project = useQuery({ ...q.project(issue?.project_key ?? ''), enabled: !!issue }).data
  useEffect(() => {
    if (issue) document.title = `${issue.key} ${issue.title} · Waypoint`
  }, [issue])
  return (
    <div
      className="flex-1 overflow-y-auto bg-panel"
      style={project ? projectStyle(project) : undefined}
    >
      <IssueDetailView key={key} issueKey={key} mode="page" />
    </div>
  )
}
