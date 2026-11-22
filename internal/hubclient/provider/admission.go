package provider

import (
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var (
	ErrReportSourceMissing = errors.New("provider report source is missing for the dispatch reservation")
	ErrIdentityMismatch    = errors.New("provider execution identity differs from its dispatch reservation")
)

type IdentityMismatchError struct {
	fields []string
}

func (e *IdentityMismatchError) Error() string {
	return ErrIdentityMismatch.Error() + ": " + strings.Join(e.fields, ", ")
}

func (e *IdentityMismatchError) Unwrap() error { return ErrIdentityMismatch }

func (e *IdentityMismatchError) DiagnosticCode() string {
	return "provider_identity_mismatch_" + strings.Join(e.fields, "_")
}

func ValidateStart(reservation *providercapacity.Reservation, identity tracker.NativeExecutionIdentity, backendIdentity string, reportsSource func() ([]providercapacity.Report, error), reject func(error) error) error {
	if reservation == nil {
		if reportsSource != nil {
			return reject(errors.New("provider reservation is missing"))
		}
		return nil
	}
	required := providercapacity.Requirement{Role: identity.Role, Backend: identity.Backend, Model: identity.Model}
	if reportsSource == nil {
		return reject(ErrReportSourceMissing)
	}
	if required != reservation.Requirement {
		var fields []string
		if required.Role != reservation.Role {
			fields = append(fields, "role")
		}
		if required.Backend != reservation.Backend {
			fields = append(fields, "backend")
		}
		if required.Model != reservation.Model {
			fields = append(fields, "model")
		}
		return reject(&IdentityMismatchError{fields: fields})
	}
	reports, err := reportsSource()
	if err != nil {
		return reject(err)
	}
	if err := providercapacity.Validate(reports); err != nil {
		return reject(err)
	}
	for _, report := range reports {
		if report.Supports(required) && report.Provider == reservation.Report.Provider && report.AccountAlias == reservation.Report.AccountAlias && report.SharedAccountAlias == reservation.Report.SharedAccountAlias {
			if backendIdentity != "" && report.LocalBackendIdentity != backendIdentity {
				return reject(errors.New("local provider backend identity conflicts across projects"))
			}
			if report.MaxConcurrent > 0 && (reservation.Report.MaxConcurrent == 0 || report.MaxConcurrent < reservation.Report.MaxConcurrent) {
				return errors.Join(runner.ErrExecutionAuthorityUnavailable, errors.New("provider capacity decreased after dispatch; release and wait"))
			}
			return nil
		}
	}
	return reject(errors.New("local provider account or model capability changed after dispatch"))
}
