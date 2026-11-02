package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/gotestreceiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	goTestReceiptRetention      = 30 * 24 * time.Hour
	goTestReceiptPruneBudget    = backgroundStorageBudget
	goTestReceiptAttestationTTL = 12 * time.Hour
	goTestReceiptKeyPurpose     = "detent go test receipt digest v1"
)

func selectedGoTestReceiptRoute(path string) bool {
	return path == nativeBase+"/go-test-receipts" || path == nativeBase+"/go-test-receipts/attestations"
}

func (s *Service) lookupGoTestReceipts(c echo.Context) error {
	key := c.QueryParam("key")
	if !gotestreceipt.ValidKey(key) {
		return s.nativeAPIError(c, nativeInvalid("key must be a lowercase sha256 hex digest"))
	}
	scope := nativeRequestScope(c)
	if s.config.TenantRepository != nil {
		var candidates []gotestreceipt.Candidate
		err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			candidates, err = r.GoTestReceipts(c.Request().Context(), scope.project, key, s.config.now().Add(-goTestReceiptRetention), gotestreceipt.MaxCandidates)
			return err
		})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, map[string]any{"candidates": candidates})
	}
	candidates, err := readGoTestReceipts(c.Request().Context(), s.database.reader, string(scope.organization), string(scope.project), key, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"candidates": candidates})
}

func (s *Service) attestGoTestReceipts(c echo.Context) error {
	scope := nativeRequestScope(c)
	id := newNativeID("gtra")
	ctx := c.Request().Context()
	if s.config.TenantRepository != nil {
		now := s.config.now()
		err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			runner, err := s.requireGoTestReceiptTenantAuthority(ctx, w, scope, now)
			if err != nil {
				return err
			}
			return w.CreateGoTestReceiptAttestation(ctx, gotestreceiptstore.Attestation{OrganizationID: scope.organization, ProjectID: scope.project, ID: id, CredentialID: scope.credential.ID, RunnerID: runner, CreatedAt: now, ExpiresAt: now.Add(goTestReceiptAttestationTTL)})
		})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		key, err := s.config.SecretKeys.Derive(goTestReceiptKeyPurpose+"\x00"+string(scope.organization)+"\x00"+string(scope.project), gotestreceipt.DigestKeyBytes)
		if err != nil {
			return s.nativeAPIError(c, fmt.Errorf("derive go test receipt digest key: %w", err))
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusCreated, gotestreceipt.Attestation{ID: id, DigestKey: key})
	}
	err := s.commitHubTransaction(ctx, func(tx *sql.Tx, now time.Time) error {
		runner, err := s.requireGoTestReceiptAuthority(ctx, tx, &scope, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO go_test_receipt_attestations (id, organization_id, project_id, credential_id, runner_id, created_at_ns, expires_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, scope.organization, scope.project, scope.credential.ID, runner, now.UnixNano(), now.Add(goTestReceiptAttestationTTL).UnixNano())
		if err != nil {
			return fmt.Errorf("create go test receipt attestation: %w", err)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	key, err := s.config.SecretKeys.Derive(goTestReceiptKeyPurpose+"\x00"+string(scope.organization)+"\x00"+string(scope.project), gotestreceipt.DigestKeyBytes)
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("derive go test receipt digest key: %w", err))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, gotestreceipt.Attestation{ID: id, DigestKey: key})
}

func (s *Service) recordGoTestReceipt(c echo.Context) error {
	var receipt gotestreceipt.Receipt
	if err := decodeAPIJSON(c, &receipt); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := receipt.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	if s.config.TenantRepository != nil {
		now := s.config.now()
		err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			runner, err := s.requireGoTestReceiptTenantAuthority(ctx, w, scope, now)
			if err != nil {
				return err
			}
			attestation, err := w.GoTestReceiptAttestation(ctx, scope.project, receipt.Attestation, scope.credential.ID)
			if errors.Is(err, corestore.ErrNotFound) {
				return nativeNotFound()
			}
			if err != nil {
				return fmt.Errorf("read go test receipt attestation: %w", err)
			}
			if attestation.RunnerID != runner {
				return nativeNotFound()
			}
			if !attestation.ExpiresAt.After(now) {
				return nativeStaleExecution("The go test receipt attestation has expired")
			}
			return w.PutGoTestReceipt(ctx, gotestreceiptstore.Receipt{OrganizationID: scope.organization, ProjectID: scope.project, BinaryKey: receipt.Key, InputsDigest: receipt.Digest, Inputs: receipt.Inputs, Package: receipt.Package, RunnerID: runner, RecordedAt: now, AttestationID: receipt.Attestation}, gotestreceipt.MaxCandidates)
		})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.NoContent(http.StatusNoContent)
	}
	err := s.commitHubTransaction(ctx, func(tx *sql.Tx, now time.Time) error {
		var expires int64
		err := tx.QueryRowContext(ctx, `SELECT expires_at_ns FROM go_test_receipt_attestations
WHERE id = ? AND organization_id = ? AND project_id = ? AND credential_id = ?`, receipt.Attestation, scope.organization, scope.project, scope.credential.ID).Scan(&expires)
		if errors.Is(err, sql.ErrNoRows) {
			return nativeNotFound()
		}
		if err != nil {
			return fmt.Errorf("read go test receipt attestation: %w", err)
		}
		if expires <= now.UnixNano() {
			return nativeStaleExecution("The go test receipt attestation has expired")
		}
		runner, err := s.requireGoTestReceiptAuthority(ctx, tx, &scope, now)
		if err != nil {
			return err
		}
		return storeGoTestReceipt(ctx, tx, string(scope.organization), string(scope.project), runner, receipt, now)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) requireGoTestReceiptTenantAuthority(ctx context.Context, reader tenantstore.Reader, scope nativeScope, now time.Time) (string, error) {
	if scope.credential.Scope != apiScopeWorker || scope.credential.Runner.RunnerID == "" || !scope.credential.Runner.Valid() {
		return "", nativeNotFound()
	}
	identity, err := reader.RunnerIdentityByCredential(ctx, scope.credential.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return "", nativeNotFound()
	}
	if err != nil {
		return "", err
	}
	if identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.MachineID != scope.credential.Runner.MachineID || identity.Scope != scope.credential.Runner.Scope || !slices.Equal(identity.Operations, scope.credential.Runner.Operations) || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Events) {
		return "", nativeNotFound()
	}
	credential, err := reader.AccessCredential(ctx, identity.TokenID)
	if err != nil {
		return "", err
	}
	if credential.OrganizationID != scope.organization || credential.ID != scope.credential.ID || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) {
		return "", nativeNotFound()
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", nativeNotFound()
	}
	return identity.RunnerID, nil
}

