package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type kanbanMutationLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

type kanbanActionTarget struct {
	key       string
	connector connector.Connector
	workflow  workflowconfig.Config
	kanban    workflowconfig.Kanban
}

type kanbanMoveRequest struct {
	projectID    string
	issueID      string
	currentState string
	targetState  string
	prNumber     int
}

type kanbanCommentRequest struct {
	projectID    string
	target       string
	issueID      string
	prRepository string
	prNumber     int
	body         string
}

func newKanbanMutationLocks() *kanbanMutationLocks {
	return &kanbanMutationLocks{locks: map[string]*sync.Mutex{}}
}

func (l *kanbanMutationLocks) withLock(key string, fn func() error) error {
	lock := l.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (l *kanbanMutationLocks) lockFor(key string) *sync.Mutex {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "default"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	lock, ok := l.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		l.locks[key] = lock
	}
	return lock
}

func (s *Server) apiKanbanMove(c echo.Context) error {
	req, response, status := parseKanbanMoveRequest(c)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if req.issueID == "" {
		if req.prNumber > 0 {
			return kanbanFeedback(c, http.StatusUnprocessableEntity, "Cannot move PR-only card without a linked issue.")
		}
		return kanbanFeedback(c, http.StatusBadRequest, "Issue id is required.")
	}
	if !kanbanStateAllowed(target.workflow, req.targetState) {
		return kanbanFeedback(c, http.StatusBadRequest, "Target state is not configured for this board.")
	}
	currentState := req.currentState
	if ok, current := s.kanbanCardFresh(req.projectID, req.issueID, req.currentState); !ok {
		message := "Card is stale; refresh and retry."
		if current != "" {
			message = fmt.Sprintf("Card is stale; current state is %s.", current)
		}
		return kanbanFeedback(c, http.StatusConflict, message)
	} else if strings.TrimSpace(current) != "" {
		currentState = current
	}
	if !target.workflow.KanbanTransitionAllowed(currentState, req.targetState) {
		return kanbanFeedback(c, http.StatusUnprocessableEntity, fmt.Sprintf("Move from %s to %s is not allowed by the Kanban transition policy.", currentState, req.targetState))
	}

	err := s.kanbanMutations.withLock(target.key, func() error {
		if target.kanban.IssueStateFieldID > 0 {
			setter, ok := target.connector.(connector.IssueFieldSetter)
			if !ok {
				return connector.ErrNotImplemented
			}
			return setter.SetIssueField(c.Request().Context(), req.issueID, target.kanban.IssueStateFieldID, mappedKanbanState(target.workflow, req.targetState))
		}
		return target.connector.UpdateIssueState(c.Request().Context(), req.issueID, req.targetState)
	})
	if err != nil {
		s.logger.WarnContext(c.Request().Context(), "kanban move failed", "project", req.projectID, "issue_id", req.issueID, "target_state", req.targetState, "error", err)
		return kanbanFeedback(c, http.StatusBadGateway, "Move failed: "+err.Error())
	}
	s.requestKanbanRefresh(c.Request().Context())
	return kanbanFeedback(c, http.StatusOK, "Moved card to "+req.targetState+".")
}

func (s *Server) apiKanbanComment(c echo.Context) error {
	req, response, status := parseKanbanCommentRequest(c)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if !s.kanbanCommentTargetKnown(req) {
		return kanbanFeedback(c, http.StatusNotFound, "Comment target is not available on the current board.")
	}

	err := s.kanbanMutations.withLock(target.key, func() error {
		switch req.target {
		case "issue":
			return target.connector.CreateComment(c.Request().Context(), req.issueID, req.body)
		case "pr":
			commenter, ok := target.connector.(connector.PullRequestCommenter)
			if !ok {
				return connector.ErrNotImplemented
			}
			return commenter.CreatePullRequestComment(c.Request().Context(), req.prRepository, req.prNumber, req.body)
		default:
			return connector.ErrNotImplemented
		}
	})
	if err != nil {
		s.logger.WarnContext(c.Request().Context(), "kanban comment failed", "project", req.projectID, "target", req.target, "error", err)
		return kanbanFeedback(c, http.StatusBadGateway, "Comment failed: "+err.Error())
	}
	s.requestKanbanRefresh(c.Request().Context())
	return kanbanFeedback(c, http.StatusOK, "Comment submitted.")
}

