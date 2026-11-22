package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func (s *Server) routes(mux *http.ServeMux) {
	a, p := s.authed, s.public

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.healthz)
	mux.Handle("GET /metrics", s.metrics())
	mux.HandleFunc("GET /cal/{file}", s.calendar)
	mux.HandleFunc("POST /api/v1/hooks/forge/{id}", s.forgeHook)
	mux.HandleFunc("POST /mcp", s.mcp)
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP server is stateless; POST JSON-RPC requests", http.StatusMethodNotAllowed)
	})

	// Auth (public).
	mux.HandleFunc("POST /api/v1/auth/login", p(s.login))
	mux.HandleFunc("POST /api/v1/auth/logout", p(s.logout))
	mux.HandleFunc("GET /api/v1/auth/link/{token}", p(s.getLink))
	mux.HandleFunc("POST /api/v1/auth/invite/{token}", p(s.acceptInvite))
	mux.HandleFunc("POST /api/v1/auth/reset/{token}", p(s.resetPassword))
	mux.HandleFunc("GET /api/v1/help/query", s.queryHelp)
	mux.HandleFunc("GET /api/v1/help", s.apiHelp)
	mux.HandleFunc("GET /api/v1", s.apiHelp)

	// Me.
	mux.HandleFunc("GET /api/v1/me", a(s.me))
	mux.HandleFunc("PATCH /api/v1/me", a(s.updateMe))
	mux.HandleFunc("POST /api/v1/me/password", a(s.changePassword))
	mux.HandleFunc("PUT /api/v1/me/avatar", a(s.setMyAvatar))
	mux.HandleFunc("DELETE /api/v1/me/avatar", a(s.deleteMyAvatar))
	mux.HandleFunc("GET /api/v1/me/tokens", a(s.listTokens))
	mux.HandleFunc("POST /api/v1/me/tokens", a(s.createToken))
	mux.HandleFunc("PATCH /api/v1/me/tokens/{id}", a(s.updateToken))
	mux.HandleFunc("DELETE /api/v1/me/tokens/{id}", a(s.revokeToken))
	mux.HandleFunc("DELETE /api/v1/me/tokens/{id}/refusals", a(s.clearTokenRefusals))
	mux.HandleFunc("GET /api/v1/me/token-permissions", a(s.tokenPermissions))
	mux.HandleFunc("GET /api/v1/me/calendar", a(s.calendarInfo))
	mux.HandleFunc("POST /api/v1/me/calendar/rotate", a(s.rotateCalendar))
	mux.HandleFunc("POST /api/v1/me/gotify/test", a(s.testGotify))
	mux.HandleFunc("GET /api/v1/me/push", a(s.pushInfo))
	mux.HandleFunc("POST /api/v1/me/push", a(s.pushSubscribe))
	mux.HandleFunc("DELETE /api/v1/me/push", a(s.pushUnsubscribe))
	mux.HandleFunc("GET /api/v1/me/sessions", a(s.listSessions))
	mux.HandleFunc("DELETE /api/v1/me/sessions/{id}", a(s.revokeSession))

	// Users.
	mux.HandleFunc("GET /api/v1/users", a(s.listUsers))
	mux.HandleFunc("GET /api/v1/users/{id}/avatar", s.avatar)

	// Admin.
	mux.HandleFunc("GET /api/v1/admin/users", a(s.adminUsers))
	mux.HandleFunc("POST /api/v1/admin/users", a(s.adminCreateUser))
	mux.HandleFunc("PATCH /api/v1/admin/users/{id}", a(s.adminUpdateUser))
	mux.HandleFunc("DELETE /api/v1/admin/users/{id}", a(s.adminDeleteUser))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}/avatar", a(s.adminSetAvatar))
	mux.HandleFunc("POST /api/v1/admin/users/{id}/reset-link", a(s.adminResetLink))
	mux.HandleFunc("GET /api/v1/admin/invites", a(s.listInvites))
	mux.HandleFunc("POST /api/v1/admin/invites", a(s.createInvite))
	mux.HandleFunc("DELETE /api/v1/admin/invites/{id}", a(s.revokeInvite))
	mux.HandleFunc("GET /api/v1/admin/webhooks", a(s.listGlobalWebhooks))
	mux.HandleFunc("POST /api/v1/admin/webhooks", a(s.createGlobalWebhook))

	// The Agents page (WP-D15).
	s.agentsHubRoutes(mux)
	s.sessionRoutes(mux) // agent sessions and requests (WP-D16)
	s.requestRoutes(mux)
	// Environments and deployments (WP-D19).
	s.environmentRoutes(mux)
	s.shippingRoutes(mux)
	s.babysitRoutes(mux)
	s.codeAuditRoutes(mux)
	s.ciBoardRoutes(mux)    // the CI board (WP-225)
	s.serverDiagRoutes(mux) // production diagnostics (WP-375)
	s.runnersRoutes(mux)
	s.sandboxRoutes(mux) // sandbox policy, person-only writes (WP-418)
	s.automationRoutes(mux)
	s.assistantRoutes(mux)  // a person's own Hermes agent (WP-ADR1)
	s.kitRoutes(mux)        // the agent kit (WP-ADR2)
	s.agentSetupRoutes(mux) // agent setup (WP-ADR44)
	s.autopilotRoutes(mux)  // autopilot and task summaries (WP-ADR9)

	// Forges.
	// Agents (WP-D7).
	mux.HandleFunc("GET /api/v1/agent", a(s.agentIndex))
	mux.HandleFunc("GET /api/v1/agent/help/{topic}", a(s.agentHelpTopic))
	mux.HandleFunc("POST /api/v1/agent/next", a(s.agentNext))
	mux.HandleFunc("GET /api/v1/agent/next", a(s.agentNext)) // peek
	mux.HandleFunc("POST /api/v1/agent/batch", a(s.agentBatch))
	mux.HandleFunc("GET /api/v1/agent/batch", a(s.agentBatch)) // peek
	mux.HandleFunc("POST /api/v1/agent/done/{key}", a(s.agentDone))
	mux.HandleFunc("POST /api/v1/agent/handoff", a(s.agentHandoff))
	mux.HandleFunc("POST /api/v1/agent/handoff/release", a(s.agentHandoffRelease))
	mux.HandleFunc("POST /api/v1/agent/handoff/outcome", a(s.agentHandoffOutcome))
	mux.HandleFunc("POST /api/v1/fanouts", a(s.createFanout)) // WP-148
	mux.HandleFunc("GET /api/v1/fanouts", a(s.listFanouts))
	mux.HandleFunc("GET /api/v1/fanouts/{id}", a(s.getFanout))
	mux.HandleFunc("POST /api/v1/fanouts/{id}/claim", a(s.claimFanout))
	mux.HandleFunc("POST /api/v1/fanouts/{id}/stop", a(s.stopFanout))
	mux.HandleFunc("POST /api/v1/fanouts/{id}/items/{key}/started", a(s.fanoutItemStarted))
	mux.HandleFunc("POST /api/v1/fanouts/{id}/items/{key}/failed", a(s.fanoutItemFailed))
	mux.HandleFunc("POST /api/v1/fanouts/{id}/items/{key}/end", a(s.endFanoutItem))
	mux.HandleFunc("POST /api/v1/agent/release/{key}", a(s.agentRelease))
	mux.HandleFunc("POST /api/v1/agent/pushed", a(s.agentPushed)) // WP-843
	mux.HandleFunc("POST /api/v1/agent/decline/{key}", a(s.agentDecline))
	mux.HandleFunc("POST /api/v1/agent/usage/{key}", a(s.agentUsage))
	mux.HandleFunc("POST /api/v1/agent/take/{key}", a(s.agentTakeKey))
	mux.HandleFunc("POST /api/v1/agent/pause/{key}", a(s.agentPause))
	mux.HandleFunc("POST /api/v1/agent/resume/{key}", a(s.agentResume))

	// Glossary, design notes, specs, links, search everything (WP-D10).
	mux.HandleFunc("GET /api/v1/instructions", a(s.listInstructions))
	mux.HandleFunc("GET /api/v1/instructions/preview", a(s.previewInstructions))
	mux.HandleFunc("POST /api/v1/instructions", a(s.createInstruction))
	mux.HandleFunc("GET /api/v1/instructions/{id}", a(s.getInstruction))
	mux.HandleFunc("PATCH /api/v1/instructions/{id}", a(s.updateInstruction))
	mux.HandleFunc("POST /api/v1/instructions/{id}/review", a(s.reviewInstruction))
	mux.HandleFunc("DELETE /api/v1/instructions/{id}", a(s.deleteInstruction))
	mux.HandleFunc("GET /api/v1/glossary", a(s.listGlossary))
	mux.HandleFunc("GET /api/v1/glossary/lookup", a(s.lookupGlossary))
	mux.HandleFunc("POST /api/v1/glossary", a(s.createGlossary))
	mux.HandleFunc("POST /api/v1/knowledge/shown", a(s.noteShown))
	mux.HandleFunc("GET /api/v1/glossary/{id}", a(s.getGlossary))
	mux.HandleFunc("PATCH /api/v1/glossary/{id}", a(s.updateGlossary))
	mux.HandleFunc("DELETE /api/v1/glossary/{id}", a(s.deleteGlossary))
	mux.HandleFunc("GET /api/v1/design", a(s.listDesign))
	mux.HandleFunc("POST /api/v1/design", a(s.createDesign))
	mux.HandleFunc("GET /api/v1/design/review", a(s.designReview))
	mux.HandleFunc("GET /api/v1/design/{id}", a(s.getDesign))
	mux.HandleFunc("PATCH /api/v1/design/{id}", a(s.updateDesign))
	mux.HandleFunc("DELETE /api/v1/design/{id}", a(s.deleteDesign))
	mux.HandleFunc("POST /api/v1/design/{id}/additions", a(s.addDesignAddition))
	mux.HandleFunc("POST /api/v1/design/{id}/approve", a(s.approveDesign))
	mux.HandleFunc("POST /api/v1/design/revisions/{rid}/{action}", a(s.reviewDesign))
	mux.HandleFunc("GET /api/v1/spec", a(s.spec))
	// Goals and the trace to them (WP-382).
	mux.HandleFunc("GET /api/v1/goals", a(s.listGoals))
	mux.HandleFunc("GET /api/v1/goals/gaps", a(s.goalGaps))
	mux.HandleFunc("POST /api/v1/goals/suggest", a(s.suggestGoals))
	mux.HandleFunc("GET /api/v1/goals/{id}/work", a(s.goalWork))
	mux.HandleFunc("POST /api/v1/issues/{key}/goal-suggest", a(s.suggestTaskGoal))
	mux.HandleFunc("GET /api/v1/trace", a(s.trace))
	mux.HandleFunc("POST /api/v1/goals/links/{id}/confirm", a(s.confirmGoalLink))
	mux.HandleFunc("POST /api/v1/goals/drift/{id}/answer", a(s.answerDriftFlag))
	mux.HandleFunc("POST /api/v1/goals/links/{id}/dismiss", a(s.dismissGoalLink))
	mux.HandleFunc("POST /api/v1/goals/dismiss-match", a(s.dismissGoalMatch))
	mux.HandleFunc("POST /api/v1/goals/links/acknowledge", a(s.acknowledgeGoalLinks))
	mux.HandleFunc("GET /api/v1/links", a(s.listItemLinks))
	mux.HandleFunc("POST /api/v1/links", a(s.addItemLink))
	mux.HandleFunc("DELETE /api/v1/links/{id}", a(s.removeItemLink))
	mux.HandleFunc("GET /api/v1/resolve", a(s.resolve))
	mux.HandleFunc("GET /api/v1/graph", a(s.graphNear))
	mux.HandleFunc("GET /api/v1/graph/path", a(s.graphPath))
	mux.HandleFunc("POST /api/v1/refs", a(s.resolveRefs))
	mux.HandleFunc("GET /api/v1/search/all", a(s.searchAll))
	mux.HandleFunc("GET /api/v1/workflows", a(s.listWorkflows))
	mux.HandleFunc("POST /api/v1/workflows", a(s.createWorkflow))
	mux.HandleFunc("PATCH /api/v1/workflows/{id}", a(s.updateWorkflow))
	mux.HandleFunc("DELETE /api/v1/workflows/{id}", a(s.deleteWorkflow))
	mux.HandleFunc("GET /api/v1/pages", a(s.listPages))
	mux.HandleFunc("POST /api/v1/pages", a(s.createPage))
	mux.HandleFunc("GET /api/v1/pages/{id}", a(s.getPage))
	mux.HandleFunc("PATCH /api/v1/pages/{id}", a(s.updatePage))
	mux.HandleFunc("DELETE /api/v1/pages/{id}", a(s.deletePage))
	mux.HandleFunc("POST /api/v1/pages/{id}/publish", a(s.publishPage))
	mux.HandleFunc("POST /api/v1/pages/{id}/story", a(s.suggestStory))
	mux.HandleFunc("PUT /api/v1/pages/{id}/issues/{key}", a(s.linkPage))
	mux.HandleFunc("DELETE /api/v1/pages/{id}/issues/{key}", a(s.linkPage))
	mux.HandleFunc("DELETE /api/v1/pages/{id}/codes/{code}", a(s.dropPageCode))
	mux.HandleFunc("GET /api/v1/projects/{key}/page-codes", a(s.pageCodesNext))
	mux.HandleFunc("PUT /api/v1/projects/{key}/page-codes", a(s.reservePageCodes))
	mux.HandleFunc("GET /api/v1/review", a(s.listUnreviewed))
	mux.HandleFunc("POST /api/v1/review/closes", a(s.reviewCloses))
	mux.HandleFunc("GET /api/v1/catch-up", a(s.catchUp))
	mux.HandleFunc("POST /api/v1/issues/{key}/try-links", a(s.addTryLink))
	mux.HandleFunc("POST /api/v1/issues/{key}/not-finished", a(s.notFinished))
	mux.HandleFunc("POST /api/v1/issues/{key}/problems", a(s.reportProblem))
	mux.HandleFunc("POST /api/v1/review/{type}/{ref}", a(s.review))
	mux.HandleFunc("GET /api/v1/risk/{kind}/{ref}", a(s.risk))
	mux.HandleFunc("GET /api/v1/tags", a(s.listTags))
	mux.HandleFunc("POST /api/v1/cli/login", p(s.cliLoginStart))
	mux.HandleFunc("POST /api/v1/cli/login/poll", p(s.cliLoginPoll))
	mux.HandleFunc("GET /api/v1/cli/login/{code}", a(s.cliLoginInfo))
	mux.HandleFunc("POST /api/v1/cli/login/{code}/approve", a(s.cliLoginApprove))
	mux.HandleFunc("GET /cli", s.cliInstall)
	mux.HandleFunc("GET /setup", s.cliSetup)
	mux.HandleFunc("GET /cli/{file}", s.cliFile)

	// AI.
	mux.HandleFunc("GET /api/v1/ai/tasks", a(s.aiTasks))
	mux.HandleFunc("PUT /api/v1/ai/tasks/{task}", a(s.setAITask))
	mux.HandleFunc("GET /api/v1/ai/safety-drill", a(s.safetyDrill))
	mux.HandleFunc("POST /api/v1/ai/safety-drill", a(s.runSafetyDrill))
	mux.HandleFunc("GET /api/v1/ai/review-arms", a(s.reviewArms))
	mux.HandleFunc("PUT /api/v1/ai/review-arms/pins", a(s.pinReviewArm))
	mux.HandleFunc("GET /api/v1/objectives", a(s.listObjectives)) // WP-927
	mux.HandleFunc("GET /api/v1/issues/{key}/objective", a(s.getObjective))
	mux.HandleFunc("PUT /api/v1/issues/{key}/objective", a(s.setObjective))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/objective", a(s.clearObjective))
	mux.HandleFunc("POST /api/v1/issues/{key}/objective/confirm", a(s.confirmObjective))
	mux.HandleFunc("POST /api/v1/issues/{key}/objective/checks", a(s.recordObjectiveCheck))
	mux.HandleFunc("POST /api/v1/issues/{key}/objective/go", a(s.objectiveGo))
	mux.HandleFunc("POST /api/v1/issues/{key}/objective/diagnose", a(s.diagnoseObjective))
	mux.HandleFunc("GET /api/v1/experiments", a(s.listExperiments)) // WP-254
	mux.HandleFunc("POST /api/v1/experiments/{key}/start", a(s.startExperiment))
	mux.HandleFunc("POST /api/v1/experiments/{key}/stop", a(s.stopExperiment))
	mux.HandleFunc("GET /api/v1/experiments/{key}/results", a(s.experimentResults))
	mux.HandleFunc("GET /api/v1/reviews/stats", a(s.reviewStats)) // WP-371
	mux.HandleFunc("GET /api/v1/mistakes", a(s.mistakes))         // WP-710
	mux.HandleFunc("GET /api/v1/ai/providers", a(s.listAIProviders))
	mux.HandleFunc("POST /api/v1/ai/providers", a(s.createAIProvider))
	mux.HandleFunc("PATCH /api/v1/ai/providers/{id}", a(s.updateAIProvider))
	mux.HandleFunc("DELETE /api/v1/ai/providers/{id}", a(s.deleteAIProvider))
	mux.HandleFunc("GET /api/v1/ai/providers/{id}/models", a(s.aiModels))
	mux.HandleFunc("GET /api/v1/projects/{key}/templates", a(s.listTemplates))
	mux.HandleFunc("POST /api/v1/projects/{key}/templates", a(s.createTemplate))
	mux.HandleFunc("PATCH /api/v1/projects/{key}/templates/{id}", a(s.updateTemplate))
	mux.HandleFunc("DELETE /api/v1/projects/{key}/templates/{id}", a(s.deleteTemplate))
	s.attachmentRoutes(mux)
	mux.HandleFunc("POST /api/v1/issues/{key}/ai/estimate", a(s.aiEstimate))
	mux.HandleFunc("POST /api/v1/issues/{key}/ai/subtasks", a(s.aiSubtasks))
	mux.HandleFunc("POST /api/v1/projects/{key}/ai/estimate", a(s.aiEstimateProject))
	mux.HandleFunc("GET /api/v1/prep/due", a(s.prepDue))
	mux.HandleFunc("GET /api/v1/escapes/due", a(s.escapesDue)) // WP-705
	mux.HandleFunc("POST /api/v1/escapes/traces", a(s.escapeTrace))
	mux.HandleFunc("GET /api/v1/escapes/proofs/due", a(s.escapeProofsDue)) // WP-709
	mux.HandleFunc("POST /api/v1/escapes/proofs", a(s.escapeProof))
	mux.HandleFunc("GET /api/v1/escapes/stats", a(s.escapeStats))
	mux.HandleFunc("GET /api/v1/escapes/files", a(s.escapeFiles))
	mux.HandleFunc("GET /api/v1/escapes", a(s.escapeList))
	mux.HandleFunc("GET /api/v1/issues/{key}/prep", a(s.getPrep))
	mux.HandleFunc("POST /api/v1/issues/{key}/prep", a(s.uploadPrep))
	mux.HandleFunc("POST /api/v1/issues/{key}/arms", a(s.issueArms))

	mux.HandleFunc("GET /api/v1/forges", a(s.listForges))
	mux.HandleFunc("POST /api/v1/forges", a(s.createForge))
	mux.HandleFunc("PATCH /api/v1/forges/{id}", a(s.updateForge))
	mux.HandleFunc("DELETE /api/v1/forges/{id}", a(s.deleteForge))
	mux.HandleFunc("GET /api/v1/forges/{id}/secret", a(s.forgeSecret))
	mux.HandleFunc("PUT /api/v1/forges/{id}/people", a(s.setForgePeople))
	mux.HandleFunc("GET /api/v1/forges/{id}/discover", a(s.previewDiscover))
	mux.HandleFunc("POST /api/v1/forges/{id}/discover", a(s.discoverRepos))
	mux.HandleFunc("GET /api/v1/forges/{id}/orgs", a(s.forgeOrgs))
	mux.HandleFunc("GET /api/v1/forges/{id}/org-hooks", a(s.listOrgHooks))
	mux.HandleFunc("POST /api/v1/forges/{id}/org-hooks", a(s.addOrgHook))
	mux.HandleFunc("DELETE /api/v1/forges/{id}/org-hooks/{hid}", a(s.removeOrgHook))

	// Projects.
	mux.HandleFunc("GET /api/v1/projects", a(s.listProjects))
	mux.HandleFunc("POST /api/v1/projects", a(s.createProject))
	mux.HandleFunc("GET /api/v1/projects/{key}", a(s.getProject))
	mux.HandleFunc("PATCH /api/v1/projects/{key}", a(s.updateProject))
	mux.HandleFunc("DELETE /api/v1/projects/{key}", a(s.deleteProject))
	mux.HandleFunc("POST /api/v1/projects/{key}/look/regenerate", a(s.regenerateLook))
	mux.HandleFunc("POST /api/v1/projects/{key}/look/reset", a(s.resetLook))
	mux.HandleFunc("GET /api/v1/projects/{key}/board", a(s.board))
	mux.HandleFunc("GET /api/v1/projects/{key}/backlog", a(s.backlog))
	mux.HandleFunc("GET /api/v1/projects/{key}/activity", a(s.activity))
	mux.HandleFunc("PUT /api/v1/projects/{key}/members/{user}", a(s.setMember))
	mux.HandleFunc("DELETE /api/v1/projects/{key}/members/{user}", a(s.removeMember))
	mux.HandleFunc("POST /api/v1/projects/{key}/statuses", a(s.createStatus))
	mux.HandleFunc("PATCH /api/v1/statuses/{id}", a(s.updateStatus))
	mux.HandleFunc("DELETE /api/v1/statuses/{id}", a(s.deleteStatus))
	mux.HandleFunc("POST /api/v1/projects/{key}/labels", a(s.createLabel))
	mux.HandleFunc("PATCH /api/v1/labels/{id}", a(s.updateLabel))
	mux.HandleFunc("DELETE /api/v1/labels/{id}", a(s.deleteLabel))
	mux.HandleFunc("GET /api/v1/projects/{key}/sprints", a(s.listSprints))
	mux.HandleFunc("POST /api/v1/projects/{key}/sprints", a(s.createSprint))
	mux.HandleFunc("GET /api/v1/sprints/{id}", a(s.getSprint))
	mux.HandleFunc("PATCH /api/v1/sprints/{id}", a(s.updateSprint))
	mux.HandleFunc("DELETE /api/v1/sprints/{id}", a(s.deleteSprint))
	mux.HandleFunc("POST /api/v1/sprints/{id}/start", a(s.startSprint))
	mux.HandleFunc("POST /api/v1/sprints/{id}/close", a(s.closeSprint))
	mux.HandleFunc("GET /api/v1/sprints/{id}/burndown", a(s.burndown))
	mux.HandleFunc("GET /api/v1/projects/{key}/automations", a(s.listAutomations))
	mux.HandleFunc("POST /api/v1/projects/{key}/automations", a(s.createAutomation))
	mux.HandleFunc("PATCH /api/v1/automations/{id}", a(s.updateAutomation))
	mux.HandleFunc("DELETE /api/v1/automations/{id}", a(s.deleteAutomation))
	mux.HandleFunc("GET /api/v1/projects/{key}/bot-runs", a(s.listBotRuns))
	mux.HandleFunc("POST /api/v1/bot-runs/{id}/cancel", a(s.cancelBotRun))
	mux.HandleFunc("POST /api/v1/bot-runs/{id}/retry", a(s.retryBotRun))
	mux.HandleFunc("GET /api/v1/automation-templates", a(s.automationTemplates))
	mux.HandleFunc("GET /api/v1/projects/{key}/webhooks", a(s.listWebhooks))
	mux.HandleFunc("POST /api/v1/projects/{key}/webhooks", a(s.createWebhook))
	mux.HandleFunc("PATCH /api/v1/webhooks/{id}", a(s.updateWebhook))
	mux.HandleFunc("DELETE /api/v1/webhooks/{id}", a(s.deleteWebhook))
	mux.HandleFunc("POST /api/v1/webhooks/{id}/test", a(s.testWebhook))
	mux.HandleFunc("GET /api/v1/webhooks/{id}/deliveries", a(s.webhookDeliveries))
	mux.HandleFunc("POST /api/v1/webhooks/{id}/deliveries/{did}/replay", a(s.replayWebhookDelivery))
	mux.HandleFunc("GET /api/v1/projects/{key}/repos", a(s.listRepos))
	mux.HandleFunc("POST /api/v1/projects/{key}/repos", a(s.addRepo))
	mux.HandleFunc("DELETE /api/v1/projects/{key}/repos/{id}", a(s.removeRepo))
	mux.HandleFunc("PATCH /api/v1/projects/{key}/repos/{id}", a(s.updateRepo))
	mux.HandleFunc("POST /api/v1/projects/{key}/repos/{id}/sync", a(s.syncRepo))
	mux.HandleFunc("POST /api/v1/projects/{key}/repos/{id}/hook", a(s.retryRepoHook))
	mux.HandleFunc("POST /api/v1/projects/{key}/repos/{id}/move", a(s.moveRepo))
	mux.HandleFunc("PUT /api/v1/projects/{key}/repos/{id}/code", a(s.uploadCodeSummary))
	mux.HandleFunc("GET /api/v1/projects/{key}/repos/{id}/code", a(s.codeSummary))
	mux.HandleFunc("GET /api/v1/projects/{key}/reports/velocity", a(s.velocity))
	mux.HandleFunc("GET /api/v1/dashboard", a(s.dashboards))
	mux.HandleFunc("GET /api/v1/projects/{key}/dashboard", a(s.projectDashboard))
	mux.HandleFunc("POST /api/v1/projects/{key}/dashboard/fix", a(s.dashboardFix))
	mux.HandleFunc("GET /api/v1/projects/{key}/reports/throughput", a(s.throughput))
	mux.HandleFunc("GET /api/v1/projects/{key}/reports/flow", a(s.flow))
	mux.HandleFunc("GET /api/v1/projects/{key}/reports/forecast", a(s.forecast))
	mux.HandleFunc("GET /api/v1/projects/{key}/reports/roadmap", a(s.roadmap))
	mux.HandleFunc("GET /api/v1/reports/time", a(s.timeReport))

	// Issues.
	mux.HandleFunc("GET /api/v1/issues", a(s.searchIssues))
	mux.HandleFunc("GET /api/v1/search", a(s.searchIssues))
	mux.HandleFunc("POST /api/v1/issues", a(s.createIssue))
	mux.HandleFunc("POST /api/v1/issues/bulk", a(s.bulkUpdate))
	mux.HandleFunc("POST /api/v1/issues/similar", a(s.similarIssues))
	mux.HandleFunc("GET /api/v1/issues/{key}", a(s.getIssue))
	mux.HandleFunc("PATCH /api/v1/issues/{key}", a(s.updateIssue))
	mux.HandleFunc("DELETE /api/v1/issues/{key}", a(s.deleteIssue))
	mux.HandleFunc("POST /api/v1/issues/{key}/transition", a(s.transition))
	mux.HandleFunc("POST /api/v1/issues/{key}/publish", a(s.publishIssue))
	mux.HandleFunc("POST /api/v1/issues/{key}/draft", a(s.draftIssue))
	mux.HandleFunc("PUT /api/v1/issues/{key}/hold", a(s.holdIssue))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/hold", a(s.liftHold))
	mux.HandleFunc("GET /api/v1/issues/{key}/outside", a(s.listOutside))
	mux.HandleFunc("POST /api/v1/outside/{id}/approve", a(s.approveOutside))
	mux.HandleFunc("POST /api/v1/outside/{id}/discard", a(s.discardOutside))
	mux.HandleFunc("GET /api/v1/pulls/{id}/outside", a(s.listOutsidePR))
	mux.HandleFunc("POST /api/v1/outside-pr/{id}/approve", a(s.approveOutsidePR))
	mux.HandleFunc("POST /api/v1/outside-pr/{id}/discard", a(s.discardOutsidePR))
	mux.HandleFunc("POST /api/v1/issues/{key}/move", a(s.moveIssue))
	mux.HandleFunc("GET /api/v1/issues/{key}/comments", a(s.listComments))
	mux.HandleFunc("POST /api/v1/issues/{key}/comments", a(s.addComment))
	mux.HandleFunc("PATCH /api/v1/comments/{id}", a(s.updateComment))
	mux.HandleFunc("DELETE /api/v1/comments/{id}", a(s.deleteComment))
	mux.HandleFunc("GET /api/v1/issues/{key}/history", a(s.history))
	mux.HandleFunc("GET /api/v1/issues/{key}/relations/suggested", a(s.suggestRelations))
	mux.HandleFunc("POST /api/v1/issues/{key}/relations/suggested/dismiss", a(s.dismissRelationSuggestion))
	mux.HandleFunc("POST /api/v1/issues/{key}/rewritten", a(s.rewrittenWork))
	mux.HandleFunc("PUT /api/v1/issues/{key}/watch", a(s.watch(true)))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/watch", a(s.watch(false)))
	mux.HandleFunc("PUT /api/v1/issues/{key}/shares/{user}", a(s.share))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/shares/{user}", a(s.unshare))
	mux.HandleFunc("POST /api/v1/issues/{key}/relations", a(s.addRelation))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/relations/{id}", a(s.removeRelation))
	mux.HandleFunc("POST /api/v1/issues/{key}/links", a(s.addLink))
	mux.HandleFunc("DELETE /api/v1/issues/{key}/links/{id}", a(s.removeLink))
	mux.HandleFunc("POST /api/v1/issues/{key}/links/refresh", a(s.refreshLinks))
	mux.HandleFunc("GET /api/v1/issues/{key}/forge-mentions", a(s.forgeMentions))
	mux.HandleFunc("GET /api/v1/issues/{key}/time", a(s.issueTime))
	mux.HandleFunc("GET /api/v1/issues/{key}/worklog", a(s.getWorkLog))
	mux.HandleFunc("POST /api/v1/issues/{key}/worklog", a(s.addWorkNote))
	mux.HandleFunc("POST /api/v1/issues/{key}/review-report", a(s.reportReview))
	mux.HandleFunc("GET /api/v1/worklog/stats", a(s.workLogStats))
	mux.HandleFunc("GET /api/v1/worklog/triage", a(s.reviewTriageStats))
	mux.HandleFunc("GET /api/v1/worklog/stakes", a(s.reviewStakesStats))
	mux.HandleFunc("POST /api/v1/projects/{key}/test-evidence", a(s.recordTestEvidence))

	// Time tracking.
	mux.HandleFunc("GET /api/v1/timer", a(s.getTimer))
	mux.HandleFunc("POST /api/v1/timer/start", a(s.startTimer))
	mux.HandleFunc("POST /api/v1/timer/stop", a(s.stopTimer))
	mux.HandleFunc("GET /api/v1/time", a(s.listTime))
	mux.HandleFunc("POST /api/v1/time", a(s.createTime))
	mux.HandleFunc("PATCH /api/v1/time/{id}", a(s.updateTime))
	mux.HandleFunc("DELETE /api/v1/time/{id}", a(s.deleteTime))

	// Notifications, views, live updates.
	mux.HandleFunc("GET /api/v1/notifications", a(s.listNotifications))
	mux.HandleFunc("POST /api/v1/notifications/read", a(s.readNotifications))
	mux.HandleFunc("GET /api/v1/inbox", a(s.inbox))
	mux.HandleFunc("GET /api/v1/inbox/counts", a(s.inboxCounts))
	mux.HandleFunc("POST /api/v1/inbox/read", a(s.readInbox))
	mux.HandleFunc("GET /api/v1/views", a(s.listViews))
	mux.HandleFunc("POST /api/v1/views", a(s.createView))
	mux.HandleFunc("PATCH /api/v1/views/{id}", a(s.updateView))
	mux.HandleFunc("DELETE /api/v1/views/{id}", a(s.deleteView))
	mux.HandleFunc("GET /api/v1/stream", s.stream)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such endpoint; see GET /api/v1/help"})
	})
	mux.HandleFunc("/", s.spa)
}

