package conversations

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const AttachmentMetadataBase = hubapi.NativeBase + "/attachment-metadata"

var cloudAttachmentReference = regexp.MustCompile(`\]\(\s*<?attachment:(att_[A-Za-z0-9_-]+)>?(?:\s+"[^"]*")?\s*\)`)

func CloudContentAttachmentReferences(body string, scope hubscope.NativeScope) ([]attachment.Reference, error) {
	refs, err := attachment.MarkdownReferences(body, string(scope.Organization), string(scope.Project))
	if err != nil {
		return nil, err
	}
	for _, match := range cloudAttachmentReference.FindAllStringSubmatch(body, -1) {
		refs = append(refs, attachment.Reference{URL: "attachment:" + match[1], ID: match[1]})
	}
	return append(refs, attachment.References(body, string(scope.Organization), string(scope.Project))...), nil
}

func (s AttachmentHTTP) CheckCloudAttachment(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	var input struct {
		Size     int64                       `json:"size"`
		Upload   *attachment.UploadRequest   `json:"upload,omitempty"`
		Evidence *attachment.EvidenceRequest `json:"evidence,omitempty"`
	}
	if err := c.Bind(&input); err != nil || input.Size < 0 || input.Size > attachment.MaxBytes {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid attachment size"))
	}
	if input.Upload != nil {
		return s.prepareCloudAttachment(c, *input.Upload)
	}
	if err := s.deps.CheckEvidence(c, input.Evidence); err != nil {
		return s.deps.APIError(c, err)
	}

	if !s.deps.StorageAvailable {
		return s.deps.APIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	scope := hubscope.NativeRequestScope(c)
	err := s.deps.ReadTenant(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.deps.Authorize(ctx, r, scope, true); err != nil {
			return err
		}
		limit, err := s.deps.QuotaLimit(ctx, r)
		if err != nil {
			return err
		}
		used, err := r.CollaborationBytes(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if input.Size > limit-used {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
		}
		return nil
	})
	if err != nil {
		return s.deps.APIError(c, hubapi.ProjectRecordError(err))
	}

	return c.JSON(http.StatusOK, map[string]string{"principal_id": hubscope.NativeRequestScope(c).Credential.ID})
}

func (s AttachmentHTTP) prepareCloudAttachment(c echo.Context, input attachment.UploadRequest) error {
	return s.deps.PrepareUpload(c, input)
}

func (s AttachmentHTTP) RecordCloudAttachment(c echo.Context) error {
	var input attachment.UploadRequest
	if err := c.Bind(&input); err != nil || input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid attachment metadata"))
	}
	return s.deps.RecordUpload(c, input)
}

func (s AttachmentHTTP) GetCloudAttachment(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	record, err := s.deps.ReadMetadata(c.Request().Context(), scope, c.Param("attachment"))
	if err != nil {
		return s.deps.APIError(c, err)
	}
	record.AuthorizedPrincipal = scope.Credential.ID
	return c.JSON(http.StatusOK, record)
}

func (s AttachmentHTTP) DeleteCloudAttachment(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if err := s.DeleteCloudAttachmentCommand(c.Request().Context(), scope, c.Param("attachment")); err != nil {
		return s.deps.APIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s AttachmentHTTP) DeleteCloudAttachmentCommand(ctx context.Context, scope hubscope.NativeScope, id string) error {
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return hubapi.NativeNotFound()
	}
	return s.deps.DeleteMetadata(ctx, scope, id)
}

func (s AttachmentHTTP) ReferenceCloudAttachment(c echo.Context) error {
	var input struct {
		WorkItemID   string               `json:"work_item_id"`
		CommentID    string               `json:"comment_id"`
		LeaseID      tracker.LeaseID      `json:"lease_id"`
		FencingToken tracker.FencingToken `json:"fencing_token"`
	}
	if err := c.Bind(&input); err != nil || input.WorkItemID == "" {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("A work item is required"))
	}
	if err := s.deps.ReferenceMetadata(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("attachment"), attachment.SourceReference{WorkItemID: input.WorkItemID, CommentID: input.CommentID}, tracker.Mutation{LeaseID: input.LeaseID, FencingToken: input.FencingToken}); err != nil {
		return s.deps.APIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s AttachmentHTTP) ExpiredCloudAttachments(c echo.Context) error {
	return s.deps.Expired(c)
}

func (s AttachmentHTTP) FinishCloudAttachmentDeletion(c echo.Context) error {
	return s.deps.FinishDeletion(c)
}

func (s AttachmentHTTP) CloudAttachmentExists(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	claims, ok := s.deps.SharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	var input attachment.ObjectLookup
	if err := c.Bind(&input); err != nil || !input.Valid(claims.Audience) {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid attachment lookup"))
	}
	var exists bool
	var references []bool
	err := s.deps.ReadTenant(ctx, tracker.OrganizationID(claims.Audience), func(r tenantstore.Reader) error {
		var err error
		if input.Keys != nil {
			references, err = r.ReferencedAttachmentObjects(ctx, input.Keys)
		} else {
			exists, err = r.AttachmentObjectExists(ctx, input.ID, input.Key)
		}
		return err
	})
	if err != nil {
		return s.deps.APIError(c, err)
	}
	if input.Keys != nil {
		return c.JSON(http.StatusOK, references)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}
