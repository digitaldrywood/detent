package runnerupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type lifecycleUpdateCommand struct {
	runnerauth.LifecycleReport
	RequestID string `json:"update_request_id"`
}

func startLifecycle(t *testing.T, f receiptFixture, index int, report runnerauth.LifecycleReport) runnerauth.LifecycleCommand {
	t.Helper()
	path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/lifecycle"
	command := runnerauth.LifecycleCommand{LifecycleReport: report}
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], command)
	if response.Code == http.StatusConflict {
		var challenge hubapi.Error
		if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil || challenge.Code != runnerauth.LifecycleStartGenerationChanged || challenge.CurrentRevision < 1 {
			t.Fatalf("start generation challenge = %s, error = %v", response.Body.String(), err)
		}
		command.PreviousGeneration = int64(challenge.CurrentRevision)
		response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], command)
	}
	requireNativeStatus(t, response, http.StatusNoContent)
	return command
}

func TestPostgresRunnerLifecycleTransitions(t *testing.T) {
	for _, test := range []struct {
		name     string
		states   []runnerauth.LifecycleState
		failures []int
	}{
		{"orderly upgrade", []runnerauth.LifecycleState{runnerauth.LifecycleStarting, runnerauth.LifecycleReady, runnerauth.LifecycleDraining, runnerauth.LifecycleUpdating, runnerauth.LifecycleShuttingDown, runnerauth.LifecycleStopped, runnerauth.LifecycleStarting}, []int{0, 0, 0, 0, 0, 0, 0}},
		{"repeated start", []runnerauth.LifecycleState{runnerauth.LifecycleStarting, runnerauth.LifecycleStarting, runnerauth.LifecycleStarting}, []int{0, 1, 3}},
		{"reported failure", []runnerauth.LifecycleState{runnerauth.LifecycleStarting, runnerauth.LifecycleStartFailed}, []int{0, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReceiptFixture(t)
			for index, binding := range f.bindings {
				path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/lifecycle"
				for step, state := range test.states {
					buildRevision := strings.Repeat("a", 40)
					report := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("process-%d", index), Sequence: int64(step + 1), State: state, Version: "1.2.3", BuildRevision: buildRevision}
					if step > 0 && test.states[step-1] == runnerauth.LifecycleStopped {
						report.ProcessID = "next-process"
						report.Sequence = 1
						report.Version = "1.2.4"
						report.BuildRevision = "next-revision"
					}
					if step > 0 && state == runnerauth.LifecycleStarting && test.states[step-1] == runnerauth.LifecycleStarting {
						report.ProcessID = fmt.Sprintf("process-%d-%d", index, step)
						report.Sequence = 1
					}
					if state == runnerauth.LifecycleStartFailed {
						report.ErrorCode = "startup_error"
					}
					if state == runnerauth.LifecycleUpdating {
						now := time.Now().UTC().Truncate(time.Microsecond)
						build := runnerauth.BuildEvidence{Version: report.Version, Commit: buildRevision, SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
						observation := runnerauth.UpdateObservation{Protocol: 1, Service: "detent", Discovery: "unknown", Running: build, ObservedAt: now, ReceivedAt: now}
						observation.Revision = observation.BuildRevision()
						if err := f.service.Repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
							return w.ReplaceRunnerUpdateObservation(t.Context(), binding.RunnerID, &observation)
						}); err != nil {
							t.Fatal(err)
						}
						seedMonotonicRequest(t, f, index, runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), RequestedAt: now, Service: "detent", Version: "1.2.4", Release: true, ExpectedBuildRevision: observation.Revision})
						report.TargetVersion = "1.2.4"
					}
					if state == runnerauth.LifecycleShuttingDown {
						report.Reason = "update"
						report.TargetVersion = "1.2.4"
					}
					var body any = report
					switch state {
					case runnerauth.LifecycleStarting:
						body = startLifecycle(t, f, index, report)
					case runnerauth.LifecycleUpdating:
						body = lifecycleUpdateCommand{report, fmt.Sprintf("update_%d", index)}
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], body), http.StatusNoContent)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], body), http.StatusNoContent)
					if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
						owner := r.(runnerstore.LifecycleReader)
						current, err := owner.RunnerLifecycle(t.Context(), binding.RunnerID)
						if err != nil {
							return err
						}
						want := state
						wantCode := report.ErrorCode
						if test.failures[step] == 3 {
							want = runnerauth.LifecycleStartFailed
							wantCode = "repeated_start"
						}
						if current == nil || current.State != want || current.Version != report.Version ||
							current.BuildRevision != report.BuildRevision || current.ProcessID != report.ProcessID || current.Sequence != report.Sequence ||
							current.TargetVersion != report.TargetVersion || current.Reason != report.Reason || current.ErrorCode != wantCode || current.LastTransitionAt.IsZero() {
							t.Fatalf("current=%+v want state %s", current, want)
						}
						history, err := owner.RunnerLifecycleHistory(t.Context(), binding.RunnerID, 100)
						if err != nil {
							return err
						}
						if len(history) > 0 && (history[0].LifecycleReport != current.LifecycleReport || !history[0].LastTransitionAt.Equal(current.LastTransitionAt) || history[0].Refusal != "") {
							t.Errorf("history current=%+v state=%+v", history[0], current)
						}
						if len(history) != step+1 {
							t.Fatalf("history=%d want %d", len(history), step+1)
						}
						expectedFailures := test.failures[step]
						if current.ConsecutiveFailedStarts != expectedFailures {
							t.Errorf("step %d failures=%d want %d", step, current.ConsecutiveFailedStarts, expectedFailures)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.states[len(test.states)-1] == runnerauth.LifecycleStarting && test.failures[len(test.failures)-1] == 0 {
					first := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("process-%d", index), Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: strings.Repeat("a", 40)}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], first), http.StatusNoContent)
					stale := first
					stale.Sequence, stale.State, stale.Reason = 99, runnerauth.LifecycleShuttingDown, "signal"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], stale), http.StatusConflict)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], stale), http.StatusConflict)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], lifecycleUpdateCommand{stale, "irrelevant"}), http.StatusConflict)
					if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
						owner := r.(runnerstore.LifecycleReader)
						history, err := owner.RunnerLifecycleHistory(t.Context(), binding.RunnerID, 100)
						if err != nil {
							return err
						}
						if len(history) != len(test.states)+1 || history[0].Refusal != runnerstore.LifecycleInvalidTransition || history[0].LifecycleReport != stale {
							t.Fatalf("expected one recorded stale-process refusal, history: %+v", history)
						}
						current, err := owner.RunnerLifecycle(t.Context(), binding.RunnerID)
						if err == nil && (current == nil || current.ProcessID != "next-process" || current.State != runnerauth.LifecycleStarting) {
							t.Errorf("replayed and refused reports changed current process: %+v", current)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					next := runnerauth.LifecycleReport{ProcessID: "next-process", Sequence: 2, State: runnerauth.LifecycleStarting, Version: "1.2.4", BuildRevision: "next-revision"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], next), http.StatusConflict)
					for sequence := int64(3); sequence <= 66; sequence++ {
						stale.Sequence = sequence
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], stale), http.StatusConflict)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], first), http.StatusNoContent)
					next.State = runnerauth.LifecycleReady
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], next), http.StatusConflict)
					next.Sequence++
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], next), http.StatusNoContent)
					if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
						history, err := r.(runnerstore.LifecycleReader).RunnerLifecycleHistory(t.Context(), binding.RunnerID, 100)
						if err == nil && len(history) != len(test.states)+1+64 {
							t.Errorf("bounded history has %d rows, want %d", len(history), len(test.states)+1+64)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1-index], runnerauth.LifecycleReport{ProcessID: "other", Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: "revision"}), http.StatusUnauthorized)
			}
		})
	}
}