// ---- auth ----

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, tok string) {
	// #nosec G124 -- Secure is set whenever the site is served over https;
	// plain-http LAN and dev setups need the cookie too.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: s.secureCookies(r),
		SameSite: http.SameSiteLaxMode, MaxAge: 60 * 24 * 3600,
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	// Only failures count, per address and per account. The per-account
	// limit is looser so someone else's guessing can't easily lock you out.
	ip := s.clientIP(r)
	userKey := "login-user:" + strings.ToLower(strings.TrimSpace(in.Login))
	if s.limiter.blocked("login:"+ip) || s.userLimiter.blocked(userKey) {
		return nil, errTooMany
	}
	tok, u, err := s.svc.Login(r.Context(), in.Login, in.Password, r.UserAgent(), ip)
	if err != nil {
		s.limiter.hit("login:" + ip)
		s.userLimiter.hit(userKey)
		return nil, fmt.Errorf("%w: wrong username or password", errBadRequest)
	}
	s.setSession(w, r, tok)
	return u, nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) (any, error) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.svc.Logout(r.Context(), c.Value); err != nil {
			return nil, err
		}
	}
	// #nosec G124 -- same attributes as setSession, so the browser clears it.
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.secureCookies(r), SameSite: http.SameSiteLaxMode})
	return nil, nil
}

