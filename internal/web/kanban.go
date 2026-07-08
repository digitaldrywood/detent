package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"sort"
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
	snapshot    string
	current     string
	project     string
	issue       telemetry.Issue
	status      kanbanPendingStateStatus
	snapshotAt  time.Time
	confirmedAt time.Time
}

type kanbanPendingRemoval struct {
	snapshot  string
	removedAt time.Time
}

type kanbanPendingStateStatus string

const (
	kanbanPendingStateAwaitingCatchup kanbanPendingStateStatus = "awaiting_catchup"
	kanbanPendingStateConfirmed       kanbanPendingStateStatus = "confirmed"
)

type kanbanSnapshotIssueEntry struct {
	issue           telemetry.Issue
	state           string
	rank            int
	index           int
	rawRuntimeState bool
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

type kanbanCommentMutationRequest struct {
	projectID  string
	issueID    string
	commentID  string
	body       string
	mutateVerb string
}

const (
	kanbanDialogContentTarget = "#kanban-dialog-content"
	kanbanProjectBoardTarget  = "#snapshot"
	kanbanFleetBoardTarget    = "#snapshot"
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

func (l *kanbanMutationLocks) cardState(key string, issueID string, snapshotState string, snapshotAt time.Time) string {
	stateKey := kanbanMutationStateKey(key, issueID)
	if stateKey == "" {
		return snapshotState
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.cardStateLocked(stateKey, snapshotState, snapshotAt)
}

func (l *kanbanMutationLocks) cardStateLocked(stateKey string, snapshotState string, snapshotAt time.Time) string {
	pending, ok := l.states[stateKey]
	if !ok {
		return snapshotState
	}
	switch pending.status {
	case kanbanPendingStateConfirmed:
		if !pending.confirmedAt.IsZero() && !snapshotAt.IsZero() && !snapshotAt.After(pending.confirmedAt) {
			return pending.current
		}
		if normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.current) {
			return snapshotState
		}
		delete(l.states, stateKey)
		return snapshotState
	default:
		if !pending.snapshotAt.IsZero() && !snapshotAt.IsZero() && snapshotAt.Before(pending.snapshotAt) {
			return pending.current
		}
		switch {
		case normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.snapshot):
			return pending.current
		case normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.current):
			pending.status = kanbanPendingStateConfirmed
			pending.confirmedAt = snapshotAt
			l.states[stateKey] = pending
			return snapshotState
		default:
			delete(l.states, stateKey)
			return snapshotState
		}
	}
}

