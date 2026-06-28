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
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type kanbanMutationLocks struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	states  map[string]kanbanPendingState
	removed map[string]kanbanPendingRemoval
}

type kanbanPendingState struct {
	snapshot string
	current  string
}

type kanbanPendingRemoval struct {
	snapshot  string
	removedAt time.Time
}

type kanbanActionTarget struct {
	key       string
	connector connector.Connector
	workflow  workflowconfig.Config
	kanban    workflowconfig.Kanban
}

type kanbanMoveRequest struct {
	projectID    string
	board        string
	issueID      string
	currentState string
	targetState  string
	prNumber     int
	drag         bool
}

type kanbanRemoveRequest struct {
	projectID    string
	issueID      string
	currentState string
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

const (
	kanbanDialogContentTarget = "#kanban-dialog-content"
	kanbanProjectBoardTarget  = "#project-kanban"
	kanbanFleetBoardTarget    = "#fleet-kanban"
	kanbanDialogSucceeded     = "kanbanActionSucceeded"
	kanbanRemovalPendingTTL   = 5 * time.Minute
)

func newKanbanMutationLocks() *kanbanMutationLocks {
	return &kanbanMutationLocks{
		locks:   map[string]*sync.Mutex{},
		states:  map[string]kanbanPendingState{},
		removed: map[string]kanbanPendingRemoval{},
	}
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

func (l *kanbanMutationLocks) cardState(key string, issueID string, snapshotState string) string {
	stateKey := kanbanMutationStateKey(key, issueID)
	if stateKey == "" {
		return snapshotState
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	pending, ok := l.states[stateKey]
	if !ok {
		return snapshotState
	}
	switch {
	case normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.snapshot):
		return pending.current
	case normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.current):
		delete(l.states, stateKey)
		return snapshotState
	default:
		delete(l.states, stateKey)
		return snapshotState
	}
}

func (l *kanbanMutationLocks) noteCardState(key string, issueID string, snapshotState string, currentState string) {
	stateKey := kanbanMutationStateKey(key, issueID)
	if stateKey == "" || strings.TrimSpace(currentState) == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if pending, ok := l.states[stateKey]; ok && normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.snapshot) {
		snapshotState = pending.snapshot
	}
	l.states[stateKey] = kanbanPendingState{
		snapshot: strings.TrimSpace(snapshotState),
		current:  strings.TrimSpace(currentState),
	}
}