func (s *Server) getLink(w http.ResponseWriter, r *http.Request) (any, error) {
	if !s.limiter.allow("link:" + s.clientIP(r)) {
		return nil, errTooMany
	}
	return s.svc.GetLink(r.Context(), r.PathValue("token"))
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AcceptInviteInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if !s.limiter.allow("link:" + s.clientIP(r)) {
		return nil, errTooMany
	}
	tok, u, err := s.svc.AcceptInvite(r.Context(), r.PathValue("token"), in, r.UserAgent(), s.clientIP(r))
	if err != nil {
		return nil, err
	}
	s.setSession(w, r, tok)
	return u, nil
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if !s.limiter.allow("link:" + s.clientIP(r)) {
		return nil, errTooMany
	}
	return nil, s.svc.ResetPassword(r.Context(), r.PathValue("token"), in.Password)
}

// ---- me ----

func (s *Server) me(w http.ResponseWriter, r *http.Request) (any, error) {
	u, err := s.svc.UserByID(r.Context(), actorFrom(r).User.ID)
	if err != nil {
		return nil, err
	}
	if t := tokenInfoFor(actorFrom(r)); t != nil {
		// A permission token also learns what it may do (WP-111).
		return struct {
			*core.User
			Token *tokenInfo `json:"token"`
		}{u, t}, nil
	}
	return u, nil
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.UserPatch
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	in.IsAdmin, in.Disabled, in.Username = nil, nil, nil
	return s.svc.UpdateUser(r.Context(), actorFrom(r), actorFrom(r).User.ID, in)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.ChangePassword(r.Context(), actorFrom(r), in.Current, in.New)
}

