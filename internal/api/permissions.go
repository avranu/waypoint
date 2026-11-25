package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"git.jmann.me/claude/waypoint/internal/core"
)

const (
	// permAlways: any signed-in token may use the route (who am I, help).
	permAlways core.Permission = "*"
	// permNever: no permission token may, whatever it was granted: managing
	// tokens, passwords, browser sessions and CLI logins would let it mint
	// or keep a stronger credential.
	permNever core.Permission = "-"
	// permMCP: the MCP endpoint, which checks each tool (mcpToolPermissions).
	permMCP core.Permission = "mcp"
)

var routePermissions = map[string]core.Permission{
	"POST /mcp": permMCP,

	"GET /api/v1/me":                         permAlways,
	"GET /api/v1/me/token-permissions":       permAlways,
	"GET /api/v1/users/{id}/avatar":          permAlways,
	"GET /api/v1/agent":                      permAlways,
	"GET /api/v1/agent/help/{topic}":         permAlways,
	"PATCH /api/v1/me":                       core.PermProfile,
	"PUT /api/v1/me/avatar":                  core.PermProfile,
	"DELETE /api/v1/me/avatar":               core.PermProfile,
	"GET /api/v1/me/calendar":                core.PermProfile,
	"POST /api/v1/me/calendar/rotate":        core.PermProfile,
	"POST /api/v1/me/gotify/test":            core.PermProfile,
	"GET /api/v1/me/push":                    core.PermProfile,
	"POST /api/v1/me/push":                   core.PermProfile,
	"DELETE /api/v1/me/push":                 core.PermProfile,
	"POST /api/v1/me/password":               permNever,
	"GET /api/v1/me/tokens":                  permNever,
	"POST /api/v1/me/tokens":                 permNever,
	"PATCH /api/v1/me/tokens/{id}":           permNever,
	"DELETE /api/v1/me/tokens/{id}":          permNever,
	"DELETE /api/v1/me/tokens/{id}/refusals": permNever,
	"GET /api/v1/me/sessions":                permNever,
	"DELETE /api/v1/me/sessions/{id}":        permNever,
	"GET /api/v1/cli/login/{code}":           permNever,
	"POST /api/v1/cli/login/{code}/approve":  permNever,
	"GET /api/v1/notifications":              core.PermNotifications,
	"POST /api/v1/notifications/read":        core.PermNotifications,
	"GET /api/v1/inbox":                      core.PermNotifications,
	"GET /api/v1/inbox/counts":               core.PermNotifications,
	"POST /api/v1/inbox/read":                core.PermNotifications,
	"PUT /api/v1/issues/{key}/watch":         core.PermNotifications,
	"DELETE /api/v1/issues/{key}/watch":      core.PermNotifications,
	"GET /api/v1/views":                      core.PermIssuesRead,
	"POST /api/v1/views":                     core.PermViewsWrite,
	"PATCH /api/v1/views/{id}":               core.PermViewsWrite,
	"DELETE /api/v1/views/{id}":              core.PermViewsWrite,

	"GET /api/v1/projects/{key}/sandbox":       core.PermIssuesRead,
	"GET /api/v1/sandbox/machines":             core.PermRunnersRead,
	"GET /api/v1/sandbox/log":                  core.PermIssuesRead,
	"GET /api/v1/sandbox/key":                  core.PermRunnersRead,
	"PATCH /api/v1/projects/{key}/sandbox":     permNever,
	"PATCH /api/v1/sandbox/machines/{id}":      permNever,
	"POST /api/v1/sandbox/log/{id}/revert":     permNever,
	"POST /api/v1/sandbox/machines/{id}/ops":   permNever,
	"POST /api/v1/sandbox/machines/{id}/drill": permNever,
	"POST /api/v1/me/reauth":                   permNever,
	"GET /api/v1/me/reauth":                    permNever,

	"GET /api/v1/assistants":                                 permNever,
	"POST /api/v1/assistants":                                permNever,
	"GET /api/v1/assistants/{id}":                            permNever,
	"PATCH /api/v1/assistants/{id}":                          permNever,
	"DELETE /api/v1/assistants/{id}":                         permNever,
	"POST /api/v1/assistants/{id}/check":                     permNever,
	"POST /api/v1/assistants/{id}/test":                      permNever,
	"POST /api/v1/assistants/{id}/freeze":                    permNever,
	"GET /api/v1/assistants/{id}/live":                       permNever,
	"POST /api/v1/assistants/{id}/rpc":                       permNever,
	"POST /api/v1/assistants/{id}/answer":                    permNever,
	"GET /api/v1/assistants/{id}/gate/approvals":             permNever,
	"GET /api/v1/assistants/{id}/gate/audit":                 permNever,
	"GET /api/v1/assistants/{id}/gate/logs/{name}":           permNever,
	"GET /api/v1/assistants/{id}/gate/connections":           permNever,
	"PUT /api/v1/assistants/{id}/gate/connections/{name}":    permNever,
	"DELETE /api/v1/assistants/{id}/gate/connections/{name}": permNever,
	"GET /api/v1/assistants/{id}/links":                      permNever,
	"POST /api/v1/assistants/{id}/links":                     permNever,
	"DELETE /api/v1/assistants/{id}/links/{link}":            permNever,
	"GET /api/v1/issues/{key}/assistant-links":               permNever,
	"GET /api/v1/assistants/{id}/api/{path...}":              permNever,
	"POST /api/v1/assistants/{id}/api/{path...}":             permNever,
	"PUT /api/v1/assistants/{id}/api/{path...}":              permNever,
	"PATCH /api/v1/assistants/{id}/api/{path...}":            permNever,
	"DELETE /api/v1/assistants/{id}/api/{path...}":           permNever,

	"GET /api/v1/users":                                     core.PermIssuesRead,
	"GET /api/v1/stream":                                    permNever,
	"GET /api/v1/issues":                                    core.PermIssuesRead,
	"GET /api/v1/search":                                    core.PermIssuesRead,
	"GET /api/v1/issues/{key}":                              core.PermIssuesRead,
	"GET /api/v1/issues/{key}/comments":                     core.PermIssuesRead,
	"GET /api/v1/issues/{key}/history":                      core.PermIssuesRead,
	"GET /api/v1/issues/{key}/relations/suggested":          core.PermIssuesRead,
	"POST /api/v1/issues/{key}/rewritten":                   core.PermIssuesEdit,
	"GET /api/v1/issues/{key}/time":                         core.PermIssuesRead,
	"GET /api/v1/issues/{key}/forge-mentions":               core.PermIssuesRead,
	"GET /api/v1/issues/{key}/worklog":                      core.PermIssuesRead,
	"GET /api/v1/worklog/stats":                             core.PermIssuesRead,
	"GET /api/v1/worklog/triage":                            core.PermIssuesRead,
	"GET /api/v1/worklog/stakes":                            core.PermIssuesRead,
	"GET /api/v1/tags":                                      core.PermIssuesRead,
	"GET /api/v1/projects":                                  core.PermIssuesRead,
	"GET /api/v1/projects/{key}":                            core.PermIssuesRead,
	"GET /api/v1/projects/{key}/board":                      core.PermIssuesRead,
	"GET /api/v1/projects/{key}/backlog":                    core.PermIssuesRead,
	"GET /api/v1/projects/{key}/activity":                   core.PermIssuesRead,
	"GET /api/v1/projects/{key}/templates":                  core.PermIssuesRead,
	"GET /api/v1/projects/{key}/sprints":                    core.PermIssuesRead,
	"GET /api/v1/sprints/{id}":                              core.PermIssuesRead,
	"GET /api/v1/sprints/{id}/burndown":                     core.PermIssuesRead,
	"GET /api/v1/workflows":                                 core.PermIssuesRead,
	"GET /api/v1/projects/{key}/reports/velocity":           core.PermIssuesRead,
	"GET /api/v1/projects/{key}/reports/throughput":         core.PermIssuesRead,
	"GET /api/v1/projects/{key}/dashboard":                  core.PermIssuesRead,
	"GET /api/v1/dashboard":                                 core.PermIssuesRead,
	"POST /api/v1/projects/{key}/dashboard/fix":             core.PermIssuesCreate,
	"GET /api/v1/projects/{key}/reports/flow":               core.PermIssuesRead,
	"GET /api/v1/projects/{key}/reports/forecast":           core.PermIssuesRead,
	"GET /api/v1/projects/{key}/reports/roadmap":            core.PermIssuesRead,
	"GET /api/v1/reports/time":                              core.PermIssuesRead,
	"GET /api/v1/time":                                      core.PermIssuesRead,
	"POST /api/v1/issues":                                   core.PermIssuesCreate,
	"PATCH /api/v1/issues/{key}":                            core.PermIssuesEdit,
	"POST /api/v1/issues/bulk":                              core.PermIssuesEdit,
	"POST /api/v1/issues/similar":                           core.PermIssuesRead,
	"POST /api/v1/issues/{key}/transition":                  core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/move":                        core.PermIssuesEdit,
	"PUT /api/v1/issues/{key}/hold":                         core.PermIssuesEdit,
	"DELETE /api/v1/issues/{key}/hold":                      core.PermIssuesEdit,
	"GET /api/v1/objectives":                                core.PermIssuesRead,
	"GET /api/v1/issues/{key}/objective":                    core.PermIssuesRead,
	"PUT /api/v1/issues/{key}/objective":                    core.PermIssuesEdit,
	"DELETE /api/v1/issues/{key}/objective":                 core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/objective/checks":            core.PermTasksWork,
	"POST /api/v1/issues/{key}/objective/diagnose":          core.PermTasksWork,
	"POST /api/v1/issues/{key}/objective/confirm":           permNever,
	"POST /api/v1/issues/{key}/objective/go":                permNever,
	"POST /api/v1/issues/{key}/relations/suggested/dismiss": permNever,
	"POST /api/v1/issues/{key}/relations":                   core.PermIssuesEdit,
	"DELETE /api/v1/issues/{key}/relations/{id}":            core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/links":                       core.PermIssuesEdit,
	"DELETE /api/v1/issues/{key}/links/{id}":                core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/links/refresh":               core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/comments":                    core.PermIssuesComment,
	"POST /api/v1/issues/{key}/worklog":                     core.PermIssuesComment,
	"POST /api/v1/issues/{key}/review-report":               core.PermIssuesComment,
	"POST /api/v1/projects/{key}/test-evidence":             core.PermIssuesComment,
	"PATCH /api/v1/comments/{id}":                           core.PermIssuesComment,
	"DELETE /api/v1/comments/{id}":                          core.PermIssuesComment,
	"GET /api/v1/timer":                                     core.PermTimeTrack,
	"POST /api/v1/timer/start":                              core.PermTimeTrack,
	"POST /api/v1/timer/stop":                               core.PermTimeTrack,
	"POST /api/v1/time":                                     core.PermTimeTrack,
	"PATCH /api/v1/time/{id}":                               core.PermTimeTrack,
	"DELETE /api/v1/time/{id}":                              core.PermTimeTrack,
	"PUT /api/v1/issues/{key}/shares/{user}":                core.PermIssuesShare,
	"DELETE /api/v1/issues/{key}/shares/{user}":             core.PermIssuesShare,
	"POST /api/v1/issues/{key}/publish":                     core.PermIssuesPublish,
	"POST /api/v1/issues/{key}/draft":                       core.PermIssuesPublish,
	"GET /api/v1/issues/{key}/outside":                      permNever,
	"POST /api/v1/outside/{id}/approve":                     permNever,
	"POST /api/v1/outside/{id}/discard":                     permNever,
	"GET /api/v1/pulls/{id}/outside":                        permNever,
	"POST /api/v1/outside-pr/{id}/approve":                  permNever,
	"POST /api/v1/outside-pr/{id}/discard":                  permNever,
	"DELETE /api/v1/issues/{key}":                           core.PermIssuesDelete,

	"GET /api/v1/search/all":                       core.PermSearch,
	"GET /api/v1/resolve":                          core.PermSearch,
	"GET /api/v1/graph":                            core.PermSearch,
	"GET /api/v1/graph/path":                       core.PermSearch,
	"POST /api/v1/refs":                            core.PermSearch,
	"GET /api/v1/links":                            core.PermSearch,
	"POST /api/v1/links":                           core.PermLinksWrite,
	"DELETE /api/v1/links/{id}":                    core.PermLinksWrite,
	"GET /api/v1/pages":                            core.PermPagesRead,
	"GET /api/v1/pages/{id}":                       core.PermPagesRead,
	"GET /api/v1/projects/{key}/page-codes":        core.PermPagesRead,
	"POST /api/v1/pages":                           core.PermPagesWrite,
	"PATCH /api/v1/pages/{id}":                     core.PermPagesWrite,
	"POST /api/v1/pages/{id}/publish":              core.PermPagesWrite,
	"POST /api/v1/pages/{id}/story":                core.PermPagesWrite,
	"PUT /api/v1/pages/{id}/issues/{key}":          core.PermPagesWrite,
	"DELETE /api/v1/pages/{id}/issues/{key}":       core.PermPagesWrite,
	"PUT /api/v1/projects/{key}/page-codes":        core.PermPagesWrite,
	"DELETE /api/v1/pages/{id}":                    core.PermPagesDelete,
	"DELETE /api/v1/pages/{id}/codes/{code}":       core.PermPagesDelete,
	"GET /api/v1/glossary":                         core.PermGlossaryRead,
	"GET /api/v1/glossary/lookup":                  core.PermGlossaryRead,
	"GET /api/v1/glossary/{id}":                    core.PermGlossaryRead,
	"POST /api/v1/glossary":                        core.PermGlossaryWrite,
	"POST /api/v1/knowledge/shown":                 core.PermGlossaryRead,
	"PATCH /api/v1/glossary/{id}":                  core.PermGlossaryWrite,
	"DELETE /api/v1/glossary/{id}":                 core.PermGlossaryDelete,
	"GET /api/v1/design":                           core.PermDesignRead,
	"GET /api/v1/goals":                            core.PermDesignRead,
	"GET /api/v1/goals/gaps":                       core.PermDesignRead,
	"GET /api/v1/trace":                            core.PermDesignRead,
	"POST /api/v1/goals/links/{id}/confirm":        core.PermKnowledgeApprove,
	"POST /api/v1/goals/links/{id}/dismiss":        core.PermKnowledgeApprove,
	"POST /api/v1/goals/dismiss-match":             core.PermKnowledgeApprove,
	"POST /api/v1/goals/drift/{id}/answer":         core.PermKnowledgeApprove,
	"POST /api/v1/goals/links/acknowledge":         core.PermKnowledgeApprove,
	"POST /api/v1/goals/suggest":                   core.PermKnowledgeApprove,
	"GET /api/v1/goals/{id}/work":                  core.PermDesignRead,
	"POST /api/v1/issues/{key}/goal-suggest":       core.PermKnowledgeApprove,
	"GET /api/v1/design/review":                    core.PermDesignRead,
	"GET /api/v1/design/{id}":                      core.PermDesignRead,
	"GET /api/v1/spec":                             core.PermDesignRead,
	"POST /api/v1/design":                          core.PermDesignWrite,
	"PATCH /api/v1/design/{id}":                    core.PermDesignWrite,
	"POST /api/v1/design/{id}/additions":           core.PermDesignWrite,
	"DELETE /api/v1/design/{id}":                   core.PermDesignDelete,
	"POST /api/v1/design/{id}/approve":             core.PermKnowledgeApprove,
	"POST /api/v1/design/revisions/{rid}/{action}": core.PermKnowledgeApprove,
	"GET /api/v1/review":                           core.PermAgentsRead,
	"GET /api/v1/risk/{kind}/{ref}":                core.PermIssuesRead,
	"POST /api/v1/review/{type}/{ref}":             core.PermKnowledgeApprove,
	"POST /api/v1/review/closes":                   core.PermKnowledgeApprove,
	"GET /api/v1/catch-up":                         core.PermIssuesRead,
	"POST /api/v1/issues/{key}/try-links":          core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/not-finished":       core.PermIssuesEdit,
	"POST /api/v1/issues/{key}/problems":           core.PermIssuesCreate,
	"GET /api/v1/attachments":                      core.PermAttachmentsRead,
	"GET /api/v1/attachments/{id}/{name}":          core.PermAttachmentsRead,
	"POST /api/v1/attachments":                     core.PermAttachmentsWrite,
	"POST /api/v1/issues/{key}/attachments":        core.PermAttachmentsWrite,
	"PATCH /api/v1/attachments/{id}":               core.PermAttachmentsWrite,
	"DELETE /api/v1/attachments/{id}":              core.PermAttachmentsWrite,

	"GET /api/v1/waiting":                       core.PermRequestsRead,
	"GET /api/v1/requests":                      core.PermRequestsRead,
	"GET /api/v1/refusals":                      core.PermRequestsRead,
	"POST /api/v1/refusals/{id}/rule":           core.PermRequestsAnswer,
	"GET /api/v1/requests/{ref}":                core.PermRequestsRead,
	"POST /api/v1/requests":                     core.PermRequestsCreate,
	"POST /api/v1/requests/{ref}/withdraw":      core.PermRequestsCreate,
	"POST /api/v1/requests/{ref}/watch":         core.PermRequestsCreate,
	"POST /api/v1/requests/{ref}/decide":        permAlways,
	"POST /api/v1/requests/{ref}/snooze":        core.PermRequestsAnswer,
	"POST /api/v1/requests/{ref}/cells/{n}/run": core.PermRequestsRun,

	"POST /api/v1/projects/{key}/sprints":                core.PermSprintsManage,
	"PATCH /api/v1/sprints/{id}":                         core.PermSprintsManage,
	"DELETE /api/v1/sprints/{id}":                        core.PermProjectsManage,
	"POST /api/v1/sprints/{id}/start":                    core.PermSprintsManage,
	"POST /api/v1/sprints/{id}/close":                    core.PermSprintsManage,
	"POST /api/v1/projects":                              core.PermProjectsManage,
	"PATCH /api/v1/projects/{key}":                       core.PermProjectsManage,
	"DELETE /api/v1/projects/{key}":                      core.PermProjectsManage,
	"POST /api/v1/projects/{key}/look/regenerate":        core.PermProjectsManage,
	"POST /api/v1/projects/{key}/look/reset":             core.PermProjectsManage,
	"PUT /api/v1/projects/{key}/members/{user}":          core.PermProjectsManage,
	"DELETE /api/v1/projects/{key}/members/{user}":       core.PermProjectsManage,
	"POST /api/v1/projects/{key}/statuses":               core.PermProjectsManage,
	"PATCH /api/v1/statuses/{id}":                        core.PermProjectsManage,
	"DELETE /api/v1/statuses/{id}":                       core.PermProjectsManage,
	"POST /api/v1/projects/{key}/labels":                 core.PermProjectsManage,
	"PATCH /api/v1/labels/{id}":                          core.PermProjectsManage,
	"DELETE /api/v1/labels/{id}":                         core.PermProjectsManage,
	"POST /api/v1/projects/{key}/templates":              core.PermProjectsManage,
	"PATCH /api/v1/projects/{key}/templates/{id}":        core.PermProjectsManage,
	"DELETE /api/v1/projects/{key}/templates/{id}":       core.PermProjectsManage,
	"POST /api/v1/workflows":                             core.PermProjectsManage,
	"PATCH /api/v1/workflows/{id}":                       core.PermProjectsManage,
	"DELETE /api/v1/workflows/{id}":                      core.PermProjectsManage,
	"GET /api/v1/projects/{key}/repos":                   core.PermPullsRead,
	"POST /api/v1/projects/{key}/repos":                  core.PermReposManage,
	"PATCH /api/v1/projects/{key}/repos/{id}":            core.PermReposManage,
	"DELETE /api/v1/projects/{key}/repos/{id}":           core.PermReposManage,
	"POST /api/v1/projects/{key}/repos/{id}/sync":        core.PermReposManage,
	"POST /api/v1/projects/{key}/repos/{id}/hook":        core.PermReposManage,
	"POST /api/v1/projects/{key}/repos/{id}/move":        core.PermReposManage,
	"PUT /api/v1/projects/{key}/repos/{id}/code":         core.PermReposManage,
	"GET /api/v1/projects/{key}/repos/{id}/code":         core.PermPullsRead,
	"GET /api/v1/projects/{key}/webhooks":                core.PermWebhooksManage,
	"POST /api/v1/projects/{key}/webhooks":               core.PermWebhooksManage,
	"PATCH /api/v1/webhooks/{id}":                        core.PermWebhooksManage,
	"DELETE /api/v1/webhooks/{id}":                       core.PermWebhooksManage,
	"POST /api/v1/webhooks/{id}/test":                    core.PermWebhooksManage,
	"GET /api/v1/webhooks/{id}/deliveries":               core.PermWebhooksManage,
	"POST /api/v1/webhooks/{id}/deliveries/{did}/replay": core.PermWebhooksManage,

	"GET /api/v1/agents":                            core.PermAgentsRead,
	"GET /api/v1/agents/overview":                   core.PermAgentsRead,
	"GET /api/v1/agents/activity":                   core.PermAgentsRead,
	"GET /api/v1/agents/queue":                      core.PermAgentsRead,
	"GET /api/v1/agents/knowledge":                  core.PermAgentsRead,
	"GET /api/v1/agents/pauses":                     core.PermAgentsRead,
	"GET /api/v1/agents/session-places":             core.PermAgentsRead,
	"GET /api/v1/knowledge/usage":                   core.PermAgentsRead,
	"GET /api/v1/changes":                           core.PermAgentsRead,
	"POST /api/v1/agents/queue/order":               core.PermAgentsManage,
	"POST /api/v1/agents/release/{key}":             core.PermAgentsManage,
	"POST /api/v1/agents/pauses":                    core.PermAgentsManage,
	"DELETE /api/v1/agents/pauses":                  core.PermAgentsManage,
	"POST /api/v1/agents/start/{key}":               core.PermSessionsStart,
	"GET /api/v1/agents/kickoff/{key}":              core.PermIssuesRead,
	"GET /api/v1/autopilot":                         core.PermAgentsRead,
	"GET /api/v1/autopilot/activity":                core.PermAgentsRead,
	"GET /api/v1/autopilot/summaries":               core.PermAgentsRead,
	"GET /api/v1/autopilot/text":                    core.PermAgentsRead,
	"GET /api/v1/autopilot/review-models":           core.PermAgentsRead,
	"GET /api/v1/projects/{key}/autopilot-text":     core.PermAgentsRead,
	"GET /api/v1/stalled-jobs/policy":               core.PermAgentsRead,
	"GET /api/v1/projects/{key}/stalled-jobs":       core.PermAgentsRead,
	"PUT /api/v1/autopilot":                         core.PermSessionsStart,
	"PUT /api/v1/autopilot/projects/{key}":          core.PermSessionsStart,
	"POST /api/v1/autopilot/off":                    core.PermSessionsStart,
	"POST /api/v1/autopilot/steps/{id}/{action}":    core.PermSessionsStart,
	"POST /api/v1/autopilot/sessions/{id}/start":    core.PermSessionsStart,
	"POST /api/v1/autopilot/summaries/reviewed":     core.PermAgentsManage,
	"POST /api/v1/agent/next":                       core.PermTasksWork,
	"GET /api/v1/agent/next":                        core.PermTasksWork,
	"POST /api/v1/agent/batch":                      core.PermTasksWork,
	"GET /api/v1/agent/batch":                       core.PermTasksWork,
	"POST /api/v1/agent/done/{key}":                 core.PermTasksWork,
	"POST /api/v1/agent/release/{key}":              core.PermTasksWork,
	"POST /api/v1/agent/pushed":                     core.PermTasksWork,
	"POST /api/v1/agent/decline/{key}":              core.PermTasksWork,
	"POST /api/v1/agent/usage/{key}":                core.PermTasksWork,
	"POST /api/v1/agent/take/{key}":                 core.PermTasksWork,
	"POST /api/v1/agent/pause/{key}":                core.PermTasksWork,
	"POST /api/v1/agent/resume/{key}":               core.PermTasksWork,
	"POST /api/v1/agent/handoff":                    core.PermSessionsStart,
	"POST /api/v1/agent/handoff/release":            core.PermSessionsStart,
	"POST /api/v1/agent/handoff/outcome":            core.PermSessionsStart,
	"POST /api/v1/fanouts":                          core.PermSessionsStart,
	"GET /api/v1/fanouts":                           core.PermSessionsRead,
	"GET /api/v1/fanouts/{id}":                      core.PermSessionsRead,
	"POST /api/v1/fanouts/{id}/claim":               core.PermSessionsStart,
	"POST /api/v1/fanouts/{id}/stop":                core.PermSessionsStart,
	"POST /api/v1/fanouts/{id}/items/{key}/started": core.PermSessionsStart,
	"POST /api/v1/fanouts/{id}/items/{key}/failed":  core.PermSessionsStart,
	"POST /api/v1/fanouts/{id}/items/{key}/end":     core.PermSessionsStart,
	"GET /api/v1/sessions":                          core.PermSessionsRead,
	"GET /api/v1/sessions/current":                  core.PermSessionsRead,
	"POST /api/v1/sessions/outstanding":             core.PermSessionsReport,
	"POST /api/v1/sessions/outstanding/told":        core.PermSessionsReport,
	"GET /api/v1/sessions/{id}":                     core.PermSessionsRead,
	"GET /api/v1/sessions/{id}/usage":               core.PermSessionsRead,
	"GET /api/v1/sessions/{id}/snapshots":           core.PermSessionsRead,
	"GET /api/v1/sessions/{id}/observed":            core.PermSessionsRead,
	"GET /api/v1/sessions/{id}/follow-up":           core.PermSessionsRead,
	"POST /api/v1/sessions/hook":                    core.PermSessionsReport,
	"POST /api/v1/sessions/hook/await":              core.PermSessionsReport,
	"POST /api/v1/sessions/hook/ship-news":          core.PermSessionsReport,
	"POST /api/v1/sessions/typed-start":             core.PermSessionsReport,
	"GET /api/v1/sessions/typed-prompts":            core.PermSessionsReport,
	"GET /api/v1/sessions/pause":                    core.PermSessionsReport,
	"POST /api/v1/sessions/pulse":                   core.PermSessionsReport,
	"POST /api/v1/usage/ingest":                     core.PermSessionsReport,
	"POST /api/v1/usage/transcript":                 core.PermSessionsReport,
	"POST /api/v1/usage/transcript/status":          core.PermSessionsReport,
	"GET /api/v1/usage/transcripts":                 core.PermUsageRead,
	"GET /api/v1/usage/facts":                       core.PermUsageRead,
	"GET /api/v1/usage/transcript":                  core.PermUsageRead,
	"GET /api/v1/usage/machines":                    core.PermUsageRead,
	"POST /api/v1/slot-holds":                       core.PermSessionsReport,
	"GET /api/v1/slot-holds":                        core.PermUsageRead,
	"POST /api/v1/usage/quota":                      core.PermSessionsReport,
	"POST /api/v1/otlp/v1/logs":                     core.PermSessionsReport,
	"POST /api/v1/otlp/v1/metrics":                  core.PermSessionsReport,
	"PUT /api/v1/sessions/{id}/follow-up":           core.PermSessionsStart,
	"DELETE /api/v1/sessions/{id}/follow-up":        core.PermSessionsStart,
	"POST /api/v1/sessions/{id}/follow-up/run":      core.PermSessionsStart,
	"GET /api/v1/runners":                           core.PermRunnersRead,
	"GET /api/v1/runners/{id}":                      core.PermRunnersRead,
	"GET /api/v1/runner-jobs":                       core.PermRunnersRead,
	"GET /api/v1/runner-jobs/{id}":                  core.PermRunnersRead,
	"POST /api/v1/runner-jobs":                      core.PermRunnersRun,
	"POST /api/v1/runner-jobs/{id}/cancel":          core.PermRunnersRun,
	"POST /api/v1/runner/register":                  core.PermRunnersHost,
	"POST /api/v1/runner/poll":                      core.PermRunnersHost,
	"GET /api/v1/autopilot/steps/{id}/wanted":       core.PermRunnersHost,
	"POST /api/v1/autopilot/steps/{id}/typed":       core.PermRunnersHost,
	"POST /api/v1/runner/jobs/{id}/log":             core.PermRunnersHost,
	"POST /api/v1/runner/jobs/{id}/finish":          core.PermRunnersHost,
	"POST /api/v1/runner/jobs/{id}/cancel":          core.PermRunnersHost,
	"POST /api/v1/runner/stalled-jobs":              core.PermRunnersHost,
	"POST /api/v1/runner/jobs/{id}/gateway":         core.PermRunnersHost,
	"GET /api/v1/runner/jobs/{id}/gateway":          core.PermRunnersHost,
	"POST /api/v1/runner/jobs/{id}/gateway/cancel":  core.PermRunnersHost,
	"DELETE /api/v1/runners/{id}":                   core.PermRunnersHost,
	"POST /api/v1/ci/report":                        core.PermCIReport,
	"GET /api/v1/ci/usage":                          core.PermCIReport,
	"GET /api/v1/kit":                               core.PermAgentsRead,
	"GET /api/v1/kit/resolve":                       core.PermSessionsReport,
	"POST /api/v1/kit/reports":                      core.PermSessionsReport,
	"GET /api/v1/kit/checkouts":                     core.PermAgentsRead,
	"POST /api/v1/kit/rollout":                      core.PermRunnersRun,
	"POST /api/v1/kit/add":                          core.PermPullsWrite,
	"POST /api/v1/kit/read":                         core.PermAdmin,

	"GET /api/v1/instructions":              core.PermAgentsRead,
	"GET /api/v1/instructions/preview":      core.PermAgentsRead,
	"GET /api/v1/instructions/{id}":         core.PermAgentsRead,
	"POST /api/v1/instructions":             core.PermDesignWrite,
	"PATCH /api/v1/instructions/{id}":       core.PermDesignWrite,
	"POST /api/v1/instructions/{id}/review": core.PermAgentsManage,
	"DELETE /api/v1/instructions/{id}":      core.PermAgentsManage,
	"GET /api/v1/agent-setup":               core.PermAgentsRead,
	"GET /api/v1/agent-setup/file":          core.PermAgentsRead,
	"GET /api/v1/agent-setup/machine-file":  core.PermAgentsRead,
	"POST /api/v1/agent-setup/refresh":      core.PermAgentsManage,
	"POST /api/v1/agent-setup/machines":     core.PermSessionsReport,
	"PUT /api/v1/agent-setup/file":          core.PermPullsWrite,
	"PUT /api/v1/agent-setup/shared":        core.PermAdmin,
	"POST /api/v1/agent-setup/publish":      core.PermAdmin,
	"GET /api/v1/admin/kit":                 core.PermAdmin,
	"PUT /api/v1/admin/kit":                 core.PermAdmin,

	"GET /api/v1/pulls":                                           core.PermPullsRead,
	"GET /api/v1/ci":                                              core.PermPullsRead,
	"GET /api/v1/ci/log":                                          core.PermPullsRead,
	"GET /api/v1/ci/doctor":                                       core.PermPullsRead,
	"GET /api/v1/ci/history":                                      core.PermPullsRead,
	"GET /api/v1/ci/line":                                         core.PermPullsRead,
	"GET /api/v1/ci/tests":                                        core.PermPullsRead,
	"GET /api/v1/ci/coverage":                                     core.PermPullsRead,
	"GET /api/v1/ci/suppressions":                                 core.PermPullsRead,
	"GET /api/v1/pulls/{id}":                                      core.PermPullsRead,
	"GET /api/v1/pulls/{id}/readiness":                            core.PermPullsRead,
	"GET /api/v1/pulls/{id}/failed-checks":                        core.PermPullsRead,
	"GET /api/v1/pulls/{id}/logs":                                 core.PermPullsRead,
	"GET /api/v1/pulls/{id}/wait":                                 core.PermPullsRead,
	"GET /api/v1/pulls/{id}/babysit":                              core.PermPullsRead,
	"GET /api/v1/pulls/{id}/todo":                                 core.PermPullsRead,
	"GET /api/v1/babysit/rounds":                                  core.PermPullsRead,
	"GET /api/v1/projects/{key}/pulls/{number}":                   core.PermPullsRead,
	"GET /api/v1/branches":                                        core.PermPullsRead,
	"GET /api/v1/branches/tidy":                                   core.PermPullsRead,
	"POST /api/v1/projects/{key}/pulls":                           core.PermPullsWrite,
	"POST /api/v1/pulls/{id}/check":                               core.PermPullsWrite,
	"POST /api/v1/findings/{id}/dismiss":                          core.PermPullsWrite,
	"POST /api/v1/findings/{id}/reopen":                           core.PermPullsWrite,
	"POST /api/v1/pulls/{id}/babysit":                             core.PermPullsWrite,
	"DELETE /api/v1/pulls/{id}/babysit":                           core.PermPullsWrite,
	"PATCH /api/v1/pulls/{id}":                                    core.PermPullsWrite,
	"POST /api/v1/pulls/{id}/urgent":                              core.PermPullsWrite,
	"DELETE /api/v1/pulls/{id}/urgent":                            core.PermPullsWrite,
	"POST /api/v1/pulls/{id}/after/{number}":                      core.PermPullsWrite,
	"DELETE /api/v1/pulls/{id}/after/{number}":                    core.PermPullsWrite,
	"POST /api/v1/review-comments/ack":                            core.PermPullsWrite,
	"POST /api/v1/review-comments/fixed":                          core.PermPullsWrite,
	"POST /api/v1/review-comments/{id}/dismiss":                   core.PermPullsWrite,
	"POST /api/v1/review-comments/{id}/reply":                     core.PermPullsWrite,
	"POST /api/v1/review-comments/{id}/react":                     core.PermPullsWrite,
	"POST /api/v1/review-comments/resolve":                        core.PermPullsWrite,
	"POST /api/v1/review-comments/unresolve":                      core.PermPullsWrite,
	"GET /api/v1/pulls/{id}/comments":                             core.PermPullsRead,
	"GET /api/v1/audits":                                          core.PermPullsRead,
	"GET /api/v1/audits/defaults":                                 core.PermPullsRead,
	"GET /api/v1/audits/nitpicks":                                 core.PermPullsRead,
	"GET /api/v1/audits/{id}":                                     core.PermPullsRead,
	"POST /api/v1/audits":                                         core.PermAuditsRun,
	"POST /api/v1/audits/findings":                                permAlways,
	"POST /api/v1/audits/{id}/rerun":                              core.PermAuditsRun,
	"POST /api/v1/audits/{id}/cancel":                             core.PermAuditsRun,
	"POST /api/v1/audits/{id}/fix":                                core.PermAuditsRun,
	"POST /api/v1/pulls/{id}/merge":                               core.PermPullsMerge,
	"DELETE /api/v1/pulls/{id}/merge":                             core.PermPullsMerge,
	"GET /api/v1/environments":                                    core.PermEnvironmentsRead,
	"GET /api/v1/environments/summary":                            core.PermEnvironmentsRead,
	"GET /api/v1/delivery":                                        core.PermEnvironmentsRead,
	"GET /api/v1/environments/at":                                 core.PermEnvironmentsRead,
	"GET /api/v1/environments/{id}":                               core.PermEnvironmentsRead,
	"GET /api/v1/environments/{id}/deployments":                   core.PermEnvironmentsRead,
	"GET /api/v1/projects/{key}/environments":                     core.PermEnvironmentsRead,
	"GET /api/v1/projects/{key}/environments/{name}":              core.PermEnvironmentsRead,
	"PATCH /api/v1/environments/{id}":                             core.PermEnvironmentsWrite,
	"DELETE /api/v1/environments/{id}":                            core.PermEnvironmentsWrite,
	"POST /api/v1/environments/{id}/stop":                         core.PermEnvironmentsWrite,
	"POST /api/v1/environments/{id}/check":                        core.PermEnvironmentsWrite,
	"POST /api/v1/environments/{id}/deployments":                  core.PermEnvironmentsWrite,
	"POST /api/v1/projects/{key}/environments":                    core.PermEnvironmentsWrite,
	"POST /api/v1/projects/{key}/environments/{name}/deployments": core.PermEnvironmentsWrite,
	"POST /api/v1/projects/{key}/environments/{name}/up":          core.PermEnvironmentsWrite,
	"POST /api/v1/projects/{key}/environments/{name}/down":        core.PermEnvironmentsWrite,
	"POST /api/v1/projects/{key}/environments/{name}/vouch":       core.PermEnvironmentsRead,
	"GET /api/v1/projects/{key}/automations":                      core.PermAutomationsRead,
	"GET /api/v1/automations/{id}":                                core.PermAutomationsRead,
	"GET /api/v1/automations/{id}/runs":                           core.PermAutomationsRead,
	"GET /api/v1/automation-runs/{id}":                            core.PermAutomationsRead,
	"GET /api/v1/automation-catalog":                              core.PermAutomationsRead,
	"GET /api/v1/automation-cron":                                 core.PermAutomationsRead,
	"GET /api/v1/automation-templates":                            core.PermAutomationsRead,
	"GET /api/v1/projects/{key}/bot-runs":                         core.PermAutomationsRead,
	"POST /api/v1/projects/{key}/automations":                     core.PermAutomationsManage,
	"PATCH /api/v1/automations/{id}":                              core.PermAutomationsManage,
	"DELETE /api/v1/automations/{id}":                             core.PermAutomationsManage,
	"POST /api/v1/automations/{id}/run":                           core.PermAutomationsManage,
	"POST /api/v1/automations/{id}/test":                          core.PermAutomationsManage,
	"POST /api/v1/automations/{id}/webhook-address":               core.PermAutomationsManage,
	"POST /api/v1/automation-runs/{id}/cancel":                    core.PermAutomationsManage,
	"POST /api/v1/automation-runs/{id}/rerun":                     core.PermAutomationsManage,
	"POST /api/v1/bot-runs/{id}/cancel":                           core.PermAutomationsManage,
	"POST /api/v1/bot-runs/{id}/retry":                            core.PermAutomationsManage,
	"POST /api/v1/issues/{key}/ai/estimate":                       core.PermAIUse,
	"POST /api/v1/issues/{key}/ai/subtasks":                       core.PermAIUse,
	"POST /api/v1/projects/{key}/ai/estimate":                     core.PermAIUse,
	"GET /api/v1/issues/{key}/prep":                               core.PermIssuesRead,
	"POST /api/v1/issues/{key}/prep":                              core.PermAIUse,
	"POST /api/v1/issues/{key}/arms":                              core.PermIssuesRead,
	"GET /api/v1/prep/due":                                        core.PermRunnersRun,
	"GET /api/v1/escapes/due":                                     core.PermRunnersRun,
	"POST /api/v1/escapes/traces":                                 core.PermRunnersRun,
	"GET /api/v1/escapes/proofs/due":                              core.PermRunnersRun,
	"POST /api/v1/escapes/proofs":                                 core.PermRunnersRun,
	"GET /api/v1/escapes/stats":                                   core.PermIssuesRead,
	"GET /api/v1/escapes/files":                                   core.PermIssuesRead,
	"GET /api/v1/escapes":                                         core.PermIssuesRead,
	"GET /api/v1/ai/tasks":                                        core.PermAIManage,
	"PUT /api/v1/ai/tasks/{task}":                                 core.PermAIManage,
	"GET /api/v1/ai/safety-drill":                                 core.PermAIManage,
	"GET /api/v1/server/status":                                   core.PermServerRead,
	"GET /api/v1/server/log":                                      core.PermServerRead,
	"GET /api/v1/server/metrics":                                  core.PermServerRead,
	"GET /api/v1/server/problems":                                 core.PermServerRead,
	"GET /api/v1/server/jobs/{name}":                              core.PermServerRead,
	"POST /api/v1/server/problems/{id}/ack":                       core.PermAdmin,
	"DELETE /api/v1/server/problems/{id}/ack":                     core.PermAdmin,
	"GET /api/v1/ai/usage":                                        core.PermServerRead,
	"GET /api/v1/ai/surplus":                                      core.PermServerRead,
	"POST /api/v1/ai/safety-drill":                                core.PermAIManage,
	"GET /api/v1/ai/review-arms":                                  core.PermAIManage,
	"PUT /api/v1/ai/review-arms/pins":                             core.PermAIManage,
	"GET /api/v1/experiments":                                     core.PermAIManage,
	"POST /api/v1/experiments/{key}/start":                        core.PermAIManage,
	"POST /api/v1/experiments/{key}/stop":                         core.PermAIManage,
	"GET /api/v1/experiments/{key}/results":                       core.PermAIManage,
	"GET /api/v1/reviews/stats":                                   core.PermPullsRead,
	"GET /api/v1/mistakes":                                        core.PermPullsRead,
	"GET /api/v1/ai/providers":                                    core.PermAIManage,
	"POST /api/v1/ai/providers":                                   core.PermAIManage,
	"PATCH /api/v1/ai/providers/{id}":                             core.PermAIManage,
	"DELETE /api/v1/ai/providers/{id}":                            core.PermAIManage,
	"GET /api/v1/ai/providers/{id}/models":                        core.PermAIManage,

	"GET /api/v1/usage/overview":                 core.PermUsageRead,
	"GET /api/v1/usage/ledger":                   core.PermUsageRead,
	"GET /api/v1/usage/trends":                   core.PermUsageRead,
	"GET /api/v1/usage/compare":                  core.PermUsageRead,
	"GET /api/v1/usage/cli":                      core.PermUsageRead,
	"GET /api/v1/usage/feature":                  core.PermUsageRead,
	"GET /api/v1/usage/task/{key}":               core.PermUsageRead,
	"GET /api/v1/usage/no-task":                  core.PermUsageRead,
	"GET /api/v1/usage/summary":                  core.PermUsageRead,
	"GET /api/v1/usage/breakdown":                core.PermUsageRead,
	"GET /api/v1/usage/refusals":                 core.PermUsageRead,
	"GET /api/v1/usage/risky":                    core.PermUsageRead,
	"GET /api/v1/usage/warm":                     core.PermUsageRead,
	"GET /api/v1/usage/delegation":               core.PermUsageRead,
	"GET /api/v1/usage/goal-context":             core.PermUsageRead,
	"GET /api/v1/usage/meaning":                  core.PermUsageRead,
	"GET /api/v1/usage/stalled":                  core.PermUsageRead,
	"GET /api/v1/usage/guard":                    core.PermUsageRead,
	"GET /api/v1/usage/guard/live":               core.PermUsageRead,
	"PUT /api/v1/usage/guard/settings":           core.PermSessionsStart,
	"POST /api/v1/usage/guard/lift":              core.PermSessionsStart,
	"GET /api/v1/usage/prices":                   core.PermUsageRead,
	"PUT /api/v1/usage/prices":                   core.PermAdmin,
	"DELETE /api/v1/usage/prices":                core.PermAdmin,
	"GET /api/v1/audit":                          core.PermAuditRead,
	"GET /api/v1/audit.csv":                      core.PermAuditRead,
	"GET /api/v1/forges":                         core.PermForgesManage,
	"POST /api/v1/forges":                        core.PermForgesManage,
	"PATCH /api/v1/forges/{id}":                  core.PermForgesManage,
	"DELETE /api/v1/forges/{id}":                 core.PermForgesManage,
	"GET /api/v1/forges/{id}/secret":             core.PermForgesManage,
	"PUT /api/v1/forges/{id}/people":             permNever,
	"GET /api/v1/forges/{id}/discover":           core.PermForgesManage,
	"POST /api/v1/forges/{id}/discover":          core.PermForgesManage,
	"GET /api/v1/forges/{id}/orgs":               core.PermForgesManage,
	"GET /api/v1/forges/{id}/org-hooks":          core.PermForgesManage,
	"POST /api/v1/forges/{id}/org-hooks":         core.PermForgesManage,
	"DELETE /api/v1/forges/{id}/org-hooks/{hid}": core.PermForgesManage,
	"GET /api/v1/admin/users":                    core.PermAdmin,
	"POST /api/v1/admin/users":                   core.PermAdmin,
	"PATCH /api/v1/admin/users/{id}":             core.PermAdmin,
	"DELETE /api/v1/admin/users/{id}":            core.PermAdmin,
	"PUT /api/v1/admin/users/{id}/avatar":        core.PermAdmin,
	"POST /api/v1/admin/users/{id}/reset-link":   core.PermAdmin,
	"GET /api/v1/admin/invites":                  core.PermAdmin,
	"POST /api/v1/admin/invites":                 core.PermAdmin,
	"DELETE /api/v1/admin/invites/{id}":          core.PermAdmin,
	"GET /api/v1/admin/webhooks":                 core.PermAdmin,
	"POST /api/v1/admin/webhooks":                core.PermAdmin,
	"GET /api/v1/admin/review-note":              core.PermAdmin,
	"PUT /api/v1/ci/pools/{forge}":               core.PermAdmin,
	"PUT /api/v1/admin/review-note":              core.PermAdmin,
	"GET /api/v1/admin/ship-reply":               core.PermAdmin,
	"PUT /api/v1/admin/ship-reply":               core.PermAdmin,
	"GET /api/v1/admin/pr-bodies":                core.PermAdmin,
	"PUT /api/v1/admin/pr-bodies":                core.PermAdmin,
	"PUT /api/v1/admin/autopilot-text":           core.PermAdmin,
	"PUT /api/v1/admin/stalled-jobs":             core.PermAdmin,
	"GET /api/v1/admin/email":                    core.PermAdmin,
	"PUT /api/v1/admin/email":                    core.PermAdmin,
	"POST /api/v1/admin/email/test":              core.PermAdmin,
	"GET /api/v1/admin/automations":              core.PermAdmin,
	"POST /api/v1/admin/automations":             core.PermAdmin,
	"GET /api/v1/admin/system":                   core.PermAdmin,
	"PUT /api/v1/admin/system-limits":            core.PermAdmin,
}

