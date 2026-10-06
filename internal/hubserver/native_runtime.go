package hubserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func validateNativeRuntime(r *tracker.NativeRuntimeObservation) error {
	if r == nil {
		return nil
	}
	if r.Completion != nil && (r.Completion.ObservedAt.IsZero() || r.Phase != "completed") {
		return nativeInvalid("Host completion observations require a completed phase and observation time")
	}
	if r.Completion != nil {
		r.Completion.Source = "host_native_completion"
		r.Completion.Coverage = "recorded_host_completion_branch_only; independent_of_provider_disposition_and_claim_release"
	}
	if !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed"}, r.Phase) || r.LocalAttemptID < 0 || len(r.Phases) > 128 || r.PhasesDropped < 0 {
		return nativeInvalid("Runtime observations require bounded phase and attempt identity")
	}
	for _, p := range r.Phases {
		if !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed"}, p.Name) || p.StartedAt.IsZero() || !p.FinishedAt.IsZero() && p.FinishedAt.Before(p.StartedAt) {
			return nativeInvalid("Invalid runtime phase interval")
		}
	}
	for _, v := range []string{r.Identity.BackendID, r.Identity.BackendKind, r.Identity.Role, r.Identity.Provider.Value, r.Identity.RequestedModel.Value, r.Identity.ResolvedModel.Value, r.Identity.ReasoningEffort.Value, r.Identity.ServiceTier.Value} {
		if v != "" && !validExecutionName(v) {
			return nativeInvalid("Invalid runtime identity")
		}
	}
	r.Identity = r.Identity.Normalize()
	r.Identity.Route = ""
	r.Identity.Selection = agentidentity.Selection{}
	if r.Activity != nil {
		if r.Activity.Schema != 1 || r.Activity.AttemptID != r.LocalAttemptID || r.Activity.Generation != r.Generation || len(r.Activity.Spans) > 1024 || len(r.Activity.Sources) > 64 {
			return nativeInvalid("Activity profile does not match its runtime observation")
		}
		profile := workflowmetrics.PublicActivityProfile(*r.Activity)
		r.Activity = &profile
	}
	if l := r.Landing; l != nil {
		if l.ChangeID != "" && !validNativeID(l.ChangeID, "change") || l.VersionID != "" && !validNativeID(l.VersionID, "version") || l.HeadSHA != "" && !validCommitID(l.HeadSHA) {
			return nativeInvalid("Invalid landing identity")
		}
		if l.Landed {
			if !validNativeID(l.ChangeID, "change") || !validNativeID(l.VersionID, "version") || !validCommitID(l.HeadSHA) || l.RefusalKind != "" || !validCommitID(l.MergeSHA) || !slices.Contains([]string{"squash", "merge", "rebase"}, l.Method) || !validExecutionName(l.BaseRef) {
				return nativeInvalid("Invalid landing receipt")
			}
		} else if l.MergeSHA != "" || !slices.Contains([]string{"head_moved", "missing_head", "conflict", "nothing_to_land", "base_protected", "base_moved"}, l.RefusalKind) {
			return nativeInvalid("Invalid landing refusal")
		}
	}
	if r.REST != nil {
		if len(r.REST.Windows) > 64 || len(r.REST.Divergences) > 64 || r.REST.Requests < 0 || r.REST.WindowsDropped < 0 || r.REST.DivergencesDropped < 0 {
			return nativeInvalid("REST evidence exceeds its bound")
		}
		r.REST.Source = "ordinary_response_headers"
		r.REST.Coverage = "selected_client_only; subprocesses_other_clients_hosts_and_account_consumer_attribution_unavailable"
		for i := range r.REST.Windows {
			w := &r.REST.Windows[i]
			if !validNativeRESTCredential(w.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, w.Resource) || !slices.Contains([]string{"search", "label issues", "repository issues", "issue comments", "issue field values", "issue dependencies", "issue reads", "pull requests", "reviews", "mutations", "workflow runs", "check run annotations", "check runs", "commit statuses", "other"}, w.EndpointFamily) || w.Requests < 0 {
				return nativeInvalid("Invalid REST operation evidence")
			}
			w.BudgetScope = "native_landing"
		}
		for _, d := range r.REST.Divergences {
			if !validNativeRESTCredential(d.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, d.Resource) || !slices.Contains([]string{"expected_shared_credential", "unattributed"}, d.Attribution) || d.ObservedRequests < 0 || d.DetentRequests < 0 || d.AttributedRequests < 0 || d.UnattributedRequests < 0 {
				return nativeInvalid("Invalid REST attribution evidence")
			}
		}
	}
	return validateNativeGitHubScope(r.GitHub)
}