func (s *Server) pushInfo(w http.ResponseWriter, r *http.Request) (any, error) {
	key, err := s.svc.PushPublicKey(r.Context())
	if err != nil {
		return nil, err
	}
	n, err := s.svc.PushDevices(r.Context(), actorFrom(r))
	if err != nil {
		return nil, err
	}
	return map[string]any{"public_key": key, "devices": n}, nil
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PushSubscription
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.SavePushSubscription(r.Context(), actorFrom(r), in, r.UserAgent())
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.DeletePushSubscription(r.Context(), actorFrom(r), in.Endpoint)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListSessions(r.Context(), actorFrom(r))
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.RevokeSession(r.Context(), actorFrom(r), r.PathValue("id"))
}

func readUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		// #nosec G120 -- the body is capped by MaxBytesReader just above
		if err := r.ParseMultipartForm(3 << 20); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadRequest, err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, fmt.Errorf("%w: missing file field", errBadRequest)
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, 3<<20))
	}
	return io.ReadAll(io.LimitReader(r.Body, 3<<20))
}

func (s *Server) setMyAvatar(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := readUpload(w, r)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: empty image", errBadRequest)
	}
	if err := s.svc.SetAvatar(r.Context(), actorFrom(r), actorFrom(r).User.ID, b); err != nil {
		return nil, err
	}
	return s.svc.UserByID(r.Context(), actorFrom(r).User.ID)
}

