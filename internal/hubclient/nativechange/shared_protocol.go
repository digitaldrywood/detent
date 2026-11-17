package nativechange

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/provider"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeLeaseLost(err error) bool          { return api.NativeLeaseLost(err) }
func executionID(prefix, value string) string { return api.ExecutionID(prefix, value) }
func cancelNativeReleasedAttempt(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, attempt tracker.NativeAttempt) error {
	return api.CancelNativeReleasedAttempt(ctx, client, item, attempt)
}
func nativeTransportUnavailable(err error) bool { return api.NativeTransportUnavailable(err) }
func nativeAuthorityLost(err error) bool        { return api.NativeAuthorityLost(err) }

func diagnosticError(err error) string { return api.DiagnosticError(err) }
func diagnosticErrorCode(err error) string {
	var mismatch *provider.IdentityMismatchError
	switch {
	case errors.Is(err, provider.ErrReportSourceMissing):
		return "provider_report_source_missing"
	case errors.As(err, &mismatch):
		return mismatch.DiagnosticCode()
	case errors.Is(err, provider.ErrIdentityMismatch):
		return "provider_identity_mismatch"
	default:
		return api.DiagnosticErrorCode(err)
	}
}
func nativeAdmissionCapacityFull(err error) bool { return api.NativeAdmissionCapacityFull(err) }
