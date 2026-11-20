package hubserver

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var nativePhaseName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validNativeGitSource(head, tree string) bool {
	return (len(head) == 40 || len(head) == 64) && changerequest.ValidHash(head, len(head)) && changerequest.ValidHash(tree, len(head)) &&
		strings.Trim(head, "0") != "" && strings.Trim(tree, "0") != ""
}

func validNativePhaseBranch(branch string) bool {
	if branch == "" || len(branch) > 512 || branch == "@" || branch == "HEAD" || strings.HasPrefix(branch, "-") ||
		strings.HasPrefix(branch, "refs/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "..") ||
		strings.Contains(branch, "@{") || strings.ContainsAny(branch, " ~^:?*[\\") ||
		strings.ContainsFunc(branch, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	for part := range strings.SplitSeq(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func (s *Service) nativeExecutionItem(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id tracker.NativeWorkItemID, write bool) (corestore.WorkItem, error) {
	if err := s.authorizeAccessScope(ctx, r, scope, write); err != nil {
		return corestore.WorkItem{}, err
	}
	project, err := r.Project(ctx, scope.Project)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if project.DeletedAt != nil {
		return corestore.WorkItem{}, corestore.ErrNotFound
	}
	item, err := r.WorkItem(ctx, scope.Project, id)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if item.Archived || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
		return corestore.WorkItem{}, corestore.ErrNotFound
	}
	return item, nil
}

func (s *Service) lockNativeExecutionAttempt(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item corestore.WorkItem, attemptID, policyID string, fence tracker.Mutation) (leasestore.Attempt, error) {
	stale := hubapi.NativeStaleExecution("The item lease is no longer current")
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return leasestore.Attempt{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	if _, err := locker.LockLease(ctx, string(fence.LeaseID)); err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, tracker.ErrLeaseNotFound) {
			return leasestore.Attempt{}, stale
		}
		return leasestore.Attempt{}, err
	}
	return s.nativeExecutionAttempt(ctx, w, scope, item, attemptID, policyID, fence)
}

func (s *Service) nativeExecutionAttempt(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item corestore.WorkItem, attemptID, policyID string, fence tracker.Mutation) (leasestore.Attempt, error) {
	stale := hubapi.NativeStaleExecution("The item lease is no longer current")
	attempt, err := r.Attempt(ctx, attemptID)
	if errors.Is(err, corestore.ErrNotFound) || (err == nil && attempt.IssueID != item.ID) {
		return leasestore.Attempt{}, stale
	}
	if err != nil {
		return leasestore.Attempt{}, err
	}
	if err := s.authorizeProgressRunner(ctx, r, scope, attempt, &fence); err != nil {
		if errors.Is(err, tracker.ErrStaleFencingToken) {
			return leasestore.Attempt{}, stale
		}
		return leasestore.Attempt{}, err
	}
	pinned, err := r.LeasePolicy(ctx, fence.LeaseID)
	if err != nil {
		return leasestore.Attempt{}, err
	}
	if pinned.ProjectID != scope.Project || pinned.PolicyID != policyID {
		return leasestore.Attempt{}, hubapi.PolicyMismatch("Publication must use the policy pinned to the item lease")
	}
	return attempt, nil
}

func (s *Service) publishNativePhaseReceipt(c echo.Context) error {
	var input tracker.NativePhaseReceipt
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 256 || input.LeaseID == "" ||
		input.FencingToken <= 0 || input.AttemptID == "" || input.PolicyID == "" || !nativePhaseName.MatchString(input.Phase) ||
		!validNativePhaseBranch(input.Branch) || !validNativeGitSource(input.HeadSHA, input.TreeSHA) || !changerequest.ValidHash(input.InputDigest, 64) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Phase receipt requires the lease, attempt, phase, branch, commit, tree, policy and input digest"))
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	itemID := tracker.NativeWorkItemID(c.Param("item"))
	var recorded tracker.NativePhaseReceipt
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		item, err := s.nativeExecutionItem(ctx, w, scope, itemID, true)
		if err != nil {
			return err
		}
		item, err = w.LockWorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		if item.Archived {
			return corestore.ErrNotFound
		}
		attempt, err := s.lockNativeExecutionAttempt(ctx, w, scope, item, input.AttemptID, input.PolicyID, input.Mutation)
		if err != nil {
			return err
		}
		if attempt.Status != "running" {
			return hubapi.NativeStaleExecution("The item attempt has ended")
		}
		owner, ok := w.(leasestore.PhaseReceiptWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		recorded, err = owner.RecordPhaseReceipt(ctx, scope.Project, item.ID, input)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, recorded)
}

func (s *Service) getNativePhaseReceipts(c echo.Context) error {
	params := c.QueryParams()
	phases, digests, policyID := params["phase"], params["input_digest"], c.QueryParam("policy_id")
	if len(phases) != len(digests) || len(phases) > 64 || (len(phases) > 0 && (policyID == "" || len(policyID) > 256)) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Phase continuation requires a policy and an input digest for each requested phase"))
	}
	seen := make(map[string]bool, len(phases))
	for i, phase := range phases {
		if seen[phase] || !nativePhaseName.MatchString(phase) || !changerequest.ValidHash(digests[i], 64) {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Phase continuation has an invalid or repeated phase or input digest"))
		}
		seen[phase] = true
	}

	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	var response any
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := s.nativeExecutionItem(ctx, r, scope, tracker.NativeWorkItemID(c.Param("item")), false)
		if err != nil {
			return err
		}
		if err := s.authorizeProgressRunner(ctx, r, scope, leasestore.Attempt{}, nil); err != nil {
			return err
		}
		owner, ok := r.(leasestore.PhaseReceiptReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if len(phases) == 0 {
			response, err = owner.RecentPhaseReceipts(ctx, scope.Project, item.ID)
		} else {
			response, err = owner.PhaseContinuation(ctx, scope.Project, item.ID, policyID, phases, digests)
		}
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, response)
}
