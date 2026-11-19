package executionapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type RuntimeDeps struct {
	Repository           tenantstore.Repository
	Now                  func() time.Time
	HTTPError            func(echo.Context, error, ...any) error
	AuthorizeAccess      func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	DetailContext        func(ctx context.Context) context.Context
	RequestDetails       func(ctx context.Context) context.Context
	DetailsFor           func(ctx context.Context) attachment.DetailStore
	HydrateAttempt       func(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error)
	HydrateChange        func(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error)
	HydrateWorkItem      func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	HydrateRelationships func(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error
	ReadAdmission        func(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, evidence *tracker.NativeRuntimeEvidence, supplied []tracker.NativeAdmissionContext) error
	ReadSourceRecovery   func(ctx context.Context, scope hubscope.NativeScope, item string, evidence *tracker.NativeRuntimeEvidence) error
	ReadWorkItem         func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	AttachChange         func(result *tracker.NativeRuntimeEvidence, detail tracker.ChangeDetail)
	Explain              func(evidence tracker.NativeRuntimeEvidence, failure *explain.FailureSignature) (explain.IssueExplanation, error)
	ArchiveJSON          func(c echo.Context, status int, value any) error
	ReadOperatorEvidence func(context.Context, hubscope.NativeScope, tracker.NativeIssue, bool) (tracker.NativeRuntimeEvidence, error)
}

func ValidateNativeRuntime(nativeRejection func(error, string, string) error, validateNativeGitHubScope func(*tracker.NativeGitHubScope) error, r *tracker.NativeRuntimeObservation) error {
	if r == nil {
		return nil
	}
	if launch := r.WorkerLaunch; launch != nil {
		digest, err := hex.DecodeString(launch.ExecutionSHA256)
		if err != nil || len(digest) != 32 || !hubapi.ValidCommitID(launch.ExecutionCommit) ||
			len(launch.ExecutionVersion) == 0 || len(launch.ExecutionVersion) > 128 || len(launch.ControlVersion) == 0 || len(launch.ControlVersion) > 128 ||
			len(launch.PolicyID) == 0 || len(launch.PolicyID) > 128 || len(launch.PolicyRevision) > 256 ||
			launch.PID <= 0 || launch.GroupID < 0 || launch.Protocol <= 0 || launch.SelectedAt.IsZero() || launch.ProcessStart.IsZero() {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid execution worker launch identity"), "runtime_validation", "data.runtime.worker_launch")
		}
	}
	if len(r.Pipeline) > 1024 || r.PipelineDropped < 0 {
		return nativeRejection(hubapi.NativePublicInvalid("Invalid pipeline timing population"), "runtime_validation", "data.runtime.pipeline")
	}
	for _, timing := range r.Pipeline {
		if !timing.Valid() {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid pipeline timing"), "runtime_validation", "data.runtime.pipeline")
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
		headInvalid := (!hubapi.ValidCommitID(g.HeadSHA)) || (!hubapi.ValidCommitID(g.TreeSHA))
		evidenceInvalid := !g.Evidence.Valid() || !g.ValidPipeline("finalization")
		if (commandInvalid) || (failedOutputTooLarge || successfulOutputTooLarge) || (durationInvalid) || (exitCodeInvalid) || (failedStageInvalid || stageInvalid) || (headInvalid) || (evidenceInvalid) {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid source validation evidence"), "runtime_validation", "data.runtime.validation")
		}
	}
	if r.Validation != nil && r.Validation.Pipeline != nil {
		r.Validation.Pipeline.Timings = gate.PublicPipeline(r.Validation.Pipeline.Timings)
	}
	if r.Completion != nil && (r.Completion.ObservedAt.IsZero() || r.Phase != "completed") {
		return nativeRejection(hubapi.NativePublicInvalid("Host completion observations require a completed phase and observation time"), "runtime_validation", "data.runtime.completion")
	}
	if r.Completion != nil {
		r.Completion.Source = "host_native_completion"
		r.Completion.Coverage = "recorded_host_completion_branch_only; independent_of_provider_disposition_and_claim_release"
	}
	if !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed", "waiting_cache_admission", "waiting_cache_consumers"}, r.Phase) || r.LocalAttemptID < 0 || len(r.Phases) > 128 || r.PhasesDropped < 0 {
		return nativeRejection(hubapi.NativePublicInvalid("Runtime observations require bounded phase and attempt identity"), "runtime_validation", "data.runtime")
	}
	for i, p := range r.Phases {
		failureReason := ""
		switch {
		case !slices.Contains([]string{"implementation", "rework", "planning", "merging", "validation", "completed", "waiting_cache_admission", "waiting_cache_consumers"}, p.Name):
			failureReason = "invalid_name"
		case p.StartedAt.IsZero():
			failureReason = "missing_start"
		case !p.FinishedAt.IsZero() && p.FinishedAt.Before(p.StartedAt):
			failureReason = "finish_before_start"
		}
		if failureReason != "" {
			err := nativeRejection(hubapi.NativePublicInvalid("Invalid runtime phase interval"), "runtime_validation", "data.runtime.phases")
			var failure *hubapi.NativeError
			if errors.As(err, &failure) {
				failure.LogAttributes = append(failure.LogAttributes, "phase_index", i, "phase_failure", failureReason)
			}
			return err
		}
	}
	for _, v := range []string{r.Identity.BackendID, r.Identity.BackendKind, r.Identity.Role, r.Identity.Provider.Value, r.Identity.RequestedModel.Value, r.Identity.ResolvedModel.Value, r.Identity.ReasoningEffort.Value, r.Identity.ServiceTier.Value} {
		if v != "" && !ValidExecutionName(v) {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid runtime identity"), "runtime_validation", "data.runtime.identity")
		}
	}
	r.Identity = r.Identity.Normalize()
	r.Identity.Route = ""
	r.Identity.Selection = agentidentity.Selection{}
	if r.Activity != nil {
		if r.Activity.Schema != 1 || r.Activity.AttemptID != r.LocalAttemptID || r.Activity.Generation != r.Generation || len(r.Activity.Spans) > 1024 || len(r.Activity.Sources) > 64 {
			return nativeRejection(hubapi.NativePublicInvalid("Activity profile does not match its runtime observation"), "runtime_validation", "data.runtime.activity")
		}
		profile := workflowmetrics.PublicActivityProfile(*r.Activity)
		r.Activity = &profile
	}
	if l := r.Landing; l != nil {
		if len(l.ConflictFiles) > 1024 {
			return nativeRejection(hubapi.NativePublicInvalid("Too many landing conflict files"), "runtime_validation", "data.runtime.landing")
		}
		for _, path := range l.ConflictFiles {
			if len(path) == 0 || len(path) > 4096 || strings.ContainsRune(path, 0) {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid landing conflict file"), "runtime_validation", "data.runtime.landing")
			}
		}
		if len(l.Pipeline) > 1024 || l.PipelineDropped < 0 {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid landing timing population"), "runtime_validation", "data.runtime.landing")
		}
		for _, timing := range l.Pipeline {
			if !timing.Valid() {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid landing timing"), "runtime_validation", "data.runtime.landing")
			}
		}
		l.Pipeline = gate.PublicPipeline(l.Pipeline)
		if l.Path != "" && !slices.Contains([]string{"clean_push", "batch_member", "rebase_short_validation", "rebase_push", "merge_resolution_pending", "merge_resolution", "merge_resolution_failed"}, l.Path) || len(l.Packages) > 1024 {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid landing validation scope"), "runtime_validation", "data.runtime.landing")
		}
		if l.Refusal != "" && l.Refusal != l.RefusalKind || l.FromState != "" || l.TargetState != "" {
			return nativeRejection(hubapi.NativePublicInvalid("Landing state observations belong to the workflow owner"), "runtime_validation", "data.runtime.landing")
		}
		l.Refusal = l.RefusalKind
		if ci := l.CI; ci != nil {
			if ci.HeadSHA != l.HeadSHA || !hubapi.ValidCommitID(ci.HeadSHA) || ci.PullRequest <= 0 || !slices.Contains([]string{"pending", "failure", "success"}, ci.State) || len(ci.RequiredChecks) > 256 || len(ci.PendingChecks) > 256 || len(ci.MissingChecks) > 256 || len(ci.FailedChecks) > 256 || len(ci.TriggerLabel) > 256 || l.Landed && ci.State != "success" {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid landing CI evidence"), "runtime_validation", "data.runtime.landing")
			}
		}
		if g := l.Gate; g != nil {
			if strings.TrimSpace(g.Command) == "" || len(g.Command) > 4096 || len(g.Output) > 64*1024 || g.DurationNS < 0 || g.ExitCode < -1 || !hubapi.ValidCommitID(g.HeadSHA) || !hubapi.ValidCommitID(g.TreeSHA) || !g.Evidence.Valid() || !g.ValidPipeline("landing_validation") || l.Landed && g.ExitCode != 0 || l.GateFailed && g.ExitCode == 0 {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid landing gate evidence"), "runtime_validation", "data.runtime.landing")
			}
		}
		if l.ChangeID != "" && !hubapi.ValidID(l.ChangeID, "change") || l.VersionID != "" && (l.ChangeID == "" || !hubapi.ValidID(l.VersionID, "version")) || l.HeadSHA != "" && !hubapi.ValidCommitID(l.HeadSHA) {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid landing identity"), "runtime_validation", "data.runtime.landing")
		}
		if l.Gate != nil && l.Gate.Pipeline != nil {
			l.Gate.Pipeline.Timings = gate.PublicPipeline(l.Gate.Pipeline.Timings)
		}
		waitingCI := (l.Waiting || l.CI != nil && l.CI.State == "pending") && !l.GateFailed && l.RefusalKind == ""
		if l.Landed {
			if !hubapi.ValidID(l.ChangeID, "change") || !hubapi.ValidID(l.VersionID, "version") || !hubapi.ValidCommitID(l.HeadSHA) || l.RefusalKind != "" || l.GateFailed || l.Waiting || !hubapi.ValidCommitID(l.MergeSHA) || !slices.Contains([]string{"squash", "merge", "rebase"}, l.Method) || !ValidExecutionName(l.BaseRef) {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid landing receipt"), "runtime_validation", "data.runtime.landing")
			}
		} else if l.MergeSHA != "" || !waitingCI && (!l.GateFailed || l.RefusalKind != "") && !slices.Contains([]string{"head_moved", "missing_head", "conflict", "nothing_to_land", "base_protected", "base_moved"}, l.RefusalKind) {
			return nativeRejection(hubapi.NativePublicInvalid("Invalid landing refusal"), "runtime_validation", "data.runtime.landing")
		}
	}
	if r.REST != nil {
		if len(r.REST.Windows) > 64 || len(r.REST.Divergences) > 64 || r.REST.Requests < 0 || r.REST.WindowsDropped < 0 || r.REST.DivergencesDropped < 0 {
			return nativeRejection(hubapi.NativePublicInvalid("REST evidence exceeds its bound"), "runtime_validation", "data.runtime.rest")
		}
		r.REST.Source = "ordinary_response_headers"
		r.REST.Coverage = "selected_client_only; subprocesses_other_clients_hosts_and_account_consumer_attribution_unavailable"
		for i := range r.REST.Windows {
			w := &r.REST.Windows[i]
			if !validNativeRESTCredential(w.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, w.Resource) || !slices.Contains([]string{"search", "label issues", "repository issues", "issue comments", "issue field values", "issue dependencies", "issue reads", "pull requests", "reviews", "mutations", "workflow runs", "check run annotations", "check runs", "commit statuses", "other"}, w.EndpointFamily) || w.Requests < 0 {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid REST operation evidence"), "runtime_validation", "data.runtime.rest")
			}
			w.BudgetScope = "native_landing"
		}
		for _, d := range r.REST.Divergences {
			if !validNativeRESTCredential(d.CredentialIdentity) || !slices.Contains([]string{"core", "search", "graphql", "integration_manifest", "unknown", ""}, d.Resource) || !slices.Contains([]string{"expected_shared_credential", "unattributed"}, d.Attribution) || d.ObservedRequests < 0 || d.DetentRequests < 0 || d.AttributedRequests < 0 || d.UnattributedRequests < 0 {
				return nativeRejection(hubapi.NativePublicInvalid("Invalid REST attribution evidence"), "runtime_validation", "data.runtime.rest")
			}
		}
	}
	return nativeRejection(validateNativeGitHubScope(r.GitHub), "runtime_validation", "data.runtime.github")
}

func (d RuntimeDeps) GetNativeExplanation(c echo.Context) error {
	ctx := context.WithValue(c.Request().Context(), hubcontent.ExplanationReadKey{}, true)
	c.SetRequest(c.Request().WithContext(ctx))
	if len(c.QueryParams()) != 0 {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Unsupported explanation selector"))
	}
	evidence, err := d.ReadNativeRuntime(ctx, hubscope.NativeRequestScope(c), c.Param("item"), "")
	if err != nil {
		return d.HTTPError(c, err)
	}
	result, err := d.Explain(evidence, nil)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (d RuntimeDeps) GetNativeRuntime(c echo.Context) error {
	for key, values := range c.QueryParams() {
		if key != "native_attempt_id" && key != "attempt_id" && key != "admission" && key != "evidence_after" || len(values) != 1 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Unsupported runtime selector"))
		}
	}
	selector := c.QueryParam("native_attempt_id")
	if c.QueryParam("attempt_id") != "" {
		if selector != "" {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Select one attempt identity"))
		}
		selector = c.QueryParam("attempt_id")
	}
	if c.QueryParams().Has("evidence_after") {
		after := c.QueryParam("evidence_after")
		if after != "" && after != "start" && !hubapi.ValidID(after, "att") || !hubapi.ValidID(selector, "attempt") || c.QueryParams().Has("admission") {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid attempt evidence cursor"))
		}
		ctx, cancel := context.WithTimeout(d.RequestDetails(d.DetailContext(c.Request().Context())), hubapi.ReadTimeout)
		defer cancel()
		evidence, _, _, err := d.ReadSelectedNativeRuntimeTenant(ctx, hubscope.NativeRequestScope(c), c.Param("item"), RuntimeAttemptSelector{AttemptID: selector, EvidenceAfter: after, FirstEvidencePage: after == ""})
		if err != nil {
			return d.HTTPError(c, err)
		}
		return d.ArchiveJSON(c, http.StatusOK, evidence)
	}
	var admission []tracker.NativeAdmissionContext
	if raw := c.QueryParam("admission"); raw != "" {
		var current tracker.NativeAdmissionContext
		if len(raw) > 8192 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Admission context exceeds its bound"))
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&current); err != nil {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid admission context"))
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Invalid admission context"))
		}
		if err := ValidateNativeAdmissionContext(current); err != nil {
			return d.HTTPError(c, err)
		}
		if hubscope.NativeRequestScope(c).Credential.Runner.RunnerID == "" {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Admission context requires the current registered runner"))
		}
		admission = append(admission, current)
	}
	result, err := d.ReadNativeRuntime(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), selector, admission...)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return d.ArchiveJSON(c, http.StatusOK, result)
}

func (d RuntimeDeps) ReadNativeRuntime(ctx context.Context, scope hubscope.NativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	ctx = d.RequestDetails(d.DetailContext(ctx))
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return d.ReadNativeRuntimeTenant(ctx, scope, item, attemptID, admission...)
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
