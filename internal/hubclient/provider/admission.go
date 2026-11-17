package provider

import (
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var (
	ErrReportSourceMissing = errors.New("provider report source is missing for the dispatch reservation")
	ErrIdentityMismatch    = errors.New("provider execution identity differs from its dispatch reservation")
)

func ValidateStart(reservation *providercapacity.Reservation, identity tracker.NativeExecutionIdentity, reportsSource func() ([]providercapacity.Report, error), reject func(error) error) error {
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
		if required.Validate() != nil || reservation.Requirement.Validate() != nil {
			return reject(ErrIdentityMismatch)
		}
		return reject(fmt.Errorf("%w: reserved %s/%s/%s, selected %s/%s/%s", ErrIdentityMismatch, reservation.Role, reservation.Backend, reservation.Model, required.Role, required.Backend, required.Model))
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
			if report.MaxConcurrent > 0 && (reservation.Report.MaxConcurrent == 0 || report.MaxConcurrent < reservation.Report.MaxConcurrent) {
				return errors.Join(runner.ErrExecutionAuthorityUnavailable, errors.New("provider capacity decreased after dispatch; release and wait"))
			}
			return nil
		}
	}
	return reject(errors.New("local provider account or model capability changed after dispatch"))
}