func (s *Server) deleteMyAvatar(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.SetAvatar(r.Context(), actorFrom(r), actorFrom(r).User.ID, nil)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAPITokens(r.Context(), actorFrom(r))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
		Days     int    `json:"expires_in_days"`
		Agent    string `json:"agent"` // makes it an agent token
		// Permissions limits what it may do (WP-111); absent or null: unrestricted.
		Permissions []core.Permission `json:"permissions"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Days < 0 || in.Days > 3650 {
		return nil, fmt.Errorf("%w: expires_in_days must be 0 (never) to 3650", errBadRequest)
	}
	tok, t, err := s.svc.CreateToken(r.Context(), actorFrom(r), core.TokenSpec{Name: in.Name, ReadOnly: in.ReadOnly,
		TTL: time.Duration(in.Days) * 24 * time.Hour, Agent: in.Agent, Permissions: in.Permissions})
	if err != nil {
		return nil, err
	}
	return map[string]any{"token": tok, "info": t}, nil
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RevokeAPIToken(r.Context(), actorFrom(r), id)
}

func (s *Server) calendarInfo(w http.ResponseWriter, r *http.Request) (any, error) {
	tok, err := s.svc.CalendarToken(r.Context(), actorFrom(r), false)
	if err != nil {
		return nil, err
	}
	return map[string]string{"url": s.svc.BaseURL() + "/cal/" + tok + ".ics"}, nil
}

func (s *Server) rotateCalendar(w http.ResponseWriter, r *http.Request) (any, error) {
	tok, err := s.svc.CalendarToken(r.Context(), actorFrom(r), true)
	if err != nil {
		return nil, err
	}
	return map[string]string{"url": s.svc.BaseURL() + "/cal/" + tok + ".ics"}, nil
}

func (s *Server) testGotify(w http.ResponseWriter, r *http.Request) (any, error) {
	u, err := s.svc.UserByID(r.Context(), actorFrom(r).User.ID)
	if err != nil {
		return nil, err
	}
	if u.Prefs.GotifyURL == "" || u.Prefs.GotifyToken == "" {
		return nil, fmt.Errorf("%w: set a Gotify URL and app token first", errBadRequest)
	}
	if err := s.svc.SendGotify(r.Context(), u.Prefs.GotifyURL, u.Prefs.GotifyToken, "Waypoint", "Notifications are working 🎉", 5, s.svc.BaseURL()); err != nil {
		return nil, fmt.Errorf("%w: Gotify: %s", errBadRequest, core.PublicError(err))
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSuffix(r.PathValue("file"), ".ics")
	b, err := s.svc.CalendarFeed(r.Context(), tok)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(b) // #nosec G705 -- an iCalendar document Waypoint built, served as text/calendar
}

// ---- users ----

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) (any, error) {
	users, err := s.svc.ListUsers(r.Context(), actorFrom(r))
	if err != nil {
		return nil, err
	}
	out := []core.UserRef{}
	for _, u := range users {
		if !u.Disabled {
			out = append(out, u.Ref())
		}
	}
	return out, nil
}

func (s *Server) avatar(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Avatars are only for signed-in users.
	if _, err := s.authenticate(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	b, ct, err := s.svc.Avatar(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(b) // #nosec G705 -- avatar bytes were sniffed as a raster image on upload (no SVG)
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) (any, error) {
	if !actorFrom(r).User.IsAdmin {
		return nil, core.ErrForbidden
	}
	return s.svc.ListUsers(r.Context(), actorFrom(r))
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.CreateUserInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateUser(r.Context(), actorFrom(r), in)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	if !actorFrom(r).User.IsAdmin {
		return nil, core.ErrForbidden
	}
	var in core.UserPatch
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	in.Prefs = nil
	return s.svc.UpdateUser(r.Context(), actorFrom(r), id, in)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteUser(r.Context(), actorFrom(r), id)
}

func (s *Server) adminSetAvatar(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	if !actorFrom(r).User.IsAdmin {
		return nil, core.ErrForbidden
	}
	b, err := readUpload(w, r)
	if err != nil {
		return nil, err
	}
	return nil, s.svc.SetAvatar(r.Context(), actorFrom(r), id, b)
}

func (s *Server) adminResetLink(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CreateResetLink(r.Context(), actorFrom(r), id)
}

func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListInvites(r.Context(), actorFrom(r))
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Email   string `json:"email"`
		IsAdmin bool   `json:"is_admin"`
		Days    int    `json:"expires_in_days"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateInvite(r.Context(), actorFrom(r), in.Email, in.IsAdmin, time.Duration(in.Days)*24*time.Hour)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.RevokeInvite(r.Context(), actorFrom(r), r.PathValue("id"))
}

