package hubserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func validNativeID(id, prefix string) bool {
	value, ok := strings.CutPrefix(id, prefix+"_")
	length := 32
	if prefix == "policy" {
		length = 64
	}
	if !ok || len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateNativeRunEvent(request tracker.NativeRunEvent) error {
	if request.SchemaVersion != 1 || !slices.Contains([]string{"run.started", "run.finished", "run.checkpointed"}, request.Type) {
		return nativeInvalid("Unsupported event type or schema version")
	}
	data := request.Data
	if !validNativeID(data.RunID, "run") || !validNativeID(data.AttemptID, "attempt") || !validNativeID(data.PolicyID, "policy") || data.FencingToken <= 0 || data.LeaseID == "" {
		return nativeInvalid("Typed run, attempt, policy and current lease references are required")
	}
	if request.Type == "run.finished" {
		if !slices.Contains([]string{"succeeded", "failed", "cancelled", "interrupted"}, data.Outcome) {
			return nativeInvalid("Run outcome is invalid")
		}
	} else if data.Outcome != "" {
		return nativeInvalid("Only a finished run has an outcome")
	}
	if len(data.ArtifactIDs) > 20 || request.Type != "run.checkpointed" && len(data.ArtifactIDs) != 0 {
		return nativeInvalid("Artifact references require a checkpoint and are limited to 20")
	}
	for _, id := range data.ArtifactIDs {
		if !validNativeID(id, "artifact") {
			return nativeInvalid("Artifact references must be typed IDs")
		}
	}
	return nil
}

func (s *Service) appendNativeRunEvent(c echo.Context) error {
	var request tracker.NativeRunEvent
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := validateNativeRunEvent(request); err != nil {
			return nil, err
		}
		issue, id, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
		if err != nil {
			return nil, err
		}
		lease, found, err := readLeaseByID(ctx, tx, request.Data.LeaseID)
		if err != nil {
			return nil, err
		}
		if !found || lease.issueID != id {
			return nil, nativeNotFound()
		}
		if err := requireCurrentLease(lease, request.Data.FencingToken, now); err != nil {
			return nil, err
		}
		if err := requireApprovedLeasePolicy(ctx, tx, request.Data.LeaseID, true); err != nil {
			return nil, err
		}
		var pinned string
		if err := tx.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", request.Data.LeaseID).Scan(&pinned); err != nil || pinned != request.Data.PolicyID {
			return nil, policyMismatch("Run event policy must match the policy pinned to its lease")
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM machines WHERE id = ? AND organization_id = ? AND token_id = ?", lease.session.Machine.ID, scope.organization, scope.credential.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, nativeNotFound()
		}
		if err := appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), request.Type, tracker.CollaborationData{Run: &request.Data}, now); err != nil {
			return nil, err
		}
		return struct {
			Accepted bool `json:"accepted"`
		}{true}, nil
	})
}
