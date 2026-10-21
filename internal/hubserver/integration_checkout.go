package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// A checkout report contains only the canonical repository name. The runner
// derives it from its local origin; no remote URL or credentials reach the Hub.
func validCheckoutRepository(repository string) bool {
	owner, name, ok := splitRepositoryFullName(repository)
	if !ok || len(repository) > 200 {
		return false
	}
	for _, part := range []string{owner, name} {
		if part == "." || part == ".." {
			return false
		}
		for _, char := range part {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func updateRunnerCheckoutReport(ctx context.Context, tx *sql.Tx, scope nativeScope, report *string, now time.Time) error {
	if report == nil {
		return nil // older runners do not modify the last report
	}
	if scope.credential.Runner.RunnerID == "" {
		return nativePublicInvalid("Checkout reports require an enrolled runner")
	}
	if *report == "" {
		_, err := tx.ExecContext(ctx, "DELETE FROM runner_checkout_repositories WHERE runner_id = ? AND project_id = ?", scope.credential.Runner.RunnerID, scope.project)
		return err
	}
	if !validCheckoutRepository(*report) || *report != strings.TrimSpace(*report) {
		return nativePublicInvalid("Checkout repository must be owner/name")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_checkout_repositories (runner_id, project_id, repository, reported_at) VALUES (?, ?, ?, ?)
ON CONFLICT(runner_id, project_id) DO UPDATE SET repository=excluded.repository, reported_at=excluded.reported_at`, scope.credential.Runner.RunnerID, scope.project, *report, formatHubTime(now))
	return err
}

func (s *Service) bindRunnerCheckoutCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, revision tracker.Revision, repository string) (json.RawMessage, error) {
	if !validCheckoutRepository(repository) {
		return nil, nativePublicInvalid("Repository must be owner/name")
	}
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
			return nil, nativePublicInvalid("Only a native project can associate a runner checkout")
		}
		if current.CheckoutRepository != "" {
			if strings.EqualFold(current.CheckoutRepository, repository) {
				return current, nil
			}
			return nil, nativePublicInvalid("This project already has a checkout repository")
		}
		if current.Repository != "" && !strings.EqualFold(current.Repository, repository) {
			return nil, nativePublicInvalid("This project already has a repository")
		}
		if err := requireIntegrationIdle(ctx, tx, scope, now); err != nil {
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