// checkPermission refuses a permission token a route it lacks the
// permission for, and records the refusal for the token's owner.
func (s *Server) checkPermission(r *http.Request, a *core.Actor) error {
	if !a.Restricted() {
		return nil
	}
	need, ok := routePermissions[r.Pattern]
	switch {
	case need == permAlways || need == permMCP:
		return nil
	case !ok || need == permNever:
		return fmt.Errorf("a token with permissions can't use %s: %w", r.Pattern, core.ErrForbidden)
	case a.Allows(need):
		return nil
	}
	err := &core.PermissionError{Permission: need}
	s.svc.RecordRefusal(r.Context(), a, need, r.Pattern)
	return err
}

// mcpToolPermissions is what each MCP tool needs from a permission token.
// A tool missing here is hidden from and refused to permission tokens.
var mcpToolPermissions = map[string]core.Permission{
	"query_help":        permAlways,
	"search_issues":     core.PermIssuesRead,
	"get_issue":         core.PermIssuesRead,
	"list_projects":     core.PermIssuesRead,
	"get_project":       core.PermIssuesRead,
	"get_board":         core.PermIssuesRead,
	"forecast":          core.PermIssuesRead,
	"create_issue":      core.PermIssuesCreate,
	"update_issue":      core.PermIssuesEdit,
	"transition_issue":  core.PermIssuesEdit,
	"add_comment":       core.PermIssuesComment,
	"log_time":          core.PermTimeTrack,
	"timer":             core.PermTimeTrack,
	"next_task":         core.PermTasksWork,
	"batch_tasks":       core.PermTasksWork,
	"take_task":         core.PermTasksWork,
	"finish_task":       core.PermTasksWork,
	"release_task":      core.PermTasksWork,
	"decline_task":      core.PermTasksWork,
	"hold_task":         core.PermIssuesEdit,
	"search":            core.PermSearch,
	"list_pages":        core.PermPagesRead,
	"get_page":          core.PermPagesRead,
	"save_page":         core.PermPagesWrite,
	"glossary_lookup":   core.PermGlossaryRead,
	"list_glossary":     core.PermGlossaryRead,
	"save_glossary":     core.PermGlossaryWrite,
	"get_spec":          core.PermDesignRead,
	"goals":             core.PermDesignRead,
	"trace":             core.PermDesignRead,
	"list_design":       core.PermDesignRead,
	"save_design":       core.PermDesignWrite,
	"add_to_design":     core.PermDesignWrite,
	"link":              core.PermLinksWrite,
	"attach_file":       core.PermAttachmentsWrite,
	"create_request":    core.PermRequestsCreate,
	"list_requests":     core.PermRequestsRead,
	"get_request":       core.PermRequestsRead,
	"list_environments": core.PermEnvironmentsRead,
	"record_deployment": core.PermEnvironmentsWrite,
	"list_automations":  core.PermAutomationsRead,
	"run_automation":    core.PermAutomationsManage,
}

