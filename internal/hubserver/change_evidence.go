package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) reviewChange(c echo.Context) error {
	var request changerequest.ReviewSubmission
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.ValidatorArchive == nil {
		return s.changeRecordEvidenceMutation(c, "review", c.Param("version"), "", request.ReviewChange)
	}
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Scope != hubapi.APIScopeWorker || request.LeaseID == "" || request.FencingToken <= 0 || request.IdempotencyKey == "" || !reflect.DeepEqual(request.ReviewChange, tracker.ReviewChange{Mutation: request.Mutation}) || !artifact.ValidHash(request.ValidatorArchive.SHA256, 64) || !artifact.ValidHash(request.ValidatorArchive.ContentSHA256, 64) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid validator archive submission"))
	}
	ctx := hubcontent.IsolateContentContext(s.runDetailContext(c.Request().Context()))
	item, change, version := c.Param("item"), c.Param("change"), c.Param("version")
	payload, err := s.readValidatorArchive(ctx, scope, item, *request.ValidatorArchive)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if payload.OrganizationID != scope.Organization || payload.ProjectID != scope.Project || string(payload.WorkItemID) != item || payload.ChangeID != change || payload.VersionID != version || payload.Review.Mutation != request.Mutation || payload.Review.Validator == nil || payload.Review.ImplementationReview != nil || payload.Review.SecurityAudit != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Validator archive differs from the submitted review"))
	}
	raw, err := s.reviewChangeRecordCommand(ctx, scope, item, change, version, c.Request().Method+" "+c.Request().URL.EscapedPath(), payload.Review)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var ack struct {
		ID        string `json:"review_id"`
		VersionID string `json:"version_id"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.ID == "" || ack.VersionID != version {
		return s.nativeAPIError(c, artifact.ErrIntegrity)
	}
	return c.JSON(http.StatusOK, ack)
}

func (s *Service) readValidatorArchive(ctx context.Context, scope hubscope.NativeScope, item string, input changerequest.ValidatorArchiveReference) (changerequest.ValidatorArchive, error) {
	var payload changerequest.ValidatorArchive
	var ref attachment.ArchiveReference
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		ref, err = r.AttachmentArchive(ctx, attachmentstore.ArchiveQuery{ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), Digest: input.SHA256, Binding: &input.BindingID})
		return err
	})
	if err != nil {
		return payload, hubapi.ProjectRecordError(err)
	}
	content := &hubcontent.ChangeContent{Call: s.storageArchiveContent(), Store: s.runDetailStore()}
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF {
		return payload, hubapi.NativePublicInvalid("Invalid validator archive payload")
	}
	return payload, nil
}

func (s *Service) getValidatorPublicationReceipt(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Scope != hubapi.APIScopeWorker {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	version, key := c.QueryParam("version_id"), c.QueryParam("idempotency_key")
	if !hubapi.ValidID(version, "version") || key == "" || len(key) > 128 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid validator receipt identity"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	operation := http.MethodPost + " " + c.Request().URL.EscapedPath() + "/versions/" + version + "/reviews"
	var hash string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
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
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, map[string]string{"request_sha256": hash})
}

func (s *Service) submitChangeCheck(c echo.Context) error {
	var request tracker.SubmitChangeCheck
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.Hosted != nil && hubscope.NativeRequestScope(c).Credential.Runner.RunnerID == "" && request.Source != "independent" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.changeCheckRecordMutation(c, request)
}

func (s *Service) discussChange(c echo.Context) error {
	var request tracker.DiscussChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.changeRecordEvidenceMutation(c, "discussion", request.VersionID, "", request)
}

func changeReviewContentFields(request tracker.ReviewChange) []cloudContentField {
	fields := []cloudContentField{{name: "body", value: request.Body}}
	for _, result := range []struct {
		name  string
		value *gate.ValidatorResult
	}{{"validator", request.Validator}, {"implementation_review", request.ImplementationReview}} {
		if result.value == nil {
			continue
		}
		for i, evidence := range result.value.CriteriaEvidence {
			fields = append(fields, cloudContentField{name: fmt.Sprintf("%s.criteria_evidence[%d].reference", result.name, i), value: evidence.Reference})
		}
	}
	return fields
}
