// A plan's user story (WP-190): a few lines read first, like an abstract.
// When an agent wrote the plan, approving it keeps the story as the
// person's intent and nothing else of it; the plan stays the agent's text.
// Until a person keeps or writes the story, it is the story model's
// suggestion and no one's intent.

import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Bot, Check, MessageSquareQuote, Sparkles, UserCheck } from 'lucide-react'
import { agentLabel } from '@/lib/agents'
import { relativeTime } from '@/lib/format'
import { q, useSuggestStory } from '@/lib/queries'
import type { Page } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { MarkdownEditor } from '@/components/issue/Description'
import { useReview } from '@/components/Provenance'

export function PlanStory({
  page: p,
  editable,
  onSave,
}: {
  page: Page
  editable: boolean
  /** saves a story edit; the text a person sends is theirs */
  onSave: (story: string) => Promise<unknown>
}) {
  const me = useQuery(q.me())
  const suggest = useSuggestStory()
  const review = useReview()
  const story = p.story ?? ''
  const theirs = story !== '' && !p.story_agent
  const offer = editable && p.category === 'plan' && !!p.text_agent && !!p.body?.trim()
  if (!story && !p.prompt && !offer) return null

  const keep = () =>
    p.unreviewed
      ? review.mutate({ type: 'page', ref: String(p.id) })
      : onSave(story).catch(() => undefined)
  const asker =
    p.prompt && me.data && p.owner && p.owner.id !== me.data.id ? p.owner.display_name : 'you'

  return (
    <section
      aria-labelledby={`story-${p.id}`}
      className="space-y-2 rounded-lg border border-line bg-panel p-3 shadow-card sm:p-4"
      data-testid="plan-story"
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <h2
          id={`story-${p.id}`}
          className="text-xs font-semibold uppercase tracking-wide text-subtle"
        >
          User story
        </h2>
        {story &&
          (theirs ? (
            <span
              title={`${p.story_model ? 'Kept' : 'Written'} by ${p.story_by?.display_name ?? 'a person'}: this is what they meant the plan to do.`}
            >
              <Badge color="var(--wp-ok)">
                <UserCheck className="size-3" aria-hidden />
                {p.story_by?.display_name ?? 'A person'}’s intent
              </Badge>
            </span>
          ) : (
            <span
              title={`Suggested by ${p.story_model ? agentLabel(p.story_model) : 'the story model'} from the plan${p.prompt ? ' and what was asked' : ''}. No person has kept it, so it is no one's intent yet.`}
            >
              <Badge color="var(--wp-warn)">
                <Bot className="size-3" aria-hidden />
                Suggested{p.story_model ? ` by ${agentLabel(p.story_model)}` : ''}
              </Badge>
            </span>
          ))}
        {theirs && p.story_at && (
          <span className="text-xs text-subtle">
            {p.story_model ? 'kept' : 'written'} {relativeTime(p.story_at)}
          </span>
        )}
      </div>

      {story || editable ? (
        <div className="text-[15px]">
          <MarkdownEditor
            value={story}
            editable={editable}
            minRows={3}
            placeholder="As …, I want …, so that … (what you meant this plan to do)"
            draftKey={editable ? `page-story:${p.id}` : undefined}
            project={p.project_key}
            onSave={onSave}
          />
        </div>
      ) : null}

      {p.story_stale && (
        <p className="text-xs text-warn">
          The plan changed after this story was{' '}
          {theirs ? (p.story_model ? 'kept' : 'written') : 'suggested'}.
          {theirs && editable ? ' Read the plan, then keep the story again or edit it.' : ''}
        </p>
      )}

      {editable && (
        <div className="flex flex-wrap items-center gap-2">
          {story && (!theirs || p.story_stale) && (
            <Button
              size="sm"
              variant="primary"
              icon={<Check className="size-3.5" />}
              loading={review.isPending}
              onClick={keep}
            >
              {p.unreviewed ? 'Approve plan' : 'Keep as my intent'}
            </Button>
          )}
          {!story && offer && (
            <Button
              size="sm"
              icon={<Sparkles className="size-3.5" />}
              loading={suggest.isPending}
              onClick={() => suggest.mutate(p.id, { onError: toastError })}
            >
              Suggest a story
            </Button>
          )}
          {story && !theirs && (
            <span className="text-xs text-muted">
              {p.unreviewed
                ? 'Approving keeps this story as your intent; the plan below stays the agent’s text.'
                : 'Keeping it, or editing it, makes it your intent.'}
            </span>
          )}
          {!story && offer && (
            <span className="text-xs text-muted">
              A few lines on what this plan is for, read before it.
            </span>
          )}
        </div>
      )}

      {p.prompt && (
        <details className="group text-[13px]">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs text-muted hover:text-fg">
            <MessageSquareQuote className="size-3.5" aria-hidden />
            <span>What {asker} asked for</span>
            <span aria-hidden>·</span>
            <Link
              to="/agents/sessions/$id"
              params={{ id: String(p.prompt.session_id) }}
              className="hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              session #{p.prompt.session_id}
            </Link>
            <span className="text-subtle">{relativeTime(p.prompt.at)}</span>
          </summary>
          <blockquote className="mt-1.5 max-h-64 overflow-auto whitespace-pre-wrap break-words border-l-2 border-line pl-3 text-muted">
            {p.prompt.text}
          </blockquote>
        </details>
      )}
    </section>
  )
}

/** The heading over an agent's plan once it has a story. */
export function PlanHeading({ page: p }: { page: Page }) {
  if (!p.story || !p.text_agent) return null
  return (
    <div className="flex flex-wrap items-baseline gap-x-2 pt-1">
      <h2 className="text-xs font-semibold uppercase tracking-wide text-subtle">The plan</h2>
      <span className="text-xs text-subtle">
        {agentLabel(p.text_agent)}’s text
        {p.reviewed_on_story && !p.unreviewed ? '; approved on its story, not line by line' : ''}
      </span>
    </div>
  )
}
