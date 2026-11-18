package hubauth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

type AuthorizationFailure struct {
	Cause error
	Kind  error
}

func (e *AuthorizationFailure) Error() string { return e.Kind.Error() }
func (e *AuthorizationFailure) Unwrap() error { return e.Kind }
func (e *AuthorizationFailure) Is(target error) bool {
	return errors.Is(e.Kind, target) || target == tenantauth.ErrUnavailable && (errors.Is(e.Kind, operatortool.ErrServiceUnavailable) || errors.Is(e.Kind, operatortool.ErrProviderUnavailable))
}
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var failure *AuthorizationFailure
	if errors.As(err, &failure) {
		return failure
	}
	if auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || errors.Is(err, operatortool.ErrProviderUnavailable) {
		return &AuthorizationFailure{Cause: err, Kind: operatortool.ErrProviderUnavailable}
	}
	if errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
		return &AuthorizationFailure{Cause: err, Kind: operatortool.ErrServiceUnavailable}
	}
	var denied *operatortool.AuthorizationError
	if errors.As(err, &denied) && denied.Cause != nil {
		return Classify(denied.Cause)
	}
	var refusal *apikey.Refusal
	var native *hubapi.Error
	if errors.As(err, &refusal) || errors.As(err, &native) && (native.Status == http.StatusForbidden || native.Status == http.StatusNotFound) {
		return operatortool.ErrAccessDenied
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return err
	}
	for _, denial := range []error{operatortool.ErrAccessDenied, accessstore.ErrDenied, auth.ErrHostedIdentity, auth.ErrInvalidSession, auth.ErrAccessExpired, cloudassert.ErrInvalid, corestore.ErrNotFound, corestore.ErrTenantMismatch, sql.ErrNoRows} {
		if errors.Is(err, denial) {
			return operatortool.ErrAccessDenied
		}
	}
	return &AuthorizationFailure{Cause: err, Kind: operatortool.ErrServiceUnavailable}
}