func (l *kanbanMutationLocks) noteCardState(key string, projectID string, issue telemetry.Issue, snapshotState string, currentState string, snapshotAt time.Time) {
	issue.ID = strings.TrimSpace(issue.ID)
	stateKey := kanbanMutationStateKey(key, issue.ID)
	if stateKey == "" || strings.TrimSpace(currentState) == "" {
		return
	}
	projectID = strings.TrimSpace(projectID)
	if strings.TrimSpace(issue.ProjectID) == "" {
		issue.ProjectID = projectID
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if pending, ok := l.states[stateKey]; ok && normalizeKanbanState(snapshotState) == normalizeKanbanState(pending.snapshot) {
		snapshotState = pending.snapshot
	}
	delete(l.removed, stateKey)
	l.states[stateKey] = kanbanPendingState{
		snapshot:   strings.TrimSpace(snapshotState),
		current:    strings.TrimSpace(currentState),
		project:    projectID,
		issue:      cloneKanbanIssue(issue),
		status:     kanbanPendingStateAwaitingCatchup,
		snapshotAt: snapshotAt,
	}
}

func (l *kanbanMutationLocks) pendingMovedCards(key string, projectID string, snapshot telemetry.Snapshot) []telemetry.Issue {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	present := map[string]struct{}{}
	for _, entry := range visibleSnapshotKanbanIssueEntries(snapshot) {
		issueID := strings.TrimSpace(entry.issue.ID)
		if issueID == "" || !sameKanbanProject(entry.issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := kanbanMutationStateKey(key, issueID)
		if _, ok := l.states[stateKey]; !ok {
			continue
		}
		present[stateKey] = struct{}{}
		l.cardStateLocked(stateKey, entry.state, snapshot.GeneratedAt)
	}
	for _, row := range snapshot.Completed {
		issue := row.Issue
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || !sameKanbanProject(issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := kanbanMutationStateKey(key, issueID)
		if _, ok := present[stateKey]; ok {
			continue
		}
		if _, ok := l.states[stateKey]; !ok {
			continue
		}
		snapshotState := strings.TrimSpace(issue.State)
		l.cardStateLocked(stateKey, snapshotState, snapshot.GeneratedAt)
	}

	projectID = strings.TrimSpace(projectID)
	out := []telemetry.Issue{}
	for stateKey, pending := range l.states {
		if _, ok := present[stateKey]; ok {
			continue
		}
		issueID := strings.TrimSpace(pending.issue.ID)
		if issueID == "" || kanbanMutationStateKey(key, issueID) != stateKey {
			continue
		}
		if projectID != "" && pending.project != "" && pending.project != projectID {
			continue
		}
		if !sameKanbanProject(pending.issue, projectID, snapshot.Project.ID) {
			continue
		}
		issue := cloneKanbanIssue(pending.issue)
		if strings.TrimSpace(issue.ProjectID) == "" {
			issue.ProjectID = projectID
		}
		issue.State = strings.TrimSpace(pending.current)
		out = append(out, issue)
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := strings.TrimSpace(out[i].Identifier)
		right := strings.TrimSpace(out[j].Identifier)
		if left != right {
			return left < right
		}
		return strings.TrimSpace(out[i].ID) < strings.TrimSpace(out[j].ID)
	})
	return out
}

func (l *kanbanMutationLocks) snapshotCardStates(key string, projectID string, snapshot telemetry.Snapshot) map[string]string {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	out := map[string]string{}
	for _, entry := range visibleSnapshotKanbanIssueEntries(snapshot) {
		issueID := strings.TrimSpace(entry.issue.ID)
		if issueID == "" || !sameKanbanProject(entry.issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := kanbanMutationStateKey(key, issueID)
		if _, ok := l.states[stateKey]; !ok {
			continue
		}
		state := l.cardStateLocked(stateKey, entry.state, snapshot.GeneratedAt)
		if strings.TrimSpace(state) != "" {
			out[stateKey] = state
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	delete(l.states, stateKey)
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
		ok, current, snapshotState, snapshotIssue, snapshotAt := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
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
			s.kanbanMutations.noteCardState(target.key, req.projectID, snapshotIssue, snapshotState, req.targetState, snapshotAt)
			return nil
		}
		if err := target.connector.UpdateIssueState(c.Request().Context(), req.issueID, req.targetState); err != nil {
			return err
		}
		s.recordKanbanLaneTransition(c.Request().Context(), req.projectID, snapshotIssue, currentState, req.targetState, "kanban_move")
		s.kanbanMutations.noteCardState(target.key, req.projectID, snapshotIssue, snapshotState, req.targetState, snapshotAt)
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
		data := s.boardData(ctx, s.latestSnapshot(ctx))
		data.Kanban.Feedback = message
		data.Kanban.FeedbackKind = "success"

		c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
		c.Response().Header().Set("HX-Retarget", kanbanFleetBoardTarget)
		c.Response().Header().Set("HX-Reswap", "morph:innerHTML")
		return render(c, templates.BoardSnapshot(data))
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
	c.Response().Header().Set("HX-Reswap", "morph:innerHTML")
	return render(c, templates.BoardSnapshot(data))
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
		ok, current, snapshotState, _, _ := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
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
	c.Response().Header().Set("HX-Reswap", "morph:innerHTML")
	return render(c, templates.BoardSnapshot(data))
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
	pendingMovedCards := s.kanbanMutations.pendingMovedCards(lockKey, projectID, snapshot)
	pendingStates := s.kanbanMutations.snapshotCardStates(lockKey, projectID, snapshot)
	applySnapshotKanbanIssues(&snapshot, func(issue *telemetry.Issue) {
		if issue == nil || strings.TrimSpace(issue.ID) == "" || !sameKanbanProject(*issue, projectID, snapshot.Project.ID) {
			return
		}
		stateKey := kanbanMutationStateKey(lockKey, issue.ID)
		state := strings.TrimSpace(pendingStates[stateKey])
		if state != "" {
			issue.State = state
		}
	})
	if len(pendingMovedCards) > 0 {
		snapshot.BoardIssues = append(snapshot.BoardIssues, pendingMovedCards...)
	}
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

func cloneKanbanIssue(issue telemetry.Issue) telemetry.Issue {
	out := issue
	out.Labels = append([]string(nil), issue.Labels...)
	out.Assignees = append([]string(nil), issue.Assignees...)
	out.Comments = cloneKanbanIssueComments(issue.Comments)
	out.BlockedBy = append([]telemetry.BlockedRef(nil), issue.BlockedBy...)
	out.Metadata = maps.Clone(issue.Metadata)
	out.PullRequest = cloneKanbanPullRequest(issue.PullRequest)
	out.Deliverable = cloneKanbanDeliverable(issue.Deliverable)
	out.MergeTiming = cloneKanbanMergeTiming(issue.MergeTiming)
	out.LeaseRenewedAt = cloneKanbanTimePointer(issue.LeaseRenewedAt)
	out.LeaseExpiresAt = cloneKanbanTimePointer(issue.LeaseExpiresAt)
	out.CreatedAt = cloneKanbanTimePointer(issue.CreatedAt)
	out.UpdatedAt = cloneKanbanTimePointer(issue.UpdatedAt)
	out.StageUpdatedAt = cloneKanbanTimePointer(issue.StageUpdatedAt)
	out.CurrentLaneEnteredAt = cloneKanbanTimePointer(issue.CurrentLaneEnteredAt)
	return out
}

func cloneKanbanIssueComments(comments []telemetry.IssueComment) []telemetry.IssueComment {
	if comments == nil {
		return nil
	}
	out := make([]telemetry.IssueComment, len(comments))
	for index, comment := range comments {
		out[index] = comment
		out[index].CreatedAt = cloneKanbanTimePointer(comment.CreatedAt)
		out[index].UpdatedAt = cloneKanbanTimePointer(comment.UpdatedAt)
	}
	return out
}

func cloneKanbanPullRequest(pr *telemetry.PullRequest) *telemetry.PullRequest {
	if pr == nil {
		return nil
	}
	out := *pr
	out.HydrationNextRetryAt = cloneKanbanTimePointer(pr.HydrationNextRetryAt)
	out.SlowChecks = append([]telemetry.PullRequestCheck(nil), pr.SlowChecks...)
	out.RunningChecks = append([]string(nil), pr.RunningChecks...)
	out.RequiredCheckFailures = append([]telemetry.PullRequestCheck(nil), pr.RequiredCheckFailures...)
	return &out
}

func cloneKanbanDeliverable(deliverable *telemetry.Deliverable) *telemetry.Deliverable {
	if deliverable == nil {
		return nil
	}
	out := *deliverable
	out.Metadata = maps.Clone(deliverable.Metadata)
	return &out
}

func cloneKanbanMergeTiming(timing *telemetry.MergeTiming) *telemetry.MergeTiming {
	if timing == nil {
		return nil
	}
	out := *timing
	out.EnteredMergingAt = cloneKanbanTimePointer(timing.EnteredMergingAt)
	out.MergeWorkerSlotAcquiredAt = cloneKanbanTimePointer(timing.MergeWorkerSlotAcquiredAt)
	out.MergeStartedAt = cloneKanbanTimePointer(timing.MergeStartedAt)
	out.BaseRefreshStartedAt = cloneKanbanTimePointer(timing.BaseRefreshStartedAt)
	out.BaseRefreshFinishedAt = cloneKanbanTimePointer(timing.BaseRefreshFinishedAt)
	out.CIWaitStartedAt = cloneKanbanTimePointer(timing.CIWaitStartedAt)
	out.CIWaitFinishedAt = cloneKanbanTimePointer(timing.CIWaitFinishedAt)
	out.MergedAt = cloneKanbanTimePointer(timing.MergedAt)
	out.MergeFailedAt = cloneKanbanTimePointer(timing.MergeFailedAt)
	return &out
}

func cloneKanbanTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	out := *value
	return &out
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
	addIssue := func(issue telemetry.Issue, state string) {
		state = strings.TrimSpace(state)
		if state == "" {
			state = strings.TrimSpace(issue.State)
		}
		if state == "" {
			return
		}
		for _, key := range kanbanIssueStateKeys(issue.ID, issue.Identifier) {
			states[key] = state
		}
	}
	for _, row := range snapshot.Completed {
		addIssue(row.Issue, row.State)
	}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		addIssue(issue, issue.State)
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
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, response, kanbanRequestValue(c, "body"), "")
		}
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, response, req.body, "")
		}
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, response)
		}
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, "Kanban integration mode is not enabled.", req.body, "")
		}
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, "Kanban integration mode is not enabled.")
		}
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if req.target == "pr" && !kanbanSupportsPullRequestComments(target.connector) {
		if kanbanDialogForm(c) {
			return s.kanbanCommentDialogValidation(c, "Comment target is not available on the current board.")
		}
		return kanbanFeedback(c, http.StatusNotFound, "Comment target is not available on the current board.")
	}
	if !s.kanbanCommentTargetKnown(req) {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, "Comment target is not available on the current board.", req.body, "")
		}
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
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, "Comment failed: "+err.Error(), req.body, "")
		}
		return kanbanFeedback(c, http.StatusBadGateway, "Comment failed: "+err.Error())
	}
	s.requestKanbanRefresh(c.Request().Context())
	if kanbanThreadForm(c) {
		return s.kanbanIssueCommentThread(c, "", "", "Comment submitted.")
	}
	return kanbanFeedback(c, http.StatusOK, "Comment submitted.")
}

