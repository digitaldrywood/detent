package hubserver

import (
	"context"
	"errors"
	"fmt"
)

var ErrHostedMigrationMismatch = errors.New("hosted database does not match the shared-origin migration target")

type HostedSharedMigration struct {
	OrganizationID         string `json:"organization_id"`
	ProviderOrganizationID string `json:"provider_organization_id"`
	FromDeployment         string `json:"from_deployment"`
	FromPublicURL          string `json:"from_public_url"`
	FromGeneration         int64  `json:"from_generation"`
	PublicURL              string `json:"public_url"`
	Generation             int64  `json:"generation"`
	Members                int    `json:"members"`
	Projects               int    `json:"projects"`
	Runners                int    `json:"runners"`
	Issues                 int    `json:"issues"`
	BillingCustomer        string `json:"billing_customer,omitempty"`
	SessionsRevoked        int64  `json:"sessions_revoked"`
	TransactionsClosed     int64  `json:"transactions_closed"`
	GrantsExpired          int64  `json:"grants_expired"`
	AlreadyMigrated        bool   `json:"already_migrated"`
}

func MigrateHostedSharedOrigin(ctx context.Context, cfg Config) (result HostedSharedMigration, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
	if cfg.Hosted == nil || cfg.Hosted.SharedEntry == nil {
		return HostedSharedMigration{}, errors.New("shared-origin migration requires a hosted configuration with shared_entry")
	}
	if err := cfg.Hosted.validate(); err != nil {
		return HostedSharedMigration{}, err
	}
	cfg.hostedBindingMigration = true
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return HostedSharedMigration{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, database.Close())
	}()
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return HostedSharedMigration{}, err
	}
	defer tx.Rollback()
	target := cfg.Hosted
	var bootstrap string
	err = tx.QueryRowContext(ctx, "SELECT organization_id, provider_id, bootstrap_subject, public_url, deployment, allocation_generation FROM hosted_tenant WHERE singleton = 1").Scan(&result.OrganizationID, &result.ProviderOrganizationID, &bootstrap, &result.FromPublicURL, &result.FromDeployment, &result.FromGeneration)
	if err != nil {
		return HostedSharedMigration{}, fmt.Errorf("%w: database has no hosted organization binding", ErrHostedMigrationMismatch)
	}
	result.PublicURL, result.Generation = target.PublicURL, target.SharedEntry.Generation
	if result.OrganizationID != target.OrganizationID || result.ProviderOrganizationID == "" || result.ProviderOrganizationID != target.WorkOSOrganizationID || bootstrap != target.BootstrapSubject {
		return HostedSharedMigration{}, fmt.Errorf("%w: organization, provider organization or bootstrap identity differs", ErrHostedMigrationMismatch)
	}
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM hosted_members WHERE active = 1), (SELECT count(*) FROM projects), (SELECT count(*) FROM runner_identities), (SELECT count(*) FROM issues),
COALESCE((SELECT customer_id FROM hosted_billing_accounts WHERE organization_id = ?), '')`, result.OrganizationID).Scan(&result.Members, &result.Projects, &result.Runners, &result.Issues, &result.BillingCustomer); err != nil {
		return HostedSharedMigration{}, err
	}
	if result.FromDeployment == "shared" && result.FromPublicURL == result.PublicURL && result.FromGeneration == result.Generation {
		result.AlreadyMigrated = true
		return result, tx.Commit()
	}
	if result.Generation <= result.FromGeneration {
		return HostedSharedMigration{}, fmt.Errorf("%w: the allocation generation must increase", ErrHostedMigrationMismatch)
	}
	now := formatHubTime(database.now())
	migration, err := tx.ExecContext(ctx, `INSERT INTO hosted_binding_migrations(organization_id,from_deployment,from_public_url,from_generation,to_public_url,to_generation,migrated_at) VALUES (?,?,?,?,?,?,?)`,
		result.OrganizationID, result.FromDeployment, result.FromPublicURL, result.FromGeneration, result.PublicURL, result.Generation, now)
	if err != nil {
		return HostedSharedMigration{}, err
	}
	id, err := migration.LastInsertId()
	if err != nil {
		return HostedSharedMigration{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_tenant SET public_url = ?, deployment = 'shared', allocation_generation = ? WHERE singleton = 1", result.PublicURL, result.Generation); err != nil {
		return HostedSharedMigration{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_binding_migrations SET applied = 1 WHERE id = ?", id); err != nil {
		return HostedSharedMigration{}, err
	}
	for _, step := range []struct {
		query string
		count *int64
	}{
		{"UPDATE hosted_sessions SET revoked_at = ? WHERE revoked_at IS NULL", &result.SessionsRevoked},
		{"UPDATE hosted_transactions SET consumed_at = ? WHERE consumed_at IS NULL", &result.TransactionsClosed},
	} {
		changed, err := tx.ExecContext(ctx, step.query, now)
		if err != nil {
			return HostedSharedMigration{}, err
		}
		if *step.count, err = changed.RowsAffected(); err != nil {
			return HostedSharedMigration{}, err
		}
	}
	expired, err := tx.ExecContext(ctx, "UPDATE artifact_grants SET expires_at = 0 WHERE expires_at > 0")
	if err != nil {
		return HostedSharedMigration{}, err
	}
	if result.GrantsExpired, err = expired.RowsAffected(); err != nil {
		return HostedSharedMigration{}, err
	}
	return result, tx.Commit()
}
