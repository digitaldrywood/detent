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
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func validateNativeRuntime(r *tracker.NativeRuntimeObservation) error {
	if r == nil {
		return nil
	}
	if launch := r.WorkerLaunch; launch != nil {
		digest, err := hex.DecodeString(launch.ExecutionSHA256)
		if err != nil || len(digest) != 32 || !validCommitID(launch.ExecutionCommit) ||
			len(launch.ExecutionVersion) == 0 || len(launch.ExecutionVersion) > 128 || len(launch.ControlVersion) == 0 || len(launch.ControlVersion) > 128 ||
			len(launch.PolicyID) == 0 || len(launch.PolicyID) > 128 || len(launch.PolicyRevision) > 256 ||
			launch.PID <= 0 || launch.GroupID < 0 || launch.Protocol <= 0 || launch.SelectedAt.IsZero() || launch.ProcessStart.IsZero() {
			return nativeRejection(nativePublicInvalid("Invalid execution worker launch identity"), "runtime_validation", "data.runtime.worker_launch")
		}
	}
	if len(r.Pipeline) > 1024 || r.PipelineDropped < 0 {
		return nativeRejection(nativePublicInvalid("Invalid pipeline timing population"), "runtime_validation", "data.runtime.pipeline")
	}
	for _, timing := range r.Pipeline {
		if !timing.Valid() {
			return nativeRejection(nativePublicInvalid("Invalid pipeline timing"), "runtime_validation", "data.runtime.pipeline")
		}
	}
	r.Pipeline = gate.PublicPipeline(r.Pipeline)
	if g := r.Validation; g != nil {
		commandInvalid := (strings.TrimSpace(g.Command) == "") || (len(g.Command) > 4096)
		failedOutputTooLarge := (g.ExitCode != 0) && (len(g.Output) > tracker.NativeFinalizationTextLimit)
		successfulOutputTooLarge := (g.ExitCode == 0) && (len(g.Output) > 64*1024)
		durationInvalid := g.DurationNS <= 0
		exitCodeInvalid := (g.ExitCode < 0) || (g.ExitCode > 255)
		failedStageInvalid := (g.ExitCode != 0) && (g.Stage != gate.StageSourceFinalization)
		stageInvalid := (g.Stage != "") && (g.Stage != gate.StageSourceFinalization)
		headInvalid := (!validCommitID(g.HeadSHA)) || (!validCommitID(g.TreeSHA))
		evidenceInvalid := !g.Evidence.Valid() || !g.ValidPipeline("finalization")
		if (commandInvalid) || (failedOutputTooLarge || successfulOutputTooLarge) || (durationInvalid) || (exitCodeInvalid) || (failedStageInvalid || stageInvalid) || (headInvalid) || (evidenceInvalid) {
			return nativeRejection(nativePublicInvalid("Invalid source validation evidence"), "runtime_validation", "data.runtime.validation")
		}
	}
	if r.Validation != nil && r.Validation.Pipeline != nil {
		r.Validation.Pipeline.Timings = gate.PublicPipeline(r.Validation.Pipeline.Timings)
	}
	if r.Completion != nil && (r.Completion.ObservedAt.IsZero() || r.Phase != "completed") {
		return nativeRejection(nativePublicInvalid("Host completion observations require a completed phase and observation time"), "runtime_validation", "data.runtime.completion")
	}
	if r.Completion != nil {
		r.Completion.Source = "host_native_completion"
		r.Completion.Coverage = "recorded_host_completion_branch_only; independent_of_provider_disposition_and_claim_release"
	}
	if !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed", "waiting_cache_admission", "waiting_cache_consumers"}, r.Phase) || r.LocalAttemptID < 0 || len(r.Phases) > 128 || r.PhasesDropped < 0 {
		return nativeRejection(nativePublicInvalid("Runtime observations require bounded phase and attempt identity"), "runtime_validation", "data.runtime")
	}
	for _, p := range r.Phases {
		if !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed", "waiting_cache_admission", "waiting_cache_consumers"}, p.Name) || p.StartedAt.IsZero() || !p.FinishedAt.IsZero() && p.FinishedAt.Before(p.StartedAt) {
			return nativeRejection(nativePublicInvalid("Invalid runtime phase interval"), "runtime_validation", "data.runtime.phases")
		}
	}
	for _, v := range []string{r.Identity.BackendID, r.Identity.BackendKind, r.Identity.Role, r.Identity.Provider.Value, r.Identity.RequestedModel.Value, r.Identity.ResolvedModel.Value, r.Identity.ReasoningEffort.Value, r.Identity.ServiceTier.Value} {
		if v != "" && !validExecutionName(v) {
			return nativeRejection(nativePublicInvalid("Invalid runtime identity"), "runtime_validation", "data.runtime.identity")
		}
	}
	r.Identity = r.Identity.Normalize()
	r.Identity.Route = ""
	r.Identity.Selection = agentidentity.Selection{}
	if r.Activity != nil {
		if r.Activity.Schema != 1 || r.Activity.AttemptID != r.LocalAttemptID || r.Activity.Generation != r.Generation || len(r.Activity.Spans) > 1024 || len(r.Activity.Sources) > 64 {
			return nativeRejection(nativePublicInvalid("Activity profile does not match its runtime observation"), "runtime_validation", "data.runtime.activity")
		}
		profile := workflowmetrics.PublicActivityProfile(*r.Activity)
		r.Activity = &profile
	}
	if l := r.Landing; l != nil {
		if len(l.ConflictFiles) > 1024 {
			return nativeRejection(nativePublicInvalid("Too many landing conflict files"), "runtime_validation", "data.runtime.landing")
		}
		for _, path := range l.ConflictFiles {
			if len(path) == 0 || len(path) > 4096 || strings.ContainsRune(path, 0) {
				return nativeRejection(nativePublicInvalid("Invalid landing conflict file"), "runtime_validation", "data.runtime.landing")
			}
		}
		if len(l.Pipeline) > 1024 || l.PipelineDropped < 0 {
			return nativeRejection(nativePublicInvalid("Invalid landing timing population"), "runtime_validation", "data.runtime.landing")
		}
		for _, timing := range l.Pipeline {
			if !timing.Valid() {
				return nativeRejection(nativePublicInvalid("Invalid landing timing"), "runtime_validation", "data.runtime.landing")
			}
		}
		l.Pipeline = gate.PublicPipeline(l.Pipeline)
		if l.Path != "" && !slices.Contains([]string{"clean_push", "batch_member", "rebase_short_validation", "rebase_push", "merge_resolution_pending", "merge_resolution", "merge_resolution_failed"}, l.Path) || len(l.Packages) > 1024 {
			return nativeRejection(nativePublicInvalid("Invalid landing validation scope"), "runtime_validation", "data.runtime.landing")
		}
		if l.Refusal != "" && l.Refusal != l.RefusalKind || l.FromState != "" || l.TargetState != "" {
			return nativeRejection(nativePublicInvalid("Landing state observations belong to the workflow owner"), "runtime_validation", "data.runtime.landing")
		}
		l.Refusal = l.RefusalKind
		if ci := l.CI; ci != nil {
			if ci.HeadSHA != l.HeadSHA || !validCommitID(ci.HeadSHA) || ci.PullRequest <= 0 || !slices.Contains([]string{"pending", "failure", "success"}, ci.State) || len(ci.RequiredChecks) > 256 || len(ci.PendingChecks) > 256 || len(ci.MissingChecks) > 256 || len(ci.FailedChecks) > 256 || len(ci.TriggerLabel) > 256 || l.Landed && ci.State != "success" {
				return nativeRejection(nativePublicInvalid("Invalid landing CI evidence"), "runtime_validation", "data.runtime.landing")
			}
		}
		if g := l.Gate; g != nil {
			if strings.TrimSpace(g.Command) == "" || len(g.Command) > 4096 || len(g.Output) > 64*1024 || g.DurationNS < 0 || g.ExitCode < -1 || !validCommitID(g.HeadSHA) || !validCommitID(g.TreeSHA) || !g.Evidence.Valid() || !g.ValidPipeline("landing_validation") || l.Landed && g.ExitCode != 0 || l.GateFailed && g.ExitCode == 0 {
				return nativeRejection(nativePublicInvalid("Invalid landing gate evidence"), "runtime_validation", "data.runtime.landing")
			}
		}
		if l.ChangeID != "" && !validNativeID(l.ChangeID, "change") || l.VersionID != "" && (l.ChangeID == "" || !validNativeID(l.VersionID, "version")) || l.HeadSHA != "" && !validCommitID(l.HeadSHA) {
			return nativeRejection(nativePublicInvalid("Invalid landing identity"), "runtime_validation", "data.runtime.landing")
		}
		if l.Gate != nil && l.Gate.Pipeline != nil {
			l.Gate.Pipeline.Timings = gate.PublicPipeline(l.Gate.Pipeline.Timings)
		}
		waitingCI := (l.Waiting || l.CI != nil && l.CI.State == "pending") && !l.GateFailed && l.RefusalKind == ""
		if l.Landed {
			if !validNativeID(l.ChangeID, "change") || !validNativeID(l.VersionID, "version") || !validCommitID(l.HeadSHA) || l.RefusalKind != "" || l.GateFailed || l.Waiting || !validCommitID(l.MergeSHA) || !slices.Contains([]string{"squash", "merge", "rebase"}, l.Method) || !validExecutionName(l.BaseRef) {
				return nativeRejection(nativePublicInvalid("Invalid landing receipt"), "runtime_validation", "data.runtime.landing")
			}
		} else if l.MergeSHA != "" || !waitingCI && (!l.GateFailed || l.RefusalKind != "") && !slices.Contains([]string{"head_moved", "missing_head", "conflict", "nothing_to_land", "base_protected", "base_moved"}, l.RefusalKind) {
			return nativeRejection(nativePublicInvalid("Invalid landing refusal"), "runtime_validation", "data.runtime.landing")
		}
	}
	if r.REST != nil {
		if len(r.REST.Windows) > 64 || len(r.REST.Divergences) > 64 || r.REST.Requests < 0 || r.REST.WindowsDropped < 0 || r.REST.DivergencesDropped < 0 {
			return nativeRejection(nativePublicInvalid("REST evidence exceeds its bound"), "runtime_validation", "data.runtime.rest")
		}
		r.REST.Source = "ordinary_response_headers"
		r.REST.Coverage = "selected_client_only; subprocesses_other_clients_hosts_and_account_consumer_attribution_unavailable"
		for i := range r.REST.Windows {
			w := &r.REST.Windows[i]
			if !validNativeRESTCredential(w.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, w.Resource) || !slices.Contains([]string{"search", "label issues", "repository issues", "issue comments", "issue field values", "issue dependencies", "issue reads", "pull requests", "reviews", "mutations", "workflow runs", "check run annotations", "check runs", "commit statuses", "other"}, w.EndpointFamily) || w.Requests < 0 {
				return nativeRejection(nativePublicInvalid("Invalid REST operation evidence"), "runtime_validation", "data.runtime.rest")
			}
			w.BudgetScope = "native_landing"
		}
		for _, d := range r.REST.Divergences {
			if !validNativeRESTCredential(d.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, d.Resource) || !slices.Contains([]string{"expected_shared_credential", "unattributed"}, d.Attribution) || d.ObservedRequests < 0 || d.DetentRequests < 0 || d.AttributedRequests < 0 || d.UnattributedRequests < 0 {
				return nativeRejection(nativePublicInvalid("Invalid REST attribution evidence"), "runtime_validation", "data.runtime.rest")
			}
		}
	}
	return nativeRejection(validateNativeGitHubScope(r.GitHub), "runtime_validation", "data.runtime.github")
}