func (s *Server) apiKanbanCommentEdit(c echo.Context) error {
	req, response, status := parseKanbanCommentMutationRequest(c, true)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	req.mutateVerb = "edit"

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if !s.kanbanCommentCanMutate(req, true) {
		return kanbanFeedback(c, http.StatusForbidden, "Only local Detent comments can be edited.")
	}

	err := s.kanbanMutations.withLock(target.key, func() error {
		editor, ok := target.connector.(connector.IssueCommentUpdater)
		if !ok {
			return connector.ErrNotImplemented
		}
		return editor.UpdateIssueComment(c.Request().Context(), req.issueID, req.commentID, req.body)
	})
	if err != nil {
		return s.kanbanCommentMutationError(c, req, err)
	}
	s.requestKanbanRefresh(c.Request().Context())
	return s.kanbanCommentThreadResponse(c, target, req)
}

func (s *Server) apiKanbanCommentDelete(c echo.Context) error {
	req, response, status := parseKanbanCommentMutationRequest(c, false)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	req.mutateVerb = "delete"

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		return kanbanFeedback(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return kanbanFeedback(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if !s.kanbanCommentCanMutate(req, false) {
		return kanbanFeedback(c, http.StatusForbidden, "Only local Detent comments can be deleted.")
	}

	err := s.kanbanMutations.withLock(target.key, func() error {
		deleter, ok := target.connector.(connector.IssueCommentDeleter)
		if !ok {
			return connector.ErrNotImplemented
		}
		return deleter.DeleteIssueComment(c.Request().Context(), req.issueID, req.commentID)
	})
	if err != nil {
		return s.kanbanCommentMutationError(c, req, err)
	}
	s.requestKanbanRefresh(c.Request().Context())
	return s.kanbanCommentThreadResponse(c, target, req)
}

func (s *Server) kanbanCommentMutationError(c echo.Context, req kanbanCommentMutationRequest, err error) error {
	s.logger.WarnContext(c.Request().Context(), "kanban comment mutation failed", "project", req.projectID, "issue_id", req.issueID, "comment_id", req.commentID, "verb", req.mutateVerb, "error", err)
	if errors.Is(err, sql.ErrNoRows) {
		return kanbanFeedback(c, http.StatusNotFound, "Local comment is not available on the current board.")
	}
	if errors.Is(err, connector.ErrNotImplemented) {
		return kanbanFeedback(c, http.StatusNotImplemented, "Local comment "+req.mutateVerb+" is not supported for this connector.")
	}
	return kanbanFeedback(c, http.StatusBadGateway, "Comment "+req.mutateVerb+" failed: "+err.Error())
}

func (s *Server) kanbanCommentThreadResponse(c echo.Context, target kanbanActionTarget, req kanbanCommentMutationRequest) error {
	if c.Request().Header.Get("HX-Request") != "true" {
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "message": "Comment " + kanbanCommentMutationPastTense(req.mutateVerb) + "."})
	}
	ctx := c.Request().Context()
	data, ok := s.projectDashboardData(ctx, req.projectID, s.latestSnapshot(ctx))
	if !ok {
		return kanbanFeedback(c, http.StatusNotFound, "Project is not available on the current board.")
	}
	card, ok := templates.FindBoardCard(data, req.projectID, req.issueID)
	if !ok {
		return kanbanFeedback(c, http.StatusNotFound, "Comment target is not available on the current board.")
	}
	reader, ok := target.connector.(connector.IssueCommentReader)
	if ok {
		comments, err := reader.FetchIssueComments(ctx, connector.Issue{
			ID:         req.issueID,
			Identifier: card.Identifier,
		})
		if err != nil {
			return kanbanFeedback(c, http.StatusBadGateway, "Comment refresh failed: "+err.Error())
		}
		card = templates.WithKanbanCardComments(card, telemetryIssueComments(comments))
	}
	conversation := templates.BoardCardConversationData(data, card, true, false)
	return render(c, templates.KanbanIssueCommentsPanel(conversation))
}

