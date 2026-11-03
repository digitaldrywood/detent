package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) runnerProjectDiagnostics(ctx context.Context, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	page, err := e.service.readRunnerProjectDiagnostics(ctx, scope, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	return hubProjectResult(page)
}

func (s *Service) getRunnerProjectDiagnostics(c echo.Context) error {
	scope := nativeRequestScope(c)
	args := operatortool.LocalProjectArguments{ProjectID: string(scope.project), RunnerID: c.QueryParam("runner_id"), IssueID: c.QueryParam("issue_id"), AttemptID: c.QueryParam("attempt_id"), Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid diagnostic limit"))
		}
		args.Limit = n
	}
	for key := range c.QueryParams() {
		if !slices.Contains([]string{"runner_id", "issue_id", "attempt_id", "cursor", "limit"}, key) {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid diagnostic selector"))
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, err := operatortool.DecodeLocalProjectArguments(operatortool.RunnerProjectDiagnostics, raw, true); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid diagnostic selector"))
	}
	page, err := s.readRunnerProjectDiagnostics(c.Request().Context(), scope, args)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readRunnerProjectDiagnostics(ctx context.Context, scope nativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	if s.database.postgres != nil {
		return s.readRunnerProjectDiagnosticsTenant(ctx, scope, args)
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	scope.project = tracker.ProjectID(args.ProjectID)
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	defer tx.Rollback()
	if err := authorizeNativeProject(ctx, tx, scope); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	var visibleIssues map[string]bool
	if scope.credential.HostedRole == "reporter" {
		visibleIssues, err = reporterIssueIDs(ctx, tx, scope)
		if err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if args.IssueID != "" && !visibleIssues[args.IssueID] {
			return runnerauth.DiagnosticPage{}, nativeNotFound()
		}
	}
	now, err := s.database.currentTime()
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, project_configuration_json FROM runner_identities WHERE organization_id = ? AND EXISTS(SELECT 1 FROM token_grants WHERE token_id = runner_identities.token_id AND project_id = ?) AND (? = '' OR id = ?) ORDER BY id LIMIT 101", scope.organization, scope.project, args.RunnerID, args.RunnerID)
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	defer rows.Close()
	type observation struct {
		id  string
		raw *string
	}
	var observed []observation
	for rows.Next() {
		var r observation
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if r.raw != nil {
			entry, err := projectConfigurationEntry(*r.raw, args.ProjectID)
			if err != nil {
				return runnerauth.DiagnosticPage{}, err
			}
			r.raw = nil
			if entry.Valid {
				r.raw = &entry.String
			}
		}
		observed = append(observed, r)
	}
	if err := rows.Err(); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	if err := rows.Close(); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	page, err := (*runnerauth.ProjectDiagnostics)(nil).Page(args.ProjectID, "", args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
	if err != nil {
		return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
	}
	found := false
	for _, item := range observed {
		r, err := readRunner(ctx, tx, scope.organization, item.id, now)
		if err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if !slices.Contains(r.ProjectIDs, scope.project) {
			continue
		}
		if found {
			return runnerauth.DiagnosticPage{}, &operatortool.RequestError{Code: "invalid_request", Message: "Multiple runners report this project; select runner_id"}
		}
		found = true
		var view runnerauth.ProjectConfiguration
		if item.raw != nil {
			if err := json.Unmarshal([]byte(*item.raw), &view); err != nil {
				return runnerauth.DiagnosticPage{}, errProjectServiceUnavailable
			}
		}
		if err := overlayProjectConfigurationObservation(ctx, tx, scope, item.id, &view); err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if view.Diagnostics != nil {
			if visibleIssues != nil {
				view.Diagnostics.Records = slices.DeleteFunc(view.Diagnostics.Records, func(record runnerauth.DiagnosticAttempt) bool { return !visibleIssues[record.IssueID] })
				view.Diagnostics.Admissions = slices.DeleteFunc(view.Diagnostics.Admissions, func(record runnerauth.DiagnosticAdmission) bool { return !visibleIssues[record.IssueID] })
				view.Diagnostics.Counts = nil
				for index := range view.Diagnostics.Admissions {
					view.Diagnostics.Admissions[index].RepositoryReservations = nil
				}
			}
			view.Diagnostics.Redact()
		}
		page, err = view.Diagnostics.Page(args.ProjectID, r.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
		if err != nil {
			return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
		}
		page.ReceivedAt = view.ObservedAt
		if err := isolation.ValidateTier(r.IsolationTier, r.OS); err != nil {
			page.Unavailable["isolation_tier"] = err.Error()
		}
		if r.ConnectionHealth != "online" {
			page.Status = "offline"
			page.Unavailable["runner_reachability"] = "offline_or_revoked"
		}
	}
	if !found {
		page.Unavailable["runner"] = "not_authorized_or_unreported"
	}
	if len(observed) > 100 {
		page.Truncated = true
		page.Unavailable["runner_inventory"] = "truncated_select_runner_id"
	}
	return page, nil
}

type nativeDiagnosticWorkItemBatchReader interface {
	WorkItemsByNativeIDs(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]corestore.WorkItem, error)
}

func (s *Service) readRunnerProjectDiagnosticsTenant(ctx context.Context, scope nativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	scope.project = tracker.ProjectID(args.ProjectID)
	now := s.config.now().UTC()
	page, err := (*runnerauth.ProjectDiagnostics)(nil).Page(args.ProjectID, args.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
	if err != nil {
		return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
	}

	read := func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if scope.credential.HostedRole == "reporter" && args.IssueID != "" {
			item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(args.IssueID))
			if errors.Is(err, corestore.ErrNotFound) || err == nil && !scope.reporterOwns(tracker.Actor{Kind: stringValue(item.ActorKind), PrincipalID: stringValue(item.ActorPrincipalID)}) {
				return nativeNotFound()
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
			authorized, err := r.RunnerProjectAuthorized(ctx, args.RunnerID, scope.project, now)
			if err != nil {
				return err
			}
			if authorized {
				runnerIDs = append(runnerIDs, args.RunnerID)
			}
		} else {
			runnerIDs, truncated, err = reader.RunnerProjectObservationRunners(ctx, scope.project, now, 100)
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
		authorized, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !authorized {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
			return nil
		}
		observation, _, err := r.RunnerProjectObservation(ctx, identity.RunnerID, scope.project, "")
		if errors.Is(err, corestore.ErrNotFound) {
			page.Unavailable["runner"] = "not_authorized_or_unreported"
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
		if scope.credential.HostedRole == "reporter" {
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

	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, read)
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, read)
	}
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	return page, nil
}

func diagnosticReporterIssues(ctx context.Context, reader tenantstore.Reader, scope nativeScope, diagnostics *runnerauth.ProjectDiagnostics, selected string) (map[string]bool, error) {
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
		items, err := batch.WorkItemsByNativeIDs(ctx, scope.project, nativeIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if scope.reporterOwns(tracker.Actor{Kind: stringValue(item.ActorKind), PrincipalID: stringValue(item.ActorPrincipalID)}) {
				visible[string(item.NativeID)] = true
			}
		}
	}
	return visible, nil
}