func (s *Service) requireGoTestReceiptAuthority(ctx context.Context, tx *sql.Tx, scope *nativeScope, now time.Time) (string, error) {
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return "", err
	}
	if runner := scope.credential.Runner.RunnerID; runner != "" {
		if err := requireRunnerAuthority(ctx, tx, *scope, now); err != nil {
			return "", err
		}
		return runner, nil
	}
	if scope.credential.Scope != apiScopeOperator {
		return "", nativeNotFound()
	}
	return scope.credential.ID, nil
}

func readGoTestReceipts(ctx context.Context, query nativeQueryer, organization, project, key string, now time.Time) ([]gotestreceipt.Candidate, error) {
	rows, err := query.QueryContext(ctx, goTestReceiptLookupQuery, organization, project, key, now.Add(-goTestReceiptRetention).UnixNano(), gotestreceipt.MaxCandidates)
	if err != nil {
		return nil, fmt.Errorf("read go test receipts: %w", err)
	}
	defer rows.Close()
	candidates := []gotestreceipt.Candidate{}
	for rows.Next() {
		var inputs, digest string
		if err := rows.Scan(&inputs, &digest); err != nil {
			return nil, fmt.Errorf("scan go test receipt: %w", err)
		}
		candidate := gotestreceipt.Candidate{Inputs: []string{}, Digest: digest}
		if inputs != "" {
			candidate.Inputs = strings.Split(inputs, "\n")
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read go test receipts: %w", err)
	}
	return candidates, nil
}

func storeGoTestReceipt(ctx context.Context, exec hostedExecer, organization, project, reporter string, receipt gotestreceipt.Receipt, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO go_test_receipts (organization_id, project_id, binary_key, inputs_digest, inputs, package, runner_id, recorded_at_ns, attestation_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(organization_id, project_id, binary_key, inputs_digest) DO UPDATE SET package=excluded.package, runner_id=excluded.runner_id, recorded_at_ns=excluded.recorded_at_ns, attestation_id=excluded.attestation_id`,
		organization, project, receipt.Key, receipt.Digest, strings.Join(receipt.Inputs, "\n"), receipt.Package, reporter, now.UnixNano(), receipt.Attestation)
	if err != nil {
		return fmt.Errorf("store go test receipt: %w", err)
	}
	if _, err := exec.ExecContext(ctx, goTestReceiptEvictQuery, organization, project, receipt.Key, gotestreceipt.MaxCandidates); err != nil {
		return fmt.Errorf("evict go test receipt: %w", err)
	}
	return nil
}

const (
	goTestReceiptLookupQuery = `SELECT inputs, inputs_digest FROM go_test_receipts
WHERE organization_id=? AND project_id=? AND binary_key=? AND attestation_id <> '' AND recorded_at_ns>=? ORDER BY recorded_at_ns DESC LIMIT ?`
	goTestReceiptEvictQuery = `DELETE FROM go_test_receipts WHERE rowid IN (SELECT rowid FROM go_test_receipts INDEXED BY go_test_receipts_lookup
WHERE organization_id=? AND project_id=? AND binary_key=? ORDER BY recorded_at_ns DESC LIMIT 1 OFFSET ?)`
	goTestReceiptPruneQuery            = "DELETE FROM go_test_receipts WHERE rowid IN (SELECT rowid FROM go_test_receipts INDEXED BY go_test_receipts_recorded WHERE recorded_at_ns < ? ORDER BY recorded_at_ns LIMIT 1)"
	goTestReceiptAttestationPruneQuery = "DELETE FROM go_test_receipt_attestations WHERE rowid IN (SELECT rowid FROM go_test_receipt_attestations INDEXED BY go_test_receipt_attestations_expiry WHERE expires_at_ns < ? ORDER BY expires_at_ns LIMIT 1)"
)

func (d *database) expireGoTestReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	return expireGoTestReceipts(ctx, d.db, now, clock)
}

func (s *Service) expireGoTestReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	if s.config.TenantRepository == nil {
		return s.database.expireGoTestReceipts(ctx, now, clock)
	}
	if s.config.Hosted == nil || s.config.Hosted.OrganizationID == "" {
		return nativeRunEventStorageUnavailable()
	}
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	cutoff := now.Add(-goTestReceiptRetention)
	deadline := clock().Add(goTestReceiptPruneBudget)
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		remaining := deadline.Sub(clock())
		if remaining <= 0 {
			return nil
		}
		statementCtx, cancel := context.WithTimeout(ctx, remaining)
		defer cancel()
		_, _, err := w.PruneGoTestReceipts(statementCtx, cutoff, now, 64)
		return err
	})
}

func expireGoTestReceipts(ctx context.Context, exec hostedExecer, now time.Time, clock func() time.Time) error {
	deadline := clock().Add(goTestReceiptPruneBudget)
	prunes := []struct {
		step   string
		query  string
		cutoff int64
	}{
		{"expire go test receipts", goTestReceiptPruneQuery, now.Add(-goTestReceiptRetention).UnixNano()},
		{"expire go test receipt attestations", goTestReceiptAttestationPruneQuery, now.UnixNano()},
	}
	for next := 0; len(prunes) > 0; next = (next + 1) % len(prunes) {
		remaining := deadline.Sub(clock())
		if remaining <= 0 {
			return nil
		}
		prune := prunes[next]
		statementCtx, cancel := context.WithTimeout(ctx, remaining)
		result, err := exec.ExecContext(statementCtx, prune.query, prune.cutoff)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", prune.step, err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count %s: %w", prune.step, err)
		}
		if deleted == 0 {
			prunes = slices.Delete(prunes, next, next+1)
			next--
			if len(prunes) == 0 {
				return nil
			}
		}
	}
	return nil
}
