package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// A checkout report contains only the canonical repository name. The runner
// derives it from its local origin; no remote URL or credentials reach the Hub.
func validCheckoutRepository(repository string) bool {
	return runnerstore.ValidCheckoutRepository(repository)
}

func (s *Service) verifiedProjectCheckout(ctx context.Context, query nativeQueryer, scope nativeScope, repository string, now time.Time) (bool, error) {
	if s.config.TenantRepository != nil {
		var verified bool
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			verified, err = r.RunnerCheckoutVerified(ctx, scope.project, repository, now)
			return err
		})
		return verified, err
	}
	return verifiedProjectCheckoutSQLite(ctx, query, scope, repository, now)
}

func verifiedProjectCheckoutSQLite(ctx context.Context, query nativeQueryer, scope nativeScope, repository string, now time.Time) (bool, error) {
	var verified bool
	err := query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM runner_checkout_repositories cr
JOIN runner_identities r ON r.id=cr.runner_id AND r.organization_id=?
JOIN api_tokens t ON t.id=r.token_id AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?))
JOIN token_grants g ON g.token_id=t.id AND g.organization_id=? AND g.project_id=cr.project_id
WHERE cr.project_id=? AND cr.repository=? COLLATE NOCASE AND r.state='active' AND julianday(cr.reported_at)>julianday(?))`,
		scope.organization, formatHubTime(now), scope.organization, scope.project, repository, formatHubTime(now.Add(-5*time.Minute))).Scan(&verified)
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

func updateRunnerCheckoutReport(ctx context.Context, tx *sql.Tx, scope nativeScope, report *string, now time.Time) error {
	if err := validateRunnerCheckoutReport(scope, report); err != nil {
		return err
	}
	if report == nil {
		return nil
	}
	if *report == "" {
		_, err := tx.ExecContext(ctx, "DELETE FROM runner_checkout_repositories WHERE runner_id = ? AND project_id = ?", scope.credential.Runner.RunnerID, scope.project)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_checkout_repositories (runner_id, project_id, repository, reported_at) VALUES (?, ?, ?, ?)
ON CONFLICT(runner_id, project_id) DO UPDATE SET repository=excluded.repository, reported_at=excluded.reported_at`, scope.credential.Runner.RunnerID, scope.project, *report, formatHubTime(now))
	return err
}

func (s *Service) bindRunnerCheckoutCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, revision tracker.Revision, repository string) (json.RawMessage, error) {
	owner, name, valid := splitRepositoryFullName(repository)
	if !valid || !validCheckoutRepository(owner+"/"+name) {
		return nil, repositoryBindingRefusal("repository_invalid", "Repository must be owner/name")
	}
	repository = owner + "/" + name
	input := struct {
		tracker.Mutation
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
		Repository       string           `json:"repository"`
		Source           string           `json:"source"`
	}{command, revision, repository, "runner_checkout"}
	return s.executeNativeMutation(ctx, scope, options, command, input, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != revision {
			return nil, nativeConflict(current.Revision)
		}
		if current.Profile != "native" {
			return nil, repositoryBindingRefusal("repository_profile_invalid", "Only a native project can associate a runner checkout")
		}
		if current.CheckoutRepository != "" {
			if strings.EqualFold(current.CheckoutRepository, repository) {
				return current, nil
			}
			return nil, repositoryBindingRefusal("repository_already_bound", "This project already has a checkout repository")
		}
		if current.Repository != "" && !strings.EqualFold(current.Repository, repository) {
			return nil, repositoryBindingRefusal("repository_already_bound", "This project already has a repository")
		}
		if err := requireIntegrationIdle(ctx, tx, scope, now); err != nil {
			var refusal *nativeError
			if errors.As(err, &refusal) && refusal.Code == "invalid_request" {
				return nil, repositoryBindingRefusal("repository_binding_busy", refusal.Message)
			}
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET checkout_repository=?, integration_revision=integration_revision+1 WHERE id=?", repository, scope.project); err != nil {
			return nil, err
		}
		if err := ensureNativeTriage(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	})
}

func repositoryBindingRefusal(code, message string) error {
	return &nativeError{Code: code, Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}
