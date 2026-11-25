package changeapi

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func validateImplementationContent(scope hubscope.NativeScope, request tracker.PublishChangeVersion) error {
	if len(request.Outcome) > workpad.MaxFinalSummaryBytes || strings.ContainsRune(request.Outcome, 0) {
		return hubapi.NativePublicInvalid("Implementation outcome exceeds the supported description format")
	}
	if (request.Outcome != "" || len(request.EvidenceAttachmentIDs) != 0) && (scope.Credential.Scope != hubapi.APIScopeWorker || !scope.Credential.Runner.Valid()) {
		return hubapi.NativePublicInvalid("Implementation outcome and evidence require a fenced runner publication")
	}
	return nil
}

func implementationEvidenceIDs(scope hubscope.NativeScope, request tracker.PublishChangeVersion, run *PreparedVersionRun) ([]string, error) {
	if request.Outcome == "" && len(request.EvidenceAttachmentIDs) == 0 {
		return nil, nil
	}
	if run == nil || run.diff == nil || run.authority.Event.Data.Identity == nil {
		return nil, hubapi.NativePublicInvalid("Implementation content must belong to the active publishing attempt")
	}
	role := run.authority.Event.Data.Identity.Role
	if role != "code" && role != "rework" {
		return nil, hubapi.NativePublicInvalid("Implementation content requires a code or rework attempt")
	}
	refs, err := attachment.MarkdownReferences(request.Outcome, string(scope.Organization), string(scope.Project))
	if err != nil {
		return nil, err
	}
	ids := slices.Clone(request.EvidenceAttachmentIDs)
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids)+len(request.Artifacts)+1 > changerequest.MaxArtifacts {
		return nil, hubapi.NativePublicInvalid("At most 64 artifact references are permitted")
	}
	for _, id := range ids {
		if _, valid := attachment.URIReference("attachment:"+id, "", ""); !valid {
			return nil, hubapi.NativePublicInvalid("Invalid implementation evidence attachment identity")
		}
	}
	return ids, nil
}

type implementationEvidence struct {
	artifact tracker.ChangeArtifact
	capture  tracker.ChangeEvidenceCapture
}

func readImplementationEvidence(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ids []string, read func(context.Context, tracker.ProjectID, string) (attachmentstore.Record, error)) ([]implementationEvidence, error) {
	var evidence []implementationEvidence
	for _, id := range ids {
		row, err := read(ctx, scope.Project, id)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, hubapi.NativePublicInvalid("Implementation evidence must be a retained attachment of this work item")
		}
		if err != nil {
			return nil, err
		}
		meta := row.Metadata
		if row.OrganizationID != scope.Organization || meta.ProjectID != string(scope.Project) || meta.WorkItemID != string(item) ||
			meta.ID != id || meta.Validate() != nil || row.EvidenceAttemptID == "" || row.Kind != "attachment" ||
			meta.AuthorizedPrincipal != "" || row.PendingContent != nil {
			return nil, hubapi.NativePublicInvalid("Implementation evidence must be a retained attachment of this work item")
		}
		availability := "available"
		if meta.DeletedAt != nil || row.ObjectDeletedAt != nil {
			availability = "missing"
		}
		kind := "log"
		switch meta.ContentType {
		case "image/png", "image/jpeg", "application/pdf":
			kind = "artifact"
		}
		evidence = append(evidence, implementationEvidence{
			artifact: tracker.ChangeArtifact{Kind: kind, URI: "attachment:" + id, SHA256: meta.SHA256, Availability: availability},
			capture:  tracker.ChangeEvidenceCapture{AttachmentID: id, AttemptID: row.EvidenceAttemptID, Caption: row.EvidenceCaption},
		})
	}
	return evidence, nil
}

func bindImplementationEvidence(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ids []string, expected []implementationEvidence) error {
	if len(ids) == 0 {
		return nil
	}
	locker, ok := w.(attachmentstore.LockingReader)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	current, err := readImplementationEvidence(ctx, scope, item, ids, locker.LockAttachment)
	if err != nil {
		return err
	}
	if !slices.Equal(expected, current) {
		return corestore.ErrRevisionConflict
	}
	for _, evidence := range current {
		if evidence.artifact.Availability != "available" {
			continue
		}
		if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: evidence.capture.AttachmentID, WorkItemID: item, RetainWithWorkItem: true}); err != nil {
			return err
		}
	}
	return nil
}

func describeImplementation(change *tracker.ChangeRequest, version tracker.ChangeVersion) {
	if version.ID != change.CurrentVersion || version.Outcome == "" {
		return
	}
	if change.Body != "" {
		change.Body += "\n\n"
	}
	change.Body += "## Implementation outcome\n\n" + version.Outcome
}