// ---- issue templates ----

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListTemplates(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.TemplateInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateTemplate(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.TemplateInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateTemplate(r.Context(), actorFrom(r), r.PathValue("key"), id, in)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteTemplate(r.Context(), actorFrom(r), r.PathValue("key"), id)
}

// ---- AI ----

func (s *Server) aiTasks(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.AITasks(r.Context(), actorFrom(r))
}

func (s *Server) setAITask(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		ProviderID int64             `json:"provider_id"`
		Model      string            `json:"model"`
		Fallbacks  []core.ChainModel `json:"fallbacks"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.SetAITask(r.Context(), actorFrom(r), r.PathValue("task"), in.ProviderID, in.Model, in.Fallbacks); err != nil {
		return nil, err
	}
	return s.svc.AITasks(r.Context(), actorFrom(r))
}

func (s *Server) safetyDrill(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SafetyDrillStatus(r.Context(), actorFrom(r))
}

func (s *Server) runSafetyDrill(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.RunSafetyDrill(r.Context(), actorFrom(r))
}

func (s *Server) reviewArms(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ReviewArms(r.Context(), actorFrom(r))
}

func (s *Server) pinReviewArm(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Arm string `json:"arm"`
		Pin string `json:"pin"` // on, off, "" to unpin
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.PinReviewArm(r.Context(), actorFrom(r), in.Arm, in.Pin)
}

func (s *Server) listAIProviders(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAIProviders(r.Context(), actorFrom(r))
}

func (s *Server) createAIProvider(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AIProviderInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateAIProvider(r.Context(), actorFrom(r), in)
}

func (s *Server) updateAIProvider(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.AIProviderInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateAIProvider(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteAIProvider(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteAIProvider(r.Context(), actorFrom(r), id)
}

func (s *Server) aiModels(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.AIModels(r.Context(), actorFrom(r), id)
}

func (s *Server) aiEstimate(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.EstimateIssue(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) aiSubtasks(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SuggestSubtasks(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) regenerateLook(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.RegenerateLook(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) resetLook(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ResetLook(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) aiEstimateProject(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := s.svc.EstimateUnestimated(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	return map[string]int{"queued": n}, nil
}

// ---- forges ----

func (s *Server) listForges(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListForges(r.Context(), actorFrom(r))
}

func (s *Server) createForge(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ForgeInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateForge(r.Context(), actorFrom(r), in)
}

func (s *Server) updateForge(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.ForgeInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateForge(r.Context(), actorFrom(r), id, in)
}

// setForgePeople says which of a forge's logins are Waypoint people (WP-279).
func (s *Server) setForgePeople(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		People []core.ForgePersonInput `json:"people"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetForgePeople(r.Context(), actorFrom(r), id, in.People)
}

func (s *Server) deleteForge(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteForge(r.Context(), actorFrom(r), id)
}

func (s *Server) forgeSecret(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	sec, err := s.svc.ForgeSecret(r.Context(), actorFrom(r), id)
	return map[string]string{"webhook_secret": sec}, err
}

// previewDiscover lists the repos discovery would attach.
func (s *Server) previewDiscover(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.PreviewDiscover(r.Context(), actorFrom(r), id)
}

// discoverRepos attaches every repo the forge's token can see, or those
// the body picks ({"repos": [...], "skip": [...]}), making projects as
// needed.
func (s *Server) discoverRepos(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var pick *core.RepoPick
	if err := decodeOptional(r, &pick); err != nil {
		return nil, err
	}
	n, err := s.svc.DiscoverRepos(r.Context(), actorFrom(r), id, pick)
	if err != nil {
		return nil, err
	}
	return map[string]int{"added": n}, nil
}

func (s *Server) forgeOrgs(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ForgeOrgs(r.Context(), actorFrom(r), id)
}

func (s *Server) listOrgHooks(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ListOrgHooks(r.Context(), actorFrom(r), id)
}

// addOrgHook adds an organisation webhook, or tries an existing one again.
func (s *Server) addOrgHook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Org string `json:"org"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddOrgHook(r.Context(), actorFrom(r), id, in.Org)
}

func (s *Server) removeOrgHook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	hid, err := pathID(r, "hid")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RemoveOrgHook(r.Context(), actorFrom(r), id, hid)
}

func (s *Server) forgeHook(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// GitHub caps payloads at 25 MB; a cut-off body would fail its signature.
	const maxHook = 25 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHook+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxHook {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.svc.HandleForgeWebhook(r.Context(), id, r.Header, body); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- projects ----

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) (any, error) {
	if m := r.URL.Query().Get("match"); m != "" {
		return s.svc.FindProjects(r.Context(), actorFrom(r), m, false)
	}
	if repo := r.URL.Query().Get("repo"); repo != "" {
		return s.svc.FindProjects(r.Context(), actorFrom(r), repo, true)
	}
	return s.svc.ListProjects(r.Context(), actorFrom(r), queryBool(r, "archived"))
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ProjectInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateProject(r.Context(), actorFrom(r), in)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetProject(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ProjectInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateProject(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.DeleteProject(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetBoard(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("q"))
}

func (s *Server) backlog(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetBacklog(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("q"))
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ProjectActivity(r.Context(), actorFrom(r), r.PathValue("key"), queryInt(r, "limit", 100))
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.SetMember(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("user"), in.Role)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.RemoveMember(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("user"))
}

func (s *Server) createStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.StatusInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateStatus(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.StatusInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.UpdateStatus(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteStatus(r.Context(), actorFrom(r), id, int64(queryInt(r, "move_to", 0)))
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.LabelInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateLabel(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.LabelInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.UpdateLabel(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteLabel(r.Context(), actorFrom(r), id)
}

// ---- sprints ----

func (s *Server) listSprints(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListSprints(r.Context(), actorFrom(r), r.PathValue("key"), queryBool(r, "closed"))
}

func (s *Server) createSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.SprintInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateSprint(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) getSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetSprint(r.Context(), actorFrom(r), id)
}

func (s *Server) updateSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.SprintInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateSprint(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteSprint(r.Context(), actorFrom(r), id)
}

func (s *Server) startSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.StartSprint(r.Context(), actorFrom(r), id)
}

func (s *Server) closeSprint(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		MoveTo string `json:"move_to"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CloseSprint(r.Context(), actorFrom(r), id, in.MoveTo)
}

func (s *Server) burndown(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.SprintBurndown(r.Context(), actorFrom(r), id)
}

// ---- bot runs (WP-D14) ----

func (s *Server) listBotRuns(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.ListBotRunsFiltered(r.Context(), actorFrom(r), r.PathValue("key"), core.BotRunFilter{
		Status:     q.Get("status"),
		Automation: int64(queryInt(r, "automation", 0)),
		Before:     int64(queryInt(r, "before", 0)),
		Limit:      queryInt(r, "limit", 50),
	})
}

func (s *Server) cancelBotRun(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CancelBotRun(r.Context(), actorFrom(r), id)
}

func (s *Server) retryBotRun(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.RetryBotRun(r.Context(), actorFrom(r), id)
}

// ---- automations & webhooks ----