func (l *kanbanMutationLocks) cardRemoved(key string, issueID string, snapshotState string) bool {
	stateKey := kanbanMutationStateKey(key, issueID)
	if stateKey == "" {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	removed, ok := l.removed[stateKey]
	if !ok {
		return false
	}
	if time.Since(removed.removedAt) > kanbanRemovalPendingTTL {
		delete(l.removed, stateKey)
		return false
	}
	if normalizeKanbanState(snapshotState) == normalizeKanbanState(removed.snapshot) {
		return true
	}
	delete(l.removed, stateKey)
	return false
}

func (l *kanbanMutationLocks) noteCardRemoved(key string, issueID string, snapshotState string) {
	stateKey := kanbanMutationStateKey(key, issueID)
	if stateKey == "" {
		return
	}

	l.mu.Lock()
	l.removed[stateKey] = kanbanPendingRemoval{
		snapshot:  strings.TrimSpace(snapshotState),
		removedAt: time.Now(),
	}
	l.mu.Unlock()
}

func kanbanMutationStateKey(key string, issueID string) string {
	key = strings.TrimSpace(key)
	issueID = strings.TrimSpace(issueID)
	if key == "" || issueID == "" {
		return ""
	}
	return key + "\x00" + issueID
}

func (s *Server) apiKanbanMoveDialog(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok && scenario.Page == "api" && strings.HasPrefix(scenario.ID, "api-kanban-move") {
		switch scenario.Variant {
		case "kanban-read-only":
			return render(c, templates.KanbanDialogErrorContent("Kanban integration mode is not enabled."))
		case "kanban-move-missing-target":
			return render(c, templates.KanbanDialogErrorContent("Target state is required."))
		default:
			return render(c, templates.KanbanMoveDialogContent(templates.KanbanMoveDialogData{
				ProjectID:    demoPrimaryProjectID,
				IssueID:      "demo-todo",
				Identifier:   "digitaldrywood/detent-core#5251",
				Title:        "Add screenshot manifest smoke test",
				CurrentState: "Todo",
				TargetState:  "In Progress",
				States:       []string{"In Progress", "Blocked", "Cancelled"},
			}))
		}
	}
	data, response := s.kanbanMoveDialogData(c, "")
	if response != "" {
		return render(c, templates.KanbanDialogErrorContent(response))
	}
	return render(c, templates.KanbanMoveDialogContent(data))
}

func (s *Server) apiKanbanMove(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok && scenario.Page == "api" && strings.HasPrefix(scenario.ID, "api-kanban-move") {
		switch scenario.Variant {
		case "kanban-transition-blocked":
			return kanbanFeedback(c, http.StatusUnprocessableEntity, "Move from Done to Todo is not allowed by the Kanban transition policy.")
		case "connector-failure":
			return kanbanFeedback(c, http.StatusBadGateway, "Move failed: demo connector failure")
		case "kanban-read-only":
			return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
		default:
			return s.demoKanbanMoveSuccess(c, scenario)
		}
	}
	req, response, status := parseKanbanMoveRequest(c)
	if response != "" {
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, "Kanban integration mode is not enabled.")
		}
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if req.issueID == "" {
		if req.prNumber > 0 {
			if kanbanDialogForm(c) {
				return s.kanbanMoveDialogValidation(c, "Cannot move PR-only card without a linked issue.")
			}
			return kanbanFeedback(c, http.StatusUnprocessableEntity, "Cannot move PR-only card without a linked issue.")
		}
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, "Issue id is required.")
		}
		return kanbanFeedback(c, http.StatusBadRequest, "Issue id is required.")
	}
	if !kanbanStateAllowed(target.workflow, req.targetState) {
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, "Target state is not configured for this board.")
		}
		return kanbanFeedback(c, http.StatusBadRequest, "Target state is not configured for this board.")
	}
	var feedback string
	var feedbackStatus int
	err := s.kanbanMutations.withLock(target.key, func() error {
		currentState := req.currentState
		ok, current, snapshotState, snapshotIssue := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
		if !ok {
			feedback = "Card is stale; refresh and retry."
			if current != "" {
				feedback = fmt.Sprintf("Card is stale; current state is %s.", current)
			}
			feedbackStatus = http.StatusConflict
			return nil
		}
		if strings.TrimSpace(current) != "" {
			currentState = current
		}
		if !target.workflow.KanbanTransitionAllowed(currentState, req.targetState) {
			feedback = fmt.Sprintf("Move from %s to %s is not allowed by the Kanban transition policy.", currentState, req.targetState)
			feedbackStatus = http.StatusUnprocessableEntity
			return nil
		}

		if target.kanban.IssueStateFieldID > 0 {
			setter, ok := target.connector.(connector.IssueFieldSetter)
			if !ok {
				return connector.ErrNotImplemented
			}
			if err := setter.SetIssueField(c.Request().Context(), req.issueID, target.kanban.IssueStateFieldID, mappedKanbanState(target.workflow, req.targetState)); err != nil {
				return err
			}
			s.recordKanbanLaneTransition(c.Request().Context(), req.projectID, snapshotIssue, currentState, req.targetState, "kanban_move_field")
			s.kanbanMutations.noteCardState(target.key, req.issueID, snapshotState, req.targetState)
			return nil
		}
		if err := target.connector.UpdateIssueState(c.Request().Context(), req.issueID, req.targetState); err != nil {
			return err
		}
		s.recordKanbanLaneTransition(c.Request().Context(), req.projectID, snapshotIssue, currentState, req.targetState, "kanban_move")
		s.kanbanMutations.noteCardState(target.key, req.issueID, snapshotState, req.targetState)
		return nil
	})
	if feedback != "" {
		if kanbanDialogForm(c) {
			return s.kanbanMoveDialogValidation(c, feedback)
		}
		return kanbanFeedback(c, feedbackStatus, feedback)
	}
	if err != nil {
		s.logger.WarnContext(c.Request().Context(), "kanban move failed", "project", req.projectID, "issue_id", req.issueID, "target_state", req.targetState, "error", err)
		return kanbanFeedback(c, http.StatusBadGateway, "Move failed: "+err.Error())
	}
	return s.kanbanMoveSuccess(c, req, "Moved card to "+req.targetState+".")
}

