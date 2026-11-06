package hubserver

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestPostgresRunnerUpdateReceiptMonotonic(t *testing.T) {
	for _, mode := range []string{"put-put", "heartbeat-heartbeat", "put-heartbeat", "heartbeat-put", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			for index := range f.bindings {
				request := runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), RequestedAt: now, Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
				seedMonotonicRequest(t, f, index, request)
				build := runnerauth.BuildEvidence{Version: "1.1.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
				old := runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
				newer := runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &build, ObservedAt: now.Add(time.Second)}
				if index == 1 {
					newer.ObservedAt = old.ObservedAt
				}
				writers := strings.Split(mode, "-")
				if mode == "concurrent" {
					start := make(chan struct{})
					var wg sync.WaitGroup
					for i, receipt := range []runnerauth.UpdateReceipt{old, newer} {
						wg.Go(func() { <-start; writeMonotonicReceipt(t, f, index, []string{"put", "heartbeat"}[i], receipt, build) })
					}
					close(start)
					wg.Wait()
				} else {
					if index == 1 {
						writeMonotonicReceipt(t, f, index, writers[1], newer, build)
					}
					writeMonotonicReceipt(t, f, index, writers[0], old, build)
					writeMonotonicReceipt(t, f, index, writers[1], newer, build)
					writeMonotonicReceipt(t, f, index, writers[0], old, build)
				}
				assertMonotonicReceipt(t, f, index, newer)
				writeMonotonicReceipt(t, f, index, "put", newer, build)
				assertMonotonicReceipt(t, f, index, newer)
			}
		})
	}
}

func seedMonotonicRequest(t *testing.T, f runnerPGFixture, index int, request runnerauth.UpdateRequest) {
	t.Helper()
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		id := f.bindings[index].RunnerID
		identity, err := w.RunnerIdentity(t.Context(), id)
		if err != nil {
			return err
		}
		identity.DisplayName = "Example"
		if err := w.UpdateRunnerIdentity(t.Context(), identity); err != nil {
			return err
		}
		owner := id
		if request.Urgent {
			owner = ""
		}
		if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: "org_startup", RunnerID: owner, Request: request}); err != nil {
			return err
		}
		if request.Urgent {
			urgent, err := w.RunnerUrgentUpdate(t.Context())
			if err != nil {
				return err
			}
			return w.SetRunnerUrgentUpdate(t.Context(), urgent.Revision, request.ID, request.RequestedAt)
		}
		identity, err = w.RunnerIdentity(t.Context(), id)
		if err != nil {
			return err
		}
		return w.SetRunnerUpdateDesired(t.Context(), id, identity.Revision, request.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeMonotonicReceipt(t *testing.T, f runnerPGFixture, index int, writer string, receipt runnerauth.UpdateReceipt, build runnerauth.BuildEvidence, allowed ...int) {
	t.Helper()
	path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
	method := http.MethodPut
	var body any = runnerReceiptWrite{Receipt: receipt, Running: build}
	status := http.StatusNoContent
	if writer == "heartbeat" {
		observation := runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: build, Receipt: &receipt, ObservedAt: build.ObservedAt}
		observation.Revision = observation.BuildRevision()
		path = "/api/v2/organizations/org_startup/projects/prj_runner/machines/" + string(f.bindings[index].MachineID) + "/heartbeat"
		method = http.MethodPost
		body = nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 1, Version: build.Version, OS: build.OS, Architecture: build.Architecture, ProtocolMajor: 2, WorkspaceIsolation: "sandbox", Update: &observation}
		status = http.StatusOK
	}
	response := performHubAPIRequest(t, f.service, method, path, f.tokens[index], body)
	if response.Code != status && !slices.Contains(allowed, response.Code) {
		t.Errorf("%s: status %d: %s", writer, response.Code, response.Body.String())
	}
}

func assertMonotonicReceipt(t *testing.T, f runnerPGFixture, index int, want runnerauth.UpdateReceipt) {
	t.Helper()
	path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
	client, ctx := f.client(t, index)
	var got runnerauth.UpdateReceipt
	if err := client.Transport().Request(ctx, http.MethodGet, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("durable receipt regressed: got %+v want %+v", got, want)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[1-index], nil), http.StatusUnauthorized)
}