func parseKanbanMoveRequest(c echo.Context) (kanbanMoveRequest, string, int) {
	req := kanbanMoveRequest{
		projectID:    strings.TrimSpace(c.FormValue("project_id")),
		issueID:      strings.TrimSpace(c.FormValue("issue_id")),
		currentState: strings.TrimSpace(c.FormValue("current_state")),
		targetState:  strings.TrimSpace(c.FormValue("target_state")),
	}
	if value := strings.TrimSpace(c.FormValue("pr_number")); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			return kanbanMoveRequest{}, "PR number is invalid.", http.StatusBadRequest
		}
		req.prNumber = number
	}
	if req.targetState == "" {
		return kanbanMoveRequest{}, "Target state is required.", http.StatusBadRequest
	}
	return req, "", 0
}

func parseKanbanCommentRequest(c echo.Context) (kanbanCommentRequest, string, int) {
	req := kanbanCommentRequest{
		projectID:    strings.TrimSpace(c.FormValue("project_id")),
		target:       strings.ToLower(strings.TrimSpace(c.FormValue("target"))),
		issueID:      strings.TrimSpace(c.FormValue("issue_id")),
		prRepository: strings.TrimSpace(c.FormValue("pr_repository")),
		body:         strings.TrimSpace(c.FormValue("body")),
	}
	if value := strings.TrimSpace(c.FormValue("pr_number")); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			return kanbanCommentRequest{}, "PR number is invalid.", http.StatusBadRequest
		}
		req.prNumber = number
	}
	if req.body == "" {
		return kanbanCommentRequest{}, "Comment body is required.", http.StatusBadRequest
	}
	switch req.target {
	case "issue":
		if req.issueID == "" {
			return kanbanCommentRequest{}, "Issue id is required.", http.StatusBadRequest
		}
	case "pr":
		if req.prRepository == "" || req.prNumber <= 0 {
			return kanbanCommentRequest{}, "PR repository and number are required.", http.StatusBadRequest
		}
	default:
		return kanbanCommentRequest{}, "Comment target must be issue or pr.", http.StatusBadRequest
	}
	return req, "", 0
}

func (s *Server) kanbanActionTarget(projectID string) (kanbanActionTarget, string, int) {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		trackedProject, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return kanbanActionTarget{}, "Project not found.", http.StatusNotFound
		}
		workflow := trackedProject.Workflow().Config
		kanban := workflow.Server.Kanban
		kanban.Normalize()
		return kanbanActionTarget{
			key:       "project:" + projectID,
			connector: trackedProject.Connector(),
			workflow:  workflow,
			kanban:    kanban,
		}, "", 0
	}

	workflow := workflowconfig.Default()
	workflow.Server.Kanban = s.kanban
	return kanbanActionTarget{
		key:       "connector:" + s.connector.Name(),
		connector: s.connector,
		workflow:  workflow,
		kanban:    s.kanban,
	}, "", 0
}

func (s *Server) dashboardKanbanData(ctx context.Context, projectID string, snapshot telemetry.Snapshot) templates.KanbanData {
	target, _, _ := s.kanbanActionTarget(projectID)
	if target.connector == nil {
		return templates.KanbanData{Mode: workflowconfig.KanbanModeReadOnly}
	}
	mode := target.kanban.Mode
	if strings.TrimSpace(projectID) == "" {
		mode = workflowconfig.KanbanModeReadOnly
	}
	states := kanbanStateNames(target.workflow, snapshot)
	return templates.KanbanData{
		Mode:               mode,
		ProjectID:          strings.TrimSpace(projectID),
		States:             states,
		AllowedTransitions: kanbanAllowedTransitions(target.workflow, states),
	}
}

func (s *Server) kanbanCardFresh(projectID string, issueID string, currentState string) (bool, string) {
	currentState = strings.TrimSpace(currentState)
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false, ""
	}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		if !sameKanbanIssue(issue, projectID, issueID, snapshot.Project.ID) {
			continue
		}
		state := strings.TrimSpace(issue.State)
		if currentState == "" || normalizeKanbanState(state) == normalizeKanbanState(currentState) {
			return true, state
		}
		return false, state
	}
	return false, ""
}

func (s *Server) kanbanCommentTargetKnown(req kanbanCommentRequest) bool {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false
	}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		if !sameKanbanProject(issue, req.projectID, snapshot.Project.ID) {
			continue
		}
		switch req.target {
		case "issue":
			if strings.TrimSpace(issue.ID) == strings.TrimSpace(req.issueID) {
				return true
			}
		case "pr":
			if issue.PullRequest == nil || issue.PullRequest.Number != req.prNumber {
				continue
			}
			if strings.EqualFold(kanbanPullRequestRepository(issue), req.prRepository) {
				return true
			}
		}
	}
	return false
}