func (s *Server) listAutomations(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAutomations(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) createAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutomationInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateAutomation(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) updateAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.AutomationInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateAutomation(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteAutomation(r.Context(), actorFrom(r), id)
}

func (s *Server) automationTemplates(w http.ResponseWriter, r *http.Request) (any, error) {
	triggers := make([]string, 0, len(core.TriggerEvents))
	for k := range core.TriggerEvents {
		triggers = append(triggers, k)
	}
	slices.Sort(triggers)
	return map[string]any{"templates": core.AutomationTemplates, "triggers": triggers, "actions": core.ActionTypes}, nil
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListWebhooks(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.WebhookInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateWebhook(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) listGlobalWebhooks(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListWebhooks(r.Context(), actorFrom(r), "")
}

func (s *Server) createGlobalWebhook(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.WebhookInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateWebhook(r.Context(), actorFrom(r), "", in)
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.WebhookInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateWebhook(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteWebhook(r.Context(), actorFrom(r), id)
}

func (s *Server) webhookDeliveries(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.WebhookDeliveries(r.Context(), actorFrom(r), id)
}

func (s *Server) replayWebhookDelivery(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	did, err := pathID(r, "did")
	if err != nil {
		return nil, err
	}
	return s.svc.ReplayWebhookDelivery(r.Context(), actorFrom(r), id, did)
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	status, err := s.svc.TestWebhook(r.Context(), actorFrom(r), id)
	if core.IsInvalid(err) || errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthorized) {
		return nil, err
	}
	res := map[string]any{"status": status, "ok": err == nil}
	if err != nil {
		res["error"] = core.PublicError(err)
	}
	return res, nil
}

// ---- repos ----

func (s *Server) updateRepo(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		SyncIssues   *bool `json:"sync_issues"`
		PostComments *bool `json:"post_comments"`
		Bots         *bool `json:"bots"`
		// The review gate (WP-D21).
		ReviewGate   *bool   `json:"review_gate"`
		GateSeverity *string `json:"gate_severity"`
		// Delete merged branches on the forge (WP-140).
		DeleteMergedBranches *bool `json:"delete_merged_branches"`
		// Merge automatically and the merge rules (WP-133).
		AutoMerge         *bool     `json:"auto_merge"`
		AutoMergeInto     *string   `json:"auto_merge_into"`
		AutoMergeBases    *[]string `json:"auto_merge_bases"`
		RequiredChecks    *[]string `json:"required_checks"`
		ReviewWaitMinutes *int      `json:"review_wait_minutes"`
		QuietMinutes      *int      `json:"quiet_minutes"`
		ExternalReviewers *[]string `json:"external_reviewers"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	// The gate and the merge policy are saved first: they are the parts a
	// bad request can fail on (an unknown severity, minutes out of range),
	// and that must leave the other switches untouched.
	if in.ReviewGate != nil || in.GateSeverity != nil {
		if err := s.svc.SetRepoGate(r.Context(), actorFrom(r), r.PathValue("key"), id, in.ReviewGate, in.GateSeverity); err != nil {
			return nil, err
		}
	}
	if in.DeleteMergedBranches != nil {
		if err := s.svc.SetRepoBranchCleanup(r.Context(), actorFrom(r), r.PathValue("key"), id, *in.DeleteMergedBranches); err != nil {
			return nil, err
		}
	}
	pol := core.MergePolicyInput{AutoMerge: in.AutoMerge, AutoMergeInto: in.AutoMergeInto, AutoMergeBases: in.AutoMergeBases, RequiredChecks: in.RequiredChecks, ReviewWaitMinutes: in.ReviewWaitMinutes,
		QuietMinutes: in.QuietMinutes, ExternalReviewers: in.ExternalReviewers}
	if err := s.svc.SetRepoMergePolicy(r.Context(), actorFrom(r), r.PathValue("key"), id, pol); err != nil {
		return nil, err
	}
	return nil, s.svc.UpdateProjectRepo(r.Context(), actorFrom(r), r.PathValue("key"), id, in.SyncIssues, in.PostComments, in.Bots)
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListProjectRepos(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) addRepo(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		ForgeID    int64  `json:"forge_id"`
		Repo       string `json:"repo"`
		SyncIssues bool   `json:"sync_issues"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddProjectRepo(r.Context(), actorFrom(r), r.PathValue("key"), in.ForgeID, in.Repo, in.SyncIssues)
}

func (s *Server) removeRepo(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RemoveProjectRepo(r.Context(), actorFrom(r), r.PathValue("key"), id)
}

// moveRepo follows a repo renamed or transferred on its forge (WP-166).
func (s *Server) moveRepo(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Repo string `json:"repo"`
		core.MoveOptions
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.MoveProjectRepo(r.Context(), actorFrom(r), r.PathValue("key"), id, in.Repo, in.MoveOptions)
}

func (s *Server) retryRepoHook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RetryRepoHook(r.Context(), actorFrom(r), r.PathValue("key"), id)
}

func (s *Server) syncRepo(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	repos, err := s.svc.ListProjectRepos(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	for _, rp := range repos {
		if rp.ID == id {
			return nil, s.svc.SyncRepo(r.Context(), id)
		}
	}
	return nil, core.ErrNotFound
}

// ---- reports ----

func (s *Server) velocity(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Velocity(r.Context(), actorFrom(r), r.PathValue("key"), queryInt(r, "limit", 10))
}

func (s *Server) throughput(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Throughput(r.Context(), actorFrom(r), r.PathValue("key"), queryIntIn(r, "weeks", 12, 1, 104))
}

func (s *Server) flow(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.CumulativeFlow(r.Context(), actorFrom(r), r.PathValue("key"), queryIntIn(r, "days", 30, 1, 365))
}

func (s *Server) forecast(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ForecastWork(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("q"))
}

func (s *Server) roadmap(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Roadmap(r.Context(), actorFrom(r), r.PathValue("key"))
}

func parseRange(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now()
	from, to := now.AddDate(0, 0, -7), now
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			return from, to, err
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			return from, to, err
		}
		to = t
	}
	return from, to, nil
}

func parseTimeParam(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: times must be RFC 3339 or YYYY-MM-DD", errBadRequest)
}

func (s *Server) timeReport(w http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := parseRange(r)
	if err != nil {
		return nil, err
	}
	return s.svc.GetTimeReport(r.Context(), actorFrom(r), from, to, r.URL.Query().Get("project"))
}

// ---- issues ----

func (s *Server) searchIssues(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	res, err := s.svc.Search(r.Context(), actorFrom(r), core.SearchParams{
		Query: q.Get("q"), Project: q.Get("project"), Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
		WithDescription: queryBool(r, "description"), DefaultOrder: cmpStr(q.Get("default_order"), "updated"),
		Near: q.Get("near"),
	})
	if err != nil {
		return nil, err
	}
	switch {
	case q.Get("format") == "text":
		return rawResponse{"text/plain; charset=utf-8", []byte(issuesText(res))}, nil
	case q.Get("fields") != "":
		return projectFields(res, strings.Split(q.Get("fields"), ","))
	}
	return res, nil
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// projectFields keeps only the requested issue fields, for compact agent output.
func projectFields(res *core.SearchResult, fields []string) (any, error) {
	b, err := json.Marshal(res.Issues)
	if err != nil {
		return nil, err
	}
	var all []map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err // unreachable: b is our own Marshal output
	}
	out := make([]map[string]any, len(all))
	for i, m := range all {
		o := map[string]any{}
		for _, f := range fields {
			f = strings.TrimSpace(f)
			if v, ok := m[f]; ok {
				o[f] = v
			}
		}
		out[i] = o
	}
	return map[string]any{"issues": out, "total": res.Total, "limit": res.Limit, "offset": res.Offset}, nil
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.IssueInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateIssue(r.Context(), actorFrom(r), in)
}

// similarIssues lists open issues like text not yet filed (WP-104).
func (s *Server) similarIssues(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.SimilarQuery
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SimilarIssues(r.Context(), actorFrom(r), in)
}

func (s *Server) bulkUpdate(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Issues      []string        `json:"issues"`
		Patch       core.IssuePatch `json:"patch"`
		Descendants bool            `json:"descendants"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	keys, err := s.svc.BulkUpdate(r.Context(), actorFrom(r), in.Issues, in.Patch, in.Descendants)
	return map[string]any{"updated": keys}, err
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	d, err := s.svc.GetIssue(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("format") == "text" {
		comments, rerr := s.svc.ListComments(r.Context(), actorFrom(r), d.Key)
		if rerr != nil {
			if !errors.Is(rerr, context.Canceled) {
				s.log.WarnContext(r.Context(), "list comments failed", "key", d.Key, "err", rerr)
			}
		}
		return rawResponse{"text/plain; charset=utf-8", []byte(issueTextHint(d, comments, true))}, nil
	}
	return d, nil
}

func (s *Server) updateIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.IssuePatch
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateIssue(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) deleteIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.DeleteIssue(r.Context(), actorFrom(r), r.PathValue("key"), queryBool(r, "cascade"))
}

func (s *Server) transition(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Status  string `json:"status"`
		Comment string `json:"comment"`
		ViaLink string `json:"via_link"` // a pull request link on the issue that moves it (WP-162)
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	d, err := s.svc.UpdateIssue(r.Context(), actorFrom(r), r.PathValue("key"), core.IssuePatch{Status: &in.Status, Comment: in.Comment, ViaLink: in.ViaLink})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// publishIssue makes a draft visible to agents (WP-42); people only.
func (s *Server) publishIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.PublishIssue(r.Context(), actorFrom(r), r.PathValue("key"))
}

// draftIssue makes an issue a draft again, with what's under it (WP-42).
func (s *Server) draftIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.DraftIssue(r.Context(), actorFrom(r), r.PathValue("key"))
}

// holdIssue puts an issue on hold, or changes its hold (WP-269).
func (s *Server) holdIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.HoldInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetHold(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// liftHold takes an issue off hold (WP-269).
func (s *Server) liftHold(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.LiftHold(r.Context(), actorFrom(r), r.PathValue("key"))
}

// listOutside lists text from outside waiting on an issue (WP-279); people only.
func (s *Server) listOutside(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListOutside(r.Context(), actorFrom(r), r.PathValue("key"))
}

// approveOutside lets text from outside in, where agents read it; people only.
func (s *Server) approveOutside(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.ApproveOutside(r.Context(), actorFrom(r), id)
}

// discardOutside keeps text from outside out for good; people only.
func (s *Server) discardOutside(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DiscardOutside(r.Context(), actorFrom(r), id)
}

// listOutsidePR lists text from outside waiting on a pull request (WP-281);
// people only.
func (s *Server) listOutsidePR(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ListOutsidePR(r.Context(), actorFrom(r), id)
}

// approveOutsidePR lets text from outside on a pull request in; people only.
func (s *Server) approveOutsidePR(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.ApproveOutsidePR(r.Context(), actorFrom(r), id)
}

// discardOutsidePR keeps a comment from outside on a pull request out;
// people only.
func (s *Server) discardOutsidePR(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DiscardOutsidePR(r.Context(), actorFrom(r), id)
}

// publishPage makes a draft page visible to agents (WP-42); people only.
func (s *Server) publishPage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	return s.svc.PublishPage(r.Context(), actorFrom(r), id)
}

// suggestStory has the story model write a page's user story now (WP-190).
func (s *Server) suggestStory(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	return s.svc.SuggestStory(r.Context(), actorFrom(r), id)
}

func (s *Server) moveIssue(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Project string `json:"project"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.MoveToProject(r.Context(), actorFrom(r), r.PathValue("key"), in.Project)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListComments(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Body string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddComment(r.Context(), actorFrom(r), r.PathValue("key"), in.Body)
}

func (s *Server) updateComment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.UpdateComment(r.Context(), actorFrom(r), id, in.Body)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteComment(r.Context(), actorFrom(r), id)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.History(r.Context(), actorFrom(r), r.PathValue("key"), queryInt(r, "limit", 200))
}

func (s *Server) watch(on bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		return nil, s.svc.Watch(r.Context(), actorFrom(r), r.PathValue("key"), on)
	}
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Role == "" {
		in.Role = "editor"
	}
	return nil, s.svc.ShareIssue(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("user"), in.Role)
}

