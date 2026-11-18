package githubbinding

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) VerifiedCheckout(ctx context.Context, scope hubapi.Scope, repository string, now time.Time) (bool, error) {
	var verified bool
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		verified, err = r.RunnerCheckoutVerified(ctx, scope.Project, repository, now)
		return err
	})
	return verified, err
}

func ValidateCheckoutReport(runner string, report *string) error {
	if report == nil {
		return nil
	}
	if runner == "" {
		return hubapi.PublicInvalid("Checkout reports require an enrolled runner")
	}
	if *report != "" && (!runnerstore.ValidCheckoutRepository(*report) || *report != strings.TrimSpace(*report)) {
		return hubapi.PublicInvalid("Checkout repository must be owner/name")
	}
	return nil
}
