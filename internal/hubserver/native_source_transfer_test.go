package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSourceTransfer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                 string
		dirty, running, expired, released, denied, stale, cancel, concurrent bool
		uncertain, prestart, cleanEffect, held, staleVersion, historical     bool
		status                                                               int
		sameRunner, advanced                                                 bool
		hosted                                                               bool
		replacement, stopped, acknowledged                                   bool
		worker, retired, restart                                             bool
	}{
		{name: "transfer retains a nondispatchable lane and human label", held: true, released: true, status: http.StatusOK},
		{name: "transfer and duplicate request", released: true, status: http.StatusOK},
		{name: "operator stop retains live source refusal", running: true, stopped: true, status: http.StatusUnprocessableEntity},
		{name: "operator stop followed by terminal acknowledgment", running: true, stopped: true, acknowledged: true, status: http.StatusOK},
		{name: "operator stop acknowledgment after replacement failure", running: true, stopped: true, acknowledged: true, replacement: true, status: http.StatusOK},
		{name: "new publication retires stale transfer selection", advanced: true, released: true, status: http.StatusOK},
		{name: "new publication preserves later dirty checkpoint recovery", advanced: true, dirty: true, released: true, status: http.StatusOK},
		{name: "same runner version-bound recovery", released: true, dirty: true, sameRunner: true, status: http.StatusOK},
		{name: "cancel explicit routing", released: true, cancel: true, status: http.StatusOK},
		{name: "concurrent transfer revisions", released: true, concurrent: true, status: http.StatusOK},
		{name: "released historical running attempt", historical: true, released: true, status: http.StatusOK},
		{name: "restart discovers retired worker termination and transfers source", worker: true, retired: true, running: true, released: true, restart: true, status: http.StatusOK},
		{name: "worker receipt before checkpoint and release", worker: true, retired: true, running: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "worker without receipt before checkpoint and release", worker: true, running: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "worker retired and attempt cleanup acknowledged", worker: true, retired: true, stopped: true, running: true, acknowledged: true, status: http.StatusOK},
		{name: "worker outstanding after attempt cleanup acknowledged", worker: true, stopped: true, running: true, acknowledged: true, status: http.StatusUnprocessableEntity},
		{name: "worker outstanding after terminal release", worker: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "released selected running attempt", running: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "active execution", running: true, status: http.StatusUnprocessableEntity},
		{name: "hosted recovery explains active execution", running: true, hosted: true, status: http.StatusUnprocessableEntity},
		{name: "hosted recovery explains released interrupted source", running: true, released: true, hosted: true, status: http.StatusUnprocessableEntity},
		{name: "interrupted owner and pre-provider replacement retain canonical source", running: true, released: true, hosted: true, replacement: true, status: http.StatusUnprocessableEntity},
		{name: "partition and lease expiry do not prove quiescence", running: true, expired: true, status: http.StatusUnprocessableEntity},
		{name: "terminal execution still owns lease", status: http.StatusUnprocessableEntity},
		{name: "legacy dirty changes no longer pin the source", dirty: true, released: true, status: http.StatusOK},
		{name: "write grant does not grant runner management", denied: true, released: true, status: http.StatusForbidden},
		{name: "clean startup with uncertain publication", cleanEffect: true, uncertain: true, released: true, status: http.StatusOK},
		{name: "uncertain publication outcome", uncertain: true, released: true, status: http.StatusOK},
		{name: "claim before startup is not quiesced", prestart: true, released: true, status: http.StatusOK},
		{name: "stale source version", staleVersion: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "stale revision", stale: true, released: true, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
			var diagnostics bytes.Buffer
			config := Config{Clock: clock.Now, Logger: slog.New(slog.NewTextHandler(&diagnostics, nil))}
			if test.name == "transfer and duplicate request" {
				objects := &changeObjectFixture{}
				config.Content = objects.call
			}
			f := newChangeFixture(t, newClockedPostgresNativeService(t, config))
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(diagnostics.String())
				}
			})
			a := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			b := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			a.redemption.DisplayName, b.redemption.DisplayName = "Source Air", "Destination Mini"
			a.enroll(t)
			b.enroll(t)
			for _, runner := range []runnerFixture{a, b} {
				reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, runner.redemption.DisplayName, runner.redemption.BackendIsolation, runner.redemption.Capacity)
			}
			if test.sameRunner {
				b = a
			}
			if test.stopped {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "source-in-progress"}, ExpectedRevision: f.issue.Revision, State: "In Progress", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &f.issue)
			}
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: f.issue.WorkItemID, MachineID: a.binding.MachineID, SessionID: "source", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			itemPath := f.base + "/work-items/" + string(f.issue.WorkItemID)
			var historical tracker.NativeAttempt
			if test.historical {
				start := nativeStartedEvent(lease)
				start.IdempotencyKey = "historical-start"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, start), http.StatusOK)
				checkpoint := start
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "historical-checkpoint", 2
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, checkpoint), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", a.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				response = performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/attempts/"+start.Data.AttemptID, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &historical)
				if historical.Status != "cancelled" || historical.Outcome != "cancelled" || historical.Current || historical.ClaimReleasedAt == nil || historical.Checkpoint == nil {
					t.Fatalf("historical attempt did not retain cancelled custody: %#v", historical)
				}
				claim.SessionID = "latest-source"
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &lease)
			}
			event := nativeStartedEvent(lease)
			event.Data.Identity.Role, event.Data.MachineID = "code", lease.MachineID
			if test.worker {
				event.Worker = &tracker.NativeAttemptWorker{PID: 101, PGID: 101, StartedAt: clock.Now(), StartKey: "worker-birth"}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
			if test.retired {
				retire := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, WorkerCleanup: event.Worker,
					Data: tracker.NativeRunData{AttemptID: event.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, retire), http.StatusOK)
			}
			event.Worker = nil
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.WorktreeState = "unpushed"
			event.Data.Handoff.HeadSHA = input.HeadSHA
			if test.cleanEffect {
				event.Data.Handoff.WorktreeState, event.Data.Handoff.Resume = "clean", "fresh_checkout"
			}
			if test.uncertain {
				event.Data.Handoff.ExternalEffect = "pr_create"
				event.Data.Handoff.EffectState = "ambiguous"
				event.Data.Handoff.EffectID = "effect_" + strings.Repeat("e", 32)
			}
			if test.dirty {
				event.Data.Handoff.WorktreeState = "dirty"
				event.Data.Handoff.HeadSHA = input.BaseSHA
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
			input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
			diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: event.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+event.Data.AttemptID+"/diff", a.redemption.Credential, diff), http.StatusAccepted)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", a.redemption.Credential, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			if test.replacement || test.stopped {
				event.IdempotencyKey, event.Data.Sequence = "published-checkpoint", 3
				event.Data.Handoff.WorktreeState = "clean"
				event.Data.Handoff.ExternalEffect, event.Data.Handoff.EffectState = "pr_create", "confirmed"
				event.Data.Handoff.EffectID = "effect_" + strings.Repeat("e", 32)
				event.Data.Handoff.Change = &tracker.NativeChangeReference{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: input.HeadSHA}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
			}
			if !test.running {
				finish := event
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome, finish.Data.Handoff = "run.finished", "finished", 3, "succeeded", nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, finish), http.StatusOK)
			}
			if test.stopped {
				credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
				if err != nil {
					t.Fatal(err)
				}
				current := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
				_, err = f.service.stopNativeRunCommand(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, "stop-source", coordinatorStopRunArguments{ProjectID: string(f.project.ID), RequestID: "stop-source", Identifier: string(f.issue.WorkItemID), ExpectedRevision: current.Revision, LeaseID: lease.ID, FencingToken: lease.FencingToken, Destination: "Todo"})
				if err != nil {
					t.Fatal(err)
				}
				f.issue = readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			}
			if test.released && !test.replacement {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", a.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			if test.name == "transfer and duplicate request" {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+version.ID+"/source", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), bundle) {
					t.Fatal("released source bundle changed")
				}
			}

			if test.expired {
				clock.Advance(91 * time.Second)
			}
			if test.replacement {
				clock.Advance(91 * time.Second)
				claim.SessionID = "replacement"
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var replacement tracker.NativeLease
				decodeHubResponse(t, response, &replacement)
				if replacement.FencingToken <= lease.FencingToken {
					t.Fatal("replacement reused interrupted writer authority")
				}
				staleFinish := event
				staleFinish.Type, staleFinish.IdempotencyKey, staleFinish.Data.Sequence = "run.finished", "obsolete-finish", 4
				staleFinish.Data.Outcome, staleFinish.Data.Handoff = "succeeded", nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, staleFinish), http.StatusConflict)
				failure := nativeStartedEvent(replacement)
				failure.Type, failure.IdempotencyKey = "run.finished", "replacement-failed"
				failure.Data.Identity, failure.Data.Outcome = nil, "failed"
				failure.Data.SessionID = ""
				failure.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 2, Generation: 2, Phase: "completed", HeartbeatAt: clock.Now()}
				failure.Data.TerminalFailure = &tracker.NativeTerminalFailure{ObservedAt: clock.Now(), Stage: "pre_provider", Attribution: "issue", ErrorClass: "source_recovery", Error: "native checkpoint requires recovery"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, failure), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(replacement.ID)+"/release", a.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: replacement.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			if test.restart {
				request := sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "before-termination"}, ExpectedRevision: f.issue.Revision, VersionID: version.ID, DestinationRunnerID: b.binding.RunnerID}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", testHubAdminToken, request), http.StatusUnprocessableEntity)
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = reopenNativeAttemptService(t, f.service)
				read := func(owner runnerFixture) runnerauth.AttemptWorkerPage {
					t.Helper()
					path := f.base + "/machines/" + string(owner.binding.MachineID) + "/heartbeat?attempt_worker_after="
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, owner.redemption.Credential, struct{}{})
					requireNativeStatus(t, response, http.StatusOK)
					var inventory runnerauth.AttemptWorkerInventory
					decodeHubResponse(t, response, &inventory)
					if inventory.RunnerID != owner.binding.RunnerID || inventory.Page == nil || len(inventory.Page.Workers) != 0 || inventory.Page.Next != "" {
						t.Fatalf("unexpected process inventory after retirement: %+v", inventory)
					}
					return *inventory.Page
				}
				if page := read(b); len(page.PendingTerminations) != 0 {
					t.Fatalf("foreign termination: %+v", page)
				}
				var pending runnerauth.AttemptTermination
				for range 2 {
					page := read(a)
					if len(page.PendingTerminations) != 1 {
						t.Fatalf("missing termination after restart: %+v", page)
					}
					pending = page.PendingTerminations[0]
					if pending.AttemptID != event.Data.AttemptID || pending.WorkItemID != f.issue.WorkItemID || pending.LeaseID != lease.ID || pending.FencingToken != lease.FencingToken {
						t.Fatalf("termination fence changed: %+v", pending)
					}
				}
				path := f.base + "/work-items/" + string(pending.WorkItemID)
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+pending.AttemptID, a.redemption.Credential, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var attempt tracker.NativeAttempt
				decodeHubResponse(t, response, &attempt)
				ack := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "restart-termination"}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: pending.AttemptID, LeaseID: pending.LeaseID, FencingToken: pending.FencingToken, RunID: attempt.RunID, Identity: attempt.Identity, PolicyID: attempt.PolicyID, Sequence: attempt.Sequence + 1, Outcome: "cancelled"}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", a.redemption.Credential, ack), http.StatusOK)
				if page := read(a); len(page.PendingTerminations) != 0 {
					t.Fatalf("acknowledged termination still visible: %+v", page)
				}
			}
			if test.acknowledged {
				var nextSequence int64
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					attempt, err := reader.Attempt(t.Context(), event.Data.AttemptID)
					nextSequence = attempt.Sequence + 1
					return err
				}); err != nil {
					t.Fatal(err)
				}
				ack := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "terminated"}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: event.Data.AttemptID, RunID: event.Data.RunID, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: event.Data.PolicyID, Identity: event.Data.Identity, Sequence: nextSequence, Outcome: "cancelled"}}
				if !test.replacement {
					for _, release := range []struct {
						name, payload  string
						missing, stale bool
					}{
						{name: "missing release", missing: true},
						{name: "other fence", stale: true},
						{name: "absent reason", payload: `{}`},
						{name: "null reason", payload: `{"reason":null}`},
						{name: "private reason", payload: `{"reason":"private release details"}`},
					} {
						t.Run(release.name, func(t *testing.T) {
							var payload struct {
								Reason string `json:"reason"`
							}
							if release.payload != "" {
								if err := json.Unmarshal([]byte(release.payload), &payload); err != nil {
									t.Fatal(err)
								}
							}
							if release.missing || release.stale {
								invalid := ack
								invalid.IdempotencyKey = "invalid-release-" + strings.ReplaceAll(release.name, " ", "-")
								if release.missing {
									invalid.Data.LeaseID = "missing-lease"
								} else {
									invalid.Data.FencingToken++
								}
								response := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, invalid)
								if response.Code < 400 || response.Code >= 500 {
									t.Fatalf("invalid release authority accepted: %d %s", response.Code, response.Body.String())
								}
								if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
									attempt, err := reader.Attempt(t.Context(), event.Data.AttemptID)
									if err == nil && (attempt.Sequence != nextSequence-1 || attempt.CleanupAcknowledgedAt != nil) {
										t.Fatal("invalid release authority changed attempt acknowledgment")
									}
									return err
								}); err != nil {
									t.Fatal(err)
								}
							}
							failure := nativeLeaseReleaseFailure(payload.Reason, clock.Now())
							if failure.Error != "attempt lease was released before the attempt finished; release reason unavailable (lease expired, replaced or its runner was revoked)" || failure.ErrorClass != "lease_released" || failure.Stage != "lease_release" || !failure.ObservedAt.Equal(clock.Now()) {
								t.Fatalf("missing release cause changed: %+v", failure)
							}
						})
					}
				}
				for _, invalid := range []string{"fence", "owner", "success", "evidence"} {
					stale := ack
					stale.IdempotencyKey = "rejected-" + invalid
					credential := a.redemption.Credential
					switch invalid {
					case "fence":
						stale.Data.FencingToken++
					case "owner":
						credential = b.redemption.Credential
					case "success":
						stale.Data.Outcome = "succeeded"
					case "evidence":
						stale.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: clock.Now()}
					}
					response := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", credential, stale)
					if response.Code < 400 || response.Code >= 500 {
						t.Fatalf("invalid %s acknowledgment: %d %s", invalid, response.Code, response.Body.String())
					}
				}
				for range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, ack), http.StatusOK)
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/attempts/"+event.Data.AttemptID, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var acknowledged tracker.NativeAttempt
				decodeHubResponse(t, response, &acknowledged)
				status := acknowledged.Status
				if failure := acknowledged.TerminalFailure; acknowledged.CleanupAcknowledgedAt == nil || status != "cancelled" || failure == nil || failure.ErrorClass != "lease_released" || failure.Stage != "lease_release" || !strings.HasSuffix(failure.Error, "release reason: cancelled") || failure.ObservedAt.IsZero() {
					t.Fatalf("cancelled attempt ended without its release cause: status=%s failure=%+v", status, failure)
				}
			}
			if test.held {
				labels := []string{"human-owned"}
				edited := performHubAPIRequest(t, f.service, http.MethodPatch, itemPath, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-hold"}, ExpectedRevision: f.issue.Revision, Labels: &labels})
				requireNativeStatus(t, edited, http.StatusOK)
				decodeHubResponse(t, edited, &f.issue)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					found := false
					for i := range record.States {
						if record.States[i].Name == f.issue.State {
							record.States[i].Dispatchable = false
							found = true
						}
					}
					if !found {
						return errors.New("held workflow state missing")
					}
					_, err = w.UpdateProjectRecord(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			recoveryRead := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/source-recovery", f.token, nil)
			requireNativeStatus(t, recoveryRead, http.StatusOK)
			var observed sourceRecoveryView
			decodeHubResponse(t, recoveryRead, &observed)
			if test.worker {
				if observed.Quiesced != (test.status == http.StatusOK) {
					t.Errorf("source recovery quiesced = %t, transfer should return %d", observed.Quiesced, test.status)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(a.binding.MachineID)+"/heartbeat?attempt_worker_after=", a.redemption.Credential, struct{}{})
				requireNativeStatus(t, response, http.StatusOK)
				var inventory runnerauth.AttemptWorkerInventory
				decodeHubResponse(t, response, &inventory)
				if inventory.Page == nil || (test.retired && len(inventory.Page.Workers) != 0) || (!test.retired && (len(inventory.Page.Workers) != 1 || inventory.Page.Workers[0].AttemptID != event.Data.AttemptID)) {
					t.Errorf("worker retired = %t, inventory = %+v", test.retired, inventory.Page)
				}
			}
			if test.replacement && !test.acknowledged && (observed.AttemptID != event.Data.AttemptID || observed.VersionID != version.ID || observed.HeadSHA != input.HeadSHA || !observed.Available || observed.Quiesced) {
				t.Fatalf("replacement erased source identity or treated interruption as completion: %+v", observed)
			}
			if test.historical && (!observed.Quiesced || observed.AttemptID != event.Data.AttemptID) {
				t.Errorf("released historical attempt contradicts terminal released source: %#v", observed)
			}
			if observed.SourceRunnerName != "Source Air" {
				t.Fatalf("scoped source runner name missing: %q", observed.SourceRunnerName)
			}
			explanationRead := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/explanation", f.token, nil)
			requireNativeStatus(t, explanationRead, http.StatusOK)
			var explanation struct {
				Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
			}
			decodeHubResponse(t, explanationRead, &explanation)
			if explanation.Runtime == nil || explanation.Runtime.SourceRecovery == nil || explanation.Runtime.SourceRecovery.AttemptID != observed.AttemptID || explanation.Runtime.SourceRecovery.SourceRunnerName != observed.SourceRunnerName || explanation.Runtime.SourceRecovery.Quiesced != observed.Quiesced || explanation.Runtime.SourceRecovery.Reason != observed.Reason {
				t.Fatal("existing explanation lost source ownership or waiting reason")
			}

			request := sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "transfer"}, ExpectedRevision: f.issue.Revision, VersionID: version.ID, DestinationRunnerID: b.binding.RunnerID}
			if test.staleVersion {
				request.VersionID = "version_stale"
			}
			if test.stale {
				request.ExpectedRevision++
			}
			token := testHubAdminToken
			if test.denied {
				token = f.token
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
			if response.Code >= 500 {
				t.Fatalf("transfer failed: %s", diagnostics.String())
			}
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				if test.hosted {
					credential, _, err := f.service.authenticateAPIToken(t.Context(), token, "", "")
					if err != nil {
						t.Fatal(err)
					}
					_, refusal := f.service.transferNativeSourceCommand(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, string(f.issue.WorkItemID), request)
					if refusal == nil {
						t.Fatal("active or interrupted writer became transferable")
					}
					response = httptest.NewRecorder()
					hosted := &Service{config: Config{Hosted: &hostedplans.HostedConfig{}, Logger: discardLogger()}}
					if err := hosted.nativeAPIError(echo.New().NewContext(httptest.NewRequest(http.MethodPost, itemPath+"/source-recovery", nil), response), refusal); err != nil {
						t.Fatal(err)
					}
					failure := requireNativeCode(t, response, test.status, "invalid_request")
					if failure.Message != observed.Reason {
						t.Fatalf("recovery action lost its unmet precondition: got %q, read %q", failure.Message, observed.Reason)
					}
					if test.released && !strings.Contains(failure.Message, "no acknowledged terminal outcome") {
						t.Fatalf("released interrupted source lacks terminal evidence: %q", failure.Message)
					}
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					row, err := reader.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
					if err == nil && row.RecoveryRunnerID != "" {
						t.Fatalf("refusal changed owner: %q", row.RecoveryRunnerID)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			var view sourceRecoveryView
			decodeHubResponse(t, response, &view)
			recovery := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"?view=recovery", f.token, nil)
			requireNativeStatus(t, recovery, http.StatusOK)
			var resumed tracker.NativeRecovery
			decodeHubResponse(t, recovery, &resumed)
			if resumed.SourceVersionID != version.ID {
				t.Fatalf("transfer lost selected restoration version: %q", resumed.SourceVersionID)
			}
			if test.dirty && (resumed.SourceAttempt() == nil || resumed.SourceAttempt().Checkpoint.HeadSHA != input.BaseSHA) {
				t.Fatal("transfer rewrote older dirty checkpoint custody")
			}
			if view.SourceRunnerID != a.binding.RunnerID || view.DestinationRunnerID != b.binding.RunnerID || !view.Available || !view.Quiesced || view.HeadSHA != input.HeadSHA || view.BaseSHA != input.BaseSHA {
				t.Fatalf("transfer lost source identity: %#v", view)
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("duplicate transfer changed receipt")
			}
			audited := 0
			after := int64(0)
			content := eventstore.NewContent(f.service.runDetailStore())
			for {
				var events []eventstore.Event
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					events, err = reader.HistoryEvents(t.Context(), f.project.ID, f.issue.WorkItemID, after, corestore.MaxPageSize)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, row := range events {
					event, err := content.Event(t.Context(), row)
					if err != nil {
						t.Fatal(err)
					}
					if event.Data.Operation == "source_recovery" {
						audited++
					}
				}
				if len(events) < corestore.MaxPageSize {
					break
				}
				next := events[len(events)-1].Sequence
				if next <= after {
					t.Fatal("source audit cursor did not advance")
				}
				after = next
			}
			if audited != 1 {
				t.Fatalf("duplicate audit events: %d", audited)
			}
			if test.advanced {
				input.AttemptID, input.RunID = "", ""
				input.Code.SHA256 = strings.Repeat("a", 64)
				next := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "new-version"}, ExpectedVersionID: version.ID, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, next, http.StatusOK)
				var newer tracker.ChangeVersion
				decodeHubResponse(t, next, &newer)
				checkpoint := resumed.SourceAttempt().Checkpoint
				if test.dirty {
					claim.SessionID = "continued-source"
					response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &lease)
					event = nativeStartedEvent(lease)
					event.IdempotencyKey = "continued-start"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
					event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "continued-checkpoint", 2
					checkpoint.Storage, checkpoint.Ref = "git_ref", tracker.CheckpointRefPrefix+string(f.issue.WorkItemID)
					checkpoint.CommitSHA, checkpoint.TreeSHA, checkpoint.BaseSHA = strings.Repeat("c", 40), strings.Repeat("e", 40), input.BaseSHA
					event.Data.Handoff = checkpoint
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
				}
				for _, bounded := range []bool{false, true} {
					path := itemPath + "?view=recovery"
					if bounded {
						path += "&projection=bounded"
					}
					response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var resumed tracker.NativeRecovery
					decodeHubResponse(t, response, &resumed)
					if newer.ID == version.ID || resumed.SourceVersionID != "" || resumed.Change == nil || resumed.Change.VersionID != newer.ID {
						t.Fatalf("stale transfer still overrides current source: selected=%q current=%+v", resumed.SourceVersionID, resumed.Change)
					}
					if test.dirty && (resumed.SourceAttemptID != event.Data.AttemptID || resumed.SourceAttempt() == nil || !reflect.DeepEqual(resumed.SourceAttempt().Checkpoint, checkpoint)) {
						t.Fatalf("new publication lost dirty checkpoint custody: %+v", resumed.SourceAttempt())
					}
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					row, err := reader.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
					if err == nil && (row.RecoveryVersionID != version.ID || row.RecoveryRunnerID != b.binding.RunnerID) {
						t.Fatalf("recovery read changed retained transfer: version=%q runner=%q", row.RecoveryVersionID, row.RecoveryRunnerID)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			current, err := f.service.readWorkItem(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			if current.State != f.issue.State || current.Archived != f.issue.Archived || !reflect.DeepEqual(current.Labels, f.issue.Labels) {
				t.Fatal("transfer changed workflow state or archive hold")
			}
			if test.historical {
				response = performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/attempts/"+historical.AttemptID, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var retained tracker.NativeAttempt
				decodeHubResponse(t, response, &retained)
				if !reflect.DeepEqual(retained, historical) {
					t.Fatal("transfer rewrote historical outcome or checkpoint")
				}
				stored, err := f.service.readExactChangeVersionRecord(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID), version.ChangeID, version.ID)
				if err != nil || !reflect.DeepEqual(stored, version) {
					t.Fatalf("transfer replaced immutable source version: %v", err)
				}
			}
			if test.name == "transfer and duplicate request" {
				foreign := newNativeFixture(t, f.service, "", "foreign-source")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"?view=recovery", foreign.token, nil), http.StatusNotFound)
				body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}, "name": operatortool.TransferItem, "arguments": map[string]any{"project_id": f.project.ID, "identifier": f.issue.WorkItemID, "request_id": "mcp-transfer", "expected_revision": strconv.FormatInt(int64(view.Revision), 10), "version_id": version.ID, "destination_runner_id": b.binding.RunnerID}}}
				path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
				for _, credential := range []string{f.token, testHubAdminToken} {
					mcp := performHubWorkCall(t, f.service, path, credential, operatortool.TransferItem, body)
					requireNativeStatus(t, mcp, http.StatusOK)
					var envelope struct {
						Result struct {
							IsError bool `json:"isError"`
							Content struct {
								Data sourceRecoveryView `json:"data"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(mcp.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if credential == f.token {
						if !envelope.Result.IsError {
							t.Fatal("MCP bypassed runner administration")
						}
						continue
					}
					if envelope.Result.IsError || envelope.Result.Content.Data.DestinationRunnerID != b.binding.RunnerID {
						t.Fatalf("MCP transfer failed: %s", mcp.Body.String())
					}
					view = envelope.Result.Content.Data
					duplicate := performHubWorkCall(t, f.service, path, credential, operatortool.TransferItem, body)
					original := envelope.Result.Content.Data
					if err := json.Unmarshal(duplicate.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Result.IsError || !reflect.DeepEqual(envelope.Result.Content.Data, original) {
						t.Fatalf("MCP transfer replay changed receipt: %s", duplicate.Body.String())
					}
				}
			}
			if test.held {
				claim.SessionID = "human-held"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim), http.StatusConflict)
				claim.MachineID = b.binding.MachineID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusConflict)
				return
			}

			if test.prestart {
				claim.MachineID, claim.SessionID = b.binding.MachineID, "destination-before-start"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusOK)
				request.IdempotencyKey, request.ExpectedRevision, request.DestinationRunnerID = "cancel-active", view.Revision, ""
				for _, advance := range []time.Duration{0, 91 * time.Second} {
					clock.Advance(advance)
					response = performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/source-recovery", f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &observed)
					if observed.Quiesced {
						t.Fatal("unreleased claim without an attempt became quiesced")
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request), http.StatusUnprocessableEntity)
				}
				return
			}
			if test.cancel {
				request.IdempotencyKey, request.ExpectedRevision, request.DestinationRunnerID = "cancel", view.Revision, ""
				response = performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &view)
				if view.DestinationRunnerID != "" {
					t.Fatal("cancellation retained destination")
				}
				recovery := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"?view=recovery", f.token, nil)
				requireNativeStatus(t, recovery, http.StatusOK)
				resumed = tracker.NativeRecovery{}
				decodeHubResponse(t, recovery, &resumed)
				if resumed.SourceVersionID != "" {
					t.Fatal("cancelled transfer retained version precedence")
				}
				return
			}
			if test.concurrent {
				var wg sync.WaitGroup
				statuses := make(chan int, 2)
				for i, destination := range []string{a.binding.RunnerID, b.binding.RunnerID} {
					wg.Go(func() {
						next := request
						next.IdempotencyKey, next.ExpectedRevision, next.DestinationRunnerID = "concurrent-"+destination, view.Revision, destination
						if i == 0 {
							next.IdempotencyKey += "-a"
						}
						statuses <- performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, next).Code
					})
				}
				wg.Wait()
				close(statuses)
				successes, conflicts := 0, 0
				for status := range statuses {
					switch status {
					case http.StatusOK:
						successes++
					case http.StatusConflict:
						conflicts++
					default:
						t.Fatalf("concurrent status=%d", status)
					}
				}
				if successes != 1 || conflicts != 1 {
					t.Fatalf("concurrent transfers succeeded=%d conflicted=%d", successes, conflicts)
				}
				return
			}
			claim.MachineID, claim.SessionID = b.binding.MachineID, "destination"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusOK)
			event.IdempotencyKey, event.Data.Sequence = "late-owner", 4
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", a.redemption.Credential, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "late-publication", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedVersionID: version.ID, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusConflict)
		})
	}
}
