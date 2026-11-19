package operatorcommands

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledAttributionDeps struct {
	Repository           tenantstore.Repository
	Importer             githubcontract.ImportBackend
	Details              func() attachment.DetailStore
	Now                  func() time.Time
	Operation            func(hubscope.NativeScope, string, string) string
	RecheckProject       func(context.Context, hubscope.NativeScope) error
	Receipt              func(context.Context, hubscope.NativeScope, string, string, string) (json.RawMessage, bool, error)
	LoadWorkItemPlan     func(context.Context, hubscope.NativeScope, string, string, any) (hubrecords.WorkItemRecordPlan, error)
	PlanWorkItemRecord   func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, any) (hubrecords.WorkItemRecordPlan, error)
	PrepareFields        func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	AuthorizeWorkItem    func(context.Context, accessstore.Reader, hubscope.NativeScope) error
	RequireCollaboration func(context.Context, tenantstore.Writer, time.Time) error
	ProjectActor         func(hubscope.NativeScope) tenantstore.ProjectActor
}

func (s ScheduledAttributionDeps) Attribute(ctx context.Context, scope hubscope.NativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	if s.Repository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	failure := request.ScheduledFailure
	if scope.Credential.Scope == hubapi.APIScopeWorker || scope.Credential.HostedRole == "reporter" {
		return nil, &hubapi.NativeError{Code: "insufficient_scope", Message: "Scheduled reporting requires operator authority", Status: http.StatusForbidden, PublicMessage: true}
	}
	if failure == nil || len(failure.PreviousGreenSHA) != 40 || len(request.Body) == 0 || len(request.Body) > 32<<10 || request.ScheduledValidation != nil || request.State != "" || request.Title != "" || request.Type != "" || request.ReportContext != nil || len(request.Assignees) != 0 || request.ChallengeToken != "" || len(request.NotDuplicateOf) != 0 || request.Priority != nil || len(request.Labels) != 0 || request.Provenance != nil || request.GitHubIssueURL != "" {
		return nil, hubapi.NativePublicInvalid("Invalid scheduled failure attribution")
	}
	for _, ch := range failure.PreviousGreenSHA {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return nil, hubapi.NativePublicInvalid("Invalid scheduled failure base")
		}
	}
	scope.Action = ""
	options := hubapi.NativeCommandOptions{OperationID: s.Operation(scope, "POST", "/scheduled-failures"), Feature: "collaboration"}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.RecheckProject(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if replay, found, err := s.Receipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash); found || err != nil {
		return replay, err
	}
	repository, err := ScheduledRepository(ctx, s.Repository, scope)
	if err != nil {
		return nil, err
	}
	evidence := failure.Evidence
	if !strings.EqualFold(repository, evidence.Repository) || len(gate.ParseScheduledEvidence(evidence.Stamp())) != 1 {
		return nil, hubapi.NativePublicInvalid("Invalid scheduled failure evidence")
	}
	var item string
	if s.Importer != nil {
		verified, verifyErr := s.Importer.FetchImportPage(ctx, githubcontract.GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence, ScheduledBranch: "develop"})
		if verifyErr == nil && verified.Scheduled != nil {
			page, rangeErr := s.Importer.FetchImportPage(ctx, githubcontract.GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled-range", CompareBase: failure.PreviousGreenSHA, CompareHead: evidence.HeadSHA})
			if rangeErr == nil && len(page.Commits) > 0 && len(page.Commits) <= 100 {
				matches := 0
				for _, sha := range page.Commits {
					readErr := s.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						owner, ok := r.(policystore.ScheduledReader)
						if !ok {
							return hubapi.NativeWorkItemStorageUnavailable()
						}
						items, err := owner.ScheduledLandedWorkItems(ctx, scope.Project, sha)
						if err != nil {
							return err
						}
						for _, id := range items {
							item = id
							matches++
						}
						return nil
					})
					if readErr != nil {
						return nil, readErr
					}
					if matches > 1 {
						break
					}
				}
				if matches != 1 {
					item = ""
				}
			}
		}
	}
	return s.commit(ctx, scope, options.OperationID, repository, item, request)
}
