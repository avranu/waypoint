import { useEffect, useState } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { BookA, Lightbulb, Link2Off } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  designApi,
  glossaryApi,
  invalidateKnowledge,
  itemHref,
  resolveQuery,
} from '@/lib/knowledge'
import { Button } from '@/components/ui/button'
import { Empty, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { ScopePicker, useScopeOptions } from '@/components/knowledge'

export function GoPage() {
  const { ref = '', project } = useSearch({ from: '/app/go' })
  const navigate = useNavigate()
  const res = useQuery({ ...resolveQuery(ref, project), enabled: !!ref })
  useEffect(() => {
    const it = res.data
    if (!it) return
    const href = itemHref(it)
    if (href) navigate({ to: href, replace: true })
    else if (it.type === 'url' && it.ref) location.replace(it.ref)
  }, [res.data, navigate])
  useEffect(() => {
    document.title = `${ref} · Waypoint`
  }, [ref])
  if (!ref) return <Empty icon={<Link2Off />} title="Nothing to look up" />
  if (res.isPending || res.data)
    return (
      <div className="mx-auto w-full max-w-md space-y-3 p-8">
        <Skeleton className="h-6 w-48" />
        <Skeleton className="h-4 w-64" />
      </div>
    )
  if (res.isError)
    return (
      <Empty icon={<Link2Off />} title={`Couldn’t look up “${ref}”`}>
        {res.error?.message}
      </Empty>
    )
  return <Unresolved name={ref} project={project} />
}

/** Nothing called that yet: make it a term or a design note. */
function Unresolved({ name, project }: { name: string; project?: string }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const scopes = useScopeOptions(project)
  const projects = useQuery(q.projects()).data ?? []
  const [scope, setScope] = useState('')
  const where = scope || scopes[0]?.value || ''
  const create = useMutation({
    mutationFn: (kind: 'glossary' | 'design'): Promise<{ id: number }> =>
      kind === 'glossary'
        ? glossaryApi.create({ term: name, project: where === 'global' ? '' : where })
        : designApi.create({ title: name, kind: 'note', project: where === 'global' ? '' : where }),
    onSuccess: (it, kind) => {
      invalidateKnowledge(qc)
      navigate({
        to: kind === 'glossary' ? '/glossary/$id' : '/design/$id',
        params: { id: String(it.id) },
        replace: true,
      })
    },
    onError: toastError,
  })
  const p = projects.find((x) => x.key === project)
  return (
    <Empty
      icon={<Link2Off />}
      title={`Nothing called “${name}” yet`}
      action={
        scopes.length > 0 && (
          <div className="flex flex-col items-center gap-3">
            <div className="flex items-center gap-1 text-xs text-subtle">
              Create it in <ScopePicker value={where} onChange={setScope} project={project} />
            </div>
            <div className="flex flex-wrap justify-center gap-2">
              <Button
                icon={<BookA className="size-4" />}
                loading={create.isPending && create.variables === 'glossary'}
                onClick={() => create.mutate('glossary')}
              >
                Create glossary term
              </Button>
              <Button
                icon={<Lightbulb className="size-4" />}
                loading={create.isPending && create.variables === 'design'}
                onClick={() => create.mutate('design')}
              >
                Create design note
              </Button>
            </div>
          </div>
        )
      }
    >
      No issue, page, glossary term or design note
      {p ? (
        <>
          {' '}
          in{' '}
          <Link to="/p/$key/board" params={{ key: p.key }} className="text-accent hover:underline">
            {p.name}
          </Link>{' '}
          (or its parents)
        </>
      ) : null}{' '}
      goes by that name. The link starts working as soon as something does.
    </Empty>
  )
}
