package validation

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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const (
	ReceiptRetention      = 30 * 24 * time.Hour
	receiptPruneBudget    = storageconversion.BackgroundBudget
	ReceiptAttestationTTL = 12 * time.Hour
	receiptKeyPurpose     = "detent go test receipt digest v1"
)

func (s *Service) LookupReceipts(c echo.Context) error {
	key := c.QueryParam("key")
	if !gotestreceipt.ValidKey(key) {
		return s.deps.Error(c, hubapi.Invalid("key must be a lowercase sha256 hex digest"))
	}
	scope := s.deps.Scope(c)
	if s.deps.Repository == nil {
		return s.deps.Error(c, hubapi.RunEventStorageUnavailable())
	}
	var candidates []gotestreceipt.Candidate
	err := s.deps.Repository.ReadTenant(c.Request().Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		candidates, err = r.GoTestReceipts(c.Request().Context(), scope.Project, key, s.deps.Now().Add(-ReceiptRetention), gotestreceipt.MaxCandidates)
		return err
	})
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"candidates": candidates})
}

func (s *Service) AttestReceipts(c echo.Context) error {
	scope := s.deps.Scope(c)
	id := hubapi.NewID("gtra")
	ctx := c.Request().Context()
	if s.deps.Repository == nil {
		return s.deps.Error(c, hubapi.RunEventStorageUnavailable())
	}
	now := s.deps.Now()
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		runner, err := s.requireGoTestReceiptTenantAuthority(ctx, w, scope, now)
		if err != nil {
			return err
		}
		return w.CreateGoTestReceiptAttestation(ctx, gotestreceiptstore.Attestation{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: id, CredentialID: scope.Credential.ID, RunnerID: runner, CreatedAt: now, ExpiresAt: now.Add(ReceiptAttestationTTL)})
	})
	if err != nil {
		return s.deps.Error(c, err)
	}
	key, err := s.deps.SecretKeys.Derive(receiptKeyPurpose+"\x00"+string(scope.Organization)+"\x00"+string(scope.Project), gotestreceipt.DigestKeyBytes)
	if err != nil {
		return s.deps.Error(c, fmt.Errorf("derive go test receipt digest key: %w", err))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, gotestreceipt.Attestation{ID: id, DigestKey: key})
}

func (s *Service) RecordReceipt(c echo.Context) error {
	var receipt gotestreceipt.Receipt
	if err := hubapi.DecodeJSON(c, &receipt); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if err := receipt.Validate(); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	scope := s.deps.Scope(c)
	ctx := c.Request().Context()
	if s.deps.Repository == nil {
		return s.deps.Error(c, hubapi.RunEventStorageUnavailable())
	}
	now := s.deps.Now()
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		runner, err := s.requireGoTestReceiptTenantAuthority(ctx, w, scope, now)
		if err != nil {
			return err
		}
		attestation, err := w.GoTestReceiptAttestation(ctx, scope.Project, receipt.Attestation, scope.Credential.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.NotFound()
		}
		if err != nil {
			return fmt.Errorf("read go test receipt attestation: %w", err)
		}
		if attestation.RunnerID != runner {
			return hubapi.NotFound()
		}
		if !attestation.ExpiresAt.After(now) {
			return staleExecution("The go test receipt attestation has expired")
		}
		return w.PutGoTestReceipt(ctx, gotestreceiptstore.Receipt{OrganizationID: scope.Organization, ProjectID: scope.Project, BinaryKey: receipt.Key, InputsDigest: receipt.Digest, Inputs: receipt.Inputs, Package: receipt.Package, RunnerID: runner, RecordedAt: now, AttestationID: receipt.Attestation}, gotestreceipt.MaxCandidates)
	})
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) requireGoTestReceiptTenantAuthority(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input, now time.Time) (string, error) {
	if scope.Credential.Scope != hubauth.Worker || scope.Credential.Runner.RunnerID == "" || !scope.Credential.Runner.Valid() {
		return "", hubapi.NotFound()
	}
	identity, err := reader.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return "", hubapi.NotFound()
	}
	if err != nil {
		return "", err
	}
	if identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.MachineID != scope.Credential.Runner.MachineID || identity.Scope != scope.Credential.Runner.Scope || !slices.Equal(identity.Operations, scope.Credential.Runner.Operations) || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Events) {
		return "", hubapi.NotFound()
	}
	credential, err := reader.AccessCredential(ctx, identity.TokenID)
	if err != nil {
		return "", err
	}
	if credential.OrganizationID != scope.Organization || credential.ID != scope.Credential.ID || credential.Scope != string(hubauth.Worker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) {
		return "", hubapi.NotFound()
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", hubapi.NotFound()
	}
	return identity.RunnerID, nil
}

func (s *Service) ExpireReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	if s.deps.Repository == nil {
		return hubapi.RunEventStorageUnavailable()
	}
	if s.deps.Organization == nil || s.deps.Organization() == "" {
		return hubapi.RunEventStorageUnavailable()
	}
	organization := s.deps.Organization()
	cutoff := now.Add(-ReceiptRetention)
	deadline := clock().Add(receiptPruneBudget)
	return s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
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
