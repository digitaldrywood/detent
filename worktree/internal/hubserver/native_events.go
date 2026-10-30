package hubserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	if request.SchemaVersion != 1 || !slices.Contains([]string{"run.started", "run.finished", "run.checkpointed", "run.observed"}, request.Type) {
		return nativeRejection(nativeInvalid("Unsupported event type or schema version"), "event_validation", "")
	}
	data := request.Data
	if data.Runtime != nil && data.Sequence <= 0 || request.Type == "run.observed" && data.Runtime == nil {
		return nativeRejection(nativeInvalid("Runtime evidence requires an ordered observation"), "event_validation", "data")
	}
	if !validNativeID(data.RunID, "run") || !validNativeID(data.AttemptID, "attempt") || !validNativeID(data.PolicyID, "policy") || data.FencingToken <= 0 || data.LeaseID == "" {
		return nativeRejection(nativeInvalid("Typed run, attempt, policy and current lease references are required"), "event_validation", "data")
	}
	if request.Type == "run.finished" {
		if !slices.Contains([]string{"succeeded", "failed", "cancelled", "interrupted"}, data.Outcome) {
			return nativeRejection(nativeInvalid("Run outcome is invalid"), "event_validation", "data.outcome")
		}
	} else if data.Outcome != "" {
		return nativeRejection(nativeInvalid("Only a finished run has an outcome"), "event_validation", "data.outcome")
	}
	if len(data.ArtifactIDs) > 20 || request.Type != "run.checkpointed" && len(data.ArtifactIDs) != 0 {
		return nativeRejection(nativeInvalid("Artifact references require a checkpoint and are limited to 20"), "event_validation", "data.artifact_ids")
	}
	for _, id := range data.ArtifactIDs {
		if !validNativeID(id, "artifact") {
			return nativeRejection(nativeInvalid("Artifact references must be typed IDs"), "event_validation", "data.artifact_ids")
		}
	}
	if err := validateNativeRunUsage(request); err != nil {
		return nativeRejection(err, "usage_validation", "data.usage")
	}
	if err := validateNativeRuntime(data.Runtime); err != nil {
		return err
	}
	return validateNativeExecution(data, request.Type)
}

func (s *Service) appendNativeRunEvent(c echo.Context) error {
	var request tracker.NativeRunEvent
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidNativeEventRequest(c, err)
	}
	if err := validateNativeRunEvent(request); err != nil {
		return s.nativeRunEventError(c, request, err)
	}
	ctx := c.Request().Context()
	_, replay, err := s.nativeCommandReplay(ctx, nativeRequestScope(c), c.Request().Method+" "+c.Request().URL.EscapedPath(), request.IdempotencyKey, request)
	if err != nil {
		return s.nativeRunEventError(c, request, err)
	}
	if !replay {
		ctx, err = s.prepareRunDetail(ctx, nativeRequestScope(c), c.Param("item"), request)
		if err != nil {
			return s.nativeRunEventError(c, request, err)
		}
		c.SetRequest(c.Request().WithContext(ctx))
	}
	err = s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		events := append(append([]tracker.NativeRunEvent{}, request.PriorEvents...), request)
		for _, event := range events {
			if _, err := s.recordNativeRunEvent(ctx, tx, scope, c.Param("item"), event, now); err != nil {
				return nil, err
			}
		}
		return struct {
			Accepted bool `json:"accepted"`
		}{true}, nil
	})
	if err == nil && (request.Data.Handoff != nil && request.Data.Handoff.Publication != nil || request.Data.Finalization != nil && request.Data.Finalization.Publication != nil) {
		s.pullRequests.invalidate(pullRequestCacheKey(nativeRequestScope(c), c.Param("item")))
	}
	return err
}