func (s *Service) getNativeExplanation(c echo.Context) error {
	ctx := context.WithValue(c.Request().Context(), explanationReadKey{}, true)
	c.SetRequest(c.Request().WithContext(ctx))
	if len(c.QueryParams()) != 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Unsupported explanation selector"))
	}
	evidence, err := s.readNativeRuntime(ctx, nativeRequestScope(c), c.Param("item"), "")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := boundedNativeExplanation(evidence, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) getNativeRuntime(c echo.Context) error {
	for key, values := range c.QueryParams() {
		if key != "native_attempt_id" && key != "attempt_id" && key != "admission" || len(values) != 1 {
			return s.nativeAPIError(c, nativePublicInvalid("Unsupported runtime selector"))
		}
	}
	selector := c.QueryParam("native_attempt_id")
	if c.QueryParam("attempt_id") != "" {
		if selector != "" {
			return s.nativeAPIError(c, nativePublicInvalid("Select one attempt identity"))
		}
		selector = c.QueryParam("attempt_id")
	}
	var admission []tracker.NativeAdmissionContext
	if raw := c.QueryParam("admission"); raw != "" {
		var current tracker.NativeAdmissionContext
		if len(raw) > 8192 {
			return s.nativeAPIError(c, nativePublicInvalid("Admission context exceeds its bound"))
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&current); err != nil {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid admission context"))
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid admission context"))
		}
		if err := validateNativeAdmissionContext(current); err != nil {
			return s.nativeAPIError(c, err)
		}
		if nativeRequestScope(c).credential.Runner.RunnerID == "" {
			return s.nativeAPIError(c, nativePublicInvalid("Admission context requires the current registered runner"))
		}
		admission = append(admission, current)
	}
	result, err := s.readNativeRuntime(c.Request().Context(), nativeRequestScope(c), c.Param("item"), selector, admission...)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func (s *Service) readNativeRuntime(ctx context.Context, scope nativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
	watermark, _, err := readDashboardWatermark(ctx, query, scope)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	e := tracker.NativeRuntimeEvidence{ChangeSequence: watermark, Issue: issue, ObservedAt: now, Selection: "unavailable", Unavailable: []string{"efficiency_receipt"}, Capacity: []tracker.NativeRuntimeCapacity{}}
	sourceRecovery, _, err := readSourceRecoveryView(ctx, query, scope, item)
	if err != nil {
		return e, err
	}
	if sourceRecovery.AttemptID != "" || sourceRecovery.VersionID != "" {
		e.SourceRecovery = &sourceRecovery
	}

	if attemptID == "" {
		err = query.QueryRowContext(ctx, "SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT 1", scope.organization, scope.project, item).Scan(&attemptID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return e, err
		}
		e.Selection = "latest"
	} else if !validNativeID(attemptID, "attempt") {
		local, err := strconv.ParseInt(attemptID, 10, 64)
		if err != nil || local <= 0 {
			return e, nativePublicInvalid("Invalid attempt identity")
		}
		matches, err := readLocalAttemptMatches(ctx, query, scope, item, local)
		if err != nil {
			return e, err
		}
		if len(matches) == 0 {
			return e, nativeNotFound()
		}
		if len(matches) != 1 {
			return e, nativePublicInvalid("Local attempt identity is ambiguous; select the native attempt")
		}
		attemptID = matches[0]
		e.Selection = "selected"
	} else {
		e.Selection = "selected"
	}
	if attemptID != "" {
		query, err = archiveQuery(ctx, query, item, "run", attemptID, 0)
		if err != nil {
			return e, err
		}
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
		event, err := latestNativeRuntimeEvent(ctx, query, scope, item, kind, "")
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
	refusal, err := latestNativeRuntimeEvent(ctx, query, scope, item, "scheduler.decision", "skipped")
	if err != nil {
		return e, err
	}
	if refusal.ID != "" {
		e.NativeCandidateExclusion = &refusal
	}
	change, found, err := readLatestNativeChangeRequest(ctx, query, scope, item)
	if err != nil {
		return e, err
	}
	if found {
		detail, err := readRuntimeChangeDetail(ctx, query, scope, change, now)
		if err != nil {
			return e, err
		}
		for _, version := range detail.Versions {
			if version.ID == change.CurrentVersion {
				landing, err := readVersionLanding(ctx, query, version.ID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return e, err
				}
				if err == nil {
					version.Landing = &landing
				}
				e.CurrentVersion = &tracker.NativeChangeVersionEvidence{
					NativeChangeReference: tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA},
					PublishedAt:           version.CreatedAt, Validation: boundedChangeCommand(version.Validation), Landing: boundedChangeLanding(version.Landing),
				}
				break
			}
		}
		detail.Change.Body = ""
		detail.External = nil
		detail.Summary.Messages = nil
		if detail.Summary.Status == "evidence_incomplete" {
			e.Unavailable = append(e.Unavailable, "change_gate_history_truncated")
		}
		detail.Change.LinkedIssues = nil
		detail.Versions = nil
		detail.Discussion = nil
		detail.Reviews = nil
		detail.Checks = nil
		e.Change = &detail
	}
	if explanationRead(ctx) && e.Attempt != nil && e.Attempt.Runtime != nil && e.Attempt.Runtime.Landing != nil && e.CurrentVersion != nil {
		landing := e.Attempt.Runtime.Landing
		var generation int64
		if err := query.QueryRowContext(ctx, "SELECT dispatch_generation FROM issues WHERE id=?", id).Scan(&generation); err != nil {
			return e, err
		}
		if landing.Waiting && landing.ChangeID == e.CurrentVersion.ChangeID && landing.VersionID == e.CurrentVersion.VersionID && landing.HeadSHA == e.CurrentVersion.HeadSHA && e.Attempt.WorkItemRevision >= issue.Revision && e.Attempt.DispatchGeneration >= generation {
			issue.ListChange = &tracker.NativeListChange{LandingPending: true, HeadSHA: e.CurrentVersion.HeadSHA}
			e.Issue.ListChange = issue.ListChange
		}
	}
	ready := nativeChangeLandingClaimable(issue.State, e.Change)
	if issue.ListChange != nil && issue.ListChange.LandingPending {
		ready = nativeChangeLandingClaimable("Merging", e.Change)
	}
	e.Scheduling = tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner-specific selectors, policy and capacity must also permit this item"}
	if !ready && !slices.Contains(e.Unavailable, "change_gate_history_truncated") {
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
	approval, found, err := readProjectPolicy(ctx, query, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return e, err
	}
	if !found {
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
		capacity := tracker.NativeRuntimeCapacity{ProviderCapacity: r.ProviderCapacity, RunnerID: r.RunnerID, ObservedAt: r.LastHeartbeatAt, Health: r.Health, Available: max(0, min(r.HostCapacity-r.HostUsed, r.RemainingCapacity(scope.project))), Exclusions: []string{}}
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

func latestNativeRuntimeEvent(ctx context.Context, q nativeQueryer, scope nativeScope, item, kind, outcome string) (tracker.CollaborationEvent, error) {
	e := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: tracker.NativeWorkItemID(item), AggregateType: "work_item"}
	var actor, data, at string
	statement := "SELECT id, sequence, type, schema_version, actor_json, " + collaborationPayloadColumn("") + ", recorded_at,payload_version,scheduler_present,scheduler_outcome,scheduler_reason,scheduler_source,scheduler_runner_id,scheduler_at,scheduler_revision,scheduler_change_id,scheduler_version_id,scheduler_head_sha,scheduler_change_present,event_revision FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type=?"
	args := []any{scope.organization, scope.project, item, kind}
	if outcome != "" && explanationRead(ctx) {
		base := statement
		statement = "SELECT * FROM (" + base + " AND scheduler_outcome=? ORDER BY sequence DESC LIMIT 1) UNION ALL SELECT * FROM (" + base + " AND scheduler_outcome IS NULL ORDER BY sequence DESC LIMIT 64) ORDER BY sequence DESC"
		args = append(args, outcome, scope.organization, scope.project, item, kind)
	} else {
		if outcome != "" {
			statement += " AND (scheduler_outcome=? OR scheduler_outcome IS NULL)"
			args = append(args, outcome)
		}
		statement += " ORDER BY sequence DESC"
		statement += " LIMIT 64"
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return tracker.CollaborationEvent{}, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var version int
		var present, changePresent sql.NullBool
		var source, reason, decisionOutcome, runner, decisionAt sql.NullString
		var revision, eventRevision sql.NullInt64
		var change tracker.NativeChangeReference
		if err := rows.Scan(&e.ID, &e.AggregateSequence, &e.Type, &e.SchemaVersion, &actor, &data, &at, &version, &present, &decisionOutcome, &reason, &source, &runner, &decisionAt, &revision, &change.ChangeID, &change.VersionID, &change.HeadSHA, &changePresent, &eventRevision); err != nil {
			return tracker.CollaborationEvent{}, err
		}
		if version == 1 && e.Type == "scheduler.decision" {
			projected := tracker.CollaborationData{Revision: tracker.Revision(eventRevision.Int64)}
			if present.Bool {
				decision := tracker.NativeSchedulerDecision{Outcome: decisionOutcome.String, Reason: reason.String, Source: source.String, RunnerID: runner.String, WorkItemRevision: tracker.Revision(revision.Int64)}
				if decisionAt.String != "" {
					decision.At, err = parseTimeValue(decisionAt.String)
					if err != nil {
						return tracker.CollaborationEvent{}, err
					}
				}
				projected.Decision = &decision
			}
			if changePresent.Bool {
				projected.Change = &change
			}
			data, err = marshalNative(projected)
			if err != nil {
				return tracker.CollaborationEvent{}, err
			}
		}
		if outcome != "" {
			var projection tracker.CollaborationData
			if err := decodeRunHistory(ctx, q, data, &projection); err != nil {
				return tracker.CollaborationEvent{}, err
			}
			if projection.Decision == nil || projection.Decision.Outcome != outcome {
				continue
			}
		}
		found = true
		break
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if !found {
		return tracker.CollaborationEvent{}, nil
	}
	if err := json.Unmarshal([]byte(actor), &e.Actor); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	if err := decodeRunHistory(ctx, q, data, &e.Data); err != nil {
		return tracker.CollaborationEvent{}, err
	}
	e.RecordedAt, err = parseTimeValue(at)
	return e, err
}

func recordNativeSchedulingDecision(ctx context.Context, tx *sql.Tx, scope *nativeScope, id tracker.WorkItemID, ready bool, capacity int, now time.Time) error {
	decision := tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "ready", Reason: "Current Change version can be claimed for native landing or evidence recovery; runner capacity remains a separate condition"}
	if !ready {
		decision.Outcome = "skipped"
		decision.Reason = "Current Change version is not ready for native landing"
		switch {
		case capacity == nativeLandingCapacityUnknown:
			decision.Reason = "Project landing capacity cannot be determined without the incoming ceiling"
		case capacity > 0:
			decision.Reason = "Project landing capacity is full"
		}
	}
	return recordNativeSchedulingOutcome(ctx, tx, scope, id, decision, now)
}

func (d *database) recordNativeClaimRefusal(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, id tracker.WorkItemID, source string, refusal error, now time.Time) error {
	var failure *nativeError
	if errors.Is(refusal, ErrNoClaimableWork) {
		failure = &nativeError{Code: "no_claimable_work", status: http.StatusConflict}
	} else if !errors.As(refusal, &failure) {
		return refusal
	}
	if query.NativeScope == nil || failure.status != http.StatusConflict {
		return refusal
	}
	if prepared := claimPrepared(ctx); prepared != nil && claimWriting(ctx) && id == 0 {
		id = prepared.selected
		if id == 0 {
			return refusal
		}
	}
	ids := []tracker.WorkItemID{id}
	if id == 0 {
		query.AvailableAt = now
		query.Limit = claimPreparationLimit
		var err error
		ids, err = nativeCandidateIDs(ctx, tx, query, query.RepositoryIDs, normalizedQueryStrings(query.Repositories), normalizedQueryStrings(query.WorkflowStates), normalizedQueryStrings(query.Authors), normalizedQueryStrings(query.Assignees), normalizedQueryStrings(query.LabelInclude), normalizedQueryStrings(query.LabelExclude))
		if err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, id, tracker.NativeSchedulerDecision{Source: source, Outcome: "skipped", Reason: failure.Code}, now); err != nil {
			return err
		}
	}
	if prepared := claimPrepared(ctx); prepared != nil && !claimWriting(ctx) {
		return refusal
	}
	if err := d.commit(ctx, tx); err != nil {
		return err
	}
	return refusal
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
	reference, err := readClaimReference(ctx, tx, *scope, item)
	if err != nil {
		return err
	}
	if reference.ChangeID != "" {
		data.Change = &reference
	}
	if decision.Outcome != "claimed" {
		raw, err := previousSchedulingDecision(ctx, tx, *scope, item, decision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var previous tracker.CollaborationData
			if err := decodeRunHistory(ctx, tx, raw, &previous); err != nil {
				return err
			}
			if previous.Decision != nil {
				before, after := *previous.Decision, decision
				before.At, after.At = time.Time{}, time.Time{}
				if decision.Outcome == "skipped" {
					before.Source = after.Source
				}
				if before == after && reflect.DeepEqual(previous.Change, data.Change) {
					return nil
				}
			}
		}
	}
	if prepared := claimPrepared(ctx); prepared != nil && !claimWriting(ctx) {
		if len(prepared.decisions) < claimPreparationLimit {
			prepared.decisions = append(prepared.decisions, preparedClaimDecision{scope: *scope, id: id, item: item, data: data})
		}
		return nil
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
