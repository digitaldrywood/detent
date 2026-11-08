package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerArtifactRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeOperator, apiScopeWorker)
	e.PUT(nativeBase+"/artifact-services/:service", s.bindArtifactService, s.requireNativeScope(apiScopeAdmin))
	e.GET(nativeBase+"/artifact-services", s.artifactServices, read)
	e.POST(nativeBase+"/artifact-services/:service/receipts", s.artifactReceipt, read)
	e.POST(nativeBase+"/artifact-services/:service/authorize", s.authorizeArtifactRead, read)
	e.POST(nativeBase+"/work-items/:item/artifact-authority", s.authorizeArtifactUpload, s.requireNativeScope(apiScopeWorker))
	e.GET(nativeBase+"/work-items/:item/artifacts", s.artifactReferences, read)
	e.GET(nativeBase+"/work-items/:item/artifacts/:artifact/revisions/:revision", s.getArtifactReference, read)
	e.POST(nativeBase+"/work-items/:item/artifacts/:artifact/access", s.artifactReadGrant, read)
}

func artifactReadGrantRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == nativeBase+"/work-items/:item/artifacts/:artifact/access"
}

func (s *Service) hostedArtifactPublisher(c echo.Context, credential apiCredential) bool {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if !credential.NativeOnly || credential.Scope != apiScopeWorker || c.Request().Method != http.MethodPost {
		return false
	}
	if c.Path() != nativeBase+"/artifact-services/:service/receipts" && c.Path() != nativeBase+"/artifact-services/:service/authorize" {
		return false
	}
	id, err := s.artifactPublisherToken(ctx, nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}, c.Param("service"))
	return err == nil && id == credential.ID
}

type bindArtifactServiceRequest struct {
	artifact.Binding
	tracker.Mutation
}

