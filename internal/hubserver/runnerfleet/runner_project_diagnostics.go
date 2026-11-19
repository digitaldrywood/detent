package runnerfleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DiagnosticDeps struct {
	Repository      tenantstore.Repository
	Now             func() time.Time
	HTTPError       func(echo.Context, error, ...any) error
	AuthorizeAccess func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	DetailsFor      func(context.Context) attachment.DetailStore
}

func (d DiagnosticDeps) GetRunnerProjectDiagnostics(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	args := operatortool.LocalProjectArguments{ProjectID: string(scope.Project), RunnerID: c.QueryParam("runner_id"), IssueID: c.QueryParam("issue_id"), AttemptID: c.QueryParam("attempt_id"), Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid diagnostic limit"))
		}
		args.Limit = n
	}
	for key := range c.QueryParams() {
		if !slices.Contains([]string{"runner_id", "issue_id", "attempt_id", "cursor", "limit"}, key) {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid diagnostic selector"))
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if _, err := operatortool.DecodeLocalProjectArguments(operatortool.RunnerProjectDiagnostics, raw, true); err != nil {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid diagnostic selector"))
	}
	page, err := d.ReadRunnerProjectDiagnostics(c.Request().Context(), scope, args)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (d DiagnosticDeps) ReadRunnerProjectDiagnostics(ctx context.Context, scope hubscope.NativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	return d.readRunnerProjectDiagnosticsTenant(ctx, scope, args)
}

type nativeDiagnosticWorkItemBatchReader interface {
	WorkItemsByNativeIDs(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]corestore.WorkItem, error)
}

func diagnosticReporterIssues(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, diagnostics *runnerauth.ProjectDiagnostics, selected string) (map[string]bool, error) {
	visible := map[string]bool{}
	if selected != "" {
		visible[selected] = true
		return visible, nil
	}
	ids := map[string]bool{}
	for _, record := range diagnostics.Records {
		ids[record.IssueID] = true
	}
	for _, admission := range diagnostics.Admissions {
		ids[admission.IssueID] = true
	}
	nativeIDs := make([]tracker.NativeWorkItemID, 0, len(ids))
	for id := range ids {
		nativeIDs = append(nativeIDs, tracker.NativeWorkItemID(id))
	}
	slices.Sort(nativeIDs)
	batch, ok := reader.(nativeDiagnosticWorkItemBatchReader)
	if !ok {
		return nil, errors.New("reporter diagnostic visibility unavailable")
	}
	for start := 0; start < len(nativeIDs); start += corestore.MaxPageSize {
		end := min(len(nativeIDs), start+corestore.MaxPageSize)
		items, err := batch.WorkItemsByNativeIDs(ctx, scope.Project, nativeIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if scope.ReporterOwns(tracker.Actor{Kind: hubapi.StringValue(item.ActorKind), PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				visible[string(item.NativeID)] = true
			}
		}
	}
	return visible, nil
}

func (d DiagnosticDeps) readRunnerProjectDiagnosticsTenant(ctx context.Context, scope hubscope.NativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	scope.Project = tracker.ProjectID(args.ProjectID)
	now := d.Now().UTC()
	page, err := (*runnerauth.ProjectDiagnostics)(nil).Page(args.ProjectID, args.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
	if err != nil {
		return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
	}

	read := func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		if scope.Credential.HostedRole == "reporter" && args.IssueID != "" {
			item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(args.IssueID))
			if errors.Is(err, corestore.ErrNotFound) || err == nil && !scope.ReporterOwns(tracker.Actor{Kind: hubapi.StringValue(item.ActorKind), PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				return hubapi.NativeNotFound()
			}
			if err != nil {
				return err
			}
		}
		reader, ok := r.(runnerstore.ProjectObservationRunnerPageReader)
		if !ok {
			return errors.New("runner diagnostic reader unavailable")
		}
		runnerIDs := []string{}
		truncated := false
		if args.RunnerID != "" {
			authorized, err := r.RunnerProjectAuthorized(ctx, args.RunnerID, scope.Project, now)
			if err != nil {
				return err
			}
			if authorized {
				runnerIDs = append(runnerIDs, args.RunnerID)
			}
		} else {
			runnerIDs, truncated, err = reader.RunnerProjectObservationRunners(ctx, scope.Project, now, 100)
			if err != nil {
				return err
			}
		}
		if len(runnerIDs) == 0 {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
			return nil
		}
		if len(runnerIDs) > 1 && args.RunnerID == "" {
			return &operatortool.RequestError{Code: "invalid_request", Message: "Multiple runners report this project; select runner_id"}
		}
		identity, err := r.RunnerIdentity(ctx, runnerIDs[0])
		if errors.Is(err, corestore.ErrNotFound) {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
			return nil
		}
		if err != nil {
			return err
		}
		authorized, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !authorized {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
			return nil
		}
		observation, _, err := r.RunnerProjectObservation(ctx, identity.RunnerID, scope.Project, "")
		if errors.Is(err, corestore.ErrNotFound) {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
			if err := isolation.ValidateTier(identity.IsolationTier, identity.OS); err != nil {
				page.Unavailable["isolation_tier"] = err.Error()
			}
			return nil
		}
		if err != nil {
			return err
		}
		page, err = observation.Diagnostics.Page(args.ProjectID, identity.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
		if err != nil {
			return operatortool.ErrInvalidArguments
		}
		page.ReceivedAt = observation.ObservedAt
		if truncated {
			page.Truncated = true
			page.Unavailable["runner_inventory"] = "truncated_select_runner_id"
		}
		if observation.Diagnostics == nil {
			page, err = (*runnerauth.ProjectDiagnostics)(nil).Page(args.ProjectID, identity.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
			if err != nil {
				return operatortool.ErrInvalidArguments
			}
			page.ReceivedAt = observation.ObservedAt
			if truncated {
				page.Truncated = true
				page.Unavailable["runner_inventory"] = "truncated_select_runner_id"
			}
			if identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || identity.LastHeartbeatAt.IsZero() || now.Sub(identity.LastHeartbeatAt) > runnerauth.HeartbeatTimeout {
				page.Status = "offline"
				page.Unavailable["runner_reachability"] = "offline_or_revoked"
			}
			if err := isolation.ValidateTier(identity.IsolationTier, identity.OS); err != nil {
				page.Unavailable["isolation_tier"] = err.Error()
			}
			return nil
		}
		diagnostics := observation.Diagnostics
		if scope.Credential.HostedRole == "reporter" {
			visible, err := diagnosticReporterIssues(ctx, r, scope, diagnostics, args.IssueID)
			if err != nil {
				return err
			}
			diagnostics.Records = slices.DeleteFunc(diagnostics.Records, func(record runnerauth.DiagnosticAttempt) bool { return !visible[record.IssueID] })
			diagnostics.Admissions = slices.DeleteFunc(diagnostics.Admissions, func(record runnerauth.DiagnosticAdmission) bool { return !visible[record.IssueID] })
			diagnostics.Counts = nil
			for index := range diagnostics.Admissions {
				diagnostics.Admissions[index].RepositoryReservations = nil
			}
		}
		diagnostics.Redact()
		page, err = diagnostics.Page(args.ProjectID, identity.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
		if err != nil {
			return operatortool.ErrInvalidArguments
		}
		page.ReceivedAt = observation.ObservedAt
		if truncated {
			page.Truncated = true
			page.Unavailable["runner_inventory"] = "truncated_select_runner_id"
		}
		if identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || identity.LastHeartbeatAt.IsZero() || now.Sub(identity.LastHeartbeatAt) > runnerauth.HeartbeatTimeout {
			page.Status = "offline"
			page.Unavailable["runner_reachability"] = "offline_or_revoked"
		}
		if err := isolation.ValidateTier(identity.IsolationTier, identity.OS); err != nil {
			page.Unavailable["isolation_tier"] = err.Error()
		}
		return nil
	}

	if repository, ok := d.Repository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, read)
	} else {
		err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, read)
	}
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	return page, nil
}