func (s *Server) kanbanMoveSuccess(c echo.Context, req kanbanMoveRequest, message string) error {
	ctx := c.Request().Context()
	s.requestKanbanRefresh(ctx)
	if c.Request().Header.Get("HX-Request") != "true" || strings.TrimSpace(req.projectID) == "" {
		return kanbanFeedback(c, http.StatusOK, message)
	}
	if strings.EqualFold(strings.TrimSpace(req.board), "fleet") {
		data := s.fleetKanbanData(ctx, s.latestSnapshot(ctx))
		data.Kanban.Feedback = message
		data.Kanban.FeedbackKind = "success"

		c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
		c.Response().Header().Set("HX-Retarget", kanbanFleetBoardTarget)
		c.Response().Header().Set("HX-Reswap", "outerHTML")
		return render(c, templates.ProjectKanbanSnapshot(data))
	}

	data, ok := s.projectDashboardData(ctx, req.projectID, s.latestSnapshot(ctx))
	if !ok {
		return kanbanFeedback(c, http.StatusOK, message)
	}
	if !req.drag {
		data.Kanban.Feedback = message
		data.Kanban.FeedbackKind = "success"
	}

	c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
	c.Response().Header().Set("HX-Retarget", kanbanProjectBoardTarget)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return render(c, templates.ProjectKanbanSnapshot(data))
}

func (s *Server) apiKanbanRemove(c echo.Context) error {
	req, response, status := parseKanbanRemoveRequest(c)
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
			return kanbanFeedback(c, http.StatusUnprocessableEntity, "Cannot remove PR-only card without a linked issue.")
		}
		return kanbanFeedback(c, http.StatusBadRequest, "Issue id is required.")
	}

	var feedback string
	var feedbackStatus int
	err := s.kanbanMutations.withLock(target.key, func() error {
		currentState := req.currentState
		ok, current, snapshotState, _ := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
		if !ok {
			feedback = "Card is stale; refresh and retry."
			if current != "" {
				feedback = fmt.Sprintf("Card is stale; current state is %s.", current)
			}
			feedbackStatus = http.StatusConflict
			return nil
		}
		if strings.TrimSpace(current) != "" {
			currentState = current
		}
		if target.kanban.IssueStateFieldID > 0 {
			clearer, ok := target.connector.(connector.IssueFieldClearer)
			if !ok {
				return connector.ErrNotImplemented
			}
			if err := clearer.ClearIssueField(c.Request().Context(), req.issueID, target.kanban.IssueStateFieldID); err != nil {
				return err
			}
			if strings.TrimSpace(snapshotState) == "" {
				snapshotState = currentState
			}
			s.kanbanMutations.noteCardRemoved(target.key, req.issueID, snapshotState)
			return nil
		}
		remover, ok := target.connector.(connector.ProjectRemover)
		if !ok {
			return connector.ErrNotImplemented
		}
		if err := remover.RemoveIssueFromProject(c.Request().Context(), req.issueID); err != nil {
			return err
		}
		if strings.TrimSpace(snapshotState) == "" {
			snapshotState = currentState
		}
		s.kanbanMutations.noteCardRemoved(target.key, req.issueID, snapshotState)
		return nil
	})
	if feedback != "" {
		return kanbanFeedback(c, feedbackStatus, feedback)
	}
	if err != nil {
		s.logger.WarnContext(c.Request().Context(), "kanban remove failed", "project", req.projectID, "issue_id", req.issueID, "error", err)
		return kanbanFeedback(c, http.StatusBadGateway, "Remove failed: "+err.Error())
	}
	return s.kanbanRemoveSuccess(c, req, "Removed card from project.")
}

func (s *Server) kanbanRemoveSuccess(c echo.Context, req kanbanRemoveRequest, message string) error {
	ctx := c.Request().Context()
	s.requestKanbanRefresh(ctx)
	if c.Request().Header.Get("HX-Request") != "true" || strings.TrimSpace(req.projectID) == "" {
		return kanbanFeedback(c, http.StatusOK, message)
	}

	data, ok := s.projectDashboardData(ctx, req.projectID, s.latestSnapshot(ctx))
	if !ok {
		return kanbanFeedback(c, http.StatusOK, message)
	}
	data.Kanban.Feedback = message
	data.Kanban.FeedbackKind = "success"

	c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
	c.Response().Header().Set("HX-Retarget", kanbanProjectBoardTarget)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return render(c, templates.ProjectKanbanSnapshot(data))
}

