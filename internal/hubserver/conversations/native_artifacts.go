package conversations

import (
	"errors"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ArtifactReadGrantRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == hubapi.NativeBase+"/work-items/:item/artifacts/:artifact/access"
}

func (s ArtifactHTTP) HostedArtifactPublisher(c echo.Context, credential hubapi.APICredential) bool {
	ctx := c.Request().Context()
	if !credential.NativeOnly || credential.Scope != hubapi.APIScopeWorker || c.Request().Method != http.MethodPost {
		return false
	}
	if c.Path() != hubapi.NativeBase+"/artifact-services/:service/receipts" && c.Path() != hubapi.NativeBase+"/artifact-services/:service/authorize" {
		return false
	}
	id, err := s.deps.PublisherToken(ctx, hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project"))}, c.Param("service"))
	return err == nil && id == credential.ID
}

type BindArtifactServiceRequest struct {
	artifact.Binding
	tracker.Mutation
}

func (s ArtifactHTTP) BindArtifactService(c echo.Context) error {
	var request BindArtifactServiceRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if request.ServiceID != c.Param("service") {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid artifact service binding"))
	}
	// Existing dashboard callers may omit a key; typed clients supply one to
	// replay the same binding command across reconnects.
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = hubapi.NewID("command")
	}
	if !s.deps.StorageAvailable {
		return s.deps.APIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	options := hubapi.NativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath()}
	result, err := s.deps.Bind(c.Request().Context(), hubscope.NativeRequestScope(c), options, request)
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s ArtifactHTTP) ArtifactServices(c echo.Context) error {
	result, err := s.deps.Services(c.Request().Context(), hubscope.NativeRequestScope(c))
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s ArtifactHTTP) artifactPublisher(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	id, err := s.deps.PublisherToken(ctx, scope, c.Param("service"))
	if err != nil || id != scope.Credential.ID {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s ArtifactHTTP) ArtifactReceipt(c echo.Context) error {
	if err := s.artifactPublisher(c); err != nil {
		return s.deps.APIError(c, err)
	}
	var ref artifact.Reference
	if err := hubapi.DecodeAPIJSON(c, &ref); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	if ref.SchemaVersion != 1 || ref.Validate() != nil || ref.OrganizationID != string(scope.Organization) || ref.ProjectID != string(scope.Project) || ref.ServiceID != c.Param("service") || !artifact.ValidID(ref.ArtifactID, "artifact") || !artifact.ValidID(ref.ManifestID, "manifest") || !artifact.ValidHash(ref.SHA256, 64) || ref.Revision < 1 || ref.Revision > artifact.MaxObjects+1 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, ref.Kind) || !slices.Contains([]string{"partial", "complete", "interrupted"}, ref.State) || ref.Availability != "available" || ref.Bytes < 0 || ref.Bytes > artifact.MaxArtifactBytes || ref.Objects < 0 || ref.Objects > artifact.MaxObjects || ref.ExpiresAt.IsZero() || ref.ObservedAt.IsZero() {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid artifact receipt"))
	}
	if err := s.deps.PublishReceipt(c.Request().Context(), scope, ref); err != nil {
		return s.deps.APIError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

func (s ArtifactHTTP) ArtifactReferences(c echo.Context) error {
	refs, err := s.deps.References(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, refs)
}

func (s ArtifactHTTP) AuthorizeArtifactUpload(c echo.Context) error {
	var r artifact.Reservation
	if err := hubapi.DecodeAPIJSON(c, &r); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	if r.Validate() != nil || r.OrganizationID != string(scope.Organization) || r.ProjectID != string(scope.Project) || r.WorkItemID != c.Param("item") || r.LeaseID == "" || r.FencingToken <= 0 || r.Key == "" || len(r.Key) > 128 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, r.Kind) || r.Bytes <= 0 || r.Bytes > artifact.MaxArtifactBytes || scope.Credential.Scope != hubapi.APIScopeWorker {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	if err := s.deps.AuthorizeUpload(c.Request().Context(), scope, r); err != nil {
		if errors.Is(err, artifact.ErrQuota) {
			return s.deps.Refused(c, http.StatusTooManyRequests, "quota_exceeded", "Too many pending artifact uploads; retry after they publish or expire", hubapi.APIErrorResponse{Code: "quota_exceeded", Message: "Too many pending artifact uploads; retry after they publish or expire"})
		}
		return s.deps.APIError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

func (s ArtifactHTTP) ArtifactReadGrant(c echo.Context) error {
	var input struct {
		Revision int64 `json:"revision"`
	}
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	result, err := s.deps.ReadGrant(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), c.Param("artifact"), input.Revision, s.deps.Now())
	if errors.Is(err, artifact.ErrExpired) {
		return s.deps.Refused(c, http.StatusGone, "expired", "Artifact retention has expired", hubapi.APIErrorResponse{Code: "expired", Message: "Artifact retention has expired"})
	}
	if errors.Is(err, artifact.ErrQuota) {
		return s.deps.Refused(c, http.StatusTooManyRequests, "quota_exceeded", "Too many active artifact access grants; retry after they expire", hubapi.APIErrorResponse{Code: "quota_exceeded", Message: "Too many active artifact access grants; retry after they expire"})
	}
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s ArtifactHTTP) AuthorizeArtifactRead(c echo.Context) error {
	ctx := c.Request().Context()
	if err := s.artifactPublisher(c); err != nil {
		return s.deps.APIError(c, err)
	}
	var r artifact.ReadAuthorization
	if err := hubapi.DecodeAPIJSON(c, &r); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if r.Token == "" || len(r.Token) > 4096 || !artifact.ValidID(r.ArtifactID, "artifact") || r.Revision < 1 {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	scope := hubscope.NativeRequestScope(c)
	grant, owner, err := s.deps.ReadStoredGrant(ctx, scope, c.Param("service"), r)
	if err != nil {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	if s.deps.Hosted {
		reader, err := s.deps.HostedReader(c, grant.PrincipalID, grant.SessionHash)
		if err != nil || !(hubscope.NativeScope{Organization: scope.Organization, Project: scope.Project, Credential: reader}).ReporterOwns(owner) {
			return s.deps.APIError(c, hubapi.NativeNotFound())
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (s ArtifactHTTP) GetArtifactReference(c echo.Context) error {
	ctx := c.Request().Context()
	revision, err := artifact.Revision(c.Param("revision"))
	if err != nil {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid artifact revision"))
	}
	result, err := s.deps.Reference(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Param("artifact"), revision, s.deps.Now())
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