func (s *Service) bindArtifactService(c echo.Context) error {
	var request bindArtifactServiceRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.ServiceID != c.Param("service") {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid artifact service binding"))
	}
	// Existing dashboard callers may omit a key; typed clients supply one to
	// replay the same binding command across reconnects.
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = newNativeID("command")
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	options := nativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath()}
	result, err := s.executeProjectRecordMutation(c.Request().Context(), nativeRequestScope(c), options, request.Mutation, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) artifactServices(c echo.Context) error {
	result, err := s.artifactServicesCommand(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) artifactPublisher(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	id, err := s.artifactPublisherToken(ctx, scope, c.Param("service"))
	if err != nil || id != scope.credential.ID {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) artifactPublisherToken(ctx context.Context, scope nativeScope, service string) (id string, err error) {
	if s.config.TenantRepository == nil {
		return "", workItemStorageUnavailable()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		binding, err := r.ArtifactService(ctx, scope.project, service)
		id = binding.PublisherTokenID
		return err
	})
	return id, err
}

func (s *Service) artifactReceipt(c echo.Context) error {
	if err := s.artifactPublisher(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var ref artifact.Reference
	if err := decodeAPIJSON(c, &ref); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if ref.SchemaVersion != 1 || ref.Validate() != nil || ref.OrganizationID != string(scope.organization) || ref.ProjectID != string(scope.project) || ref.ServiceID != c.Param("service") || !artifact.ValidID(ref.ArtifactID, "artifact") || !artifact.ValidID(ref.ManifestID, "manifest") || !artifact.ValidHash(ref.SHA256, 64) || ref.Revision < 1 || ref.Revision > artifact.MaxObjects+1 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, ref.Kind) || !slices.Contains([]string{"partial", "complete", "interrupted"}, ref.State) || ref.Availability != "available" || ref.Bytes < 0 || ref.Bytes > artifact.MaxArtifactBytes || ref.Objects < 0 || ref.Objects > artifact.MaxObjects || ref.ExpiresAt.IsZero() || ref.ObservedAt.IsZero() {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid artifact receipt"))
	}
	if err := s.publishArtifactReceiptTenant(c.Request().Context(), scope, ref); err != nil {
		return s.nativeAPIError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *Service) artifactReferences(c echo.Context) error {
	refs, err := s.readArtifactReferences(c.Request().Context(), nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, refs)
}

func (s *Service) readArtifactReferences(ctx context.Context, scope nativeScope, item string) ([]artifact.Reference, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
		return nil, nativeNotFound()
	}
	var refs []artifact.Reference
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		refs, err = r.ArtifactReferences(ctx, scope.project, tracker.NativeWorkItemID(item), 64)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	for i := range refs {
		if !s.config.now().Before(refs[i].ExpiresAt) {
			refs[i].Availability = "expired"
		}
	}
	return refs, nil
}

func (s *Service) authorizeArtifactUpload(c echo.Context) error {
	var r artifact.Reservation
	if err := decodeAPIJSON(c, &r); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if r.Validate() != nil || r.OrganizationID != string(scope.organization) || r.ProjectID != string(scope.project) || r.WorkItemID != c.Param("item") || r.LeaseID == "" || r.FencingToken <= 0 || r.Key == "" || len(r.Key) > 128 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, r.Kind) || r.Bytes <= 0 || r.Bytes > artifact.MaxArtifactBytes || scope.credential.Scope != apiScopeWorker {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := s.authorizeArtifactUploadTenant(c.Request().Context(), scope, r); err != nil {
		if errors.Is(err, artifact.ErrQuota) {
			return s.refusedAPIError(c, http.StatusTooManyRequests, "quota_exceeded", "Too many pending artifact uploads; retry after they publish or expire", apiErrorResponse{Code: "quota_exceeded", Message: "Too many pending artifact uploads; retry after they publish or expire"})
		}
		return s.nativeAPIError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *Service) artifactReadGrant(c echo.Context) error {
	var input struct {
		Revision int64 `json:"revision"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.artifactReadGrantCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("artifact"), input.Revision, s.config.now())
	if errors.Is(err, artifact.ErrExpired) {
		return s.refusedAPIError(c, http.StatusGone, "expired", "Artifact retention has expired", apiErrorResponse{Code: "expired", Message: "Artifact retention has expired"})
	}
	if errors.Is(err, artifact.ErrQuota) {
		return s.refusedAPIError(c, http.StatusTooManyRequests, "quota_exceeded", "Too many active artifact access grants; retry after they expire", apiErrorResponse{Code: "quota_exceeded", Message: "Too many active artifact access grants; retry after they expire"})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Service) authorizeArtifactRead(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if err := s.artifactPublisher(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var r artifact.ReadAuthorization
	if err := decodeAPIJSON(c, &r); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if r.Token == "" || len(r.Token) > 4096 || !artifact.ValidID(r.ArtifactID, "artifact") || r.Revision < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeRequestScope(c)
	grant, owner, err := s.readArtifactGrantTenant(ctx, scope, c.Param("service"), r)
	if err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.Hosted != nil {
		reader, err := s.hostedArtifactReader(c, grant.PrincipalID, grant.SessionHash)
		if err != nil || !(nativeScope{organization: scope.organization, project: scope.project, credential: reader}).reporterOwns(owner) {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) hostedArtifactReader(c echo.Context, principal, sessionHash string) (apiCredential, error) {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if sessionHash == "" {
		scope := nativeRequestScope(c)
		var stored accessstore.Credential
		err := s.config.TenantRepository.ReadTenant(readCtx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			stored, err = r.AccessCredential(readCtx, principal)
			if err != nil {
				return err
			}
			member, err := r.AccessCredentialHasMember(readCtx, principal)
			if err != nil {
				return err
			}
			if member {
				return auth.ErrHostedIdentity
			}
			return nil
		})
		if err != nil {
			return apiCredential{}, err
		}
		credential, _, err := s.authenticateTenantAPIHash(readCtx, scope.organization, stored.Hash, "", "")
		if err != nil || credential.ID != principal {
			return apiCredential{}, auth.ErrHostedIdentity
		}
		return credential, nil
	}
	session, err := s.WebSession(ctx, sessionHash, s.config.now())
	if err != nil {
		return apiCredential{}, err
	}
	if s.config.Hosted.SharedEntry == nil && hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) && (session.Identity == nil || session.Identity.SupportActor == "") {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	credential, _, err := s.hostedSessionCredential(ctx, session, sessionHash)
	if err != nil || credential.ID != principal {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	scope := nativeRequestScope(c)
	scope.credential = credential
	if s.config.TenantRepository == nil {
		return apiCredential{}, workItemStorageUnavailable()
	}
	if err := s.config.TenantRepository.ReadTenant(readCtx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(readCtx, r, scope, false)
	}); err != nil {
		return apiCredential{}, err
	}
	return credential, s.hostedAudit(ctx, session.Identity, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0)
}

func (s *Service) artifactServicesCommand(ctx context.Context, scope nativeScope) (result []artifact.Binding, err error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		result, err = r.ArtifactServices(ctx, scope.project)
		return err
	})
	for i := range result {
		result[i].PublisherTokenID = ""
	}
	return result, projectRecordError(err)
}

func (s *Service) artifactReadGrantCommand(ctx context.Context, scope nativeScope, item, artifactID string, revision int64, now time.Time) (artifact.Grant, error) {
	if s.config.TenantRepository == nil {
		return artifact.Grant{}, workItemStorageUnavailable()
	}
	return s.artifactReadGrantTenant(ctx, scope, item, artifactID, revision, now)
}

func (s *Service) recheckHostedArtifactReader(ctx context.Context, _ *sql.Tx, scope nativeScope) error {
	if scope.credential.Hosted == nil {
		return nil
	}
	if s.config.TenantRepository == nil {
		return workItemStorageUnavailable()
	}
	if _, _, err := s.hostedMutationIdentity(ctx, scope.credential); err != nil {
		return err
	}
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(ctx, r, scope, false)
	})
}

func (s *Service) getArtifactReference(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	revision, err := artifact.Revision(c.Param("revision"))
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid artifact revision"))
	}
	result, err := s.artifactReferenceCommand(ctx, nativeRequestScope(c), c.Param("item"), c.Param("artifact"), revision, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
func (s *Service) artifactReferenceCommand(ctx context.Context, scope nativeScope, item, id string, revision int64, now time.Time) (artifact.Reference, error) {
	var result artifact.Reference
	if s.config.TenantRepository == nil {
		return result, workItemStorageUnavailable()
	}
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return result, err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
		return result, nativeNotFound()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result, err = r.ArtifactReference(ctx, scope.project, tracker.NativeWorkItemID(item), id, revision)
		return err
	})
	if err != nil {
		return artifact.Reference{}, projectRecordError(err)
	}
	if !now.Before(result.ExpiresAt) {
		result.Availability = "expired"
	}
	return result, nil
}