func kanbanCommentMutationPastTense(verb string) string {
	switch verb {
	case "edit":
		return "edited"
	case "delete":
		return "deleted"
	default:
		return strings.TrimSpace(verb)
	}
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
	if data.Target == "pr" && !kanbanSupportsPullRequestComments(target.connector) {
		return data, "Comment target is not available on the current board."
	}
	return data, ""
}

func (s *Server) kanbanIssueCommentThread(c echo.Context, message string, body string, notice string) error {
	data := s.kanbanThreadConversationData(c)
	data = templates.KanbanConversationWithIssueForm(data, body, message, notice)
	return render(c, templates.KanbanIssueCommentsPanel(data))
}

func (s *Server) kanbanThreadConversationData(c echo.Context) templates.KanbanConversationData {
	ctx := c.Request().Context()
	projectID := kanbanRequestValue(c, "project_id")
	projectScope := kanbanRequestValue(c, "kanban_board") == "project" && projectID != ""
	data := s.boardData(ctx, s.latestSnapshot(ctx))
	if projectScope {
		if scoped, ok := s.projectDashboardData(ctx, projectID, s.latestSnapshot(ctx)); ok {
			data = scoped
		}
	}
	issueIdentity := kanbanRequestValue(c, "issue_identity")
	if issueIdentity == "" {
		issueIdentity = kanbanRequestValue(c, "identifier")
	}
	if issueIdentity == "" {
		issueIdentity = kanbanRequestValue(c, "issue_id")
	}
	card, ok := templates.FindBoardCard(data, projectID, issueIdentity)
	if !ok {
		return templates.KanbanConversationData{
			ProjectID:     projectID,
			BoardScope:    kanbanRequestValue(c, "kanban_board"),
			IssueIdentity: issueIdentity,
			IssueID:       kanbanRequestValue(c, "issue_id"),
			Identifier:    kanbanRequestValue(c, "identifier"),
			Title:         kanbanRequestValue(c, "title"),
			CanComment:    true,
		}
	}
	boardActions := strings.EqualFold(kanbanRequestValue(c, "board_actions"), "true")
	expanded := strings.EqualFold(kanbanRequestValue(c, "expanded"), "true")
	conversation := templates.BoardCardConversationData(data, card, boardActions, expanded)
	return s.hydrateKanbanConversation(ctx, conversation)
}