func TestPostgresRunnerLifecycleHistoryIsBounded(t *testing.T) {
	f := newReceiptFixture(t)
	for index, binding := range f.bindings {
		path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/lifecycle"
		post := func(body any, status int) {
			t.Helper()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], body), status)
		}
		read := func(wantRows int) runnerauth.Lifecycle {
			t.Helper()
			var current runnerauth.Lifecycle
			err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(runnerstore.LifecycleReader)
				lifecycle, err := owner.RunnerLifecycle(t.Context(), binding.RunnerID)
				if err != nil {
					return err
				}
				current = *lifecycle
				history, err := owner.RunnerLifecycleHistory(t.Context(), binding.RunnerID, 100)
				if err == nil && len(history) != wantRows {
					t.Errorf("runner %d history has %d rows, want %d", index, len(history), wantRows)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return current
		}
		report := runnerauth.LifecycleReport{ProcessID: "first-process", Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: "revision"}
		first := startLifecycle(t, f, index, report)
		report.Sequence, report.State = 2, runnerauth.LifecycleReady
		post(report, http.StatusNoContent)
		ready := read(2)
		for n := int64(3); n <= 70; n++ {
			report.Sequence = n
			post(report, http.StatusNoContent)
		}
		post(report, http.StatusNoContent)
		if current := read(2); current.Sequence != 70 || !current.LastTransitionAt.Equal(ready.LastTransitionAt) {
			t.Errorf("coalesced receipt moved the transition time or failed to advance the fence: %+v", current)
		}
		for range 34 {
			report.Sequence++
			report.State = runnerauth.LifecycleDraining
			post(report, http.StatusNoContent)
			report.Sequence++
			report.State = runnerauth.LifecycleReady
			post(report, http.StatusNoContent)
		}
		read(64)
		report.ProcessID, report.Sequence, report.State = "new-process", 1, runnerauth.LifecycleStarting
		startLifecycle(t, f, index, report)
		post(first, http.StatusConflict)
		if current := read(64); current.LifecycleReport != report {
			t.Fatalf("evicted start receipt changed the current process: %+v", current)
		}
	}
}

