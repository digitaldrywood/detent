package runnerupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

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
					report := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("process-%d", index), Sequence: int64(step + 1), State: state, Version: "1.2.3", BuildRevision: "revision"}
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
						seedMonotonicRequest(t, f, index, runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), RequestedAt: time.Now().UTC().Truncate(time.Microsecond), Service: "detent", Version: "1.2.4", Release: true, ExpectedBuildRevision: strings.Repeat("a", 64)})
						report.TargetVersion = "1.2.4"
					}
					if state == runnerauth.LifecycleShuttingDown {
						report.Reason = "update"
						report.TargetVersion = "1.2.4"
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], report), http.StatusNoContent)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], report), http.StatusNoContent)
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
					first := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("process-%d", index), Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: "revision"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], first), http.StatusNoContent)
					stale := first
					stale.Sequence, stale.State, stale.Reason = 99, runnerauth.LifecycleShuttingDown, "signal"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], stale), http.StatusConflict)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], stale), http.StatusConflict)
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
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1-index], runnerauth.LifecycleReport{ProcessID: "other", Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: "revision"}), http.StatusUnauthorized)
			}
		})
	}
}

func TestPostgresRunnerTargetOffers(t *testing.T) {
	for _, scope := range []string{"runner", "organization"} {
		t.Run(scope, func(t *testing.T) {
			f := newReceiptFixture(t)
			admin := f.service.AdminKey(t)
			service := New(Deps{Repository: f.service.Repository, Now: time.Now, Version: "1.3.0"})
			now := time.Now().UTC().Truncate(time.Microsecond)
			request := runnerauth.UpdateRequest{ID: "offered", RequestedAt: now, Service: "detent", Version: "1.3.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
			seedMonotonicRequest(t, f, 1, request)
			seedMonotonicRequest(t, f, 1, runnerauth.UpdateRequest{ID: "urgent", RequestedAt: now, Service: "detent", Version: "1.3.0", Release: true, Urgent: true})
			build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
			observation := runnerauth.UpdateObservation{Protocol: 1, Service: "detent", Discovery: "unknown", Running: build, ObservedAt: now, ReceivedAt: now}
			observation.Revision = observation.BuildRevision()
			published := true
			observe := func() {
				t.Helper()
				if err := f.service.Repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), f.bindings[1].RunnerID)
					if err != nil {
						return err
					}
					return service.StoreObservation(t.Context(), w, identity, &observation, 2, build.Version, build.OS, build.Architecture, time.Now().UTC(), published)
				}); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 3 {
				report := runnerauth.LifecycleReport{ProcessID: fmt.Sprintf("failed-%d", i), Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.3.0", BuildRevision: "revision"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_startup/runners/"+f.bindings[0].RunnerID+"/lifecycle", f.tokens[0], report), http.StatusNoContent)
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
			failed := read(0)
			if failed.Health == "online" || failed.Lifecycle == nil || failed.Lifecycle.State != runnerauth.LifecycleStartFailed || failed.Lifecycle.ConsecutiveFailedStarts != 3 || failed.Lifecycle.Version != "1.3.0" {
				t.Fatalf("failed runner=%+v", failed)
			}
			observe()
			if view := read(1); view.Desired != nil {
				t.Fatalf("failed release still offered: %+v", view.Desired)
			}
			targetPath := "/api/v2/organizations/org_startup/runner-update/target"
			if scope == "runner" {
				targetPath = "/api/v2/organizations/org_startup/runners/" + f.bindings[1].RunnerID + "/update/target"
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, targetPath, admin.Token, runnerstore.UpdateTarget{Version: "1.2.4"}), http.StatusOK)
			report := runnerauth.LifecycleReport{ProcessID: "healthy", Sequence: 1, State: runnerauth.LifecycleStarting, Version: "1.2.3", BuildRevision: build.Commit}
			path := "/api/v2/organizations/org_startup/runners/" + f.bindings[1].RunnerID + "/lifecycle"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], report), http.StatusNoContent)
			published = false
			report.Sequence++
			report.State = runnerauth.LifecycleReady
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], report), http.StatusNoContent)
			observe()
			view := read(1)
			if view.Desired == nil || view.Desired.Version != "1.2.4" || view.Desired.FollowHub || view.TargetVersion != "1.2.4" || !view.TargetHeld {
				t.Fatalf("held offer=%+v", view)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, targetPath, f.tokens[1], runnerstore.UpdateTarget{Version: "1.3.0", Revision: 1}), http.StatusForbidden)
			report.Sequence++
			report.State = runnerauth.LifecycleUpdating
			report.TargetVersion = "1.2.4"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], report), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, targetPath, admin.Token, runnerstore.UpdateTarget{Version: "1.2.5", Revision: 1}), http.StatusOK)
			report.Sequence++
			report.State = runnerauth.LifecycleShuttingDown
			report.Reason = "update"
			targetless := report
			targetless.TargetVersion = ""
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], targetless), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], targetless), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], report), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[1], report), http.StatusConflict)
			if err := f.service.Repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(runnerstore.LifecycleReader)
				current, err := owner.RunnerLifecycle(t.Context(), f.bindings[1].RunnerID)
				if err != nil {
					return err
				}
				if current == nil || current.ProcessID != report.ProcessID || current.Sequence != report.Sequence-1 || current.State != runnerauth.LifecycleUpdating || current.TargetVersion != "1.2.4" {
					t.Fatalf("refused update changed current process: %+v", current)
				}
				history, err := owner.RunnerLifecycleHistory(t.Context(), f.bindings[1].RunnerID, 100)
				if err == nil && (len(history) != 5 || history[0].Refusal != runnerstore.LifecycleUpdateNotOffered || history[0].LifecycleReport != report ||
					history[1].Refusal != runnerstore.LifecycleUpdateNotOffered || history[1].LifecycleReport != targetless) {
					t.Errorf("expected one hold refusal, history: %+v", history)
				}
				return err
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