func (s *Server) hydrateKanbanConversation(ctx context.Context, data templates.KanbanConversationData) templates.KanbanConversationData {
	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		data.IssueError = response
		if data.PRNumber > 0 {
			data.PRCommentsSupported = false
			data.PRComments = nil
		}
		return data
	}
	if reader, ok := target.connector.(connector.IssueCommentReader); ok {
		comments, err := reader.FetchIssueComments(ctx, connector.Issue{
			ID:         data.IssueID,
			Identifier: data.Identifier,
			URL:        data.IssueURL,
		})
		if err != nil {
			data.IssueError = "Issue comments unavailable: " + err.Error()
		} else {
			data = templates.KanbanConversationWithIssueComments(data, telemetryCommentsFromConnector(comments), data.IssueError)
		}
	}
	if data.PRNumber <= 0 || strings.TrimSpace(data.PRRepository) == "" {
		data.PRCommentsSupported = false
		data.PRComments = nil
		return data
	}
	reader, ok := target.connector.(connector.PullRequestCommentReader)
	if !ok {
		data.PRCommentsSupported = false
		data.PRComments = nil
		data.PRError = ""
		return data
	}
	comments, err := reader.FetchPullRequestComments(ctx, data.PRRepository, data.PRNumber)
	data.PRCommentsSupported = true
	if err != nil {
		data.PRError = "PR comments unavailable: " + err.Error()
		return data
	}
	return templates.KanbanConversationWithPRComments(data, telemetryCommentsFromConnector(comments), true, data.PRError)
}

