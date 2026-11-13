package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hubChangeApplication struct {
	service *Service
	scope   hubscope.NativeScope
}

func (a hubChangeApplication) authorized(ctx context.Context, args operatortool.ChangeArguments, write bool) (hubscope.NativeScope, error) {
	scope := a.scope
	scope.Project = tracker.ProjectID(args.ProjectID)
	if a.service.config.TenantRepository == nil || a.service.config.AccessRepository == nil {
		return scope, operatortool.ErrServiceUnavailable
	}
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin || scope.Credential.Runner.RunnerID != "" {
		return scope, operatortool.ErrAccessDenied
	}
	if err := a.service.requireHostedProject(ctx, nil, scope, write); err != nil {
		return scope, operatortool.ErrAccessDenied
	}
	project, err := a.service.readProjectRecord(ctx, scope)
	if err != nil || project.Core.DeletedAt != nil {
		return scope, operatortool.ErrAccessDenied
	}
	if args.ItemID != "" {
		if _, _, err := a.service.readWorkItemRecord(ctx, scope, args.ItemID); err != nil {
			return scope, operatortool.ErrAccessDenied
		}
	}
	return scope, nil
}

func (a hubChangeApplication) result(scope hubscope.NativeScope, args operatortool.ChangeArguments) operatortool.ChangeResult {
	result := operatortool.ChangeResult{OrganizationID: string(scope.Organization), ProjectID: args.ProjectID, WorkItemID: args.ItemID, ChangeID: args.ChangeID, GeneratedAt: a.service.config.now().UTC(), Freshness: "live"}
	result.URL = "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(args.ProjectID)
	if args.ItemID != "" {
		result.URL += "/work-items/" + url.PathEscape(args.ItemID)
	}
	if args.ChangeID != "" {
		result.URL += "/changes/" + url.PathEscape(args.ChangeID)
	}
	return result
}

func (a hubChangeApplication) ReadChange(ctx context.Context, name string, args operatortool.ChangeArguments) (value operatortool.ChangeResult, resultErr error) {
	ctx = a.service.contentContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	defer func() {
		if resultErr != nil {
			if archive := hubapi.NativeArchiveReadError(resultErr); archive != nil {
				resultErr = archive
			}
			resultErr = fmt.Errorf("%s: %w", strings.ReplaceAll(name, "_", " "), resultErr)
		}
	}()
	if a.service.config.TenantRepository == nil {
		return operatortool.ChangeResult{}, operatortool.ErrServiceUnavailable
	}
	switch name {
	case operatortool.ListChanges:
		return a.readChangeListRecords(ctx, args)
	case operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff, operatortool.GetNativeRun, operatortool.ChangeViewedFiles, operatortool.ListWorkItemPullRequests, operatortool.GetChangeReviewPolicy, operatortool.ArtifactServices, operatortool.ArtifactReferences, operatortool.GetArtifactReference:
		scope, err := a.authorized(ctx, args, false)
		if err != nil {
			return operatortool.ChangeResult{}, err
		}
		value = a.result(scope, args)
		switch name {
		case operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff:
			source := args.Source
			if source == "" {
				source = tracker.DiffSourceAttempt
			}
			var diff *tracker.AttemptDiff
			if name == operatortool.GetAttemptDiff {
				var stored tracker.AttemptDiff
				stored, err = a.service.readAttemptDiff(ctx, scope, args.ItemID, args.AttemptID, source, args.Sequence)
				if err == nil {
					diff = &stored
				}
			} else {
				var stored tracker.WorkItemDiff
				stored, err = a.service.readWorkItemDiff(ctx, scope, args.ItemID, source)
				diff = stored.Diff
			}
			if err != nil {
				err = hubapi.ProjectRecordError(err)
			}
			value.Diff, value.NextOffset = operatortool.ChangeDiffPage(diff, args)
		case operatortool.GetNativeRun:
			var attempt tracker.NativeAttempt
			attempt, err = a.service.readNativeAttempt(ctx, scope, args.ItemID, args.AttemptID)
			if err == nil {
				value.Attempt = &attempt
			}
		case operatortool.ChangeViewedFiles:
			value.Viewed, err = a.service.readChangeViewedFileRecords(ctx, scope, args.ItemID, args.ChangeID, args.VersionID)
			value.Viewed, value.NextOffset = operatortool.ChangePage(value.Viewed, args)
		case operatortool.ListWorkItemPullRequests:
			value.PullRequests, err = a.service.readWorkItemPullRequests(ctx, scope, args.ItemID, "")
		case operatortool.GetChangeReviewPolicy:
			var rules tracker.ChangeReviewPolicy
			rules, err = a.service.changeReviewPolicyRecord(ctx, scope)
			if err == nil {
				value.Policy = &rules
			}
		case operatortool.ArtifactServices:
			value.Services, err = a.service.artifactServicesCommand(ctx, scope)
		case operatortool.ArtifactReferences:
			value.Artifacts, err = a.service.readArtifactReferences(ctx, scope, args.ItemID)
		case operatortool.GetArtifactReference:
			var reference artifact.Reference
			reference, err = a.service.artifactReferenceCommand(ctx, scope, args.ItemID, args.ArtifactID, args.Revision, a.service.config.now())
			if err == nil {
				value.Artifacts = []artifact.Reference{reference}
			}
		}
		if err != nil {
			return operatortool.ChangeResult{}, hubSafeChangeError(err)
		}
		return value, nil
	case operatortool.GetChange, operatortool.GetChangeVersion:
		scope, err := a.authorized(ctx, args, false)
		if err != nil {
			return operatortool.ChangeResult{}, err
		}
		scope.Action = name
		ctx = a.service.archiveContext(ctx, scope, args.ArchiveURL)
		defer func() { value.Archive = nativeArchiveDescriptor(ctx) }()
		value = a.result(scope, args)
		if name == operatortool.GetChange {
			value, err = a.service.readBoundedChangeRecord(ctx, scope, args, value)
		} else {
			var version tracker.ChangeVersion
			version, err = a.service.readExactChangeVersionRecord(ctx, scope, args.ItemID, args.ChangeID, args.VersionID)
			if err == nil {
				value.Version = &version
			}
		}
		if err != nil && errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return operatortool.ChangeResult{}, operatortool.ErrAccessDenied
		}
		return value, err
	default:
		return operatortool.ChangeResult{}, operatortool.ErrServiceUnavailable
	}
}