// mcpToolNeeds is what calling tool with args needs beyond the tool's own
// permission: a comment saved with an edit needs the comment permission. The arguments
// are read the way mcpCall reads them; unreadable ones need permNever.
func mcpToolNeeds(tool string, args json.RawMessage) []core.Permission {
	need, ok := mcpToolPermissions[tool]
	if !ok {
		return []core.Permission{permNever}
	}
	out := []core.Permission{need}
	a := mcpArgs{}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return []core.Permission{permNever}
	}
	if tool == "update_issue" || tool == "transition_issue" {
		if c, ok := a["comment"]; ok && c != nil && c != "" {
			out = append(out, core.PermIssuesComment)
		}
	}
	return out
}

// checkTool refuses a permission token an MCP tool it may not use.
func (s *Server) checkTool(ctx context.Context, a *core.Actor, tool string, args json.RawMessage) error {
	if !a.Restricted() {
		return nil
	}
	for _, need := range mcpToolNeeds(tool, args) {
		switch {
		case need == permAlways || a.Allows(need):
		case need == permNever:
			return fmt.Errorf("a token with permissions can't use the %s tool: %w", tool, core.ErrForbidden)
		default:
			s.svc.RecordRefusal(ctx, a, need, "MCP tool "+tool)
			return &core.PermissionError{Permission: need}
		}
	}
	return nil
}