func telemetryCommentsFromConnector(comments []connector.IssueComment) []telemetry.IssueComment {
	out := make([]telemetry.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, telemetry.IssueComment{
			ID:                comment.ID,
			Backend:           comment.Backend,
			Body:              comment.Body,
			URL:               comment.URL,
			AuthorLogin:       comment.AuthorLogin,
			AuthorDisplayName: comment.AuthorDisplayName,
			CreatedAt:         cloneCommentTime(comment.CreatedAt),
			UpdatedAt:         cloneCommentTime(comment.UpdatedAt),
			Local:             comment.Local,
			CanEdit:           comment.CanEdit,
			CanDelete:         comment.CanDelete,
			TargetType:        comment.TargetType,
		})
	}
	return out
}

func cloneCommentTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	out := *value
	return &out
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

func parseKanbanCommentMutationRequest(c echo.Context, requireBody bool) (kanbanCommentMutationRequest, string, int) {
	req := kanbanCommentMutationRequest{
		projectID: kanbanFormOrQueryValue(c, "project_id"),
		issueID:   kanbanFormOrQueryValue(c, "issue_id"),
		commentID: kanbanFormOrQueryValue(c, "comment_id"),
		body:      strings.TrimSpace(c.FormValue("body")),
	}
	if req.issueID == "" {
		return kanbanCommentMutationRequest{}, "Issue id is required.", http.StatusBadRequest
	}
	if req.commentID == "" {
		return kanbanCommentMutationRequest{}, "Comment id is required.", http.StatusBadRequest
	}
	if requireBody && req.body == "" {
		return kanbanCommentMutationRequest{}, "Comment body is required.", http.StatusBadRequest
	}
	return req, "", 0
}