func (s *Server) kanbanSnapshotWithPendingStates(lockKey string, projectID string, snapshot telemetry.Snapshot) telemetry.Snapshot {
	if s.kanbanMutations == nil {
		return snapshot
	}
	snapshot = cloneKanbanIssueSlices(snapshot)
	filterSnapshotKanbanIssues(&snapshot, func(issue telemetry.Issue) bool {
		if strings.TrimSpace(issue.ID) == "" || !sameKanbanProject(issue, projectID, snapshot.Project.ID) {
			return true
		}
		return !s.kanbanMutations.cardRemoved(lockKey, issue.ID, issue.State)
	})
	applySnapshotKanbanIssues(&snapshot, func(issue *telemetry.Issue) {
		if issue == nil || strings.TrimSpace(issue.ID) == "" || !sameKanbanProject(*issue, projectID, snapshot.Project.ID) {
			return
		}
		state := s.kanbanMutations.cardState(lockKey, issue.ID, issue.State)
		if strings.TrimSpace(state) != "" {
			issue.State = state
		}
	})
	states := kanbanIssueStateIndex(snapshot)
	applySnapshotKanbanIssues(&snapshot, func(issue *telemetry.Issue) {
		if issue == nil || len(issue.BlockedBy) == 0 || !sameKanbanProject(*issue, projectID, snapshot.Project.ID) {
			return
		}
		issue.BlockedBy = kanbanBlockedRefsWithCurrentStates(issue.BlockedBy, states)
	})
	return snapshot
}

func filterSnapshotKanbanIssues(snapshot *telemetry.Snapshot, keep func(telemetry.Issue) bool) {
	if snapshot == nil || keep == nil {
		return
	}
	snapshot.BoardIssues = filterTelemetryIssues(snapshot.BoardIssues, keep)
	snapshot.Pipeline = filterTelemetryIssues(snapshot.Pipeline, keep)
	snapshot.Running = filterTelemetryRunning(snapshot.Running, keep)
	snapshot.Queue = filterTelemetryQueued(snapshot.Queue, keep)
	snapshot.Blocked = filterTelemetryBlocked(snapshot.Blocked, keep)
}

func filterTelemetryIssues(issues []telemetry.Issue, keep func(telemetry.Issue) bool) []telemetry.Issue {
	out := issues[:0]
	for _, issue := range issues {
		if keep(issue) {
			out = append(out, issue)
		}
	}
	return out
}

