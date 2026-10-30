package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeRunEventRejectionsLogged(t *testing.T) {
	var logs bytes.Buffer
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}))
	runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	publishCapacity(t, f.nativeFixture, runner, capacityReport(time.Now().UTC()))
	worker := runner.redemption.Credential
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, providerClaim(runner, f.issue, "session"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.ProviderReservation == nil {
		t.Fatal("claim has no provider reservation")
	}
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, start), http.StatusOK)
	version := f.publish(t, "version", "")
	now := time.Now().UTC()
	finalization := func() *tracker.NativeFinalization {
		return &tracker.NativeFinalization{ObservedAt: now, ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
	}
	landing := func() *tracker.NativeRuntimeObservation {
		return &tracker.NativeRuntimeObservation{Phase: "completed", Landing: &tracker.NativeLandingReceipt{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Waiting: true}}
	}
	checkpoint := func(e *tracker.NativeRunEvent) {
		e.Type, e.Data.Outcome, e.Data.CompletionBody = "run.checkpointed", "", ""
		e.Data.Handoff = &tracker.NativeCheckpoint{Resume: "resume_session", Availability: "available", Storage: "local_only", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}
	}
	usage := func(e *tracker.NativeRunEvent) *tracker.NativeUsage {
		e.Data.Usage = []tracker.NativeUsage{{Provider: "sentinel-private", Model: "test-model"}}
		return &e.Data.Usage[0]
	}
	assertRefusal := func(t *testing.T, response *httptest.ResponseRecorder, want tracker.PublicationRejection, attempt, reason string) {
		t.Helper()
		var failure struct {
			Code, Message string
			Details       struct{ Rejection tracker.PublicationRejection }
		}
		decodeHubResponse(t, response, &failure)
		if failure.Code != "invalid_request" || failure.Message == "" || strings.Contains(failure.Message, "unavailable") || failure.Details.Rejection != want {
			t.Fatalf("refusal = %s", response.Body.String())
		}
		if reason != "" && failure.Message != reason {
			t.Fatalf("refusal reason = %q, want %q", failure.Message, reason)
		}
		warnings := 0
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var entry map[string]any
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal(err)
			}
			if entry["level"] != "WARN" {
				continue
			}
			if entry["reason"] == "" || reason != "" && entry["reason"] != reason {
				t.Fatalf("logged reason = %q, want a nonempty reason matching %q when specified", entry["reason"], reason)
			}
			warnings++
			for key, value := range map[string]any{"code": "invalid_request", "category": want.Category, "field": want.Field, "route": nativeBase + "/work-items/:item/events", "organization": string(f.project.OrganizationID), "project": string(f.project.ID), "work_item": string(f.issue.WorkItemID), "attempt": attempt} {
				if entry[key] != value {
					t.Errorf("%s = %v, want %v", key, entry[key], value)
				}
			}
		}
		if warnings != 1 || strings.Contains(logs.String()+response.Body.String(), "sentinel-private") {
			t.Fatalf("unsafe or missing rejection log: %s", logs.String())
		}
	}
	for _, test := range []struct {
		name, category, field, reason string
		edit                          func(*tracker.NativeRunEvent)
	}{
		{"oversized step", "event_validation", "data.steps.validator", "Execution step receipt exceeds its bounded payload", func(e *tracker.NativeRunEvent) {
			e.Data.Steps = []tracker.NativeStepReceipt{{Step: "validator", PolicyID: e.Data.PolicyID, ReviewState: strings.Repeat("a", 64), Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("b", 40), WorkspaceDigest: "digest"}, CompletedAt: now, Validator: &gate.ValidatorResult{Summary: strings.Repeat("sentinel-private", 20000)}}}
		}},
		{"idempotency", "event_validation", "", "", func(e *tracker.NativeRunEvent) { e.IdempotencyKey = "" }},
		{"schema", "event_validation", "", "", func(e *tracker.NativeRunEvent) { e.SchemaVersion = 2 }},
		{"type", "event_validation", "", "", func(e *tracker.NativeRunEvent) { e.Type = "sentinel-private" }},
		{"observation without runtime", "event_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Type, e.Data.Outcome = "run.observed", "" }},
		{"unordered runtime", "event_validation", "data", "", func(e *tracker.NativeRunEvent) {
			e.Data.Sequence = 0
			e.Data.Runtime = &tracker.NativeRuntimeObservation{}
		}},
		{"references", "event_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Data.RunID = "sentinel-private" }},
		{"outcome", "event_validation", "data.outcome", "", func(e *tracker.NativeRunEvent) { e.Data.Outcome = "sentinel-private" }},
		{"nonterminal outcome", "event_validation", "data.outcome", "", func(e *tracker.NativeRunEvent) { e.Type = "run.started" }},
		{"terminal artifacts", "event_validation", "data.artifact_ids", "", func(e *tracker.NativeRunEvent) { e.Data.ArtifactIDs = []string{newNativeID("artifact")} }},
		{"invalid artifact", "event_validation", "data.artifact_ids", "", func(e *tracker.NativeRunEvent) {
			e.Type, e.Data.Outcome = "run.checkpointed", ""
			e.Data.ArtifactIDs = []string{"sentinel-private"}
		}},
		{"usage", "usage_validation", "data.usage", "", func(e *tracker.NativeRunEvent) { e.Data.Usage = []tracker.NativeUsage{{Input: -1}} }},
		{"usage on start", "usage_validation", "data.usage", "Usage requires a checkpoint or a finished run", func(e *tracker.NativeRunEvent) {
			usage(e)
			e.Type, e.Data.Outcome, e.Data.CompletionBody = "run.started", "", ""
		}},
		{"usage population", "usage_validation", "data.usage", "A run event carries at most 32 usage entries", func(e *tracker.NativeRunEvent) {
			e.Data.Usage = make([]tracker.NativeUsage, maxNativeUsageEntries+1)
		}},
		{"usage duplicate", "usage_validation", "data.usage", "Usage entries must name distinct provider and model pairs", func(e *tracker.NativeRunEvent) {
			usage(e)
			e.Data.Usage = append(e.Data.Usage, e.Data.Usage[0])
		}},
		{"usage counts", "usage_validation", "data.usage", "Usage counts and cost cannot be negative", func(e *tracker.NativeRunEvent) {
			usage(e).Input = -1
		}},
		{"usage cost bound", "usage_validation", "data.usage", "Usage cost must be finite and bounded", func(e *tracker.NativeRunEvent) {
			usage(e).CostEstimate = 1e9 + 1
		}},
		{"usage billing mode", "usage_validation", "data.usage", "Usage billing mode must be metered, subscription or unknown", func(e *tracker.NativeRunEvent) {
			usage(e).BillingMode = "sentinel-private"
		}},
		{"usage kind", "usage_validation", "data.usage", "Usage kind must be cumulative or incremental", func(e *tracker.NativeRunEvent) {
			usage(e).UsageKind = "sentinel-private"
		}},
		{"usage incremental identity", "usage_validation", "data.usage", "Incremental usage requires a source, revision and execution interval", func(e *tracker.NativeRunEvent) {
			usage(e).UsageKind = "incremental"
		}},
		{"usage cumulative source", "usage_validation", "data.usage", "Cumulative usage uses the attempt's canonical source", func(e *tracker.NativeRunEvent) {
			usage(e).SourceID = "sentinel-private"
		}},
		{"usage source bounds", "usage_validation", "data.usage", "Usage requires a bounded source and nonnegative revision", func(e *tracker.NativeRunEvent) {
			usage(e).Revision = -1
		}},
		{"usage cost coverage", "usage_validation", "data.usage", "Reported cost coverage must be complete or partial", func(e *tracker.NativeRunEvent) {
			usage(e).CostCoverage = "sentinel-private"
		}},
		{"usage reported cost", "usage_validation", "data.usage", "Reported cost requires a nonnegative bounded amount and explicit runner provenance", func(e *tracker.NativeRunEvent) {
			u := usage(e)
			u.ReportedCostMicros, u.CostSource = new(int64(-1)), "runner_report"
		}},
		{"usage currency", "usage_validation", "data.usage", "Reported cost requires a three-letter uppercase currency", func(e *tracker.NativeRunEvent) {
			u := usage(e)
			u.ReportedCostMicros, u.CostSource, u.Currency = new(int64(1)), "runner_report", "sentinel-private"
		}},
		{"usage cost provenance", "usage_validation", "data.usage", "Cost provenance requires a reported amount", func(e *tracker.NativeRunEvent) {
			usage(e).CostSource = "runner_report"
		}},
		{"runtime phase", "runtime_validation", "data.runtime", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "sentinel-private"}
		}},
		{"runtime launch", "runtime_validation", "data.runtime.worker_launch", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{}}
		}},
		{"runtime launch policy", "execution_validation", "data.runtime.worker_launch.policy_id", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", WorkerLaunch: &tracker.NativeWorkerLaunch{
				PID: 1, ProcessStart: now, ExecutionVersion: "test", ExecutionCommit: strings.Repeat("a", 40),
				ExecutionSHA256: strings.Repeat("b", 64), ControlVersion: "test", PolicyID: newNativeID("policy"),
				Protocol: 1, SelectedAt: now,
			}}
		}},
		{"runtime pipeline", "runtime_validation", "data.runtime.pipeline", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{PipelineDropped: -1}
		}},
		{"runtime pipeline timing", "runtime_validation", "data.runtime.pipeline", "Invalid pipeline timing", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Pipeline: []gate.PipelineTiming{{Command: "sentinel-private"}}}
		}},
		{"runtime validation", "runtime_validation", "data.runtime.validation", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{Command: "sentinel-private"}}
		}},
		{"runtime completion", "runtime_validation", "data.runtime.completion", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{}}
		}},
		{"runtime phases", "runtime_validation", "data.runtime.phases", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Phases: []tracker.NativePhase{{Name: "sentinel-private"}}}
		}},
		{"runtime identity", "runtime_validation", "data.runtime.identity", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Identity: agentidentity.Identity{BackendID: "sentinel-private\ninvalid"}}
		}},
		{"runtime activity", "runtime_validation", "data.runtime.activity", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Activity: &workflowmetrics.ActivityProfile{Schema: 2}}
		}},
		{"runtime landing", "runtime_validation", "data.runtime.landing", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.HeadSHA = "sentinel-private"
		}},
		{"landing timing bounds", "runtime_validation", "data.runtime.landing", "Invalid landing timing population", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.PipelineDropped = -1
		}},
		{"landing timing", "runtime_validation", "data.runtime.landing", "Invalid landing timing", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.Pipeline = []gate.PipelineTiming{{Command: "sentinel-private"}}
		}},
		{"landing validation scope", "runtime_validation", "data.runtime.landing", "Invalid landing validation scope", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.Path = "sentinel-private"
		}},
		{"landing workflow state", "runtime_validation", "data.runtime.landing", "Landing state observations belong to the workflow owner", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.TargetState = "sentinel-private"
		}},
		{"landing CI", "runtime_validation", "data.runtime.landing", "Invalid landing CI evidence", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.CI = &tracker.NativeLandingCIReceipt{HeadSHA: version.HeadSHA, PullRequest: 1, State: "sentinel-private"}
		}},
		{"landing gate", "runtime_validation", "data.runtime.landing", "Invalid landing gate evidence", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.Gate = &gate.CommandResult{Command: "sentinel-private"}
		}},
		{"landing receipt", "runtime_validation", "data.runtime.landing", "Invalid landing receipt", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.Landed, e.Data.Runtime.Landing.Waiting = true, false
			e.Data.Runtime.Landing.MergeSHA = "sentinel-private"
		}},
		{"landing refusal", "runtime_validation", "data.runtime.landing", "Invalid landing refusal", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.Waiting, e.Data.Runtime.Landing.RefusalKind = false, "sentinel-private"
		}},
		{"runtime REST bounds", "runtime_validation", "data.runtime.rest", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", REST: &tracker.NativeRESTEvidence{Requests: -1}}
		}},
		{"runtime REST operation", "runtime_validation", "data.runtime.rest", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", REST: &tracker.NativeRESTEvidence{
				Windows: []tracker.NativeRESTWindow{{EndpointFamily: "sentinel-private"}},
			}}
		}},
		{"runtime REST attribution", "runtime_validation", "data.runtime.rest", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", REST: &tracker.NativeRESTEvidence{
				Divergences: []tracker.NativeRESTDivergence{{Attribution: "sentinel-private"}},
			}}
		}},
		{"runtime GitHub scope", "runtime_validation", "data.runtime.github", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", GitHub: &tracker.NativeGitHubScope{Scope: "sentinel-private"}}
		}},
		{"terminal failure", "execution_validation", "data.terminal_failure", "", func(e *tracker.NativeRunEvent) {
			e.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "sentinel-private"}
		}},
		{"finalization", "execution_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = &tracker.NativeFinalization{Error: "sentinel-private"}
		}},
		{"finalization owner", "execution_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.ChangeID = ""
		}},
		{"source identity", "execution_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: "sentinel-private"}
		}},
		{"finalization publication", "execution_validation", "data.finalization", "PR publication requires a settled exact version, policy and verified delivery identity", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.Settled, e.Data.Finalization.Changed = true, true
			e.Data.Finalization.Publication = &tracker.NativePRPublication{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, PolicyID: "sentinel-private"}
		}},
		{"evidence", "execution_validation", "data.evidence", "", func(e *tracker.NativeRunEvent) {
			e.Data.Evidence = []tracker.NativeEvidence{{Caption: "sentinel-private"}}
		}},
		{"completion body", "execution_validation", "data.completion_body", "", func(e *tracker.NativeRunEvent) { e.Data.CompletionBody = strings.Repeat("sentinel-private", 5000) }},
		{"missing decision request", "execution_validation", "data.completion_body", "blocked completion with human_action requires decision_request", func(e *tracker.NativeRunEvent) {
			e.Data.CompletionBody = "```detent-status\nschema: 1\nstatus: blocked\nhuman_action: Choose a name\n```"
			e.Data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true}
		}},
		{"malformed decision request", "execution_validation", "data.completion_body", "blocked completion with human_action requires a valid decision_request in detent-status", func(e *tracker.NativeRunEvent) {
			e.Data.CompletionBody = strings.Replace(decisionCompletion, "  question: Which naming rule should the installer use?\n", "", 1)
			e.Data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true}
		}},
		{"disposition", "execution_validation", "data.disposition", "", func(e *tracker.NativeRunEvent) {
			e.Data.Disposition = &tracker.NativeDisposition{Status: "sentinel-private"}
		}},
		{"final summary", "execution_validation", "data.disposition", "Final summary must be bounded UTF-8 text", func(e *tracker.NativeRunEvent) {
			e.Data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusComplete, FinalSummary: strings.Repeat("sentinel-private", workpad.MaxFinalSummaryBytes/len("sentinel-private")+1)}
		}},
		{"unordered execution metadata", "execution_validation", "data", "Execution metadata requires an ordered event", func(e *tracker.NativeRunEvent) {
			e.Data.Sequence, e.Data.CompletionBody = 0, ""
		}},
		{"identity", "execution_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Data.Identity = nil }},
		{"handoff", "execution_validation", "data.handoff", "", func(e *tracker.NativeRunEvent) {
			e.Type, e.Data.Outcome, e.Data.CompletionBody = "run.checkpointed", "", ""
		}},
		{"handoff state", "execution_validation", "data.handoff", "Checkpoint has an unsupported recovery or effect state", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.Resume = "sentinel-private"
		}},
		{"handoff head", "execution_validation", "data.handoff", "Checkpoint heads must be commit IDs", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.HeadSHA = "sentinel-private"
		}},
		{"handoff effect", "execution_validation", "data.handoff", "External effects require a typed reconciliation identity and state", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.ExternalEffect, e.Data.Handoff.EffectState, e.Data.Handoff.EffectID = "git_push", "pending", "sentinel-private"
		}},
		{"handoff Git checkpoint", "execution_validation", "data.handoff", "Git checkpoints require a Detent checkpoint ref and commit, tree and base IDs", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.Storage, e.Data.Handoff.Ref = "git_ref", "sentinel-private"
		}},
		{"handoff customer storage", "execution_validation", "data.handoff", "Customer checkpoint references require artifacts and independent availability verification", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.Storage = "customer_store"
		}},
		{"handoff Change reference", "execution_validation", "data.handoff", "Change references require typed immutable version and head identities", func(e *tracker.NativeRunEvent) {
			checkpoint(e)
			e.Data.Handoff.Change = &tracker.NativeChangeReference{ChangeID: "sentinel-private"}
		}},
		{"finalizer head", "owner_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.HeadSHA = strings.Repeat("c", 40)
		}},
		{"source owner", "execution_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: newNativeID("change"), VersionID: version.ID, HeadSHA: version.HeadSHA}
		}},
		{"source attempt", "owner_validation", "data.finalization", "", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			e.Data.Finalization.SourceAttemptID = newNativeID("attempt")
		}},
		{"landing head", "owner_validation", "data.runtime.landing", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.HeadSHA = strings.Repeat("c", 40)
		}},
		{"landing authority", "owner_validation", "data.runtime.landing", "", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			l := e.Data.Runtime.Landing
			l.Landed, l.Waiting, l.MergeSHA, l.Method, l.BaseRef = true, false, strings.Repeat("c", 40), "squash", "develop"
		}},
		{"machine owner", "owner_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Data.MachineID = "other-machine" }},
		{"session owner", "owner_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Data.SessionID = "other-session" }},
		{"runner owner", "owner_validation", "data", "", func(e *tracker.NativeRunEvent) { e.Data.RunnerID = "other-runner" }},
		{"provider reservation ordering", "owner_validation", "data.sequence", "", func(e *tracker.NativeRunEvent) {
			e.Data.Sequence, e.Data.Identity, e.Data.CompletionBody = 0, nil, ""
			e.Data.Usage = []tracker.NativeUsage{{Provider: "sentinel-private", Model: "test-model"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := start
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", test.name, 2, "succeeded"
			event.Data.CompletionBody = "sentinel-private"
			test.edit(&event)
			logs.Reset()
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			assertRefusal(t, response, tracker.PublicationRejection{Category: test.category, Field: test.field}, start.Data.AttemptID, test.reason)
		})
	}
	for _, test := range []struct {
		name, field string
		body        map[string]any
	}{
		{"unknown field", "", map[string]any{"sentinel-private": "sentinel-private"}},
		{"schema type", "schema_version", map[string]any{"schema_version": "sentinel-private"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs.Reset()
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, test.body)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			assertRefusal(t, response, tracker.PublicationRejection{Category: "body_decode", Field: test.field}, "", "")
		})
	}

}

func TestHostedNativeRejectionMessages(t *testing.T) {
	for _, test := range []struct {
		name, category, field, message, want string
	}{
		{"owner", "owner_validation", "data.finalization", "Finalizer version must name its recorded head", "Finalizer version must name its recorded head"},
		{"runtime", "runtime_validation", "data.runtime.activity", "Activity profile does not match its runtime observation", "Activity profile does not match its runtime observation"},
		{"source", "source_verification", "source", "sentinel-private", "publication rejected: category=source_verification field=source [private error text omitted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{config: Config{Hosted: &HostedConfig{}, Logger: discardLogger()}}
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), response)
			if err := service.nativeAPIError(c, nativeRejection(nativeInvalid(test.message), test.category, test.field)); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			var failure struct {
				Message string
				Details struct{ Rejection tracker.PublicationRejection }
			}
			decodeHubResponse(t, response, &failure)
			if failure.Message != test.want || failure.Details.Rejection.Category != test.category || failure.Details.Rejection.Field != test.field || strings.Contains(response.Body.String(), "sentinel-private") {
				t.Fatalf("hosted refusal = %s", response.Body.String())
			}
		})
	}
}