func kanbanFormOrQueryValue(c echo.Context, key string) string {
	if value := strings.TrimSpace(c.FormValue(key)); value != "" {
		return value
	}
	return strings.TrimSpace(c.QueryParam(key))
}

func kanbanDialogForm(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" && strings.EqualFold(strings.TrimSpace(c.FormValue("kanban_dialog")), "true")
}

func kanbanThreadForm(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" && strings.EqualFold(strings.TrimSpace(c.FormValue("kanban_thread")), "true")
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
		Mode:                        mode,
		ProjectID:                   strings.TrimSpace(projectID),
		States:                      states,
		TerminalStates:              target.workflow.Tracker.TerminalStates,
		TerminalStatesByProject:     s.kanbanTerminalStatesByProject(projectID),
		AllowedTransitions:          kanbanAllowedTransitions(target.workflow, states),
		ShowBlockedAlerts:           target.kanban.ShowBlockedAlerts,
		SupportsPullRequestComments: kanbanSupportsPullRequestComments(target.connector),
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
			Mode:                        target.kanban.Mode,
			ProjectID:                   projectID,
			States:                      states,
			TerminalStates:              target.workflow.Tracker.TerminalStates,
			AllowedTransitions:          kanbanAllowedTransitions(target.workflow, states),
			SupportsPullRequestComments: kanbanSupportsPullRequestComments(target.connector),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func kanbanSupportsPullRequestComments(c connector.Connector) bool {
	if c == nil {
		return false
	}
	_, ok := c.(connector.PullRequestCommenter)
	return ok
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

func (s *Server) kanbanCardFresh(lockKey string, projectID string, issueID string, currentState string) (bool, string, string, telemetry.Issue, time.Time) {
	currentState = strings.TrimSpace(currentState)
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false, "", "", telemetry.Issue{}, time.Time{}
	}
	entry, ok := kanbanCardFreshEntry(snapshot, projectID, issueID)
	if !ok {
		return false, "", "", telemetry.Issue{}, snapshot.GeneratedAt
	}
	snapshotState := strings.TrimSpace(entry.state)
	state := snapshotState
	if s.kanbanMutations != nil {
		state = s.kanbanMutations.cardState(lockKey, issueID, snapshotState, snapshot.GeneratedAt)
	}
	if currentState == "" || normalizeKanbanState(state) == normalizeKanbanState(currentState) {
		return true, state, snapshotState, entry.issue, snapshot.GeneratedAt
	}
	return false, state, snapshotState, entry.issue, snapshot.GeneratedAt
}

func kanbanCardFreshEntry(snapshot telemetry.Snapshot, projectID string, issueID string) (kanbanSnapshotIssueEntry, bool) {
	var selected kanbanSnapshotIssueEntry
	for _, entry := range visibleSnapshotKanbanIssueEntries(snapshot) {
		if !sameKanbanIssue(entry.issue, projectID, issueID, snapshot.Project.ID) {
			continue
		}
		if selected.issue.ID != "" && entry.rank < selected.rank {
			continue
		}
		selected = entry
	}
	return selected, selected.issue.ID != ""
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

func (s *Server) kanbanCommentCanMutate(req kanbanCommentMutationRequest, edit bool) bool {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false
	}
	for _, issue := range snapshotKanbanIssues(snapshot) {
		if !sameKanbanIssue(issue, req.projectID, req.issueID, snapshot.Project.ID) {
			continue
		}
		for _, comment := range issue.Comments {
			if strings.TrimSpace(comment.ID) != req.commentID || !comment.Local {
				continue
			}
			if edit {
				return comment.CanEdit
			}
			return comment.CanDelete
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

func telemetryIssueComments(comments []connector.IssueComment) []telemetry.IssueComment {
	out := make([]telemetry.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, telemetry.IssueComment{
			ID:                comment.ID,
			Backend:           comment.Backend,
			Body:              comment.Body,
			URL:               comment.URL,
			AuthorLogin:       comment.AuthorLogin,
			AuthorDisplayName: comment.AuthorDisplayName,
			CreatedAt:         cloneKanbanTimePointer(comment.CreatedAt),
			UpdatedAt:         cloneKanbanTimePointer(comment.UpdatedAt),
			Local:             comment.Local,
			CanEdit:           comment.CanEdit,
			CanDelete:         comment.CanDelete,
			TargetType:        comment.TargetType,
		})
	}
	return out
}

func kanbanFeedback(c echo.Context, status int, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = http.StatusText(status)
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		class := "border-ok bg-ok/15 text-ok"
		if status >= http.StatusBadRequest {
			class = "border-err bg-err/15 text-err"
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
		if kanbanRawGitHubIssueState(issue.State) {
			if _, ok := seen[normalizeKanbanState(issue.State)]; !ok {
				continue
			}
		}
		add(issue.State)
	}
	return states
}

func kanbanRawGitHubIssueState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "open", "closed":
		return true
	default:
		return false
	}
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

func snapshotKanbanIssueEntries(snapshot telemetry.Snapshot) []kanbanSnapshotIssueEntry {
	entries := make([]kanbanSnapshotIssueEntry, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked))
	index := 0
	appendIssue := func(issue telemetry.Issue, fallback string, rank int) {
		state := strings.TrimSpace(issue.State)
		fallback = strings.TrimSpace(fallback)
		rawRuntimeState := false
		if fallback != "" && kanbanRawGitHubIssueState(state) {
			state = fallback
			rawRuntimeState = true
		}
		if state == "" {
			state = fallback
		}
		entries = append(entries, kanbanSnapshotIssueEntry{
			issue:           issue,
			state:           state,
			rank:            rank,
			index:           index,
			rawRuntimeState: rawRuntimeState,
		})
		index++
	}
	for _, issue := range snapshot.BoardIssues {
		appendIssue(issue, "", 5)
	}
	for _, issue := range snapshot.Pipeline {
		appendIssue(issue, "", 10)
	}
	for _, row := range snapshot.Queue {
		appendIssue(row.Issue, "Todo", 20)
	}
	for _, row := range snapshot.Running {
		appendIssue(row.Issue, "In Progress", 30)
	}
	for _, row := range snapshot.Blocked {
		appendIssue(row.Issue, "Blocked", 40)
	}
	return entries
}

func visibleSnapshotKanbanIssueEntries(snapshot telemetry.Snapshot) []kanbanSnapshotIssueEntry {
	entries := snapshotKanbanIssueEntries(snapshot)
	byKey := make(map[string]kanbanSnapshotIssueEntry, len(entries))
	for _, entry := range entries {
		key := snapshotKanbanIssueEntryKey(entry.issue, snapshot.Project.ID)
		if key == "" {
			continue
		}
		current, ok := byKey[key]
		if ok && entry.rawRuntimeState {
			continue
		}
		if ok && entry.rank < current.rank {
			continue
		}
		byKey[key] = entry
	}
	visible := make([]kanbanSnapshotIssueEntry, 0, len(byKey))
	for _, entry := range byKey {
		visible = append(visible, entry)
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return visible[i].index < visible[j].index
	})
	return visible
}

func snapshotKanbanIssueEntryKey(issue telemetry.Issue, snapshotProjectID string) string {
	projectID := strings.TrimSpace(issue.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(snapshotProjectID)
	}
	prefix := ""
	if projectID != "" {
		prefix = "project:" + projectID + ":"
	}
	if issueID := strings.TrimSpace(issue.ID); issueID != "" {
		return prefix + "id:" + issueID
	}
	if identifier := strings.ToLower(strings.TrimSpace(issue.Identifier)); identifier != "" {
		return prefix + "identifier:" + identifier
	}
	return ""
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
