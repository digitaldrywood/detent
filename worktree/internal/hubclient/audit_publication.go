package hubclient

import (
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type auditPublicationError struct {
	err        error
	diagnostic tracker.PublicationFailure
}

func (e *auditPublicationError) Error() string {
	return "security audit publication failed"
}

func (e *auditPublicationError) Unwrap() error {
	return e.err
}

func (e *auditPublicationError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	failure := tracker.NativeTerminalFailure{}
	var carrier interface {
		NativeTerminalFailure() tracker.NativeTerminalFailure
	}
	if errors.As(e.err, &carrier) {
		failure = carrier.NativeTerminalFailure()
	}
	failure.Publication = &e.diagnostic
	return failure
}

func auditPublicationFailure(err error, request tracker.ReviewChange) error {
	if err == nil || request.SecurityAudit == nil {
		return err
	}
	encoded, encodeErr := json.Marshal(request)
	if encodeErr != nil {
		return err
	}
	diagnostic := tracker.PublicationFailure{
		Operation: "security_audit/review", Code: "request_failed",
		RequestBytes: int64(len(encoded)), BodyBytes: int64(len(request.Body)),
		OutputBytes: int64(request.SecurityAudit.Run.OutputBytes),
	}
	var api *APIError
	if errors.As(err, &api) && api.Status >= 300 && api.Status <= 599 {
		diagnostic.Code, diagnostic.HTTPStatus = "http_rejection", api.Status
	}
	return &auditPublicationError{err: err, diagnostic: diagnostic}
}