func filterTelemetryRunning(rows []telemetry.Running, keep func(telemetry.Issue) bool) []telemetry.Running {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func filterTelemetryQueued(rows []telemetry.Queued, keep func(telemetry.Issue) bool) []telemetry.Queued {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func filterTelemetryBlocked(rows []telemetry.Blocked, keep func(telemetry.Issue) bool) []telemetry.Blocked {
	out := rows[:0]
	for _, row := range rows {
		if keep(row.Issue) {
			out = append(out, row)
		}
	}
	return out
}

func cloneKanbanIssueSlices(snapshot telemetry.Snapshot) telemetry.Snapshot {
	snapshot.BoardIssues = append([]telemetry.Issue(nil), snapshot.BoardIssues...)
	snapshot.Pipeline = append([]telemetry.Issue(nil), snapshot.Pipeline...)
	snapshot.Running = append([]telemetry.Running(nil), snapshot.Running...)
	snapshot.Queue = append([]telemetry.Queued(nil), snapshot.Queue...)
	snapshot.Blocked = append([]telemetry.Blocked(nil), snapshot.Blocked...)
	snapshot.Completed = append([]telemetry.Completed(nil), snapshot.Completed...)
	return snapshot
}

func applySnapshotKanbanIssues(snapshot *telemetry.Snapshot, apply func(*telemetry.Issue)) {
	if snapshot == nil || apply == nil {
		return
	}
	for i := range snapshot.BoardIssues {
		apply(&snapshot.BoardIssues[i])
	}
	for i := range snapshot.Pipeline {
		apply(&snapshot.Pipeline[i])
	}
	for i := range snapshot.Running {
		apply(&snapshot.Running[i].Issue)
	}
	for i := range snapshot.Queue {
		apply(&snapshot.Queue[i].Issue)
	}
	for i := range snapshot.Blocked {
		apply(&snapshot.Blocked[i].Issue)
	}
	for i := range snapshot.Completed {
		apply(&snapshot.Completed[i].Issue)
	}
}

func kanbanIssueStateIndex(snapshot telemetry.Snapshot) map[string]string {
	states := map[string]string{}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		state := strings.TrimSpace(issue.State)
		if state == "" {
			continue
		}
		for _, key := range kanbanIssueStateKeys(issue.ID, issue.Identifier) {
			states[key] = state
		}
	}
	return states
}

func kanbanBlockedRefsWithCurrentStates(refs []telemetry.BlockedRef, states map[string]string) []telemetry.BlockedRef {
	if len(refs) == 0 {
		return refs
	}
	out := append([]telemetry.BlockedRef(nil), refs...)
	for i := range out {
		for _, key := range kanbanIssueStateKeys(out[i].ID, out[i].Identifier) {
			if state := strings.TrimSpace(states[key]); state != "" {
				out[i].State = state
				break
			}
		}
	}
	return out
}

func kanbanIssueStateKeys(id string, identifier string) []string {
	keys := []string{}
	if id = strings.TrimSpace(id); id != "" {
		keys = append(keys, "id:"+id)
	}
	if identifier = strings.ToLower(strings.TrimSpace(identifier)); identifier != "" {
		keys = append(keys, "identifier:"+identifier)
	}
	return keys
}

func (s *Server) apiKanbanCommentDialog(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok && scenario.Page == "api" && strings.HasPrefix(scenario.ID, "api-kanban-comment") {
		switch scenario.Variant {
		case "kanban-comment-invalid-target":
			return render(c, templates.KanbanDialogErrorContent("Comment target is not available on the current board."))
		case "kanban-comment-pr":
			return render(c, templates.KanbanCommentDialogContent(templates.KanbanCommentDialogData{
				ProjectID:    demoPrimaryProjectID,
				Target:       "pr",
				PRRepository: "digitaldrywood/detent-core",
				PRNumber:     5290,
				Identifier:   "digitaldrywood/detent-core#5290",
				Title:        "Review deterministic chart colors",
				Body:         "Looks good for the screenshot demo.",
			}))
		default:
			return render(c, templates.KanbanCommentDialogContent(templates.KanbanCommentDialogData{
				ProjectID:  demoPrimaryProjectID,
				Target:     "issue",
				IssueID:    "demo-todo",
				Identifier: "digitaldrywood/detent-core#5251",
				Title:      "Add screenshot manifest smoke test",
				Body:       "Please verify the screenshot manifest route.",
			}))
		}
	}
	data, response := s.kanbanCommentDialogData(c, "")
	if response != "" {
		return render(c, templates.KanbanDialogErrorContent(response))
	}
	return render(c, templates.KanbanCommentDialogContent(data))
}

func (s *Server) apiKanbanComment(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok && scenario.Page == "api" && strings.HasPrefix(scenario.ID, "api-kanban-comment") {
		switch scenario.Variant {
		case "kanban-comment-empty-body":
			return kanbanFeedback(c, http.StatusBadRequest, "Comment body is required.")
		case "connector-failure":
			return kanbanFeedback(c, http.StatusBadGateway, "Comment failed: demo connector failure")
		default:
			return kanbanFeedback(c, http.StatusOK, "Comment submitted.")
		}
	}
	req, response, status := parseKanbanCommentRequest(c)
	if response != "" {
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, "Kanban integration mode is not enabled.")
		}
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if !s.kanbanCommentTargetKnown(req) {
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, "Comment target is not available on the current board.")
		}
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

func (s *Server) kanbanMoveDialogValidation(c echo.Context, message string) error {
	c.Response().Header().Set("HX-Retarget", kanbanDialogContentTarget)
	c.Response().Header().Set("HX-Reswap", "innerHTML")
	data, response := s.kanbanMoveDialogData(c, message)
	if response != "" {
		return render(c, templates.KanbanDialogErrorContent(response))
	}
	return render(c, templates.KanbanMoveDialogContent(data))
}

