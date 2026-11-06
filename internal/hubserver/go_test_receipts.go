package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
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

func (s *Service) attestGoTestReceipts(c echo.Context) error {
	scope := nativeRequestScope(c)
	id := newNativeID("gtra")
	ctx := c.Request().Context()
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
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
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
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

func (s *Service) expireGoTestReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	if s.config.TenantRepository == nil {
		return nativeRunEventStorageUnavailable()
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
