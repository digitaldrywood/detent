package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
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
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin || scope.credential.Runner.RunnerID != "" {
		return scope, operatortool.ErrAccessDenied
	}
	if err := a.service.requireHostedProject(ctx, a.service.database.db, scope, write); err != nil {
		return scope, operatortool.ErrAccessDenied
	}
	if err := a.service.database.authorizeNativeProject(ctx, scope); err != nil {
		return scope, operatortool.ErrAccessDenied
	}
	if args.ItemID != "" {
		if _, _, err := readNativeIssue(ctx, a.service.database.db, scope, args.ItemID); err != nil {
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
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%s: %w", strings.ReplaceAll(name, "_", " "), resultErr)
		}
	}()
	scope, err := a.authorized(ctx, args, false)
	if err != nil {
		return operatortool.ChangeResult{}, err
	}
	s := a.service
	result := a.result(scope, args)
	switch name {
	case operatortool.ArtifactLibrary:
		return result, operatortool.ErrServiceUnavailable
	case operatortool.ListChanges:
		rows, err := changeRows[tracker.ChangeRequest](ctx, s.database.db, `SELECT c.record_json FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? ORDER BY c.rowid LIMIT ? OFFSET ?`, scope.organization, scope.project, args.ItemID, changeLimit(args)+1, args.Offset)
		if len(rows) > changeLimit(args) {
			next := args.Offset + changeLimit(args)
			result.NextOffset = &next
			rows = rows[:changeLimit(args)]
		}
		result.Changes = rows
		return result, err
	case operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff:
		source := args.Source
		if source == "" {
			source = tracker.DiffSourceAttempt
		}
		var diff *tracker.AttemptDiff
		if name == operatortool.GetAttemptDiff {
			value, readErr := s.readAttemptDiff(ctx, scope, args.ItemID, args.AttemptID, source, args.Sequence)
			diff, err = &value, readErr
		} else {
			value, readErr := s.readWorkItemDiff(ctx, scope, args.ItemID, source)
			diff, err = value.Diff, readErr
		}
		result.Diff, result.NextOffset = operatortool.ChangeDiffPage(diff, args)
		return result, err
	case operatortool.ListWorkItemPullRequests:
		rows, readErr := s.readWorkItemPullRequests(ctx, scope, args.ItemID, "")
		result.PullRequests, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, readErr
	case operatortool.GetNativeRun:
		attempt, readErr := s.readNativeAttempt(ctx, scope, args.ItemID, args.AttemptID)
		if readErr != nil {
			return result, readErr
		}
		result.Attempt = &attempt
		changes, readErr := s.readChanges(ctx, scope, args.ItemID)
		if readErr != nil {
			return result, readErr
		}
		result.Changes = changes
		refs, readErr := s.readArtifactReferences(ctx, scope, args.ItemID)
		for _, ref := range refs {
			if ref.AttemptID == args.AttemptID {
				result.Artifacts = append(result.Artifacts, ref)
			}
		}
		return result, readErr
	case operatortool.GetChange, operatortool.GetChangeVersion:
		tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return result, fmt.Errorf("begin snapshot: %w", err)
		}
		defer tx.Rollback()
		if name == operatortool.GetChange {
			result, err = readBoundedChange(ctx, tx, scope, args, result)
		} else {
			if _, err = readChange(ctx, tx, scope, args.ItemID, args.ChangeID); err == nil {
				var version tracker.ChangeVersion
				version, err = readChangeVersion(ctx, tx, args.ChangeID, args.VersionID)
				result.Version = &version
			}
		}
		if err != nil {
			return result, fmt.Errorf("load detail: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return result, fmt.Errorf("finish snapshot: %w", err)
		}
		return result, nil
	case operatortool.GetChangeReviewPolicy:
		policy, err := readChangePolicy(ctx, s.database.db, scope)
		result.Policy = &policy
		return result, err
	case operatortool.ChangeViewedFiles:
		rows, err := s.changeViewedFilesCommand(ctx, scope, args.ItemID, args.ChangeID, args.VersionID)
		result.Viewed, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, err
	case operatortool.GetArtifactReference:
		ref, err := s.artifactReferenceCommand(ctx, s.database.db, scope, args.ItemID, args.ArtifactID, args.Revision, s.config.now())
		result.Artifacts = []artifact.Reference{ref}
		return result, err
	case operatortool.ArtifactServices:
		result.Services, err = s.artifactServicesCommand(ctx, scope)
		return result, err
	case operatortool.ArtifactReferences:
		rows, err := s.readArtifactReferences(ctx, scope, args.ItemID)
		result.Artifacts, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, err
	default:
		return result, operatortool.ErrUnknownTool
	}
}
func changeLimit(args operatortool.ChangeArguments) int {
	if args.Limit == 0 {
		return 100
	}
	return args.Limit
}