func (s *Server) kanbanCommentDialogValidation(c echo.Context, message string) error {
	c.Response().Header().Set("HX-Retarget", kanbanDialogContentTarget)
	c.Response().Header().Set("HX-Reswap", "innerHTML")
	data, response := s.kanbanCommentDialogData(c, message)
	if response != "" {
		return render(c, templates.KanbanDialogErrorContent(response))
	}
	return render(c, templates.KanbanCommentDialogContent(data))
}

func (s *Server) kanbanMoveDialogData(c echo.Context, message string) (templates.KanbanMoveDialogData, string) {
	data := templates.KanbanMoveDialogData{
		ProjectID:    kanbanRequestValue(c, "project_id"),
		Board:        kanbanRequestValue(c, "kanban_board"),
		IssueID:      kanbanRequestValue(c, "issue_id"),
		Identifier:   kanbanRequestValue(c, "identifier"),
		Title:        kanbanRequestValue(c, "title"),
		CurrentState: kanbanRequestValue(c, "current_state"),
		TargetState:  kanbanRequestValue(c, "target_state"),
		Error:        message,
	}
	if value := kanbanRequestValue(c, "pr_number"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			data.Error = "PR number is invalid."
		} else {
			data.PRNumber = number
		}
	}

	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		return data, response
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return data, "Kanban integration mode is not enabled."
	}
	data.States = target.workflow.KanbanAllowedTransitionTargets(data.CurrentState)
	if len(data.States) == 0 && data.CurrentState == "" {
		data.States = kanbanStateNames(target.workflow, s.latestSnapshot(c.Request().Context()))
	}
	if data.TargetState == "" {
		data.TargetState = kanbanMoveDialogDefaultTarget(data.CurrentState, data.States)
	}
	return data, ""
}

func kanbanMoveDialogDefaultTarget(source string, allowedTargets []string) string {
	if len(allowedTargets) == 0 {
		return ""
	}
	preferred := kanbanMoveDialogPreferredTarget(source)
	if preferred != "" {
		for _, target := range allowedTargets {
			target = strings.TrimSpace(target)
			if normalizeKanbanState(target) == normalizeKanbanState(preferred) {
				return target
			}
		}
	}
	for _, target := range allowedTargets {
		if target = strings.TrimSpace(target); target != "" {
			return target
		}
	}
	return ""
}

func kanbanMoveDialogPreferredTarget(source string) string {
	switch normalizeKanbanState(source) {
	case "backlog", "blocked":
		return "Todo"
	case "todo", "rework":
		return "In Progress"
	case "in progress":
		return "Human Review"
	case "human review":
		return "Merging"
	default:
		return ""
	}
}

func (s *Server) kanbanCommentDialogData(c echo.Context, message string) (templates.KanbanCommentDialogData, string) {
	data := templates.KanbanCommentDialogData{
		ProjectID:    kanbanRequestValue(c, "project_id"),
		Target:       strings.ToLower(kanbanRequestValue(c, "target")),
		IssueID:      kanbanRequestValue(c, "issue_id"),
		PRRepository: kanbanRequestValue(c, "pr_repository"),
		Identifier:   kanbanRequestValue(c, "identifier"),
		Title:        kanbanRequestValue(c, "title"),
		Body:         kanbanRequestValue(c, "body"),
		Error:        message,
	}
	if data.Target == "" {
		data.Target = "issue"
	}
	if value := kanbanRequestValue(c, "pr_number"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			data.Error = "PR number is invalid."
		} else {
			data.PRNumber = number
		}
	}

	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		return data, response
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return data, "Kanban integration mode is not enabled."
	}
	return data, ""
}

