package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) attributeScheduledFailure(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	failure := request.ScheduledFailure
	if scope.credential.Scope == apiScopeWorker || scope.credential.HostedRole == "reporter" {
		return nil, &nativeError{Code: "insufficient_scope", Message: "Scheduled reporting requires operator authority", status: http.StatusForbidden, publicMessage: true}
	}
	if failure == nil || len(failure.PreviousGreenSHA) != 40 || len(request.Body) == 0 || len(request.Body) > 32<<10 || request.ScheduledValidation != nil || request.State != "" || request.Title != "" || request.Type != "" || request.ReportContext != nil || len(request.Assignees) != 0 || request.ChallengeToken != "" || len(request.NotDuplicateOf) != 0 || request.Priority != nil || len(request.Labels) != 0 || request.Provenance != nil || request.GitHubIssueURL != "" {
		return nil, nativePublicInvalid("Invalid scheduled failure attribution")
	}
	for _, ch := range failure.PreviousGreenSHA {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return nil, nativePublicInvalid("Invalid scheduled failure base")
		}
	}
	scope.action = ""
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/scheduled-failures"), Feature: "collaboration"}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if replay, found, err := s.authorizedChangeRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash); found || err != nil {
		return replay, err
	}
	repository, err := s.scheduledRepository(ctx, scope)
	if err != nil {
		return nil, err
	}
	evidence := failure.Evidence
	if !strings.EqualFold(repository, evidence.Repository) || len(gate.ParseScheduledEvidence(evidence.Stamp())) != 1 {
		return nil, nativePublicInvalid("Invalid scheduled failure evidence")
	}
	var item string
	if s.config.ImportBackend != nil {
		verified, verifyErr := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence, ScheduledBranch: "develop"})
		if verifyErr == nil && verified.Scheduled != nil {
			page, rangeErr := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled-range", CompareBase: failure.PreviousGreenSHA, CompareHead: evidence.HeadSHA})
			if rangeErr == nil && len(page.Commits) > 0 && len(page.Commits) <= 100 {
				matches := 0
				for _, sha := range page.Commits {
					readErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						owner, ok := r.(policystore.ScheduledReader)
						if !ok {
							return workItemStorageUnavailable()
						}
						items, err := owner.ScheduledLandedWorkItems(ctx, scope.project, sha)
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
	return s.commitScheduledAttribution(ctx, scope, options.OperationID, repository, item, request)
}
