package hubserver

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
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
	result, err := s.appendNativeRunEventTenant(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeRunEventError(c, request, err)
	}
	if request.Data.Handoff != nil && request.Data.Handoff.Publication != nil || request.Data.Finalization != nil && request.Data.Finalization.Publication != nil {
		s.pullRequests.invalidate(pullRequestCacheKey(nativeRequestScope(c), c.Param("item")))
	}
	return c.JSONBlob(http.StatusOK, result)
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
	var fence *leasestore.FenceError
	if errors.As(err, &fence) {
		return s.refusedAPIError(c, http.StatusConflict, "stale_fencing_token", "Lease fencing token is stale", apiErrorResponse{Code: "stale_fencing_token", Message: "Lease fencing token is stale"}, "fence_check", fence.Check)
	}
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
