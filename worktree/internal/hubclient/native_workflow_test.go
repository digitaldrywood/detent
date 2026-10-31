package hubclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkflowWriteReleasesStaleFencing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		failure   string
		successor bool
	}{
		{name: "two stale leases", failure: "stale_fencing_token"},
		{name: "current token"},
		{name: "network retry", failure: "network"},
		{name: "server retry", failure: "server"},
		{name: "uncertain release retains authority", failure: "release_network"},
		{name: "server release failure retains authority", failure: "release_server"},
		{name: "delayed release preserves successor", failure: "stale_fencing_token", successor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			leases := []tracker.NativeLease{
				{ID: "lease_first", WorkItemID: "wi_first", FencingToken: 1},
				{ID: "lease_second", WorkItemID: "wi_second", FencingToken: 2},
			}
			writes := make(map[tracker.NativeWorkItemID]int)
			releases := make(map[tracker.LeaseID]int)
			var native *NativeClient
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				status, payload := http.StatusOK, `{}`
				if strings.HasSuffix(request.URL.Path, "/workflow") {
					var transition tracker.Transition
					if err := json.NewDecoder(request.Body).Decode(&transition); err != nil {
						return nil, err
					}
					id := tracker.NativeWorkItemID(strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, native.base()+"/work-items/"), "/workflow"))
					writes[id]++
					for _, lease := range leases {
						if lease.WorkItemID == id && (transition.LeaseID != lease.ID || transition.FencingToken != lease.FencingToken) {
							return nil, errors.New("workflow lost lease fencing")
						}
					}
					if writes[id] == 1 {
						switch test.failure {
						case "stale_fencing_token":
							status, payload = http.StatusConflict, `{"code":"stale_fencing_token"}`
						case "network":
							return nil, errors.New("connection reset by peer")
						case "server":
							status, payload = http.StatusServiceUnavailable, `{"code":"unavailable"}`
						}
					}
				}
				if strings.HasSuffix(request.URL.Path, "/release") {
					for _, lease := range leases {
						if !strings.HasSuffix(request.URL.Path, "/leases/"+string(lease.ID)+"/release") {
							continue
						}
						releases[lease.ID]++
						if releases[lease.ID] == 1 {
							switch test.failure {
							case "release_network":
								return nil, errors.New("connection reset by peer")
							case "release_server":
								status, payload = http.StatusServiceUnavailable, `{"code":"unavailable"}`
							}
						}
						if test.successor {
							lease.ID, lease.FencingToken = "successor_"+lease.ID, lease.FencingToken+10
							native.client.nativeLeases.Store(native.base()+"/"+string(lease.WorkItemID), lease)
						}
					}
					if test.failure == "stale_fencing_token" {
						status, payload = http.StatusConflict, `{"code":"stale_fencing_token"}`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
			})
			client, err := New(Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"local": "prj_test"}, Machine: Machine{ID: "machine_test", Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			source := scheduler.nativeProject("local")
			native = source.client
			for _, lease := range leases {
				scheduler.nativeClaims[string(lease.WorkItemID)] = nativeClaim{source: source, lease: lease}
				scheduler.claims[string(lease.WorkItemID)] = nativeTrackerLease(lease)
				client.nativeLeases.Store(native.base()+"/"+string(lease.WorkItemID), lease)
			}
			for _, lease := range leases {
				request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("workflow_%s", lease.WorkItemID)}, State: "In Review"}
				_, err := native.Transition(t.Context(), lease.WorkItemID, request)
				lost := test.failure == "stale_fencing_token"
				if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) != lost {
					t.Fatalf("workflow authority classification = %v", err)
				}
				if test.failure == "network" || test.failure == "server" {
					if !errors.Is(err, ErrUnavailable) || native.fencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{}).FencingToken != lease.FencingToken {
						t.Fatalf("transient failure discarded retry authority: %v", err)
					}
					if _, err = native.Transition(t.Context(), lease.WorkItemID, request); err != nil {
						t.Fatalf("retry failed: %v", err)
					}
				} else if !lost && err != nil {
					t.Fatal(err)
				}
				releaseErr := scheduler.ReleaseClaim(t.Context(), string(lease.WorkItemID), "completed")
				if strings.HasPrefix(test.failure, "release_") {
					if !errors.Is(releaseErr, ErrUnavailable) || native.fencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{}).FencingToken != lease.FencingToken {
						t.Fatalf("uncertain release discarded local authority: %v", releaseErr)
					}
					releaseErr = scheduler.ReleaseClaim(t.Context(), string(lease.WorkItemID), "completed")
				}
				if releaseErr != nil {
					t.Fatal(releaseErr)
				}
				fence := native.fencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{})
				if test.successor {
					if fence.LeaseID != "successor_"+lease.ID || fence.FencingToken != lease.FencingToken+10 {
						t.Fatalf("delayed release discarded current authority: %+v", fence)
					}
				} else if fence.LeaseID != "" || fence.FencingToken != 0 {
					t.Errorf("released attempt retained workflow authority: %+v", fence)
				}
				wantWrites := 1
				if test.failure == "network" || test.failure == "server" {
					wantWrites = 2
				}
				if writes[lease.WorkItemID] != wantWrites {
					t.Errorf("workflow requests = %d, want %d", writes[lease.WorkItemID], wantWrites)
				}
			}
			if len(scheduler.nativeClaims) != 0 || len(scheduler.claims) != 0 {
				t.Fatal("workflow completion retained local claims")
			}
		})
	}
}