func TestPostgresLegacyRunnerOfferBudget(t *testing.T) {
	for _, mode := range []string{"runner hold", "organization hold", "follow Hub"} {
		t.Run(mode, func(t *testing.T) {
			version := "1.3.0"
			f := newReceiptFixtureAtVersion(t, &version, 2)
			service := New(Deps{Repository: f.service.Repository, Now: time.Now, Version: version})
			heartbeat, ok := f.service.Repository.(interface {
				WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
			})
			if !ok {
				t.Fatal("runner heartbeat owner unavailable")
			}
			admin := f.service.AdminKey(t)
			followHub := mode == "follow Hub"
			for index, binding := range f.bindings {
				path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/update"
				if mode == "runner hold" {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path+"/target", admin.Token, runnerstore.UpdateTarget{Version: version}), http.StatusOK)
				}
				if mode == "organization hold" && index == 0 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, "/api/v2/organizations/org_startup/runner-update/target", admin.Token, runnerstore.UpdateTarget{Version: version}), http.StatusOK)
				}
				now := time.Now().UTC().Truncate(time.Microsecond)
				var receipt *runnerauth.UpdateReceipt
				observe := func(build int) *runnerauth.UpdateRequest {
					t.Helper()
					now = now.Add(time.Microsecond)
					running := runnerauth.BuildEvidence{Version: version, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%064x", build), Source: "release", OS: "linux", Architecture: "amd64", ObservedAt: now}
					observation := runnerauth.UpdateObservation{Protocol: 1, Service: "detent", Discovery: "unknown", Supported: true, Running: running, ObservedAt: now, Receipt: receipt}
					observation.Revision = observation.BuildRevision()
					if err := heartbeat.WriteRunnerHeartbeat(t.Context(), "org_startup", f.credentials[index].ID, binding.RunnerID, func(ctx context.Context, w tenantstore.Writer) error {
						identity, err := w.RunnerIdentity(ctx, binding.RunnerID)
						if err != nil {
							return err
						}
						return service.StoreObservation(ctx, w, identity, &observation, 2, version, running.OS, running.Architecture, now, true)
					}); err != nil {
						t.Fatal(err)
					}
					response := performHubAPIRequest(t, f.service, http.MethodGet, path, admin.Token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var view runnerauth.UpdateView
					if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
					if view.Lifecycle != nil || view.Observation == nil || view.Observation.Revision != observation.Revision {
						t.Fatalf("unexpected legacy observation: %+v", view)
					}
					if request := view.Desired; request != nil && (request.FollowHub != followHub || request.Version != version || request.ExpectedBuildRevision != observation.Revision || !request.Release || !request.FromRelease) {
						t.Fatalf("legacy request for build %d: %+v", build, request)
					}
					return view.Desired
				}
				var first runnerauth.UpdateRequest
				requests := make(map[string]bool)
				for build := range 8 {
					offer := observe(build)
					if offer == nil || requests[offer.ID] {
						t.Fatalf("expected a distinct offer for build %d, got %+v", build, offer)
					}
					requests[offer.ID] = true
					if build == 0 {
						first = *offer
					}
					receipt = &runnerauth.UpdateReceipt{Request: *offer, Status: "refused", FailureReason: "Running build changed", ObservedAt: now}
				}
				for build := 8; build < 11; build++ {
					if offer := observe(build); offer != nil {
						t.Fatalf("build %d received an offer beyond the per-runner budget: %+v", build, offer)
					}
				}
				if offer := observe(0); offer == nil || !offer.Matches(first) {
					t.Errorf("known build did not recover its original offer: %+v", offer)
				}
			}
		})
	}
}