func (s *Service) validateNativeRunOwner(ctx context.Context, tx nativeQueryer, scope nativeScope, item string, request *tracker.NativeRunEvent, now time.Time) (tracker.NativeIssue, bool, error) {
	if err := validateNativeRunEvent(*request); err != nil {
		return tracker.NativeIssue{}, false, err
	}
	issue, id, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return tracker.NativeIssue{}, false, err
	}
	lease, found, err := readLeaseByID(ctx, tx, request.Data.LeaseID)
	if err != nil {
		return tracker.NativeIssue{}, false, err
	}
	if !found {
		return tracker.NativeIssue{}, false, nativeNotFoundAt("event_lease_missing")
	}
	if lease.issueID != id {
		return tracker.NativeIssue{}, false, nativeNotFoundAt("event_lease_other_item")
	}
	termination := lease.session.ReleasedAt != nil && lease.session.FencingToken == request.Data.FencingToken &&
		request.Type == "run.finished" && request.Data.Outcome == "cancelled" && request.Data.Sequence > 0
	if !termination {
		if err := requireCurrentLease(lease, request.Data.FencingToken, now); err != nil {
			return tracker.NativeIssue{}, false, err
		}
	}
	if request.Type == "run.started" {
		if err := checkMonthlyLease(ctx, tx, lease, now); err != nil {
			return tracker.NativeIssue{}, false, err
		}
	}
	if !termination {
		if err := requireLeaseRouting(ctx, tx, request.Data.LeaseID); err != nil {
			return tracker.NativeIssue{}, false, err
		}
	}
	var pinned string
	if err := tx.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", request.Data.LeaseID).Scan(&pinned); err != nil || pinned != request.Data.PolicyID {
		return tracker.NativeIssue{}, false, policyMismatch("Run event policy must match the policy pinned to its lease")
	}
	if err := requireLeaseRunner(ctx, tx, request.Data.LeaseID, scope); err != nil {
		return tracker.NativeIssue{}, false, err
	}
	if f := request.Data.Finalization; !termination && f != nil && f.ChangeID != "" {
		if _, err := readChange(ctx, tx, scope, string(issue.WorkItemID), f.ChangeID); err != nil {
			return tracker.NativeIssue{}, false, err
		}
		if f.VersionID != "" {
			version, err := readChangeVersion(ctx, tx, f.ChangeID, f.VersionID)
			if err != nil {
				return tracker.NativeIssue{}, false, err
			}
			if version.HeadSHA != f.HeadSHA {
				return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Finalizer version must name its recorded head"), "owner_validation", "data.finalization")
			}
		}
		if source := f.SourceVersion; source != nil {
			if source.ChangeID != f.ChangeID {
				return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Finalizer source must belong to the reported Change"), "owner_validation", "data.finalization")
			}
			version, err := readChangeVersion(ctx, tx, source.ChangeID, source.VersionID)
			if err != nil {
				return tracker.NativeIssue{}, false, err
			}
			if version.HeadSHA != source.HeadSHA || version.AttemptID != f.SourceAttemptID {
				return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Finalizer source must match its recorded version and attempt"), "owner_validation", "data.finalization")
			}
		}
	}
	if runtime := request.Data.Runtime; !termination && runtime != nil && runtime.Landing != nil && runtime.Landing.ChangeID != "" {
		l := runtime.Landing
		change, err := readChange(ctx, tx, scope, string(issue.WorkItemID), l.ChangeID)
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		if l.VersionID != "" {
			version, err := readChangeVersion(ctx, tx, change.ID, l.VersionID)
			if err != nil {
				return tracker.NativeIssue{}, false, err
			}
			if version.HeadSHA != l.HeadSHA {
				return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Landing evidence must name its immutable version head"), "owner_validation", "data.runtime.landing")
			}
		}
		if l.Landed && (change.Landed == nil || change.Landed.VersionID != l.VersionID || change.Landed.MergeSHA != l.MergeSHA || change.Landed.HeadSHA != l.HeadSHA || change.Landed.BaseRef != l.BaseRef || change.Landed.Method != l.Method) {
			return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Landing evidence requires the recorded Change landing authority"), "owner_validation", "data.runtime.landing")
		}
	}
	if request.Type == "run.finished" && request.Data.Outcome == "succeeded" && request.Data.Sequence > 0 {
		if err := normalizeNativeDependencyBlockers(ctx, tx, scope, request.Data.Disposition); err != nil {
			return tracker.NativeIssue{}, false, err
		}
	}
	if request.Data.Sequence > 0 {
		if request.Data.MachineID != "" && request.Data.MachineID != lease.session.Machine.ID || request.Data.SessionID != "" && request.Data.SessionID != lease.session.SessionID || request.Data.RunnerID != "" && request.Data.RunnerID != scope.credential.Runner.RunnerID {
			return tracker.NativeIssue{}, false, nativeRejection(nativeInvalid("Execution identity must match the authenticated lease owner"), "owner_validation", "data")
		}
		request.Data.MachineID = lease.session.Machine.ID
		request.Data.SessionID = lease.session.SessionID
		request.Data.RunnerID = scope.credential.Runner.RunnerID
	}
	return issue, termination, nil
}

