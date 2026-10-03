package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) previewProviderCandidates(c echo.Context) error {
	var request tracker.NativeCapacityPreview
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if request.Limit < 0 || request.Limit > 100 {
		return s.nativeAPIError(c, nativeInvalid("Candidate limit must be between 1 and 100"))
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		scope := nativeRequestScope(c)
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		if err := authorizeClaimScope(ctx, tx, tracker.ClaimRequest{MachineID: request.MachineID}, &scope); err != nil {
			return nil, err
		}
		query := claimCandidateQuery{DispatchPriorityByState: request.DispatchPriorityByState, DispatchPriorityByLabel: request.DispatchPriorityByLabel, PrioritizeUnblockers: request.PrioritizeUnblockers, PolicyID: request.PolicyID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
		query.Limit, query.After, query.AvailableAt = request.Limit, request.After, now
		if _, err := validateClaimPolicy(ctx, tx, query, request.MachineID); err != nil {
			return nil, err
		}
		if scope.credential.Runner.RunnerID != "" {
			if err := validateRunnerDispatch(ctx, tx, scope, now); err != nil {
				return nil, err
			}
		}
		ids, err := claimCandidateIDs(ctx, tx, query, nil, nil, normalizedQueryStrings(request.WorkflowStates), normalizedQueryStrings(request.Authors), normalizedQueryStrings(request.Assignees), normalizedQueryStrings(request.LabelInclude), normalizedQueryStrings(request.LabelExclude), nil)
		if err != nil {
			return nil, err
		}
		page := tracker.NativeCapacityPage{Items: []tracker.NativeIssue{}}
		limit := request.Limit
		if limit == 0 {
			limit = 100
		}
		for _, id := range ids {
			lease, found, err := readUnreleasedLease(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
				continue
			}
			ready, _, err := nativeLandingCandidateReady(ctx, tx, &scope, id, now)
			if err != nil {
				return nil, err
			}
			if !ready {
				continue
			}
			var native string
			if err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id = ?", id).Scan(&native); err != nil {
				return nil, err
			}
			issue, _, err := readNativeIssue(ctx, tx, scope, native)
			if err != nil {
				return nil, err
			}
			page.Items = append(page.Items, s.nativeIssueResponse(issue))
		}
		if len(ids) == limit {
			page.Next = ids[len(ids)-1]
		}
		return page, nil
	})
}
