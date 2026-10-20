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
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeRunEventRejectionsLogged(t *testing.T) {
	var logs bytes.Buffer
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}))
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f.nativeFixture, worker, "machine", "session", f.issue.WorkItemID)
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
	assertRefusal := func(t *testing.T, response *httptest.ResponseRecorder, want tracker.PublicationRejection, attempt string) {
		t.Helper()
		var failure struct {
			Code, Message string
			Details       struct{ Rejection tracker.PublicationRejection }
		}
		decodeHubResponse(t, response, &failure)
		if failure.Code != "invalid_request" || failure.Message == "" || strings.Contains(failure.Message, "unavailable") || failure.Details.Rejection != want {
			t.Fatalf("refusal = %s", response.Body.String())
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
			if entry["reason"] == "" {
				t.Fatal("refusal reason missing from log")
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
		name, category, field string
		edit                  func(*tracker.NativeRunEvent)
	}{
		{"idempotency", "event_validation", "", func(e *tracker.NativeRunEvent) { e.IdempotencyKey = "" }},
		{"schema", "event_validation", "", func(e *tracker.NativeRunEvent) { e.SchemaVersion = 2 }},
		{"type", "event_validation", "", func(e *tracker.NativeRunEvent) { e.Type = "sentinel-private" }},
		{"observation without runtime", "event_validation", "data", func(e *tracker.NativeRunEvent) { e.Type, e.Data.Outcome = "run.observed", "" }},
		{"unordered runtime", "event_validation", "data", func(e *tracker.NativeRunEvent) {
			e.Data.Sequence = 0
			e.Data.Runtime = &tracker.NativeRuntimeObservation{}
		}},
		{"references", "event_validation", "data", func(e *tracker.NativeRunEvent) { e.Data.RunID = "sentinel-private" }},
		{"outcome", "event_validation", "data.outcome", func(e *tracker.NativeRunEvent) { e.Data.Outcome = "sentinel-private" }},
		{"nonterminal outcome", "event_validation", "data.outcome", func(e *tracker.NativeRunEvent) { e.Type = "run.started" }},
		{"terminal artifacts", "event_validation", "data.artifact_ids", func(e *tracker.NativeRunEvent) { e.Data.ArtifactIDs = []string{newNativeID("artifact")} }},
		{"invalid artifact", "event_validation", "data.artifact_ids", func(e *tracker.NativeRunEvent) {
			e.Type, e.Data.Outcome = "run.checkpointed", ""
			e.Data.ArtifactIDs = []string{"sentinel-private"}
		}},
		{"usage", "usage_validation", "data.usage", func(e *tracker.NativeRunEvent) { e.Data.Usage = []tracker.NativeUsage{{Input: -1}} }},
		{"runtime phase", "runtime_validation", "data.runtime", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "sentinel-private"}
		}},
		{"runtime launch", "runtime_validation", "data.runtime.worker_launch", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{}}
		}},
		{"runtime pipeline", "runtime_validation", "data.runtime.pipeline", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{PipelineDropped: -1}
		}},
		{"runtime validation", "runtime_validation", "data.runtime.validation", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{Command: "sentinel-private"}}
		}},
		{"runtime completion", "runtime_validation", "data.runtime.completion", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{}}
		}},
		{"runtime phases", "runtime_validation", "data.runtime.phases", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Phases: []tracker.NativePhase{{Name: "sentinel-private"}}}
		}},
		{"runtime identity", "runtime_validation", "data.runtime.identity", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Identity: agentidentity.Identity{BackendID: "sentinel-private\ninvalid"}}
		}},
		{"runtime activity", "runtime_validation", "data.runtime.activity", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Activity: &workflowmetrics.ActivityProfile{Schema: 2}}
		}},
		{"runtime landing", "runtime_validation", "data.runtime.landing", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.HeadSHA = "sentinel-private"
		}},
		{"terminal failure", "execution_validation", "data.terminal_failure", func(e *tracker.NativeRunEvent) {
			e.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "sentinel-private"}
		}},
		{"finalization", "execution_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = &tracker.NativeFinalization{Error: "sentinel-private"}
		}},
		{"finalization owner", "execution_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.ChangeID = ""
		}},
		{"source identity", "execution_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: "sentinel-private"}
		}},
		{"evidence", "execution_validation", "data.evidence", func(e *tracker.NativeRunEvent) {
			e.Data.Evidence = []tracker.NativeEvidence{{Caption: "sentinel-private"}}
		}},
		{"completion body", "execution_validation", "data.completion_body", func(e *tracker.NativeRunEvent) { e.Data.CompletionBody = strings.Repeat("sentinel-private", 5000) }},
		{"disposition", "execution_validation", "data.disposition", func(e *tracker.NativeRunEvent) {
			e.Data.Disposition = &tracker.NativeDisposition{Status: "sentinel-private"}
		}},
		{"identity", "execution_validation", "data", func(e *tracker.NativeRunEvent) { e.Data.Identity = nil }},
		{"handoff", "execution_validation", "data.handoff", func(e *tracker.NativeRunEvent) {
			e.Type, e.Data.Outcome, e.Data.CompletionBody = "run.checkpointed", "", ""
		}},
		{"finalizer head", "owner_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.HeadSHA = strings.Repeat("c", 40)
		}},
		{"source owner", "execution_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: newNativeID("change"), VersionID: version.ID, HeadSHA: version.HeadSHA}
		}},
		{"source attempt", "owner_validation", "data.finalization", func(e *tracker.NativeRunEvent) {
			e.Data.Finalization = finalization()
			e.Data.Finalization.SourceVersion = &tracker.NativeChangeReference{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			e.Data.Finalization.SourceAttemptID = newNativeID("attempt")
		}},
		{"landing head", "owner_validation", "data.runtime.landing", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			e.Data.Runtime.Landing.HeadSHA = strings.Repeat("c", 40)
		}},
		{"landing authority", "owner_validation", "data.runtime.landing", func(e *tracker.NativeRunEvent) {
			e.Data.Runtime = landing()
			l := e.Data.Runtime.Landing
			l.Landed, l.Waiting, l.MergeSHA, l.Method, l.BaseRef = true, false, strings.Repeat("c", 40), "squash", "develop"
		}},
		{"machine owner", "owner_validation", "data", func(e *tracker.NativeRunEvent) { e.Data.MachineID = "other-machine" }},
		{"session owner", "owner_validation", "data", func(e *tracker.NativeRunEvent) { e.Data.SessionID = "other-session" }},
		{"runner owner", "owner_validation", "data", func(e *tracker.NativeRunEvent) { e.Data.RunnerID = "other-runner" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := start
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", test.name, 2, "succeeded"
			event.Data.CompletionBody = "sentinel-private"
			test.edit(&event)
			logs.Reset()
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			assertRefusal(t, response, tracker.PublicationRejection{Category: test.category, Field: test.field}, start.Data.AttemptID)
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
			assertRefusal(t, response, tracker.PublicationRejection{Category: "body_decode", Field: test.field}, "")
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
			service := &Service{config: Config{Hosted: &HostedConfig{}}}
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