func TestPostgresRunnerUpdateReceiptReconciliation(t *testing.T) {
	f := newRunnerPGFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index := range f.bindings {
		request := runnerauth.UpdateRequest{ID: fmt.Sprintf("reconcile_%d", index), RequestedAt: now, Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
		seedMonotonicRequest(t, f, index, request)
		build := runnerauth.BuildEvidence{Version: "1.1.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
		for step, change := range []struct {
			status           string
			second           int
			failure          string
			applied, running bool
		}{
			{"draining", 0, "", false, false},
			{"uncertain", 1, "Interrupted update", false, false},
			{"applying", 2, "", false, false},
			{"draining", 2, "", false, false},
			{"applied", 3, "", true, false},
			{"restart_requested", 3, "", true, false},
			{"running", 3, "", true, true},
			{"uncertain", 4, "Restart failed", true, true},
			{"running", 5, "", true, true},
			{"applied", 5, "", true, true},
			{"running", 5, "", true, true},
		} {
			receipt := runnerauth.UpdateReceipt{Request: request, Status: change.status, ObservedAt: now.Add(time.Duration(change.second) * time.Second), FailureReason: change.failure}
			if change.applied {
				receipt.Applied = &build
			}
			if change.running {
				receipt.Running = &build
			}
			process := build
			process.ObservedAt = now.Add(time.Duration(step) * time.Second)
			if step == 9 {
				process.Version = "1.0.0"
			}
			writer := []string{"put", "heartbeat"}[(step+index)%2]
			writeMonotonicReceipt(t, f, index, writer, receipt, process)
			assertMonotonicReceipt(t, f, index, receipt)
		}
	}
}

func TestPostgresRunnerUpdateReceiptRequestSupersession(t *testing.T) {
	for _, kinds := range []string{"normal-normal", "normal-urgent", "urgent-normal", "urgent-urgent"} {
		t.Run(kinds, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			for index := range f.bindings {
				requests := make([]runnerauth.UpdateRequest, 2)
				for i, kind := range strings.Split(kinds, "-") {
					requests[i] = runnerauth.UpdateRequest{ID: fmt.Sprintf("supersede_%d_%d", index, i), RequestedAt: now.Add(time.Duration(index*2+i) * time.Second), Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
					if kind == "urgent" {
						requests[i].Urgent = true
						requests[i].ExpectedBuildRevision = ""
					}
				}
				if kinds == "urgent-normal" {
					requests[1].Version = "1.2.0"
				}
				if kinds == "normal-urgent" {
					requests[1].Version = "1.2.0"
					requests[1].RequestedAt = requests[0].RequestedAt.Add(-time.Second)
				}
				build := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
				seedMonotonicRequest(t, f, index, requests[0])
				previous := runnerauth.UpdateReceipt{Request: requests[0], Status: "uncertain", ObservedAt: now, FailureReason: "Interrupted update"}
				writeMonotonicReceipt(t, f, index, "put", previous, build)
				seedMonotonicRequest(t, f, index, requests[1])
				next := runnerauth.UpdateReceipt{Request: requests[1], Status: "draining", ObservedAt: now.Add(time.Second)}
				writeMonotonicReceipt(t, f, index, "heartbeat", next, build)
				next.RetainPreviousFailure(&previous)
				assertMonotonicReceipt(t, f, index, next)
				previous.ObservedAt = now.Add(time.Hour)
				writeMonotonicReceipt(t, f, index, "heartbeat", previous, build)
				writeMonotonicReceipt(t, f, index, "put", previous, build, http.StatusConflict)
				assertMonotonicReceipt(t, f, index, next)
				replay := next
				replay.PreviousFailure = nil
				writeMonotonicReceipt(t, f, index, "put", replay, build)
				assertMonotonicReceipt(t, f, index, next)
			}
		})
	}
}

func TestPostgresRunnerUpdateReceiptRequiresCurrentCredential(t *testing.T) {
	f := newRunnerPGFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index := range f.bindings {
		request := runnerauth.UpdateRequest{ID: fmt.Sprintf("credential_%d", index), RequestedAt: now, Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
		seedMonotonicRequest(t, f, index, request)
		build := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
		receipt := runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
		path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, f.tokens[1-index], runnerReceiptWrite{Receipt: receipt, Running: build}), http.StatusUnauthorized)
		writeMonotonicReceipt(t, f, index, "put", receipt, build)
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), fmt.Sprintf("credential_%d", index+1), now)
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, f.tokens[index], runnerReceiptWrite{Receipt: receipt, Running: build}), http.StatusUnauthorized)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[index], nil), http.StatusUnauthorized)
		observation := runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: build, Receipt: &receipt, ObservedAt: now}
		observation.Revision = observation.BuildRevision()
		heartbeat := nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 1, Version: build.Version, OS: build.OS, Architecture: build.Architecture, ProtocolMajor: 2, WorkspaceIsolation: "sandbox", Update: &observation}
		path = "/api/v2/organizations/org_startup/projects/prj_runner/machines/" + string(f.bindings[index].MachineID) + "/heartbeat"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], heartbeat), http.StatusUnauthorized)
	}
}
