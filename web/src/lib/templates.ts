import { queryOptions } from '@tanstack/react-query'
import { api, enc } from './api'

export interface IssueTemplate {
  id: number
  project_id: number
  name: string
  type: string
  title: string
  description: string
  labels: string[]
  points: number | null
  subtasks: string[]
  created_at: string
}

export const templatesQuery = (projectKey: string) =>
  queryOptions({
    queryKey: ['templates', projectKey],
    queryFn: () => api.get<IssueTemplate[]>(`/projects/${enc(projectKey)}/templates`),
    staleTime: 5 * 60_000,
  })