func changeLimit(args operatortool.ChangeArguments) int {
	if args.Limit == 0 {
		return 100
	}
	return args.Limit
}

func (a hubChangeApplication) MutateChange(ctx context.Context, name string, args operatortool.ChangeArguments) (operatortool.ChangeResult, error) {
	ctx = a.service.contentContext(ctx)
	if content, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok {
		ctx = context.WithValue(ctx, hubcontent.ChangeContentKey{}, &hubcontent.ChangeContent{Encoder: content.Encoder, Call: content.Call, Store: content.Store, Register: content.Register, ReadOrganization: content.ReadOrganization, ReadReference: content.ReadReference, Query: content.Query})
	}
	scope, err := a.authorized(ctx, args, name != operatortool.ArtifactAccess)
	if err != nil {
		return operatortool.ChangeResult{}, err
	}
	s := a.service
	result := a.result(scope, args)
	command := tracker.Mutation{IdempotencyKey: args.RequestID}
	scope.Action = name
	ctx = hubcontent.IsolateContentContext(s.runDetailContext(ctx))
	switch name {
	case operatortool.ViewChangeFile:
		if args.Bundle == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.ViewChangeFile{Mutation: command, Bundle: *args.Bundle, FileSHA256: args.FileSHA256, Viewed: args.Viewed}
		result.Receipt, err = s.viewChangeFileRecordCommand(ctx, scope, args.ItemID, args.ChangeID, args.VersionID, http.MethodPost+" "+result.URL+"/versions/"+url.PathEscape(args.VersionID)+"/viewed-files", request)
		if err != nil {
			return result, err
		}
	case operatortool.ApproveChangeReviewPolicy:
		if args.Policy == nil {
			return result, operatortool.ErrInvalidArguments
		}
		if scope.Credential.Hosted != nil {
			scope.RequireHostedAdmin = true
		}
		request := tracker.ApproveChangeReviewPolicy{Mutation: command, ExpectedID: args.ExpectedPolicyID, Policy: *args.Policy}
		result.Receipt, err = s.approveChangeReviewPolicyRecordCommand(ctx, scope, http.MethodPut+" "+result.URL+"/change-review-policy", request)
		if err != nil {
			return result, err
		}
	case operatortool.AssociateReleasePick:
		if args.ReleasePick == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.AssociateReleasePick{Mutation: command, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ReleasePickInput: *args.ReleasePick}
		result.Receipt, err = s.associateReleasePickTenant(ctx, scope, args.ItemID, http.MethodPost+" "+result.URL+"/release-pick", request)
	case operatortool.PublishReleasePick:
		if args.ReleasePick == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.PublishReleasePick{Mutation: command, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ReleasePickInput: *args.ReleasePick}
		var publication tracker.ReleasePickPublication
		publication, err = s.prepareReleasePublication(ctx, scope, args.ItemID, request)
		if err == nil {
			result.Receipt, err = s.recordReleasePublicationCommand(ctx, scope, args.ItemID, request, publication)
		}
	case operatortool.BindArtifactService:
		if args.Binding == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := bindArtifactServiceRequest{Mutation: command, Binding: *args.Binding}
		options := hubapi.NativeCommandOptions{OperationID: http.MethodPut + " " + result.URL + "/artifact-services/" + url.PathEscape(args.Binding.ServiceID)}
		result.Receipt, err = s.executeProjectRecordMutation(ctx, scope, options, command, request)
	case operatortool.CreateChange:
		request := tracker.CreateChange{Mutation: command, Title: args.Title, Body: args.Body, LinkedIssues: args.LinkedIssues}
		result.Receipt, err = s.createChangeRecordCommand(ctx, scope, args.ItemID, http.MethodPost+" "+result.URL+"/changes", request)
		if err != nil {
			return result, err
		}
		var change tracker.ChangeRequest
		if err := json.Unmarshal(result.Receipt, &change); err != nil {
			return result, err
		}
		args.ChangeID, result.ChangeID = change.ID, change.ID
		result.URL = a.result(scope, args).URL
	case operatortool.PublishChangeVersion:
		if args.ExpectedVersionID == nil || args.Code == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.PublishChangeVersion{Mutation: command, ExpectedVersionID: *args.ExpectedVersionID, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: args.BaseSHA, HeadSHA: args.HeadSHA, MergeBaseSHA: args.MergeBaseSHA, Repository: args.Repository, Code: *args.Code, Source: args.SourceCapture, Artifacts: args.Artifacts, PolicyID: args.PolicyID, External: args.External}, SourceBundle: args.SourceBundle}
		result.Receipt, err = s.publishChangeVersionRecordCommand(ctx, scope, args.ItemID, args.ChangeID, http.MethodPost+" "+result.URL+"/versions", request)
		if err != nil {
			return result, err
		}
		var version tracker.ChangeVersion
		if err := json.Unmarshal(result.Receipt, &version); err != nil {
			return result, err
		}
		result.Version = &version
		item, _, err := s.readWorkItemRecord(ctx, scope, args.ItemID)
		if err != nil {
			return result, err
		}
		result.WorkItemState = hubapi.StringValue(item.WorkflowStateName)
		result, err = s.readBoundedChangeRecord(ctx, scope, args, result)
		if err != nil {
			return result, err
		}
	case operatortool.DiscussChange:
		request := tracker.DiscussChange{Mutation: command, VersionID: args.VersionID, Body: args.Body, Escape: args.Escape}
		result.Receipt, err = s.changeRecordEvidenceCommand(ctx, scope, args.ItemID, args.ChangeID, http.MethodPost+" "+result.URL+"/discussion", "discussion", args.VersionID, "", request)
		if err != nil {
			return result, err
		}
	case operatortool.ArtifactAccess:
		result.Receipt, err = a.artifactAccessReceipt(ctx, scope, args)
		if err != nil {
			return result, err
		}
		grant, err := s.artifactReadGrantCommand(ctx, scope, args.ItemID, args.ArtifactID, args.Revision, s.config.now())
		if err != nil {
			return result, err
		}
		access, err := operatortool.DownloadResult(grant)
		if err != nil {
			return result, err
		}
		result.Access = &access
	case operatortool.ReviewChange:
		if scope.Actor().Kind != "human" {
			return result, operatortool.ErrServiceUnavailable
		}
		if scope.Credential.Hosted != nil {
			scope.RequireHostedAdmin = true
		}
		request := tracker.ReviewChange{Mutation: command, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ExpectedVersionID: args.VersionID, Decision: args.Decision, Body: args.Body, Bundle: args.Bundle}
		result.Receipt, err = s.reviewChangeRecordCommand(ctx, scope, args.ItemID, args.ChangeID, args.VersionID, http.MethodPost+" "+result.URL+"/versions/"+url.PathEscape(args.VersionID)+"/reviews", request)
		if err != nil {
			return result, err
		}
	default:
		return result, operatortool.ErrServiceUnavailable
	}
	return result, err
}

