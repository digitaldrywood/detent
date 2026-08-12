package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

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
	kanbanDialogContentTarget      = "#kanban-dialog-content"
	kanbanProjectBoardTarget       = "#snapshot"
	kanbanFleetBoardTarget         = "#snapshot"
	kanbanDialogSucceeded          = "kanbanActionSucceeded"
	kanbanSuccessTriggerHeader     = "HX-Trigger-After-Swap"
	kanbanBlockedState             = "Blocked"
	kanbanRefreshRetryDelay        = 2 * time.Second
	kanbanMoveUnsupportedMessage   = "This project's tracker does not support moving cards."
	kanbanRemoveUnsupportedMessage = "This project's tracker does not support removing cards."
)

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
		return s.kanbanMoveValidationResponse(c, status, response)
	}
	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		return s.kanbanMoveValidationResponse(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return s.kanbanMoveValidationResponse(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if !kanbanCanMoveCards(target) {
		return s.kanbanMoveValidationResponse(c, http.StatusForbidden, kanbanMoveUnsupportedMessage)
	}
	if req.issueID == "" {
		if req.prNumber > 0 {
			return s.kanbanMoveValidationResponse(c, http.StatusUnprocessableEntity, "Cannot move PR-only card without a linked issue.")
		}
		return s.kanbanMoveValidationResponse(c, http.StatusBadRequest, "Issue id is required.")
	}
	if !kanbanstate.StateAllowed(target.workflow, req.targetState) {
		return s.kanbanMoveValidationResponse(c, http.StatusBadRequest, "Target state is not configured for this board.")
	}
	var feedback string
	var feedbackStatus int
	var moveIssueIdentifier string
	var moveCurrentState string
	var moveDataSeq uint64
	err := s.kanbanMutations.WithLock(target.key, func() error {
		currentState := req.currentState
		ok, current, snapshotState, snapshotIssue, dataSeqAtWrite := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
		if strings.TrimSpace(current) != "" {
			currentState = current
		}
		moveIssueIdentifier = kanbanBlockedMoveIssueIdentifier(snapshotIssue, req.issueID)
		moveCurrentState = currentState
		moveDataSeq = dataSeqAtWrite
		if !ok && (strings.TrimSpace(current) == "" || !target.workflow.KanbanTransitionAllowed(currentState, req.targetState)) {
			s.logger.WarnContext(c.Request().Context(), "kanban move rejected: stale card",
				"project", req.projectID,
				"issue_id", req.issueID,
				"identifier", moveIssueIdentifier,
				"current_state", req.currentState,
				"live_state", currentState,
				"target_state", req.targetState,
				"data_seq", dataSeqAtWrite,
			)
			feedback = "Card is stale; refresh and retry."
			if current != "" {
				feedback = fmt.Sprintf("Card is stale; current state is %s.", current)
			}
			feedbackStatus = http.StatusConflict
			return nil
		}
		if !target.workflow.KanbanTransitionAllowed(currentState, req.targetState) {
			s.logger.WarnContext(c.Request().Context(), "kanban move blocked by transition policy",
				"project", req.projectID,
				"issue_id", req.issueID,
				"identifier", moveIssueIdentifier,
				"current_state", currentState,
				"target_state", req.targetState,
				"data_seq", dataSeqAtWrite,
			)
			feedback = fmt.Sprintf("Move from %s to %s is not allowed by the Kanban transition policy.", currentState, req.targetState)
			feedbackStatus = http.StatusUnprocessableEntity
			return nil
		}

		if target.kanban.IssueStateFieldID > 0 {
			setter, ok := target.connector.(connector.IssueFieldSetter)
			if !ok {
				return connector.ErrNotImplemented
			}
			if err := setter.SetIssueField(c.Request().Context(), req.issueID, target.kanban.IssueStateFieldID, kanbanstate.MappedState(target.workflow, req.targetState)); err != nil {
				return err
			}
			s.recordKanbanLaneTransition(
				c.Request().Context(),
				req.projectID,
				snapshotIssue,
				currentState,
				req.targetState,
				"kanban_move_field",
				target.connector,
				kanbanAdmissionTargetState(target.workflow),
				kanbanMutationProvenanceSource(c),
			)
			s.kanbanMutations.NoteCardState(target.key, req.projectID, snapshotIssue, snapshotState, req.targetState, dataSeqAtWrite)
			return nil
		}
		if err := target.connector.UpdateIssueState(c.Request().Context(), req.issueID, req.targetState); err != nil {
			return err
		}
		s.recordKanbanLaneTransition(
			c.Request().Context(),
			req.projectID,
			snapshotIssue,
			currentState,
			req.targetState,
			"kanban_move",
			target.connector,
			kanbanAdmissionTargetState(target.workflow),
			kanbanMutationProvenanceSource(c),
		)
		s.kanbanMutations.NoteCardState(target.key, req.projectID, snapshotIssue, snapshotState, req.targetState, dataSeqAtWrite)
		return nil
	})
	if feedback != "" {
		return s.kanbanMoveValidationResponse(c, feedbackStatus, feedback)
	}
	if err != nil {
		var blocked *connector.StateUpdateBlockedError
		if errors.As(err, &blocked) {
			s.logger.WarnContext(c.Request().Context(), "kanban move blocked by connector",
				"project", req.projectID,
				"issue_id", req.issueID,
				"identifier", moveIssueIdentifier,
				"current_state", moveCurrentState,
				"target_state", req.targetState,
				"data_seq", moveDataSeq,
				"error", blocked,
			)
			return s.kanbanMoveValidationResponse(c, http.StatusUnprocessableEntity, kanbanBlockedMoveMessage(blocked, req.targetState, moveIssueIdentifier))
		}
		if errors.Is(err, connector.ErrNotImplemented) {
			return s.kanbanMoveValidationResponse(c, http.StatusNotImplemented, kanbanMoveUnsupportedMessage)
		}
		s.logger.WarnContext(c.Request().Context(), "kanban move failed",
			"project", req.projectID,
			"issue_id", req.issueID,
			"identifier", moveIssueIdentifier,
			"current_state", moveCurrentState,
			"target_state", req.targetState,
			"data_seq", moveDataSeq,
			"error", err,
		)
		return kanbanFeedback(c, http.StatusBadGateway, "Move failed: "+err.Error())
	}
	var runtimeMove orchestrator.OperatorMoveResult
	if s.operatorMoves != nil && strings.EqualFold(strings.TrimSpace(moveCurrentState), kanbanBlockedState) &&
		!strings.EqualFold(strings.TrimSpace(req.targetState), kanbanBlockedState) {
		var reconcileErr error
		runtimeMove, reconcileErr = s.operatorMoves.ReconcileOperatorMove(c.Request().Context(), orchestrator.OperatorMoveRequest{
			ProjectID:  req.projectID,
			IssueID:    req.issueID,
			Identifier: moveIssueIdentifier,
			FromState:  moveCurrentState,
			ToState:    req.targetState,
		})
		if reconcileErr != nil {
			s.logger.WarnContext(c.Request().Context(), "kanban move runtime reconcile failed",
				"project", req.projectID,
				"issue_id", req.issueID,
				"identifier", moveIssueIdentifier,
				"current_state", moveCurrentState,
				"target_state", req.targetState,
				"data_seq", moveDataSeq,
				"error", reconcileErr,
			)
		}
	}
	s.logger.InfoContext(c.Request().Context(), "kanban move succeeded",
		"project", req.projectID,
		"issue_id", req.issueID,
		"identifier", moveIssueIdentifier,
		"current_state", moveCurrentState,
		"target_state", req.targetState,
		"data_seq", moveDataSeq,
		"runtime_block_cleared", runtimeMove.BlockedCleared,
	)
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

		setKanbanActionSucceeded(c, message)
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

	setKanbanActionSucceeded(c, message)
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
	if !kanbanCanRemoveCards(target) {
		return kanbanFeedback(c, http.StatusForbidden, kanbanRemoveUnsupportedMessage)
	}
	if req.issueID == "" {
		if req.prNumber > 0 {
			return kanbanFeedback(c, http.StatusUnprocessableEntity, "Cannot remove PR-only card without a linked issue.")
		}
		return kanbanFeedback(c, http.StatusBadRequest, "Issue id is required.")
	}

	var feedback string
	var feedbackStatus int
	var removeIssueIdentifier string
	err := s.kanbanMutations.WithLock(target.key, func() error {
		currentState := req.currentState
		ok, current, snapshotState, snapshotIssue, dataSeqAtWrite := s.kanbanCardFresh(target.key, req.projectID, req.issueID, req.currentState)
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
		removeIssueIdentifier = kanbanBlockedMoveIssueIdentifier(snapshotIssue, req.issueID)
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
			s.kanbanMutations.NoteCardRemoved(target.key, req.issueID, snapshotState, dataSeqAtWrite)
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
		s.kanbanMutations.NoteCardRemoved(target.key, req.issueID, snapshotState, dataSeqAtWrite)
		return nil
	})
	if feedback != "" {
		return kanbanFeedback(c, feedbackStatus, feedback)
	}
	if err != nil {
		var blocked *connector.StateUpdateBlockedError
		if errors.As(err, &blocked) {
			return kanbanFeedback(c, http.StatusUnprocessableEntity, kanbanBlockedMoveMessage(blocked, "", removeIssueIdentifier))
		}
		if errors.Is(err, connector.ErrNotImplemented) {
			return kanbanFeedback(c, http.StatusNotImplemented, kanbanRemoveUnsupportedMessage)
		}
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

	setKanbanActionSucceeded(c, message)
	c.Response().Header().Set("HX-Retarget", kanbanProjectBoardTarget)
	c.Response().Header().Set("HX-Reswap", "morph:innerHTML")
	return render(c, templates.BoardSnapshot(data))
}

func (s *Server) kanbanSnapshotWithPendingStates(lockKey string, projectID string, snapshot telemetry.Snapshot) telemetry.Snapshot {
	if s.kanbanMutations == nil {
		return snapshot
	}
	snapshot = kanbanstate.CloneIssueSlices(snapshot)
	dataSeq := kanbanstate.SnapshotProjectDataSeq(snapshot, projectID)
	kanbanstate.FilterSnapshotIssues(&snapshot, func(issue telemetry.Issue) bool {
		if strings.TrimSpace(issue.ID) == "" || !kanbanstate.SameProject(issue, projectID, snapshot.Project.ID) {
			return true
		}
		return !s.kanbanMutations.CardRemoved(lockKey, issue.ID, issue.State, dataSeq)
	})
	terminalStates := s.kanbanWorkflow.Tracker.TerminalStates
	if configured := s.kanbanTerminalStatesByProject(projectID)[projectID]; len(configured) > 0 {
		terminalStates = configured
	}
	pendingMovedCards := s.kanbanMutations.PendingMovedCards(lockKey, projectID, snapshot, terminalStates)
	pendingStates := s.kanbanMutations.SnapshotCardStates(lockKey, projectID, snapshot)
	kanbanstate.ApplySnapshotIssues(&snapshot, func(issue *telemetry.Issue) {
		if issue == nil || strings.TrimSpace(issue.ID) == "" || !kanbanstate.SameProject(*issue, projectID, snapshot.Project.ID) {
			return
		}
		stateKey := kanbanstate.MutationStateKey(lockKey, issue.ID)
		state := strings.TrimSpace(pendingStates[stateKey])
		if state != "" {
			issue.State = state
		}
	})
	if len(pendingMovedCards) > 0 {
		snapshot.BoardIssues = append(snapshot.BoardIssues, pendingMovedCards...)
	}
	states := kanbanstate.IssueStateIndex(snapshot)
	kanbanstate.ApplySnapshotIssues(&snapshot, func(issue *telemetry.Issue) {
		if issue == nil || len(issue.BlockedBy) == 0 || !kanbanstate.SameProject(*issue, projectID, snapshot.Project.ID) {
			return
		}
		issue.BlockedBy = kanbanstate.BlockedRefsWithCurrentStates(issue.BlockedBy, states)
	})
	return snapshot
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
		return s.kanbanCommentValidationResponse(c, status, response)
	}

	target, response, status := s.kanbanActionTarget(req.projectID)
	if response != "" {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, response, req.body, "")
		}
		return s.kanbanCommentValidationResponse(c, status, response)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, "Kanban integration mode is not enabled.", req.body, "")
		}
		return s.kanbanCommentValidationResponse(c, http.StatusForbidden, "Kanban integration mode is not enabled.")
	}
	if req.target == "pr" && !kanbanSupportsPullRequestComments(target.connector) {
		return s.kanbanCommentValidationResponse(c, http.StatusNotFound, "Comment target is not available on the current board.")
	}
	if !s.kanbanCommentTargetKnown(req) {
		if kanbanThreadForm(c) {
			return s.kanbanIssueCommentThread(c, "Comment target is not available on the current board.", req.body, "")
		}
		return s.kanbanCommentValidationResponse(c, http.StatusNotFound, "Comment target is not available on the current board.")
	}

	err := s.kanbanMutations.WithLock(target.key, func() error {
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

	err := s.kanbanMutations.WithLock(target.key, func() error {
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

	err := s.kanbanMutations.WithLock(target.key, func() error {
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

func (s *Server) kanbanMoveValidationResponse(c echo.Context, status int, message string) error {
	if kanbanDialogForm(c) {
		return s.kanbanMoveDialogValidation(c, message)
	}
	return kanbanFeedback(c, status, message)
}

func kanbanBlockedMoveIssueIdentifier(issue telemetry.Issue, fallback string) string {
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	if id := strings.TrimSpace(issue.ID); id != "" {
		return id
	}
	return strings.TrimSpace(fallback)
}

func kanbanBlockedMoveMessage(blocked *connector.StateUpdateBlockedError, targetState string, identifier string) string {
	target := strings.TrimSpace(targetState)
	identifier = strings.TrimSpace(identifier)
	current := ""
	if blocked != nil {
		if target == "" {
			target = strings.TrimSpace(blocked.TargetState)
		}
		current = strings.TrimSpace(blocked.CurrentState)
		if identifier == "" {
			identifier = strings.TrimSpace(blocked.IssueID)
		}
	}
	if target == "" {
		target = "requested state"
	}
	if identifier == "" {
		identifier = "issue"
	}
	if current == "" {
		current = "unknown"
	}
	return fmt.Sprintf("Move to %s was refused: %s is in terminal state %s.", target, identifier, current)
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

func (s *Server) kanbanCommentValidationResponse(c echo.Context, status int, message string) error {
	if kanbanDialogForm(c) {
		return s.kanbanCommentDialogValidation(c, message)
	}
	return kanbanFeedback(c, status, message)
}

func (s *Server) kanbanMoveDialogData(c echo.Context, message string) (templates.KanbanMoveDialogData, string) {
	data := templates.KanbanMoveDialogData{
		ProjectID:    kanbanRequestValue(c, "project_id"),
		Board:        kanbanRequestValue(c, "kanban_board"),
		IssueID:      kanbanRequestValue(c, "issue_id"),
		Identifier:   kanbanRequestValue(c, "identifier"),
		IssueURL:     kanbanRequestValue(c, "issue_url"),
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
	if !kanbanCanMoveCards(target) {
		return data, kanbanMoveUnsupportedMessage
	}
	data.States = target.workflow.KanbanAllowedTransitionTargets(data.CurrentState)
	if len(data.States) == 0 && data.CurrentState == "" {
		data.States = kanbanstate.StateNames(target.workflow, s.latestSnapshot(c.Request().Context()))
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
			if kanbanstate.NormalizeState(target) == kanbanstate.NormalizeState(preferred) {
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
	switch kanbanstate.NormalizeState(source) {
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
		IssueURL:     kanbanRequestValue(c, "issue_url"),
		PRURL:        kanbanRequestValue(c, "pr_url"),
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

func (s *Server) apiBoardConversation(c echo.Context) error {
	projectID := strings.TrimSpace(c.QueryParam("project"))
	data, demo, err := s.boardCardDashboardData(c, false)
	if err != nil {
		return err
	}
	card, ok := templates.FindBoardCard(data, projectID, c.QueryParam("issue"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Card not found")
	}
	conversation := templates.BoardCardConversationData(data, card, c.QueryParam("actions") == "board", c.QueryParam("expanded") == "1")
	conversation = s.kanbanConversationShellData(conversation, demo)
	switch c.QueryParam("target") {
	case "pr":
		conversation.PRPending = false
		if !demo {
			conversation = s.hydrateKanbanPRConversation(c.Request().Context(), conversation)
		}
		return render(c, templates.KanbanPRCommentsPanel(conversation))
	default:
		conversation.IssuePending = false
		if !demo {
			conversation = s.hydrateKanbanIssueConversation(c.Request().Context(), conversation)
		}
		return render(c, templates.KanbanIssueCommentsPanel(conversation))
	}
}

func (s *Server) kanbanConversationShellData(data templates.KanbanConversationData, demo bool) templates.KanbanConversationData {
	if demo {
		data.IssuePending = true
		data.PRCommentsSupported = data.PRNumber > 0 && strings.TrimSpace(data.PRRepository) != ""
		data.PRPending = data.PRCommentsSupported
		return data
	}
	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		data.IssueError = response
		data.PRCommentsSupported = false
		return data
	}
	_, data.IssuePending = target.connector.(connector.IssueCommentReader)
	_, data.PRCommentsSupported = target.connector.(connector.PullRequestCommentReader)
	data.PRCommentsSupported = data.PRCommentsSupported && data.PRNumber > 0 && strings.TrimSpace(data.PRRepository) != ""
	data.PRPending = data.PRCommentsSupported
	return data
}

func (s *Server) hydrateKanbanConversation(ctx context.Context, data templates.KanbanConversationData) templates.KanbanConversationData {
	data = s.hydrateKanbanIssueConversation(ctx, data)
	return s.hydrateKanbanPRConversation(ctx, data)
}

func (s *Server) hydrateKanbanIssueConversation(ctx context.Context, data templates.KanbanConversationData) templates.KanbanConversationData {
	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		data.IssueError = response
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
	return data
}

func (s *Server) hydrateKanbanPRConversation(ctx context.Context, data templates.KanbanConversationData) templates.KanbanConversationData {
	target, response, _ := s.kanbanActionTarget(data.ProjectID)
	if response != "" {
		data.PRCommentsSupported = false
		data.PRComments = nil
		data.PRError = response
		return data
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
			AuthorKind:        comment.AuthorKind,
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
	states := kanbanstate.StateNames(target.workflow, snapshot)
	canMove, canRemove := kanbanCardCapabilities(target)
	data := templates.KanbanData{
		Mode:                        mode,
		ProjectID:                   strings.TrimSpace(projectID),
		TrackerKind:                 target.workflow.Tracker.Kind,
		States:                      states,
		TerminalStates:              target.workflow.Tracker.TerminalStates,
		TerminalStatesByProject:     s.kanbanTerminalStatesByProject(projectID),
		DispatchPriorityByLabel:     append([]string(nil), target.workflow.Agent.DispatchPriorityByLabel...),
		AllowedTransitions:          kanbanstate.AllowedTransitions(target.workflow, states),
		ShowBlockedAlerts:           target.kanban.ShowBlockedAlerts,
		SupportsPullRequestComments: kanbanSupportsPullRequestComments(target.connector),
		CanMoveCards:                canMove,
		CanRemoveCards:              canRemove,
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
		states := kanbanstate.StateNames(target.workflow, snapshot)
		canMove, canRemove := kanbanCardCapabilities(target)
		out[projectID] = templates.KanbanProjectData{
			Mode:                        target.kanban.Mode,
			ProjectID:                   projectID,
			TrackerKind:                 target.workflow.Tracker.Kind,
			States:                      states,
			TerminalStates:              target.workflow.Tracker.TerminalStates,
			DispatchPriorityByLabel:     append([]string(nil), target.workflow.Agent.DispatchPriorityByLabel...),
			AllowedTransitions:          kanbanstate.AllowedTransitions(target.workflow, states),
			SupportsPullRequestComments: kanbanSupportsPullRequestComments(target.connector),
			CanMoveCards:                canMove,
			CanRemoveCards:              canRemove,
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

func kanbanCardCapabilities(target kanbanActionTarget) (bool, bool) {
	caps := connector.DetectCapabilities(target.connector)
	canMove := caps.UpdateIssueState
	canRemove := caps.RemoveFromProject
	if target.kanban.IssueStateFieldID > 0 {
		canMove = caps.SetIssueFields
		canRemove = caps.ClearIssueFields
	}
	return canMove, canRemove
}

func kanbanCanMoveCards(target kanbanActionTarget) bool {
	canMove, _ := kanbanCardCapabilities(target)
	return canMove
}

func kanbanCanRemoveCards(target kanbanActionTarget) bool {
	_, canRemove := kanbanCardCapabilities(target)
	return canRemove
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

func (s *Server) kanbanCardFresh(lockKey string, projectID string, issueID string, currentState string) (bool, string, string, telemetry.Issue, uint64) {
	currentState = strings.TrimSpace(currentState)
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false, "", "", telemetry.Issue{}, 0
	}
	dataSeq := kanbanstate.SnapshotProjectDataSeq(snapshot, projectID)
	entry, ok := kanbanstate.CardFreshEntry(snapshot, projectID, issueID)
	if !ok {
		return false, "", "", telemetry.Issue{}, dataSeq
	}
	snapshotState := strings.TrimSpace(entry.State)
	state := snapshotState
	if s.kanbanMutations != nil {
		state = s.kanbanMutations.CardState(lockKey, issueID, snapshotState, dataSeq)
	}
	if currentState == "" || kanbanstate.NormalizeState(state) == kanbanstate.NormalizeState(currentState) {
		return true, state, snapshotState, entry.Issue, dataSeq
	}
	return false, state, snapshotState, entry.Issue, dataSeq
}

func (s *Server) recordKanbanLaneTransition(
	ctx context.Context,
	projectID string,
	issue telemetry.Issue,
	currentState string,
	targetState string,
	reason string,
	tracker connector.Connector,
	admissionTargetState string,
	provenanceSource provenance.Source,
) {
	if s.store == nil {
		return
	}
	currentState = strings.TrimSpace(currentState)
	targetState = strings.TrimSpace(targetState)
	if targetState == "" || kanbanstate.NormalizeState(currentState) == kanbanstate.NormalizeState(targetState) {
		return
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(issue.ProjectID)
	}
	if projectID == "" {
		projectID = "default"
	}
	now := time.Now().UTC()
	connectorIssue := connector.Issue{
		ID:             issue.ID,
		Identifier:     issue.Identifier,
		URL:            issue.URL,
		State:          currentState,
		CreatedAt:      issue.CreatedAt,
		UpdatedAt:      issue.UpdatedAt,
		StageUpdatedAt: issue.StageUpdatedAt,
	}
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		number := issue.PullRequest.Number
		connectorIssue.PRNumber = &number
	}
	actor := provenance.Actor{}
	if reader, ok := tracker.(connector.IssueStateTransitionReader); ok {
		observed := connectorIssue
		observed.State = targetState
		transition, found, err := reader.IssueStateTransition(ctx, observed)
		if err != nil {
			if s.logger != nil {
				s.logger.WarnContext(ctx, "read kanban lane transition actor failed", "project", projectID, "issue_id", issue.ID, "identifier", issue.Identifier, "target_state", targetState, "error", err)
			}
		} else if found {
			if !transition.EnteredAt.IsZero() {
				now = transition.EnteredAt.UTC()
			}
			actor = provenance.Actor{Login: transition.Actor.Login, Kind: transition.Actor.Kind}
		}
	}
	attribution := provenance.AttributionFromSource(provenanceSource, actor)
	var admission *provenance.Admission
	if strings.EqualFold(strings.TrimSpace(targetState), strings.TrimSpace(admissionTargetState)) &&
		strings.TrimSpace(admissionTargetState) != "" {
		admission = &provenance.Admission{Attributed: false}
	}
	if err := workflowmetrics.RecordLaneTransition(ctx, s.store, workflowmetrics.LaneTransition{
		ProjectID:    projectID,
		Issue:        connectorIssue,
		TargetState:  targetState,
		At:           now,
		Reason:       strings.TrimSpace(reason),
		MetadataJSON: provenance.Apply("{}", attribution, admission),
	}); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "record kanban lane transition metric failed", "project", projectID, "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", currentState, "target_state", targetState, "error", err)
	}
}

func kanbanMutationProvenanceSource(c echo.Context) provenance.Source {
	if c == nil || c.Request() == nil {
		return ""
	}
	request := c.Request()
	if len(requestAPITokens(request)) > 0 {
		return provenance.SourceExternalAutomation
	}
	if _, ok := webSessionFromContext(request.Context()); ok {
		return provenance.SourceHumanSession
	}
	if request.Header.Get("HX-Request") == "true" && requestSameOriginDashboardSource(request) {
		return provenance.SourceHumanSession
	}
	return ""
}

func kanbanAdmissionTargetState(cfg workflowconfig.Config) string {
	if !cfg.BacklogAdmission.Enabled {
		return ""
	}
	return cfg.BacklogAdmission.TargetState
}

func (s *Server) kanbanCommentTargetKnown(req kanbanCommentRequest) bool {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return false
	}
	for _, issue := range kanbanstate.SnapshotIssues(snapshot) {
		if !kanbanstate.SameProject(issue, req.projectID, snapshot.Project.ID) {
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
	for _, issue := range kanbanstate.SnapshotIssues(snapshot) {
		if !kanbanstate.SameIssue(issue, req.projectID, req.issueID, snapshot.Project.ID) {
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
	s.requestKanbanRefreshWithRetry(ctx, true)
}

func (s *Server) requestKanbanRefreshWithRetry(ctx context.Context, retryOnError bool) {
	if s.refresher == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := s.refresher.RequestRefresh(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "kanban refresh request failed", "error", err)
		if retryOnError {
			s.scheduleKanbanRefreshRetry(ctx)
		}
		return
	}
	if response.Refused {
		s.logger.WarnContext(ctx, "kanban refresh request refused", "retry_at", response.RetryAt)
	}
}

func (s *Server) scheduleKanbanRefreshRetry(ctx context.Context) {
	if !s.kanbanRetryInFlight.CompareAndSwap(false, true) {
		return
	}
	afterFunc := s.afterFunc
	if afterFunc == nil {
		afterFunc = time.AfterFunc
	}
	retryCtx := context.WithoutCancel(ctx)
	afterFunc(kanbanRefreshRetryDelay, func() {
		defer s.kanbanRetryInFlight.Store(false)
		s.requestKanbanRefreshWithRetry(retryCtx, false)
	})
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
			AuthorKind:        comment.AuthorKind,
			AuthorDisplayName: comment.AuthorDisplayName,
			CreatedAt:         kanbanstate.CloneTimePointer(comment.CreatedAt),
			UpdatedAt:         kanbanstate.CloneTimePointer(comment.UpdatedAt),
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
		if status < http.StatusBadRequest {
			setKanbanActionSucceeded(c, message)
			return c.HTML(status, `<div id="kanban-feedback" role="status" aria-live="polite" hidden>`+html.EscapeString(message)+`</div>`)
		}
		return c.HTML(status, `<div id="kanban-feedback" role="status" aria-live="polite" class="rounded-md border border-err bg-err/15 px-3 py-2 text-sm text-err">`+html.EscapeString(message)+`</div>`)
	}
	if status >= http.StatusBadRequest {
		return c.JSON(status, errorResponse("kanban_action_failed", message))
	}
	return c.JSON(status, map[string]any{"ok": true, "message": message})
}

func setKanbanActionSucceeded(c echo.Context, message string) {
	trigger := `{"` + kanbanDialogSucceeded + `":{"message":` + strconv.Quote(message) + `}}`
	c.Response().Header().Set(kanbanSuccessTriggerHeader, trigger)
}

func kanbanPullRequestRepository(issue telemetry.Issue) string {
	if issue.PullRequest != nil {
		if repository := kanbanRepositoryFromPullRequestURL(issue.PullRequest.URL); repository != "" {
			return repository
		}
	}
	return kanbanstate.IssueRepository(issue.Identifier)
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
