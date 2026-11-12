package hubserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

// A checkout report contains only the canonical repository name. The runner
// derives it from its local origin; no remote URL or credentials reach the Hub.
func validCheckoutRepository(repository string) bool {
	return runnerstore.ValidCheckoutRepository(repository)
}

func (s *Service) verifiedProjectCheckout(ctx context.Context, query nativeQueryer, scope nativeScope, repository string, now time.Time) (bool, error) {
	var verified bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		verified, err = r.RunnerCheckoutVerified(ctx, scope.project, repository, now)
		return err
	})
	return verified, err
}

func validateRunnerCheckoutReport(scope nativeScope, report *string) error {
	if report == nil {
		return nil
	}
	if scope.credential.Runner.RunnerID == "" {
		return nativePublicInvalid("Checkout reports require an enrolled runner")
	}
	if *report != "" && (!validCheckoutRepository(*report) || *report != strings.TrimSpace(*report)) {
		return nativePublicInvalid("Checkout repository must be owner/name")
	}
	return nil
}

func repositoryBindingRefusal(code, message string) error {
	return &nativeError{Code: code, Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}