func (s *Service) getNativeExplanation(c echo.Context) error {
	if len(c.QueryParams()) != 0 {
		return s.nativeAPIError(c, nativeInvalid("Unsupported explanation selector"))
	}
	evidence, err := s.readNativeRuntime(c.Request().Context(), nativeRequestScope(c), c.Param("item"), "")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, explain.FromNativeEvidence(evidence))
}

func (s *Service) getNativeRuntime(c echo.Context) error {
	for key, values := range c.QueryParams() {
		if key != "native_attempt_id" && key != "attempt_id" && key != "admission" || len(values) != 1 {
			return s.nativeAPIError(c, nativeInvalid("Unsupported runtime selector"))
		}
	}
	selector := c.QueryParam("native_attempt_id")
	if c.QueryParam("attempt_id") != "" {
		if selector != "" {
			return s.nativeAPIError(c, nativeInvalid("Select one attempt identity"))
		}
		selector = c.QueryParam("attempt_id")
	}
	var admission []tracker.NativeAdmissionContext
	if raw := c.QueryParam("admission"); raw != "" {
		var current tracker.NativeAdmissionContext
		if len(raw) > 8192 {
			return s.nativeAPIError(c, nativeInvalid("Admission context exceeds its bound"))
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&current); err != nil {
			return s.nativeAPIError(c, nativeInvalid("Invalid admission context"))
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return s.nativeAPIError(c, nativeInvalid("Invalid admission context"))
		}
		if err := validateNativeAdmissionContext(current); err != nil {
			return s.nativeAPIError(c, err)
		}
		if nativeRequestScope(c).credential.Runner.RunnerID == "" {
			return s.nativeAPIError(c, nativeInvalid("Admission context requires the current registered runner"))
		}
		admission = append(admission, current)
	}
	result, err := s.readNativeRuntime(c.Request().Context(), nativeRequestScope(c), c.Param("item"), selector, admission...)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readNativeRuntime(ctx context.Context, scope nativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	defer tx.Rollback()
	return readNativeRuntimeWithAdmission(ctx, tx, scope, item, attemptID, s.config.now().UTC(), minimumRunnerVersion(s.config.Version), admission...)
}

func readNativeRuntime(ctx context.Context, query nativeQueryer, scope nativeScope, item, attemptID string, now time.Time, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	return readNativeRuntimeWithAdmission(ctx, query, scope, item, attemptID, now, "", admission...)
}

func readNativeRuntimeWithAdmission(ctx context.Context, query nativeQueryer, scope nativeScope, item, attemptID string, now time.Time, minimumVersion string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	issue, id, err := readNativeIssue(ctx, query, scope, item)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	nonExecutableReason := connector.NonExecutableReason(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels})
	issue = issue.RuntimeReference()
	e := tracker.NativeRuntimeEvidence{Issue: issue, ObservedAt: now, Selection: "unavailable", Unavailable: []string{"efficiency_receipt"}, Capacity: []tracker.NativeRuntimeCapacity{}}
	if attemptID == "" {
		err = query.QueryRowContext(ctx, "SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT 1", scope.organization, scope.project, item).Scan(&attemptID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return e, err
		}
		e.Selection = "latest"
	} else if !validNativeID(attemptID, "attempt") {
		local, err := strconv.ParseInt(attemptID, 10, 64)
		if err != nil || local <= 0 {
			return e, nativeInvalid("Invalid attempt identity")
		}
		rows, err := query.QueryContext(ctx, "SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND json_extract(data_json, '$.runtime.local_attempt_id')=? LIMIT 2", scope.organization, scope.project, item, local)
		if err != nil {
			return e, err
		}
		defer rows.Close()
		var matches []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return e, err
			}
			matches = append(matches, value)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return e, err
		}
		if len(matches) == 0 {
			return e, nativeNotFound()
		}
		if len(matches) != 1 {
			return e, nativeInvalid("Local attempt identity is ambiguous; select the native attempt")
		}
		attemptID = matches[0]
		e.Selection = "selected"
	} else {
		e.Selection = "selected"
	}
	if attemptID != "" {
		a, err := readNativeAttempt(ctx, query, scope, item, attemptID, now)
		if err != nil {
			return e, err
		}
		e.Attempt = &a
		if a.Current && a.Status == "running" && e.Selection == "latest" {
			e.Selection = "active"
		}
	}
	if e.Attempt == nil {
		e.Selection = "unavailable"
		e.Unavailable = append(e.Unavailable, "native_attempt")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil {
		e.Unavailable = append(e.Unavailable, "runtime_phase_heartbeat")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil || e.Attempt.Runtime.Activity == nil {
		e.Unavailable = append(e.Unavailable, "instruction_activity")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil || e.Attempt.Runtime.REST == nil {
		e.Unavailable = append(e.Unavailable, "rest_accounting")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil || e.Attempt.Runtime.GitHub == nil {
		e.Unavailable = append(e.Unavailable, "github_scope_timings")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil || e.Attempt.Runtime.Landing == nil {
		e.Unavailable = append(e.Unavailable, "attempt_landing_receipt")
	}
	if e.Attempt == nil || e.Attempt.Finalization == nil {
		e.Unavailable = append(e.Unavailable, "host_finalization")
	}
	if e.Attempt == nil || e.Attempt.TerminalFailure == nil {
		e.Unavailable = append(e.Unavailable, "terminal_failure")
	}
	if e.Attempt == nil || e.Attempt.Runtime == nil || e.Attempt.Runtime.Completion == nil {
		e.Unavailable = append(e.Unavailable, "host_issue_acceptance")
	}
	for _, kind := range []string{"workflow.transitioned", "scheduler.decision"} {
		event, err := latestNativeRuntimeEvent(ctx, query, scope, item, kind)
		if err != nil {
			return e, err
		}
		if event.ID == "" {
			continue
		}
		if kind == "workflow.transitioned" {
			e.LatestTransition = &event
		} else {
			e.LatestDecision = &event
		}
	}
	if e.LatestDecision == nil {
		e.Unavailable = append(e.Unavailable, "historical_scheduler_decision")
	}
	change, found, err := readLatestNativeChangeRequest(ctx, query, scope, item)
	if err != nil {
		return e, err
	}
	if found {
		detail, err := readCurrentChangeDetail(ctx, query, scope, change, now)
		if err != nil {
			return e, err
		}
		detail.Change.Body = ""
		detail.External = nil
		detail.Summary.Messages = nil
		detail.Change.LinkedIssues = nil
		detail.Versions = nil
		detail.Discussion = nil
		detail.Reviews = nil
		detail.Checks = nil
		e.Change = &detail
	}
	ready := nativeChangeLandingReady(issue.State, e.Change)
	e.Scheduling = tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner-specific selectors, policy and capacity must also permit this item"}
	if !ready {
		e.Scheduling.Outcome = "skipped"
		e.Scheduling.Reason = "Current Change version is not ready for native landing"
	}
	lease, found, err := readUnreleasedLease(ctx, query, id)
	if err != nil {
		return e, err
	}
	if found && !now.Before(lease.session.RenewedAt) && now.Before(lease.session.ExpiresAt) {
		l := lease.session
		current := tracker.NativeLease{ID: l.ID, WorkItemID: issue.WorkItemID, MachineID: l.Machine.ID, SessionID: l.SessionID, FencingToken: l.FencingToken, AcquiredAt: l.AcquiredAt, RenewedAt: l.RenewedAt, ExpiresAt: l.ExpiresAt, ServerTime: now}
		if err := query.QueryRowContext(ctx, "SELECT coalesce((SELECT policy_id FROM lease_policies WHERE lease_id=?), '')", l.ID).Scan(&current.PolicyID); err != nil {
			return e, err
		}
		reservation, reserved, err := readProviderReservation(ctx, query, l.ID)
		if err != nil {
			return e, err
		}
		if reserved {
			current.ProviderReservation = &reservation
		}
		e.CurrentLease = &current
		e.Scheduling.Outcome = "claimed"
		e.Scheduling.Reason = "A current fenced lease owns this item"
	}
	approval, err := readProjectPolicy(ctx, query, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		var failure *nativeError
		if !errors.As(err, &failure) {
			return e, err
		}
		e.Unavailable = append(e.Unavailable, "approved_dispatch_policy")
		return e, nil
	}
	runners, truncated, err := readRuntimeRunners(ctx, query, scope, now)
	if err != nil {
		return e, err
	}
	if truncated {
		e.Unavailable = append(e.Unavailable, "capacity_projection_truncated")
	}
	for _, r := range runners {
		capacity := tracker.NativeRuntimeCapacity{ProviderCapacity: r.ProviderCapacity, RunnerID: r.RunnerID, ObservedAt: r.LastHeartbeatAt, Health: r.Health, Available: max(0, min(r.HostCapacity-r.HostUsed, min(r.CapacityLimit, r.ReportedCapacity)-r.Used)), Exclusions: []string{}}
		for _, exclusion := range r.Exclusions(scope.project, approval.Policy.Requirements, false) {
			capacity.Exclusions = append(capacity.Exclusions, exclusion.Code)
		}
		e.Capacity = append(e.Capacity, capacity)
	}
	if len(e.Capacity) == 0 {
		e.Unavailable = append(e.Unavailable, "runner_capacity")
	}
	if e.CurrentLease == nil {
		if err := readNativeAdmission(ctx, query, scope, id, approval.Policy.ID, approval.Policy.Requirements, runners, truncated, ready, &e, admission, minimumVersion, nonExecutableReason); err != nil {
			return e, err
		}
	}
	return e, nil
}

func latestNativeRuntimeEvent(ctx context.Context, q nativeQueryer, scope nativeScope, item, kind string) (tracker.CollaborationEvent, error) {
	e := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: tracker.NativeWorkItemID(item), AggregateType: "work_item"}
	var actor, data, at string
	err := q.QueryRowContext(ctx, "SELECT id, sequence, type, schema_version, actor_json, data_json, recorded_at FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type=? ORDER BY sequence DESC LIMIT 1", scope.organization, scope.project, item, kind).Scan(&e.ID, &e.AggregateSequence, &e.Type, &e.SchemaVersion, &actor, &data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.CollaborationEvent{}, nil
	}
	if err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if err := json.Unmarshal([]byte(actor), &e.Actor); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	e.RecordedAt, err = parseTimeValue(at)
	return e, err
}

func recordNativeSchedulingDecision(ctx context.Context, tx *sql.Tx, scope *nativeScope, id tracker.WorkItemID, ready bool, now time.Time) error {
	decision := tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "ready", Reason: "Current Change version is ready for native landing; runner capacity remains a separate condition"}
	if !ready {
		decision.Outcome = "skipped"
		decision.Reason = "Current Change version is not ready for native landing"
	}
	return recordNativeSchedulingOutcome(ctx, tx, scope, id, decision, now)
}

func recordNativeSchedulingOutcome(ctx context.Context, tx *sql.Tx, scope *nativeScope, id tracker.WorkItemID, decision tracker.NativeSchedulerDecision, now time.Time) error {
	if scope == nil || id <= 0 {
		return nil
	}
	var item string
	if err := tx.QueryRowContext(ctx, "SELECT native_id, revision FROM issues WHERE id=? AND organization_id=? AND project_id=?", id, scope.organization, scope.project).Scan(&item, &decision.WorkItemRevision); err != nil {
		return err
	}
	decision.At = now
	decision.RunnerID = scope.credential.Runner.RunnerID
	data := tracker.CollaborationData{Decision: &decision}
	change, found, err := readLatestNativeChangeRequest(ctx, tx, *scope, item)
	if err != nil {
		return err
	}
	if found {
		version, _, err := readNativeChangeVersion(ctx, tx, change)
		if err != nil {
			return err
		}
		data.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: change.CurrentVersion, HeadSHA: version.HeadSHA}
	}
	if decision.Outcome != "claimed" {
		var raw string
		err := tx.QueryRowContext(ctx, "SELECT data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision' AND json_extract(data_json, '$.decision.source')=? AND coalesce(json_extract(data_json, '$.decision.runner_id'), '')=? ORDER BY sequence DESC LIMIT 1", scope.organization, scope.project, item, decision.Source, decision.RunnerID).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var previous tracker.CollaborationData
			if err := json.Unmarshal([]byte(raw), &previous); err != nil {
				return err
			}
			if previous.Decision != nil {
				before, after := *previous.Decision, decision
				before.At, after.At = time.Time{}, time.Time{}
				if before == after && reflect.DeepEqual(previous.Change, data.Change) {
					return nil
				}
			}
		}
	}
	return appendNativeHistory(ctx, tx, *scope, item, "scheduler.decision", data, now)
}

func validNativeRESTCredential(identity string) bool {
	if digest, ok := strings.CutPrefix(identity, "github-rest:"); ok {
		if len(digest) != 12 {
			return false
		}
		_, err := hex.DecodeString(digest)
		return err == nil
	}
	if id, ok := strings.CutPrefix(identity, "github-app-installation:"); ok {
		number, err := strconv.ParseInt(id, 10, 64)
		return err == nil && number > 0
	}
	return false
}