func (a hubChangeApplication) MutateChange(ctx context.Context, name string, args operatortool.ChangeArguments) (operatortool.ChangeResult, error) {
	scope, err := a.authorized(ctx, args, name != operatortool.ArtifactAccess)
	if err != nil {
		return operatortool.ChangeResult{}, err
	}
	s := a.service
	result := a.result(scope, args)
	command := tracker.Mutation{IdempotencyKey: args.RequestID}
	var input any
	var op func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)
	path := result.URL
	method := "POST"
	switch name {
	case operatortool.PublishChangeVersion:
		if args.ExpectedVersionID == nil || args.Code == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.PublishChangeVersion{Mutation: command, ExpectedVersionID: *args.ExpectedVersionID, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: args.BaseSHA, HeadSHA: args.HeadSHA, MergeBaseSHA: args.MergeBaseSHA, Repository: args.Repository, Code: *args.Code, Source: args.SourceCapture, Artifacts: args.Artifacts, PolicyID: args.PolicyID, External: args.External}, SourceBundle: args.SourceBundle}
		input = request
		path += "/versions"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.publishChangeVersionCommand(ctx, tx, scope, args.ItemID, args.ChangeID, request, now)
		}
	case operatortool.CreateChange:
		request := tracker.CreateChange{Mutation: command, Title: args.Title, Body: args.Body, LinkedIssues: args.LinkedIssues}
		input = request
		path += "/changes"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.createChangeCommand(ctx, tx, scope, args.ItemID, request, now)
		}
	case operatortool.DiscussChange:
		request := tracker.DiscussChange{Mutation: command, VersionID: args.VersionID, Body: args.Body, Escape: args.Escape}
		input = request
		path += "/discussion"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.discussChangeCommand(ctx, tx, scope, args.ItemID, args.ChangeID, request, now)
		}
	case operatortool.ReviewChange:
		if scope.credential.Hosted != nil {
			scope.requireHostedAdmin = true
		}
		request := tracker.ReviewChange{Mutation: command, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ExpectedVersionID: args.VersionID, Decision: args.Decision, Body: args.Body, Bundle: args.Bundle}
		input = request
		path += "/versions/" + args.VersionID + "/reviews"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.reviewChangeCommand(ctx, tx, scope, args.ItemID, args.ChangeID, args.VersionID, request, now)
		}
	case operatortool.ViewChangeFile:
		if args.Bundle == nil {
			return result, operatortool.ErrInvalidArguments
		}
		request := tracker.ViewChangeFile{Mutation: command, Bundle: *args.Bundle, FileSHA256: args.FileSHA256, Viewed: args.Viewed}
		input = request
		path += "/versions/" + args.VersionID + "/viewed-files"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.viewChangeFileCommand(ctx, tx, scope, args.ItemID, args.ChangeID, args.VersionID, request, now)
		}
	case operatortool.ApproveChangeReviewPolicy:
		if args.Policy == nil || scope.credential.Hosted == nil && scope.credential.Scope != apiScopeAdmin {
			return result, operatortool.ErrAccessDenied
		}
		scope.requireHostedAdmin = true
		request := tracker.ApproveChangeReviewPolicy{Mutation: command, ExpectedID: args.ExpectedPolicyID, Policy: *args.Policy}
		input = request
		path += "/change-review-policy"
		method = "PUT"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.approveChangeReviewPolicyCommand(ctx, tx, scope, request)
		}
	case operatortool.BindArtifactService:
		if args.Binding == nil || scope.credential.Hosted == nil && scope.credential.Scope != apiScopeAdmin {
			return result, operatortool.ErrAccessDenied
		}
		scope.requireHostedAdmin = true
		request := struct {
			artifact.Binding
			tracker.Mutation
		}{*args.Binding, command}
		input = request
		path += "/artifact-services/" + args.Binding.ServiceID
		method = "PUT"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			return s.bindArtifactServiceCommand(ctx, tx, scope, args.Binding.ServiceID, *args.Binding)
		}
	case operatortool.ArtifactAccess:
		// The retry receipt pins only safe immutable identity. A replay still checks
		// current access and mints a new short-lived token; tokens are never durable.
		input = args
		path += "/artifacts/" + args.ArtifactID + "/access"
		op = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			var raw []byte
			if err := tx.QueryRowContext(ctx, `SELECT reference_json FROM artifact_references WHERE organization_id=? AND project_id=? AND work_item_id=? AND artifact_id=? AND revision=?`, scope.organization, scope.project, args.ItemID, args.ArtifactID, args.Revision).Scan(&raw); err != nil {
				return nil, err
			}
			var ref artifact.Reference
			if err := json.Unmarshal(raw, &ref); err != nil {
				return nil, err
			}
			if ref.SHA256 != args.SHA256 || !now.Before(ref.ExpiresAt) || ref.Availability != "available" {
				return nil, operatortool.ErrAccessDenied
			}
			return struct {
				ArtifactID string `json:"artifact_id"`
				Revision   int64  `json:"revision"`
				SHA256     string `json:"sha256"`
			}{args.ArtifactID, args.Revision, args.SHA256}, nil
		}
	default:
		return result, operatortool.ErrUnknownTool
	}
	result.Receipt, err = s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: method + " " + path, Item: args.ItemID, Feature: "collaboration", ArtifactRead: name == operatortool.ArtifactAccess}, command, input, op)
	if err != nil {
		return result, err
	}
	if name == operatortool.PublishChangeVersion {
		var version tracker.ChangeVersion
		if err := json.Unmarshal(result.Receipt, &version); err != nil {
			return result, err
		}
		result.Version = &version
		tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return result, err
		}
		defer tx.Rollback()
		change, err := readChange(ctx, tx, scope, args.ItemID, args.ChangeID)
		if err != nil {
			return result, err
		}
		detail, err := readCurrentChangeDetail(ctx, tx, scope, change, s.config.now())
		if err != nil {
			return result, err
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, args.ItemID)
		if err != nil {
			return result, err
		}
		result.Detail, result.WorkItemState = &detail, issue.State
		return result, tx.Commit()
	}
	if name == operatortool.CreateChange {
		var change tracker.ChangeRequest
		if err := json.Unmarshal(result.Receipt, &change); err != nil {
			return result, err
		}
		args.ChangeID = change.ID
		result.ChangeID = change.ID
		result.URL = a.result(scope, args).URL
	}
	if name == operatortool.ArtifactAccess {
		ref, err := s.artifactReferenceCommand(ctx, s.database.db, scope, args.ItemID, args.ArtifactID, args.Revision, s.config.now())
		if err != nil {
			return result, err
		}
		if ref.SHA256 != args.SHA256 {
			return result, operatortool.ErrAccessDenied
		}
		grant, err := s.artifactReadGrantCommand(ctx, scope, args.ItemID, args.ArtifactID, args.Revision, s.config.now())
		if err != nil {
			return result, err
		}
		access, err := operatortool.DownloadResult(grant)
		result.Access = &access
		return result, err
	}
	return result, nil
}