func (a hubChangeApplication) artifactAccessReceipt(ctx context.Context, scope hubscope.NativeScope, args operatortool.ChangeArguments) (json.RawMessage, error) {
	s := a.service
	if strings.TrimSpace(args.RequestID) == "" || len(args.RequestID) > 128 {
		return nil, operatortool.ErrInvalidArguments
	}
	reference, err := s.artifactReferenceCommand(ctx, scope, args.ItemID, args.ArtifactID, args.Revision, s.config.now())
	if err != nil {
		return nil, err
	}
	if reference.SHA256 != args.SHA256 {
		return nil, operatortool.ErrAccessDenied
	}
	if reference.Availability != "available" {
		return nil, artifact.ErrExpired
	}
	hash, err := hubapi.CommandHash(args)
	if err != nil {
		return nil, err
	}
	operation := "operator artifact access/" + args.ProjectID + "/" + args.ItemID + "/" + args.ArtifactID
	if raw, found, err := s.changeRecordReceipt(ctx, scope, operation, args.RequestID, hash); err != nil || found {
		return raw, err
	}
	raw, err := json.Marshal(reference)
	if err != nil {
		return nil, err
	}
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: args.RequestID, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		item, err := w.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(args.ItemID))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		current, err := w.ArtifactReference(ctx, scope.Project, item.NativeID, args.ArtifactID, args.Revision)
		if err != nil {
			return err
		}
		if current != reference {
			return corestore.ErrRevisionConflict
		}
		binding, err := w.ArtifactService(ctx, scope.Project, reference.ServiceID)
		if err != nil {
			return err
		}
		if !artifact.ValidOrigin(binding.Origin) {
			return artifact.ErrMissing
		}
		if !s.config.now().Before(reference.ExpiresAt) {
			return artifact.ErrExpired
		}
		existing, err := w.Receipt(ctx, scope.Credential.ID, operation, args.RequestID)
		if err == nil {
			if existing.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return raw, nil
}