func (s *Service) recordNativeRunEvent(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, request tracker.NativeRunEvent, now time.Time) (any, error) {
	raw, cloneErr := json.Marshal(request)
	if cloneErr != nil {
		return nil, cloneErr
	}
	request = tracker.NativeRunEvent{}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}

	authorityTime, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	issue, termination, err := s.validateNativeRunOwner(ctx, tx, scope, item, &request, authorityTime)
	if err != nil {
		return nil, err
	}
	publish := true
	if request.Data.Sequence > 0 {
		appended, history, err := recordNativeAttempt(ctx, tx, scope, issue.WorkItemID, &request, now, termination)
		if err != nil {
			return nil, err
		}
		publish = history
		if !appended {
			return struct {
				Accepted bool `json:"accepted"`
			}{true}, nil
		}
	} else {
		_, reserved, err := readProviderReservation(ctx, tx, request.Data.LeaseID)
		if err != nil {
			return nil, err
		}
		if reserved {
			return nil, nativeRejection(nativeInvalid("Provider reservations require ordered execution events"), "owner_validation", "data.sequence")
		}
		var ordered int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM native_attempts WHERE id = ? OR lease_id = ?", request.Data.AttemptID, request.Data.LeaseID).Scan(&ordered); err != nil {
			return nil, err
		}
		if ordered != 0 {
			return nil, nativeExecutionConflict("Ordered attempts cannot accept legacy events")
		}
	}
	if !termination {
		if err := recordAttemptUsage(ctx, tx, scope, request, s.usagePrices(), now); err != nil {
			return nil, err
		}
	}
	if publish {
		if request.Data.Sequence == 0 {
			if err := recordRunDetailReference(ctx, tx, scope, issue.WorkItemID, request.Data, formatHubTime(now)); err != nil {
				return nil, err
			}
		}
		if request.Type == "run.finished" && request.Data.Outcome == "succeeded" && request.Data.Sequence > 0 {
			issue, err = recordNativeDependencyBlockers(ctx, tx, scope, issue, request.Data.Disposition, now)
			if err != nil {
				return nil, err
			}
		}
		data := tracker.CollaborationData{Run: &request.Data}
		if request.Type == "run.finished" {
			data.Revision = issue.Revision
		}
		if err := appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), request.Type, data, now); err != nil {
			return nil, err
		}
	}
	return struct {
		Accepted bool `json:"accepted"`
	}{true}, nil
}

func (s *Service) invalidNativeEventRequest(c echo.Context, err error) error {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return s.invalidAPIRequest(c, err)
	}
	field := ""
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && (runnerauth.PublicationRejection{Category: "body_decode", Field: typeError.Field}).Valid() {
		field = typeError.Field
	}
	return s.nativeRunEventError(c, tracker.NativeRunEvent{}, nativeRejection(nativeInvalid("Request body is invalid"), "body_decode", field))
}

func (s *Service) nativeRunEventError(c echo.Context, request tracker.NativeRunEvent, err error) error {
	var attributes []any
	var failure *nativeError
	if errors.As(err, &failure) && failure.status == http.StatusUnprocessableEntity {
		rejection, ok := failure.Details["rejection"].(runnerauth.PublicationRejection)
		if !ok || !rejection.Valid() {
			rejection = runnerauth.PublicationRejection{Category: "event_validation"}
			err = nativeRejection(err, rejection.Category, rejection.Field)
		}
		attempt := request.Data.AttemptID
		if !validNativeID(attempt, "attempt") {
			attempt = ""
		}
		scope := nativeRequestScope(c)
		item := c.Param("item")
		if !validNativeID(item, "wi") {
			item = ""
		}
		attributes = []any{"category", rejection.Category, "field", rejection.Field,
			"organization", scope.organization, "project", scope.project, "work_item", item, "attempt", attempt}
	}
	return s.nativeAPIError(c, err, attributes...)
}
