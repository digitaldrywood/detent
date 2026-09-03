package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type workflowMutationRequest struct {
	WorkflowStateID int64  `json:"workflow_state_id"`
	Label           string `json:"label"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type dependencyMutationRequest struct {
	Action            string             `json:"action"`
	BlockerWorkItemID tracker.WorkItemID `json:"blocker_work_item_id"`
	Provenance        string             `json:"provenance,omitempty"`
}

type priorityMutationRequest struct {
	Scope          string `json:"scope"`
	State          string `json:"state"`
	Priority       string `json:"priority"`
	IdempotencyKey string `json:"idempotency_key"`
}

type orderMutationRequest struct {
	Scope string `json:"scope"`
	State string `json:"state"`
	Rank  string `json:"rank"`
}

type mutationResponse struct {
	WorkItemID tracker.WorkItemID `json:"work_item_id"`
	Kind       string             `json:"kind"`
	Status     string             `json:"status"`
	OutboxID   int64              `json:"outbox_id,omitempty"`
}

func (s *Service) changeWorkItemWorkflow(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request workflowMutationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	repositoryID, err := s.database.workItemRepositoryID(c.Request().Context(), id)
	if err != nil {
		return trackerAPIError(c, err)
	}
	item, err := s.ChangeWorkflowState(c.Request().Context(), WorkflowStateChange{
		IssueID:         int64(id),
		WorkflowStateID: request.WorkflowStateID,
		Mutation: WorkflowLabelMutation{
			IdempotencyKey: request.IdempotencyKey,
			RepositoryID:   repositoryID,
			IssueID:        int64(id),
			Label:          request.Label,
		},
	})
	if err != nil {
		return operatorAPIError(c, err)
	}
	return c.JSON(http.StatusAccepted, mutationResponse{WorkItemID: id, Kind: "workflow", Status: "pending", OutboxID: item.ID})
}

func (s *Service) changeWorkItemDependency(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request dependencyMutationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.Action = strings.ToLower(strings.TrimSpace(request.Action))
	request.Provenance = strings.TrimSpace(request.Provenance)
	if request.Provenance == "" {
		request.Provenance = "hub"
	}
	if request.BlockerWorkItemID <= 0 || request.BlockerWorkItemID == id || (request.Action != "add" && request.Action != "remove") {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_dependency", Message: "Dependency action and a distinct positive blocker_work_item_id are required"})
	}
	err = s.database.changeDependency(c.Request().Context(), id, request)
	if err != nil {
		return operatorAPIError(c, err)
	}
	return c.JSON(http.StatusOK, mutationResponse{WorkItemID: id, Kind: "dependency", Status: "applied"})
}

func (s *Service) changeWorkItemPriority(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request priorityMutationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.Scope = strings.TrimSpace(request.Scope)
	request.State = strings.TrimSpace(request.State)
	request.Priority = strings.ToLower(strings.TrimSpace(request.Priority))
	priority, ok := queuePriority(request.Priority)
	if request.Scope == "" || request.State == "" || !ok {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_priority", Message: "Scope, state, and a valid priority are required"})
	}
	repositoryID, err := s.database.workItemRepositoryID(c.Request().Context(), id)
	if err != nil {
		return trackerAPIError(c, err)
	}
	record, err := (WorkflowLabelMutation{
		IdempotencyKey: request.IdempotencyKey,
		RepositoryID:   repositoryID,
		IssueID:        int64(id),
		Label:          "priority:" + request.Priority,
		ManagedPrefix:  "priority:",
	}).outboxRecord()
	if err != nil {
		return operatorAPIError(c, err)
	}
	item, err := s.commitOutbox(c.Request().Context(), record, func(tx *sql.Tx, now string) error {
		return upsertQueueEntry(c.Request().Context(), tx, id, request.Scope, request.State, "", &priority, now)
	})
	if err != nil {
		return operatorAPIError(c, err)
	}
	return c.JSON(http.StatusAccepted, mutationResponse{WorkItemID: id, Kind: "priority", Status: "pending", OutboxID: item.ID})
}

func (s *Service) changeWorkItemOrder(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request orderMutationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.Scope = strings.TrimSpace(request.Scope)
	request.State = strings.TrimSpace(request.State)
	request.Rank = strings.TrimSpace(request.Rank)
	if request.Scope == "" || request.State == "" || request.Rank == "" {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_order", Message: "Scope, state, and rank are required"})
	}
	if err := s.database.changeQueueOrder(c.Request().Context(), id, request); err != nil {
		return operatorAPIError(c, err)
	}
	return c.JSON(http.StatusOK, mutationResponse{WorkItemID: id, Kind: "order", Status: "applied"})
}

func operatorAPIError(c echo.Context, err error) error {
	status := http.StatusUnprocessableEntity
	code := "invalid_mutation"
	message := "Mutation is invalid"
	switch {
	case errors.Is(err, tracker.ErrWorkItemNotFound):
		status, code, message = http.StatusNotFound, "work_item_not_found", "Work item was not found"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "idempotency_conflict", "Idempotency key conflicts with an earlier mutation"
	case strings.Contains(err.Error(), "not found"):
		status, code, message = http.StatusNotFound, "mutation_target_not_found", "Mutation target was not found"
	}
	return c.JSON(status, apiErrorResponse{Code: code, Message: message})
}

func (d *database) workItemRepositoryID(ctx context.Context, id tracker.WorkItemID) (int64, error) {
	var repositoryID int64
	if err := d.db.QueryRowContext(ctx, "SELECT repository_id FROM issues WHERE id = ?", id).Scan(&repositoryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: %d", tracker.ErrWorkItemNotFound, id)
		}
		return 0, fmt.Errorf("read hub work item repository: %w", err)
	}
	return repositoryID, nil
}

func (d *database) changeDependency(ctx context.Context, id tracker.WorkItemID, request dependencyMutationRequest) (resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hub dependency mutation: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	if err := requireWorkItem(ctx, tx, id); err != nil {
		return err
	}
	if err := requireWorkItem(ctx, tx, request.BlockerWorkItemID); err != nil {
		return err
	}
	now, err := d.currentTime()
	if err != nil {
		return err
	}
	if request.Action == "add" {
		var createsCycle bool
		if err := tx.QueryRowContext(ctx, `
WITH RECURSIVE descendants(id) AS (
  SELECT dependent_issue_id FROM issue_dependencies WHERE blocker_issue_id = ?
  UNION
  SELECT dependency.dependent_issue_id
  FROM issue_dependencies dependency
  JOIN descendants ON dependency.blocker_issue_id = descendants.id
)
SELECT EXISTS(SELECT 1 FROM descendants WHERE id = ?)`, id, request.BlockerWorkItemID).Scan(&createsCycle); err != nil {
			return fmt.Errorf("validate hub dependency graph: %w", err)
		}
		if createsCycle {
			return errors.New("dependency would create a cycle")
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(blocker_issue_id, dependent_issue_id) DO UPDATE SET provenance = excluded.provenance, updated_at = excluded.updated_at`,
			request.BlockerWorkItemID, id, request.Provenance, formatHubTime(now), formatHubTime(now))
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM issue_dependencies WHERE blocker_issue_id = ? AND dependent_issue_id = ?", request.BlockerWorkItemID, id)
	}
	if err != nil {
		return fmt.Errorf("apply hub dependency mutation: %w", err)
	}
	if err := insertOperatorEvent(ctx, tx, id, "dependency_"+request.Action, map[string]any{"blocker_work_item_id": request.BlockerWorkItemID, "provenance": request.Provenance}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hub dependency mutation: %w", err)
	}
	return nil
}