func TestPostgresRunnerTargetOffers(t *testing.T) {
	for _, scope := range []string{"runner", "organization"} {
		t.Run(scope, func(t *testing.T) {
			hubVersion := "1.3.0"
			f := newReceiptFixtureAtVersion(t, &hubVersion, 2)
			heartbeat, ok := f.service.Repository.(interface {
				WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
			})
			if !ok {
				t.Fatal("runner heartbeat owner unavailable")
			}
			admin := f.service.AdminKey(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
			observe := func(index int, running runnerauth.BuildEvidence) string {
				t.Helper()
				observation := runnerauth.UpdateObservation{Protocol: 1, Service: "detent", Discovery: "unknown", Running: running, ObservedAt: now, ReceivedAt: now}
				observation.Revision = observation.BuildRevision()
				if err := heartbeat.WriteRunnerHeartbeat(t.Context(), "org_startup", f.credentials[index].ID, f.bindings[index].RunnerID, func(ctx context.Context, w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(ctx, f.bindings[index].RunnerID)
					if err != nil {
						return err
					}
					service := New(Deps{Repository: f.service.Repository, Now: time.Now, Version: hubVersion})
					return service.StoreObservation(ctx, w, identity, &observation, 2, running.Version, running.OS, running.Architecture, time.Now().UTC(), true)
				}); err != nil {
					t.Fatal(err)
				}
				return observation.Revision
			}
			report := func(index int, body any, status int) {
				t.Helper()
				if start, ok := body.(runnerauth.LifecycleReport); ok && start.State == runnerauth.LifecycleStarting && status == http.StatusNoContent {
					startLifecycle(t, f, index, start)
					return
				}
				path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/lifecycle"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], body), status)
			}
			read := func(index int) runnerauth.UpdateView {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/org_startup/runners/"+f.bindings[index].RunnerID+"/update", admin.Token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view runnerauth.UpdateView
				if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				return view
			}
			peerFleet := func() runnerstore.UpdateState {
				t.Helper()
				var update runnerstore.UpdateState
				err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
					records, err := r.(runnerstore.FleetReader).FleetRecords(t.Context(), 10)
					if err != nil {
						return err
					}
					for _, rec := range records {
						if rec.Identity.RunnerID == f.bindings[1].RunnerID {
							update = rec.Update
							return nil
						}
					}
					return corestore.ErrNotFound
				})
				if err != nil {
					t.Fatal(err)
				}
				return update
			}
			baseTarget := "/api/v2/organizations/org_startup/runner-update/target"
			targetPath := baseTarget
			if scope == "runner" {
				targetPath = "/api/v2/organizations/org_startup/runners/" + f.bindings[1].RunnerID + "/update/target"
			}
			setTarget := func(path, version string, rev int64) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, admin.Token, runnerstore.UpdateTarget{Version: version, Revision: rev}), http.StatusOK)
			}

			peer := runnerauth.LifecycleReport{ProcessID: "peer", Sequence: 1, State: runnerauth.LifecycleStarting, Version: build.Version, BuildRevision: build.Commit}
			report(1, peer, http.StatusNoContent)
			peer.Sequence, peer.State = 2, runnerauth.LifecycleReady
			report(1, peer, http.StatusNoContent)
			observe(1, build)
			initialOffer := read(1).Desired
			if initialOffer == nil || initialOffer.Version != "1.3.0" {
				t.Fatalf("initial offer=%+v", initialOffer)
			}
			peer.Sequence, peer.State, peer.TargetVersion = 3, runnerauth.LifecycleDraining, "1.3.0"
			report(1, lifecycleUpdateCommand{peer, initialOffer.ID}, http.StatusNoContent)
			hubVersion = build.Version
			peer.Sequence, peer.State = 4, runnerauth.LifecycleUpdating
			report(1, lifecycleUpdateCommand{peer, initialOffer.ID}, http.StatusConflict)
			if view := read(1); view.Desired != nil || view.TargetVersion != build.Version || view.Lifecycle == nil || view.Lifecycle.State != runnerauth.LifecycleDraining {
				t.Fatalf("withdrawn offer during drain: %+v", view)
			}
			observe(1, build)
			if fleet := peerFleet(); fleet.Desired != nil {
				t.Fatalf("reconciliation kept the withdrawn offer: %+v", fleet)
			}
			hubVersion = "1.3.0"
			observe(1, build)
			peer.Sequence++
			activation := lifecycleUpdateCommand{peer, initialOffer.ID}
			report(1, activation, http.StatusNoContent)
			hubVersion = build.Version
			report(1, activation, http.StatusConflict)
			hubVersion = "1.3.0"
			peer.Sequence, peer.State, peer.TargetVersion = 6, runnerauth.LifecycleReady, ""
			report(1, peer, http.StatusNoContent)
			changedBuild := build
			changedBuild.SHA256 = strings.Repeat("c", 64)
			changedRevision := observe(1, changedBuild)
			changedOffer := read(1).Desired
			if changedOffer == nil || changedOffer.ID == initialOffer.ID || changedOffer.ExpectedBuildRevision != changedRevision {
				t.Fatalf("offer for changed build=%+v", changedOffer)
			}
			observe(1, build)
			if offer := read(1).Desired; offer == nil || !offer.Matches(*initialOffer) {
				t.Errorf("returning to the original build did not reuse its offer: %+v", offer)
			}
			observe(1, changedBuild)
			if offer := read(1).Desired; offer == nil || !offer.Matches(*changedOffer) {
				t.Errorf("returning to the changed build did not reuse its offer: %+v", offer)
			}
			if scope == "runner" {
				for i := range 7 {
					otherBuild := build
					otherBuild.SHA256 = fmt.Sprintf("%064x", i)
					revision := observe(1, otherBuild)
					offer := read(1).Desired
					if i < 6 && (offer == nil || offer.ExpectedBuildRevision != revision) || i == 6 && offer != nil {
						t.Errorf("offer for additional build %d: %+v", i, offer)
					}
				}
				if fleet := peerFleet(); fleet.Desired != nil {
					t.Errorf("fleet returned a stale build offer at capacity: %+v", fleet)
				}
				peer.Sequence, peer.State, peer.TargetVersion = 7, runnerauth.LifecycleUpdating, "1.3.0"
				report(1, lifecycleUpdateCommand{peer, changedOffer.ID}, http.StatusConflict)
			}
			observe(1, build)
			if offer := read(1).Desired; offer == nil || !offer.Matches(*initialOffer) {
				t.Errorf("known build lost its offer at capacity: %+v", offer)
			}
			starts := func(index int, prefix string) runnerauth.LifecycleReport {
				t.Helper()
				var startup runnerauth.LifecycleReport
				for i := range 3 {
					startup = runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("%s-%d", prefix, i), Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.3.0", BuildRevision: "new-revision"}
					report(index, startup, http.StatusNoContent)
				}
				failed := read(index)
				if failed.Health == "online" || failed.Lifecycle == nil || failed.Lifecycle.State != runnerauth.LifecycleStartFailed || failed.Lifecycle.ConsecutiveFailedStarts != 3 || failed.Lifecycle.Version != "1.3.0" {
					t.Fatalf("failed runner=%+v", failed)
				}
				return startup
			}
			starts(0, "unoffered")
			if view := read(1); view.Desired == nil || peerFleet().Desired == nil {
				t.Fatal("unoffered starts suppressed peer update")
			}

			authorizeRollout := func(index int) {
				t.Helper()
				start := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("before-update-%d", index), Sequence: 1, State: runnerauth.LifecycleStarting, Version: build.Version, BuildRevision: build.Commit}
				report(index, start, http.StatusNoContent)
				start.Sequence, start.State = 2, runnerauth.LifecycleReady
				report(index, start, http.StatusNoContent)
				observe(index, build)
				offer := read(index).Desired
				if offer == nil || offer.Version != "1.3.0" {
					t.Fatalf("rollout offer=%+v", offer)
				}
				start.Sequence, start.State, start.TargetVersion = 3, runnerauth.LifecycleUpdating, "1.3.0"
				report(index, lifecycleUpdateCommand{start, offer.ID}, http.StatusNoContent)
			}
			authorizeRollout(0)
			peer.Sequence, peer.State, peer.TargetVersion = 8, runnerauth.LifecycleDraining, "1.3.0"
			report(1, lifecycleUpdateCommand{peer, initialOffer.ID}, http.StatusNoContent)
			var authorizedFailure runnerauth.LifecycleReport
			if scope == "runner" {
				authorizedFailure = starts(0, "self-reported")
				if view := read(1); view.Desired == nil || peerFleet().Desired == nil {
					t.Fatalf("runner-reported starts paused the peer without supervisor evidence: %+v", view)
				}
			}
			supervisorPath := "/api/v2/organizations/org_startup/runners/" + f.bindings[0].RunnerID + "/update/restart-evidence"
			grant := performHubAPIRequest(t, f.service, http.MethodGet, supervisorPath, admin.Token, nil)
			requireNativeStatus(t, grant, http.StatusOK)
			var supervisedRollout runnerstore.Rollout
			if err := json.Unmarshal(grant.Body.Bytes(), &supervisedRollout); err != nil || supervisedRollout.ActivationID == "" || supervisedRollout.RequestID == "" || supervisedRollout.Version != "1.3.0" {
				t.Fatalf("supervisor activation %s, error=%v", grant.Body.String(), err)
			}
			failureEvidence := runnerstore.RestartEvidence{ActivationID: supervisedRollout.ActivationID, Version: supervisedRollout.Version, Outcome: runnerauth.LifecycleStartFailed, FailedStarts: 3}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, f.tokens[0], failureEvidence), http.StatusForbidden)
			otherRunnerPath := "/api/v2/organizations/org_startup/runners/" + f.bindings[1].RunnerID + "/update/restart-evidence"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, otherRunnerPath, admin.Token, failureEvidence), http.StatusConflict)
			otherVersion := failureEvidence
			otherVersion.Version = "1.3.1"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, otherVersion), http.StatusConflict)
			if view := read(1); view.Desired == nil {
				t.Fatalf("refused supervisor evidence changed peer offers: %+v", view)
			}
			for range 2 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, failureEvidence), http.StatusNoContent)
			}
			if own, peer := read(0), read(1); own.Desired != nil || peer.Desired != nil || peerFleet().Desired != nil {
				t.Errorf("supervised runner failure left an offer active: own=%+v peer=%+v", own.Desired, peer.Desired)
			}
			if scope == "organization" {
				authorizedFailure = starts(0, "after-supervisor")
			}
			if scope == "organization" {
				unofferedBuild := build
				unofferedBuild.SHA256 = strings.Repeat("f", 64)
				observe(1, unofferedBuild)
				if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
					state, err := r.RunnerUpdateState(t.Context(), f.bindings[1].RunnerID)
					if err == nil && state.Desired != nil {
						t.Errorf("created an offer for a new build during the pause: %+v", state.Desired)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				observe(1, build)
			}
			activationAfterPause := peer
			activationAfterPause.Sequence, activationAfterPause.State = 9, runnerauth.LifecycleUpdating
			report(1, lifecycleUpdateCommand{activationAfterPause, initialOffer.ID}, http.StatusConflict)
			report(1, lifecycleUpdateCommand{activationAfterPause, initialOffer.ID}, http.StatusConflict)
			if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(runnerstore.LifecycleReader)
				current, err := owner.RunnerLifecycle(t.Context(), f.bindings[1].RunnerID)
				if err != nil {
					return err
				}
				history, err := owner.RunnerLifecycleHistory(t.Context(), f.bindings[1].RunnerID, 100)
				if err != nil {
					return err
				}
				if current.LifecycleReport != peer || len(history) < 2 || history[0].LifecycleReport != activationAfterPause ||
					history[0].Refusal != runnerstore.LifecycleUpdateNotOffered || history[1].LifecycleReport != peer || history[1].Refusal != "" {
					t.Fatalf("paused activation changed the owner or duplicated the refusal: current=%+v history=%+v", current, history)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			seedMonotonicRequest(t, f, 1, runnerauth.UpdateRequest{ID: "urgent", RequestedAt: now, Service: "detent", Version: "1.3.0", Release: true, Urgent: true})
			if view := read(1); view.Desired != nil {
				t.Fatalf("urgent offer bypassed failed rollout: %+v", view.Desired)
			}
			if fleet := peerFleet(); fleet.Desired != nil || fleet.UrgentAllowed {
				t.Fatalf("fleet view bypassed failed rollout: %+v", fleet)
			}
			shutdown := authorizedFailure
			shutdown.Sequence, shutdown.State, shutdown.Reason = 2, runnerauth.LifecycleShuttingDown, "signal"
			report(0, shutdown, http.StatusNoContent)
			if view := read(1); view.Desired != nil {
				t.Errorf("shutdown cleared the failed rollout pause: %+v", view.Desired)
			}
			if fleet := peerFleet(); fleet.Desired != nil || fleet.UrgentAllowed {
				t.Errorf("shutdown cleared the failed rollout pause in fleet: %+v", fleet)
			}
			reverted := runnerauth.LifecycleReport{ProcessID: "reverted", Sequence: 1, State: runnerauth.LifecycleStarting, Version: build.Version, BuildRevision: build.Commit}
			report(0, reverted, http.StatusNoContent)
			reverted.Sequence, reverted.State = 2, runnerauth.LifecycleReady
			report(0, reverted, http.StatusNoContent)
			if view := read(1); view.Desired != nil {
				t.Errorf("ready on another release cleared the failed rollout pause: %+v", view.Desired)
			}

			var organizationRevision int64
			if scope == "organization" {
				setTarget(baseTarget, "", 0)
				organizationRevision = 1
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, failureEvidence), http.StatusConflict)
			} else {
				recovery := authorizedFailure
				recovery.ProcessID = "recovered"
				report(0, recovery, http.StatusNoContent)
				recovery.Sequence, recovery.State = 2, runnerauth.LifecycleReady
				report(0, recovery, http.StatusNoContent)
				if view := read(1); view.Desired != nil {
					t.Fatalf("runner-reported readiness cleared the supervisor pause: %+v", view.Desired)
				}
				readyEvidence := failureEvidence
				readyEvidence.Outcome, readyEvidence.FailedStarts = runnerauth.LifecycleReady, 0
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, readyEvidence), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, readyEvidence), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, supervisorPath, admin.Token, failureEvidence), http.StatusConflict)
				observe(1, build)
			}
			if view := read(1); view.Desired == nil || view.Desired.Version != "1.3.0" {
				t.Fatalf("peer offer did not resume: %+v", view.Desired)
			}
			if fleet := peerFleet(); (fleet.Desired == nil) != (scope == "organization") || !fleet.UrgentAllowed {
				t.Fatalf("fleet offer revisions and urgent policy after recovery: %+v", fleet)
			}

			build.Version = "1.3.0"
			healthy := runnerauth.LifecycleReport{ProcessID: "healthy", Sequence: 1, State: runnerauth.LifecycleStarting, Version: build.Version, BuildRevision: build.Commit}
			report(1, healthy, http.StatusNoContent)
			healthy.Sequence, healthy.State = 2, runnerauth.LifecycleReady
			report(1, healthy, http.StatusNoContent)
			var revision int64
			if scope == "organization" {
				revision = organizationRevision
			}
			setTarget(targetPath, "1.2.4", revision)
			currentBuild := observe(1, build)
			view := read(1)
			if view.Desired == nil || view.Desired.Version != "1.2.4" || view.Desired.FollowHub || view.TargetVersion != "1.2.4" || !view.TargetHeld {
				t.Fatalf("held offer=%+v", view)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, targetPath, f.tokens[1], runnerstore.UpdateTarget{Version: "1.3.0", Revision: revision + 1}), http.StatusForbidden)
			healthy.Sequence, healthy.State, healthy.TargetVersion = 3, runnerauth.LifecycleDraining, "1.2.4"
			report(1, lifecycleUpdateCommand{healthy, view.Desired.ID}, http.StatusNoContent)

			setTarget(targetPath, "", revision+1)
			observe(1, build)
			if cleared := read(1); cleared.Desired != nil || cleared.TargetHeld {
				t.Fatalf("cleared hold still offered: %+v", cleared)
			}
			if fleet := peerFleet(); fleet.Desired != nil || fleet.TargetHeld {
				t.Fatalf("cleared hold still in fleet view: %+v", fleet)
			}
			healthy.Sequence, healthy.State = 4, runnerauth.LifecycleUpdating
			refused := lifecycleUpdateCommand{healthy, view.Desired.ID}
			report(1, refused, http.StatusConflict)
			report(1, refused, http.StatusConflict)

			setTarget(targetPath, "1.2.4", revision+2)
			observe(1, build)
			newOffer := read(1).Desired
			if newOffer == nil || newOffer.ID == view.Desired.ID || newOffer.ExpectedBuildRevision != currentBuild {
				t.Fatalf("reinstated hold reused obsolete offer: %+v", newOffer)
			}
			healthy.Sequence++
			report(1, lifecycleUpdateCommand{healthy, view.Desired.ID}, http.StatusConflict)
			healthy.Sequence++
			report(1, lifecycleUpdateCommand{healthy, newOffer.ID}, http.StatusNoContent)
			setTarget(targetPath, "", revision+3)
			healthy.Sequence, healthy.State, healthy.Reason = 7, runnerauth.LifecycleShuttingDown, "update"
			report(1, healthy, http.StatusNoContent)
			if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(runnerstore.LifecycleReader)
				current, err := owner.RunnerLifecycle(t.Context(), f.bindings[1].RunnerID)
				if err != nil {
					return err
				}
				history, err := owner.RunnerLifecycleHistory(t.Context(), f.bindings[1].RunnerID, 100)
				if err != nil {
					return err
				}
				if current == nil || current.LifecycleReport != healthy || len(history) < 4 ||
					history[0].RolloutRequestID != newOffer.ID || history[1].RolloutRequestID != newOffer.ID ||
					history[2].Refusal != runnerstore.LifecycleUpdateNotOffered || history[3].Refusal != runnerstore.LifecycleUpdateNotOffered {
					t.Fatalf("activation history: current=%+v history=%+v", current, history)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresRunnerStartDeadline(t *testing.T) {
	f := newReceiptFixture(t)
	service := New(Deps{Repository: f.service.Repository, Now: time.Now})
	var deadline time.Time
	for index, binding := range f.bindings {
		report := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("deadline-%d", index), Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: "revision"}
		path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/lifecycle"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], report), http.StatusNoContent)
		report.Sequence++
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], report), http.StatusConflict)
		if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.(runnerstore.LifecycleReader).RunnerLifecycle(t.Context(), binding.RunnerID)
			if err != nil {
				return err
			}
			if index == 0 {
				deadline = current.LastTransitionAt.Add(runnerauth.RunnerReadyTimeout)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if used, err := service.ExpireStarts(t.Context(), "org_startup", deadline.Add(-time.Microsecond), 1); err != nil || used != 0 {
		t.Fatalf("before deadline used=%d error=%v", used, err)
	}
	if used, err := service.ExpireStarts(t.Context(), "org_startup", deadline, 1); err != nil || used != 1 {
		t.Fatalf("at deadline used=%d error=%v", used, err)
	}
	if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		owner := r.(runnerstore.LifecycleReader)
		failed, err := owner.RunnerLifecycle(t.Context(), f.bindings[0].RunnerID)
		if err != nil {
			return err
		}
		if failed.State != runnerauth.LifecycleStartFailed || failed.ErrorCode != "ready_timeout" || failed.ConsecutiveFailedStarts != 1 {
			t.Fatalf("deadline state=%+v", failed)
		}
		other, err := owner.RunnerLifecycle(t.Context(), f.bindings[1].RunnerID)
		if err != nil {
			return err
		}
		if other.State != runnerauth.LifecycleStarting {
			t.Fatalf("budget expired another start: %+v", other)
		}
		history, err := owner.RunnerLifecycleHistory(t.Context(), f.bindings[0].RunnerID, 100)
		if err != nil {
			return err
		}
		if len(history) != 3 || history[0].State != runnerauth.LifecycleStartFailed || history[1].Refusal != runnerstore.LifecycleInvalidTransition {
			t.Fatalf("deadline history=%+v", history)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report := runnerauth.LifecycleReport{ProcessID: "deadline-0", Sequence: 3, State: runnerauth.LifecycleReady, Version: "1.2.3", BuildRevision: "revision"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_startup/runners/"+f.bindings[0].RunnerID+"/lifecycle", f.tokens[0], report), http.StatusNoContent)
}
