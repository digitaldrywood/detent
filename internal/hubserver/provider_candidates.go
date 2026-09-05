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
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		scope := nativeRequestScope(c)
		if scope.credential.Runner.RunnerID == "" {
			return nil, nativeInvalid("Provider selection requires an enrolled runner")
		}
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		if err := authorizeClaimScope(ctx, tx, tracker.ClaimRequest{MachineID: request.MachineID}, &scope); err != nil {
			return nil, err
		}
		query := claimCandidateQuery{PolicyID: request.PolicyID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
		if _, err := validateClaimPolicy(ctx, tx, query, request.MachineID); err != nil {
			return nil, err
		}
		if err := validateRunnerDispatch(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		ids, err := claimCandidateIDs(ctx, tx, query, nil, nil, normalizedQueryStrings(request.WorkflowStates), normalizedQueryStrings(request.Authors), normalizedQueryStrings(request.Assignees), normalizedQueryStrings(request.LabelInclude), normalizedQueryStrings(request.LabelExclude), nil)
		if err != nil {
			return nil, err
		}
		page := tracker.NativeCapacityPage{Items: []tracker.NativeIssue{}}
		started := request.After == 0
		for _, id := range ids {
			if !started {
				started = id == request.After
				continue
			}
			lease, found, err := readUnreleasedLease(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
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
			page.Items = append(page.Items, issue)
			if len(page.Items) == 100 {
				page.Next = id
				break
			}
		}
		if !started {
			return nil, providerWait("provider_candidate_changed", "Queue changed during provider selection; refresh the candidate page")
		}
		return page, nil
	})
}