func (s *Server) requestKanbanRefresh(ctx context.Context) {
	if s.refresher == nil {
		return
	}
	if _, err := s.refresher.RequestRefresh(ctx); err != nil {
		s.logger.DebugContext(ctx, "kanban refresh request failed", "error", err)
	}
}

func kanbanFeedback(c echo.Context, status int, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = http.StatusText(status)
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		class := "border-success bg-success-soft text-success"
		if status >= http.StatusBadRequest {
			class = "border-danger bg-danger-soft text-danger"
		}
		return c.HTML(status, `<div id="kanban-feedback" role="status" aria-live="polite" class="rounded-md border px-3 py-2 text-sm `+class+`">`+html.EscapeString(message)+`</div>`)
	}
	if status >= http.StatusBadRequest {
		return c.JSON(status, errorResponse("kanban_action_failed", message))
	}
	return c.JSON(status, map[string]any{"ok": true, "message": message})
}

func kanbanStateAllowed(cfg workflowconfig.Config, state string) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	states := kanbanStateNames(cfg, telemetry.Snapshot{})
	if len(states) == 0 {
		return true
	}
	for _, configured := range states {
		if normalizeKanbanState(configured) == normalizeKanbanState(state) {
			return true
		}
	}
	return false
}

func kanbanStateNames(cfg workflowconfig.Config, snapshot telemetry.Snapshot) []string {
	states := make([]string, 0, len(cfg.Tracker.ActiveStates)+len(cfg.Tracker.ObservedStates)+len(cfg.Tracker.TerminalStates))
	seen := map[string]struct{}{}
	add := func(values ...string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := normalizeKanbanState(value)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			states = append(states, value)
		}
	}
	add(cfg.KanbanStateNames()...)
	for _, issue := range snapshotKanbanIssues(snapshot) {
		add(issue.State)
	}
	return states
}

func kanbanAllowedTransitions(cfg workflowconfig.Config, states []string) map[string][]string {
	out := make(map[string][]string, len(states))
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		out[state] = cfg.KanbanAllowedTransitionTargets(state)
	}
	return out
}

func snapshotKanbanIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := make([]telemetry.Issue, 0, len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked)+len(snapshot.Completed))
	issues = append(issues, snapshot.Pipeline...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Completed {
		issues = append(issues, row.Issue)
	}
	return issues
}

func sameKanbanIssue(issue telemetry.Issue, projectID string, issueID string, snapshotProjectID string) bool {
	if strings.TrimSpace(issue.ID) != strings.TrimSpace(issueID) {
		return false
	}
	return sameKanbanProject(issue, projectID, snapshotProjectID)
}

func sameKanbanProject(issue telemetry.Issue, projectID string, snapshotProjectID string) bool {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return true
	}
	issueProjectID := strings.TrimSpace(issue.ProjectID)
	if issueProjectID != "" {
		return issueProjectID == projectID
	}
	return strings.TrimSpace(snapshotProjectID) == "" || strings.TrimSpace(snapshotProjectID) == projectID
}

func kanbanIssueRepository(identifier string) string {
	repo, _, ok := strings.Cut(strings.TrimSpace(identifier), "#")
	if !ok {
		return ""
	}
	return strings.TrimSpace(repo)
}

func kanbanPullRequestRepository(issue telemetry.Issue) string {
	if issue.PullRequest != nil {
		if repository := kanbanRepositoryFromPullRequestURL(issue.PullRequest.URL); repository != "" {
			return repository
		}
	}
	return kanbanIssueRepository(issue.Identifier)
}

func kanbanRepositoryFromPullRequestURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return ""
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}

func mappedKanbanState(cfg workflowconfig.Config, state string) string {
	state = strings.TrimSpace(state)
	if !cfg.Tracker.StateMap.IsMap {
		return state
	}
	if mapped, ok := cfg.Tracker.StateMap.Map[state]; ok {
		if value, ok := mapped.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	normalized := normalizeKanbanState(state)
	for detentState, mapped := range cfg.Tracker.StateMap.Map {
		if normalizeKanbanState(detentState) != normalized {
			continue
		}
		if value, ok := mapped.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return state
}

func normalizeKanbanState(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}