func (d *database) changeQueueOrder(ctx context.Context, id tracker.WorkItemID, request orderMutationRequest) (resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hub order mutation: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	now, err := d.currentTime()
	if err != nil {
		return err
	}
	if err := upsertQueueEntry(ctx, tx, id, request.Scope, request.State, request.Rank, nil, formatHubTime(now)); err != nil {
		return err
	}
	if err := insertOperatorEvent(ctx, tx, id, "queue_order_changed", map[string]any{"scope": request.Scope, "state": request.State, "rank": request.Rank}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hub order mutation: %w", err)
	}
	return nil
}

func upsertQueueEntry(ctx context.Context, tx *sql.Tx, id tracker.WorkItemID, scope string, state string, rank string, priority *int, now string) error {
	var workflowStateID sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT workflow_state_id FROM issues WHERE id = ?", id).Scan(&workflowStateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", tracker.ErrWorkItemNotFound, id)
		}
		return fmt.Errorf("read hub queue work item: %w", err)
	}
	var existingRank string
	var existingPriority sql.NullInt64
	err := tx.QueryRowContext(ctx, "SELECT rank, priority_override FROM queue_entries WHERE scope = ? AND issue_id = ?", scope, id).Scan(&existingRank, &existingPriority)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read hub queue entry: %w", err)
	}
	if rank == "" {
		if err == nil {
			rank = existingRank
		} else {
			rank = fmt.Sprintf("issue-%020d", id)
		}
	}
	var priorityValue any
	if priority != nil {
		priorityValue = *priority
	} else if existingPriority.Valid {
		priorityValue = existingPriority.Int64
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO queue_entries (issue_id, workflow_state_id, scope, state, rank, priority_override, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, issue_id) DO UPDATE SET
  workflow_state_id = excluded.workflow_state_id,
  state = excluded.state,
  rank = excluded.rank,
  priority_override = excluded.priority_override,
  updated_at = excluded.updated_at`,
		id, workflowStateID, scope, state, rank, priorityValue, now, now)
	if err != nil {
		return fmt.Errorf("upsert hub queue entry: %w", err)
	}
	return nil
}

func insertOperatorEvent(ctx context.Context, tx *sql.Tx, id tracker.WorkItemID, kind string, payload map[string]any, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode hub operator event: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO work_events (issue_id, fencing_token, machine_id, session_id, run_id, kind, payload_json, occurred_at, recorded_at)
VALUES (?, NULL, NULL, NULL, NULL, ?, ?, ?, ?)`, id, kind, string(encoded), formatHubTime(now), formatHubTime(now))
	if err != nil {
		return fmt.Errorf("append hub operator event: %w", err)
	}
	return nil
}

func queuePriority(value string) (int, bool) {
	switch value {
	case "urgent":
		return tracker.QueuePriorityUrgent, true
	case "high":
		return tracker.QueuePriorityHigh, true
	case "normal":
		return tracker.QueuePriorityNormal, true
	case "low":
		return tracker.QueuePriorityLow, true
	case "unset":
		return 4, true
	default:
		return 0, false
	}
}
