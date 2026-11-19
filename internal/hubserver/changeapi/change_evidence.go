package changeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *HTTP) ReviewChange(c echo.Context) error {
	var request changerequest.ReviewSubmission
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if request.ValidatorArchive == nil {
		return s.deps.ChangeRecordEvidenceMutation(c, "review", c.Param("version"), "", request.ReviewChange)
	}
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Scope != hubapi.APIScopeWorker || request.LeaseID == "" || request.FencingToken <= 0 || request.IdempotencyKey == "" || !reflect.DeepEqual(request.ReviewChange, tracker.ReviewChange{Mutation: request.Mutation}) || !artifact.ValidHash(request.ValidatorArchive.SHA256, 64) || !artifact.ValidHash(request.ValidatorArchive.ContentSHA256, 64) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Invalid validator archive submission"))
	}
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	item, change, version := c.Param("item"), c.Param("change"), c.Param("version")
	payload, err := s.readValidatorArchive(ctx, scope, item, *request.ValidatorArchive, request.Mutation)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if payload.OrganizationID != scope.Organization || payload.ProjectID != scope.Project || string(payload.WorkItemID) != item || payload.ChangeID != change || payload.VersionID != version || payload.Review.Mutation != request.Mutation || payload.Review.Validator == nil || payload.Review.ImplementationReview != nil || payload.Review.SecurityAudit != nil {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Validator archive differs from the submitted review"))
	}
	raw, err := s.deps.ReviewChangeRecordCommand(ctx, scope, item, change, version, c.Request().Method+" "+c.Request().URL.EscapedPath(), payload.Review)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var ack struct {
		ID        string `json:"review_id"`
		VersionID string `json:"version_id"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.ID == "" || ack.VersionID != version {
		return s.deps.NativeAPIError(c, artifact.ErrIntegrity)
	}
	return c.JSON(http.StatusOK, ack)
}

func (s *HTTP) readValidatorArchive(ctx context.Context, scope hubscope.NativeScope, item string, input changerequest.ValidatorArchiveReference, mutation tracker.Mutation) (changerequest.ValidatorArchive, error) {
	var payload changerequest.ValidatorArchive
	var ref attachment.ArchiveReference
	sessions := executionapi.SessionDeps{AuthorizeAccess: s.deps.AuthorizeAccessScope}
	fence := runnersession.Mutation{LeaseID: mutation.LeaseID, FencingToken: mutation.FencingToken}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if _, _, err := sessions.RunnerSessionFence(ctx, r, scope, fence, item, false); err != nil {
			return err
		}
		var err error
		ref, err = r.AttachmentArchive(ctx, attachmentstore.ArchiveQuery{ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), Digest: input.SHA256, Binding: &input.BindingID})
		return err
	})
	if err != nil {
		return payload, hubapi.ProjectRecordError(err)
	}
	content := &hubcontent.ChangeContent{Call: s.deps.StorageArchiveContent(), Store: s.deps.RunDetailStore()}
	if content.Call == nil && content.Store == nil {
		return payload, hubapi.NativeWorkItemStorageUnavailable()
	}
	if ref.Validate(string(scope.Organization)) != nil || ref.BindingID != input.BindingID || ref.SHA256 != input.SHA256 || ref.SchemaVersion != 1 {
		return payload, artifact.ErrIntegrity
	}
	raw, err := hubcontent.ReadPublishedContent(ctx, content, string(scope.Organization), ref)
	if err != nil {
		return payload, err
	}
	if artifact.Digest(raw) != input.ContentSHA256 {
		return payload, artifact.ErrIntegrity
	}
	if !validValidatorArchiveJSON(raw) {
		return payload, hubapi.NativePublicInvalid("Invalid validator archive payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return payload, hubapi.NativePublicInvalid("Invalid validator archive payload")
	}
	return payload, nil
}

func validValidatorArchiveJSON(raw []byte) bool {
	if len(raw) > attachment.MaxArchiveBytes {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(raw) {
		return false
	}
	shapeBudget := hubapi.MaxAPIRequestBodyBytes / int(reflect.TypeFor[gate.CommandResult]().Size())
	var containers []byte
	var quoted, escaped bool
	for _, b := range raw {
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			quoted = b != '"'
			escaped = b == '\\'
			continue
		}
		switch b {
		case '"':
			quoted = true
			continue
		case '{', '[':
			containers = append(containers, b)
		case '}', ']':
			containers = containers[:len(containers)-1]
			continue
		case ',':
			if containers[len(containers)-1] != '[' {
				continue
			}
		default:
			continue
		}
		shapeBudget--
		if shapeBudget < 0 {
			return false
		}
	}
	return true
}

func (s *HTTP) getValidatorPublicationReceipt(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Scope != hubapi.APIScopeWorker {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	version, key := c.QueryParam("version_id"), c.QueryParam("idempotency_key")
	if !hubapi.ValidID(version, "version") || key == "" || len(key) > 128 {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Invalid validator receipt identity"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	operation := http.MethodPost + " " + c.Request().URL.EscapedPath() + "/versions/" + version + "/reviews"
	var hash string
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		receipt, err := r.Receipt(ctx, scope.Credential.ID, operation, key)
		if err != nil {
			return err
		}
		hash = receipt.RequestHash
		return nil
	})
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, map[string]string{"request_sha256": hash})
}

func (s *HTTP) SubmitChangeCheck(c echo.Context) error {
	var request tracker.SubmitChangeCheck
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if _, hosted := s.deps.HostedOrganization(); hosted && hubscope.NativeRequestScope(c).Credential.Runner.RunnerID == "" && request.Source != "independent" {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	if s.deps.Repository == nil {
		return s.deps.NativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.deps.ChangeCheckRecordMutation(c, request)
}

func (s *HTTP) DiscussChange(c echo.Context) error {
	var request tracker.DiscussChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	return s.deps.ChangeRecordEvidenceMutation(c, "discussion", request.VersionID, "", request)
}