func parseKanbanMoveRequest(c echo.Context) (kanbanMoveRequest, string, int) {
	req := kanbanMoveRequest{
		projectID:    strings.TrimSpace(c.FormValue("project_id")),
		board:        strings.TrimSpace(c.FormValue("kanban_board")),
		issueID:      strings.TrimSpace(c.FormValue("issue_id")),
		currentState: strings.TrimSpace(c.FormValue("current_state")),
		targetState:  strings.TrimSpace(c.FormValue("target_state")),
		drag:         strings.EqualFold(strings.TrimSpace(c.FormValue("kanban_drag")), "true"),
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

func parseKanbanRemoveRequest(c echo.Context) (kanbanRemoveRequest, string, int) {
	req := kanbanRemoveRequest{
		projectID:    strings.TrimSpace(c.FormValue("project_id")),
		issueID:      strings.TrimSpace(c.FormValue("issue_id")),
		currentState: strings.TrimSpace(c.FormValue("current_state")),
	}
	if value := strings.TrimSpace(c.FormValue("pr_number")); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			return kanbanRemoveRequest{}, "PR number is invalid.", http.StatusBadRequest
		}
		req.prNumber = number
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

func kanbanDialogForm(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" && strings.EqualFold(strings.TrimSpace(c.FormValue("kanban_dialog")), "true")
}

func kanbanRequestValue(c echo.Context, key string) string {
	if c.Request().Method == http.MethodGet {
		return strings.TrimSpace(c.QueryParam(key))
	}
	return strings.TrimSpace(c.FormValue(key))
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

	workflow := s.kanbanWorkflow
	actionConnector := s.connector
	if trackedProject := s.firstKanbanActionProject(); trackedProject != nil {
		workflow = trackedProject.Workflow().Config
		workflow.Server.Kanban = s.kanban
		if projectConnector := trackedProject.Connector(); projectConnector != nil {
			actionConnector = projectConnector
		}
	}
	if actionConnector == nil {
		return kanbanActionTarget{}, "Connector not configured.", http.StatusServiceUnavailable
	}
	return kanbanActionTarget{
		key:       "connector:" + actionConnector.Name(),
		connector: actionConnector,
		workflow:  workflow,
		kanban:    s.kanban,
	}, "", 0
}

func (s *Server) firstKanbanActionProject() *project.Project {
	if s.registry == nil {
		return nil
	}
	for _, trackedProject := range s.registry.List() {
		if trackedProject != nil && trackedProject.Connector() != nil {
			return trackedProject
		}
	}
	return nil
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
	data := templates.KanbanData{
		Mode:                    mode,
		ProjectID:               strings.TrimSpace(projectID),
		States:                  states,
		TerminalStates:          target.workflow.Tracker.TerminalStates,
		TerminalStatesByProject: s.kanbanTerminalStatesByProject(projectID),
		AllowedTransitions:      kanbanAllowedTransitions(target.workflow, states),
	}
	if strings.TrimSpace(projectID) == "" {
		data.Projects = s.kanbanProjectsData(snapshot)
	}
	return data
}

func (s *Server) kanbanProjectsData(snapshot telemetry.Snapshot) map[string]templates.KanbanProjectData {
	if s.registry == nil {
		return nil
	}
	projects := s.registry.List()
	if len(projects) == 0 {
		return nil
	}
	out := make(map[string]templates.KanbanProjectData, len(projects))
	for _, trackedProject := range projects {
		if trackedProject == nil {
			continue
		}
		projectID := strings.TrimSpace(string(trackedProject.ID()))
		if projectID == "" {
			continue
		}
		target, _, _ := s.kanbanActionTarget(projectID)
		if target.connector == nil {
			continue
		}
		states := kanbanStateNames(target.workflow, snapshot)
		out[projectID] = templates.KanbanProjectData{
			Mode:               target.kanban.Mode,
			ProjectID:          projectID,
			States:             states,
			TerminalStates:     target.workflow.Tracker.TerminalStates,
			AllowedTransitions: kanbanAllowedTransitions(target.workflow, states),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) fleetKanbanSnapshotWithPendingStates(snapshot telemetry.Snapshot) telemetry.Snapshot {
	if s.registry != nil {
		applied := false
		for _, trackedProject := range s.registry.List() {
			if trackedProject == nil {
				continue
			}
			projectID := strings.TrimSpace(string(trackedProject.ID()))
			if projectID == "" {
				continue
			}
			target, _, _ := s.kanbanActionTarget(projectID)
			if target.key == "" {
				continue
			}
			snapshot = s.kanbanSnapshotWithPendingStates(target.key, projectID, snapshot)
			applied = true
		}
		if applied {
			return snapshot
		}
	}
	if target, _, _ := s.kanbanActionTarget(""); target.key != "" {
		return s.kanbanSnapshotWithPendingStates(target.key, "", snapshot)
	}
	return snapshot
}

func (s *Server) kanbanTerminalStatesByProject(projectID string) map[string][]string {
	if s.registry == nil {
		return nil
	}

	out := map[string][]string{}
	add := func(id string, states []string) {
		id = strings.TrimSpace(id)
		if id == "" || len(states) == 0 {
			return
		}
		out[id] = append([]string(nil), states...)
	}

	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		if trackedProject, ok := s.registry.Get(project.ID(projectID)); ok {
			add(projectID, trackedProject.Workflow().Config.Tracker.TerminalStates)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	for _, trackedProject := range s.registry.List() {
		if trackedProject == nil {
			continue
		}
		add(string(trackedProject.ID()), trackedProject.Workflow().Config.Tracker.TerminalStates)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) kanbanCardFresh(lockKey string, projectID string, issueID string, currentState string) (bool, string, string, telemetry.Issue) {
	currentState = strings.TrimSpace(currentState)
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false, "", "", telemetry.Issue{}
	}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		if !sameKanbanIssue(issue, projectID, issueID, snapshot.Project.ID) {
			continue
		}
		snapshotState := strings.TrimSpace(issue.State)
		state := snapshotState
		if s.kanbanMutations != nil {
			state = s.kanbanMutations.cardState(lockKey, issueID, snapshotState)
		}
		if currentState == "" || normalizeKanbanState(state) == normalizeKanbanState(currentState) {
			return true, state, snapshotState, issue
		}
		return false, state, snapshotState, issue
	}
	return false, "", "", telemetry.Issue{}
}

func (s *Server) recordKanbanLaneTransition(
	ctx context.Context,
	projectID string,
	issue telemetry.Issue,
	currentState string,
	targetState string,
	reason string,
) {
	if s.store == nil {
		return
	}
	currentState = strings.TrimSpace(currentState)
	targetState = strings.TrimSpace(targetState)
	if targetState == "" || normalizeKanbanState(currentState) == normalizeKanbanState(targetState) {
		return
	}
	now := time.Now()
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(issue.ProjectID)
	}
	if projectID == "" {
		projectID = "default"
	}
	base := store.WorkflowPhaseEvent{
		ProjectID:      projectID,
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		IssueURL:       issue.URL,
		PRNumber:       kanbanWorkflowPRNumber(issue),
		PhaseType:      store.WorkflowPhaseTypeLane,
		Reason:         strings.TrimSpace(reason),
		StartedAt:      now,
		EndpointFamily: "tracker",
		MetadataJSON:   "{}",
	}
	if base.Reason == "" {
		base.Reason = "kanban_move"
	}
	if currentState != "" {
		startedAt := kanbanLaneStartedAt(issue, now)
		exitEvent := base
		exitEvent.PhaseName = currentState
		exitEvent.Status = "exited"
		exitEvent.StartedAt = startedAt
		exitEvent.FinishedAt = now
		exitEvent.DurationSeconds = kanbanWorkflowDurationSeconds(startedAt, now)
		if _, err := s.store.RecordWorkflowPhaseEvent(ctx, exitEvent); err != nil && s.logger != nil {
			s.logger.WarnContext(ctx, "record kanban lane exit metric failed", "project", projectID, "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", currentState, "target_state", targetState, "error", err)
		}
	}
	enterEvent := base
	enterEvent.PhaseName = targetState
	enterEvent.PreviousPhaseName = currentState
	enterEvent.Status = "entered"
	if _, err := s.store.RecordWorkflowPhaseEvent(ctx, enterEvent); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "record kanban lane enter metric failed", "project", projectID, "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", currentState, "target_state", targetState, "error", err)
	}
}

func kanbanLaneStartedAt(issue telemetry.Issue, fallback time.Time) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate == nil || candidate.IsZero() || candidate.After(fallback) {
			continue
		}
		return *candidate
	}
	return fallback
}

func kanbanWorkflowDurationSeconds(startedAt time.Time, finishedAt time.Time) int64 {
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return 0
	}
	return int64(finishedAt.Sub(startedAt) / time.Second)
}

func kanbanWorkflowPRNumber(issue telemetry.Issue) *int64 {
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return nil
	}
	value := int64(issue.PullRequest.Number)
	return &value
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
		} else {
			c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
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
	issues := make([]telemetry.Issue, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked))
	issues = append(issues, snapshot.BoardIssues...)
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