func (s *Server) unshare(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.ShareIssue(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("user"), "")
}

func (s *Server) addRelation(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.RelationInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.Relate(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) dismissRelationSuggestion(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Issue string `json:"issue"`
		Undo  bool   `json:"undo"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Undo {
		return nil, s.svc.UndoRelationDismissal(r.Context(), actorFrom(r), r.PathValue("key"), in.Issue)
	}
	return nil, s.svc.DismissRelationSuggestion(r.Context(), actorFrom(r), r.PathValue("key"), in.Issue)
}

func (s *Server) removeRelation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RemoveRelation(r.Context(), actorFrom(r), r.PathValue("key"), id)
}

func (s *Server) addLink(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddLink(r.Context(), actorFrom(r), r.PathValue("key"), in.URL, in.Title)
}

func (s *Server) removeLink(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.RemoveLink(r.Context(), actorFrom(r), r.PathValue("key"), id)
}

// forgeMentions are the forge items naming the issue (WP-379); ?all=1 adds
// the linked ones and those whose text no longer names it.
func (s *Server) forgeMentions(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ForgeMentions(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("all") == "1")
}

func (s *Server) refreshLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	d, err := s.svc.GetIssue(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	if err := s.svc.RefreshLinks(r.Context(), d.ID); err != nil {
		return nil, err
	}
	return s.svc.GetIssue(r.Context(), actorFrom(r), d.Key)
}

// getWorkLog is the task's work log (WP-285): adversarial review rounds,
// notes, agent runs, pull requests, review findings and comment verdicts.
func (s *Server) getWorkLog(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetWorkLog(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) addWorkNote(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.WorkNoteInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddWorkNote(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// recordTestEvidence: wp ship's report of the runs on the tree it ships.
func (s *Server) recordTestEvidence(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.TestEvidenceInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RecordTestEvidence(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// workLogStats: what adversarial reviews found, per project, over ?days=.
func (s *Server) workLogStats(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetWorkLogStats(r.Context(), actorFrom(r), r.URL.Query().Get("project"), queryIntIn(r, "days", 30, 1, 365))
}

func (s *Server) reviewTriageStats(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetReviewTriageStats(r.Context(), actorFrom(r), r.URL.Query().Get("project"), queryIntIn(r, "days", 30, 1, 365), queryBool(r, "rows"))
}

func (s *Server) reviewStakesStats(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetReviewStakesStats(r.Context(), actorFrom(r), r.URL.Query().Get("project"), queryIntIn(r, "days", 14, 1, 365), queryBool(r, "rows"))
}

func (s *Server) issueTime(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListTimeEntries(r.Context(), actorFrom(r), core.TimeQuery{Issue: r.PathValue("key")})
}

// ---- time ----

func (s *Server) getTimer(w http.ResponseWriter, r *http.Request) (any, error) {
	t, err := s.svc.RunningTimer(r.Context(), actorFrom(r))
	if err != nil {
		return nil, err
	}
	return map[string]any{"timer": t}, nil
}

func (s *Server) startTimer(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Issue string `json:"issue"`
		Note  string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	t, err := s.svc.StartTimer(r.Context(), actorFrom(r), in.Issue, in.Note)
	return map[string]any{"timer": t}, err
}

func (s *Server) stopTimer(w http.ResponseWriter, r *http.Request) (any, error) {
	t, err := s.svc.StopTimer(r.Context(), actorFrom(r))
	return map[string]any{"stopped": t}, err
}

func (s *Server) listTime(w http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := parseRange(r)
	if err != nil {
		return nil, err
	}
	return s.svc.ListTimeEntries(r.Context(), actorFrom(r), core.TimeQuery{From: &from, To: &to, Project: r.URL.Query().Get("project"), Limit: queryInt(r, "limit", 500)})
}

func (s *Server) createTime(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.TimeEntryInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateTimeEntry(r.Context(), actorFrom(r), in)
}

func (s *Server) updateTime(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.TimeEntryInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateTimeEntry(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteTime(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteTimeEntry(r.Context(), actorFrom(r), id)
}

// ---- notifications & views ----

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) (any, error) {
	list, unread, err := s.svc.ListNotifications(r.Context(), actorFrom(r), queryBool(r, "unread"), queryInt(r, "limit", 50))
	return map[string]any{"notifications": list, "unread": unread}, err
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Inbox(r.Context(), actorFrom(r))
}

func (s *Server) inboxCounts(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.InboxCounts(r.Context(), actorFrom(r))
}

func (s *Server) readInbox(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.InboxScope
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	n, err := s.svc.MarkInboxRead(r.Context(), actorFrom(r), in)
	return map[string]any{"marked": n}, err
}

func (s *Server) readNotifications(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		IDs  []int64 `json:"ids"`
		Upto int64   `json:"upto"` // with no ids: mark only up to this notification id
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.MarkNotificationsReadUpto(r.Context(), actorFrom(r), in.IDs, in.Upto)
}

func (s *Server) listViews(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListViews(r.Context(), actorFrom(r))
}

func (s *Server) createView(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ViewInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateView(r.Context(), actorFrom(r), in)
}

func (s *Server) updateView(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.ViewInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.UpdateView(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteView(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteView(r.Context(), actorFrom(r), id)
}

// stream sends live updates as server-sent events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	waits(r)
	a, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sub := s.svc.Hub.Subscribe(a.User.ID)
	sub.HidesDrafts = a.HidesDrafts()
	defer s.svc.Hub.Unsubscribe(sub)
	sseClients.Inc()
	defer sseClients.Dec()
	fmt.Fprintf(w, "retry: 3000\nevent: hello\ndata: {}\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return // shutting down; the client reconnects to the new process
		case <-ping.C:
			// Signed out, disabled or revoked since connecting: stop.
			if _, err := s.authenticate(r); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return // client gone
			}
			fl.Flush()
		case e := <-sub.C:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Kind, b)
			fl.Flush()
		}
	}
}
