package conversations

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/artifact/cataloghub"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s ArtifactCatalogHTTP) catalogPublisherScope(ctx context.Context, r postgres.ArtifactCatalogReader, command artifact.CatalogCommand, credential hubapi.APICredential, write bool) (hubscope.NativeScope, artifact.Binding, error) {
	org := tracker.OrganizationID(command.OrganizationID)
	if s.deps.Hosted && org != s.deps.Organization() {
		return hubscope.NativeScope{}, artifact.Binding{}, artifact.ErrDenied
	}
	project := tracker.ProjectID(command.ProjectID)
	if project == "" {
		binding, err := r.CatalogServiceBinding(ctx, postgres.CatalogServiceBindingInput{OrganizationID: org, ID: command.ServiceID, PublisherTokenID: credential.ID})
		if err != nil {
			return hubscope.NativeScope{}, artifact.Binding{}, artifact.ErrDenied
		}
		project = binding.ProjectID
	}
	tenant, ok := r.(tenantstore.Reader)
	if !ok {
		return hubscope.NativeScope{}, artifact.Binding{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	scope := hubscope.NativeScope{Organization: org, Project: project, Credential: credential}
	if err := s.deps.Authorize(ctx, tenant, scope, write); err != nil {
		return scope, artifact.Binding{}, artifact.ErrDenied
	}
	current, err := tenant.AccessCredential(ctx, credential.ID)
	if err != nil || !current.Valid(credential.Hash, s.deps.Now()) || current.Scope != string(hubapi.APIScopeWorker) || !current.NativeOnly {
		return scope, artifact.Binding{}, artifact.ErrDenied
	}
	if _, err := tenant.AccessTokenGrant(ctx, current.ID, project, credential.HostedRole); err != nil {
		return scope, artifact.Binding{}, artifact.ErrDenied
	}
	binding, err := tenant.ArtifactService(ctx, project, command.ServiceID)
	if err != nil || binding.PublisherTokenID != current.ID {
		return scope, artifact.Binding{}, artifact.ErrDenied
	}
	return scope, binding, nil
}

func (s ArtifactCatalogHTTP) ArtifactCatalog(c echo.Context) error {
	if !s.deps.StorageAvailable {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	credential, _, err := s.deps.Authenticate(c)
	if err != nil || credential.Scope != hubapi.APIScopeWorker || !credential.NativeOnly || credential.Hosted != nil || credential.Runner.RunnerID != "" || c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		return c.JSON(http.StatusForbidden, struct {
			Code string `json:"code"`
		}{Code: artifact.ErrDenied.Error()})
	}
	policy := func(ctx context.Context, r postgres.ArtifactCatalogReader, requested artifact.CatalogScope) (artifact.Config, error) {
		command := artifact.CatalogCommand{CatalogScope: requested}
		scope, binding, err := s.catalogPublisherScope(ctx, r, command, credential, false)
		if err != nil {
			return artifact.Config{}, err
		}
		row, err := r.CatalogPolicy(ctx, postgres.CatalogPolicyInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: binding.ServiceID})
		if err != nil {
			return artifact.Config{}, err
		}
		p := artifact.Policy{ID: row.CatalogPolicyID, Limits: artifact.Limits{RetainedBytes: row.CatalogRetainedLimit, ReservedBytes: row.CatalogReservedLimit, ArtifactBytes: row.CatalogArtifactLimit, UploadBytes: row.CatalogUploadLimit, RetentionSeconds: row.CatalogRetentionSeconds}, AbandonedUploadSeconds: row.CatalogAbandonedSeconds, DeletionRecordSeconds: row.CatalogDeletionSeconds, BackupSeconds: row.CatalogBackupSeconds}
		orgRetained, orgReserved := int64(1<<60), int64(1<<60)
		if binding.Mode == "hosted" {
			plans := s.deps.Plans().Plans()
			billing, ok := r.(billingstore.Reader)
			if !ok || plans == nil || plans.WindowSeconds <= 0 {
				return artifact.Config{}, artifact.ErrQuota
			}
			entitlement, err := hubapi.FeatureResult(s.deps.Plans().EntitlementRecord(ctx, billing, s.deps.Now()))
			if err != nil {
				return artifact.Config{}, err
			}
			if !slices.Contains(entitlement.Features, "hosted_artifacts") {
				return artifact.Config{}, artifact.ErrQuota
			}
			orgRetained, orgReserved = entitlement.Allowances["artifact_retained_bytes"], entitlement.Allowances["artifact_reserved_bytes"]
			p.Limits.RetainedBytes = min(p.Limits.RetainedBytes, entitlement.Allowances["artifact_retained_bytes"])
			p.Limits.ReservedBytes = min(p.Limits.ReservedBytes, entitlement.Allowances["artifact_reserved_bytes"])
			p.Limits.ArtifactBytes = min(p.Limits.ArtifactBytes, entitlement.Allowances["artifact_bytes"])
			p.Limits.UploadBytes = min(p.Limits.UploadBytes, entitlement.Allowances["artifact_upload_bytes"])
			p.Limits.RetentionSeconds = min(p.Limits.RetentionSeconds, entitlement.Allowances["artifact_retention_seconds"])
			p.Limits.RelayBytes = entitlement.Allowances["relay_bytes"]
			p.Limits.WindowSeconds = plans.WindowSeconds
			p.Limits.TelemetrySeconds = plans.WindowSeconds * plans.RetentionWindows
		}
		cfg := artifact.Config{ServiceID: binding.ServiceID, OrganizationID: string(scope.Organization), ProjectID: requested.ProjectID, PublisherID: credential.ID, Mode: binding.Mode, HostedOptIn: binding.HostedOptIn, Policy: p}
		cfg.OrganizationRetainedLimit = orgRetained
		cfg.OrganizationReservedLimit = orgReserved
		if err := cfg.Validate(); err != nil {
			return artifact.Config{}, artifact.ErrQuota
		}
		return cfg, nil
	}
	authorize := func(ctx context.Context, r postgres.ArtifactCatalogReader, command artifact.CatalogCommand, write bool) error {
		scope, binding, err := s.catalogPublisherScope(ctx, r, command, credential, write)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		if command.Operation == "traffic" && binding.Mode != "hosted" {
			return artifact.ErrDenied
		}
		writer, ok := r.(postgres.ArtifactCatalogWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var reservation artifact.Reservation
		switch command.Operation {
		case "reserve":
			reservation = command.Upload.Reservation
		case "put_object", "begin_object_write", "prepare_manifest", "begin_manifest_write", "commit":
			row, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ServiceID: command.ServiceID, ID: command.UploadID})
			if err != nil {
				return artifact.ErrDenied
			}
			if command.Operation == "commit" {
				_, err := r.CatalogManifest(ctx, postgres.CatalogManifestInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ServiceID: command.ServiceID, UploadID: command.UploadID, Revision: command.Manifest.Revision})
				if err == nil {
					return nil
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if command.Manifest.State == "interrupted" {
					return nil
				}
			}
			if command.Operation == "prepare_manifest" && command.Intent.ManifestState == "interrupted" {
				return nil
			}
			if command.Operation == "begin_manifest_write" {
				intent, err := r.CatalogManifestIntent(ctx, postgres.CatalogManifestIntentInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ServiceID: command.ServiceID, UploadID: command.UploadID, ID: command.Intent.ID})
				if err != nil {
					return artifact.ErrDenied
				}
				if intent.ManifestState == "interrupted" {
					return nil
				}
			}
			reservation = artifact.Reservation{Scope: artifact.Scope{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), WorkItemID: row.WorkItemID, RunID: row.RunID, AttemptID: row.AttemptID, VersionID: row.VersionID}, ServiceID: row.ServiceID, Key: row.IdempotencyKey, Kind: row.Kind, Bytes: row.RequestedBytes, LeaseID: row.LeaseID, FencingToken: row.FencingToken}
		default:
			return nil
		}
		err = writer.CatalogExecutionAuthority(ctx, postgres.CatalogExecutionAuthorityInput{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(reservation.WorkItemID), AttemptID: reservation.AttemptID, RunID: reservation.RunID, LeaseID: reservation.LeaseID, FencingToken: reservation.FencingToken, ServiceID: command.ServiceID, IdempotencyKey: reservation.Key, Kind: reservation.Kind, Bytes: reservation.Bytes, Now: s.deps.Now()})
		if err != nil {
			return artifact.ErrDenied
		}
		return nil
	}
	owner, err := s.deps.NewCatalog(policy, cataloghub.Options{Authorize: authorize, Now: s.deps.Now})
	if err != nil {
		return s.deps.APIError(c, err)
	}
	handler, err := artifact.NewCatalogHandler(owner, func(context.Context, string, artifact.CatalogCommand) error { return nil })
	if err != nil {
		return s.deps.APIError(c, err)
	}
	handler.ServeHTTP(c.Response(), c.Request())
	return nil
}

func (s ArtifactCatalogHTTP) SetArtifactCatalogPolicy(c echo.Context) error {
	if !s.deps.StorageAvailable {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	var policy artifact.Policy
	if err := hubapi.DecodeAPIJSON(c, &policy); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if err := policy.Validate(); err != nil {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid artifact catalog policy"))
	}
	err := s.deps.WriteCatalog(c.Request().Context(), scope.Organization, func(w postgres.ArtifactCatalogWriter) error {
		tenant, ok := w.(tenantstore.Reader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := s.deps.Authorize(c.Request().Context(), tenant, scope, true); err != nil {
			return err
		}
		if scope.Credential.Hosted == nil {
			current, err := tenant.AccessCredential(c.Request().Context(), scope.Credential.ID)
			if err != nil || current.Scope != string(hubapi.APIScopeAdmin) {
				return artifact.ErrDenied
			}
		}
		_, err := w.SetCatalogPolicy(c.Request().Context(), scope.Project, c.Param("service"), policy)
		return err
	})
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
