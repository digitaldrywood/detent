package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) attributeScheduledFailure(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
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
	if replay, found, err := s.nativeCommandReplay(ctx, scope, options.OperationID, request.IdempotencyKey, request); found || err != nil {
		return replay, err
	}
	repository, err := policyRepository(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
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
					readErr := func() error {
						rows, err := s.database.reader.QueryContext(ctx, `SELECT work_item_id FROM change_requests WHERE organization_id=? AND project_id=? AND landed_merge_sha=? AND landed_base_ref='develop' LIMIT 2`, scope.organization, scope.project, sha)
						if err != nil {
							return err
						}
						defer rows.Close()
						for rows.Next() {
							if err := rows.Scan(&item); err != nil {
								return err
							}
							matches++
						}
						return rows.Err()
					}()
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
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		result := struct {
			Attributed bool `json:"attributed"`
		}{Attributed: item != ""}
		if item == "" {
			return result, nil
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if issue.Archived {
			result.Attributed = false
			return result, nil
		}
		if err := returnScheduledFailureToWorkflow(ctx, tx, scope, issue, now); err != nil {
			return nil, err
		}
		_, err = insertNativeComment(ctx, tx, scope, issue, request.Body, nil, now)
		return result, err
	})
}
