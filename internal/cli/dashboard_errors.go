package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func classifyDashboardReadError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var transport *DashboardTransportError
	if errors.As(err, &transport) {
		if transport.Timeout {
			return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, transport.Error(), "Confirm Detent is responsive, or retry the command.", nil)
		}
		return NewClassifiedError(ErrDashboardUnreachable, errorCodeDashboardUnreachable, transport.Error(), "Start Detent or verify the configured host and port.", nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, "dashboard API request timed out", "Confirm Detent is responsive, or retry the command.", nil)
	}
	var response *DashboardResponseError
	if !errors.As(err, &response) {
		return err
	}
	detail := strings.TrimSpace(response.Message)
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return NewClassifiedError(ErrDashboardUnauthorized, errorCodeDashboardUnauthorized, detail, "Set DETENT_API_TOKEN to a valid read credential or update api_token in the resolved config.", nil)
	case response.StatusCode == http.StatusForbidden:
		return NewClassifiedError(ErrDashboardForbidden, errorCodeDashboardForbidden, detail, "Use a credential with read scope for the requested project.", nil)
	case response.Code == "ambiguous_reference":
		return NewClassifiedError(ErrDashboardAmbiguousReference, errorCodeAmbiguousReference, detail, "Use an issue ID, canonical identifier, or full issue URL within the selected project.", nil)
	case response.Code == "issue_not_found":
		return NewClassifiedError(ErrDashboardIssueNotFound, errorCodeIssueNotFound, detail, "Verify --project and the issue reference.", nil)
	case response.StatusCode == http.StatusNotFound:
		return NewClassifiedError(ErrDashboardUnsupportedModel, errorCodeUnsupportedModelVersion, "running service does not support the issue explanation API", "Upgrade or restart Detent so the running service provides the issue explanation endpoint.", nil)
	case response.Code == "version_conflict":
		return NewClassifiedError(ErrDashboardUnsupportedModel, errorCodeUnsupportedModelVersion, detail, "Upgrade Detent so the CLI and running service support the same read-model schema.", nil)
	case response.Code == "bad_request":
		return NewClassifiedError(ErrValidation, errorCodeValidation, detail, "Verify --project and the issue reference.", nil)
	case response.Code == "runtime_unavailable":
		return NewClassifiedError(ErrDashboardRuntimeUnavailable, errorCodeRuntimeUnavailable, detail, "Retry after the running Detent service publishes its issue explanation runtime.", nil)
	default:
		return NewClassifiedError(ErrDashboardRequestFailed, errorCodeDashboardRequestFailed, detail, "Inspect the running Detent service and retry the command.", nil)
	}
}