// toolsFor is the MCP tool list an actor sees: a permission token sees only
// the tools it may call.
func toolsFor(a *core.Actor) []mcpTool {
	if !a.Restricted() {
		return mcpTools
	}
	out := []mcpTool{}
	for _, t := range mcpTools {
		need, ok := mcpToolPermissions[t.Name]
		if ok && (need == permAlways || a.Allows(need)) {
			out = append(out, t)
		}
	}
	return out
}

// tokenPermissions is the catalog the token settings page edits.
func (s *Server) tokenPermissions(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Permissions(), nil
}

func (s *Server) updateToken(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in map[string]json.RawMessage
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	var p core.TokenPatch
	for k, v := range in {
		switch k {
		case "name":
			if err := json.Unmarshal(v, &p.Name); err != nil {
				return nil, fmt.Errorf("%w: name must be a string", errBadRequest)
			}
		case "read_only":
			if err := json.Unmarshal(v, &p.ReadOnly); err != nil {
				return nil, fmt.Errorf("%w: read_only must be true or false", errBadRequest)
			}
		case "permissions":
			p.SetPermissions = true
			if err := json.Unmarshal(v, &p.Permissions); err != nil {
				return nil, fmt.Errorf("%w: permissions must be a list of permission names, or null", errBadRequest)
			}
		default:
			return nil, fmt.Errorf("%w: unknown field %q", errBadRequest, k)
		}
	}
	return s.svc.UpdateAPIToken(r.Context(), actorFrom(r), id, p)
}

func (s *Server) clearTokenRefusals(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.ClearTokenRefusals(r.Context(), actorFrom(r), id)
}

// tokenInfo is what /me says about a permission token, so a client knows
// what it may do before it tries.
type tokenInfo struct {
	Permissions []core.Permission `json:"permissions"`
}

func tokenInfoFor(a *core.Actor) *tokenInfo {
	if !a.Restricted() {
		return nil
	}
	ps := a.Permissions.List()
	slices.Sort(ps)
	return &tokenInfo{Permissions: ps}
}
