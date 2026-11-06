package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hubChangeApplication struct {
	service *Service
	scope   nativeScope
}

func (a hubChangeApplication) authorized(ctx context.Context, args operatortool.ChangeArguments, write bool) (nativeScope, error) {
	scope := a.scope
	scope.project = tracker.ProjectID(args.ProjectID)
	if a.service.config.TenantRepository == nil || a.service.config.AccessRepository == nil {
		return scope, operatortool.ErrServiceUnavailable
	}
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin || scope.credential.Runner.RunnerID != "" {
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

func (a hubChangeApplication) result(scope nativeScope, args operatortool.ChangeArguments) operatortool.ChangeResult {
	result := operatortool.ChangeResult{OrganizationID: string(scope.organization), ProjectID: args.ProjectID, WorkItemID: args.ItemID, ChangeID: args.ChangeID, GeneratedAt: a.service.config.now().UTC(), Freshness: "live"}
	result.URL = "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(args.ProjectID)
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	defer func() {
		if resultErr != nil {
			if archive := nativeArchiveReadError(resultErr); archive != nil {
				resultErr = archive
			}
			resultErr = fmt.Errorf("%s: %w", strings.ReplaceAll(name, "_", " "), resultErr)
		}
	}()
	if a.service.config.TenantRepository != nil {
		if name == operatortool.ListChanges {
			return a.readChangeListRecords(ctx, args)
		}
		return operatortool.ChangeResult{}, operatortool.ErrServiceUnavailable
	}

	return operatortool.ChangeResult{}, operatortool.ErrServiceUnavailable
}

func changeLimit(args operatortool.ChangeArguments) int {
	if args.Limit == 0 {
		return 100
	}
	return args.Limit
}

func (a hubChangeApplication) MutateChange(ctx context.Context, name string, args operatortool.ChangeArguments) (operatortool.ChangeResult, error) {
	ctx = a.service.contentContext(ctx)
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		ctx = context.WithValue(ctx, changeContentKey{}, &changeContent{encoder: content.encoder, call: content.call, store: content.store, register: content.register, query: content.query})
	}
	scope, err := a.authorized(ctx, args, name != operatortool.ArtifactAccess)
	if err != nil {
		return operatortool.ChangeResult{}, err
	}
	s := a.service
	result := a.result(scope, args)
	command := tracker.Mutation{IdempotencyKey: args.RequestID}
	scope.action = name
	ctx = isolateContentContext(s.runDetailContext(ctx))
	switch name {
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
		result.WorkItemState = stringValue(item.WorkflowStateName)
	case operatortool.ReviewChange:
		if scope.actor().Kind != "human" {
			return result, operatortool.ErrServiceUnavailable
		}
		if scope.credential.Hosted != nil {
			scope.requireHostedAdmin = true
		}
		request := tracker.ReviewChange{Mutation: command, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ExpectedVersionID: args.VersionID, Decision: args.Decision, Body: args.Body, Bundle: args.Bundle}
		result.Receipt, err = s.reviewChangeRecordCommand(ctx, scope, args.ItemID, args.ChangeID, args.VersionID, http.MethodPost+" "+result.URL+"/versions/"+url.PathEscape(args.VersionID)+"/reviews", request)
		if err != nil {
			return result, err
		}
	default:
		return result, operatortool.ErrServiceUnavailable
	}
	return result, nil
}
